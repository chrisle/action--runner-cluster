package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chrisle/action-runner-cluster/internal/config"
)

func TestConfigWizardCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arc.yaml")
	input := strings.Join([]string{
		"my-org", // organization
		"",       // auth method → token
		"",       // token → ${GITHUB_TOKEN}
		"30s",    // poll interval
		"",       // runner group → empty
		// first pool (mandatory)
		"linux",  // name
		"docker", // provider
		"",       // labels → default
		"1",      // min
		"8",      // max
		"",       // image → suggested ghcr.io/my-org/arc-runner:linux
		"",       // add another pool? → no
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := runConfigWizard(path, strings.NewReader(input), &out); err != nil {
		t.Fatalf("wizard: %v\noutput:\n%s", err, out.String())
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config written with perms %o, want 600", perm)
	}

	raw, _ := os.ReadFile(path)
	cfg, err := config.ParseEditable(raw)
	if err != nil {
		t.Fatalf("written config does not parse: %v\n%s", err, raw)
	}
	if cfg.GitHub.Org != "my-org" {
		t.Errorf("org = %q, want my-org", cfg.GitHub.Org)
	}
	// The env reference must land in the file verbatim, never expanded.
	if cfg.GitHub.Token != "${GITHUB_TOKEN}" {
		t.Errorf("token = %q, want the literal ${GITHUB_TOKEN}", cfg.GitHub.Token)
	}
	if got := cfg.GitHub.PollInterval.String(); got != "30s" {
		t.Errorf("poll_interval = %s, want 30s", got)
	}
	if len(cfg.Pools) != 1 {
		t.Fatalf("pools = %d, want 1", len(cfg.Pools))
	}
	p := cfg.Pools[0]
	if p.Name != "linux" || p.Provider != "docker" || p.Min != 1 || p.Max != 8 {
		t.Errorf("pool = %+v", p)
	}
	if p.Docker == nil || p.Docker.Image != "ghcr.io/my-org/arc-runner:linux" {
		t.Errorf("docker spec = %+v", p.Docker)
	}
	if want := []string{"self-hosted", "linux", "x64"}; strings.Join(p.Labels, ",") != strings.Join(want, ",") {
		t.Errorf("labels = %v, want %v", p.Labels, want)
	}
}

func TestConfigWizardEditKeepsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arc.yaml")
	orig := `github:
  org: acme
  token: ${GITHUB_TOKEN}
  poll_interval: 20s
server:
  addr: 127.0.0.1:9999
pools:
  - name: linux
    labels: [self-hosted, linux, x64]
    provider: docker
    min: 2
    max: 6
    docker:
      image: ghcr.io/acme/arc-runner:linux
      pull: always
      cpus: 4
`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	// All-Enter session: org, auth method, token, poll interval, runner group,
	// pool action (keep), add another (no).
	input := strings.Repeat("\n", 7)
	var out bytes.Buffer
	if err := runConfigWizard(path, strings.NewReader(input), &out); err != nil {
		t.Fatalf("wizard: %v\noutput:\n%s", err, out.String())
	}

	raw, _ := os.ReadFile(path)
	cfg, err := config.ParseEditable(raw)
	if err != nil {
		t.Fatalf("edited config does not parse: %v\n%s", err, raw)
	}
	if cfg.GitHub.Org != "acme" || cfg.GitHub.Token != "${GITHUB_TOKEN}" {
		t.Errorf("github = %+v", cfg.GitHub)
	}
	if got := cfg.GitHub.PollInterval.String(); got != "20s" {
		t.Errorf("poll_interval = %s, want 20s", got)
	}
	if cfg.Server.Addr != "127.0.0.1:9999" {
		t.Errorf("server.addr = %q, want 127.0.0.1:9999", cfg.Server.Addr)
	}
	if len(cfg.Pools) != 1 {
		t.Fatalf("pools = %d, want 1", len(cfg.Pools))
	}
	// Fields the wizard never asks about must survive the round trip.
	d := cfg.Pools[0].Docker
	if d == nil || d.Pull != "always" || d.CPUs != 4 {
		t.Errorf("docker spec lost fields: %+v", d)
	}
}

func TestConfigWizardAbortWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arc.yaml")
	// Input ends mid-session.
	err := runConfigWizard(path, strings.NewReader("my-org\n"), &bytes.Buffer{})
	if err == nil {
		t.Fatal("want an error from a truncated session")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("aborted wizard left a file behind")
	}
}
