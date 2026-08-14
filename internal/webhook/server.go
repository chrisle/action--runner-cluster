// Package webhook makes arc event-driven: a local HTTP endpoint receives
// GitHub workflow_job deliveries through a Cloudflare quick tunnel, and each
// relevant event pokes the orchestrator into an immediate reconcile pass.
// Polling remains as a slow safety net for missed deliveries.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/chrisle/action-runner-cluster/internal/ghapi"
)

// maxBody bounds a webhook payload; workflow_job events are a few KB.
const maxBody = 1 << 20

// Server verifies and handles GitHub webhook deliveries.
type Server struct {
	secret []byte
	poke   func()
	log    *slog.Logger
}

// NewServer builds a handler with a freshly generated secret. The secret
// rotates every arc start and is pushed to GitHub during hook registration,
// so nothing needs to persist.
func NewServer(poke func(), log *slog.Logger) *Server {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return &Server{
		secret: []byte(hex.EncodeToString(buf)),
		poke:   poke,
		log:    log,
	}
}

// Secret is what GitHub signs deliveries with.
func (s *Server) Secret() string { return string(s.secret) }

// Handler serves ghapi.WebhookPath and its per-host subpaths. Anything else
// on the tunnel 404s.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+ghapi.WebhookPath, s.handle)
	mux.HandleFunc("POST "+ghapi.WebhookPath+"/", s.handle)
	return mux
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	if !s.validSignature(r.Header.Get("X-Hub-Signature-256"), body) {
		// The tunnel URL is public; unsigned traffic is noise or probing.
		s.log.Debug("webhook delivery with bad signature dropped")
		http.Error(w, "signature", http.StatusUnauthorized)
		return
	}

	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		w.WriteHeader(http.StatusOK)
		return
	case "workflow_job":
	default:
		w.WriteHeader(http.StatusOK) // subscribed but uninteresting
		return
	}

	var ev struct {
		Action      string `json:"action"`
		WorkflowJob struct {
			Name   string   `json:"name"`
			Labels []string `json:"labels"`
		} `json:"workflow_job"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "payload", http.StatusBadRequest)
		return
	}

	// queued means demand appeared; completed means capacity freed up. Either
	// way the reconciler re-derives the whole picture itself — the payload is
	// only a doorbell, so a spoofed or stale body can never create runners.
	if ev.Action == "queued" || ev.Action == "completed" {
		s.log.Info("workflow_job event", "action", ev.Action,
			"repo", ev.Repository.FullName, "job", ev.WorkflowJob.Name,
			"labels", ev.WorkflowJob.Labels)
		s.poke()
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) validSignature(header string, body []byte) bool {
	const prefix = "sha256="
	if len(header) <= len(prefix) {
		return false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(body)
	want := prefix + hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(header), []byte(want)) == 1
}
