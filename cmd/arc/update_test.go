package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubUpdateHooks(t *testing.T, verify func(string) error) {
	t.Helper()
	origAPI, origVerify := updateAPIBase, verifyNewBinary
	verifyNewBinary = verify
	t.Cleanup(func() { updateAPIBase, verifyNewBinary = origAPI, origVerify })
}

func TestLatestRelease(t *testing.T) {
	stubUpdateHooks(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+updateRepo+"/releases/latest" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"tag_name": "v9.9.9",
			"assets": [
				{"name": "arc-linux-amd64", "browser_download_url": "https://example.test/linux"},
				{"name": "arc-darwin-arm64", "browser_download_url": "https://example.test/darwin"}
			]
		}`))
	}))
	defer srv.Close()
	updateAPIBase = srv.URL

	rel, err := latestRelease(context.Background(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v9.9.9" || len(rel.Assets) != 2 || rel.Assets[1].Name != "arc-darwin-arm64" {
		t.Errorf("release = %+v", rel)
	}
}

func TestLatestReleaseNoReleases(t *testing.T) {
	stubUpdateHooks(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	updateAPIBase = srv.URL

	if _, err := latestRelease(context.Background(), srv.Client()); err == nil ||
		!strings.Contains(err.Error(), "no releases") {
		t.Errorf("err = %v, want a no-releases error", err)
	}
}

func TestInstallToReplacesBinary(t *testing.T) {
	stubUpdateHooks(t, func(string) error { return nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("new-binary-content"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	exe := filepath.Join(dir, "arc")
	if err := os.WriteFile(exe, []byte("old-binary-content"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installTo(context.Background(), srv.Client(), srv.URL, exe); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(exe)
	if string(got) != "new-binary-content" {
		t.Errorf("binary content = %q", got)
	}
	fi, _ := os.Stat(exe)
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("perms = %o, want 755", perm)
	}
	// No temp files may be left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files in %s: %v", dir, entries)
	}
}

func TestInstallToRejectsBadBinary(t *testing.T) {
	stubUpdateHooks(t, func(string) error { return errors.New("not arc") })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("junk"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	exe := filepath.Join(dir, "arc")
	if err := os.WriteFile(exe, []byte("old-binary-content"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installTo(context.Background(), srv.Client(), srv.URL, exe); err == nil {
		t.Fatal("want an error from a failed verification")
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old-binary-content" {
		t.Errorf("current binary was touched despite failed verification: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files in %s: %v", dir, entries)
	}
}

func TestInstallToRejectsEmptyDownload(t *testing.T) {
	stubUpdateHooks(t, func(string) error { return nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	dir := t.TempDir()
	exe := filepath.Join(dir, "arc")
	if err := os.WriteFile(exe, []byte("old-binary-content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installTo(context.Background(), srv.Client(), srv.URL, exe); err == nil ||
		!strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want an empty-download error", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old-binary-content" {
		t.Errorf("current binary was replaced by an empty download")
	}
}
