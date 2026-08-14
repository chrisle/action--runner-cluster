package orchestrator

import (
	"io"
	"log/slog"
	"testing"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/ghapi"
	"github.com/chrisle/action-runner-cluster/internal/provider"
)

func runnerWith(name, repo string, labels ...string) ghapi.Runner {
	r := ghapi.Runner{Name: name, Status: "online", Repo: repo}
	for _, l := range labels {
		r.Labels = append(r.Labels, struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}{Name: l})
	}
	return r
}

func TestDiscountForeignIdle(t *testing.T) {
	cfg := &config.Config{Pools: []*config.Pool{
		{Name: "macos", Labels: []string{"self-hosted", "macos", "arm64"}, Max: 4},
	}}
	o := &Orchestrator{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	jobs := []ghapi.Job{
		{ID: 1, Repo: "app", Labels: []string{"self-hosted", "macos"}},
		{ID: 2, Repo: "web", Labels: []string{"self-hosted", "macos"}},
	}
	d := &Demand{
		ByPool: map[string]int{"macos": 2},
		Jobs:   map[string][]ghapi.Job{"macos": jobs},
	}

	// Another arc host has an idle runner registered on repo "app": it covers
	// job 1 but, being repo-scoped, not job 2.
	runners := []ghapi.Runner{
		runnerWith("arc-macos-ffffff-a1b2c3d4", "app", "self-hosted", "macos", "arm64"),
	}
	o.discountForeignIdle(d, runners, map[string][]provider.Instance{})

	if d.ByPool["macos"] != 1 {
		t.Errorf("demand = %d, want 1 (one job covered by the foreign idle runner)", d.ByPool["macos"])
	}
	if len(d.Jobs["macos"]) != 1 || d.Jobs["macos"][0].ID != 2 {
		t.Errorf("remaining jobs = %+v, want only job 2", d.Jobs["macos"])
	}
}

func TestDiscountIgnoresOwnAndBusyRunners(t *testing.T) {
	cfg := &config.Config{Pools: []*config.Pool{
		{Name: "macos", Labels: []string{"self-hosted", "macos", "arm64"}, Max: 4},
	}}
	o := &Orchestrator{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	jobs := []ghapi.Job{{ID: 1, Repo: "app", Labels: []string{"self-hosted", "macos"}}}
	d := &Demand{
		ByPool: map[string]int{"macos": 1},
		Jobs:   map[string][]ghapi.Job{"macos": jobs},
	}

	busy := runnerWith("arc-macos-ffffff-11111111", "app", "self-hosted", "macos", "arm64")
	busy.Busy = true
	offline := runnerWith("arc-macos-ffffff-22222222", "app", "self-hosted", "macos", "arm64")
	offline.Status = "offline"
	mine := runnerWith("arc-macos-eeeeee-33333333", "app", "self-hosted", "macos", "arm64")

	live := map[string][]provider.Instance{
		"macos": {{RunnerName: mine.Name}},
	}
	o.discountForeignIdle(d, []ghapi.Runner{busy, offline, mine}, live)

	if d.ByPool["macos"] != 1 {
		t.Errorf("demand = %d, want 1 (busy, offline, and own runners are not capacity)", d.ByPool["macos"])
	}
}
