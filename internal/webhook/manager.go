package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/ghapi"
	"github.com/chrisle/action-runner-cluster/internal/hostid"
)

// Manager runs the whole webhook pipeline: local listener, Cloudflare
// tunnel, and GitHub hook registration — restarting the tunnel and
// re-registering hooks whenever it dies. Failures degrade to polling, never
// to a dead cluster.
type Manager struct {
	cfg *config.Config
	gh  *ghapi.Client
	srv *Server
	log *slog.Logger
}

// NewManager wires the pipeline. poke is called on every relevant delivery.
func NewManager(cfg *config.Config, gh *ghapi.Client, poke func(), log *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg,
		gh:  gh,
		srv: NewServer(poke, log),
		log: log.With("component", "webhook"),
	}
}

// Run blocks until ctx is done, supervising the tunnel.
func (m *Manager) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()

	httpSrv := &http.Server{Handler: m.srv.Handler()}
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	backoff := time.Second
	for {
		tunnel, err := StartTunnel(ctx, ln.Addr().String(), m.log)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			m.log.Error("tunnel start failed; polling still covers scaling",
				"error", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < 2*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		m.log.Info("tunnel up", "url", tunnel.URL)
		if err := m.register(ctx, tunnel.URL); err != nil {
			m.log.Error("webhook registration failed; polling still covers scaling", "error", err)
		}

		select {
		case <-ctx.Done():
			tunnel.Stop()
			return nil
		case err := <-tunnel.Done():
			// Quick tunnels are free but not durable. A new one gets a new
			// URL, so the loop re-registers every hook.
			m.log.Warn("tunnel exited; restarting", "error", err)
		}
	}
}

// register points every relevant webhook at the tunnel. The path embeds this
// host's id so each arc host keeps its own hook — every host gets every
// event, and the idle-aware demand logic keeps them from double-provisioning.
func (m *Manager) register(ctx context.Context, tunnelURL string) error {
	url := tunnelURL + ghapi.WebhookPath + "/" + hostid.ID()
	secret := m.srv.Secret()

	// One org hook covers every repo in the org.
	if m.cfg.GitHub.Owner == "" {
		if err := m.gh.EnsureWebhook(ctx, "", url, secret); err != nil {
			return fmt.Errorf("org webhook: %w", err)
		}
		m.log.Info("org webhook registered", "url", url)
		return nil
	}

	// Personal accounts only have repo hooks, so each watched repo gets one.
	repos, err := m.gh.ListRepos(ctx, ghapi.RepoFilterOpts{
		Include:      m.cfg.GitHub.Repos.Include,
		Exclude:      m.cfg.GitHub.Repos.Exclude,
		ActiveWithin: m.cfg.GitHub.Repos.ActiveWithin.Duration(),
		Archived:     m.cfg.GitHub.Repos.Archived,
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range repos {
		if err := m.gh.EnsureWebhook(ctx, r.Name, url, secret); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
	}
	m.log.Info("repo webhooks registered", "repos", len(repos)-len(errs), "failed", len(errs))
	return errors.Join(errs...)
}
