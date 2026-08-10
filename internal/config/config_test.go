package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const minimalConfig = `
github:
  org: acme
  token: ghp_test
pools:
  - name: linux
    labels: [self-hosted, linux, x64]
    provider: docker
    min: 1
    max: 4
    docker:
      image: ghcr.io/acme/runner:linux
`

func TestParseMinimal(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.GitHub.Org != "acme" {
		t.Errorf("org = %q", cfg.GitHub.Org)
	}
	if got := cfg.GitHub.PollInterval.Duration(); got != DefaultPollInterval {
		t.Errorf("poll_interval default = %s, want %s", got, DefaultPollInterval)
	}
	if got := cfg.Pools[0].IdleTimeout.Duration(); got != DefaultIdleTimeout {
		t.Errorf("idle_timeout default = %s", got)
	}
	if cfg.Pools[0].Docker.Pull != "missing" {
		t.Errorf("pull default = %q", cfg.Pools[0].Docker.Pull)
	}
	if cfg.Pools[0].Docker.WorkDir != DefaultDockerWorkDir {
		t.Errorf("work_dir default = %q", cfg.Pools[0].Docker.WorkDir)
	}
}

func TestDurationParsing(t *testing.T) {
	cfg, err := Parse([]byte(`
github:
  org: acme
  token: t
  poll_interval: 30s
pools:
  - name: linux
    labels: [linux]
    provider: docker
    min: 0
    max: 2
    idle_timeout: 2m30s
    job_timeout: 1h
    docker: {image: img}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.GitHub.PollInterval.Duration(); got != 30*time.Second {
		t.Errorf("poll_interval = %s", got)
	}
	if got := cfg.Pools[0].IdleTimeout.Duration(); got != 150*time.Second {
		t.Errorf("idle_timeout = %s", got)
	}
	if got := cfg.Pools[0].JobTimeout.Duration(); got != time.Hour {
		t.Errorf("job_timeout = %s", got)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("ARC_TEST_TOKEN", "secret-value")

	cfg, err := Parse([]byte(`
github:
  org: acme
  token: ${ARC_TEST_TOKEN}
pools:
  - name: linux
    labels: [linux]
    provider: docker
    min: ${ARC_TEST_MIN:-0}
    max: 2
    docker: {image: img}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.GitHub.Token != "secret-value" {
		t.Errorf("token = %q, want the expanded value", cfg.GitHub.Token)
	}
	if cfg.Pools[0].Min != 0 {
		t.Errorf("min = %d, want the default from ${VAR:-0}", cfg.Pools[0].Min)
	}
}

func TestUndefinedEnvIsAnError(t *testing.T) {
	// Silently expanding to "" would produce a 401 much later, far from the
	// cause. Fail at load instead.
	_, err := Parse([]byte(`
github:
  org: acme
  token: ${ARC_DEFINITELY_NOT_SET}
pools:
  - name: linux
    labels: [linux]
    provider: docker
    min: 0
    max: 1
    docker: {image: img}
`))
	if err == nil {
		t.Fatal("expected an error for an undefined variable")
	}
	if !strings.Contains(err.Error(), "ARC_DEFINITELY_NOT_SET") {
		t.Errorf("error should name the missing variable, got: %v", err)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing org",
			yaml: "github:\n  token: t\npools:\n  - {name: p, labels: [l], provider: docker, min: 0, max: 1, docker: {image: i}}\n",
			want: "github.org is required",
		},
		{
			name: "no credentials",
			yaml: "github:\n  org: acme\npools:\n  - {name: p, labels: [l], provider: docker, min: 0, max: 1, docker: {image: i}}\n",
			want: "github.token or github.app",
		},
		{
			name: "min above max",
			yaml: "github:\n  org: a\n  token: t\npools:\n  - {name: p, labels: [l], provider: docker, min: 5, max: 2, docker: {image: i}}\n",
			want: "exceeds max",
		},
		{
			name: "duplicate pool names",
			yaml: "github:\n  org: a\n  token: t\npools:\n  - {name: p, labels: [l], provider: docker, min: 0, max: 1, docker: {image: i}}\n  - {name: p, labels: [m], provider: docker, min: 0, max: 1, docker: {image: i}}\n",
			want: "duplicate pool name",
		},
		{
			name: "docker pool without image",
			yaml: "github:\n  org: a\n  token: t\npools:\n  - {name: p, labels: [l], provider: docker, min: 0, max: 1, docker: {}}\n",
			want: "docker.image is required",
		},
		{
			// The single most expensive misconfiguration to debug: Docker on a
			// Mac silently gives Linux containers, so a macOS pool on docker
			// produces runners that cannot build Mac software.
			name: "macos pool on docker",
			yaml: "github:\n  org: a\n  token: t\npools:\n  - {name: p, labels: [self-hosted, macos], provider: docker, min: 0, max: 1, docker: {image: i}}\n",
			want: "Docker cannot run macOS containers",
		},
		{
			name: "unknown provider",
			yaml: "github:\n  org: a\n  token: t\npools:\n  - {name: p, labels: [l], provider: tart, min: 0, max: 1}\n",
			want: "unknown provider",
		},
		{
			name: "no pools",
			yaml: "github:\n  org: a\n  token: t\npools: []\n",
			want: "at least one pool is required",
		},
		{
			name: "token and app together",
			yaml: "github:\n  org: a\n  token: t\n  app: {app_id: 1, installation_id: 2, private_key: k}\npools:\n  - {name: p, labels: [l], provider: docker, min: 0, max: 1, docker: {image: i}}\n",
			want: "mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v\nwant it to contain %q", err, tt.want)
			}
		})
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	// A typo in a key would otherwise be silently ignored, leaving the operator
	// convinced they set something they did not.
	_, err := Parse([]byte(`
github:
  org: acme
  token: t
  pol_interval: 30s
pools:
  - {name: p, labels: [l], provider: docker, min: 0, max: 1, docker: {image: i}}
`))
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "pol_interval") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

func TestWindowsPoolGetsWindowsWorkDir(t *testing.T) {
	cfg, err := Parse([]byte(`
github:
  org: acme
  token: t
pools:
  - name: win
    labels: [self-hosted, windows, x64]
    provider: docker
    min: 0
    max: 2
    docker: {image: img}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Pools[0].Docker.WorkDir; got != DefaultWinDockerWorkDir {
		t.Errorf("windows work_dir = %q, want %q", got, DefaultWinDockerWorkDir)
	}
}

func TestProcessProviderDefaults(t *testing.T) {
	// A process pool with no template_dir should get a sensible default, and a
	// completely omitted process: block should also be tolerated.
	cfg, err := Parse([]byte(`
github:
  org: acme
  token: t
pools:
  - name: mac
    labels: [self-hosted, macos, arm64]
    provider: process
    min: 1
    max: 3
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := cfg.Pools[0]
	if p.Process == nil {
		t.Fatal("process block should have been auto-created")
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".arc", "runner-template")
	if p.Process.TemplateDir != want {
		t.Errorf("template_dir default = %q, want %q", p.Process.TemplateDir, want)
	}
	if p.Process.InstancesDir == "" {
		t.Error("instances_dir should have been defaulted")
	}
}

func TestDarwinDefaultsToProcessProvider(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only default")
	}
	cfg, err := Parse([]byte(`
github:
  org: acme
  token: t
pools:
  - name: mac
    labels: [self-hosted, macos, arm64]
    min: 1
    max: 3
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Pools[0].Provider; got != ProviderProcess {
		t.Errorf("default provider on darwin = %q, want %q", got, ProviderProcess)
	}
}
