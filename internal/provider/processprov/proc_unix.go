//go:build !windows

package processprov

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// configureProcAttr puts the runner in its own process group so the whole job
// tree can be signalled at once.
func configureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processAlive reports whether a pid is running. Signal 0 performs the
// permission and existence check without delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// signalGroup sends a signal to the whole process group. The negative pid is
// what addresses the group rather than the leader alone, so job steps that
// spawned their own children get the signal too.
func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		// The group may already be gone; fall back to the leader.
		return syscall.Kill(pid, sig)
	}
	return nil
}

func killGroup(pid int) error {
	return signalGroup(pid, syscall.SIGKILL)
}

// pidVerifier guards against pid reuse. After the orchestrator restarts, a pid
// recorded in an instance state file may have been recycled by an unrelated
// process, and signalling it would kill something that isn't ours. Each pid is
// checked against its command line once and the answer is remembered.
type pidVerifier struct {
	mu   sync.Mutex
	seen map[string]bool
}

var verifier = pidVerifier{seen: map[string]bool{}}

// processMatches reports whether pid actually looks like the runner for dir.
func processMatches(pid int, dir string) bool {
	key := strconv.Itoa(pid) + "\x00" + dir
	verifier.mu.Lock()
	if v, ok := verifier.seen[key]; ok {
		verifier.mu.Unlock()
		return v
	}
	verifier.mu.Unlock()

	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	// If ps is unavailable the check cannot be made; assume the pid is ours
	// rather than destroying a live runner on a diagnostic failure.
	match := err != nil
	if err == nil {
		cmdline := string(out)
		match = strings.Contains(cmdline, dir) ||
			strings.Contains(cmdline, "Runner.Listener") ||
			strings.Contains(cmdline, "run.sh")
	}

	verifier.mu.Lock()
	verifier.seen[key] = match
	verifier.mu.Unlock()
	return match
}
