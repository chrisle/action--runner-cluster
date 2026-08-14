package ghapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Repo is a repository the orchestrator may poll.
type Repo struct {
	Name     string    `json:"name"`
	FullName string    `json:"full_name"`
	Archived bool      `json:"archived"`
	Disabled bool      `json:"disabled"`
	Fork     bool      `json:"fork"`
	PushedAt time.Time `json:"pushed_at"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Job is a workflow job that is waiting for a runner.
type Job struct {
	ID       int64     `json:"id"`
	RunID    int64     `json:"run_id"`
	Repo     string    `json:"repo"`
	Name     string    `json:"name"`
	Labels   []string  `json:"labels"`
	Status   string    `json:"status"` // queued | in_progress | completed
	Started  time.Time `json:"started_at"`
	QueuedAt time.Time `json:"queued_at"`
}

// SelfHosted reports whether the job asks for a self-hosted runner.
//
// A job that only names GitHub-hosted labels (ubuntu-latest and friends) must
// not drive our scaling, or every hosted job in the org would spin up local
// runners that GitHub never routes work to.
func (j Job) SelfHosted() bool {
	if len(j.Labels) == 0 {
		return false
	}
	for _, l := range j.Labels {
		if strings.EqualFold(l, "self-hosted") {
			return true
		}
	}
	// Some workflows use a bare custom label without "self-hosted". Treat any
	// label that isn't a known GitHub-hosted image as self-hosted.
	for _, l := range j.Labels {
		if !githubHostedLabel(l) {
			return true
		}
	}
	return false
}

// githubHostedLabel reports whether a label names a GitHub-hosted image.
func githubHostedLabel(l string) bool {
	l = strings.ToLower(l)
	for _, prefix := range []string{
		"ubuntu-", "windows-", "macos-", "ubuntu_", "windows_", "macos_",
	} {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	switch l {
	case "ubuntu", "windows", "macos":
		return true
	}
	return false
}

// ListRepos returns the account's repositories, filtered by the config's rules.
func (c *Client) ListRepos(ctx context.Context, filter RepoFilterOpts) ([]Repo, error) {
	var all []Repo
	// sort=pushed puts recently active repos first, so ActiveWithin can stop
	// paginating early instead of walking the whole account.
	p := fmt.Sprintf("/orgs/%s/repos?per_page=100&type=all&sort=pushed&direction=desc", c.org)
	if c.owner != "" {
		// /users/{owner}/repos only returns public repos, so personal mode
		// lists the authenticated user's own repos and then filters to the
		// configured owner as a guard against a token for the wrong account.
		p = "/user/repos?per_page=100&affiliation=owner&sort=pushed&direction=desc"
	}

	cutoff := time.Time{}
	if filter.ActiveWithin > 0 {
		cutoff = time.Now().Add(-filter.ActiveWithin)
	}

	stop := false
	err := c.paginate(ctx, p, "", func(page []byte) error {
		if stop {
			return nil
		}
		var repos []Repo
		if err := json.Unmarshal(page, &repos); err != nil {
			return fmt.Errorf("decode repos: %w", err)
		}
		for _, r := range repos {
			if !cutoff.IsZero() && r.PushedAt.Before(cutoff) {
				// Sorted by push time, so everything after this is older too.
				stop = true
				break
			}
			if c.owner != "" && !strings.EqualFold(r.Owner.Login, c.owner) {
				continue
			}
			// Forks rarely run their own Actions; watching them multiplies
			// API cost for nothing. An explicit include list overrides this.
			if c.owner != "" && r.Fork && len(filter.Include) == 0 {
				continue
			}
			if r.Disabled {
				continue
			}
			if r.Archived && !filter.Archived {
				continue
			}
			if !filter.match(r.Name) {
				continue
			}
			all = append(all, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

// RepoFilterOpts is the resolved repo filter.
type RepoFilterOpts struct {
	Include      []string
	Exclude      []string
	ActiveWithin time.Duration
	Archived     bool
}

func (f RepoFilterOpts) match(name string) bool {
	if len(f.Include) > 0 && !matchAny(f.Include, name) {
		return false
	}
	return !matchAny(f.Exclude, name)
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if strings.EqualFold(p, name) {
			return true
		}
		if ok, err := path.Match(strings.ToLower(p), strings.ToLower(name)); err == nil && ok {
			return true
		}
	}
	return false
}

// PendingJobs returns every self-hosted job across the given repos that is
// waiting for a runner.
//
// There is no org-wide "list queued jobs" endpoint, so this walks each repo's
// active workflow runs and inspects their jobs. Every request is conditional on
// an ETag: repos with no new activity answer 304, which GitHub does not charge
// against the rate limit, so polling a large org stays affordable.
func (c *Client) PendingJobs(ctx context.Context, repos []Repo, concurrency int) ([]Job, error) {
	if concurrency < 1 {
		concurrency = 8
	}

	type result struct {
		jobs []Job
		err  error
	}

	sem := make(chan struct{}, concurrency)
	results := make([]result, len(repos))
	var wg sync.WaitGroup

	for i, repo := range repos {
		wg.Add(1)
		go func(i int, repo Repo) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = result{err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			jobs, err := c.repoPendingJobs(ctx, repo)
			results[i] = result{jobs: jobs, err: err}
		}(i, repo)
	}
	wg.Wait()

	var (
		jobs []Job
		errs []string
	)
	for i, r := range results {
		if r.err != nil {
			// One broken repo (deleted, permissions changed) must not blind the
			// orchestrator to demand in every other repo.
			errs = append(errs, fmt.Sprintf("%s: %v", repos[i].Name, r.err))
			continue
		}
		jobs = append(jobs, r.jobs...)
	}

	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })

	if len(errs) > 0 && len(errs) == len(repos) {
		return jobs, fmt.Errorf("all repo polls failed: %s", strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		return jobs, &PartialError{Failures: errs}
	}
	return jobs, nil
}

// PartialError reports that some repos failed while others succeeded.
type PartialError struct{ Failures []string }

func (e *PartialError) Error() string {
	return fmt.Sprintf("%d repo poll(s) failed: %s", len(e.Failures), strings.Join(e.Failures, "; "))
}

func (c *Client) repoPendingJobs(ctx context.Context, repo Repo) ([]Job, error) {
	runIDs := map[int64]bool{}

	// A run that is "in_progress" can still hold queued jobs: matrix legs and
	// jobs gated on `needs:` enter the queue after the run has already started.
	// Polling only status=queued would miss all of them.
	for _, status := range []string{"queued", "in_progress"} {
		p := fmt.Sprintf("/repos/%s/%s/actions/runs?status=%s&per_page=100&exclude_pull_requests=true",
			c.entity(), repo.Name, status)
		key := fmt.Sprintf("runs:%s:%s", repo.Name, status)

		var out struct {
			WorkflowRuns []struct {
				ID     int64  `json:"id"`
				Status string `json:"status"`
			} `json:"workflow_runs"`
		}
		if _, err := c.getJSON(ctx, p, key, &out); err != nil {
			return nil, err
		}
		for _, r := range out.WorkflowRuns {
			runIDs[r.ID] = true
		}
	}
	if len(runIDs) == 0 {
		return nil, nil
	}

	ids := make([]int64, 0, len(runIDs))
	for id := range runIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var jobs []Job
	for _, runID := range ids {
		p := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=100", c.entity(), repo.Name, runID)
		key := fmt.Sprintf("jobs:%s:%d", repo.Name, runID)

		var out struct {
			Jobs []struct {
				ID          int64     `json:"id"`
				RunID       int64     `json:"run_id"`
				Name        string    `json:"name"`
				Status      string    `json:"status"`
				Labels      []string  `json:"labels"`
				StartedAt   time.Time `json:"started_at"`
				CreatedAt   time.Time `json:"created_at"`
				RunnerName  string    `json:"runner_name"`
				RunnerGroup string    `json:"runner_group_name"`
			} `json:"jobs"`
		}
		if _, err := c.getJSON(ctx, p, key, &out); err != nil {
			if IsNotFound(err) {
				continue // run vanished between the two calls
			}
			return nil, err
		}

		for _, j := range out.Jobs {
			if j.Status != "queued" {
				continue
			}
			job := Job{
				ID:       j.ID,
				RunID:    j.RunID,
				Repo:     repo.Name,
				Name:     j.Name,
				Labels:   j.Labels,
				Status:   j.Status,
				Started:  j.StartedAt,
				QueuedAt: j.CreatedAt,
			}
			if !job.SelfHosted() {
				continue
			}
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}
