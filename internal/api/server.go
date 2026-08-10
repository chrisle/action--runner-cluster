// Package api serves the orchestrator's local control plane: status for the
// CLI, live min/max adjustment, drain, and Prometheus metrics.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/orchestrator"
	"github.com/chrisle/action-runner-cluster/internal/state"
)

// Server is the control API.
type Server struct {
	cfg       *config.Config
	orch      *orchestrator.Orchestrator
	overrides *state.Overrides
	log       *slog.Logger
	http      *http.Server
}

// New builds the control API server.
func New(cfg *config.Config, orch *orchestrator.Orchestrator, overrides *state.Overrides, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, orch: orch, overrides: overrides, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.auth(s.handleMetrics))
	mux.HandleFunc("GET /v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("POST /v1/pools/{pool}/scale", s.auth(s.handleScale))
	mux.HandleFunc("POST /v1/pools/{pool}/drain", s.auth(s.handleDrain))
	mux.HandleFunc("POST /v1/pools/{pool}/resume", s.auth(s.handleResume))
	mux.HandleFunc("GET /v1/pools/{pool}/instances/{id}/logs", s.auth(s.handleLogs))

	s.http = &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Start begins serving. It returns once the listener is open so startup
// failures (a port already in use) surface immediately rather than in a
// goroutine nobody is watching.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("control api listen on %s: %w", s.cfg.Server.Addr, err)
	}
	s.log.Info("control api listening", "addr", ln.Addr().String())

	go func() {
		if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("control api stopped", "error", err)
		}
	}()
	return nil
}

// Shutdown stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// auth enforces the optional bearer token.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Server.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			// Constant-time compare so the token cannot be recovered by timing.
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Server.Token)) != 1 {
				writeError(w, http.StatusUnauthorized, "invalid or missing token")
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap := s.orch.Snapshot()
	ok, reason := s.orch.Healthy()

	status := http.StatusOK
	body := map[string]any{"ok": ok, "updated_at": snap.UpdatedAt}
	if !ok {
		status = http.StatusServiceUnavailable
		body["reason"] = reason
	}
	if snap.LastError != "" {
		body["last_error"] = snap.LastError
	}
	writeJSON(w, status, body)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.orch.Snapshot())
}

type scaleRequest struct {
	Min *int `json:"min"`
	Max *int `json:"max"`
	// Reset clears the override and returns the pool to its config values.
	Reset bool `json:"reset"`
}

func (s *Server) handleScale(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	cfgPool := s.cfg.Pool(pool)
	if cfgPool == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown pool %q", pool))
		return
	}

	var req scaleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	if req.Reset {
		if err := s.overrides.Clear(pool); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"pool": pool, "min": cfgPool.Min, "max": cfgPool.Max, "source": "config",
		})
		return
	}

	// Validate against the effective values, not just the ones being set, so
	// `arc scale p --min 5` against a config max of 3 is rejected instead of
	// silently clamped.
	effMin, effMax := cfgPool.Min, cfgPool.Max
	if ov, ok := s.overrides.Get(pool); ok {
		if ov.Min != nil {
			effMin = *ov.Min
		}
		if ov.Max != nil {
			effMax = *ov.Max
		}
	}
	if req.Min != nil {
		effMin = *req.Min
	}
	if req.Max != nil {
		effMax = *req.Max
	}

	switch {
	case req.Min == nil && req.Max == nil:
		writeError(w, http.StatusBadRequest, "specify min, max, or reset")
		return
	case effMin < 0:
		writeError(w, http.StatusBadRequest, "min must be >= 0")
		return
	case effMax < 1:
		writeError(w, http.StatusBadRequest, "max must be >= 1")
		return
	case effMin > effMax:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("min (%d) would exceed max (%d)", effMin, effMax))
		return
	}

	if err := s.overrides.Set(pool, req.Min, req.Max); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("pool limits changed", "pool", pool, "min", effMin, "max", effMax)
	writeJSON(w, http.StatusOK, map[string]any{
		"pool": pool, "min": effMin, "max": effMax, "source": "override",
	})
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	s.setDrain(w, r, true)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.setDrain(w, r, false)
}

func (s *Server) setDrain(w http.ResponseWriter, r *http.Request, drained bool) {
	pool := r.PathValue("pool")
	if err := s.orch.SetDrained(pool, drained); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pool": pool, "drained": drained})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	pool, id := r.PathValue("pool"), r.PathValue("id")
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10000 {
			lines = n
		}
	}
	out, err := s.orch.Logs(r.Context(), pool, id, lines)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))
}

// handleMetrics emits Prometheus text format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap := s.orch.Snapshot()
	var b strings.Builder

	metric := func(name, help, typ string, emit func()) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		emit()
	}

	metric("arc_pool_runners_live", "Runners currently alive in the pool.", "gauge", func() {
		for _, p := range snap.Pools {
			fmt.Fprintf(&b, "arc_pool_runners_live{pool=%q} %d\n", p.Name, p.Live)
		}
	})
	metric("arc_pool_runners_busy", "Runners currently executing a job.", "gauge", func() {
		for _, p := range snap.Pools {
			fmt.Fprintf(&b, "arc_pool_runners_busy{pool=%q} %d\n", p.Name, p.Busy)
		}
	})
	metric("arc_pool_runners_idle", "Runners online and waiting for work.", "gauge", func() {
		for _, p := range snap.Pools {
			fmt.Fprintf(&b, "arc_pool_runners_idle{pool=%q} %d\n", p.Name, p.Idle)
		}
	})
	metric("arc_pool_runners_desired", "Target runner count for the pool.", "gauge", func() {
		for _, p := range snap.Pools {
			fmt.Fprintf(&b, "arc_pool_runners_desired{pool=%q} %d\n", p.Name, p.Desired)
		}
	})
	metric("arc_pool_jobs_queued", "Queued jobs assigned to the pool.", "gauge", func() {
		for _, p := range snap.Pools {
			fmt.Fprintf(&b, "arc_pool_jobs_queued{pool=%q} %d\n", p.Name, p.Queued)
		}
	})
	metric("arc_pool_min", "Configured minimum runners.", "gauge", func() {
		for _, p := range snap.Pools {
			fmt.Fprintf(&b, "arc_pool_min{pool=%q} %d\n", p.Name, p.Min)
		}
	})
	metric("arc_pool_max", "Configured maximum runners.", "gauge", func() {
		for _, p := range snap.Pools {
			fmt.Fprintf(&b, "arc_pool_max{pool=%q} %d\n", p.Name, p.Max)
		}
	})
	metric("arc_jobs_unassigned", "Queued jobs no pool can serve.", "gauge", func() {
		fmt.Fprintf(&b, "arc_jobs_unassigned %d\n", len(snap.Unassigned))
	})
	metric("arc_github_rate_limit_remaining", "Remaining GitHub API requests.", "gauge", func() {
		fmt.Fprintf(&b, "arc_github_rate_limit_remaining %d\n", snap.RateLimit.Remaining)
	})
	metric("arc_last_reconcile_timestamp_seconds", "Unix time of the last reconcile.", "gauge", func() {
		fmt.Fprintf(&b, "arc_last_reconcile_timestamp_seconds %d\n", snap.UpdatedAt.Unix())
	})

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
