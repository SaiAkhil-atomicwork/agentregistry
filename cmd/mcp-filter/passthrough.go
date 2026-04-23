package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Passthrough targets are upstream MCP endpoints that agentgateway cannot
// proxy natively (typically because of URL-handling quirks — notably Fastn's
// multi-param query string which agentgateway v1.1.0 percent-encodes on
// forward, breaking auth). For each such target we configure a clean URL
// here and publish a short proxy path (`/passthrough/<name>`) that
// agentgateway can safely point at. mcp-filter then forwards requests to
// the real URL with query string, headers, and body intact.
//
// Config file format (mode 600 on disk):
//
//	{
//	  "fastn": {
//	    "url": "https://mcp.ucl.dev/mcp/?id=...&api_key=${FASTN_API_KEY}&space_id=..."
//	  }
//	}
//
// Env vars are expanded via ${NAME} before parsing, same as manual-overlay.

type passthroughTarget struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

type passthroughStore struct {
	path    string
	mu      sync.RWMutex
	targets map[string]passthroughTarget
	loaded  time.Time
	ttl     time.Duration
}

// newPassthroughStore reads the passthrough config file. If path is empty or
// the file doesn't exist, returns an empty store (passthrough disabled).
func newPassthroughStore(path string, ttl time.Duration) (*passthroughStore, error) {
	s := &passthroughStore{path: path, ttl: ttl, targets: make(map[string]passthroughTarget)}
	if path == "" {
		return s, nil
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *passthroughStore) reload() error {
	if s.path == "" {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.targets = map[string]passthroughTarget{}
			s.loaded = time.Now()
			s.mu.Unlock()
			return nil
		}
		return fmt.Errorf("read passthrough config %s: %w", s.path, err)
	}
	expanded := expandPassthroughEnv(raw)
	next := make(map[string]passthroughTarget)
	if err := json.Unmarshal(expanded, &next); err != nil {
		return fmt.Errorf("parse passthrough config %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.targets = next
	s.loaded = time.Now()
	s.mu.Unlock()
	return nil
}

// get fetches a target by name with TTL-based auto-reload so ops can edit
// the file without restarting the service.
func (s *passthroughStore) get(name string) (passthroughTarget, bool) {
	s.mu.RLock()
	needsReload := s.ttl > 0 && time.Since(s.loaded) > s.ttl
	s.mu.RUnlock()
	if needsReload {
		if err := s.reload(); err != nil {
			log.Printf("passthrough reload failed: %v", err)
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.targets[name]
	return t, ok
}

var passthroughEnvPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandPassthroughEnv(raw []byte) []byte {
	return passthroughEnvPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		m := passthroughEnvPattern.FindSubmatch(match)
		if len(m) < 2 {
			return match
		}
		if v, ok := os.LookupEnv(string(m[1])); ok {
			// JSON-safe: the expansion happens before json.Unmarshal, so quote-
			// sensitive values (tokens with quotes, newlines) would break parsing.
			// Since we expect simple opaque tokens here, a raw substitution is fine.
			return []byte(v)
		}
		return match
	})
}

// handlePassthrough implements `/passthrough/<name>`. It ignores any sub-path
// after the name — MCP's streamable-HTTP spec uses a single endpoint per
// session and every request is a POST against that endpoint, so we just
// forward to the target URL verbatim.
func (p *proxy) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	if p.passthrough == nil {
		http.Error(w, "passthrough not configured", http.StatusNotFound)
		return
	}
	// /passthrough/<name>[/rest]
	rest := strings.TrimPrefix(r.URL.Path, "/passthrough/")
	name := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		name = rest[:i]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		http.Error(w, "passthrough target name missing", http.StatusBadRequest)
		return
	}
	target, ok := p.passthrough.get(name)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown passthrough target %q", name), http.StatusNotFound)
		return
	}
	if target.URL == "" {
		http.Error(w, fmt.Sprintf("passthrough target %q has no URL", name), http.StatusInternalServerError)
		return
	}

	targetURL, err := url.Parse(target.URL)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid passthrough URL: %v", err), http.StatusInternalServerError)
		return
	}

	// Read the request body so we can replay to the upstream.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), strings.NewReader(string(body)))
	if err != nil {
		http.Error(w, "build upstream: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Forward the incoming headers (minus hop-by-hop). Content-Length is set
	// automatically from the body reader.
	copyHeaders(upstreamReq.Header, r.Header)
	upstreamReq.Header.Del("Host")
	upstreamReq.Header.Del("Content-Length")

	// Inject any static headers the operator configured (e.g. header-based
	// auth for an MCP that doesn't accept query-string auth).
	for k, v := range target.Headers {
		upstreamReq.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil && err != io.EOF {
		log.Printf("passthrough copy error (%s): %v", name, err)
	}
}

// fileBase returns the filename portion of a path — used only for logging.
func fileBase(p string) string { return filepath.Base(p) }
