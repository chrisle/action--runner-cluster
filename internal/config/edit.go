package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file supports tools that rewrite the config file — today `arc config`.
// The normal Load path expands ${VAR} references, which is exactly wrong for an
// editor: loading and re-saving would bake the expanded secrets into the file.

// DefaultUserPath is where `arc config` writes and the CLI falls back to
// reading: ~/.config/arc/arc.yaml. Empty when the home directory is unknown.
func DefaultUserPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "arc", "arc.yaml")
}

// ParseEditable decodes config YAML strictly but leaves it otherwise untouched:
// no ${VAR} expansion, no defaults, no validation. What comes out is what is
// literally in the file, so it can be edited and written back losslessly.
func ParseEditable(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return &cfg, nil
}

// CheckBytes reports whether raw would survive the real load pipeline, minus
// env expansion — ${VAR} references are treated as opaque values, since whether
// they resolve is a property of the runtime environment, not of the file.
func CheckBytes(raw []byte) error {
	cfg, err := ParseEditable(raw)
	if err != nil {
		return err
	}
	cfg.applyDefaults()
	return cfg.Validate()
}

// Marshal renders the config as YAML suitable for writing back to disk.
func (c *Config) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// IsEnvRef reports whether a value is an unexpanded ${VAR} reference.
func IsEnvRef(s string) bool {
	return strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}")
}
