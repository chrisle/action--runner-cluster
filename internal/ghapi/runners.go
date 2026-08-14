package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// DefaultRunnerGroupID is the id of the "Default" org runner group, which is
// fixed across every org. Custom groups need Enterprise Cloud or GHES.
const DefaultRunnerGroupID = 1

// Runner is a self-hosted runner as GitHub sees it.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Status string `json:"status"` // online | offline
	Busy   bool   `json:"busy"`
	Labels []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"labels"`

	// Repo is the repository the runner is registered against in
	// personal-account mode. Empty for org-level runners. Set by this
	// client, not by the API.
	Repo string `json:"-"`
}

// Online reports whether the runner is connected to GitHub.
func (r Runner) Online() bool { return r.Status == "online" }

// JITConfig is a just-in-time runner registration.
//
// JIT configs are strictly better than the classic config.sh + registration
// token flow here: the runner is pre-registered so we know its id before it
// starts, it is inherently single-job, and it leaves nothing on disk to clean up.
type JITConfig struct {
	Runner  Runner `json:"runner"`
	Encoded string `json:"encoded_jit_config"`
}

// ListRunners returns every self-hosted runner the cluster can see. In org
// mode that is one org-level listing and repos is ignored; in personal-account
// mode runners only exist per repo, so the given repos are each listed and the
// results merged, with Runner.Repo recording where each one lives.
func (c *Client) ListRunners(ctx context.Context, repos []Repo) ([]Runner, error) {
	if c.owner == "" {
		return c.listRunnersAt(ctx, fmt.Sprintf("/orgs/%s/actions/runners?per_page=100", c.org), "")
	}
	var all []Runner
	for _, repo := range repos {
		path := fmt.Sprintf("/repos/%s/%s/actions/runners?per_page=100", c.owner, repo.Name)
		runners, err := c.listRunnersAt(ctx, path, repo.Name)
		if err != nil {
			return nil, fmt.Errorf("list runners in %s: %w", repo.Name, err)
		}
		all = append(all, runners...)
	}
	return all, nil
}

func (c *Client) listRunnersAt(ctx context.Context, path, repo string) ([]Runner, error) {
	var all []Runner
	err := c.paginate(ctx, path, "", func(page []byte) error {
		var out struct {
			Runners []Runner `json:"runners"`
		}
		if err := json.Unmarshal(page, &out); err != nil {
			return fmt.Errorf("decode runners: %w", err)
		}
		for i := range out.Runners {
			out.Runners[i].Repo = repo
		}
		all = append(all, out.Runners...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

// GenerateJITConfig pre-registers a runner and returns its encoded config.
// workFolder is relative to the runner installation directory. repo is the
// repository to register against in personal-account mode; empty registers
// on the org.
func (c *Client) GenerateJITConfig(ctx context.Context, repo, name string, labels []string, groupID int64, workFolder string) (*JITConfig, error) {
	if groupID == 0 {
		groupID = DefaultRunnerGroupID
	}
	if workFolder == "" {
		workFolder = "_work"
	}
	body := map[string]any{
		"name":            name,
		"runner_group_id": groupID,
		"labels":          labels,
		"work_folder":     workFolder,
	}
	path := fmt.Sprintf("/orgs/%s/actions/runners/generate-jitconfig", c.org)
	if repo != "" {
		// Repo-level registration also takes runner_group_id and accepts the
		// Default group's fixed id 1.
		path = fmt.Sprintf("/repos/%s/%s/actions/runners/generate-jitconfig", c.entity(), repo)
	}
	var out JITConfig
	if err := c.postJSON(ctx, path, body, &out); err != nil {
		return nil, fmt.Errorf("generate jit config for %s: %w", name, err)
	}
	if out.Encoded == "" {
		return nil, fmt.Errorf("generate jit config for %s: empty config returned", name)
	}
	out.Runner.Repo = repo
	return &out, nil
}

// RemoveRunner deregisters a runner. repo is where the runner is registered
// in personal-account mode; empty means the org.
//
// GitHub refuses with 422 while the runner is executing a job, which is the
// safety net that keeps a scale-down from killing running work. Callers should
// treat ErrRunnerBusy as "try again later", not as a failure.
func (c *Client) RemoveRunner(ctx context.Context, repo string, id int64) error {
	path := fmt.Sprintf("/orgs/%s/actions/runners/%d", c.org, id)
	if repo != "" {
		path = fmt.Sprintf("/repos/%s/%s/actions/runners/%d", c.entity(), repo, id)
	}
	_, _, err := c.request(ctx, http.MethodDelete, path, nil, "")
	switch {
	case err == nil:
		return nil
	case IsNotFound(err):
		return nil // already gone; the desired state holds
	default:
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
			return ErrRunnerBusy
		}
		return err
	}
}

// ErrRunnerBusy is returned when GitHub refuses to remove a runner mid-job.
var ErrRunnerBusy = errors.New("runner is executing a job")

// RunnerGroupID resolves a runner group name to its id. An empty name yields
// the Default group. Results are cached; group ids do not change.
func (c *Client) RunnerGroupID(ctx context.Context, name string) (int64, error) {
	if name == "" || strings.EqualFold(name, "default") {
		return DefaultRunnerGroupID, nil
	}
	if c.owner != "" {
		return 0, fmt.Errorf("runner group %q: personal accounts have no runner groups", name)
	}

	c.groupMu.Lock()
	if id, ok := c.groups[name]; ok {
		c.groupMu.Unlock()
		return id, nil
	}
	c.groupMu.Unlock()

	var found int64
	path := fmt.Sprintf("/orgs/%s/actions/runner-groups?per_page=100", c.org)
	err := c.paginate(ctx, path, "", func(page []byte) error {
		var out struct {
			RunnerGroups []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"runner_groups"`
		}
		if err := json.Unmarshal(page, &out); err != nil {
			return err
		}
		for _, g := range out.RunnerGroups {
			if strings.EqualFold(g.Name, name) {
				found = g.ID
			}
		}
		return nil
	})
	if err != nil {
		if IsNotFound(err) {
			return 0, fmt.Errorf("runner group %q: custom runner groups require GitHub Enterprise "+
				"Cloud or GHES; leave runner_group empty to use Default", name)
		}
		return 0, err
	}
	if found == 0 {
		return 0, fmt.Errorf("runner group %q not found in org %s", name, c.org)
	}

	c.groupMu.Lock()
	if c.groups == nil {
		c.groups = map[string]int64{}
	}
	c.groups[name] = found
	c.groupMu.Unlock()
	return found, nil
}
