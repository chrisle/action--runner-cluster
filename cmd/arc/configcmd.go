package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
)

// cmdConfig runs the interactive configuration wizard. With no existing file it
// builds one from scratch; with an existing one it walks the same prompts
// prefilled with the current values, so Enter-Enter-Enter is a no-op edit.
func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	cfgPath := fs.String("config", "",
		"file to create or edit (default $ARC_CONFIG, then ~/.config/arc/arc.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *cfgPath
	if path == "" {
		path = os.Getenv("ARC_CONFIG")
	}
	if path == "" {
		path = config.DefaultUserPath()
	}
	if path == "" {
		return errors.New("cannot determine a config path (home directory unknown); pass -config")
	}
	return runConfigWizard(path, os.Stdin, os.Stdout)
}

func runConfigWizard(path string, in io.Reader, out io.Writer) error {
	w := &wizard{in: bufio.NewScanner(in), out: out}

	var cfg *config.Config
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		cfg, err = config.ParseEditable(raw)
		if err != nil {
			return fmt.Errorf("%s exists but does not parse, so the wizard cannot edit it "+
				"safely: %w\nFix or delete the file and rerun arc config", path, err)
		}
		fmt.Fprintf(out, "Editing %s\n", path)
	case os.IsNotExist(err):
		cfg = &config.Config{
			GitHub: config.GitHub{
				Token:        "${GITHUB_TOKEN}",
				PollInterval: config.Duration(config.DefaultPollInterval),
			},
			Server: config.Server{Addr: config.DefaultAddr},
			Log:    config.Log{Level: "info", Format: "text"},
		}
		fmt.Fprintf(out, "Creating %s\n", path)
	default:
		return fmt.Errorf("read %s: %w", path, err)
	}
	fmt.Fprintln(out, "Enter keeps the value in [brackets]; type - to clear one.")

	fmt.Fprintln(out, "\ngithub")
	cfg.GitHub.Org = w.askRequired("  organization", cfg.GitHub.Org)

	method := "token"
	if cfg.GitHub.App != nil {
		method = "app"
	}
	switch w.askChoice("  auth method", []string{"token", "app"}, method) {
	case "token":
		cfg.GitHub.App = nil
		tok := cfg.GitHub.Token
		if tok == "" {
			tok = "${GITHUB_TOKEN}"
		}
		cfg.GitHub.Token = w.askSecret(
			"  token (a PAT with admin:org, or ${GITHUB_TOKEN} to read the environment)", tok)
	case "app":
		cfg.GitHub.Token = ""
		if cfg.GitHub.App == nil {
			cfg.GitHub.App = &config.AppAuth{}
		}
		a := cfg.GitHub.App
		a.AppID = w.askInt64("  app id", a.AppID)
		a.InstallationID = w.askInt64("  installation id", a.InstallationID)
		if a.PrivateKey == "" { // an inline PEM is left alone
			a.PrivateKeyPath = w.askRequired("  private key path (PEM)", a.PrivateKeyPath)
		}
	}
	cfg.GitHub.PollInterval = w.askDuration("  poll interval",
		cfg.GitHub.PollInterval, config.DefaultPollInterval)
	cfg.GitHub.RunnerGroup = w.ask("  runner group (empty = Default)", cfg.GitHub.RunnerGroup)

	fmt.Fprintln(out, "\npools")
	kept := cfg.Pools[:0]
	for _, p := range cfg.Pools {
		if p == nil {
			continue
		}
		label := fmt.Sprintf("  pool %q (%s, labels %s, min %d max %d)",
			p.Name, p.Provider, strings.Join(p.Labels, "/"), p.Min, p.Max)
		switch w.askChoice(label, []string{"keep", "edit", "delete"}, "keep") {
		case "keep":
			kept = append(kept, p)
		case "edit":
			w.editPool(cfg, p)
			kept = append(kept, p)
		case "delete":
		}
	}
	cfg.Pools = kept

	for w.err == nil && (len(cfg.Pools) == 0 || w.askBool("  add another pool?", false)) {
		if len(cfg.Pools) == 0 {
			fmt.Fprintln(out, "  at least one pool is required — define the first:")
		}
		p := &config.Pool{}
		w.editPool(cfg, p)
		cfg.Pools = append(cfg.Pools, p)
	}

	if w.err != nil {
		return fmt.Errorf("aborted (%v); nothing was written", w.err)
	}

	rendered, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := config.CheckBytes(rendered); err != nil {
		return fmt.Errorf("the assembled config does not validate; nothing was written:\n%w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// 0600: the file may hold a PAT or registry credentials.
	if err := os.WriteFile(path, rendered, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nWrote %s\nNext: arc doctor  # verify credentials and providers\n", path)
	return nil
}

func (w *wizard) editPool(cfg *config.Config, p *config.Pool) {
	p.Name = w.askRequired("    name", p.Name)

	prov := p.Provider
	if prov == "" {
		// Docker is the norm; macOS cannot be containerized, so a Mac defaults
		// to the process provider.
		prov = config.ProviderDocker
		if runtime.GOOS == "darwin" {
			prov = config.ProviderProcess
		}
	}
	p.Provider = w.askChoice("    provider (docker = containers; process = bare processes, required for macOS)",
		[]string{config.ProviderDocker, config.ProviderProcess}, prov)

	if len(p.Labels) == 0 {
		p.Labels = defaultLabels(p.Provider)
	}
	for {
		p.Labels = w.askList("    labels", p.Labels)
		if len(p.Labels) > 0 || w.err != nil {
			break
		}
		fmt.Fprintln(w.out, "    at least one label is required")
		p.Labels = defaultLabels(p.Provider)
	}

	p.Min = w.askInt("    min warm runners", p.Min)
	if p.Max == 0 {
		p.Max = 4
	}
	p.Max = w.askInt("    max runners", p.Max)

	switch p.Provider {
	case config.ProviderDocker:
		p.Process = nil
		if p.Docker == nil {
			p.Docker = &config.DockerSpec{Pull: "missing"}
		}
		img := p.Docker.Image
		if img == "" {
			img = fmt.Sprintf("ghcr.io/%s/arc-runner:linux", orgPlaceholder(cfg))
		}
		p.Docker.Image = w.askRequired("    image", img)
	case config.ProviderProcess:
		p.Docker = nil
		if p.Process == nil {
			p.Process = &config.ProcessSpec{}
		}
		tpl := p.Process.TemplateDir
		if tpl == "" {
			if home, err := os.UserHomeDir(); err == nil {
				tpl = filepath.Join(home, ".arc", "runner-template")
			}
		}
		p.Process.TemplateDir = w.ask("    runner template dir", tpl)
	}
}

func defaultLabels(provider string) []string {
	if provider == config.ProviderProcess {
		switch runtime.GOOS {
		case "darwin":
			return []string{"self-hosted", "macos", "arm64"}
		case "windows":
			return []string{"self-hosted", "windows", "x64"}
		}
	}
	return []string{"self-hosted", "linux", "x64"}
}

// orgPlaceholder is the org for use in suggested image names. An ${ENV}
// reference cannot be embedded in a registry path suggestion, so fall back to
// a marker the user will obviously replace.
func orgPlaceholder(cfg *config.Config) string {
	if org := cfg.GitHub.Org; org != "" && !config.IsEnvRef(org) {
		return org
	}
	return "OWNER"
}

// wizard asks questions on a line-oriented stream. Every helper is total: after
// the input ends, w.err is set and each ask returns its current value, so a
// truncated session falls through quickly and nothing is written.
type wizard struct {
	in  *bufio.Scanner
	out io.Writer
	err error
}

func (w *wizard) read() (string, bool) {
	if w.err != nil {
		return "", false
	}
	if !w.in.Scan() {
		w.err = errors.New("input ended")
		if err := w.in.Err(); err != nil {
			w.err = err
		}
		fmt.Fprintln(w.out)
		return "", false
	}
	return strings.TrimSpace(w.in.Text()), true
}

// ask returns the typed value, current on Enter, or "" when cleared with "-".
func (w *wizard) ask(label, current string) string {
	if w.err != nil {
		return current
	}
	if current != "" {
		fmt.Fprintf(w.out, "%s [%s]: ", label, current)
	} else {
		fmt.Fprintf(w.out, "%s: ", label)
	}
	s, ok := w.read()
	switch {
	case !ok || s == "":
		return current
	case s == "-":
		return ""
	}
	return s
}

func (w *wizard) askRequired(label, current string) string {
	for {
		v := w.ask(label, current)
		if v != "" || w.err != nil {
			return v
		}
		fmt.Fprintln(w.out, "    a value is required")
	}
}

// askSecret behaves like askRequired but never prints a literal credential
// back to the terminal; ${VAR} references are shown as-is.
func (w *wizard) askSecret(label, current string) string {
	display := current
	if current != "" && !config.IsEnvRef(current) {
		display = maskSecret(current)
	}
	for {
		if display != "" {
			fmt.Fprintf(w.out, "%s [%s]: ", label, display)
		} else {
			fmt.Fprintf(w.out, "%s: ", label)
		}
		s, ok := w.read()
		if !ok {
			return current
		}
		if s == "" || s == "-" {
			if current != "" && s == "" {
				return current
			}
			fmt.Fprintln(w.out, "    a value is required")
			continue
		}
		return s
	}
}

func maskSecret(s string) string {
	if len(s) > 8 {
		return s[:4] + "…" + s[len(s)-4:]
	}
	return "…"
}

func (w *wizard) askInt(label string, current int) int {
	for {
		s := w.ask(label, strconv.Itoa(current))
		if w.err != nil {
			return current
		}
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
		fmt.Fprintf(w.out, "    not a number: %q\n", s)
	}
}

func (w *wizard) askInt64(label string, current int64) int64 {
	for {
		cur := ""
		if current != 0 {
			cur = strconv.FormatInt(current, 10)
		}
		s := w.ask(label, cur)
		if w.err != nil {
			return current
		}
		if s == "" {
			fmt.Fprintln(w.out, "    a value is required")
			continue
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		fmt.Fprintf(w.out, "    not a number: %q\n", s)
	}
}

func (w *wizard) askBool(label string, current bool) bool {
	def := "y/N"
	if current {
		def = "Y/n"
	}
	for {
		fmt.Fprintf(w.out, "%s [%s]: ", label, def)
		s, ok := w.read()
		if !ok || s == "" {
			return current
		}
		switch strings.ToLower(s) {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(w.out, "    answer y or n")
	}
}

func (w *wizard) askChoice(label string, options []string, current string) string {
	opts := strings.Join(options, "|")
	for {
		s := w.ask(fmt.Sprintf("%s (%s)", label, opts), current)
		if w.err != nil {
			return current
		}
		for _, o := range options {
			if strings.EqualFold(s, o) {
				return o
			}
		}
		fmt.Fprintf(w.out, "    choose one of: %s\n", opts)
	}
}

func (w *wizard) askList(label string, current []string) []string {
	s := w.ask(label+" (comma-separated)", strings.Join(current, ", "))
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (w *wizard) askDuration(label string, current config.Duration, fallback time.Duration) config.Duration {
	if current == 0 {
		current = config.Duration(fallback)
	}
	for {
		s := w.ask(label, current.String())
		if w.err != nil {
			return current
		}
		if d, err := time.ParseDuration(s); err == nil {
			return config.Duration(d)
		}
		fmt.Fprintf(w.out, "    not a duration (like 15s or 5m): %q\n", s)
	}
}
