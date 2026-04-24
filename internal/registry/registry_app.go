package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpregistry "github.com/agentregistry-dev/agentregistry/internal/mcp/registryserver"
	"github.com/agentregistry-dev/agentregistry/internal/registry/api"
	apitypes "github.com/agentregistry-dev/agentregistry/internal/registry/api/apitypes"
	"github.com/agentregistry-dev/agentregistry/internal/registry/api/router"
	"github.com/agentregistry-dev/agentregistry/internal/registry/config"
	internaldb "github.com/agentregistry-dev/agentregistry/internal/registry/database"
	"github.com/agentregistry-dev/agentregistry/internal/registry/embeddings"
	"github.com/agentregistry-dev/agentregistry/internal/registry/importer"
	"github.com/agentregistry-dev/agentregistry/internal/registry/jobs"
	"github.com/agentregistry-dev/agentregistry/internal/registry/kinds"
	"github.com/agentregistry-dev/agentregistry/internal/registry/platforms/kubernetes"
	"github.com/agentregistry-dev/agentregistry/internal/registry/platforms/local"
	"github.com/agentregistry-dev/agentregistry/internal/registry/seed"
	"github.com/agentregistry-dev/agentregistry/internal/registry/service"
	agentsvc "github.com/agentregistry-dev/agentregistry/internal/registry/service/agent"
	deploymentsvc "github.com/agentregistry-dev/agentregistry/internal/registry/service/deployment"
	promptsvc "github.com/agentregistry-dev/agentregistry/internal/registry/service/prompt"
	providersvc "github.com/agentregistry-dev/agentregistry/internal/registry/service/provider"
	serversvc "github.com/agentregistry-dev/agentregistry/internal/registry/service/server"
	skillsvc "github.com/agentregistry-dev/agentregistry/internal/registry/service/skill"
	"github.com/agentregistry-dev/agentregistry/internal/registry/telemetry"
	"github.com/agentregistry-dev/agentregistry/internal/version"
	"github.com/agentregistry-dev/agentregistry/pkg/logging"
	"github.com/agentregistry-dev/agentregistry/pkg/models"
	"github.com/agentregistry-dev/agentregistry/pkg/registry/auth"
	"github.com/agentregistry-dev/agentregistry/pkg/registry/database"
	"github.com/agentregistry-dev/agentregistry/pkg/types"
)

func App(ctx context.Context, opts ...types.AppOptions) error {
	var options types.AppOptions
	if len(opts) > 0 {
		options = opts[0]
	}
	cfg := config.NewConfig()
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	// Create a context with timeout for PostgreSQL connection
	dbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	setupLogging(cfg.LogLevel)

	// Build auth providers from options (before database creation)
	// Only create jwtManager if JWT is configured
	var jwtManager *auth.JWTManager
	if cfg.JWTPrivateKey != "" {
		jwtManager = auth.NewJWTManager(cfg)
	}

	// Resolve authn provider: use provided, or default to JWT-based if configured
	authnProvider := options.AuthnProvider
	if authnProvider == nil && jwtManager != nil {
		authnProvider = jwtManager
	}

	// Resolve authz provider: use provided, or default to public authz
	authzProvider := options.AuthzProvider
	if authzProvider == nil {
		slog.Info("using public authz provider")
		authzProvider = auth.NewPublicAuthzProvider(jwtManager)
	}
	authz := auth.Authorizer{Authz: authzProvider}

	// Database selection: use DATABASE_URL="noop" only when you provide the database
	// entirely via AppOptions.DatabaseFactory (e.g. in-memory or custom backend) and
	// do not want a real PostgreSQL connection. In that case DatabaseFactory is required.
	// For normal deployments, set DATABASE_URL to a real Postgres connection string.
	var db database.Store
	if cfg.DatabaseURL == "noop" { //nolint:nestif
		if options.DatabaseFactory == nil {
			return fmt.Errorf("DATABASE_URL=noop requires DatabaseFactory to be set in AppOptions")
		}
		slog.Info("using DatabaseFactory to create database", "mode", "noop")
		var err error
		db, err = options.DatabaseFactory(ctx, "", nil, authz)
		if err != nil {
			return fmt.Errorf("failed to create database via factory: %w", err)
		}
	} else {
		baseDB, err := internaldb.NewPostgreSQL(dbCtx, cfg.DatabaseURL, authz, cfg.DatabaseVectorEnabled)
		if err != nil {
			return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
		}

		// Allow implementors to wrap the database and run additional migrations
		db = baseDB
		if options.DatabaseFactory != nil {
			db, err = options.DatabaseFactory(ctx, cfg.DatabaseURL, baseDB, authz)
			if err != nil {
				if err := baseDB.Close(); err != nil {
					slog.Error("error closing base database connection", "error", err)
				}
				return fmt.Errorf("failed to create extended database: %w", err)
			}
		}
	}

	// Store the database instance for later cleanup
	defer func() {
		if err := db.Close(); err != nil {
			slog.Error("error closing database connection", "error", err)
		} else {
			slog.Info("database connection closed successfully")
		}
	}()

	var embeddingProvider embeddings.Provider
	if cfg.Embeddings.Enabled {
		client := &http.Client{Timeout: 30 * time.Second}
		if provider, err := embeddings.Factory(&cfg.Embeddings, client); err != nil {
			slog.Warn("semantic embeddings disabled", "error", err)
		} else {
			embeddingProvider = provider
		}
	}

	serverService := serversvc.New(serversvc.Dependencies{
		Servers:            db.Servers(),
		Tx:                 db,
		Config:             cfg,
		EmbeddingsProvider: embeddingProvider,
	})
	agentService := agentsvc.New(agentsvc.Dependencies{
		Agents:             db.Agents(),
		Skills:             db.Skills(),
		Prompts:            db.Prompts(),
		Tx:                 db,
		Config:             cfg,
		EmbeddingsProvider: embeddingProvider,
	})
	providerService := providersvc.New(providersvc.Dependencies{
		StoreDB:           db,
		ProviderPlatforms: options.ProviderPlatforms,
	})
	providerPlatforms := providerService.PlatformAdapters()
	deploymentPlatforms := map[string]types.DeploymentPlatformAdapter{
		"local":      local.NewLocalDeploymentAdapter(serverService, agentService, cfg.RuntimeDir, cfg.AgentGatewayPort),
		"kubernetes": kubernetes.NewKubernetesDeploymentAdapter(providerService, serverService, agentService),
	}
	maps.Copy(deploymentPlatforms, options.DeploymentPlatforms)
	skillService := skillsvc.New(skillsvc.Dependencies{Skills: db.Skills(), Tx: db})
	promptService := promptsvc.New(promptsvc.Dependencies{Prompts: db.Prompts(), Tx: db})
	deploymentService := deploymentsvc.New(deploymentsvc.Dependencies{
		StoreDB:            db,
		Deployments:        db.Deployments(),
		Providers:          providerService,
		Servers:            serverService,
		Agents:             agentService,
		DeploymentAdapters: deploymentPlatforms,
	})
	// Import builtin seed data unless it is disabled
	if !cfg.DisableBuiltinSeed {
		slog.Info("importing builtin seed data in the background")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			ctx = auth.WithSystemContext(ctx)

			if err := seed.ImportBuiltinSeedData(ctx, serverService); err != nil {
				slog.Error("failed to import builtin seed data", "error", err)
			}
		}()
	}

	// Reconcile-on-startup: re-apply every active deployment via its platform
	// adapter. Two reasons:
	//   1. The on-disk runtime dir (agent-gateway.yaml + docker-compose.yaml)
	//      may be missing or stale (server reboot wiped /tmp; runtime dir was
	//      moved; first boot in a fresh environment). The DB is the source of
	//      truth; this reconstructs the runtime files from it.
	//   2. The local platform adapter's manual-overlay merge runs on every
	//      Deploy() call — re-deploying ensures the latest overlay is applied
	//      even if no /v0/deployments POST has happened recently.
	// Runs in the background after a short delay so HTTP serving comes up
	// immediately. Failures are logged but never crash the daemon.
	go func() {
		// Give the HTTP listener and DB connection pool a moment to settle
		// before issuing reads + adapter calls.
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		ctx = auth.WithSystemContext(ctx)
		reconcileActiveDeploymentsOnStartup(ctx, deploymentService)
	}()

	// Import seed data if seed source is provided
	if cfg.SeedFrom != "" {
		slog.Info("importing data in the background", "seed_from", cfg.SeedFrom)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			ctx = auth.WithSystemContext(ctx)

			importerService := importer.NewService(serverService)
			if embeddingProvider != nil {
				importerService.SetEmbeddingProvider(embeddingProvider)
				importerService.SetEmbeddingDimensions(cfg.Embeddings.Dimensions)
				importerService.SetGenerateEmbeddings(cfg.Embeddings.Enabled)
			}
			if err := importerService.ImportFromPath(ctx, cfg.SeedFrom, cfg.EnrichServerData); err != nil {
				slog.Error("failed to import seed data", "error", err)
			}
		}()
	}

	slog.Info("starting agentregistry", "version", version.Version, "commit", version.GitCommit)

	// Prepare version information
	versionInfo := &apitypes.VersionBody{
		Version:   version.Version,
		GitCommit: version.GitCommit,
		BuildTime: version.BuildDate,
	}

	shutdownTelemetry, metrics, err := telemetry.InitMetrics(cfg.Version)
	if err != nil {
		return fmt.Errorf("failed to initialize metrics: %v", err)
	}

	defer func() {
		if err := shutdownTelemetry(context.Background()); err != nil {
			slog.Error("failed to shutdown telemetry", "error", err)
		}
	}()

	// Build the kind registry and register all 6 OSS kinds.
	kindReg := kinds.NewRegistry()
	kindReg.Register(kinds.Kind{
		Kind:     "agent",
		Plural:   "agents",
		Aliases:  []string{"Agent"},
		SpecType: reflect.TypeFor[kinds.AgentSpec](),
		Apply:    kinds.MakeApplyFunc("agent", kinds.ToAgentJSON, agentService.ApplyAgent, agentService.GetAgentVersion),
		Get:      kinds.MakeGetFunc(agentService.GetAgent, agentService.GetAgentVersion),
		Delete:   agentService.DeleteAgent,
		TableColumns: []kinds.Column{
			{Header: "NAME"}, {Header: "VERSION"}, {Header: "FRAMEWORK"},
			{Header: "LANGUAGE"}, {Header: "PROVIDER"}, {Header: "MODEL"},
		},
		InitTemplate: kinds.MakeInitTemplate("agent", kinds.AgentSpec{Description: "TODO: describe your agent"}),
	})
	kindReg.Register(kinds.Kind{
		Kind:     "skill",
		Plural:   "skills",
		Aliases:  []string{"Skill"},
		SpecType: reflect.TypeFor[kinds.SkillSpec](),
		Apply:    kinds.MakeApplyFunc("skill", kinds.ToSkillJSON, skillService.ApplySkill, skillService.GetSkillVersion),
		Get:      kinds.MakeGetFunc(skillService.GetSkill, skillService.GetSkillVersion),
		Delete:   skillService.DeleteSkill,
		TableColumns: []kinds.Column{
			{Header: "NAME"}, {Header: "VERSION"}, {Header: "CATEGORY"}, {Header: "DESCRIPTION"},
		},
		InitTemplate: kinds.MakeInitTemplate("skill", kinds.SkillSpec{Description: "TODO: describe your skill"}),
	})
	kindReg.Register(kinds.Kind{
		Kind:     "prompt",
		Plural:   "prompts",
		Aliases:  []string{"Prompt"},
		SpecType: reflect.TypeFor[kinds.PromptSpec](),
		Apply:    kinds.MakeApplyFunc("prompt", kinds.ToPromptJSON, promptService.ApplyPrompt, promptService.GetPromptVersion),
		Get:      kinds.MakeGetFunc(promptService.GetPrompt, promptService.GetPromptVersion),
		Delete:   promptService.DeletePrompt,
		TableColumns: []kinds.Column{
			{Header: "NAME"}, {Header: "VERSION"}, {Header: "DESCRIPTION"},
		},
		InitTemplate: kinds.MakeInitTemplate("prompt", kinds.PromptSpec{Description: "TODO: describe your prompt", Content: "TODO: write your prompt content"}),
	})
	kindReg.Register(kinds.Kind{
		Kind:     "mcp",
		Plural:   "mcps",
		Aliases:  []string{"MCPServer", "mcpserver", "mcp-server", "mcpservers"},
		SpecType: reflect.TypeFor[kinds.MCPSpec](),
		Apply:    kinds.MakeApplyFunc("mcp", kinds.ToServerJSON, serverService.ApplyServer, serverService.GetServerVersion),
		Get:      kinds.MakeGetFunc(serverService.GetServer, serverService.GetServerVersion),
		Delete:   serverService.DeleteServer,
		TableColumns: []kinds.Column{
			{Header: "NAME"}, {Header: "VERSION"}, {Header: "DESCRIPTION"},
		},
		InitTemplate: kinds.MakeInitTemplate("mcp", kinds.MCPSpec{Description: "TODO: describe your MCP server"}),
	})
	kindReg.Register(kinds.Kind{
		Kind:     "provider",
		Plural:   "providers",
		Aliases:  []string{"Provider"},
		SpecType: reflect.TypeFor[kinds.ProviderSpec](),
		Apply:    providerApplyFunc(providerService),
		Get:      func(ctx context.Context, name, _ string) (any, error) { return providerService.GetProvider(ctx, name) },
		Delete:   func(ctx context.Context, name, _ string) error { return providerService.DeleteProvider(ctx, name, "") },
		TableColumns: []kinds.Column{
			{Header: "NAME"}, {Header: "PLATFORM"},
		},
		InitTemplate: kinds.MakeInitTemplate("provider", kinds.ProviderSpec{
			Platform: "kubernetes",
		}),
	})
	kindReg.Register(kinds.Kind{
		Kind:     "deployment",
		Plural:   "deployments",
		Aliases:  []string{"Deployment"},
		SpecType: reflect.TypeFor[kinds.DeploymentSpec](),
		Apply:    deploymentApplyFunc(deploymentService),
		TableColumns: []kinds.Column{
			{Header: "NAME"}, {Header: "VERSION"}, {Header: "RESOURCE_TYPE"},
			{Header: "PROVIDER"}, {Header: "STATUS"},
		},
	})

	routeOpts := &router.RouteOptions{
		ProviderPlatforms:   providerPlatforms,
		DeploymentPlatforms: deploymentPlatforms,
		ExtraRoutes:         options.ExtraRoutes,
		KindRegistry:        kindReg,
	}

	// Initialize job manager and indexer for embeddings.
	if cfg.Embeddings.Enabled && embeddingProvider != nil {
		jobManager := jobs.NewManager()
		indexer := service.NewIndexer(serverService, agentService, embeddingProvider, cfg.Embeddings.Dimensions)
		routeOpts.Indexer = indexer
		routeOpts.JobManager = jobManager
		slog.Info("embeddings indexing API enabled")
	}

	// Initialize HTTP server
	baseServer := api.NewServer(cfg, router.RegistryServices{
		Server:     serverService,
		Agent:      agentService,
		Skill:      skillService,
		Prompt:     promptService,
		Provider:   providerService,
		Deployment: deploymentService,
	}, metrics, versionInfo, options.UIHandler, authnProvider, routeOpts)

	var server types.Server
	if options.HTTPServerFactory != nil {
		server = options.HTTPServerFactory(baseServer, db)
	} else {
		server = baseServer
	}

	if options.OnHTTPServerCreated != nil {
		options.OnHTTPServerCreated(server)
	}

	var mcpHTTPServer *http.Server
	if cfg.MCPPort > 0 {
		mcpServer := mcpregistry.NewServer(serverService, agentService, skillService, deploymentService)

		var handler http.Handler = mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
			return mcpServer
		}, &mcp.StreamableHTTPOptions{})

		// Set up authentication middleware if one is configured
		if authnProvider != nil {
			handler = mcpAuthnMiddleware(authnProvider)(handler)
		}

		addr := ":" + strconv.Itoa(int(cfg.MCPPort))
		mcpHTTPServer = &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}

		go func() {
			slog.Info("MCP HTTP server starting", "address", addr)
			if err := mcpHTTPServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("failed to start MCP server", "error", err)
				os.Exit(1)
			}
		}()
	}

	// Start server in a goroutine so it doesn't block signal handling
	go func() {
		if err := server.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("failed to start server", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)

	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down server")

	// Create context with timeout for shutdown
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()

	// Gracefully shutdown the server
	if err := server.Shutdown(sctx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}
	if mcpHTTPServer != nil {
		if err := mcpHTTPServer.Shutdown(sctx); err != nil {
			slog.Error("MCP server forced to shutdown", "error", err)
		}
	}

	slog.Info("server exiting")
	return nil
}

// mcpAuthnMiddleware creates a middleware that uses the AuthnProvider to authenticate requests and add to session context.
// this session context is used by the db + authz provider to check permissions.
func mcpAuthnMiddleware(authn auth.AuthnProvider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			// authenticate using the configured provider
			session, err := authn.Authenticate(ctx, r.Header.Get, r.URL.Query())
			if err == nil && session != nil {
				ctx = auth.AuthSessionTo(ctx, session)
				r = r.WithContext(ctx)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// setupLogging configures the global slog logger
func setupLogging(levelStr string) {
	logging.SetupDefault()

	if levelStr == "" {
		levelStr = "info"
	}
	level, err := logging.ParseLevel(levelStr)
	if err != nil {
		slog.Error("failed to parse log level, defaulting to info", "error", err)
		level = slog.LevelInfo
	}
	// set all loggers to the specified level
	logging.Reset(level)
}

// providerApplyFunc returns the Apply function for the provider kind.
// Extracted from the inline registration to keep the registration block declarative.
func providerApplyFunc(svc providersvc.Registry) kinds.ApplyFunc {
	return func(ctx context.Context, doc *kinds.Document, opts kinds.ApplyOpts) (*kinds.Result, error) {
		spec, err := kinds.AssertSpec[kinds.ProviderSpec]("provider", doc)
		if err != nil {
			return nil, err
		}
		if spec.Platform == "" {
			return nil, fmt.Errorf("provider: spec.platform is required")
		}
		if opts.DryRun {
			_, err := svc.GetProvider(ctx, doc.Metadata.Name)
			if err != nil {
				return &kinds.Result{Kind: "provider", Name: doc.Metadata.Name, Status: kinds.StatusCreated}, nil
			}
			return &kinds.Result{Kind: "provider", Name: doc.Metadata.Name, Status: kinds.StatusConfigured}, nil
		}
		name := doc.Metadata.Name
		cfg := spec.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		if _, err := svc.ApplyProvider(ctx, name, spec.Platform, &models.UpdateProviderInput{
			Name: &name, Config: cfg,
		}); err != nil {
			return nil, err
		}
		return kinds.AppliedResult("provider", doc), nil
	}
}

// deploymentApplyFunc returns the Apply function for the deployment kind.
// Extracted from the inline registration to keep the registration block declarative.
func deploymentApplyFunc(svc deploymentsvc.Registry) kinds.ApplyFunc {
	return func(ctx context.Context, doc *kinds.Document, opts kinds.ApplyOpts) (*kinds.Result, error) {
		spec, err := kinds.AssertSpec[kinds.DeploymentSpec]("deployment", doc)
		if err != nil {
			return nil, err
		}
		if spec.ProviderID == "" {
			return nil, fmt.Errorf("deployment: spec.providerId is required")
		}
		rt := spec.ResourceType
		// "server" is accepted as an alias for "mcp" for backwards compatibility.
		if rt != "agent" && rt != "mcp" && rt != "server" {
			return nil, fmt.Errorf("deployment: spec.resourceType must be one of \"agent\", \"mcp\", \"server\"; got %q", rt)
		}
		if opts.DryRun {
			resourceName := doc.Metadata.Name
			existing, listErr := svc.ListDeployments(ctx, &models.DeploymentFilter{ResourceName: &resourceName})
			status := kinds.StatusCreated
			if listErr == nil {
				for _, d := range existing {
					if d.ServerName == doc.Metadata.Name && (doc.Metadata.Version == "" || d.Version == doc.Metadata.Version) {
						status = kinds.StatusConfigured
						break
					}
				}
			}
			return &kinds.Result{Kind: "deployment", Name: doc.Metadata.Name, Version: doc.Metadata.Version, Status: status}, nil
		}
		if rt == "agent" {
			if _, err := svc.ApplyAgentDeployment(ctx, doc.Metadata.Name, doc.Metadata.Version, spec.ProviderID, spec.Env, spec.ProviderConfig, spec.PreferRemote, opts.Force); err != nil {
				return nil, err
			}
		} else {
			if _, err := svc.ApplyServerDeployment(ctx, doc.Metadata.Name, doc.Metadata.Version, spec.ProviderID, spec.Env, spec.ProviderConfig, spec.PreferRemote, opts.Force); err != nil {
				return nil, err
			}
		}
		return kinds.AppliedResult("deployment", doc), nil
	}
}

// reconcileActiveDeploymentsOnStartup walks every deployment record with
// status=deployed and re-issues an idempotent ApplyDeployment so the platform
// adapter rebuilds its on-disk runtime state (agent-gateway.yaml, compose
// project) from the DB. Without this, a daemon process that boots into a
// fresh runtime dir (random suffix, /tmp wipe, manual relocation) leaves the
// gateway with an empty config until the next deploy event lands — and that
// event then "wins alone," wiping every other backend.
//
// Errors per deployment are logged and skipped; the goal is best-effort
// recovery, not a hard guarantee. The function is safe to call concurrently
// with the HTTP serving loop because each ApplyDeployment is its own
// transactional unit.
func reconcileActiveDeploymentsOnStartup(ctx context.Context, svc deploymentsvc.Registry) {
	deployedStatus := models.DeploymentStatusDeployed
	deployments, err := svc.ListDeployments(ctx, &models.DeploymentFilter{Status: &deployedStatus})
	if err != nil {
		slog.Error("startup reconcile: failed to list deployments", "error", err)
		return
	}
	if len(deployments) == 0 {
		slog.Info("startup reconcile: no active deployments")
		return
	}
	slog.Info("startup reconcile: re-applying active deployments", "count", len(deployments))
	var ok, fail int
	for _, d := range deployments {
		if d == nil {
			continue
		}
		var err error
		if d.ResourceType == "agent" {
			_, err = svc.ApplyAgentDeployment(ctx, d.ServerName, d.Version, d.ProviderID, d.Env, d.ProviderConfig, d.PreferRemote, false)
		} else {
			_, err = svc.ApplyServerDeployment(ctx, d.ServerName, d.Version, d.ProviderID, d.Env, d.ProviderConfig, d.PreferRemote, false)
		}
		if err != nil {
			slog.Warn("startup reconcile: deployment re-apply failed",
				"id", d.ID, "name", d.ServerName, "version", d.Version, "error", err)
			fail++
			continue
		}
		ok++
	}
	slog.Info("startup reconcile: complete", "succeeded", ok, "failed", fail)
}
