package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/api"
	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/ghapi"
	"github.com/chrisle/action-runner-cluster/internal/orchestrator"
	"github.com/chrisle/action-runner-cluster/internal/state"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to arc.yaml")
	addr := fs.String("addr", "", "override the control API listen address")
	skipPreflight := fs.Bool("skip-preflight", false,
		"start even if a provider fails its preflight check")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}

	log := newLogger(cfg.Log)
	log.Info("arc starting", "version", version, "config", cfg.Path)

	gh, err := ghapi.New(cfg, log)
	if err != nil {
		return err
	}

	overrides, err := state.LoadOverrides(cfg.Server.StateDir)
	if err != nil {
		return err
	}

	orch, err := orchestrator.New(cfg, gh, overrides, log)
	if err != nil {
		return err
	}
	defer orch.Close()

	// Signals are trapped before preflight so a hung daemon connection can
	// still be interrupted with Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	preflightCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	err = orch.Preflight(preflightCtx)
	cancel()
	if err != nil {
		if !*skipPreflight {
			return fmt.Errorf("%w\n\nRun `arc doctor` for detail, or start with "+
				"-skip-preflight to bring up the pools that do work", err)
		}
		log.Warn("preflight failed, continuing because -skip-preflight was set", "error", err)
	}

	srv := api.New(cfg, orch, overrides, log)
	if err := srv.Start(); err != nil {
		return err
	}

	runErr := orch.Run(ctx)

	// Give the API a moment to finish in-flight requests. Runners are
	// deliberately left alone: they are ephemeral and will finish their current
	// job and exit on their own, and killing them would fail live builds.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)

	log.Info("arc stopped; running jobs were left to finish")
	return runErr
}

// loadConfig resolves the config path and loads it.
func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		path = os.Getenv("ARC_CONFIG")
	}
	if path == "" {
		path = "arc.yaml"
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no config at %s (pass -config, set ARC_CONFIG, or "+
			"copy config.example.yaml to arc.yaml)", path)
	}
	return config.Load(path)
}

func newLogger(cfg config.Log) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
