package dockerprov

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/provider"
)

// testImage is small, present on most machines, and exits on its own, which is
// what the reaping path needs to observe.
const testImage = "alpine:3"

// requireDocker skips the test unless a usable Docker daemon is reachable.
// Set ARC_SKIP_DOCKER_TESTS=1 to skip regardless.
func requireDocker(t *testing.T) *engine {
	t.Helper()
	if os.Getenv("ARC_SKIP_DOCKER_TESTS") != "" {
		t.Skip("ARC_SKIP_DOCKER_TESTS is set")
	}

	eng, err := newEngine("", nil)
	if err != nil {
		t.Skipf("no docker host: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := eng.version(ctx); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
	return eng
}

func testProvider(t *testing.T, name string) *Provider {
	t.Helper()
	pool := &config.Pool{
		Name:     name,
		Labels:   []string{"self-hosted", "linux", "x64"},
		Provider: config.ProviderDocker,
		Min:      0,
		Max:      4,
		Docker: &config.DockerSpec{
			Image:   testImage,
			Pull:    "missing",
			WorkDir: "/home/runner/_work",
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := New(pool, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestIntegrationContainerLifecycle(t *testing.T) {
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// A unique pool name keeps concurrent runs and leftover state from other
	// tests out of this one's label filter.
	p := testProvider(t, "arctest-lifecycle")

	if err := p.ensureImage(ctx); err != nil {
		t.Skipf("cannot obtain %s: %v", testImage, err)
	}

	// Nothing should exist for a fresh pool name.
	before, err := p.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("expected no instances for a fresh pool, got %d", len(before))
	}

	inst, err := p.Create(ctx, provider.Spec{
		Pool:       p.pool.Name,
		RunnerName: "arc-arctest-lifecycle-aabbccdd",
		RunnerID:   4242,
		JITConfig:  "not-a-real-jit-config",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Whatever happens next, do not leave a container behind.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = p.Destroy(cleanupCtx, inst.ID)
	})

	if inst.ID == "" {
		t.Fatal("Create returned an empty instance id")
	}

	// The instance must be discoverable purely from the daemon, with its
	// metadata intact: that is what lets the orchestrator recover after a
	// restart without any local state.
	found, err := p.List(ctx)
	if err != nil {
		t.Fatalf("List after create: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("List returned %d instances, want 1", len(found))
	}
	got := found[0]
	if got.RunnerName != "arc-arctest-lifecycle-aabbccdd" {
		t.Errorf("RunnerName = %q, not recovered from container labels", got.RunnerName)
	}
	if got.RunnerID != 4242 {
		t.Errorf("RunnerID = %d, want 4242", got.RunnerID)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt was not recovered from container labels")
	}
	if got.Pool != p.pool.Name {
		t.Errorf("Pool = %q, want %q", got.Pool, p.pool.Name)
	}

	// alpine's default command exits immediately, so the instance should reach
	// StateExited — the same transition a finished runner makes.
	deadline := time.Now().Add(30 * time.Second)
	var state provider.State
	for time.Now().Before(deadline) {
		list, err := p.List(ctx)
		if err != nil {
			t.Fatalf("List while waiting for exit: %v", err)
		}
		if len(list) == 0 {
			t.Fatal("instance disappeared before it was destroyed")
		}
		state = list[0].State
		if state == provider.StateExited {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if state != provider.StateExited {
		t.Fatalf("instance state = %q after 30s, want exited", state)
	}

	if _, _, err := p.ExitCode(ctx, inst.ID); err != nil {
		t.Errorf("ExitCode: %v", err)
	}

	if err := p.Destroy(ctx, inst.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	after, err := p.List(ctx)
	if err != nil {
		t.Fatalf("List after destroy: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("Destroy left %d instance(s) behind", len(after))
	}

	// Destroying twice must not fail: the reconciler can race with itself.
	if err := p.Destroy(ctx, inst.ID); err != nil {
		t.Errorf("second Destroy should be a no-op, got: %v", err)
	}
}

func TestIntegrationPoolIsolation(t *testing.T) {
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	a := testProvider(t, "arctest-pool-a")
	b := testProvider(t, "arctest-pool-b")

	if err := a.ensureImage(ctx); err != nil {
		t.Skipf("cannot obtain %s: %v", testImage, err)
	}

	instA, err := a.Create(ctx, provider.Spec{
		Pool: a.pool.Name, RunnerName: "arc-arctest-pool-a-11112222", RunnerID: 1, JITConfig: "x",
	})
	if err != nil {
		t.Fatalf("Create in pool a: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.Destroy(c, instA.ID)
	})

	// Pool b must not see pool a's container. If the label filter were wrong,
	// one pool would reap another pool's runners mid-job.
	listB, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List pool b: %v", err)
	}
	if len(listB) != 0 {
		t.Errorf("pool b sees %d instance(s) belonging to pool a", len(listB))
	}

	listA, err := a.List(ctx)
	if err != nil {
		t.Fatalf("List pool a: %v", err)
	}
	if len(listA) != 1 {
		t.Errorf("pool a sees %d instances, want 1", len(listA))
	}
}

func TestIntegrationPreflightRejectsWrongContainerOS(t *testing.T) {
	eng := requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v, err := eng.version(ctx)
	if err != nil {
		t.Skipf("docker version: %v", err)
	}
	if v.Os == "windows" {
		t.Skip("this daemon runs Windows containers; the mismatch under test cannot occur")
	}

	// A Windows pool pointed at a Linux daemon is the misconfiguration that
	// silently produces Linux runners wearing a "windows" label. Preflight must
	// catch it rather than letting jobs fail confusingly much later.
	pool := &config.Pool{
		Name:     "arctest-windows",
		Labels:   []string{"self-hosted", "windows", "x64"},
		Provider: config.ProviderDocker,
		Max:      1,
		Docker:   &config.DockerSpec{Image: testImage, Pull: "never", WorkDir: `C:\actions-runner\_work`},
	}
	p, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = p.Preflight(ctx)
	if err == nil {
		t.Fatal("expected preflight to reject a windows pool on a linux daemon")
	}
	if !strings.Contains(err.Error(), "windows label") {
		t.Errorf("error should explain the OS mismatch, got: %v", err)
	}
}
