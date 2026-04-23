package main

import (
	"encoding/json"
	"fmt"
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
type agentAllowlist struct {
	AllowAll bool         `json:"allowAll"`
	Entries  []AllowEntry `json:"entries,omitempty"`
}

// allowTool returns true when `toolName` passes the allowlist. Empty entries
// with AllowAll=false means nothing passes — "deny by default". Empty entries
// with AllowAll=true (or an unknown agent in a permissive source) means
// everything passes.
func (a agentAllowlist) allowTool(toolName string) bool {
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
		// TODO(phase 2): implement HTTP client against Paperclip's
		// GET /api/internal/agents/<id>/allowed-tools endpoint.
		return nil, fmt.Errorf("paperclip:// allowlist source not yet implemented (phase 2)")
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
