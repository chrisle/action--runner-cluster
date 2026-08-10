package processprov

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRobocopyExitCodes pins the one piece of the Windows clone path that is
// easy to get backwards. robocopy reports what it did rather than whether it
// worked: a successful copy exits 1, not 0. Reading that as a failure the
// ordinary way would break every runner launch on Windows.
func TestRobocopyExitCodes(t *testing.T) {
	tests := []struct {
		code int
		want bool
		why  string
	}{
		{0, true, "nothing needed copying"},
		{1, true, "files copied — the normal success case"},
		{2, true, "extra files in destination"},
		{3, true, "files copied plus extras"},
		{4, true, "mismatched files present"},
		{5, true, "copied plus mismatched"},
		{6, true, "extras plus mismatched"},
		{7, true, "copied, extras and mismatched"},
		{8, false, "at least one file could not be copied"},
		{9, false, "copy failure plus other bits"},
		{16, false, "serious error, no files copied"},
	}

	for _, tt := range tests {
		if got := robocopyOK(tt.code); got != tt.want {
			t.Errorf("robocopyOK(%d) = %v, want %v (%s)", tt.code, got, tt.want, tt.why)
		}
	}
}

// TestRobocopyRejectsNegativeExit guards the case where the process was killed
// by a signal or never ran, which surfaces as a negative exit code.
func TestRobocopyRejectsNegativeExit(t *testing.T) {
	if robocopyOK(-1) {
		t.Error("robocopyOK(-1) = true; a killed process is not a successful copy")
	}
}

// TestCloneTreeCopiesContents verifies the shared copy loop still works on this
// platform. The Windows branch cannot run here, but the loop it shares with the
// macOS and Linux branches can, and that loop was reworked to carry per-tool
// exit-code handling.
func TestCloneTreeCopiesContents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by robocopy exit-code tests; needs a Windows host to run")
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin", "Runner.Listener"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "instance")
	if err := cloneTree(context.Background(), src, dst); err != nil {
		t.Fatalf("cloneTree: %v", err)
	}

	for _, want := range []string{"run.sh", filepath.Join("bin", "Runner.Listener")} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("expected %s in the clone: %v", want, err)
		}
	}

	// The entrypoint must stay executable, or the runner cannot start.
	info, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("run.sh lost its executable bit: mode %v", info.Mode())
	}
}
