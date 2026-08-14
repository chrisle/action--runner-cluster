package processprov

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/chrisle/action-runner-cluster/internal/config"
)

func testProvider(t *testing.T, templateDir string) *Provider {
	t.Helper()
	pool := &config.Pool{
		Name:   "macos",
		Labels: []string{"self-hosted", "macos"},
		Process: &config.ProcessSpec{
			TemplateDir:  templateDir,
			InstancesDir: filepath.Join(t.TempDir(), "instances"),
		},
	}
	p, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// stubRunnerServers points the bootstrap at fake GitHub endpoints serving the
// given archive bytes as version 9.9.9.
func stubRunnerServers(t *testing.T, archive []byte) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name": "v9.9.9"}`))
	}))
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	origAPI, origDL := runnerReleaseAPI, runnerDownloadURL
	runnerReleaseAPI, runnerDownloadURL = api.URL, dl.URL
	t.Cleanup(func() {
		runnerReleaseAPI, runnerDownloadURL = origAPI, origDL
		api.Close()
		dl.Close()
	})
}

// fakeRunnerArchive builds a minimal runner tarball: the entrypoint plus a
// stale credentials file that the bootstrap must strip.
func fakeRunnerArchive(t *testing.T) []byte {
	t.Helper()
	src := t.TempDir()
	for name, content := range map[string]string{
		"run.sh":       "#!/bin/sh\n",
		"config.sh":    "#!/bin/sh\n",
		".credentials": "stale",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "runner.tar.gz")
	if err := exec.Command("tar", "-czf", out, "-C", src, ".").Run(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEnsureTemplateDownloadsWhenMissing(t *testing.T) {
	stubRunnerServers(t, fakeRunnerArchive(t))
	tmpl := filepath.Join(t.TempDir(), "runner-template")
	p := testProvider(t, tmpl)

	if err := p.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpl, "run.sh")); err != nil {
		t.Errorf("template has no entrypoint: %v", err)
	}
	// Credentials from the archive must be stripped, or Preflight itself
	// would have rejected the template.
	if _, err := os.Stat(filepath.Join(tmpl, ".credentials")); !os.IsNotExist(err) {
		t.Error("stale .credentials survived the bootstrap")
	}
	// No archive or staging leftovers.
	if _, err := os.Stat(tmpl + ".download"); !os.IsNotExist(err) {
		t.Error("staging directory left behind")
	}
	entries, _ := os.ReadDir(tmpl)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".gz" {
			t.Errorf("archive left in template: %s", e.Name())
		}
	}
}

func TestEnsureTemplateLeavesExistingAlone(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("bootstrap touched the network for an existing template")
	}))
	defer api.Close()
	origAPI := runnerReleaseAPI
	runnerReleaseAPI = api.URL
	t.Cleanup(func() { runnerReleaseAPI = origAPI })

	tmpl := t.TempDir() // exists, but is not a runner install
	p := testProvider(t, tmpl)
	if err := p.ensureTemplate(context.Background()); err != nil {
		t.Fatalf("ensureTemplate on existing dir: %v", err)
	}
	// Preflight must still reject it on its merits — the bootstrap only
	// handles the missing case, it never repairs a broken directory.
	if err := p.Preflight(context.Background()); err == nil {
		t.Error("preflight accepted a directory that is not a runner install")
	}
}

func TestEnsureTemplateFailedDownloadLeavesNothing(t *testing.T) {
	stubRunnerServers(t, nil) // empty download → rejected
	tmpl := filepath.Join(t.TempDir(), "runner-template")
	p := testProvider(t, tmpl)

	if err := p.ensureTemplate(context.Background()); err == nil {
		t.Fatal("want an error from an empty download")
	}
	if _, err := os.Stat(tmpl); !os.IsNotExist(err) {
		t.Error("failed bootstrap left a template directory")
	}
	if _, err := os.Stat(tmpl + ".download"); !os.IsNotExist(err) {
		t.Error("failed bootstrap left the staging directory")
	}
}

// TestEnsureTemplateLive downloads the real actions-runner release from
// GitHub. Opt-in: slow and network-bound.
func TestEnsureTemplateLive(t *testing.T) {
	if os.Getenv("ARC_LIVE_TEMPLATE_TEST") == "" {
		t.Skip("set ARC_LIVE_TEMPLATE_TEST=1 to download the real runner release")
	}
	tmpl := filepath.Join(t.TempDir(), "runner-template")
	p := testProvider(t, tmpl)
	if err := p.Preflight(context.Background()); err != nil {
		t.Fatalf("live bootstrap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpl, p.runnerEntrypoint())); err != nil {
		t.Errorf("live template has no entrypoint: %v", err)
	}
}
