package orchestrator

import (
	"sort"
	"strings"

	"github.com/chrisle/action-runner-cluster/internal/config"
	"github.com/chrisle/action-runner-cluster/internal/ghapi"
)

// PoolMatcher decides which pool can serve a job.
type PoolMatcher struct {
	pools []*config.Pool
	// labelSets is pool name -> lowercased label set.
	labelSets map[string]map[string]bool
}

// NewPoolMatcher precomputes label sets for matching.
func NewPoolMatcher(pools []*config.Pool) *PoolMatcher {
	m := &PoolMatcher{pools: pools, labelSets: make(map[string]map[string]bool, len(pools))}
	for _, p := range pools {
		set := make(map[string]bool, len(p.Labels)+1)
		for _, l := range p.Labels {
			set[strings.ToLower(l)] = true
		}
		// Every self-hosted runner carries "self-hosted" implicitly, whether or
		// not the config lists it.
		set["self-hosted"] = true
		m.labelSets[p.Name] = set
	}
	return m
}

// CanServe reports whether a pool satisfies a job's runs-on labels.
//
// GitHub routes a job to a runner only if the runner carries every label the
// job asked for. Extra labels on the runner are fine. So the pool's label set
// must be a superset of the job's.
func (m *PoolMatcher) CanServe(pool *config.Pool, job ghapi.Job) bool {
	set := m.labelSets[pool.Name]
	if set == nil {
		return false
	}
	for _, want := range job.Labels {
		if !set[strings.ToLower(want)] {
			return false
		}
	}
	return true
}

// Candidates returns pools that can serve the job, tightest fit first.
//
// "Tightest" means the fewest labels beyond what the job asked for. Given a
// generic linux pool and a linux+gpu pool, a plain `runs-on: [self-hosted,
// linux]` job goes to the generic one, leaving the scarcer GPU machines free
// for jobs that actually asked for them.
func (m *PoolMatcher) Candidates(job ghapi.Job) []*config.Pool {
	var out []*config.Pool
	for _, p := range m.pools {
		if m.CanServe(p, job) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return len(m.labelSets[out[i].Name]) < len(m.labelSets[out[j].Name])
	})
	return out
}

// Demand is the number of queued jobs assigned to each pool.
type Demand struct {
	// ByPool maps pool name to the count of queued jobs assigned to it.
	ByPool map[string]int
	// Jobs maps pool name to the jobs assigned to it, for status output.
	Jobs map[string][]ghapi.Job
	// Unassigned holds queued jobs no pool can serve.
	Unassigned []ghapi.Job
}

// AssignDemand distributes queued jobs across pools.
//
// Assignment respects each pool's remaining headroom (max minus what is already
// running) so a job that a saturated pool cannot take spills over to another
// pool that shares its labels, instead of being counted against a pool that
// will never grow to serve it.
func (m *PoolMatcher) AssignDemand(jobs []ghapi.Job, live map[string]int, limits map[string]Limits) Demand {
	d := Demand{
		ByPool: make(map[string]int, len(m.pools)),
		Jobs:   make(map[string][]ghapi.Job, len(m.pools)),
	}

	// headroom is how many more runners each pool may create.
	headroom := make(map[string]int, len(m.pools))
	for _, p := range m.pools {
		lim := limits[p.Name]
		headroom[p.Name] = lim.Max - live[p.Name]
		if headroom[p.Name] < 0 {
			headroom[p.Name] = 0
		}
	}

	for _, job := range jobs {
		candidates := m.Candidates(job)
		if len(candidates) == 0 {
			d.Unassigned = append(d.Unassigned, job)
			continue
		}

		placed := false
		for _, p := range candidates {
			if headroom[p.Name] <= 0 {
				continue
			}
			headroom[p.Name]--
			d.ByPool[p.Name]++
			d.Jobs[p.Name] = append(d.Jobs[p.Name], job)
			placed = true
			break
		}
		if !placed {
			// Every eligible pool is at max. The job still counts as demand on
			// the tightest-fit pool so `arc status` shows the queue backing up
			// rather than silently reporting nothing to do.
			p := candidates[0]
			d.ByPool[p.Name]++
			d.Jobs[p.Name] = append(d.Jobs[p.Name], job)
		}
	}
	return d
}

// Limits are the effective min and max for a pool, after any live override.
type Limits struct {
	Min int
	Max int
}

// Decision is what the reconciler should do to one pool this tick.
type Decision struct {
	Pool string
	// Desired is the target number of live runners.
	Desired int
	// Create is how many new runners to launch.
	Create int
	// Cull is how many surplus idle runners to remove.
	Cull int
	// Reason explains the decision, for logs and status.
	Reason string
}

// PoolState is the observed state of a pool this tick.
type PoolState struct {
	// Live is every instance the provider currently owns, minus exited ones.
	Live int
	// Busy is how many live runners GitHub reports as executing a job.
	Busy int
	// Idle is live runners that are online with GitHub and not busy.
	Idle int
	// Starting is instances that exist but have not yet appeared online.
	Starting int
}

// Plan computes the scaling decision for one pool.
//
// The target is:
//
//	desired = clamp(max(min, busy + queued), min, max)
//
// Runners are ephemeral: each takes exactly one job and exits. So a queued job
// needs a runner that is idle *now*, and a busy runner cannot help it. Counting
// busy + queued keeps every in-flight job's runner alive while still providing
// one fresh runner per waiting job. The min floor keeps warm capacity so the
// first job of the day doesn't pay full startup cost.
func Plan(pool string, st PoolState, demand int, lim Limits) Decision {
	desired := st.Busy + demand
	if desired < lim.Min {
		desired = lim.Min
	}
	if desired > lim.Max {
		desired = lim.Max
	}

	d := Decision{Pool: pool, Desired: desired}

	switch {
	case st.Live < desired:
		d.Create = desired - st.Live
		d.Reason = "scaling up for queued jobs"
		if demand == 0 {
			d.Reason = "topping up to minimum"
		}
	case st.Live > desired:
		// Only ever remove runners that are idle. Busy ones are executing jobs,
		// and starting ones have not had a chance to pick one up yet.
		d.Cull = st.Live - desired
		if d.Cull > st.Idle {
			d.Cull = st.Idle
		}
		if d.Cull > 0 {
			d.Reason = "culling idle surplus"
		}
	default:
		d.Reason = "at target"
	}

	return d
}
