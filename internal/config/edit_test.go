package config

import (
	"strings"
	"testing"
)

const editableYAML = `github:
  org: acme
  token: ${GITHUB_TOKEN}
pools:
  - name: linux
    labels: [self-hosted, linux, x64]
    provider: docker
    min: 0
    max: 4
    docker:
      image: ghcr.io/acme/arc-runner:linux
`

func TestParseEditableKeepsEnvRefs(t *testing.T) {
	cfg, err := ParseEditable([]byte(editableYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.Token != "${GITHUB_TOKEN}" {
		t.Errorf("token = %q, want the unexpanded reference", cfg.GitHub.Token)
	}
	// No defaults may leak in: what is absent in the file stays absent.
	if cfg.GitHub.PollInterval != 0 {
		t.Errorf("poll_interval = %v, want zero (unset)", cfg.GitHub.PollInterval)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	cfg, err := ParseEditable([]byte(editableYAML))
	if err != nil {
		t.Fatal(err)
	}
	out, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "null") {
		t.Errorf("marshal emitted null fields:\n%s", out)
	}
	// min: 0 is meaningful and must survive even with omitempty elsewhere.
	if !strings.Contains(string(out), "min: 0") {
		t.Errorf("marshal dropped min: 0:\n%s", out)
	}
	back, err := ParseEditable(out)
	if err != nil {
		t.Fatalf("marshal output does not reparse: %v\n%s", err, out)
	}
	if back.GitHub.Token != "${GITHUB_TOKEN}" || back.Pools[0].Docker.Image != cfg.Pools[0].Docker.Image {
		t.Errorf("round trip changed values: %+v", back)
	}
	if err := CheckBytes(out); err != nil {
		t.Errorf("CheckBytes rejected a valid config: %v", err)
	}
}

func TestCheckBytesCatchesInvalid(t *testing.T) {
	bad := strings.Replace(editableYAML, "org: acme", "org: \"\"", 1)
	if err := CheckBytes([]byte(bad)); err == nil {
		t.Error("want an error for a config with no org")
	}
}
