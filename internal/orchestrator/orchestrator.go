// Package orchestrator reconciles the desired number of runners against what
// is actually running.
//
// The loop is deliberately a reconciler rather than an event handler: every
// tick it re-derives the whole picture from GitHub and the providers, then
// makes the smallest change that moves toward the target. Missed events,
// crashed containers, a restarted orchestrator and manual meddling with docker
// all converge on the next tick instead of leaving permanent drift.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/ghapi"
	"github.com/chrisle/action-runner-cluster/internal/provider"
	"github.com/chrisle/action-runner-cluster/internal/provider/dockerprov"
	"github.com/chrisle/action-runner-cluster/internal/provider/processprov"
	"github.com/chrisle/action-runner-cluster/internal/state"
)

// maxCreateConcurrency bounds simultaneous runner launches. Image pulls and
// template clones are IO-heavy; launching twenty at once makes all twenty slow.
const maxCreateConcurrency = 4

// Orchestrator owns the reconcile loop.
type Orchestrator struct {
	cfg       *config.Config
	gh        *ghapi.Client
	log       *slog.Logger
	overrides *state.Overrides

	providers map[string]provider.Provider
	matcher   *PoolMatcher

	mu       sync.RWMutex
	snapshot Snapshot

	// repos is the cached list of pollable org repos.
	repoMu       sync.Mutex
	repos        []ghapi.Repo
	reposFetched time.Time

	// idleSince tracks when each instance was first seen idle, so a surplus
	// runner is only culled after it has been idle for the whole timeout.
	idleSince map[string]time.Time

	// drained pools create no new runners and cull to zero.
	drainMu sync.RWMutex
	drained map[string]bool
}

// Snapshot is the orchestrator's view of the world, served by `arc status`.
type Snapshot struct {
	UpdatedAt    time.Time       `json:"updated_at"`
	Org          string          `json:"org"`
	Pools        []PoolSnapshot  `json:"pools"`
	Unassigned   []ghapi.Job     `json:"unassigned_jobs,omitempty"`
	RateLimit    ghapi.RateLimit `json:"rate_limit"`
	ReposWatched int             `json:"repos_watched"`
	LastError    string          `json:"last_error,omitempty"`
	// ConsecutiveFailures counts reconcile passes that failed outright. A loop
	// that is ticking but failing every pass is not healthy, even though its
	// timestamp keeps advancing — bad credentials look exactly like this.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
}

// PoolSnapshot is one pool's observed and desired state.
type PoolSnapshot struct {
	Name      string             `json:"name"`
	Provider  string             `json:"provider"`
	Labels    []string           `json:"labels"`
	Min       int                `json:"min"`
	Max       int                `json:"max"`
	Desired   int                `json:"desired"`
	Live      int                `json:"live"`
	Busy      int                `json:"busy"`
	Idle      int                `json:"idle"`
	Starting  int                `json:"starting"`
	Queued    int                `json:"queued"`
	Drained   bool               `json:"drained,omitempty"`
	Reason    string             `json:"reason,omitempty"`
	Error     string             `json:"error,omitempty"`
	Instances []InstanceSnapshot `json:"instances"`
}

// InstanceSnapshot is one runner's state.
type InstanceSnapshot struct {
	Name      string    `json:"name"`
	ID        string    `json:"id"`
	RunnerID  int64     `json:"runner_id"`
	State     string    `json:"state"`
	Online    bool      `json:"online"`
	Busy      bool      `json:"busy"`
	Age       string    `json:"age"`
	CreatedAt time.Time `json:"created_at"`
	Detail    string    `json:"detail,omitempty"`
}

// New builds an orchestrator and its providers.
func New(cfg *config.Config, gh *ghapi.Client, overrides *state.Overrides, log *slog.Logger) (*Orchestrator, error) {
	o := &Orchestrator{
		cfg:       cfg,
		gh:        gh,
		log:       log,
		overrides: overrides,
		providers: make(map[string]provider.Provider, len(cfg.Pools)),
		matcher:   NewPoolMatcher(cfg.Pools),
		idleSince: make(map[string]time.Time),
		drained:   make(map[string]bool),
	}

	for _, pool := range cfg.Pools {
		p, err := buildProvider(pool, log)
		if err != nil {
			return nil, err
		}
		o.providers[pool.Name] = p
	}
	return o, nil
}

func buildProvider(pool *config.Pool, log *slog.Logger) (provider.Provider, error) {
	switch pool.Provider {
	case config.ProviderDocker:
		return dockerprov.New(pool, log)
	case config.ProviderProcess:
		return processprov.New(pool, log)
	default:
		return nil, fmt.Errorf("pool %s: unknown provider %q", pool.Name, pool.Provider)
	}
}

// Preflight checks every provider before the loop starts. It reports all
// failures rather than the first, so one run surfaces everything to fix.
func (o *Orchestrator) Preflight(ctx context.Context) error {
	var errs []string
	for _, pool := range o.cfg.Pools {
		if err := o.PreflightPool(ctx, pool.Name); err != nil {
			errs = append(errs, fmt.Sprintf("pool %s: %v", pool.Name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New("provider preflight failed:\n  - " + joinLines(errs))
	}
	return nil
}

// PreflightPool checks a single pool's provider.
func (o *Orchestrator) PreflightPool(ctx context.Context, pool string) error {
	p, ok := o.providers[pool]
	if !ok {
		return fmt.Errorf("unknown pool %q", pool)
	}
	return p.Preflight(ctx)
}

// Run drives the reconcile loop until ctx is cancelled.
func (o *Orchestrator) Run(ctx context.Context) error {
	interval := o.cfg.GitHub.PollInterval.Duration()
	o.log.Info("orchestrator started",
		"org", o.cfg.GitHub.Org,
		"pools", len(o.cfg.Pools),
		"poll_interval", interval,
		"auth", o.gh.AuthDescription())

	// Reconcile immediately so a restart converges without waiting a full tick.
	o.reconcileOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Housekeeping runs less often than reconciliation: pruning debris and
	// sweeping the HTTP cache are not urgent, and both walk more state.
	housekeeping := time.NewTicker(10 * time.Minute)
	defer housekeeping.Stop()

	for {
		select {
		case <-ctx.Done():
			o.log.Info("orchestrator stopping")
			return nil
		case <-ticker.C:
			o.reconcileOnce(ctx)
		case <-housekeeping.C:
			o.housekeep(ctx)
		}
	}
}

// reconcileOnce performs a single pass. It never returns an error: a failed
// tick is logged and surfaced in the snapshot, and the next tick tries again.
func (o *Orchestrator) reconcileOnce(ctx context.Context) {
	start := time.Now()
	snap := Snapshot{UpdatedAt: start, Org: o.cfg.GitHub.Org}

	// 1. What GitHub thinks is registered, and which runners are busy.
	runners, err := o.gh.ListRunners(ctx)
	if err != nil {
		o.log.Error("list runners failed", "error", err)
		o.recordFailure(fmt.Sprintf("list runners: %v", err))
		return
	}
	byName := make(map[string]ghapi.Runner, len(runners))
	for _, r := range runners {
		byName[r.Name] = r
	}

	// 2. What each provider actually has, and reap anything finished.
	liveByPool := make(map[string][]provider.Instance, len(o.cfg.Pools))
	poolErr := make(map[string]string)
	for _, pool := range o.cfg.Pools {
		instances, err := o.providers[pool.Name].List(ctx)
		if err != nil {
			o.log.Error("list instances failed", "pool", pool.Name, "error", err)
			poolErr[pool.Name] = err.Error()
			continue
		}
		liveByPool[pool.Name] = o.reap(ctx, pool, instances, byName)
	}

	// 3. What work is waiting.
	repos, err := o.pollableRepos(ctx)
	if err != nil {
		o.log.Error("list repos failed", "error", err)
		o.recordFailure(fmt.Sprintf("list repos: %v", err))
		return
	}
	snap.ReposWatched = len(repos)

	jobs, err := o.gh.PendingJobs(ctx, repos, 8)
	if err != nil {
		var partial *ghapi.PartialError
		if errors.As(err, &partial) {
			// Some repos failed; the rest of the picture is still actionable.
			o.log.Warn("some repos could not be polled", "error", partial)
			snap.LastError = partial.Error()
		} else {
			o.log.Error("poll queued jobs failed", "error", err)
			o.recordFailure(fmt.Sprintf("poll jobs: %v", err))
			return
		}
	}

	// 4. Assign demand and decide.
	liveCounts := make(map[string]int, len(o.cfg.Pools))
	limits := make(map[string]Limits, len(o.cfg.Pools))
	for _, pool := range o.cfg.Pools {
		liveCounts[pool.Name] = len(liveByPool[pool.Name])
		limits[pool.Name] = o.limitsFor(pool)
	}
	demand := o.matcher.AssignDemand(jobs, liveCounts, limits)
	snap.Unassigned = demand.Unassigned

	if len(demand.Unassigned) > 0 {
		for _, j := range demand.Unassigned {
			o.log.Warn("queued job matches no pool",
				"repo", j.Repo, "job", j.Name, "labels", j.Labels)
		}
	}

	// 5. Act, one pool at a time.
	for _, pool := range o.cfg.Pools {
		ps := o.applyPool(ctx, pool, liveByPool[pool.Name], byName, demand.ByPool[pool.Name])
		if e := poolErr[pool.Name]; e != "" {
			ps.Error = e
		}
		snap.Pools = append(snap.Pools, ps)
	}

	snap.RateLimit = o.gh.RateLimit()
	o.mu.Lock()
	// A completed pass clears the failure streak; snap starts at zero.
	o.snapshot = snap
	o.mu.Unlock()

	o.log.Debug("reconcile complete",
		"duration", time.Since(start).Round(time.Millisecond),
		"queued", len(jobs),
		"repos", len(repos))
}

// reap destroys instances that have finished and deregisters runners GitHub
// still lists but that no longer have a live instance behind them. It returns
// the instances that remain live.
func (o *Orchestrator) reap(ctx context.Context, pool *config.Pool, instances []provider.Instance, byName map[string]ghapi.Runner) []provider.Instance {
	live := make([]provider.Instance, 0, len(instances))

	for _, inst := range instances {
		switch {
		case inst.State == provider.StateExited || inst.State == provider.StateUnknown:
			// An ephemeral runner exits after exactly one job, so this is the
			// normal end of life, not a failure.
			o.log.Debug("reaping finished runner", "pool", pool.Name, "runner", inst.RunnerName, "detail", inst.Detail)
			o.destroyInstance(ctx, pool, inst, byName, "finished")

		case inst.Age() > pool.JobTimeout.Duration():
			// A job that outlives job_timeout is stuck: GitHub's own timeout
			// did not fire, or the runner lost its connection mid-job. Nothing
			// good comes from letting it hold a slot forever.
			o.log.Warn("runner exceeded job_timeout, destroying",
				"pool", pool.Name, "runner", inst.RunnerName,
				"age", inst.Age().Round(time.Second), "timeout", pool.JobTimeout)
			o.destroyInstance(ctx, pool, inst, byName, "job timeout")

		default:
			live = append(live, inst)
		}
	}

	// Registrations with no instance behind them: the container was removed
	// out-of-band, or the orchestrator died between registering and creating.
	// GitHub would otherwise route jobs to a runner that does not exist.
	have := make(map[string]bool, len(instances))
	for _, inst := range instances {
		have[inst.RunnerName] = true
	}
	for name, r := range byName {
		if have[name] || !belongsToPool(name, pool.Name) {
			continue
		}
		if r.Busy {
			continue // never touch a runner mid-job
		}
		o.log.Info("removing orphaned runner registration", "pool", pool.Name, "runner", name, "runner_id", r.ID)
		if err := o.gh.RemoveRunner(ctx, r.ID); err != nil && !errors.Is(err, ghapi.ErrRunnerBusy) {
			o.log.Warn("failed to remove orphaned runner", "runner", name, "error", err)
		}
	}

	return live
}

// destroyInstance removes the instance and its GitHub registration.
func (o *Orchestrator) destroyInstance(ctx context.Context, pool *config.Pool, inst provider.Instance, byName map[string]ghapi.Runner, why string) {
	// Deregister first. If the instance is destroyed first, GitHub keeps an
	// offline runner listed and may still try to route a job to it.
	if r, ok := byName[inst.RunnerName]; ok {
		if err := o.gh.RemoveRunner(ctx, r.ID); err != nil {
			if errors.Is(err, ghapi.ErrRunnerBusy) {
				o.log.Info("runner picked up a job, leaving it alone",
					"pool", pool.Name, "runner", inst.RunnerName)
				return
			}
			o.log.Warn("deregister failed, destroying anyway",
				"pool", pool.Name, "runner", inst.RunnerName, "error", err)
		}
	}

	if err := o.providers[pool.Name].Destroy(ctx, inst.ID); err != nil {
		o.log.Error("destroy instance failed",
			"pool", pool.Name, "runner", inst.RunnerName, "reason", why, "error", err)
		return
	}

	o.mu.Lock()
	delete(o.idleSince, instanceKey(pool.Name, inst.RunnerName))
	o.mu.Unlock()
}

// applyPool computes and executes the decision for a single pool.
func (o *Orchestrator) applyPool(ctx context.Context, pool *config.Pool, live []provider.Instance, byName map[string]ghapi.Runner, demand int) PoolSnapshot {
	lim := o.limitsFor(pool)
	drained := o.isDrained(pool.Name)
	if drained {
		lim = Limits{Min: 0, Max: 0}
		demand = 0
	}

	st := PoolState{Live: len(live)}
	instances := make([]InstanceSnapshot, 0, len(live))

	for _, inst := range live {
		r, known := byName[inst.RunnerName]
		snap := InstanceSnapshot{
			Name:      inst.RunnerName,
			ID:        inst.ID,
			RunnerID:  inst.RunnerID,
			State:     string(inst.State),
			CreatedAt: inst.CreatedAt,
			Age:       inst.Age().Round(time.Second).String(),
			Detail:    inst.Detail,
		}
		switch {
		case known && r.Busy:
			st.Busy++
			snap.Online, snap.Busy = true, true
		case known && r.Online():
			st.Idle++
			snap.Online = true
		default:
			// Registered but not yet connected, or already gone from GitHub.
			st.Starting++
		}
		instances = append(instances, snap)
	}

	decision := Plan(pool.Name, st, demand, lim)

	switch {
	case decision.Create > 0 && !drained:
		o.scaleUp(ctx, pool, decision.Create)
	case decision.Cull > 0:
		o.scaleDown(ctx, pool, live, byName, decision.Cull)
	}

	sort.Slice(instances, func(i, j int) bool { return instances[i].CreatedAt.Before(instances[j].CreatedAt) })

	return PoolSnapshot{
		Name:      pool.Name,
		Provider:  pool.Provider,
		Labels:    pool.Labels,
		Min:       lim.Min,
		Max:       lim.Max,
		Desired:   decision.Desired,
		Live:      st.Live,
		Busy:      st.Busy,
		Idle:      st.Idle,
		Starting:  st.Starting,
		Queued:    demand,
		Drained:   drained,
		Reason:    decision.Reason,
		Instances: instances,
	}
}

// scaleUp launches n runners concurrently, bounded by maxCreateConcurrency.
func (o *Orchestrator) scaleUp(ctx context.Context, pool *config.Pool, n int) {
	groupID, err := o.gh.RunnerGroupID(ctx, o.cfg.EffectiveRunnerGroup(pool))
	if err != nil {
		o.log.Error("resolve runner group failed", "pool", pool.Name, "error", err)
		return
	}

	o.log.Info("scaling up", "pool", pool.Name, "count", n)

	sem := make(chan struct{}, maxCreateConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := o.launchOne(ctx, pool, groupID); err != nil {
				o.log.Error("launch runner failed", "pool", pool.Name, "error", err)
			}
		}()
	}
	wg.Wait()
}

// launchOne registers a JIT runner and starts an instance for it.
func (o *Orchestrator) launchOne(ctx context.Context, pool *config.Pool, groupID int64) error {
	name := runnerName(pool.Name)

	jit, err := o.gh.GenerateJITConfig(ctx, name, pool.Labels, groupID, "_work")
	if err != nil {
		return err
	}

	inst, err := o.providers[pool.Name].Create(ctx, provider.Spec{
		Pool:       pool.Name,
		RunnerName: name,
		RunnerID:   jit.Runner.ID,
		JITConfig:  jit.Encoded,
	})
	if err != nil {
		// The registration exists but nothing will ever claim it. Remove it, or
		// GitHub accumulates offline runners it may try to route jobs to.
		// Use a detached context so cleanup still happens during shutdown.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if rmErr := o.gh.RemoveRunner(cleanupCtx, jit.Runner.ID); rmErr != nil {
			o.log.Warn("failed to remove registration for runner that never started",
				"runner", name, "runner_id", jit.Runner.ID, "error", rmErr)
		}
		return err
	}

	o.log.Info("runner launched", "pool", pool.Name, "runner", name, "instance", inst.ID, "runner_id", jit.Runner.ID)
	return nil
}

// scaleDown removes up to n idle runners, oldest first.
//
// Only runners that have been continuously idle for the pool's idle_timeout are
// eligible. Culling the instant demand drops would thrash: a burst of jobs
// arriving seconds apart would create and destroy the same capacity repeatedly.
func (o *Orchestrator) scaleDown(ctx context.Context, pool *config.Pool, live []provider.Instance, byName map[string]ghapi.Runner, n int) {
	now := time.Now()
	timeout := pool.IdleTimeout.Duration()

	type candidate struct {
		inst provider.Instance
		idle time.Duration
	}
	var eligible []candidate

	o.mu.Lock()
	for _, inst := range live {
		r, ok := byName[inst.RunnerName]
		if !ok || r.Busy || !r.Online() {
			// Not idle, or not yet connected. Forget any idle timestamp so a
			// runner that goes busy starts its idle clock fresh afterwards.
			delete(o.idleSince, instanceKey(pool.Name, inst.RunnerName))
			continue
		}
		key := instanceKey(pool.Name, inst.RunnerName)
		since, seen := o.idleSince[key]
		if !seen {
			o.idleSince[key] = now
			continue
		}
		if idle := now.Sub(since); idle >= timeout {
			eligible = append(eligible, candidate{inst: inst, idle: idle})
		}
	}
	o.mu.Unlock()

	if len(eligible) == 0 {
		return
	}

	// Oldest idle first: it has had the longest chance to pick up work and is
	// the most likely to be holding a stale environment.
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].idle > eligible[j].idle })
	if len(eligible) > n {
		eligible = eligible[:n]
	}

	o.log.Info("scaling down", "pool", pool.Name, "count", len(eligible))
	for _, c := range eligible {
		o.destroyInstance(ctx, pool, c.inst, byName, "idle surplus")
	}
}

// limitsFor returns a pool's min/max after applying any live override.
func (o *Orchestrator) limitsFor(pool *config.Pool) Limits {
	lim := Limits{Min: pool.Min, Max: pool.Max}
	if o.overrides != nil {
		if ov, ok := o.overrides.Get(pool.Name); ok {
			if ov.Min != nil {
				lim.Min = *ov.Min
			}
			if ov.Max != nil {
				lim.Max = *ov.Max
			}
		}
	}
	if lim.Min > lim.Max {
		lim.Min = lim.Max
	}
	return lim
}

// pollableRepos returns the cached repo list, refreshing it when stale.
func (o *Orchestrator) pollableRepos(ctx context.Context) ([]ghapi.Repo, error) {
	o.repoMu.Lock()
	defer o.repoMu.Unlock()

	if time.Since(o.reposFetched) < o.cfg.GitHub.Repos.RefreshInterval.Duration() && o.repos != nil {
		return o.repos, nil
	}

	repos, err := o.gh.ListRepos(ctx, ghapi.RepoFilterOpts{
		Include:      o.cfg.GitHub.Repos.Include,
		Exclude:      o.cfg.GitHub.Repos.Exclude,
		ActiveWithin: o.cfg.GitHub.Repos.ActiveWithin.Duration(),
		Archived:     o.cfg.GitHub.Repos.Archived,
	})
	if err != nil {
		if o.repos != nil {
			// Serve the stale list rather than going blind to demand.
			o.log.Warn("repo refresh failed, using cached list", "error", err, "repos", len(o.repos))
			return o.repos, nil
		}
		return nil, err
	}

	if len(repos) != len(o.repos) {
		o.log.Info("watching repos", "count", len(repos))
	}
	o.repos, o.reposFetched = repos, time.Now()
	return repos, nil
}

// housekeep removes debris and bounds memory use.
func (o *Orchestrator) housekeep(ctx context.Context) {
	for _, pool := range o.cfg.Pools {
		n, err := o.providers[pool.Name].Prune(ctx)
		if err != nil {
			o.log.Warn("prune failed", "pool", pool.Name, "error", err)
			continue
		}
		if n > 0 {
			o.log.Info("pruned orphaned instances", "pool", pool.Name, "count", n)
		}
	}
	o.gh.SweepCache(time.Hour)

	// Drop idle bookkeeping for instances that no longer exist.
	o.mu.Lock()
	for key, since := range o.idleSince {
		if time.Since(since) > 24*time.Hour {
			delete(o.idleSince, key)
		}
	}
	o.mu.Unlock()
}

// Snapshot returns the most recent view of the cluster.
func (o *Orchestrator) Snapshot() Snapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.snapshot
}

// Logs returns the tail of an instance's output.
func (o *Orchestrator) Logs(ctx context.Context, pool, id string, lines int) (string, error) {
	p, ok := o.providers[pool]
	if !ok {
		return "", fmt.Errorf("unknown pool %q", pool)
	}
	return p.Logs(ctx, id, lines)
}

// SetDrained stops or resumes a pool. A drained pool creates nothing and culls
// its idle runners to zero, without disturbing jobs already running.
func (o *Orchestrator) SetDrained(pool string, drained bool) error {
	if o.cfg.Pool(pool) == nil {
		return fmt.Errorf("unknown pool %q", pool)
	}
	o.drainMu.Lock()
	o.drained[pool] = drained
	o.drainMu.Unlock()
	o.log.Info("pool drain state changed", "pool", pool, "drained", drained)
	return nil
}

func (o *Orchestrator) isDrained(pool string) bool {
	o.drainMu.RLock()
	defer o.drainMu.RUnlock()
	return o.drained[pool]
}

// Close shuts down providers.
func (o *Orchestrator) Close() error {
	for _, p := range o.providers {
		_ = p.Close()
	}
	return nil
}

// recordFailure marks a reconcile pass as having failed outright.
func (o *Orchestrator) recordFailure(msg string) {
	o.mu.Lock()
	o.snapshot.Org = o.cfg.GitHub.Org
	o.snapshot.LastError = msg
	o.snapshot.UpdatedAt = time.Now()
	o.snapshot.ConsecutiveFailures++
	o.mu.Unlock()
}

// Healthy reports whether the loop is doing useful work.
//
// A loop that ticks on schedule but fails every pass — the shape of an expired
// token or a revoked App installation — must not read as healthy, or monitoring
// will stay green while no job ever gets a runner. One failure is tolerated:
// GitHub has transient 5xx.
func (o *Orchestrator) Healthy() (bool, string) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	if o.snapshot.UpdatedAt.IsZero() {
		return false, "no reconcile has completed yet"
	}
	if o.snapshot.ConsecutiveFailures >= 3 {
		return false, fmt.Sprintf("%d consecutive reconcile failures: %s",
			o.snapshot.ConsecutiveFailures, o.snapshot.LastError)
	}

	stale := 5 * o.cfg.GitHub.PollInterval.Duration()
	if stale < time.Minute {
		stale = time.Minute
	}
	if age := time.Since(o.snapshot.UpdatedAt); age > stale {
		return false, fmt.Sprintf("last reconcile was %s ago", age.Round(time.Second))
	}
	return true, ""
}

// runnerSuffixLen is the length of the random hex suffix in a runner name.
const runnerSuffixLen = 8

// runnerName builds a unique runner name that encodes its pool, so orphaned
// registrations can be attributed back to a pool without extra state.
func runnerName(pool string) string {
	var b [runnerSuffixLen / 2]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("arc-%s-%s", pool, hex.EncodeToString(b[:]))
}

// belongsToPool reports whether a runner name was minted for the given pool.
//
// The exact-length hex suffix check matters: pools named "linux" and
// "linux-gpu" both produce names starting with "arc-linux-", and a loose prefix
// test would let the linux pool deregister the gpu pool's runners.
func belongsToPool(runner, pool string) bool {
	prefix := "arc-" + pool + "-"
	if !strings.HasPrefix(runner, prefix) {
		return false
	}
	suffix := runner[len(prefix):]
	if len(suffix) != runnerSuffixLen {
		return false
	}
	for _, c := range suffix {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func instanceKey(pool, runner string) string { return pool + "/" + runner }

func joinLines(s []string) string {
	out := ""
	for i, line := range s {
		if i > 0 {
			out += "\n  - "
		}
		out += line
	}
	return out
}
