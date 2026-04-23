package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// a2aBridge discovers the A2A agents that agentgateway exposes at
// `/agents/<name>` routes and exposes their skills as MCP tools in the
// proxy's tools/list response. When Claude picks one of these tools, the
// bridge translates the MCP tools/call back into an A2A message/send and
// returns the resulting text as an MCP content-text result.
type a2aBridge struct {
	upstreamMCPBase string // e.g. http://127.0.0.1:21212/mcp (for deriving the gateway root)
	upstreamAdmin   string // e.g. http://127.0.0.1:15000 (currently unused — reserved for Listeners/Routes/Backends API)
	gatewayRoot     string // e.g. http://127.0.0.1:21212

	cacheTTL     time.Duration
	probeTimeout time.Duration
	client       *http.Client

	mu           sync.RWMutex
	refreshedAt  time.Time
	toolsByName  map[string]bridgeTool // tool name → bridge mapping
	currentTools []bridgeTool          // for stable ordering in tools/list
}

// bridgeTool is the MCP-facing view of an A2A skill.
type bridgeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// bridge-private routing:
	agentRoute string `json:"-"` // full URL of the A2A agent (e.g. http://…/agents/com-…)
	skillID    string `json:"-"`
}

// newA2ABridge builds a bridge. gatewayRoot is derived from the MCP URL's
// scheme+host+port so the bridge can hit `/agents/<route>/.well-known/agent.json`.
func newA2ABridge(upstreamMCP, upstreamAdmin string, cacheTTL, probeTimeout time.Duration) *a2aBridge {
	u, err := url.Parse(upstreamMCP)
	root := ""
	if err == nil {
		root = u.Scheme + "://" + u.Host
	}
	return &a2aBridge{
		upstreamMCPBase: upstreamMCP,
		upstreamAdmin:   upstreamAdmin,
		gatewayRoot:     root,
		cacheTTL:        cacheTTL,
		probeTimeout:    probeTimeout,
		client:          &http.Client{Timeout: probeTimeout},
		toolsByName:     make(map[string]bridgeTool),
	}
}

// toolsList returns the current bridge-contributed MCP tools. Refreshes from
// the gateway when the cache is stale.
func (b *a2aBridge) toolsList() []bridgeTool {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	fresh := time.Since(b.refreshedAt) < b.cacheTTL
	tools := b.currentTools
	b.mu.RUnlock()
	if fresh {
		return tools
	}
	// Refresh (best-effort — log & fall back to stale on error).
	if err := b.refresh(); err != nil {
		log.Printf("a2a bridge refresh: %v (serving stale list, %d tools)", err, len(tools))
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.currentTools
}

func (b *a2aBridge) owns(toolName string) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	_, ok := b.toolsByName[toolName]
	b.mu.RUnlock()
	return ok
}

// refresh discovers all currently-configured A2A agents on the gateway and
// rebuilds the tool table. Discovery path:
//  1. Enumerate /agents/* routes from the admin UI's /ui/routes JSON (falls
//     back to config-file parsing if that's not available).
//  2. For each route, GET <gatewayRoot><route>/.well-known/agent.json to
//     fetch the AgentCard.
//  3. Translate the card's skills[] into one or more MCP tools, named
//     `a2a_<route-slug>_<skill-id>` so the name is unique and greppable.
//
// We intentionally use a small config-file fallback: the admin UI's API
// surface is thin and shifts between upstream versions, but the gateway
// config file (which the reconciler writes) is stable and shipped alongside.
func (b *a2aBridge) refresh() error {
	ctx, cancel := context.WithTimeout(context.Background(), b.probeTimeout*3)
	defer cancel()

	routes, err := b.discoverAgentRoutes(ctx)
	if err != nil {
		return fmt.Errorf("discover routes: %w", err)
	}

	newTools := make([]bridgeTool, 0, len(routes))
	byName := make(map[string]bridgeTool, len(routes))

	for _, route := range routes {
		card, err := b.fetchAgentCard(ctx, route)
		if err != nil {
			log.Printf("a2a bridge: fetch agent card for %s: %v", route, err)
			continue
		}
		for _, sk := range card.Skills {
			name := fmt.Sprintf("a2a_%s_%s", routeSlug(route), skillSlug(sk.ID, sk.Name))
			desc := sk.Description
			if desc == "" && card.Description != "" {
				desc = card.Description
			}
			if desc == "" {
				desc = card.Name
			}
			schema := defaultInputSchema()
			t := bridgeTool{
				Name:        name,
				Description: desc,
				InputSchema: schema,
				agentRoute:  b.gatewayRoot + route,
				skillID:     sk.ID,
			}
			newTools = append(newTools, t)
			byName[name] = t
		}
	}

	b.mu.Lock()
	b.currentTools = newTools
	b.toolsByName = byName
	b.refreshedAt = time.Now()
	b.mu.Unlock()
	log.Printf("a2a bridge: refreshed %d tools from %d agent routes", len(newTools), len(routes))
	return nil
}

// handleToolCall translates an MCP tools/call for a bridge-owned tool into
// an A2A message/send against the agent's JSON-RPC endpoint, and marshals
// the result back into MCP shape: `{content:[{type:"text",text:"…"}]}`.
func (b *a2aBridge) handleToolCall(w http.ResponseWriter, r *http.Request, env jsonRPCEnvelope) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(env.Params, &params); err != nil {
		writeJSONRPCError(w, env.ID, -32602, "invalid params: "+err.Error(), http.StatusOK)
		return
	}
	b.mu.RLock()
	t, ok := b.toolsByName[params.Name]
	b.mu.RUnlock()
	if !ok {
		writeJSONRPCError(w, env.ID, -32601, "unknown bridge tool: "+params.Name, http.StatusOK)
		return
	}

	// Extract the user's input text. For v1 we accept either {"text":"…"}
	// or {"input":"…"} or the entire arguments blob serialized.
	var args map[string]json.RawMessage
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			writeJSONRPCError(w, env.ID, -32602, "invalid arguments: "+err.Error(), http.StatusOK)
			return
		}
	}
	text := extractTextArg(args)

	// Build A2A message/send.
	a2aBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "message/send",
		"params": map[string]any{
			"message": map[string]any{
				"role":  "user",
				"parts": []map[string]any{{"kind": "text", "text": text}},
			},
		},
	}
	payload, _ := json.Marshal(a2aBody)

	reqCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, t.agentRoute+"/", bytes.NewReader(payload))
	if err != nil {
		writeJSONRPCError(w, env.ID, -32000, "build upstream: "+err.Error(), http.StatusOK)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSONRPCError(w, env.ID, -32000, "a2a upstream error: "+err.Error(), http.StatusOK)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var a2aResp struct {
		Result struct {
			Status struct {
				State string `json:"state"`
			} `json:"status"`
			History []struct {
				Role  string `json:"role"`
				Parts []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"history"`
		} `json:"result"`
		Error *jsonRPCError `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &a2aResp); err != nil {
		writeJSONRPCError(w, env.ID, -32000, "a2a parse error: "+err.Error(), http.StatusOK)
		return
	}
	if a2aResp.Error != nil {
		writeJSONRPCError(w, env.ID, a2aResp.Error.Code, "a2a: "+a2aResp.Error.Message, http.StatusOK)
		return
	}

	// Pull the last assistant message from history.
	var answer string
	for i := len(a2aResp.Result.History) - 1; i >= 0; i-- {
		if a2aResp.Result.History[i].Role == "agent" {
			var sb strings.Builder
			for _, p := range a2aResp.Result.History[i].Parts {
				if p.Kind == "text" {
					sb.WriteString(p.Text)
				}
			}
			answer = sb.String()
			break
		}
	}
	mcpResult := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": answer},
		},
		"isError": false,
	}
	resultRaw, _ := json.Marshal(mcpResult)
	out := jsonRPCResponse{JSONRPC: "2.0", ID: env.ID, Result: resultRaw}
	b2, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b2)
}

// ---------------------------------------------------------------------------
// route discovery
// ---------------------------------------------------------------------------

// agentRoutePattern matches the path-prefix values the reconciler emits for
// Agent deployments — always `/agents/<sanitized-name>`.
var agentRoutePattern = regexp.MustCompile(`^/agents/[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// discoverAgentRoutes returns the set of /agents/<name> path prefixes
// currently configured on the gateway. Path: the live generated
// agent-gateway.yaml is mounted into the daemon container at /config on EC2
// but on the host is at <platformDir>/agent-gateway.yaml; we find it by
// asking the admin UI (which exposes the current Listeners/Routes state).
//
// Strategy v1: scan the admin UI's HTML for JSON-embedded routes. Fallback:
// nothing, which means the bridge will temporarily surface zero tools.
func (b *a2aBridge) discoverAgentRoutes(ctx context.Context) ([]string, error) {
	// agentgateway v1.1.0 admin UI exposes the live config as JSON at
	// `/config` (no `/api/` prefix). We also try a couple of older paths
	// for resilience across upstream versions.
	adminRoot := strings.TrimRight(b.upstreamAdmin, "/")
	candidates := []string{
		adminRoot + "/config",
		adminRoot + "/api/config",
		adminRoot + "/api/routes",
	}
	for _, endpoint := range candidates {
		routes, err := b.fetchRoutesFromJSON(ctx, endpoint)
		if err == nil && len(routes) > 0 {
			return routes, nil
		}
	}
	// Could not discover via admin API. Return nothing — tools/list will
	// simply not include bridge tools until discovery succeeds.
	return nil, nil
}

// fetchRoutesFromJSON hits a JSON endpoint and extracts every pathPrefix
// matching `/agents/<name>`. We walk the JSON generically so we don't need
// to mirror the upstream agentgateway schema exactly.
func (b *a2aBridge) fetchRoutesFromJSON(ctx context.Context, endpoint string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("admin endpoint %s returned %d", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	walkForPathPrefixes(doc, seen)
	out := make([]string, 0, len(seen))
	for r := range seen {
		if agentRoutePattern.MatchString(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

func walkForPathPrefixes(node any, out map[string]struct{}) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if k == "pathPrefix" {
				if s, ok := child.(string); ok && s != "" {
					out[s] = struct{}{}
				}
			}
			walkForPathPrefixes(child, out)
		}
	case []any:
		for _, c := range v {
			walkForPathPrefixes(c, out)
		}
	}
}

// fetchAgentCard hits <root><route>/.well-known/agent.json and unmarshals the
// minimum fields we need (name, description, skills[]).
func (b *a2aBridge) fetchAgentCard(ctx context.Context, route string) (*agentCard, error) {
	u := b.gatewayRoot + route + "/.well-known/agent.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s returned %d", u, resp.StatusCode)
	}
	var card agentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return nil, err
	}
	return &card, nil
}

// agentCard is the minimum subset of the A2A agent-card schema we need.
type agentCard struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Skills      []agentSkill `json:"skills"`
}

type agentSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ---------------------------------------------------------------------------
// naming helpers
// ---------------------------------------------------------------------------

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func routeSlug(route string) string {
	trimmed := strings.TrimPrefix(route, "/agents/")
	return nonAlnum.ReplaceAllString(trimmed, "-")
}

func skillSlug(id, name string) string {
	if id != "" {
		return nonAlnum.ReplaceAllString(id, "_")
	}
	return nonAlnum.ReplaceAllString(name, "_")
}

func extractTextArg(args map[string]json.RawMessage) string {
	if args == nil {
		return ""
	}
	for _, k := range []string{"text", "input", "prompt", "message", "query"} {
		if raw, ok := args[k]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && s != "" {
				return s
			}
		}
	}
	// Fallback: serialize the whole thing.
	obj := make(map[string]any, len(args))
	for k, v := range args {
		var a any
		_ = json.Unmarshal(v, &a)
		obj[k] = a
	}
	b, _ := json.Marshal(obj)
	return string(b)
}

// defaultInputSchema is a permissive JSON schema used when we don't have a
// more specific one from the A2A agent-card (A2A's card schema doesn't
// require an input schema per-skill in v0.2.5).
func defaultInputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {
      "type": "string",
      "description": "User input to send to the A2A agent as message/send"
    }
  },
  "required": ["text"]
}`)
}
