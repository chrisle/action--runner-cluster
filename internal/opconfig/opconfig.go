// Package opconfig pulls arc's account configuration from 1Password, so a
// host needs no config file at all: `arc run` reads the vault, derives a pool
// for its own platform, and starts serving jobs.
//
// The contract is one item:
//
//	vault "arc", item "github"
//	  credential — a GitHub token (classic PAT with repo scope, plus
//	               admin:org when the account is an organization)
//	  username   — the GitHub account the runners belong to
//
// Secrets stay in 1Password; arc holds them only in memory. The op CLI does
// the authentication — the desktop app integration (biometric prompt) or an
// OP_SERVICE_ACCOUNT_TOKEN environment variable both work.
package opconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Item is what the vault provides.
type Item struct {
	// Token is the GitHub credential.
	Token string `json:"token"`
	// Login is the GitHub account (user or org) runners belong to.
	Login string `json:"login"`
	// AccountType is "User" or "Organization", filled in after the login is
	// verified against GitHub and kept in the cache.
	AccountType string `json:"account_type,omitempty"`
	// FetchedAt records when the vault was last read.
	FetchedAt time.Time `json:"fetched_at,omitempty"`
}

// execOP is swapped out in tests.
var execOP = func(ctx context.Context) ([]byte, error) {
	if _, err := exec.LookPath("op"); err != nil {
		return nil, errors.New("the 1Password CLI (op) is not installed")
	}
	cmd := exec.CommandContext(ctx, "op", "item", "get", "github", "--vault", "arc", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("op: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// Load reads the vault item. One op invocation: each call can cost the user
// an authorization prompt, so everything comes from a single item get.
func Load(ctx context.Context) (*Item, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	raw, err := execOP(ctx)
	if err != nil {
		return nil, fmt.Errorf("read 1Password item \"github\" in vault \"arc\": %w\n"+
			"Set it up with: op vault create arc && op item create --vault arc "+
			"--category \"API Credential\" --title github credential=<token> username=<account>", err)
	}

	var doc struct {
		Fields []struct {
			Label string `json:"label"`
			Value string `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse op output: %w", err)
	}

	item := &Item{}
	for _, f := range doc.Fields {
		switch f.Label {
		case "credential":
			item.Token = f.Value
		case "username":
			item.Login = f.Value
		}
	}
	if item.Token == "" {
		return nil, errors.New("1Password item arc/github has no credential field value")
	}
	if item.Login == "" {
		return nil, errors.New("1Password item arc/github has no username field value; " +
			"set it to the GitHub account (user or org) the runners belong to")
	}
	item.FetchedAt = time.Now()
	return item, nil
}

// The vault is read once and cached: op can cost an interactive authorization
// per invocation, and a host running arc as a service should not depend on op
// being reachable on every restart. A cached token that GitHub stops
// accepting triggers a fresh vault read (see the caller).

// CachePath is a var so tests can redirect it.
var CachePath = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".arc", "credentials.json")
}

// ReadCache returns the cached item, or nil when absent or unreadable.
// Deleting the file is always safe; the next run re-reads the vault.
func ReadCache() *Item {
	path := CachePath()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var item Item
	if err := json.Unmarshal(raw, &item); err != nil || item.Token == "" || item.Login == "" {
		return nil
	}
	return &item
}

// WriteCache persists the item with owner-only permissions.
func WriteCache(item *Item) error {
	path := CachePath()
	if path == "" {
		return errors.New("no home directory for the credentials cache")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
