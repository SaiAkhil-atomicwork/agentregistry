package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// proxy handles an incoming MCP request from a client (e.g. Claude CLI running
// in a Paperclip agent), optionally filters the response, and returns it.
type proxy struct {
	cfg         config
	allowlist   *allowlistCache
	bridge      *a2aBridge
	passthrough *passthroughStore
	client      *http.Client
}

// handleMCP dispatches `/mcp/<agent-id>` requests.
func (p *proxy) handleMCP(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimPrefix(r.URL.Path, "/mcp/")
	agentID = strings.Trim(agentID, "/")
	if agentID == "" {
		writeJSONRPCError(w, nil, -32600, "agent id missing in path (use /mcp/<agent-id>)", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONRPCError(w, nil, -32700, "could not read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	var envelope jsonRPCEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error: "+err.Error(), http.StatusBadRequest)
		return
	}

	list, err := p.allowlist.get(agentID)
	if err != nil {
		log.Printf("allowlist fetch for %s failed: %v — forwarding without filtering", agentID, err)
		list = agentAllowlist{AllowAll: true}
	}

	switch envelope.Method {
	case "tools/call":
		toolName, _ := extractToolName(envelope.Params)
		if toolName == "" {
			// Let the gateway return its own error.
			p.forward(w, r, body)
			return
		}
		// Bridge-owned tool? Dispatch to the bridge.
		if p.bridge != nil && p.bridge.owns(toolName) {
			if !list.allowTool(toolName) {
				writeJSONRPCError(w, envelope.ID, -32601, fmt.Sprintf("tool not permitted for this agent: %s", toolName), http.StatusOK)
				return
			}
			p.bridge.handleToolCall(w, r, envelope)
			return
		}
		if !list.allowTool(toolName) {
			writeJSONRPCError(w, envelope.ID, -32601, fmt.Sprintf("tool not permitted for this agent: %s", toolName), http.StatusOK)
			return
		}
		p.forward(w, r, body)

	case "tools/list":
		// Forward first, then filter the response, then also merge in bridge
		// tools (filtered the same way).
		upstreamStatus, upstreamHeaders, upstreamBody, err := p.forwardReadAll(r, body)
		if err != nil {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
			return
		}
		filtered, rewrittenBody, ok := rewriteToolsListResponse(upstreamBody, list, p.bridge)
		if !ok {
			// Not a JSON response we could rewrite (SSE with multiple events, etc.). Pass through.
			copyHeaders(w.Header(), upstreamHeaders)
			w.WriteHeader(upstreamStatus)
			_, _ = w.Write(upstreamBody)
			return
		}
		copyHeaders(w.Header(), upstreamHeaders)
		// Rewrite always produces JSON (either SSE re-assembled or native JSON).
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rewrittenBody)))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("Transfer-Encoding")
		w.WriteHeader(upstreamStatus)
		_, _ = w.Write(rewrittenBody)
		log.Printf("tools/list agent=%s upstream=%d filtered=%d", agentID, filtered.total, filtered.kept)

	default:
		// initialize / notifications/* / ping / etc. — pass through untouched.
		p.forward(w, r, body)
	}
}

// forward streams the request to upstream and the response to the client,
// preserving headers and body (handles both JSON and SSE).
func (p *proxy) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.cfg.upstreamMCP, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Host")
	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// forwardReadAll is forward()'s cousin that buffers the response so we can
// inspect and rewrite the body. Only used for tools/list where we need to
// filter.
func (p *proxy) forwardReadAll(r *http.Request, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.cfg.upstreamMCP, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Host")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, out, nil
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		// Hop-by-hop headers; let net/http set them as needed.
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC helpers
// ---------------------------------------------------------------------------

type jsonRPCEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string, httpStatus int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	}
	b, _ := json.Marshal(resp)
	_, _ = w.Write(b)
}

// extractToolName pulls `params.name` out of a tools/call request.
func extractToolName(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return "", err
	}
	return params.Name, nil
}

// ---------------------------------------------------------------------------
// tools/list rewriting
// ---------------------------------------------------------------------------

type filterStats struct {
	total int
	kept  int
}

// rewriteToolsListResponse strips tools that are denied by `list` from the
// upstream tools/list response, and optionally appends tools from the A2A
// bridge that pass the same filter. Returns the new body bytes when it was
// able to parse the upstream response; otherwise returns (_, _, false) so
// the caller can pass through untouched.
//
// Handles both native JSON (`{"jsonrpc":"2.0","result":{"tools":[…]}}`) and
// streamable-HTTP SSE events of the form `data: {json}\n`.
func rewriteToolsListResponse(body []byte, list agentAllowlist, bridge *a2aBridge) (filterStats, []byte, bool) {
	stats := filterStats{}
	payload, isSSE := extractJSONPayload(body)
	if payload == nil {
		return stats, body, false
	}

	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Result  struct {
			Tools      json.RawMessage `json:"tools"`
			NextCursor string          `json:"nextCursor,omitempty"`
		} `json:"result"`
		Error json.RawMessage `json:"error,omitempty"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return stats, body, false
	}

	var tools []json.RawMessage
	if len(resp.Result.Tools) > 0 {
		if err := json.Unmarshal(resp.Result.Tools, &tools); err != nil {
			return stats, body, false
		}
	}
	stats.total = len(tools)

	kept := make([]json.RawMessage, 0, len(tools))
	for _, raw := range tools {
		var head struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &head); err != nil || head.Name == "" {
			continue
		}
		if !list.allowTool(head.Name) {
			continue
		}
		kept = append(kept, raw)
	}

	// Append bridge-contributed tools (filtered with the same allowlist).
	if bridge != nil {
		bridgeTools := bridge.toolsList()
		for _, t := range bridgeTools {
			if !list.allowTool(t.Name) {
				continue
			}
			b, err := json.Marshal(t)
			if err == nil {
				kept = append(kept, b)
			}
		}
	}
	stats.kept = len(kept)

	// Rebuild the JSON response around the filtered tool list.
	newResult := struct {
		Tools      []json.RawMessage `json:"tools"`
		NextCursor string            `json:"nextCursor,omitempty"`
	}{Tools: kept, NextCursor: resp.Result.NextCursor}
	newResultRaw, err := json.Marshal(newResult)
	if err != nil {
		return stats, body, false
	}
	final := jsonRPCResponse{
		JSONRPC: resp.JSONRPC,
		ID:      resp.ID,
		Result:  newResultRaw,
	}
	out, err := json.Marshal(final)
	if err != nil {
		return stats, body, false
	}
	_ = isSSE // we always return native JSON — clients accept both (Accept: application/json, text/event-stream)
	return stats, out, true
}

// extractJSONPayload returns the JSON payload from either a native JSON body
// or a streamable-HTTP SSE body (first `data: {json}` line wins — the
// response to a single JSON-RPC request is always one envelope). Returns nil
// if we couldn't find one.
func extractJSONPayload(body []byte) ([]byte, bool) {
	b := bytes.TrimSpace(body)
	if len(b) > 0 && (b[0] == '{' || b[0] == '[') {
		return b, false
	}
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) > 0 {
			return payload, true
		}
	}
	return nil, false
}
