package ghapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
)

// ownerClient builds a personal-account-mode client against a stub server.
func ownerClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := &config.Config{}
	cfg.GitHub.Owner = "chrisle"
	cfg.GitHub.Token = "t"
	cfg.GitHub.APIURL = srv.URL
	cfg.GitHub.WebURL = srv.URL
	c, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestOwnerModeEndpoints(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/user/repos":
			// One repo owned by the configured user, one by someone else —
			// the foreign one must be filtered out.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "app", "owner": map[string]any{"login": "chrisle"},
					"pushed_at": time.Now().Format(time.RFC3339)},
				{"name": "not-mine", "owner": map[string]any{"login": "someone-else"},
					"pushed_at": time.Now().Format(time.RFC3339)},
			})
		case r.URL.Path == "/repos/chrisle/app/actions/runners" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"runners": [{"id": 7, "name": "arc-macos-aabbccdd", "status": "online", "busy": false}]}`))
		case r.URL.Path == "/repos/chrisle/app/actions/runners/generate-jitconfig":
			_, _ = w.Write([]byte(`{"runner": {"id": 9, "name": "arc-macos-11223344"}, "encoded_jit_config": "zzz"}`))
		case r.URL.Path == "/repos/chrisle/app/actions/runners/9" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	c, _ := ownerClient(t, handler)
	ctx := context.Background()

	repos, err := c.ListRepos(ctx, RepoFilterOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Name != "app" {
		t.Fatalf("repos = %+v, want just the owner's repo", repos)
	}

	runners, err := c.ListRunners(ctx, repos)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 || runners[0].Repo != "app" || runners[0].ID != 7 {
		t.Fatalf("runners = %+v, want one runner tagged with its repo", runners)
	}

	jit, err := c.GenerateJITConfig(ctx, "app", "arc-macos-11223344", []string{"self-hosted"}, 0, "_work")
	if err != nil {
		t.Fatal(err)
	}
	if jit.Runner.Repo != "app" || jit.Encoded != "zzz" {
		t.Fatalf("jit = %+v, want repo recorded on the runner", jit)
	}

	if err := c.RemoveRunner(ctx, "app", 9); err != nil {
		t.Fatal(err)
	}

	if _, err := c.RunnerGroupID(ctx, "custom"); err == nil {
		t.Error("want an error for a custom runner group in personal mode")
	}
	if id, err := c.RunnerGroupID(ctx, ""); err != nil || id != DefaultRunnerGroupID {
		t.Errorf("default group = %d, %v; want %d", id, err, DefaultRunnerGroupID)
	}
}
