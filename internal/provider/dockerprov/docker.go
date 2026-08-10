// Package dockerprov runs GitHub Actions runners as Docker containers.
//
// Containers are the natural fit for disposable runners: the whole filesystem
// is thrown away with the container, so no cache, checkout or toolchain
// download can leak into the next job. The provider leans on that by removing
// containers with volumes attached (v=true) and, optionally, by mounting the
// runner work directory as tmpfs so the workspace never reaches disk at all.
//
// Linux containers work from any host. Windows containers require the Docker
// daemon to be running on a Windows host in Windows-container mode. macOS
// cannot be containerized at all — use the process provider for macOS pools.
package dockerprov

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/provider"
)

// Provider implements provider.Provider using the Docker Engine API.
type Provider struct {
	pool    *config.Pool
	spec    *config.DockerSpec
	engine  *engine
	log     *slog.Logger
	windows bool

	pullOnce sync.Once
	pullErr  error
}

// New builds a Docker provider for a pool.
func New(pool *config.Pool, log *slog.Logger) (*Provider, error) {
	spec := pool.Docker
	if spec == nil {
		return nil, fmt.Errorf("pool %s: missing docker configuration", pool.Name)
	}

	var tlsOpts *TLSOpts
	if spec.TLS != nil {
		tlsOpts = &TLSOpts{
			CAFile:     spec.TLS.CAFile,
			CertFile:   spec.TLS.CertFile,
			KeyFile:    spec.TLS.KeyFile,
			SkipVerify: spec.TLS.SkipVerify,
		}
	}

	eng, err := newEngine(spec.Host, tlsOpts)
	if err != nil {
		return nil, fmt.Errorf("pool %s: %w", pool.Name, err)
	}
	if a := spec.RegistryAuth; a != nil {
		eng.setRegistryAuth(a.Username, a.Password, a.Server)
	}

	windows := false
	for _, l := range pool.Labels {
		if strings.EqualFold(l, "windows") {
			windows = true
		}
	}

	return &Provider{
		pool:    pool,
		spec:    spec,
		engine:  eng,
		log:     log.With("pool", pool.Name, "provider", "docker"),
		windows: windows,
	}, nil
}

func (p *Provider) Kind() string { return config.ProviderDocker }

// Preflight checks the daemon is reachable, that it is running the right kind
// of containers for this pool, and that the image is available.
func (p *Provider) Preflight(ctx context.Context) error {
	v, err := p.engine.version(ctx)
	if err != nil {
		return fmt.Errorf("cannot reach docker at %s: %w", p.spec.Host, err)
	}
	p.log.Debug("docker daemon", "version", v.Version, "os", v.Os, "arch", v.Arch)

	// This is the mistake that costs the most time to debug: a Windows pool
	// pointed at Docker Desktop on macOS or Linux silently gets Linux
	// containers, and the runner registers with the wrong OS.
	if p.windows && v.Os != "windows" {
		return fmt.Errorf("pool %s declares the windows label but docker at %s runs %s containers. "+
			"Point this pool at a Windows host with Docker Desktop in Windows-container mode",
			p.pool.Name, p.spec.Host, v.Os)
	}
	if !p.windows && v.Os == "windows" {
		return fmt.Errorf("pool %s is a %s pool but docker at %s runs windows containers",
			p.pool.Name, v.Os, p.spec.Host)
	}

	return p.ensureImage(ctx)
}

// ensureImage pulls the runner image according to the pull policy. It runs once
// per process for "missing", so scale-ups don't re-check on every create.
func (p *Provider) ensureImage(ctx context.Context) error {
	switch p.spec.Pull {
	case "never":
		exists, err := p.engine.imageExists(ctx, p.spec.Image)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("image %s is not present and pull is set to never", p.spec.Image)
		}
		return nil

	case "always":
		p.log.Info("pulling runner image", "image", p.spec.Image)
		return p.engine.pullImage(ctx, p.spec.Image, p.spec.Platform)

	default: // "missing"
		p.pullOnce.Do(func() {
			exists, err := p.engine.imageExists(ctx, p.spec.Image)
			if err != nil {
				p.pullErr = err
				return
			}
			if exists {
				return
			}
			p.log.Info("pulling runner image", "image", p.spec.Image)
			p.pullErr = p.engine.pullImage(ctx, p.spec.Image, p.spec.Platform)
		})
		return p.pullErr
	}
}

// Create starts a runner container.
func (p *Provider) Create(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	if err := p.ensureImage(ctx); err != nil {
		return nil, err
	}

	now := time.Now()
	req, err := p.buildCreateRequest(spec, now)
	if err != nil {
		return nil, err
	}

	created, err := p.engine.createContainer(ctx, spec.RunnerName, *req, p.spec.Platform)
	if err != nil {
		return nil, fmt.Errorf("create container for %s: %w", spec.RunnerName, err)
	}
	for _, w := range created.Warnings {
		p.log.Warn("docker create warning", "runner", spec.RunnerName, "warning", w)
	}

	if err := p.engine.startContainer(ctx, created.ID); err != nil {
		// The container exists but will never run. Remove it rather than
		// leaving a dead container that Prune has to clean up later.
		if rmErr := p.engine.removeContainer(context.WithoutCancel(ctx), created.ID); rmErr != nil {
			p.log.Warn("failed to remove container that would not start",
				"runner", spec.RunnerName, "error", rmErr)
		}
		return nil, fmt.Errorf("start container for %s: %w", spec.RunnerName, err)
	}

	return &provider.Instance{
		ID:         created.ID,
		Pool:       p.pool.Name,
		RunnerName: spec.RunnerName,
		RunnerID:   spec.RunnerID,
		State:      provider.StateStarting,
		CreatedAt:  now,
	}, nil
}

func (p *Provider) buildCreateRequest(spec provider.Spec, now time.Time) (*createRequest, error) {
	env := []string{
		// The runner image entrypoint reads this and execs run.sh --jitconfig.
		"ARC_JITCONFIG=" + spec.JITConfig,
		"ARC_POOL=" + p.pool.Name,
		"ARC_RUNNER_NAME=" + spec.RunnerName,
		// Suppress the runner's own update check: a JIT runner lives for one
		// job, and a self-update mid-startup just wastes time and can fail.
		"RUNNER_ALLOW_RUNASROOT=1",
		"DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=0",
	}
	for k, v := range p.spec.Env {
		env = append(env, k+"="+v)
	}

	hc := hostConfig{
		AutoRemove:  false, // reaped explicitly so exits stay observable
		Privileged:  p.spec.Privileged,
		NetworkMode: p.spec.Network,
		Binds:       append([]string(nil), p.spec.Volumes...),
		Isolation:   p.spec.Isolation,
	}
	hc.RestartPolicy.Name = "no"
	hc.LogConfig.Type = "json-file"
	hc.LogConfig.Config = map[string]string{"max-size": "10m", "max-file": "2"}

	if p.spec.DockerInDocker {
		if p.windows {
			return nil, fmt.Errorf("docker_in_docker is not supported for Windows containers")
		}
		hc.Binds = append(hc.Binds, "/var/run/docker.sock:/var/run/docker.sock")
	}

	// tmpfs is a Linux container feature. On Windows the workspace lives in the
	// container's own writable layer, which is still discarded on removal.
	if p.spec.WorkTmpfs && !p.windows {
		opts := "rw,exec,mode=1777"
		if p.spec.WorkTmpfsSize != "" {
			opts += ",size=" + p.spec.WorkTmpfsSize
		}
		hc.Tmpfs = map[string]string{p.spec.WorkDir: opts}
	}

	if p.spec.ShmSize != "" {
		n, err := parseBytes(p.spec.ShmSize)
		if err != nil {
			return nil, fmt.Errorf("shm_size: %w", err)
		}
		hc.ShmSize = n
	}
	if p.spec.Memory != "" {
		n, err := parseBytes(p.spec.Memory)
		if err != nil {
			return nil, fmt.Errorf("memory: %w", err)
		}
		hc.Memory = n
	}
	if p.spec.CPUs > 0 {
		hc.NanoCPUs = int64(p.spec.CPUs * 1e9)
	}

	return &createRequest{
		Image:      p.spec.Image,
		Env:        env,
		Hostname:   truncateHostname(spec.RunnerName),
		StopSignal: "SIGTERM",
		Labels: map[string]string{
			provider.LabelManaged:    "true",
			provider.LabelPool:       p.pool.Name,
			provider.LabelRunnerName: spec.RunnerName,
			provider.LabelRunnerID:   strconv.FormatInt(spec.RunnerID, 10),
			provider.LabelCreated:    now.UTC().Format(time.RFC3339),
		},
		HostConfig: hc,
	}, nil
}

// List returns every container this pool owns, running or exited.
func (p *Provider) List(ctx context.Context) ([]provider.Instance, error) {
	containers, err := p.engine.listContainers(ctx, []string{
		provider.LabelManaged + "=true",
		provider.LabelPool + "=" + p.pool.Name,
	})
	if err != nil {
		return nil, err
	}

	out := make([]provider.Instance, 0, len(containers))
	for _, c := range containers {
		inst := provider.Instance{
			ID:         c.ID,
			Pool:       p.pool.Name,
			RunnerName: c.Labels[provider.LabelRunnerName],
			Detail:     c.Status,
			State:      dockerState(c.State),
		}
		if id, err := strconv.ParseInt(c.Labels[provider.LabelRunnerID], 10, 64); err == nil {
			inst.RunnerID = id
		}
		if ts, err := time.Parse(time.RFC3339, c.Labels[provider.LabelCreated]); err == nil {
			inst.CreatedAt = ts
		}
		if inst.RunnerName == "" && len(c.Names) > 0 {
			inst.RunnerName = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, inst)
	}
	return out, nil
}

// dockerState maps Docker's container states onto provider states.
func dockerState(s string) provider.State {
	switch s {
	case "created":
		return provider.StateStarting
	case "running", "restarting", "paused", "removing":
		return provider.StateRunning
	case "exited", "dead":
		return provider.StateExited
	default:
		return provider.StateUnknown
	}
}

// Destroy force-removes the container along with its anonymous volumes.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	if err := p.engine.removeContainer(ctx, id); err != nil {
		return fmt.Errorf("remove container %s: %w", short(id), err)
	}
	return nil
}

// Logs returns the tail of the container's output.
func (p *Provider) Logs(ctx context.Context, id string, lines int) (string, error) {
	return p.engine.logs(ctx, id, lines)
}

// Prune removes exited containers belonging to this pool. A crashed
// orchestrator can leave these behind, and each one holds a container layer.
func (p *Provider) Prune(ctx context.Context) (int, error) {
	instances, err := p.List(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, inst := range instances {
		if inst.State != provider.StateExited {
			continue
		}
		if err := p.Destroy(ctx, inst.ID); err != nil {
			p.log.Warn("prune failed", "container", short(inst.ID), "error", err)
			continue
		}
		removed++
	}
	return removed, nil
}

func (p *Provider) Close() error { return nil }

// ExitCode reports how a container finished, used to explain a runner that
// died before ever picking up a job.
func (p *Provider) ExitCode(ctx context.Context, id string) (int, string, error) {
	info, err := p.engine.inspect(ctx, id)
	if err != nil {
		return 0, "", err
	}
	detail := info.State.Status
	if info.State.OOMKilled {
		detail = "oom-killed"
	} else if info.State.Error != "" {
		detail = info.State.Error
	}
	return info.State.ExitCode, detail, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// truncateHostname keeps the container hostname inside the 63-byte DNS label
// limit; runner names can exceed it once the pool name and suffix are joined.
func truncateHostname(name string) string {
	if len(name) <= 63 {
		return name
	}
	return name[:63]
}
