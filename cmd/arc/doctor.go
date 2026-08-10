package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/ghapi"
	"github.com/chrisle/action-runner-cluster/internal/orchestrator"
	"github.com/chrisle/action-runner-cluster/internal/state"
)

// cmdDoctor checks everything that has to be true for `arc run` to work, and
// reports all failures rather than stopping at the first, so one pass is enough
// to fix a fresh install.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to arc.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	failures := 0
	check := func(name string, fn func() (string, error)) {
		detail, err := fn()
		switch {
		case err != nil:
			failures++
			fmt.Printf("  ✗ %s\n      %s\n", name, indent(err.Error()))
		case detail != "":
			fmt.Printf("  ✓ %s — %s\n", name, detail)
		default:
			fmt.Printf("  ✓ %s\n", name)
		}
	}

	fmt.Println("configuration")
	var cfg *config.Config
	check("config file parses and validates", func() (string, error) {
		var err error
		cfg, err = loadConfig(*cfgPath)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s, %d pool(s)", cfg.Path, len(cfg.Pools)), nil
	})
	if cfg == nil {
		fmt.Printf("\n%d check(s) failed.\n", failures)
		return fmt.Errorf("cannot continue without a valid config")
	}

	// Silence the orchestrator's own logging; doctor prints its own report.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	fmt.Println("\ngithub")
	var gh *ghapi.Client
	check("credentials load", func() (string, error) {
		var err error
		gh, err = ghapi.New(cfg, log)
		if err != nil {
			return "", err
		}
		return gh.AuthDescription(), nil
	})

	if gh != nil {
		var runners []ghapi.Runner
		check("can list org runners (admin:org)", func() (string, error) {
			var err error
			runners, err = gh.ListRunners(ctx)
			if err != nil {
				if ghapi.IsNotFound(err) {
					return "", fmt.Errorf("org %q not found, or the credential cannot see it. "+
						"A classic PAT needs the admin:org scope; a GitHub App needs the "+
						"organization_self_hosted_runners: write permission", cfg.GitHub.Org)
				}
				if ghapi.IsForbidden(err) {
					return "", fmt.Errorf("permission denied. A classic PAT needs admin:org; "+
						"a GitHub App needs organization_self_hosted_runners: write: %w", err)
				}
				return "", err
			}
			busy := 0
			for _, r := range runners {
				if r.Busy {
					busy++
				}
			}
			return fmt.Sprintf("%d registered, %d busy", len(runners), busy), nil
		})

		check("can enumerate repos to poll", func() (string, error) {
			repos, err := gh.ListRepos(ctx, ghapi.RepoFilterOpts{
				Include:      cfg.GitHub.Repos.Include,
				Exclude:      cfg.GitHub.Repos.Exclude,
				ActiveWithin: cfg.GitHub.Repos.ActiveWithin.Duration(),
				Archived:     cfg.GitHub.Repos.Archived,
			})
			if err != nil {
				return "", err
			}
			if len(repos) == 0 {
				return "", fmt.Errorf("no repos matched the filter, so no queued job will ever " +
					"be seen. Check github.repos.include/exclude and active_within")
			}
			names := make([]string, 0, 3)
			for i, r := range repos {
				if i == 3 {
					break
				}
				names = append(names, r.Name)
			}
			suffix := ""
			if len(repos) > 3 {
				suffix = fmt.Sprintf(", +%d more", len(repos)-3)
			}
			return fmt.Sprintf("%d repo(s): %s%s", len(repos), strings.Join(names, ", "), suffix), nil
		})

		for _, pool := range cfg.Pools {
			group := cfg.EffectiveRunnerGroup(pool)
			if group == "" {
				continue
			}
			check(fmt.Sprintf("runner group %q resolves (pool %s)", group, pool.Name), func() (string, error) {
				id, err := gh.RunnerGroupID(ctx, group)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("id %d", id), nil
			})
		}

		rl := gh.RateLimit()
		if rl.Limit > 0 {
			fmt.Printf("  · rate limit %d/%d remaining, resets in %s\n",
				rl.Remaining, rl.Limit, time.Until(rl.Reset).Round(time.Second))
			estimate := estimateHourlyRequests(cfg)
			if rl.Limit > 0 && estimate > rl.Limit {
				fmt.Printf("  ! polling every %s could use ~%d requests/hour against a limit of %d.\n"+
					"      Narrow github.repos, or raise github.poll_interval.\n",
					cfg.GitHub.PollInterval, estimate, rl.Limit)
			}
		}
	}

	fmt.Println("\nproviders")
	overrides, err := state.LoadOverrides(cfg.Server.StateDir)
	if err != nil {
		failures++
		fmt.Printf("  ✗ state directory\n      %s\n", indent(err.Error()))
		overrides = nil
	} else {
		fmt.Printf("  ✓ state directory — %s\n", cfg.Server.StateDir)
	}

	if gh != nil && overrides != nil {
		orch, err := orchestrator.New(cfg, gh, overrides, log)
		if err != nil {
			failures++
			fmt.Printf("  ✗ providers construct\n      %s\n", indent(err.Error()))
		} else {
			defer orch.Close()
			for _, pool := range cfg.Pools {
				p := pool
				check(fmt.Sprintf("pool %s (%s)", p.Name, p.Provider), func() (string, error) {
					if err := orch.PreflightPool(ctx, p.Name); err != nil {
						return "", err
					}
					return fmt.Sprintf("min %d, max %d, labels [%s]",
						p.Min, p.Max, strings.Join(p.Labels, ", ")), nil
				})
			}
		}
	}

	fmt.Println("\ncontrol api")
	check("orchestrator reachable", func() (string, error) {
		var health map[string]any
		if err := newControlClient(cfg).do(ctx, "GET", "/healthz", nil, &health); err != nil {
			return "", fmt.Errorf("not running (this is fine if you have not started it yet): %w", err)
		}
		return fmt.Sprintf("%v", health["ok"]), nil
	})

	fmt.Println()
	if failures > 0 {
		return fmt.Errorf("%d check(s) failed", failures)
	}
	fmt.Println("All checks passed.")
	return nil
}

// estimateHourlyRequests approximates the worst-case REST cost of polling.
// Conditional requests that return 304 are free, so this is an upper bound that
// only materialises when every watched repo is continuously active.
func estimateHourlyRequests(cfg *config.Config) int {
	interval := cfg.GitHub.PollInterval.Duration()
	if interval <= 0 {
		return 0
	}
	ticksPerHour := int(time.Hour / interval)
	// Per tick: one runner list, plus two run-list calls per repo. The
	// per-run job calls only happen when a repo actually has active runs.
	perTick := 1 + 2*20 // assume 20 active repos as a rough middle case
	return ticksPerHour * perTick
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n      ")
}
