package orchestrator

import (
	"testing"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/ghapi"
)

func TestPlan(t *testing.T) {
	tests := []struct {
		name    string
		state   PoolState
		demand  int
		limits  Limits
		desired int
		create  int
		cull    int
	}{
		{
			name:    "cold start tops up to min",
			state:   PoolState{},
			demand:  0,
			limits:  Limits{Min: 1, Max: 8},
			desired: 1,
			create:  1,
		},
		{
			name:    "idle at min does nothing",
			state:   PoolState{Live: 1, Idle: 1},
			demand:  0,
			limits:  Limits{Min: 1, Max: 8},
			desired: 1,
		},
		{
			// The warm runner takes one job; the other two need new runners.
			name:    "three queued jobs against one idle runner",
			state:   PoolState{Live: 1, Idle: 1},
			demand:  3,
			limits:  Limits{Min: 1, Max: 8},
			desired: 3,
			create:  2,
		},
		{
			// Busy runners cannot serve queued work, so they add to the target
			// rather than counting against it.
			name:    "busy runners do not absorb queued demand",
			state:   PoolState{Live: 2, Busy: 2},
			demand:  3,
			limits:  Limits{Min: 1, Max: 8},
			desired: 5,
			create:  3,
		},
		{
			name:    "max caps scale up",
			state:   PoolState{Live: 2, Busy: 2},
			demand:  50,
			limits:  Limits{Min: 1, Max: 4},
			desired: 4,
			create:  2,
		},
		{
			name:    "surplus idle runners are culled back to min",
			state:   PoolState{Live: 5, Idle: 5},
			demand:  0,
			limits:  Limits{Min: 1, Max: 8},
			desired: 1,
			cull:    4,
		},
		{
			// This is the scenario that matters most: a burst finishes, and
			// everything above the floor must go so no cache survives.
			name:    "cull never exceeds the number of idle runners",
			state:   PoolState{Live: 5, Busy: 3, Idle: 2},
			demand:  0,
			limits:  Limits{Min: 1, Max: 8},
			desired: 3,
			cull:    2,
		},
		{
			name:    "starting runners are not culled",
			state:   PoolState{Live: 4, Starting: 4},
			demand:  0,
			limits:  Limits{Min: 1, Max: 8},
			desired: 1,
			cull:    0,
		},
		{
			name:    "min zero scales all the way down",
			state:   PoolState{Live: 2, Idle: 2},
			demand:  0,
			limits:  Limits{Min: 0, Max: 8},
			desired: 0,
			cull:    2,
		},
		{
			name:    "drained pool culls everything idle",
			state:   PoolState{Live: 3, Busy: 1, Idle: 2},
			demand:  0,
			limits:  Limits{Min: 0, Max: 0},
			desired: 0,
			cull:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Plan("test", tt.state, tt.demand, tt.limits)
			if got.Desired != tt.desired {
				t.Errorf("desired = %d, want %d", got.Desired, tt.desired)
			}
			if got.Create != tt.create {
				t.Errorf("create = %d, want %d", got.Create, tt.create)
			}
			if got.Cull != tt.cull {
				t.Errorf("cull = %d, want %d", got.Cull, tt.cull)
			}
		})
	}
}

func TestPlanNeverCullsBusyRunners(t *testing.T) {
	// Exhaustive guard: whatever the numbers, a decision must never remove
	// more runners than are idle. Culling a busy runner fails a live build.
	for live := 0; live <= 10; live++ {
		for busy := 0; busy <= live; busy++ {
			for idle := 0; idle+busy <= live; idle++ {
				st := PoolState{Live: live, Busy: busy, Idle: idle, Starting: live - busy - idle}
				for _, demand := range []int{0, 1, 5} {
					d := Plan("p", st, demand, Limits{Min: 0, Max: 10})
					if d.Cull > idle {
						t.Fatalf("state %+v demand %d: cull %d exceeds idle %d", st, demand, d.Cull, idle)
					}
					if d.Cull > 0 && d.Create > 0 {
						t.Fatalf("state %+v demand %d: created and culled in the same tick", st, demand)
					}
				}
			}
		}
	}
}

func pools() []*config.Pool {
	return []*config.Pool{
		{Name: "linux", Labels: []string{"self-hosted", "linux", "x64"}, Max: 8},
		{Name: "linux-gpu", Labels: []string{"self-hosted", "linux", "x64", "gpu"}, Max: 2},
		{Name: "macos", Labels: []string{"self-hosted", "macos", "arm64"}, Max: 4},
		{Name: "windows", Labels: []string{"self-hosted", "windows", "x64"}, Max: 4},
	}
}

func TestCanServe(t *testing.T) {
	m := NewPoolMatcher(pools())
	linux := pools()[0]
	gpu := pools()[1]

	tests := []struct {
		name   string
		pool   *config.Pool
		labels []string
		want   bool
	}{
		{"exact match", linux, []string{"self-hosted", "linux", "x64"}, true},
		{"subset of pool labels", linux, []string{"self-hosted", "linux"}, true},
		{"case insensitive", linux, []string{"Self-Hosted", "Linux"}, true},
		{"job wants a label the pool lacks", linux, []string{"self-hosted", "linux", "gpu"}, false},
		{"gpu pool serves gpu job", gpu, []string{"self-hosted", "linux", "gpu"}, true},
		{"wrong os", linux, []string{"self-hosted", "windows"}, false},
		{"self-hosted is implicit", linux, []string{"linux", "x64"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.CanServe(tt.pool, ghapi.Job{Labels: tt.labels})
			if got != tt.want {
				t.Errorf("CanServe(%s, %v) = %v, want %v", tt.pool.Name, tt.labels, got, tt.want)
			}
		})
	}
}

func TestCandidatesPrefersTightestFit(t *testing.T) {
	m := NewPoolMatcher(pools())

	// A generic linux job could run on either linux pool. It must not consume
	// scarce GPU capacity that a GPU job will need.
	got := m.Candidates(ghapi.Job{Labels: []string{"self-hosted", "linux"}})
	if len(got) != 2 {
		t.Fatalf("expected 2 candidate pools, got %d", len(got))
	}
	if got[0].Name != "linux" {
		t.Errorf("first candidate = %s, want linux", got[0].Name)
	}

	// A GPU job has only one home.
	got = m.Candidates(ghapi.Job{Labels: []string{"self-hosted", "linux", "gpu"}})
	if len(got) != 1 || got[0].Name != "linux-gpu" {
		t.Errorf("gpu job candidates = %v, want [linux-gpu]", names(got))
	}
}

func TestAssignDemandSpillsOverWhenPoolIsFull(t *testing.T) {
	m := NewPoolMatcher(pools())
	jobs := []ghapi.Job{
		{ID: 1, Labels: []string{"self-hosted", "linux"}},
		{ID: 2, Labels: []string{"self-hosted", "linux"}},
		{ID: 3, Labels: []string{"self-hosted", "linux"}},
	}

	// The generic linux pool is capped at 1 and already has it. Two jobs must
	// spill to the gpu pool, which also carries the linux label.
	live := map[string]int{"linux": 1}
	limits := map[string]Limits{
		"linux":     {Min: 0, Max: 1},
		"linux-gpu": {Min: 0, Max: 2},
		"macos":     {Min: 0, Max: 4},
		"windows":   {Min: 0, Max: 4},
	}

	d := m.AssignDemand(jobs, live, limits)

	// Two jobs fit in the gpu pool's headroom.
	if d.ByPool["linux-gpu"] != 2 {
		t.Errorf("linux-gpu demand = %d, want 2", d.ByPool["linux-gpu"])
	}
	// The third fits nowhere. It is charged to the best-fit pool so the
	// backed-up queue is visible in `arc status`, even though that pool is at
	// max and Plan will not create anything for it.
	if d.ByPool["linux"] != 1 {
		t.Errorf("linux demand = %d, want 1 (overflow charged to best fit)", d.ByPool["linux"])
	}
	if total := d.ByPool["linux"] + d.ByPool["linux-gpu"]; total != 3 {
		t.Errorf("total assigned = %d, want 3", total)
	}
	if len(d.Unassigned) != 0 {
		t.Errorf("unassigned = %v, want none (a pool exists that could serve these)", d.Unassigned)
	}

	// The overflow must not cause the capped pool to exceed its max.
	got := Plan("linux", PoolState{Live: 1, Busy: 1}, d.ByPool["linux"], limits["linux"])
	if got.Create != 0 {
		t.Errorf("capped pool wanted to create %d runners, want 0", got.Create)
	}
}

func TestAssignDemandReportsUnservableJobs(t *testing.T) {
	m := NewPoolMatcher(pools())
	jobs := []ghapi.Job{
		{ID: 1, Repo: "app", Name: "build", Labels: []string{"self-hosted", "freebsd"}},
	}
	d := m.AssignDemand(jobs, map[string]int{}, map[string]Limits{})
	if len(d.Unassigned) != 1 {
		t.Fatalf("unassigned = %d, want 1", len(d.Unassigned))
	}
	if d.Unassigned[0].ID != 1 {
		t.Errorf("wrong job reported unassigned: %+v", d.Unassigned[0])
	}
}

func TestBelongsToPool(t *testing.T) {
	tests := []struct {
		runner string
		pool   string
		want   bool
	}{
		{"arc-linux-a1b2c3d4", "linux", true},
		{"arc-linux-gpu-a1b2c3d4", "linux-gpu", true},
		// The collision that a naive prefix check gets wrong: the linux pool
		// must not claim (and then deregister) linux-gpu's runners.
		{"arc-linux-gpu-a1b2c3d4", "linux", false},
		{"arc-linux-a1b2c3d4", "linux-gpu", false},
		{"arc-linux-NOTHEX!", "linux", false},
		{"arc-linux-a1b2c3", "linux", false},
		{"some-other-runner", "linux", false},
		{"arc-linux-", "linux", false},
	}
	for _, tt := range tests {
		if got := belongsToPool(tt.runner, tt.pool); got != tt.want {
			t.Errorf("belongsToPool(%q, %q) = %v, want %v", tt.runner, tt.pool, got, tt.want)
		}
	}
}

func TestRunnerNameRoundTrips(t *testing.T) {
	for _, pool := range []string{"linux", "linux-gpu", "macos-arm64"} {
		name := runnerName(pool)
		if !belongsToPool(name, pool) {
			t.Errorf("runnerName(%q) = %q, which belongsToPool rejects", pool, name)
		}
	}
}

func names(pools []*config.Pool) []string {
	out := make([]string, len(pools))
	for i, p := range pools {
		out[i] = p.Name
	}
	return out
}
