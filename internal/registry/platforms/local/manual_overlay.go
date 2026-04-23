package local

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	platformtypes "github.com/agentregistry-dev/agentregistry/internal/registry/platforms/types"
	"go.yaml.in/yaml/v3"
)

// manualOverlayFileName is the file name (under platformDir) that holds
// hand-defined gateway entries which the reconciler merges into the
// generated agent-gateway.yaml on every reconcile pass. Lets ops teams
// expose extra MCPs (Composio, Fastn, etc.) without going through
// `arctl deployments create`.
const manualOverlayFileName = "manual-overlay.yaml"

// ManualOverlay is a small subset of AgentGatewayConfig that ops can author
// by hand. Names should be prefixed `manual-` so they cannot collide with
// deployment-managed entries (whose names are derived from deployment IDs).
type ManualOverlay struct {
	// MCPTargets are merged into the federated `mcp_route` MCPBackend.
	// Entries with a Name matching an existing target replace it; otherwise
	// they are appended.
	MCPTargets []platformtypes.MCPTarget `yaml:"mcpTargets,omitempty"`

	// Routes are appended as additional non-MCP routes on the first listener.
	// Entries with a Name matching an existing route replace it. Entries
	// whose Name equals the federated MCP route are ignored.
	Routes []platformtypes.LocalRoute `yaml:"routes,omitempty"`
}

// envVarPattern matches ${VAR_NAME}. Shell-style defaults like ${FOO:-bar}
// are intentionally not supported — keep the file simple.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// LoadManualOverlay reads <platformDir>/manual-overlay.yaml. Returns
// (nil, nil) if the file does not exist. Applies ${ENV_VAR} interpolation
// before YAML parsing so secrets stay out of the file on disk.
func LoadManualOverlay(platformDir string) (*ManualOverlay, error) {
	path := filepath.Join(platformDir, manualOverlayFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read manual overlay %s: %w", path, err)
	}
	expanded := expandEnvVars(raw)
	var overlay ManualOverlay
	if err := yaml.Unmarshal(expanded, &overlay); err != nil {
		return nil, fmt.Errorf("parse manual overlay %s: %w", path, err)
	}
	return &overlay, nil
}

// expandEnvVars replaces every ${VAR_NAME} occurrence with the corresponding
// process env var. Unset vars are left verbatim so misconfigurations surface
// as obvious YAML strings (e.g. "${MISSING}") instead of empty values.
func expandEnvVars(raw []byte) []byte {
	return envVarPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		m := envVarPattern.FindSubmatch(match)
		if len(m) < 2 {
			return match
		}
		if v, ok := os.LookupEnv(string(m[1])); ok {
			return []byte(v)
		}
		return match
	})
}

// ApplyManualOverlay merges overlay into gatewayCfg in place.
//
// Merge semantics:
//   - overlay.MCPTargets: each target replaces any existing MCP target with
//     the same Name in the federated mcp_route backend, or is appended if
//     no match. If the federated route does not yet exist, it is created.
//   - overlay.Routes: each route replaces any existing non-MCP route with
//     the same Name on the first listener, or is appended. Entries with
//     RouteName == localMCPRouteName are skipped (use MCPTargets instead).
//
// No-op when gatewayCfg or overlay is nil/empty, or when there is no listener
// on the first bind.
func ApplyManualOverlay(gatewayCfg *platformtypes.AgentGatewayConfig, overlay *ManualOverlay) {
	if gatewayCfg == nil || overlay == nil {
		return
	}
	if len(overlay.MCPTargets) == 0 && len(overlay.Routes) == 0 {
		return
	}
	if len(gatewayCfg.Binds) == 0 || len(gatewayCfg.Binds[0].Listeners) == 0 {
		return
	}
	listener := &gatewayCfg.Binds[0].Listeners[0]

	if len(overlay.MCPTargets) > 0 {
		mergeOverlayMCPTargets(listener, overlay.MCPTargets)
	}
	if len(overlay.Routes) > 0 {
		mergeOverlayRoutes(listener, overlay.Routes)
	}
}

func mergeOverlayMCPTargets(listener *platformtypes.LocalListener, targets []platformtypes.MCPTarget) {
	mcpRouteIdx := -1
	for i, route := range listener.Routes {
		if route.RouteName == localMCPRouteName {
			mcpRouteIdx = i
			break
		}
	}

	if mcpRouteIdx == -1 {
		// No federated MCP route yet — create one populated with overlay
		// targets only. Prepend so it appears before any agent routes,
		// matching the order produced by translateLocalAgentGatewayConfig.
		newRoute := platformtypes.LocalRoute{
			RouteName: localMCPRouteName,
			Matches: []platformtypes.RouteMatch{{
				Path: platformtypes.PathMatch{PathPrefix: "/mcp"},
			}},
			Backends: []platformtypes.RouteBackend{{
				Weight: 100,
				MCP:    &platformtypes.MCPBackend{Targets: append([]platformtypes.MCPTarget{}, targets...)},
			}},
		}
		listener.Routes = append([]platformtypes.LocalRoute{newRoute}, listener.Routes...)
		return
	}

	mcpRoute := &listener.Routes[mcpRouteIdx]
	if len(mcpRoute.Backends) == 0 {
		mcpRoute.Backends = []platformtypes.RouteBackend{{Weight: 100, MCP: &platformtypes.MCPBackend{}}}
	} else if mcpRoute.Backends[0].MCP == nil {
		mcpRoute.Backends[0].MCP = &platformtypes.MCPBackend{}
	}
	existing := mcpRoute.Backends[0].MCP.Targets
	for _, ovl := range targets {
		replaced := false
		for i := range existing {
			if existing[i].Name == ovl.Name {
				existing[i] = ovl
				replaced = true
				break
			}
		}
		if !replaced {
			existing = append(existing, ovl)
		}
	}
	mcpRoute.Backends[0].MCP.Targets = existing
}

func mergeOverlayRoutes(listener *platformtypes.LocalListener, routes []platformtypes.LocalRoute) {
	for _, ovl := range routes {
		if ovl.RouteName == localMCPRouteName {
			// Overriding the federated MCP route via overlay routes is
			// almost certainly a mistake — silently ignore.
			continue
		}
		replaced := false
		for i := range listener.Routes {
			if listener.Routes[i].RouteName == ovl.RouteName {
				listener.Routes[i] = ovl
				replaced = true
				break
			}
		}
		if !replaced {
			listener.Routes = append(listener.Routes, ovl)
		}
	}
}
