package processprov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Overridable in tests.
var (
	runnerReleaseAPI  = "https://api.github.com"
	runnerDownloadURL = "https://github.com/actions/runner/releases/download"
)

// ensureTemplate provisions TemplateDir with an unconfigured actions-runner
// when the directory does not exist at all, so a fresh host works with zero
// manual setup. An existing directory — even a broken one — is never touched:
// the rest of Preflight judges its contents, and overwriting something the
// user built by hand would be worse than failing.
func (p *Provider) ensureTemplate(ctx context.Context) error {
	tmpl := p.spec.TemplateDir
	if tmpl == "" {
		return errors.New("template_dir is empty")
	}
	if _, err := os.Stat(tmpl); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	version, err := latestRunnerVersion(ctx)
	if err != nil {
		return fmt.Errorf("resolve the latest actions-runner release: %w", err)
	}
	archive := runnerArchiveName(version)
	url := fmt.Sprintf("%s/v%s/%s", runnerDownloadURL, version, archive)
	p.log.Info("runner template missing; downloading it",
		"dir", tmpl, "version", version, "archive", archive)

	// Stage next to the final path and rename at the end, so an interrupted
	// download can never masquerade as a working template.
	staging := tmpl + ".download"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()

	archivePath := filepath.Join(staging, archive)
	size, err := downloadFile(ctx, url, archivePath)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	p.log.Info("downloaded runner archive", "mb", size/(1<<20))

	if err := extractArchive(ctx, archivePath, staging); err != nil {
		return fmt.Errorf("extract %s: %w", archive, err)
	}
	if err := os.Remove(archivePath); err != nil {
		return err
	}

	// A fresh archive is unconfigured, but be explicit: credentials cloned
	// into every instance would make the runners fight over one identity.
	for _, stale := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		_ = os.Remove(filepath.Join(staging, stale))
	}

	if _, err := os.Stat(filepath.Join(staging, p.runnerEntrypoint())); err != nil {
		return fmt.Errorf("downloaded archive does not contain %s; refusing to install it",
			p.runnerEntrypoint())
	}

	if runtime.GOOS == "darwin" {
		// Nothing in this download path sets quarantine, but a quarantined
		// runner binary would fail every job with a Gatekeeper prompt no one
		// is there to click — clear it just in case.
		_ = exec.CommandContext(ctx, "xattr", "-dr", "com.apple.quarantine", staging).Run()
	}

	if err := os.Rename(staging, tmpl); err != nil {
		return err
	}
	committed = true
	p.log.Info("runner template ready", "dir", tmpl, "version", version)
	return nil
}

func latestRunnerVersion(ctx context.Context) (string, error) {
	url := runnerReleaseAPI + "/repos/actions/runner/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "arc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github responded %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	version := strings.TrimPrefix(body.TagName, "v")
	if version == "" {
		return "", errors.New("release has no tag name")
	}
	return version, nil
}

// runnerArchiveName is the artifact name actions/runner publishes for this
// platform, e.g. actions-runner-osx-arm64-2.336.0.tar.gz.
func runnerArchiveName(version string) string {
	osName := runtime.GOOS
	switch osName {
	case "darwin":
		osName = "osx"
	case "windows":
		osName = "win"
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("actions-runner-%s-%s-%s%s", osName, arch, version, ext)
}

func downloadFile(ctx context.Context, url, dst string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "arc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("responded %d", resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		f.Close()
		return 0, err
	}
	if n == 0 {
		f.Close()
		return 0, errors.New("download was empty")
	}
	return n, f.Close()
}

// extractArchive unpacks with the system tar, which handles .tar.gz on every
// platform — and .zip too on Windows, where tar is bsdtar (shipped since
// Windows 10 1803).
func extractArchive(ctx context.Context, archive, dst string) error {
	args := []string{"-x", "-f", archive, "-C", dst}
	if strings.HasSuffix(archive, ".tar.gz") {
		args = append([]string{"-z"}, args...)
	}
	cmd := exec.CommandContext(ctx, "tar", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
