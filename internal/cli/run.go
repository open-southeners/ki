package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/compose"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/containers"
	"github.com/open-southeners/ki-agent/internal/controlplane"
	"github.com/open-southeners/ki-agent/internal/deploys"
	"github.com/open-southeners/ki-agent/internal/edge"
	"github.com/open-southeners/ki-agent/internal/enroll"
	"github.com/open-southeners/ki-agent/internal/enrollment"
	"github.com/open-southeners/ki-agent/internal/git"
	"github.com/open-southeners/ki-agent/internal/projects"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/runtime/docker"
	"github.com/open-southeners/ki-agent/internal/secret"
	"github.com/open-southeners/ki-agent/internal/server"
	"github.com/open-southeners/ki-agent/internal/store"
	"github.com/open-southeners/ki-agent/internal/version"
	"github.com/open-southeners/ki-agent/internal/webhooks"
)

func newRunCmd(f *flags) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the agent in the foreground until SIGINT or SIGTERM",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			explicit := cmd.Flags().Changed("config")
			return runAgent(ctx, cmd.ErrOrStderr(), f, explicit)
		},
	}
}

// runAgent loads config, prepares the data directory and serves until ctx is
// cancelled. configExplicit makes a missing config file an error.
func runAgent(ctx context.Context, logOut io.Writer, f *flags, configExplicit bool) error {
	if configExplicit {
		if _, err := os.Stat(f.configPath); err != nil {
			return fmt.Errorf("config file: %w", err)
		}
	}
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return err
	}
	if f.logLevel != "" {
		cfg.LogLevel = f.logLevel
	}
	level, err := config.ParseLogLevel(cfg.LogLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: level}))

	for _, d := range []struct {
		path string
		perm os.FileMode
	}{
		{cfg.DataDir, 0o750},
		{filepath.Join(cfg.DataDir, "tmp"), 0o750},
		{filepath.Join(cfg.DataDir, "projects"), 0o750},
		{filepath.Join(cfg.DataDir, "git", "home"), 0o750},
		{filepath.Join(cfg.DataDir, "deploys"), 0o700},
	} {
		if err := os.MkdirAll(d.path, d.perm); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}
	}

	logger.Info("ki-agent starting",
		"version", version.Version, "commit", version.Commit, "data_dir", cfg.DataDir)

	// The engine connection is lazy: the agent starts even when Docker is
	// down and container methods answer RuntimeUnavailable until it is up.
	rt, err := docker.New(cfg.Docker.Host)
	if err != nil {
		return err
	}
	defer rt.Close()
	pingCtx, cancelPing := context.WithTimeout(ctx, 3*time.Second)
	if info, err := rt.Ping(pingCtx); err != nil {
		logger.Warn("container runtime unreachable; container methods will fail until it is",
			"host", cfg.Docker.Host, "err", err)
	} else {
		logger.Info("container runtime connected", "host", cfg.Docker.Host, "version", info.Version)
	}
	cancelPing()

	box, err := secret.LoadOrCreate(filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	gitClient, err := git.New(git.Options{DataDir: cfg.DataDir})
	if err != nil {
		return err
	}
	projSvc, err := projects.New(projects.Options{
		Store: db, Secret: box, Git: gitClient, Runtime: rt, DataDir: cfg.DataDir, Logger: logger,
	})
	if err != nil {
		return err
	}

	composeRunner := &compose.Runner{
		CLI:        cfg.Docker.CLI,
		DockerHost: cfg.Docker.Host,
		Home:       filepath.Join(cfg.DataDir, "git", "home"),
		Logger:     logger,
	}
	// The edge routes to the real webhook listener, so the port it targets
	// follows the effective listen address.
	webhookListen := effectiveWebhooksListen(cfg, logger)
	edgeMgr := edge.New(edge.Options{
		Config: cfg.Edge, DataDir: cfg.DataDir,
		Compose: composeRunner, Runtime: rt, Exposures: db, Logger: logger,
	})
	defer edgeMgr.Close() // leaves the proxy container running, by design
	projSvc.SetReservedHostnames(edgeMgr.ServerHostnames)
	projSvc.OnExposureChange(edgeMgr.Trigger)
	projSvc.OnDeleted(func(context.Context, string) { edgeMgr.Trigger() })

	// The deploy manager is closed (cancelling and recording running deploys)
	// before the store closes: defers run last-in first-out.
	deployMgr, err := deploys.New(deploys.Options{
		Store:         db,
		Projects:      projSvc,
		Git:           gitClient,
		Compose:       composeRunner,
		OnFinished:    func(string) { edgeMgr.Trigger() },
		Runtime:       rt,
		DataDir:       cfg.DataDir,
		HealthTimeout: cfg.Deploys.HealthTimeout,
		Keep:          cfg.Deploys.Keep,
		LogMaxBytes:   int(cfg.Deploys.LogMaxBytes),
		Logger:        logger,
	})
	if err != nil {
		return err
	}
	if err := deployMgr.Start(ctx); err != nil {
		return err
	}
	defer deployMgr.Close()
	projSvc.OnDelete(deployMgr.Cancel)

	tlsCfg, err := cfg.ControlPlane.TLSConfig()
	if err != nil {
		return err
	}
	router := rpc.NewRouter()
	ag := agent.New()
	ag.Register(router)
	containers.New(rt, logger).Register(router)
	projects.NewHandlers(projSvc).Register(router)
	deploys.NewHandlers(deployMgr).Register(router)
	edgeHandlers := edge.NewHandlers(edgeMgr, filepath.Join(cfg.DataDir, "edge"), logger)
	edgeHandlers.DomainOwner = func(ctx context.Context, domain string) (string, string, error) {
		return db.DomainOwner(ctx, domain)
	}
	edgeHandlers.Register(router)
	srv, err := server.New(server.Options{Config: cfg, Router: router, Logger: logger})
	if err != nil {
		return err
	}
	mgr := controlplane.NewManager(logger)
	enr := enrollment.New(enrollment.Options{
		Client:  enroll.NewClient(enroll.Options{TLSConfig: tlsCfg, UserAgent: "ki-agent/" + version.Version}),
		Manager: mgr, Config: cfg.ControlPlane, DataDir: cfg.DataDir, DefaultURL: version.EnrollURL,
		Serve:  func(ctx context.Context, c rpc.Conn) { srv.ServeConn(ctx, rpc.TransportControlPlane, c) },
		Logger: logger,
	})
	enr.Register(router)
	ag.SetControlPlane(mgr.StatusAny)
	if err := enr.Apply(); err != nil {
		return err
	}
	if err := srv.Listen(); err != nil {
		return err
	}

	// Started once the listeners are up; reconcile problems never fail the agent.
	if err := edgeMgr.Start(ctx); err != nil {
		return err
	}

	if webhookListen != "" {
		ln, err := net.Listen("tcp", webhookListen)
		if err != nil {
			return fmt.Errorf("webhooks listen: %w", err)
		}
		whSrv := &webhooks.Server{
			Addr:        webhookListen,
			TLSCertFile: cfg.Webhooks.TLSCertFile,
			TLSKeyFile:  cfg.Webhooks.TLSKeyFile,
			Logger:      logger,
			Handler: webhooks.NewHandler(webhooks.Options{
				Projects:     webhookProjects{projSvc},
				Deployer:     deployMgr.Deployer(),
				MaxBodyBytes: cfg.Webhooks.MaxBodyBytes,
				Logger:       logger,
			}),
		}
		whCtx, stopWH := context.WithCancel(ctx)
		whDone := make(chan struct{})
		go func() {
			defer close(whDone)
			if err := whSrv.Serve(whCtx, ln); err != nil {
				logger.Error("webhook listener failed", "err", err)
			}
		}()
		defer func() {
			stopWH()
			<-whDone
		}()
	}

	// On shutdown the manager stops dialing at once and its live connection
	// is closed by the server's peer tracking. cpCtx also ends it if Serve
	// fails without ctx being cancelled.
	cpCtx, stopCP := context.WithCancel(ctx)
	defer stopCP()
	cpDone := make(chan struct{})
	go func() {
		defer close(cpDone)
		_ = mgr.Run(cpCtx)
	}()
	err = srv.Serve(ctx)
	stopCP()
	<-cpDone
	if err != nil {
		return err
	}
	logger.Info("ki-agent stopped")
	return nil
}

// effectiveWebhooksListen returns the address the webhook listener binds and
// aligns cfg.Edge.WebhooksPort with it, so the edge always routes /hooks to
// the real listener. With the edge enabled and no explicit address the
// listener binds the loopback port edge.webhooks_port; without either, there
// is no listener (empty result).
func effectiveWebhooksListen(cfg *config.Config, logger *slog.Logger) string {
	if !cfg.Edge.Enabled {
		return cfg.Webhooks.Listen
	}
	if cfg.Webhooks.Listen == "" {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Edge.WebhooksPort))
	}
	host, portStr, err := net.SplitHostPort(cfg.Webhooks.Listen)
	if err != nil { // config validation rejects this; keep the listener as is
		return cfg.Webhooks.Listen
	}
	if port, err := strconv.Atoi(portStr); err == nil && port != cfg.Edge.WebhooksPort {
		logger.Info("edge routes /hooks to the webhooks.listen port instead of edge.webhooks_port",
			"port", port, "edge_webhooks_port", cfg.Edge.WebhooksPort)
		cfg.Edge.WebhooksPort = port
	}
	if ip := net.ParseIP(host); host != "" && !(ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		logger.Warn("webhooks.listen is neither loopback nor a wildcard address: /hooks through the edge targets 127.0.0.1 only and will not reach it",
			"listen", cfg.Webhooks.Listen, "edge_target", "127.0.0.1:"+strconv.Itoa(cfg.Edge.WebhooksPort))
	}
	if cfg.Webhooks.TLSEnabled() {
		logger.Warn("webhooks.tls_* is set: the edge forwards /hooks over plain HTTP and cannot reach a TLS listener; "+
			"drop the TLS files and let the edge terminate TLS",
			"listen", cfg.Webhooks.Listen)
	}
	return cfg.Webhooks.Listen
}

// webhookProjects adapts the projects service to webhooks.Projects. An
// unknown webhook id surfaces as store.ErrNotFound.
type webhookProjects struct{ svc *projects.Service }

func (w webhookProjects) ByWebhookID(ctx context.Context, id string) (webhooks.Hook, error) {
	h, err := w.svc.WebhookHook(ctx, id)
	if err != nil {
		return webhooks.Hook{}, err
	}
	return webhooks.Hook{
		Slug: h.Slug, Branch: h.Branch, TagPattern: h.TagPattern, Status: h.Status, Secret: h.Secret,
	}, nil
}
