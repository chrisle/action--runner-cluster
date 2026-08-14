package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chrisle/action-runner-cluster/internal/ghapi"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestServerPokesOnSignedQueuedEvent(t *testing.T) {
	poked := 0
	s := NewServer(func() { poked++ }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := []byte(`{"action":"queued","workflow_job":{"name":"build","labels":["self-hosted","macos"]},"repository":{"full_name":"chrisle/app"}}`)

	req := httptest.NewRequest("POST", ghapi.WebhookPath+"/abc123", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", sign(s.Secret(), body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 || poked != 1 {
		t.Errorf("code = %d, poked = %d; want 200 and one poke", rec.Code, poked)
	}
}

func TestServerRejectsBadSignature(t *testing.T) {
	poked := 0
	s := NewServer(func() { poked++ }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := []byte(`{"action":"queued"}`)

	req := httptest.NewRequest("POST", ghapi.WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", sign("wrong-secret", body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 401 || poked != 0 {
		t.Errorf("code = %d, poked = %d; want 401 and no poke", rec.Code, poked)
	}
}

func TestServerIgnoresOtherActions(t *testing.T) {
	poked := 0
	s := NewServer(func() { poked++ }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, payload := range []struct{ event, body string }{
		{"ping", `{"zen":"ok"}`},
		{"workflow_job", `{"action":"in_progress"}`},
	} {
		body := []byte(payload.body)
		req := httptest.NewRequest("POST", ghapi.WebhookPath, strings.NewReader(payload.body))
		req.Header.Set("X-GitHub-Event", payload.event)
		req.Header.Set("X-Hub-Signature-256", sign(s.Secret(), body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s: code = %d, want 200", payload.event, rec.Code)
		}
	}
	if poked != 0 {
		t.Errorf("poked = %d, want 0", poked)
	}
}

func TestTunnelURLPattern(t *testing.T) {
	line := `2026-08-14T20:00:00Z INF +  https://random-words-here.trycloudflare.com  +`
	if got := tunnelURLRE.FindString(line); got != "https://random-words-here.trycloudflare.com" {
		t.Errorf("parsed %q", got)
	}
}
