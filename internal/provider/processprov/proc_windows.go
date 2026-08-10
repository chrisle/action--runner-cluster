//go:build windows

package processprov

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// configureProcAttr gives the runner its own process group so taskkill /T can
// take down the whole job tree.
func configureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// processAlive reports whether a pid is running, via tasklist. Windows has no
// signal-0 equivalent, and OpenProcess succeeds on exited-but-unreaped
// handles, so the process list is the reliable check.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "\""+strconv.Itoa(pid)+"\"")
}

// signalGroup asks the process tree to close. Windows has no SIGTERM for a
// non-console child, so this is a taskkill without /F, which posts WM_CLOSE and
// lets the runner unregister itself.
func signalGroup(pid int, _ syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T").Run()
}

// killGroup terminates the process tree unconditionally.
func killGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}

// processMatches verifies a pid belongs to a runner rather than being a reused
// pid. tasklist cannot show command lines, so this checks the image name.
func processMatches(pid int, _ string) bool {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return true // cannot tell; assume ours rather than orphaning a runner
	}
	line := strings.ToLower(string(out))
	return strings.Contains(line, "runner.listener") ||
		strings.Contains(line, "cmd.exe") ||
		strings.Contains(line, "run.cmd")
}
