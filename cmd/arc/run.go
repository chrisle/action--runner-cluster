package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/api"
	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/ghapi"
	"github.com/chrisle/action-runner-cluster/internal/opconfig"
	"github.com/chrisle/action-runner-cluster/internal/orchestrator"
	"github.com/chrisle/action-runner-cluster/internal/state"
	"github.com/chrisle/action-runner-cluster/internal/webhook"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to arc.yaml")
	addr := fs.String("addr", "", "override the control API listen address")
	maxRunners := fs.Int("max", config.DefaultMaxRunners,
		"max concurrent runners for this machine's pool (1Password-config mode)")
	skipPreflight := fs.Bool("skip-preflight", false,
		"start even if a provider fails its preflight check")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfigMax(*cfgPath, *maxRunners)
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

	if cfg.GitHub.Webhook {
		wm := webhook.NewManager(cfg, gh, orch.Poke, log)
		go func() {
			if err := wm.Run(ctx); err != nil {
				log.Error("webhook pipeline stopped; polling still covers scaling", "error", err)
			}
		}()
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
	return loadConfigMax(path, config.DefaultMaxRunners)
}

// loadConfigMax resolves configuration: an explicit or discovered file wins;
// with no file anywhere, configuration comes from the 1Password vault and the
// host serves its own platform with at most maxRunners concurrent runners.
func loadConfigMax(path string, maxRunners int) (*config.Config, error) {
	explicit := path != ""
	if path == "" {
		path = os.Getenv("ARC_CONFIG")
		explicit = path != ""
	}
	if path == "" {
		path = "arc.yaml"
		// No local arc.yaml: fall back to the per-user config `arc config` writes.
		if _, err := os.Stat(path); err != nil {
			if p := config.DefaultUserPath(); p != "" {
				if _, err := os.Stat(p); err == nil {
					path = p
				}
			}
		}
	}
	if _, err := os.Stat(path); err != nil {
		if explicit {
			return nil, fmt.Errorf("no config at %s", path)
		}
		cfg, opErr := autoConfig(maxRunners)
		if opErr != nil {
			return nil, fmt.Errorf("no config file found and 1Password configuration "+
				"failed: %w\n(run `arc config` to create a file instead, pass -config, "+
				"or set ARC_CONFIG)", opErr)
		}
		return cfg, nil
	}
	return config.Load(path)
}

// loadVaultItem and probeAccountType are swapped out in tests.
var (
	loadVaultItem = opconfig.Load

	// probeAccountType asks GitHub whether the login is a user or an org —
	// the two register runners through different APIs — and doubles as
	// validation that the token still authenticates.
	probeAccountType = func(ctx context.Context, item *opconfig.Item) (string, error) {
		probe := &config.Config{}
		probe.GitHub.Owner = item.Login
		probe.GitHub.Token = item.Token
		probe.GitHub.APIURL = "https://api.github.com"
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		gh, err := ghapi.New(probe, quiet)
		if err != nil {
			return "", err
		}
		return gh.AccountType(ctx, item.Login)
	}
)

// autoConfig builds configuration from the 1Password vault: account and token
// from op://arc/github, one pool for this machine's platform scaling from
// zero, webhook-driven.
//
// The vault is only read on the first run (or when its cached token stops
// authenticating): the result is cached at ~/.arc/credentials.json so
// restarts never depend on — or prompt through — the op CLI.
func autoConfig(maxRunners int) (*config.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	item := opconfig.ReadCache()
	fromCache := item != nil
	if item == nil {
		var err error
		item, err = loadVaultItem(ctx)
		if err != nil {
			return nil, err
		}
	}

	accountType, err := probeAccountType(ctx, item)
	switch {
	case err == nil:
	case fromCache && isAuthError(err):
		// The cached token was rotated or revoked — refresh from the vault.
		fresh, opErr := loadVaultItem(ctx)
		if opErr != nil {
			return nil, fmt.Errorf("cached credentials were rejected by GitHub and the "+
				"1Password refresh failed: %w", opErr)
		}
		item, fromCache = fresh, false
		if accountType, err = probeAccountType(ctx, item); err != nil {
			return nil, fmt.Errorf("verify account %q with the vault token: %w", item.Login, err)
		}
	case fromCache && item.AccountType != "":
		// GitHub is unreachable but the cache was validated before: come up
		// anyway and let the orchestrator retry, instead of a service that
		// cannot boot without network.
		accountType = item.AccountType
	default:
		return nil, fmt.Errorf("verify account %q with the vault token: %w", item.Login, err)
	}

	item.AccountType = accountType
	if err := opconfig.WriteCache(item); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not cache credentials (the vault will be "+
			"read again next start): %v\n", err)
	}

	cfg := &config.Config{}
	cfg.GitHub.Token = item.Token
	cfg.GitHub.Webhook = true
	if accountType == "Organization" {
		cfg.GitHub.Org = item.Login
	} else {
		cfg.GitHub.Owner = item.Login
	}
	cfg.Pools = []*config.Pool{config.HostPool(maxRunners)}

	// Round-trip through the normal load pipeline for defaults + validation.
	raw, err := cfg.Marshal()
	if err != nil {
		return nil, err
	}
	out, err := config.Parse(raw)
	if err != nil {
		return nil, err
	}
	out.Path = "1Password op://arc/github"
	if fromCache {
		out.Path += " (cached)"
	}
	return out, nil
}

// isAuthError reports a definitive credential rejection, as opposed to
// network trouble or a GitHub outage.
func isAuthError(err error) bool {
	var ae *ghapi.APIError
	return errors.As(err, &ae) &&
		(ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden)
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
