package ghapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// WebhookPath is the base URL path arc's webhook endpoint serves. Each host
// appends its host id (WebhookPath + "/" + hostid), and that full path is the
// marker identifying which of an account's webhooks belong to which arc host
// — so re-registration updates this host's hook without stealing another's.
const WebhookPath = "/arc/webhook"

// AccountType reports whether login is a "User" or an "Organization".
func (c *Client) AccountType(ctx context.Context, login string) (string, error) {
	var out struct {
		Type string `json:"type"`
	}
	if _, err := c.getJSON(ctx, "/users/"+login, "", &out); err != nil {
		return "", fmt.Errorf("look up account %s: %w", login, err)
	}
	return out.Type, nil
}

type hookInfo struct {
	ID     int64 `json:"id"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

// EnsureWebhook points this host's arc webhook for repo (or the org, when
// repo is empty) at url, creating or updating as needed. Ours is the hook
// whose URL ends in url's path — tunnel hostnames change every start, the
// path never does. The secret rotates on every arc start, so an existing
// hook is always rewritten.
func (c *Client) EnsureWebhook(ctx context.Context, repo, url, secret string) error {
	marker := url
	if i := strings.Index(url, WebhookPath); i >= 0 {
		marker = url[i:]
	}

	base := fmt.Sprintf("/orgs/%s/hooks", c.org)
	if repo != "" {
		base = fmt.Sprintf("/repos/%s/%s/hooks", c.entity(), repo)
	}

	var existing []hookInfo
	err := c.paginate(ctx, base+"?per_page=100", "", func(page []byte) error {
		var hooks []hookInfo
		if err := json.Unmarshal(page, &hooks); err != nil {
			return fmt.Errorf("decode hooks: %w", err)
		}
		existing = append(existing, hooks...)
		return nil
	})
	if err != nil {
		return fmt.Errorf("list hooks: %w", err)
	}

	body := map[string]any{
		"active": true,
		"events": []string{"workflow_job"},
		"config": map[string]any{
			"url":          url,
			"content_type": "json",
			"secret":       secret,
		},
	}

	for _, h := range existing {
		if !strings.HasSuffix(h.Config.URL, marker) {
			continue
		}
		path := fmt.Sprintf("%s/%d", base, h.ID)
		if _, _, err := c.request(ctx, http.MethodPatch, path, body, ""); err != nil {
			return fmt.Errorf("update hook %d: %w", h.ID, err)
		}
		return nil
	}

	body["name"] = "web"
	if _, _, err := c.request(ctx, http.MethodPost, base, body, ""); err != nil {
		return fmt.Errorf("create hook: %w", err)
	}
	return nil
}
