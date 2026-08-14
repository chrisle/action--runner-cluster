package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/chrisle/action-runner-cluster/internal/config"
)

// arc installs itself as the host's native background service, so "arc
// install" once is the whole setup: launchd on macOS, systemd on Linux, a
// logon scheduled task on Windows. "arc start"/"arc stop" then drive it.

const launchdLabel = "com.arc.runner"

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	maxRunners := fs.Int("max", config.DefaultMaxRunners,
		"max concurrent runners for this machine's pool")
	if err := fs.Parse(args); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(exe, *maxRunners)
	case "linux":
		return installSystemd(exe, *maxRunners)
	case "windows":
		return installTask(exe, *maxRunners)
	default:
		return fmt.Errorf("no service installer for %s", runtime.GOOS)
	}
}

func cmdStartStop(action string) error {
	switch runtime.GOOS {
	case "darwin":
		uid := os.Getuid()
		if action == "stop" {
			return runCmd("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
		}
		return runCmd("launchctl", "bootstrap", fmt.Sprintf("gui/%d", uid), launchdPlistPath())
	case "linux":
		return runCmd("systemctl", action, "arc")
	case "windows":
		if action == "stop" {
			// /End kills the task's process tree: the launcher loop and arc.
			return runCmd("schtasks", "/End", "/TN", "arc")
		}
		return runCmd("schtasks", "/Run", "/TN", "arc")
	default:
		return fmt.Errorf("no service control for %s", runtime.GOOS)
	}
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err,
			strings.TrimSpace(string(out)))
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		fmt.Println(s)
	}
	return nil
}

// --- macOS -----------------------------------------------------------------

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

// launchdPlist keeps arc alive in the user's session. AbandonProcessGroup
// matters: without it a restart would kill runners mid-job. The optional
// ~/.arc/env sourcing lets a host keep its op service-account token there;
// once the credentials cache exists it is not needed at all.
func launchdPlist(exe string, max int) string {
	home, _ := os.UserHomeDir()
	script := fmt.Sprintf(
		`export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"; set -a; [ -f "$HOME/.arc/env" ] && . "$HOME/.arc/env"; set +a; exec "%s" run --max=%d`,
		exe, max)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>-c</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>AbandonProcessGroup</key><true/>
  <key>StandardOutPath</key><string>%s/.arc/arc.log</string>
  <key>StandardErrorPath</key><string>%s/.arc/arc.log</string>
</dict>
</plist>
`, launchdLabel, script, home, home)
}

func installLaunchd(exe string, max int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, ".arc"), 0o755); err != nil {
		return err
	}
	path := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(launchdPlist(exe, max)), 0o644); err != nil {
		return err
	}
	uid := os.Getuid()
	// Replace any loaded copy so install doubles as reinstall.
	_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, launchdLabel)).Run()
	if err := runCmd("launchctl", "bootstrap", fmt.Sprintf("gui/%d", uid), path); err != nil {
		return err
	}
	fmt.Printf("Installed and started %s (log: ~/.arc/arc.log)\nControl it with: arc stop / arc start\n", launchdLabel)
	return nil
}

// --- Linux -----------------------------------------------------------------

// systemdUnit runs arc as the installing user. KillMode=process so restarts
// never kill runners mid-job — they finish, exit, and arc re-adopts state.
// The EnvironmentFile is optional (leading dash): with a credentials cache
// present nothing else is needed.
func systemdUnit(exe, user string, max int) string {
	return fmt.Sprintf(`[Unit]
Description=arc GitHub Actions runner cluster
After=network-online.target
Wants=network-online.target

[Service]
User=%s
EnvironmentFile=-/etc/arc/env
ExecStart=%s run --max=%d
Restart=always
RestartSec=5
# Never kill runners on restart: they finish their job and exit, and arc
# re-adopts them from on-disk state when it comes back.
KillMode=process

[Install]
WantedBy=multi-user.target
`, user, exe, max)
}

func installSystemd(exe string, max int) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing the systemd service needs root: sudo arc install")
	}
	user := os.Getenv("SUDO_USER")
	if user == "" || user == "root" {
		return fmt.Errorf("run via sudo from the account that should own the runners " +
			"(sudo arc install), not as root directly")
	}
	unit := "/etc/systemd/system/arc.service"
	if err := os.WriteFile(unit, []byte(systemdUnit(exe, user, max)), 0o644); err != nil {
		return err
	}
	if err := runCmd("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCmd("systemctl", "enable", "--now", "arc"); err != nil {
		return err
	}
	fmt.Println("Installed and started arc.service (logs: journalctl -u arc)\n" +
		"Control it with: sudo arc stop / sudo arc start")
	return nil
}

// --- Windows ---------------------------------------------------------------

// windowsLauncher loops because scheduled tasks have no KeepAlive: if arc
// exits (crash, update, port briefly taken), the loop brings it back.
func windowsLauncher(exe string, max int) string {
	return fmt.Sprintf(`$tok = "$env:LOCALAPPDATA\arc\op-token.txt"
if (Test-Path $tok) { $env:OP_SERVICE_ACCOUNT_TOKEN = (Get-Content $tok -Raw).Trim() }
while ($true) {
  & "%s" run --max=%d *>> "$env:LOCALAPPDATA\arc\arc.log"
  Start-Sleep -Seconds 30
}
`, exe, max)
}

func installTask(exe string, max int) error {
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "arc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	launcher := filepath.Join(dir, "run-arc.ps1")
	if err := os.WriteFile(launcher, []byte(windowsLauncher(exe, max)), 0o644); err != nil {
		return err
	}
	tr := fmt.Sprintf(`powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "%s"`, launcher)
	if err := runCmd("schtasks", "/Create", "/F", "/TN", "arc", "/SC", "ONLOGON", "/TR", tr); err != nil {
		return err
	}
	if err := runCmd("schtasks", "/Run", "/TN", "arc"); err != nil {
		return err
	}
	fmt.Printf("Installed and started scheduled task \"arc\" (log: %s\\arc.log)\n"+
		"Control it with: arc stop / arc start\n", dir)
	return nil
}
