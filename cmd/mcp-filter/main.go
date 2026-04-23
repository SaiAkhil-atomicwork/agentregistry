// mcp-filter is a small HTTP proxy that sits in front of agentgateway's MCP
// endpoint and enforces a per-agent allowlist over the federated tools/list
// and tools/call responses. It also optionally bridges A2A agents (exposed by
// agentgateway at `/agents/<name>` routes) into the same tools/list, so
// Paperclip agents see A2A skills alongside MCP tools natively.
//
// Architecture (fit with AtomClaw):
//
//	Paperclip agent → http://<host>:21213/mcp/<agent-id>  ─┐
//	                                                       │ filter + bridge
//	                     agentgateway :21212/mcp  ◄────────┤ (this proxy)
//	                     agentgateway :21212/agents/* ◄────┘
//
// Per-agent allowlist is fetched from a pluggable AllowlistSource (a static
// JSON file on disk today; a Paperclip API endpoint is the intended
// follow-on). Empty/missing allowlist = allow everything (back-compat).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type config struct {
	listenAddr         string
	upstreamMCP        string
	upstreamAdmin      string
	allowlistSource    string
	passthroughConfig  string
	a2aBridge          bool
	cacheTTL           time.Duration
	shutdownGrace      time.Duration
	agentCardTimeout   time.Duration
	passthroughReload  time.Duration
}

func loadConfig() config {
	cfg := config{
		listenAddr:        getenv("MCPFILTER_LISTEN_ADDR", ":21213"),
		upstreamMCP:       getenv("MCPFILTER_UPSTREAM_MCP", "http://127.0.0.1:21212/mcp"),
		upstreamAdmin:     getenv("MCPFILTER_UPSTREAM_ADMIN", "http://127.0.0.1:15000"),
		allowlistSource:   getenv("MCPFILTER_ALLOWLIST_SOURCE", "file:///etc/atomclaw/mcp-allowlist.json"),
		passthroughConfig: getenv("MCPFILTER_PASSTHROUGH_CONFIG", ""),
		a2aBridge:         getenv("MCPFILTER_A2A_BRIDGE", "true") == "true",
		cacheTTL:          parseDuration(getenv("MCPFILTER_CACHE_TTL", "30s")),
		shutdownGrace:     parseDuration(getenv("MCPFILTER_SHUTDOWN_GRACE", "10s")),
		agentCardTimeout:  parseDuration(getenv("MCPFILTER_A2A_PROBE_TIMEOUT", "3s")),
		passthroughReload: parseDuration(getenv("MCPFILTER_PASSTHROUGH_RELOAD", "10s")),
	}
	flag.StringVar(&cfg.listenAddr, "listen", cfg.listenAddr, "address:port to listen on")
	flag.StringVar(&cfg.upstreamMCP, "upstream-mcp", cfg.upstreamMCP, "agentgateway MCP URL")
	flag.StringVar(&cfg.upstreamAdmin, "upstream-admin", cfg.upstreamAdmin, "agentgateway admin URL (used for A2A route discovery)")
	flag.StringVar(&cfg.allowlistSource, "allowlist-source", cfg.allowlistSource, "allowlist source (file:///path.json or paperclip://host)")
	flag.StringVar(&cfg.passthroughConfig, "passthrough-config", cfg.passthroughConfig, "path to passthrough-targets JSON (for MCPs agentgateway cannot proxy natively, e.g. Fastn)")
	flag.BoolVar(&cfg.a2aBridge, "a2a-bridge", cfg.a2aBridge, "expose A2A agents (from /agents/*) as virtual MCP tools")
	flag.DurationVar(&cfg.cacheTTL, "cache-ttl", cfg.cacheTTL, "TTL for allowlist + A2A-agent-card cache")
	flag.Parse()
	return cfg
}

func main() {
	cfg := loadConfig()
	log.Printf("mcp-filter starting: listen=%s upstream=%s allowlist=%s a2aBridge=%t ttl=%s",
		cfg.listenAddr, cfg.upstreamMCP, cfg.allowlistSource, cfg.a2aBridge, cfg.cacheTTL)

	source, err := newAllowlistSource(cfg.allowlistSource)
	if err != nil {
		log.Fatalf("allowlist source: %v", err)
	}
	allowlist := newAllowlistCache(source, cfg.cacheTTL)

	var bridge *a2aBridge
	if cfg.a2aBridge {
		bridge = newA2ABridge(cfg.upstreamMCP, cfg.upstreamAdmin, cfg.cacheTTL, cfg.agentCardTimeout)
	}

	passthrough, err := newPassthroughStore(cfg.passthroughConfig, cfg.passthroughReload)
	if err != nil {
		log.Fatalf("passthrough config: %v", err)
	}
	if cfg.passthroughConfig != "" {
		log.Printf("passthrough enabled: file=%s reload=%s", cfg.passthroughConfig, cfg.passthroughReload)
	}

	p := &proxy{
		cfg:         cfg,
		allowlist:   allowlist,
		bridge:      bridge,
		passthrough: passthrough,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/mcp/", p.handleMCP)
	mux.HandleFunc("/passthrough/", p.handlePassthrough)
	mux.HandleFunc("/internal/invalidate/", func(w http.ResponseWriter, r *http.Request) {
		agentID := strings.TrimPrefix(r.URL.Path, "/internal/invalidate/")
		agentID = strings.Trim(agentID, "/")
		if agentID == "" {
			http.Error(w, "agent id missing", http.StatusBadRequest)
			return
		}
		allowlist.invalidate(agentID)
		w.WriteHeader(http.StatusNoContent)
	})
	// Also useful while developing — dump every cached allowlist.
	mux.HandleFunc("/internal/debug/allowlists", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(allowlist.debugJSON())
	})

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http server: %v", err)
		}
		cancel()
	}()

	waitForShutdown(ctx, srv, cfg.shutdownGrace)
}

func waitForShutdown(ctx context.Context, srv *http.Server, grace time.Duration) {
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		// default to 30s if unparseable
		return 30 * time.Second
	}
	return d
}

