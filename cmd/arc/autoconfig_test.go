package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/chrisle/action-runner-cluster/internal/ghapi"
	"github.com/chrisle/action-runner-cluster/internal/opconfig"
)

func stubAutoConfig(t *testing.T) (*int, *opconfig.Item) {
	t.Helper()
	dir := t.TempDir()
	origPath, origLoad, origProbe := opconfig.CachePath, loadVaultItem, probeAccountType
	opconfig.CachePath = func() string { return filepath.Join(dir, "credentials.json") }

	vaultItem := &opconfig.Item{Token: "ghp_fromvault", Login: "chrisle"}
	opCalls := 0
	loadVaultItem = func(context.Context) (*opconfig.Item, error) {
		opCalls++
		copy := *vaultItem
		return &copy, nil
	}
	probeAccountType = func(_ context.Context, item *opconfig.Item) (string, error) {
		return "User", nil
	}
	t.Cleanup(func() {
		opconfig.CachePath, loadVaultItem, probeAccountType = origPath, origLoad, origProbe
	})
	return &opCalls, vaultItem
}

func TestAutoConfigReadsVaultOnceThenCache(t *testing.T) {
	opCalls, _ := stubAutoConfig(t)

	cfg, err := autoConfig(4)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.Owner != "chrisle" || cfg.GitHub.Token != "ghp_fromvault" {
		t.Errorf("cfg github = %+v", cfg.GitHub)
	}
	if *opCalls != 1 {
		t.Fatalf("first run: op calls = %d, want 1", *opCalls)
	}

	// Second run must come from the cache — and still honor a new -max.
	cfg2, err := autoConfig(8)
	if err != nil {
		t.Fatal(err)
	}
	if *opCalls != 1 {
		t.Errorf("second run: op calls = %d, want still 1 (cache)", *opCalls)
	}
	if cfg2.Pools[0].Max != 8 {
		t.Errorf("max = %d, want the fresh flag value 8", cfg2.Pools[0].Max)
	}
	if cfg2.Path != "1Password op://arc/github (cached)" {
		t.Errorf("path = %q", cfg2.Path)
	}
}

func TestAutoConfigRefreshesWhenCachedTokenRejected(t *testing.T) {
	opCalls, vaultItem := stubAutoConfig(t)

	if _, err := autoConfig(4); err != nil {
		t.Fatal(err)
	}

	// The token gets rotated in the vault; GitHub now rejects the cached one.
	vaultItem.Token = "ghp_rotated"
	probeAccountType = func(_ context.Context, item *opconfig.Item) (string, error) {
		if item.Token == "ghp_rotated" {
			return "User", nil
		}
		return "", &ghapi.APIError{Status: http.StatusUnauthorized, Message: "bad credentials"}
	}

	cfg, err := autoConfig(4)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.Token != "ghp_rotated" {
		t.Errorf("token = %q, want the refreshed one", cfg.GitHub.Token)
	}
	if *opCalls != 2 {
		t.Errorf("op calls = %d, want 2 (initial + refresh)", *opCalls)
	}
	// And the cache must now hold the fresh token.
	if cached := opconfig.ReadCache(); cached == nil || cached.Token != "ghp_rotated" {
		t.Errorf("cache after refresh = %+v", cached)
	}
}

func TestAutoConfigToleratesGitHubOutageWithValidatedCache(t *testing.T) {
	opCalls, _ := stubAutoConfig(t)
	if _, err := autoConfig(4); err != nil {
		t.Fatal(err)
	}

	// GitHub unreachable — a boot-time service must still come up.
	probeAccountType = func(context.Context, *opconfig.Item) (string, error) {
		return "", errors.New("dial tcp: network is unreachable")
	}
	cfg, err := autoConfig(4)
	if err != nil {
		t.Fatalf("outage with validated cache should not fail: %v", err)
	}
	if cfg.GitHub.Owner != "chrisle" {
		t.Errorf("cfg github = %+v", cfg.GitHub)
	}
	if *opCalls != 1 {
		t.Errorf("op calls = %d, want 1 (no refresh on network trouble)", *opCalls)
	}
}
