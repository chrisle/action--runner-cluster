// Package config loads and validates the orchestrator's YAML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider kinds.
const (
	ProviderDocker  = "docker"
	ProviderProcess = "process"
)

// Config is the whole orchestrator configuration.
type Config struct {
	GitHub GitHub  `yaml:"github"`
	Server Server  `yaml:"server,omitempty"`
	Pools  []*Pool `yaml:"pools"`
	Log    Log     `yaml:"log,omitempty"`

	// Path the config was loaded from. Set by Load, not by YAML.
	Path string `yaml:"-"`
}

// GitHub holds API connection and polling settings.
type GitHub struct {
	// Org is the GitHub organization runners register against.
	Org string `yaml:"org"`
	// Token is a classic PAT with admin:org. Mutually exclusive with App.
	Token string `yaml:"token,omitempty"`
	// App is GitHub App credentials. Preferred over Token for org-wide use.
	App *AppAuth `yaml:"app,omitempty"`
	// APIURL overrides the API base for GitHub Enterprise Server.
	APIURL string `yaml:"api_url,omitempty"`
	// WebURL overrides the web base (the runner registration URL) for GHES.
	WebURL string `yaml:"web_url,omitempty"`
	// RunnerGroup is the org runner group new runners join. Empty means Default.
	RunnerGroup string `yaml:"runner_group,omitempty"`
	// PollInterval is how often the orchestrator scans for queued jobs.
	PollInterval Duration `yaml:"poll_interval,omitempty"`
	// Repos controls which org repos get polled for queued jobs.
	Repos RepoFilter `yaml:"repos,omitempty"`
}

// AppAuth is GitHub App installation authentication.
type AppAuth struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKey     string `yaml:"private_key,omitempty"`      // PEM contents
	PrivateKeyPath string `yaml:"private_key_path,omitempty"` // or a path to the PEM
}

// RepoFilter narrows the set of org repos that get polled. Polling every repo
// in a large org burns rate limit, so callers can scope it down.
type RepoFilter struct {
	// Include, if non-empty, is an allowlist of repo names (glob supported).
	Include []string `yaml:"include,omitempty"`
	// Exclude drops repo names from the set (glob supported).
	Exclude []string `yaml:"exclude,omitempty"`
	// ActiveWithin only polls repos pushed to within this window. Zero disables
	// the filter and polls every repo.
	ActiveWithin Duration `yaml:"active_within,omitempty"`
	// RefreshInterval is how often the repo list itself is re-fetched.
	RefreshInterval Duration `yaml:"refresh_interval,omitempty"`
	// Archived includes archived repos when true.
	Archived bool `yaml:"archived,omitempty"`
}

// Server configures the local control API.
type Server struct {
	// Addr is the listen address for the control API. Bind to loopback unless
	// you put an authenticating proxy in front.
	Addr string `yaml:"addr,omitempty"`
	// StateDir holds runtime state (scale overrides, provider bookkeeping).
	StateDir string `yaml:"state_dir,omitempty"`
	// Token, if set, is required as a Bearer token on control API requests.
	Token string `yaml:"token,omitempty"`
}

// Log configures output.
type Log struct {
	Level  string `yaml:"level,omitempty"`  // debug|info|warn|error
	Format string `yaml:"format,omitempty"` // text|json
}

// Pool is one homogeneous group of runners sharing labels and a provider.
type Pool struct {
	// Name identifies the pool in the CLI and in runner names. [a-z0-9-]
	Name string `yaml:"name"`
	// Labels are the labels runners in this pool register with. A job is
	// eligible for this pool when every label it requests is in this set.
	Labels []string `yaml:"labels"`
	// Provider is "docker" or "process".
	Provider string `yaml:"provider"`
	// Min is the warm floor: this many runners stay registered while idle.
	Min int `yaml:"min"`
	// Max caps concurrent runners in this pool.
	Max int `yaml:"max"`
	// IdleTimeout is how long a surplus idle runner lingers before being culled.
	IdleTimeout Duration `yaml:"idle_timeout,omitempty"`
	// JobTimeout force-destroys an instance that has lived this long, as a
	// guard against jobs that hang without GitHub cancelling them.
	JobTimeout Duration `yaml:"job_timeout,omitempty"`
	// RunnerGroup overrides GitHub.RunnerGroup for this pool.
	RunnerGroup string `yaml:"runner_group,omitempty"`

	Docker  *DockerSpec  `yaml:"docker,omitempty"`
	Process *ProcessSpec `yaml:"process,omitempty"`
}

// DockerSpec configures the docker provider for a pool.
type DockerSpec struct {
	// Host is the Docker Engine endpoint: unix:///var/run/docker.sock,
	// tcp://host:2375, or npipe:////./pipe/docker_engine on Windows.
	Host string `yaml:"host,omitempty"`
	// Image is the runner image. It must contain an actions-runner install and
	// honour the ARC_JITCONFIG environment variable.
	Image string `yaml:"image"`
	// Pull controls image pulling: always|missing|never.
	Pull string `yaml:"pull,omitempty"`
	// Platform pins a platform for multi-arch images, e.g. linux/amd64.
	Platform string `yaml:"platform,omitempty"`
	// Privileged runs the container privileged. Needed for docker-in-docker.
	Privileged bool `yaml:"privileged,omitempty"`
	// Network is the container network mode.
	Network string `yaml:"network,omitempty"`
	// Isolation is the Windows container isolation mode: process|hyperv.
	Isolation string `yaml:"isolation,omitempty"`
	// Volumes are extra bind mounts in "src:dst[:ro]" form. Use sparingly:
	// anything bound in here survives the container and can accumulate.
	Volumes []string `yaml:"volumes,omitempty"`
	// Env is extra environment for the runner container.
	Env map[string]string `yaml:"env,omitempty"`
	// WorkTmpfs mounts the runner work directory as tmpfs (Linux only), so the
	// job workspace never touches disk. Ignored on Windows containers.
	WorkTmpfs bool `yaml:"work_tmpfs,omitempty"`
	// WorkTmpfsSize is the tmpfs size, e.g. "8g". Empty means the daemon default.
	WorkTmpfsSize string `yaml:"work_tmpfs_size,omitempty"`
	// WorkDir is the in-container path of the runner work directory.
	WorkDir string `yaml:"work_dir,omitempty"`
	// CPUs limits CPU, e.g. 2 or 1.5. Zero means unlimited.
	CPUs float64 `yaml:"cpus,omitempty"`
	// Memory limits memory, e.g. "4g". Empty means unlimited.
	Memory string `yaml:"memory,omitempty"`
	// ShmSize sets /dev/shm, e.g. "2g". Useful for browser test suites.
	ShmSize string `yaml:"shm_size,omitempty"`
	// DockerInDocker binds the host docker socket into the container. This
	// makes container-based job steps work but gives jobs host-level docker
	// access, so keep it off unless you trust every workflow in the org.
	DockerInDocker bool `yaml:"docker_in_docker,omitempty"`
	// TLS configures mutual TLS for a tcp:// daemon.
	TLS *DockerTLS `yaml:"tls,omitempty"`
	// RegistryAuth supplies credentials for pulling a private runner image.
	// The daemon does its own pulling, so `docker login` on the host does not
	// help here: the credentials have to travel with the API request.
	RegistryAuth *RegistryAuth `yaml:"registry_auth,omitempty"`
}

// DockerTLS configures client certificates for a remote daemon.
type DockerTLS struct {
	CAFile     string `yaml:"ca_file,omitempty"`
	CertFile   string `yaml:"cert_file,omitempty"`
	KeyFile    string `yaml:"key_file,omitempty"`
	SkipVerify bool   `yaml:"skip_verify,omitempty"`
}

// RegistryAuth holds image registry credentials.
type RegistryAuth struct {
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	Server   string `yaml:"server,omitempty"`
}

// ProcessSpec configures the process provider for a pool. This is how macOS
// runners work, since macOS cannot be containerized.
type ProcessSpec struct {
	// TemplateDir is a pristine actions-runner installation. It is never
	// modified; each instance gets a copy-on-write clone of it.
	TemplateDir string `yaml:"template_dir,omitempty"`
	// InstancesDir is where per-runner clones live. Each is deleted in full
	// when its runner exits, which is what keeps caches from accumulating.
	InstancesDir string `yaml:"instances_dir,omitempty"`
	// Env is extra environment for the runner process.
	Env map[string]string `yaml:"env,omitempty"`
	// User, if set, runs the runner as this user (requires the orchestrator to
	// run as root). Empty runs as the orchestrator's own user.
	User string `yaml:"user,omitempty"`
	// PreflightScript, if set, runs before the runner starts. Non-zero exit
	// aborts the instance.
	PreflightScript string `yaml:"preflight_script,omitempty"`
}

// Defaults applied when the config leaves a field empty.
const (
	DefaultPollInterval     = 15 * time.Second
	DefaultIdleTimeout      = 5 * time.Minute
	DefaultJobTimeout       = 6 * time.Hour
	DefaultRepoRefresh      = 10 * time.Minute
	DefaultAddr             = "127.0.0.1:8730"
	DefaultDockerWorkDir    = "/home/runner/_work"
	DefaultWinDockerWorkDir = `C:\actions-runner\_work`
)

var poolNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

// Load reads, expands, parses and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	cfg.Path = abs
	return cfg, nil
}

// Parse expands environment references, unmarshals and validates.
func Parse(raw []byte) (*Config, error) {
	expanded, err := expandEnv(string(raw))
	if err != nil {
		return nil, err
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var envRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default} in the raw config text.
//
// A ${VAR} with no default and no value set is an error rather than an empty
// string, because silently blanking a token produces a confusing 401 much later
// and far from the cause. References inside YAML comments are exempt: the
// example config documents optional settings in commented-out lines, and those
// must not make a config unloadable.
func expandEnv(s string) (string, error) {
	var missing []string
	var out strings.Builder
	out.Grow(len(s))

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		code, comment := splitYAMLComment(line)

		out.WriteString(envRE.ReplaceAllStringFunc(code, func(m string) string {
			g := envRE.FindStringSubmatch(m)
			name, hasDefault, def := g[1], g[2] != "", g[3]
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
			if hasDefault {
				return def
			}
			missing = append(missing, name)
			return ""
		}))
		out.WriteString(comment)
	}

	if len(missing) > 0 {
		return "", fmt.Errorf("undefined environment variables referenced in config: %s "+
			"(use ${%s:-default} to make one optional)", strings.Join(dedupe(missing), ", "), missing[0])
	}
	return out.String(), nil
}

// splitYAMLComment divides a line into its code and its trailing comment.
//
// In YAML a '#' begins a comment only at the start of a line or after
// whitespace, and never inside a quoted scalar — so "pass#word" and
// "a # b" are values, not comments.
func splitYAMLComment(line string) (code, comment string) {
	var inSingle, inDouble bool
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			// A backslash-escaped quote does not close the scalar.
			if i > 0 && line[i-1] == '\\' {
				continue
			}
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return line[:i], line[i:]
			}
		}
	}
	return line, ""
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) applyDefaults() {
	if c.GitHub.PollInterval == 0 {
		c.GitHub.PollInterval = Duration(DefaultPollInterval)
	}
	if c.GitHub.APIURL == "" {
		c.GitHub.APIURL = "https://api.github.com"
	}
	if c.GitHub.WebURL == "" {
		c.GitHub.WebURL = "https://github.com"
	}
	if c.GitHub.Repos.RefreshInterval == 0 {
		c.GitHub.Repos.RefreshInterval = Duration(DefaultRepoRefresh)
	}
	if c.Server.Addr == "" {
		c.Server.Addr = DefaultAddr
	}
	if c.Server.StateDir == "" {
		c.Server.StateDir = defaultStateDir()
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}

	for _, p := range c.Pools {
		if p == nil {
			continue
		}
		if p.Provider == "" {
			// On macOS the only way to get a genuine macOS runner is to run the
			// runner binary directly: Docker Desktop runs Linux containers in a
			// VM, not macOS ones. Default to the process provider so a Mac host
			// works without explicit configuration.
			if runtime.GOOS == "darwin" {
				p.Provider = ProviderProcess
			} else {
				p.Provider = ProviderDocker
			}
		}
		if p.IdleTimeout == 0 {
			p.IdleTimeout = Duration(DefaultIdleTimeout)
		}
		if p.JobTimeout == 0 {
			p.JobTimeout = Duration(DefaultJobTimeout)
		}
		if p.RunnerGroup == "" {
			p.RunnerGroup = c.GitHub.RunnerGroup
		}
		if p.Docker != nil {
			d := p.Docker
			if d.Pull == "" {
				d.Pull = "missing"
			}
			if d.WorkDir == "" {
				if isWindowsPool(p) {
					d.WorkDir = DefaultWinDockerWorkDir
				} else {
					d.WorkDir = DefaultDockerWorkDir
				}
			}
			if d.Host == "" {
				d.Host = defaultDockerHost()
			}
		}
		if p.Provider == ProviderProcess {
			if p.Process == nil {
				p.Process = &ProcessSpec{}
			}
			if p.Process.TemplateDir == "" {
				if home, err := os.UserHomeDir(); err == nil {
					p.Process.TemplateDir = filepath.Join(home, ".arc", "runner-template")
				}
			}
			if p.Process.InstancesDir == "" {
				p.Process.InstancesDir = filepath.Join(c.Server.StateDir, "instances", p.Name)
			}
		}
	}
}

// isWindowsPool reports whether the pool's labels declare a Windows runner.
func isWindowsPool(p *Pool) bool {
	for _, l := range p.Labels {
		if strings.EqualFold(l, "windows") {
			return true
		}
	}
	return false
}

// Validate checks the config for problems that would only surface as confusing
// runtime failures.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if c.GitHub.Org == "" {
		add("github.org is required")
	}
	switch {
	case c.GitHub.Token == "" && c.GitHub.App == nil:
		add("one of github.token or github.app is required")
	case c.GitHub.Token != "" && c.GitHub.App != nil:
		add("github.token and github.app are mutually exclusive")
	}
	if a := c.GitHub.App; a != nil {
		if a.AppID == 0 {
			add("github.app.app_id is required")
		}
		if a.InstallationID == 0 {
			add("github.app.installation_id is required")
		}
		if a.PrivateKey == "" && a.PrivateKeyPath == "" {
			add("github.app needs private_key or private_key_path")
		}
	}
	if d := c.GitHub.PollInterval.Duration(); d < time.Second {
		add("github.poll_interval %s is too aggressive; use 1s or more", d)
	}

	if len(c.Pools) == 0 {
		add("at least one pool is required")
	}

	seen := map[string]bool{}
	for i, p := range c.Pools {
		if p == nil {
			add("pools[%d] is empty", i)
			continue
		}
		where := fmt.Sprintf("pool %q", p.Name)
		if p.Name == "" {
			where = fmt.Sprintf("pools[%d]", i)
			add("%s: name is required", where)
		} else if !poolNameRE.MatchString(p.Name) {
			add("%s: name must be lowercase alphanumeric with dashes, 2-40 chars", where)
		}
		if seen[p.Name] {
			add("%s: duplicate pool name", where)
		}
		seen[p.Name] = true

		if len(p.Labels) == 0 {
			add("%s: at least one label is required", where)
		}
		for _, l := range p.Labels {
			if strings.TrimSpace(l) != l || l == "" {
				add("%s: label %q has surrounding whitespace or is empty", where, l)
			}
		}
		if p.Min < 0 {
			add("%s: min must be >= 0", where)
		}
		if p.Max < 1 {
			add("%s: max must be >= 1", where)
		}
		if p.Min > p.Max {
			add("%s: min (%d) exceeds max (%d)", where, p.Min, p.Max)
		}

		switch p.Provider {
		case ProviderDocker:
			if p.Process != nil {
				add("%s: process settings given but provider is docker", where)
			}
			if p.Docker == nil {
				add("%s: provider docker requires a docker: block", where)
				break
			}
			if p.Docker.Image == "" {
				add("%s: docker.image is required", where)
			}
			switch p.Docker.Pull {
			case "always", "missing", "never":
			default:
				add("%s: docker.pull must be always, missing or never", where)
			}
			if p.Docker.Isolation != "" && !isWindowsPool(p) {
				add("%s: docker.isolation only applies to Windows pools", where)
			}
			if isMacPool(p) {
				add("%s: labels declare macOS but provider is docker. Docker cannot run "+
					"macOS containers; use provider: process for macOS pools", where)
			}
		case ProviderProcess:
			if p.Docker != nil {
				add("%s: docker settings given but provider is process", where)
			}
		default:
			add("%s: unknown provider %q (want docker or process)", where, p.Provider)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func isMacPool(p *Pool) bool {
	for _, l := range p.Labels {
		switch strings.ToLower(l) {
		case "macos", "macos-latest", "darwin", "osx":
			return true
		}
	}
	return false
}

// Pool returns the named pool, or nil.
func (c *Config) Pool(name string) *Pool {
	for _, p := range c.Pools {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// EffectiveRunnerGroup returns the runner group for a pool.
func (c *Config) EffectiveRunnerGroup(p *Pool) string {
	if p.RunnerGroup != "" {
		return p.RunnerGroup
	}
	return c.GitHub.RunnerGroup
}

func defaultStateDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "action-runner-cluster")
	}
	return ".arc-state"
}

func defaultDockerHost() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	if runtime.GOOS == "windows" {
		return `npipe:////./pipe/docker_engine`
	}
	return "unix:///var/run/docker.sock"
}
