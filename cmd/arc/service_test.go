package main

import (
	"strings"
	"testing"
)

func TestLaunchdPlist(t *testing.T) {
	p := launchdPlist("/Users/x/.local/bin/arc", 4)
	for _, want := range []string{
		"<string>com.arc.runner</string>",
		"run --max=4",
		"<key>KeepAlive</key><true/>",
		// Without AbandonProcessGroup a restart kills runners mid-job.
		"<key>AbandonProcessGroup</key><true/>",
		// launchd's minimal PATH cannot find op otherwise.
		"/opt/homebrew/bin",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestSystemdUnit(t *testing.T) {
	u := systemdUnit("/usr/local/bin/arc", "chrisle", 6)
	for _, want := range []string{
		"User=chrisle",
		"ExecStart=/usr/local/bin/arc run --max=6",
		"Restart=always",
		"KillMode=process",
		// Optional: with a credentials cache no env file is needed.
		"EnvironmentFile=-/etc/arc/env",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

func TestWindowsLauncher(t *testing.T) {
	l := windowsLauncher(`C:\Users\c\arc.exe`, 2)
	for _, want := range []string{
		`run --max=2`,
		"Start-Sleep -Seconds 30", // the KeepAlive loop schtasks lacks
		"op-token.txt",
	} {
		if !strings.Contains(l, want) {
			t.Errorf("launcher missing %q", want)
		}
	}
	if !strings.Contains(l, "while ($true)") {
		t.Error("launcher has no restart loop")
	}
}
