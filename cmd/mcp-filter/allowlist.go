package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// AllowEntry is one row in an agent's allowlist. Exactly one of ServerPrefix,
// ToolName must be set. Server-level allows match tool names whose prefix
// begins with `ServerPrefix` (handy because agentgateway federates tools with
// prefixes like `manual-composio_`, `com-firecrawl-mcp-scrape-<uuid>_`, etc).
type AllowEntry struct {
	ServerPrefix string `json:"serverPrefix,omitempty"`
	ToolName     string `json:"toolName,omitempty"`
}

// agentAllowlist is the in-memory shape: a list of allow entries plus an
// optional "allow all" sentinel (no entries AND allowAll=true).
//
// MCPEnabled and A2AEnabled mirror the Paperclip Protocols-tab toggles. They
// are applied AFTER the entries-based allowlist passes, so:
//   - mcpEnabled=false → deny every tool, regardless of entries / allowAll
//   - a2aEnabled=false → deny tools whose name starts with `a2a_`
// Both are pointers so we can distinguish "explicitly false" from "field
// missing" when sources don't provide them. Missing → treat as enabled.
type agentAllowlist struct {
	AllowAll    bool         `json:"allowAll"`
	Entries     []AllowEntry `json:"entries,omitempty"`
	MCPEnabled  *bool        `json:"mcpEnabled,omitempty"`
	A2AEnabled  *bool        `json:"a2aEnabled,omitempty"`
}

// allowTool returns true when `toolName` passes the allowlist. Empty entries
// with AllowAll=false means nothing passes — "deny by default". Empty entries
// with AllowAll=true (or an unknown agent in a permissive source) means
// everything passes.
//
// On top of that, the MCPEnabled / A2AEnabled gates from the Protocols tab
// override allow rules: an agent with MCP off sees nothing; an agent with
// A2A off doesn't see synthetic `a2a_*` tools regardless of any allow rule.
func (a agentAllowlist) allowTool(toolName string) bool {
	if a.MCPEnabled != nil && !*a.MCPEnabled {
		return false
	}
	if a.A2AEnabled != nil && !*a.A2AEnabled && strings.HasPrefix(toolName, "a2a_") {
		return false
	}
	if a.AllowAll {
		return true
	}
	for _, e := range a.Entries {
		if e.ToolName != "" && e.ToolName == toolName {
			return true
		}
		if e.ServerPrefix != "" && strings.HasPrefix(toolName, e.ServerPrefix) {
			return true
		}
	}
	return false
}

// AllowlistSource is anything that can produce an allowlist for a given
// agent-id. Implementations are expected to be cheap to call (file read,
// HTTP with short timeout, etc.); caching is a layer above.
type AllowlistSource interface {
	get(agentID string) (agentAllowlist, error)
}

// newAllowlistSource parses a `file://…json` or `paperclip://host` descriptor
// and returns the appropriate source.
func newAllowlistSource(spec string) (AllowlistSource, error) {
	if spec == "" {
		return permissiveSource{}, nil
	}
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("parse allowlist source %q: %w", spec, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "file":
		path := u.Path
		if path == "" {
			path = strings.TrimPrefix(spec, "file://")
		}
		return &fileSource{path: path}, nil
	case "paperclip", "paperclip+http", "paperclip+https":
		// paperclip://host[:port]                  → http://host[:port]
		// paperclip+https://host/…                 → https://host/…
		// paperclip://10.2.141.154:80?host=foo.com → connects to the IP/port
		//   for the TCP hop but sends `Host: foo.com` so a name-based
		//   reverse proxy (e.g. nginx with multiple server_name blocks)
		//   matches the right vhost. Useful when the registry-of-truth
		//   hostname isn't directly DNS-routable from this host.
		scheme := "http"
		if u.Scheme == "paperclip+https" {
			scheme = "https"
		}
		base := scheme + "://" + u.Host
		if u.Path != "" && u.Path != "/" {
			base += strings.TrimRight(u.Path, "/")
		}
		hostOverride := u.Query().Get("host")
		token := os.Getenv("PAPERCLIP_INTERNAL_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("PAPERCLIP_INTERNAL_TOKEN env var must be set for paperclip:// allowlist source")
		}
		return &paperclipSource{
			baseURL:      base,
			token:        token,
			hostOverride: hostOverride,
			client:       &http.Client{Timeout: 3 * time.Second},
		}, nil
	case "permissive":
		return permissiveSource{}, nil
	default:
		return nil, fmt.Errorf("unsupported allowlist source scheme %q", u.Scheme)
	}
}

// permissiveSource allows everything. Useful as a fallback when no source is
// configured and for development.
type permissiveSource struct{}

func (permissiveSource) get(_ string) (agentAllowlist, error) {
	return agentAllowlist{AllowAll: true}, nil
}

// fileSource reads a JSON file on every request. The file is a map from
// agent-id to allowlist. Missing agent = allow all (permissive default).
//
// Example file content:
//
//	{
//	  "11111111-...": { "entries": [ { "serverPrefix": "manual-composio_" } ] },
//	  "22222222-...": { "allowAll": true }
//	}
type fileSource struct {
	path string
}

func (s *fileSource) get(agentID string) (agentAllowlist, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			// No file → permissive default.
			return agentAllowlist{AllowAll: true}, nil
		}
		return agentAllowlist{}, fmt.Errorf("read %s: %w", s.path, err)
	}
	var all map[string]agentAllowlist
	if err := json.Unmarshal(b, &all); err != nil {
		return agentAllowlist{}, fmt.Errorf("parse %s: %w", s.path, err)
	}
	lst, ok := all[agentID]
	if !ok {
		return agentAllowlist{AllowAll: true}, nil
	}
	return lst, nil
}

// paperclipSource fetches per-agent allowlists from Paperclip's
// `/api/internal/agents/<id>/allowed-tools` endpoint. Auth is a shared
// secret in the `x-internal-token` header (PAPERCLIP_INTERNAL_TOKEN).
type paperclipSource struct {
	baseURL      string
	token        string
	hostOverride string
	client       *http.Client
}

func (p *paperclipSource) get(agentID string) (agentAllowlist, error) {
	u := p.baseURL + "/api/internal/agents/" + url.PathEscape(agentID) + "/allowed-tools"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return agentAllowlist{}, fmt.Errorf("build request: %w", err)
	}
	if p.hostOverride != "" {
		req.Host = p.hostOverride
	}
	req.Header.Set("X-Internal-Token", p.token)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return agentAllowlist{}, fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Unknown agent → permissive. The proxy still serves tools in case
		// the agent was created just before Paperclip propagated.
		return agentAllowlist{AllowAll: true}, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return agentAllowlist{}, fmt.Errorf("paperclip rejected x-internal-token (check PAPERCLIP_INTERNAL_TOKEN)")
	case resp.StatusCode >= 400:
		return agentAllowlist{}, fmt.Errorf("paperclip returned %d for %s", resp.StatusCode, u)
	}
	var body agentAllowlist
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return agentAllowlist{}, fmt.Errorf("decode response: %w", err)
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// cache layer
// ---------------------------------------------------------------------------

type cacheEntry struct {
	list      agentAllowlist
	expiresAt time.Time
}

type allowlistCache struct {
	source AllowlistSource
	ttl    time.Duration
	mu     sync.RWMutex
	items  map[string]cacheEntry
}

func newAllowlistCache(source AllowlistSource, ttl time.Duration) *allowlistCache {
	return &allowlistCache{
		source: source,
		ttl:    ttl,
		items:  make(map[string]cacheEntry),
	}
}

func (c *allowlistCache) get(agentID string) (agentAllowlist, error) {
	c.mu.RLock()
	entry, ok := c.items[agentID]
	c.mu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.list, nil
	}
	list, err := c.source.get(agentID)
	if err != nil {
		// On fetch failure, fall back to the previous cached value (if any)
		// to avoid denying all traffic when the control plane hiccups.
		if ok {
			return entry.list, nil
		}
		return agentAllowlist{}, err
	}
	c.mu.Lock()
	c.items[agentID] = cacheEntry{list: list, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return list, nil
}

func (c *allowlistCache) invalidate(agentID string) {
	c.mu.Lock()
	delete(c.items, agentID)
	c.mu.Unlock()
}

func (c *allowlistCache) debugJSON() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	snap := make(map[string]agentAllowlist, len(c.items))
	for k, v := range c.items {
		snap[k] = v.list
	}
	b, _ := json.MarshalIndent(snap, "", "  ")
	return b
}
