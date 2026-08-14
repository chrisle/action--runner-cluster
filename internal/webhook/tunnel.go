package webhook

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
)

// cloudflaredBase is a var so tests can point downloads at a stub server.
var cloudflaredBase = "https://github.com/cloudflare/cloudflared/releases/latest/download"

// Tunnel is a running Cloudflare quick tunnel exposing a local address.
//
// Quick tunnels need no Cloudflare account: cloudflared assigns a random
// https://<name>.trycloudflare.com URL per start. arc re-registers its
// webhooks whenever the URL changes, so the randomness costs nothing.
type Tunnel struct {
	// URL is the public https endpoint.
	URL string

	cmd  *exec.Cmd
	done chan error
}

var tunnelURLRE = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// StartTunnel launches cloudflared (downloading it first if needed) and waits
// for the public URL to be assigned.
func StartTunnel(ctx context.Context, localAddr string, log *slog.Logger) (*Tunnel, error) {
	bin, err := ensureCloudflared(ctx, log)
	if err != nil {
		return nil, fmt.Errorf("cloudflared: %w", err)
	}

	cmd := exec.Command(bin, "tunnel", "--url", "http://"+localAddr, "--no-autoupdate")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cloudflared: %w", err)
	}

	t := &Tunnel{cmd: cmd, done: make(chan error, 1)}

	// cloudflared prints the assigned URL to stderr shortly after start.
	urlCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if m := tunnelURLRE.FindString(line); m != "" {
				select {
				case urlCh <- m:
				default:
				}
			}
		}
	}()
	go func() { t.done <- cmd.Wait() }()

	select {
	case url := <-urlCh:
		t.URL = url
		return t, nil
	case err := <-t.done:
		return nil, fmt.Errorf("cloudflared exited before assigning a URL: %v", err)
	case <-time.After(60 * time.Second):
		t.Stop()
		return nil, fmt.Errorf("cloudflared did not assign a tunnel URL within 60s")
	case <-ctx.Done():
		t.Stop()
		return nil, ctx.Err()
	}
}

// Done reports the tunnel process exiting, for supervision.
func (t *Tunnel) Done() <-chan error { return t.done }

// Stop kills the tunnel process.
func (t *Tunnel) Stop() {
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
}

// ensureCloudflared returns a cloudflared binary, downloading the official
// release into ~/.arc/bin on first use so arc stays the only thing a host
// has to install.
func ensureCloudflared(ctx context.Context, log *slog.Logger) (string, error) {
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := "cloudflared"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dest := filepath.Join(home, ".arc", "bin", name)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	asset, packaged := cloudflaredAsset()
	url := cloudflaredBase + "/" + asset
	log.Info("downloading cloudflared", "url", url, "dest", dest)

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".cloudflared-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %d", url, resp.StatusCode)
	}
	n, err := io.Copy(tmp, resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil || n == 0 {
		return "", fmt.Errorf("download %s failed: %v (%d bytes)", url, err, n)
	}

	if packaged {
		// The darwin release is a .tgz holding the bare binary.
		dir := filepath.Dir(dest)
		if out, err := exec.CommandContext(ctx, "tar", "-xzf", tmp.Name(), "-C", dir).CombinedOutput(); err != nil {
			return "", fmt.Errorf("extract cloudflared: %v: %s", err, out)
		}
		extracted := filepath.Join(dir, "cloudflared")
		if extracted != dest {
			if err := os.Rename(extracted, dest); err != nil {
				return "", err
			}
		}
	} else {
		if err := os.Rename(tmp.Name(), dest); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		return "", err
	}
	return dest, nil
}

// cloudflaredAsset names the release artifact for this platform and whether
// it is a tarball rather than a bare binary.
func cloudflaredAsset() (asset string, packaged bool) {
	arch := runtime.GOARCH // release names use amd64/arm64 directly
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf("cloudflared-darwin-%s.tgz", arch), true
	case "windows":
		return fmt.Sprintf("cloudflared-windows-%s.exe", arch), false
	default:
		return fmt.Sprintf("cloudflared-linux-%s", arch), false
	}
}
