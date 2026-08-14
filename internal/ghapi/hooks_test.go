package ghapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

func TestEnsureWebhookUpdatesOwnHostsHook(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/chrisle/app/hooks":
			// One hook from this host (old tunnel URL), one from another host.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 5, "config": map[string]any{"url": "https://old.trycloudflare.com/arc/webhook/aaaaaa"}},
				{"id": 6, "config": map[string]any{"url": "https://x.trycloudflare.com/arc/webhook/ffffff"}},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/chrisle/app/hooks/5":
			var body struct {
				Config struct{ URL, Secret string } `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Config.URL != "https://new.trycloudflare.com/arc/webhook/aaaaaa" || body.Config.Secret == "" {
				t.Errorf("patched config = %+v", body.Config)
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	c, _ := ownerClient(t, handler)

	err := c.EnsureWebhook(context.Background(), "app",
		"https://new.trycloudflare.com/arc/webhook/aaaaaa", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	// Host ffffff's hook (id 6) must never be touched.
	for _, call := range calls {
		if call == "PATCH /repos/chrisle/app/hooks/6" {
			t.Error("updated another host's hook")
		}
	}
}

func TestEnsureWebhookCreatesWhenMissing(t *testing.T) {
	created := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/chrisle/app/hooks":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/chrisle/app/hooks":
			var body struct {
				Name   string                       `json:"name"`
				Events []string                     `json:"events"`
				Config struct{ URL, Secret string } `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Name != "web" || len(body.Events) != 1 || body.Events[0] != "workflow_job" {
				t.Errorf("created hook = %+v", body)
			}
			created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	c, _ := ownerClient(t, handler)
	if err := c.EnsureWebhook(context.Background(), "app",
		"https://t.trycloudflare.com/arc/webhook/aaaaaa", "s"); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("hook was not created")
	}
}
