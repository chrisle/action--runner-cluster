// Package processprov runs GitHub Actions runners as ephemeral local
// processes, each in its own throwaway directory.
//
// This exists because macOS cannot be containerized. Docker Desktop on a Mac
// runs a Linux VM, so a "macOS runner in Docker" is really a Linux runner and
// cannot build or sign Mac software. The only way to get a genuine macOS runner
// is to run the runner binary on macOS directly.
//
// The disposal guarantee that containers get for free is reconstructed here:
//
//   - A pristine runner installation (TemplateDir) is never modified.
//   - Each instance gets its own copy of it, made with a copy-on-write clone
//     (APFS clonefile on macOS, reflink on Linux) so it costs almost no time
//     or disk despite the runner being a few hundred megabytes.
//   - When the runner exits, the entire instance directory is deleted. The
//     workspace, the checked-out repo, ~/.npm inside it, every downloaded
//     toolchain: all gone, because they only ever existed inside that clone.
//
// Anything a job writes outside the instance directory (a real ~/Library
// cache, /tmp, Homebrew) is outside this provider's control, which is the
// unavoidable difference between a process and a container.
package processprov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/provider"
)

// stateFile records enough to recover an instance after an orchestrator
// restart, since a process (unlike a container) carries no labels.
const stateFile = ".arc-instance.json"

// instanceState is what gets written to stateFile.
type instanceState struct {
	Pool       string    `json:"pool"`
	RunnerName string    `json:"runner_name"`
	RunnerID   int64     `json:"runner_id"`
	PID        int       `json:"pid"`
	CreatedAt  time.Time `json:"created_at"`
}

// Provider implements provider.Provider with local processes.
type Provider struct {
	pool *config.Pool
	spec *config.ProcessSpec
	log  *slog.Logger
}

// New builds a process provider for a pool.
func New(pool *config.Pool, log *slog.Logger) (*Provider, error) {
	if pool.Process == nil {
		return nil, fmt.Errorf("pool %s: missing process configuration", pool.Name)
	}
	return &Provider{
		pool: pool,
		spec: pool.Process,
		log:  log.With("pool", pool.Name, "provider", "process"),
	}, nil
}

func (p *Provider) Kind() string { return config.ProviderProcess }

// Preflight verifies the runner template looks like a real installation and
// that the instances directory is writable. A template that does not exist at
// all is downloaded and installed automatically first.
func (p *Provider) Preflight(ctx context.Context) error {
	tmpl := p.spec.TemplateDir
	if err := p.ensureTemplate(ctx); err != nil {
		return fmt.Errorf("template_dir %s is missing and could not be set up "+
			"automatically: %w\nDownload the runner from "+
			"https://github.com/actions/runner/releases and extract it there", tmpl, err)
	}
	info, err := os.Stat(tmpl)
	if err != nil {
		return fmt.Errorf("template_dir %s: %w (download the runner from "+
			"https://github.com/actions/runner/releases and extract it there)", tmpl, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("template_dir %s is not a directory", tmpl)
	}

	entry := p.runnerEntrypoint()
	if _, err := os.Stat(filepath.Join(tmpl, entry)); err != nil {
		return fmt.Errorf("template_dir %s does not contain %s; it is not an "+
			"actions-runner installation", tmpl, entry)
	}

	// A runner that was ever configured non-JIT leaves credentials behind.
	// Cloning those into every instance makes runners fight over one identity.
	for _, stale := range []string{".runner", ".credentials"} {
		if _, err := os.Stat(filepath.Join(tmpl, stale)); err == nil {
			return fmt.Errorf("template_dir %s contains %s from a previous `config.sh` run. "+
				"Remove it: the template must be an unconfigured runner", tmpl, stale)
		}
	}

	if err := os.MkdirAll(p.spec.InstancesDir, 0o755); err != nil {
		return fmt.Errorf("instances_dir %s: %w", p.spec.InstancesDir, err)
	}

	// Every instance is a copy of the template, so a missing copy tool breaks
	// the provider entirely. Catch it here rather than at the first scale-up,
	// where it would surface as a launch failure with no obvious cause.
	if tool := copyTool(); tool != "" {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s was not found on PATH; it is needed to clone "+
				"the runner template for each job: %w", tool, err)
		}
	}

	if p.spec.PreflightScript != "" {
		if _, err := os.Stat(p.spec.PreflightScript); err != nil {
			return fmt.Errorf("preflight_script %s: %w", p.spec.PreflightScript, err)
		}
	}

	// The runner is a .NET application, and a fresh Linux host often lacks
	// the system libraries it needs (libicu chiefly). Without this probe the
	// failure surfaces as runners that die instantly at job time with
	// nothing useful in the logs.
	if runtime.GOOS == "linux" {
		if err := p.checkRunnerDeps(ctx); err != nil {
			return err
		}
	}
	return nil
}

// checkRunnerDeps proves the runner binary can actually execute on this host.
func (p *Provider) checkRunnerDeps(ctx context.Context) error {
	listener := filepath.Join(p.spec.TemplateDir, "bin", "Runner.Listener")
	if _, err := os.Stat(listener); err != nil {
		return nil // unusual layout; the entrypoint check already governs
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, listener, "--version")
	cmd.Dir = p.spec.TemplateDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	low := strings.ToLower(msg + " " + err.Error())
	for _, marker := range []string{"icu", "shared librar", "globalization", "dotnet", "no such file"} {
		if strings.Contains(low, marker) {
			return fmt.Errorf("the runner cannot start on this host — system libraries "+
				"are missing: %v (%s)\nInstall them with: sudo %s/bin/installdependencies.sh",
				err, msg, p.spec.TemplateDir)
		}
	}
	// Some other failure: report it at warn level rather than blocking a
	// host that might still work.
	p.log.Warn("runner version probe failed; jobs may not start", "error", err, "output", msg)
	return nil
}

// copyTool names the executable cloneTree relies on for this platform.
func copyTool() string {
	if runtime.GOOS == "windows" {
		return "robocopy"
	}
	return "cp"
}

// runnerEntrypoint is the script that starts a runner on this platform.
func (p *Provider) runnerEntrypoint() string {
	if runtime.GOOS == "windows" {
		return "run.cmd"
	}
	return "run.sh"
}

// Create clones the runner template and starts a runner in it.
func (p *Provider) Create(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	dir := filepath.Join(p.spec.InstancesDir, spec.RunnerName)

	// A leftover directory from a crashed instance would poison the clone.
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clear stale instance dir %s: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}

	if err := cloneTree(ctx, p.spec.TemplateDir, dir); err != nil {
		return nil, fmt.Errorf("clone runner template: %w", err)
	}

	// buildEnv points TMPDIR at <dir>/_temp, and tools trust TMPDIR to exist —
	// `go env` hard-fails with "creating work dir: stat …/_temp: no such file
	// or directory" rather than creating it. The template usually has no
	// _temp (nothing writes there at template-build time), so create it here
	// instead of depending on the template's contents.
	if err := os.MkdirAll(filepath.Join(dir, "_temp"), 0o755); err != nil {
		return nil, fmt.Errorf("create instance temp dir: %w", err)
	}

	// Undo on any failure past this point, so a half-built instance never
	// lingers on disk.
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dir)
		}
	}()

	if p.spec.PreflightScript != "" {
		cmd := exec.CommandContext(ctx, p.spec.PreflightScript)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "ARC_INSTANCE_DIR="+dir, "ARC_RUNNER_NAME="+spec.RunnerName)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("preflight script failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	logPath := filepath.Join(dir, "runner.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create runner log: %w", err)
	}
	defer logFile.Close()

	entry := filepath.Join(dir, p.runnerEntrypoint())
	cmd := exec.Command(entry, "--jitconfig", spec.JITConfig)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = p.buildEnv(dir, spec)

	// Put the runner in its own process group. Job steps spawn children, and a
	// group is what lets Destroy kill the whole tree rather than orphaning
	// compilers and simulators that keep running after the runner is gone.
	configureProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start runner %s: %w", spec.RunnerName, err)
	}

	now := time.Now()
	st := instanceState{
		Pool:       p.pool.Name,
		RunnerName: spec.RunnerName,
		RunnerID:   spec.RunnerID,
		PID:        cmd.Process.Pid,
		CreatedAt:  now,
	}
	if err := writeState(dir, st); err != nil {
		_ = killGroup(cmd.Process.Pid)
		return nil, err
	}

	// Reap the child when it exits, so it does not become a zombie. The exit
	// itself is observed through List, which checks liveness by pid.
	go func() {
		_ = cmd.Wait()
	}()

	success = true
	p.log.Debug("started runner process", "runner", spec.RunnerName, "pid", cmd.Process.Pid, "dir", dir)

	return &provider.Instance{
		ID:         spec.RunnerName,
		Pool:       p.pool.Name,
		RunnerName: spec.RunnerName,
		RunnerID:   spec.RunnerID,
		State:      provider.StateStarting,
		CreatedAt:  now,
	}, nil
}

func (p *Provider) buildEnv(dir string, spec provider.Spec) []string {
	env := os.Environ()
	env = append(env,
		"ARC_POOL="+p.pool.Name,
		"ARC_RUNNER_NAME="+spec.RunnerName,
		"ARC_INSTANCE_DIR="+dir,
		// Keep the runner's temp scratch inside the instance directory so it
		// is deleted along with everything else.
		"RUNNER_TEMP="+filepath.Join(dir, "_work", "_temp"),
		"TMPDIR="+filepath.Join(dir, "_temp"),
	)
	for k, v := range p.spec.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// List reports every instance directory this pool owns and whether its process
// is still alive. State is read from disk, not memory, so the orchestrator
// recovers its view after a restart.
func (p *Provider) List(ctx context.Context) ([]provider.Instance, error) {
	entries, err := os.ReadDir(p.spec.InstancesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var out []provider.Instance
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(p.spec.InstancesDir, e.Name())
		st, err := readState(dir)
		if err != nil {
			// A directory with no readable state is debris from a crash
			// mid-create. Prune will remove it.
			p.log.Debug("instance directory has no state", "dir", dir, "error", err)
			out = append(out, provider.Instance{
				ID:         e.Name(),
				Pool:       p.pool.Name,
				RunnerName: e.Name(),
				State:      provider.StateUnknown,
				Detail:     "no instance state",
			})
			continue
		}

		inst := provider.Instance{
			ID:         e.Name(),
			Pool:       p.pool.Name,
			RunnerName: st.RunnerName,
			RunnerID:   st.RunnerID,
			CreatedAt:  st.CreatedAt,
		}
		switch {
		case !processAlive(st.PID):
			inst.State = provider.StateExited
			inst.Detail = "process exited"
		case !processMatches(st.PID, dir):
			// The pid is alive but belongs to something else: it was recycled
			// after the runner died and the orchestrator was not running to
			// notice. Treat the instance as finished and never signal that pid.
			inst.State = provider.StateExited
			inst.Detail = "pid recycled by another process"
		default:
			inst.State = provider.StateRunning
			inst.Detail = fmt.Sprintf("pid %d", st.PID)
		}
		out = append(out, inst)
	}
	return out, nil
}

// Destroy kills the runner's whole process group and deletes its directory.
// Deleting the directory is what reclaims the disk: everything the job wrote
// lived inside it.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	dir := filepath.Join(p.spec.InstancesDir, filepath.Base(id))

	if st, err := readState(dir); err == nil && processAlive(st.PID) && processMatches(st.PID, dir) {
		// SIGTERM first: the runner unregisters itself from GitHub and finishes
		// writing job logs. Only escalate if it ignores that.
		if err := signalGroup(st.PID, syscall.SIGTERM); err != nil {
			p.log.Debug("SIGTERM to runner group failed", "pid", st.PID, "error", err)
		}
		if !waitForExit(st.PID, 20*time.Second) {
			p.log.Warn("runner did not exit after SIGTERM, killing", "runner", id, "pid", st.PID)
			_ = killGroup(st.PID)
			waitForExit(st.PID, 5*time.Second)
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove instance dir %s: %w", dir, err)
	}
	return nil
}

// Logs returns the tail of the runner's log file.
func (p *Provider) Logs(ctx context.Context, id string, lines int) (string, error) {
	if lines <= 0 {
		lines = 100
	}
	path := filepath.Join(p.spec.InstancesDir, filepath.Base(id), "runner.log")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	split := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(split) > lines {
		split = split[len(split)-lines:]
	}
	return strings.Join(split, "\n"), nil
}

// Prune removes instance directories whose process is gone.
func (p *Provider) Prune(ctx context.Context) (int, error) {
	instances, err := p.List(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, inst := range instances {
		if inst.State == provider.StateRunning || inst.State == provider.StateStarting {
			continue
		}
		if err := p.Destroy(ctx, inst.ID); err != nil {
			p.log.Warn("prune failed", "instance", inst.ID, "error", err)
			continue
		}
		removed++
	}
	return removed, nil
}

func (p *Provider) Close() error { return nil }

func writeState(dir string, st instanceState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write instance state: %w", err)
	}
	// Rename so a reader never sees a partially written file.
	return os.Rename(tmp, filepath.Join(dir, stateFile))
}

func readState(dir string) (instanceState, error) {
	var st instanceState
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	return st, nil
}

func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !processAlive(pid)
}

// copyAttempt is one way to duplicate the template tree, along with how to read
// the exit code of the tool that does it.
type copyAttempt struct {
	args []string
	// succeeded reports whether a non-zero exit code still means success. Most
	// tools use zero for success and need nothing here; robocopy does not.
	succeeded func(code int) bool
}

// cloneTree copies src to dst, preferring a copy-on-write clone. On APFS and
// on Linux filesystems with reflink support this is near-instant and consumes
// almost no additional disk, which is what makes a fresh several-hundred-
// megabyte runner installation per job practical.
//
// Windows has no equivalent on NTFS, so the copy there is a real byte-for-byte
// duplication of the whole runner installation. It works, but it costs seconds
// and real disk churn on every job. Prefer the docker provider on any Windows
// edition that can run containers; this path exists for the ones that cannot,
// Windows Home chiefly. See scripts/setup-windows-template.ps1.
func cloneTree(ctx context.Context, src, dst string) error {
	var attempts []copyAttempt
	switch runtime.GOOS {
	case "darwin":
		// -c requests an APFS clone; it falls back to a real copy off APFS.
		attempts = append(attempts,
			copyAttempt{args: []string{"cp", "-Rc", src + "/", dst}},
			copyAttempt{args: []string{"cp", "-R", src + "/", dst}})
	case "linux":
		attempts = append(attempts,
			copyAttempt{args: []string{"cp", "-a", "--reflink=auto", src + "/.", dst}})
	case "windows":
		// robocopy is the only copy tool guaranteed to be present: Windows has
		// no cp, and xcopy chokes on paths past 260 characters, which a runner's
		// nested _work tree reaches easily.
		//
		// /R and /W matter: robocopy defaults to retrying a locked file a
		// million times, thirty seconds apart, which would hang a scale-up
		// indefinitely rather than failing and letting the orchestrator retry.
		attempts = append(attempts, copyAttempt{
			args: []string{"robocopy", src, dst,
				"/E", "/NFL", "/NDL", "/NJH", "/NJS", "/NP", "/R:1", "/W:1"},
			succeeded: robocopyOK,
		})
	default:
		attempts = append(attempts,
			copyAttempt{args: []string{"cp", "-R", src + "/.", dst}})
	}

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	var lastErr error
	for _, a := range attempts {
		cmd := exec.CommandContext(ctx, a.args[0], a.args[1:]...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}

		var ee *exec.ExitError
		if a.succeeded != nil && errors.As(err, &ee) && a.succeeded(ee.ExitCode()) {
			return nil
		}

		lastErr = fmt.Errorf("%s: %w: %s", strings.Join(a.args, " "), err, strings.TrimSpace(string(out)))
	}
	return lastErr
}

// robocopyOK interprets robocopy's exit code.
//
// robocopy does not follow the usual convention: it reports what it did as a
// bitmask, so a perfectly successful copy exits 1 ("files were copied") and
// exit 0 means "nothing needed copying". Anything below 8 is success; 8 and
// above mean at least one file could not be copied. Treating non-zero as
// failure the ordinary way would reject every successful clone.
func robocopyOK(code int) bool { return code >= 0 && code < 8 }
