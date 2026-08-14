package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// updateRepo is where the release workflow publishes binaries.
const updateRepo = "chrisle/action--runner-cluster"

// updateAPIBase is a var so tests can point it at a stub server.
var updateAPIBase = "https://api.github.com"

// cmdUpdate replaces the running binary with the latest GitHub release.
func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "report whether a newer release exists, without installing")
	force := fs.Bool("force", false, "reinstall even when already on the latest version")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := &http.Client{}

	rel, err := latestRelease(ctx, client)
	if err != nil {
		return fmt.Errorf("look up the latest release: %w", err)
	}

	fmt.Printf("current %s, latest %s\n", version, rel.Tag)
	if !*force {
		switch {
		case version == rel.Tag:
			fmt.Println("Already up to date.")
			return nil
		case strings.HasPrefix(version, rel.Tag+"-"):
			// A git-describe version like v1.1.0-3-gabc123: built from commits
			// on top of the latest tag. Overwriting it with the release would
			// be a downgrade.
			fmt.Printf("This is a development build ahead of %s; use -force to overwrite it.\n", rel.Tag)
			return nil
		}
	}
	if *check {
		fmt.Printf("Run \"arc update\" to install %s.\n", rel.Tag)
		return nil
	}

	asset := assetName()
	var url string
	for _, a := range rel.Assets {
		if a.Name == asset {
			url = a.URL
		}
	}
	if url == "" {
		return fmt.Errorf("release %s has no asset %q for %s/%s", rel.Tag, asset, runtime.GOOS, runtime.GOARCH)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Replace what the symlink points at, not the symlink itself.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// A leftover from a previous Windows update; harmless elsewhere.
	_ = os.Remove(exe + ".old")

	fmt.Printf("downloading %s → %s\n", asset, exe)
	if err := installTo(ctx, client, url, exe); err != nil {
		return err
	}
	fmt.Printf("Updated to %s.\n", rel.Tag)
	return nil
}

type release struct {
	Tag    string
	Assets []releaseAsset
}

type releaseAsset struct {
	Name string
	URL  string
}

func latestRelease(ctx context.Context, c *http.Client) (*release, error) {
	url := updateAPIBase + "/repos/" + updateRepo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "arc/"+version)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s has no releases", updateRepo)
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("github responded %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var raw struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	rel := &release{Tag: raw.TagName}
	for _, a := range raw.Assets {
		rel.Assets = append(rel.Assets, releaseAsset{Name: a.Name, URL: a.BrowserDownloadURL})
	}
	return rel, nil
}

// assetName is the release artifact for this platform, as `make release`
// names them.
func assetName() string {
	name := fmt.Sprintf("arc-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// installTo downloads url over exePath. The download lands in a temp file in
// the same directory, so the final rename never crosses a filesystem and the
// running binary is replaced in one atomic step — a failure at any point
// leaves the current install untouched.
func installTo(ctx context.Context, c *http.Client, url, exePath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "arc/"+version)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s responded %d", url, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(exePath), ".arc-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s (rerun with elevated permissions?): %w", exePath, err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed into place

	n, copyErr := io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	switch {
	case copyErr != nil:
		return fmt.Errorf("download interrupted: %w", copyErr)
	case closeErr != nil:
		return closeErr
	case n == 0:
		return fmt.Errorf("download from %s was empty", url)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if err := verifyNewBinary(tmp.Name()); err != nil {
		return err
	}
	return swapBinary(tmp.Name(), exePath)
}

// verifyNewBinary is a var so tests can stub it: test fixtures are not
// executables.
var verifyNewBinary = func(path string) error {
	out, err := exec.Command(path, "version").CombinedOutput()
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if err != nil {
		return fmt.Errorf("downloaded binary does not run: %v (%s)", err, line)
	}
	if !strings.HasPrefix(line, "arc ") {
		return fmt.Errorf("downloaded binary does not look like arc: %q", line)
	}
	return nil
}

// swapBinary replaces dst with src. Windows cannot overwrite a running
// executable but can rename it, so the old one moves aside first; the
// leftover is cleaned up by the next update.
func swapBinary(src, dst string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(src, dst)
	}
	old := dst + ".old"
	_ = os.Remove(old)
	if err := os.Rename(dst, old); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		_ = os.Rename(old, dst) // roll back
		return err
	}
	return nil
}
