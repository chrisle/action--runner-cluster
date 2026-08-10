// Package provider defines how a runner instance is created and destroyed.
//
// Two implementations exist:
//
//   - docker  — Linux containers anywhere, Windows containers on a Windows host.
//   - process — an ephemeral runner process in a private, disposable directory.
//     This is how macOS runners work, because macOS cannot be containerized.
//
// Every provider must guarantee the same thing: when an instance is destroyed,
// nothing it wrote survives. That guarantee is what keeps caches, cloned repos
// and toolchain downloads from accumulating across jobs.
package provider

import (
	"context"
	"time"
)

// State is the lifecycle state of an instance as the provider sees it.
type State string

const (
	// StateStarting means created but not yet confirmed running.
	StateStarting State = "starting"
	// StateRunning means the runner process or container is alive.
	StateRunning State = "running"
	// StateExited means it finished; the orchestrator should reap it.
	StateExited State = "exited"
	// StateUnknown means the provider could not determine the state.
	StateUnknown State = "unknown"
)

// Instance is one runner managed by a provider.
type Instance struct {
	// ID is the provider-local identifier (container id, instance directory).
	ID string `json:"id"`
	// Pool is the pool this instance belongs to.
	Pool string `json:"pool"`
	// RunnerName is the name the runner registered with on GitHub.
	RunnerName string `json:"runner_name"`
	// RunnerID is the GitHub runner id from the JIT registration.
	RunnerID int64 `json:"runner_id"`
	// State is the current lifecycle state.
	State State `json:"state"`
	// CreatedAt is when the instance was created.
	CreatedAt time.Time `json:"created_at"`
	// ExitCode is set when State is StateExited.
	ExitCode int `json:"exit_code,omitempty"`
	// Detail is a short human-readable status, shown in `arc status`.
	Detail string `json:"detail,omitempty"`
}

// Age returns how long the instance has existed.
func (i Instance) Age() time.Duration { return time.Since(i.CreatedAt) }

// Spec describes a runner to create.
type Spec struct {
	// Pool is the pool name.
	Pool string
	// RunnerName is the pre-registered runner name.
	RunnerName string
	// RunnerID is the pre-registered GitHub runner id.
	RunnerID int64
	// JITConfig is the base64 just-in-time runner configuration.
	JITConfig string
}

// Provider creates and destroys runner instances for one pool.
type Provider interface {
	// Kind names the provider implementation ("docker", "process").
	Kind() string

	// Preflight verifies the provider can actually work: daemon reachable,
	// image present, template directory readable. Called by `arc doctor` and
	// once at startup so misconfiguration surfaces immediately rather than as
	// a scale-up failure an hour later.
	Preflight(ctx context.Context) error

	// Create launches a runner. It returns as soon as the runner is started;
	// the runner connecting to GitHub is observed separately.
	Create(ctx context.Context, spec Spec) (*Instance, error)

	// List returns every instance this provider currently owns, including
	// exited ones awaiting reaping. It reads from the underlying system rather
	// than in-memory state, so the orchestrator recovers after a restart.
	List(ctx context.Context) ([]Instance, error)

	// Destroy removes the instance and every byte of scratch state it created.
	// It must be idempotent: destroying an unknown id is not an error.
	Destroy(ctx context.Context, id string) error

	// Logs returns the tail of an instance's output, for diagnosing a runner
	// that failed to come up.
	Logs(ctx context.Context, id string, lines int) (string, error)

	// Prune removes orphaned state left behind by crashes: containers and
	// directories that belong to no live instance. Returns how many it removed.
	Prune(ctx context.Context) (int, error)

	// Close releases provider resources.
	Close() error
}

// Metadata keys shared by providers for tagging instances.
const (
	LabelManaged    = "arc.managed"
	LabelPool       = "arc.pool"
	LabelRunnerName = "arc.runner-name"
	LabelRunnerID   = "arc.runner-id"
	LabelCreated    = "arc.created"
)
