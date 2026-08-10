// Package state persists small pieces of runtime state across restarts.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Override is a live adjustment to a pool's limits, set with `arc scale`.
//
// Nil means "inherit from the config file", which is what allows `arc scale
// linux --max 12` to raise the ceiling without also pinning the floor.
type Override struct {
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
}

// Overrides is a persisted map of pool name to override.
type Overrides struct {
	path string

	mu sync.RWMutex
	m  map[string]Override
}

// LoadOverrides reads the override file, creating an empty set if absent.
func LoadOverrides(stateDir string) (*Overrides, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	o := &Overrides{
		path: filepath.Join(stateDir, "overrides.json"),
		m:    map[string]Override{},
	}

	data, err := os.ReadFile(o.path)
	if err != nil {
		if os.IsNotExist(err) {
			return o, nil
		}
		return nil, fmt.Errorf("read %s: %w", o.path, err)
	}
	if err := json.Unmarshal(data, &o.m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", o.path, err)
	}
	return o, nil
}

// Get returns the override for a pool.
func (o *Overrides) Get(pool string) (Override, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	ov, ok := o.m[pool]
	return ov, ok
}

// All returns a copy of every override.
func (o *Overrides) All() map[string]Override {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]Override, len(o.m))
	for k, v := range o.m {
		out[k] = v
	}
	return out
}

// Set records an override and persists it. A nil min and nil max clears it.
func (o *Overrides) Set(pool string, min, max *int) error {
	o.mu.Lock()
	existing := o.m[pool]
	if min != nil {
		existing.Min = min
	}
	if max != nil {
		existing.Max = max
	}
	if existing.Min == nil && existing.Max == nil {
		delete(o.m, pool)
	} else {
		o.m[pool] = existing
	}
	snapshot := make(map[string]Override, len(o.m))
	for k, v := range o.m {
		snapshot[k] = v
	}
	o.mu.Unlock()

	return o.persist(snapshot)
}

// Clear removes any override for a pool, returning it to config values.
func (o *Overrides) Clear(pool string) error {
	o.mu.Lock()
	delete(o.m, pool)
	snapshot := make(map[string]Override, len(o.m))
	for k, v := range o.m {
		snapshot[k] = v
	}
	o.mu.Unlock()
	return o.persist(snapshot)
}

// persist writes atomically: a torn overrides file would fail to parse at the
// next start and take the orchestrator down with it.
func (o *Overrides) persist(snapshot map[string]Override) error {
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, o.path); err != nil {
		return fmt.Errorf("replace %s: %w", o.path, err)
	}
	return nil
}
