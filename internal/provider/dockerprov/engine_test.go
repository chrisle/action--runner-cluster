package dockerprov

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"1024", 1024},
		{"512m", 512 << 20},
		{"4g", 4 << 30},
		{"4G", 4 << 30},
		{"2gb", 2 << 30},
		{"1.5g", 1610612736},
		{"256k", 256 << 10},
	}
	for _, tt := range tests {
		got, err := parseBytes(tt.in)
		if err != nil {
			t.Errorf("parseBytes(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseBytes(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}

	if _, err := parseBytes("many gigabytes"); err == nil {
		t.Error("expected an error for unparseable input")
	}
}

func TestSplitImageTag(t *testing.T) {
	tests := []struct {
		in       string
		wantName string
		wantTag  string
	}{
		{"ubuntu", "ubuntu", "latest"},
		{"ubuntu:24.04", "ubuntu", "24.04"},
		{"ghcr.io/acme/runner:v1", "ghcr.io/acme/runner", "v1"},
		{"ghcr.io/acme/runner", "ghcr.io/acme/runner", "latest"},
		// A port in the registry host is not a tag; splitting on the last colon
		// naively would try to pull tag "5000/runner".
		{"registry:5000/runner", "registry:5000/runner", "latest"},
		{"registry:5000/runner:v2", "registry:5000/runner", "v2"},
		{"ubuntu@sha256:abc", "ubuntu", "sha256:abc"},
	}
	for _, tt := range tests {
		name, tag := splitImageTag(tt.in)
		if name != tt.wantName || tag != tt.wantTag {
			t.Errorf("splitImageTag(%q) = (%q, %q), want (%q, %q)",
				tt.in, name, tag, tt.wantName, tt.wantTag)
		}
	}
}

// frame builds one Docker log stream frame.
func frame(stream byte, payload string) []byte {
	var b bytes.Buffer
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	b.Write(header)
	b.WriteString(payload)
	return b.Bytes()
}

func TestDemuxLogs(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, "starting runner\n"))
	in.Write(frame(2, "warning: no cache\n"))
	in.Write(frame(1, "listening for jobs\n"))

	got, err := demuxLogs(&in)
	if err != nil {
		t.Fatalf("demuxLogs: %v", err)
	}
	want := "starting runner\nwarning: no cache\nlistening for jobs\n"
	if got != want {
		t.Errorf("demuxLogs = %q, want %q", got, want)
	}
}

func TestDemuxLogsPassesThroughUnframedOutput(t *testing.T) {
	// A TTY-attached container returns raw text with no frame headers. Trying
	// to demux it would corrupt the first eight bytes.
	raw := "plain text with no framing at all\n"
	got, err := demuxLogs(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("demuxLogs: %v", err)
	}
	if got != raw {
		t.Errorf("demuxLogs = %q, want %q", got, raw)
	}
}

func TestDemuxLogsHandlesTruncatedFrame(t *testing.T) {
	// Log tails get cut mid-frame all the time; this must return what it has
	// rather than failing.
	in := append(frame(1, "complete line\n"), 1, 0, 0, 0, 0, 0, 0)
	got, err := demuxLogs(bytes.NewReader(in))
	if err != nil {
		t.Fatalf("demuxLogs: %v", err)
	}
	if !strings.Contains(got, "complete line") {
		t.Errorf("demuxLogs dropped complete data: %q", got)
	}
}

func TestNewEngineRejectsUnsupportedHosts(t *testing.T) {
	if _, err := newEngine("npipe:////./pipe/docker_engine", nil); err == nil {
		t.Error("expected npipe to be rejected with guidance")
	} else if !strings.Contains(err.Error(), "ssh://") {
		t.Errorf("npipe error should suggest an alternative, got: %v", err)
	}

	if _, err := newEngine("carrier-pigeon://somewhere", nil); err == nil {
		t.Error("expected an unknown scheme to be rejected")
	}
}

func TestNewEngineAcceptsSupportedHosts(t *testing.T) {
	for _, host := range []string{
		"unix:///var/run/docker.sock",
		"tcp://192.168.1.10:2375",
		"ssh://runner@build-box",
		"",
	} {
		if _, err := newEngine(host, nil); err != nil {
			t.Errorf("newEngine(%q): %v", host, err)
		}
	}
}
