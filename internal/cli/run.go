package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/containers"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/runtime/docker"
	"github.com/open-southeners/ki-agent/internal/server"
	"github.com/open-southeners/ki-agent/internal/version"
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

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
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

	router := rpc.NewRouter()
	agent.New().Register(router)
	containers.New(rt, logger).Register(router)
	srv, err := server.New(server.Options{Config: cfg, Router: router, Logger: logger})
	if err != nil {
		return err
	}
	if err := srv.Listen(); err != nil {
		return err
	}
	if err := srv.Serve(ctx); err != nil {
		return err
	}
	logger.Info("ki-agent stopped")
	return nil
}
