package local

import (
	"os"
	"path/filepath"
	"testing"

	platformtypes "github.com/agentregistry-dev/agentregistry/internal/registry/platforms/types"
)

func TestLoadManualOverlay_MissingFileReturnsNil(t *testing.T) {
	dir := t.TempDir()
	overlay, err := LoadManualOverlay(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overlay != nil {
		t.Fatalf("expected nil overlay, got %#v", overlay)
	}
}

func TestLoadManualOverlay_ParsesAndExpandsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OVERLAY_TEST_KEY", "secret-123")
	contents := `
mcpTargets:
  - name: manual-composio
    mcp:
      host: https://connect.composio.dev/mcp
    policies:
      requestHeaderModifier:
        set:
          X-CONSUMER-API-KEY: ${OVERLAY_TEST_KEY}
  - name: manual-fastn
    mcp:
      host: https://example.com/mcp/?api_key=${MISSING_KEY}
`
	if err := os.WriteFile(filepath.Join(dir, manualOverlayFileName), []byte(contents), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	overlay, err := LoadManualOverlay(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if overlay == nil || len(overlay.MCPTargets) != 2 {
		t.Fatalf("expected 2 targets, got %#v", overlay)
	}

	if overlay.MCPTargets[0].Name != "manual-composio" {
		t.Errorf("name[0]: want manual-composio, got %q", overlay.MCPTargets[0].Name)
	}
	got := overlay.MCPTargets[0].Policies.RequestHeaderModifier.Set["X-CONSUMER-API-KEY"]
	if got != "secret-123" {
		t.Errorf("env interpolation: want %q, got %q", "secret-123", got)
	}

	// Unset env var should remain verbatim so misconfigs are obvious.
	if overlay.MCPTargets[1].MCP.Host != "https://example.com/mcp/?api_key=${MISSING_KEY}" {
		t.Errorf("unset env should be left verbatim, got %q", overlay.MCPTargets[1].MCP.Host)
	}
}

func TestApplyManualOverlay_NilNoop(t *testing.T) {
	cfg := newTestGatewayCfg(nil)
	ApplyManualOverlay(cfg, nil)
	ApplyManualOverlay(nil, &ManualOverlay{MCPTargets: []platformtypes.MCPTarget{{Name: "x"}}})
	// no panics, no changes — we just want to make sure these are safe.
	if len(cfg.Binds[0].Listeners[0].Routes) != 0 {
		t.Fatalf("nil overlay should not add routes")
	}
}

func TestApplyManualOverlay_AppendsToExistingMCPRoute(t *testing.T) {
	cfg := newTestGatewayCfg([]platformtypes.MCPTarget{
		{Name: "deployed-slack", MCP: &platformtypes.MCPTargetSpec{Host: "http://slack:3000/mcp"}},
	})
	overlay := &ManualOverlay{
		MCPTargets: []platformtypes.MCPTarget{
			{Name: "manual-composio", MCP: &platformtypes.MCPTargetSpec{Host: "https://connect.composio.dev/mcp"}},
			{Name: "manual-fastn", MCP: &platformtypes.MCPTargetSpec{Host: "https://example.com/mcp"}},
		},
	}

	ApplyManualOverlay(cfg, overlay)

	targets := mustMCPTargets(t, cfg)
	if len(targets) != 3 {
		t.Fatalf("want 3 targets, got %d (%v)", len(targets), targetNamesOf(targets))
	}
	wantNames := []string{"deployed-slack", "manual-composio", "manual-fastn"}
	for i, want := range wantNames {
		if targets[i].Name != want {
			t.Errorf("target[%d]: want %q, got %q", i, want, targets[i].Name)
		}
	}
}

func TestApplyManualOverlay_ReplacesByName(t *testing.T) {
	cfg := newTestGatewayCfg([]platformtypes.MCPTarget{
		{Name: "manual-composio", MCP: &platformtypes.MCPTargetSpec{Host: "https://OLD"}},
	})
	overlay := &ManualOverlay{
		MCPTargets: []platformtypes.MCPTarget{
			{Name: "manual-composio", MCP: &platformtypes.MCPTargetSpec{Host: "https://NEW"}},
		},
	}

	ApplyManualOverlay(cfg, overlay)

	targets := mustMCPTargets(t, cfg)
	if len(targets) != 1 {
		t.Fatalf("expected single target after replace, got %d", len(targets))
	}
	if targets[0].MCP.Host != "https://NEW" {
		t.Errorf("expected host replaced, got %q", targets[0].MCP.Host)
	}
}

func TestApplyManualOverlay_CreatesMCPRouteWhenAbsent(t *testing.T) {
	cfg := newTestGatewayCfg(nil)
	// Drop the auto-created (empty) MCP route to simulate a fresh listener.
	cfg.Binds[0].Listeners[0].Routes = nil

	overlay := &ManualOverlay{
		MCPTargets: []platformtypes.MCPTarget{
			{Name: "manual-fastn", MCP: &platformtypes.MCPTargetSpec{Host: "https://example.com/mcp"}},
		},
	}
	ApplyManualOverlay(cfg, overlay)

	routes := cfg.Binds[0].Listeners[0].Routes
	if len(routes) != 1 || routes[0].RouteName != localMCPRouteName {
		t.Fatalf("expected new mcp_route to be created, got %+v", routes)
	}
	targets := routes[0].Backends[0].MCP.Targets
	if len(targets) != 1 || targets[0].Name != "manual-fastn" {
		t.Errorf("expected single manual-fastn target, got %v", targetNamesOf(targets))
	}
}

func TestApplyManualOverlay_RoutesReplaceByNameAndIgnoreMCPName(t *testing.T) {
	cfg := newTestGatewayCfg(nil)
	cfg.Binds[0].Listeners[0].Routes = append(cfg.Binds[0].Listeners[0].Routes, platformtypes.LocalRoute{
		RouteName: "agent_foo_route",
		Matches:   []platformtypes.RouteMatch{{Path: platformtypes.PathMatch{PathPrefix: "/agents/foo"}}},
		Backends:  []platformtypes.RouteBackend{{Weight: 100, Host: "old-host:8080"}},
	})

	overlay := &ManualOverlay{
		Routes: []platformtypes.LocalRoute{
			{
				RouteName: "agent_foo_route",
				Matches:   []platformtypes.RouteMatch{{Path: platformtypes.PathMatch{PathPrefix: "/agents/foo"}}},
				Backends:  []platformtypes.RouteBackend{{Weight: 100, Host: "new-host:8080"}},
			},
			{
				RouteName: "manual_extra_route",
				Matches:   []platformtypes.RouteMatch{{Path: platformtypes.PathMatch{PathPrefix: "/extra"}}},
				Backends:  []platformtypes.RouteBackend{{Weight: 100, Host: "extra:9000"}},
			},
			{
				// Should be ignored — overlay must not redefine the federated MCP route.
				RouteName: localMCPRouteName,
				Matches:   []platformtypes.RouteMatch{{Path: platformtypes.PathMatch{PathPrefix: "/mcp"}}},
				Backends:  []platformtypes.RouteBackend{{Weight: 100, Host: "evil:9000"}},
			},
		},
	}

	ApplyManualOverlay(cfg, overlay)

	routes := cfg.Binds[0].Listeners[0].Routes
	byName := map[string]platformtypes.LocalRoute{}
	for _, r := range routes {
		byName[r.RouteName] = r
	}
	if got := byName["agent_foo_route"].Backends[0].Host; got != "new-host:8080" {
		t.Errorf("route replacement failed: got %q", got)
	}
	if _, ok := byName["manual_extra_route"]; !ok {
		t.Errorf("expected manual_extra_route appended, routes=%v", routeNamesOf(routes))
	}
	// Confirm we did NOT clobber the existing mcp_route via overlay.Routes.
	mcp := byName[localMCPRouteName]
	if len(mcp.Backends) > 0 && mcp.Backends[0].Host == "evil:9000" {
		t.Errorf("overlay was allowed to overwrite mcp_route — should be ignored")
	}
}

// --- helpers ---

func newTestGatewayCfg(initialTargets []platformtypes.MCPTarget) *platformtypes.AgentGatewayConfig {
	routes := []platformtypes.LocalRoute{}
	if initialTargets != nil {
		routes = append(routes, platformtypes.LocalRoute{
			RouteName: localMCPRouteName,
			Matches:   []platformtypes.RouteMatch{{Path: platformtypes.PathMatch{PathPrefix: "/mcp"}}},
			Backends: []platformtypes.RouteBackend{{
				Weight: 100,
				MCP:    &platformtypes.MCPBackend{Targets: initialTargets},
			}},
		})
	}
	return &platformtypes.AgentGatewayConfig{
		Config: struct{}{},
		Binds: []platformtypes.LocalBind{{
			Port: 21212,
			Listeners: []platformtypes.LocalListener{{
				Name:     "default",
				Protocol: platformtypes.LocalListenerProtocolHTTP,
				Routes:   routes,
			}},
		}},
	}
}

func mustMCPTargets(t *testing.T, cfg *platformtypes.AgentGatewayConfig) []platformtypes.MCPTarget {
	t.Helper()
	for _, r := range cfg.Binds[0].Listeners[0].Routes {
		if r.RouteName == localMCPRouteName {
			return r.Backends[0].MCP.Targets
		}
	}
	t.Fatalf("no mcp_route in cfg")
	return nil
}

func targetNamesOf(targets []platformtypes.MCPTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.Name
	}
	return out
}

func routeNamesOf(routes []platformtypes.LocalRoute) []string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = r.RouteName
	}
	return out
}

func TestAgentRouteBackendHost_LocalAndRemote(t *testing.T) {
	cases := []struct {
		name        string
		agent       *platformtypes.Agent
		serviceName string
		want        string
		wantErr     bool
	}{
		{
			name:        "container agent uses docker service name",
			agent:       &platformtypes.Agent{Deployment: platformtypes.AgentDeployment{Image: "ghcr.io/x/y", Port: 8123}},
			serviceName: "myagent-svc",
			want:        "myagent-svc:8123",
		},
		{
			name:        "container agent without explicit port falls back to DefaultLocalAgentPort (8080)",
			agent:       &platformtypes.Agent{Deployment: platformtypes.AgentDeployment{Image: "ghcr.io/x/y"}},
			serviceName: "noport",
			want:        "noport:8080",
		},
		{
			name: "URL-only A2A agent on host.docker.internal",
			agent: &platformtypes.Agent{
				Remote: &platformtypes.AgentRemote{Type: "a2a", URL: "http://host.docker.internal:9001"},
			},
			serviceName: "ignored-when-remote",
			want:        "host.docker.internal:9001",
		},
		{
			name: "URL-only HTTPS agent infers default port 443",
			agent: &platformtypes.Agent{
				Remote: &platformtypes.AgentRemote{Type: "a2a", URL: "https://example.com/a2a"},
			},
			serviceName: "external",
			want:        "example.com:443",
		},
		{
			name: "URL-only with empty URL errors",
			agent: &platformtypes.Agent{
				Remote: &platformtypes.AgentRemote{Type: "a2a", URL: ""},
			},
			serviceName: "x",
			wantErr:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := agentRouteBackendHost(tc.agent, tc.serviceName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (host=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
