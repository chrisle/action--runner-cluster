package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
)

// parsePositional parses flags that may be interleaved with positional
// arguments, and returns the positional ones.
//
// Go's flag package stops parsing at the first non-flag token. Left alone,
// `arc scale linux -max 12` would parse zero flags and silently do nothing
// the user asked for — the worst possible failure for a scaling command.
// Alternating between collecting positionals and parsing the remainder makes
// every argument order work.
func parsePositional(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			positional = append(positional, args[0])
			args = args[1:]
		}
		if len(args) == 0 {
			return positional, nil
		}

		before := len(args)
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()

		// Guard against spinning if Parse consumed nothing.
		if len(args) >= before {
			return append(positional, args...), nil
		}
	}
}

// controlClient talks to a running orchestrator's control API.
type controlClient struct {
	base  string
	token string
	http  *http.Client
}

func newControlClient(cfg *config.Config) *controlClient {
	addr := cfg.Server.Addr
	// A bare :8730 or 0.0.0.0:8730 is a listen address, not something to dial.
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	addr = strings.Replace(addr, "0.0.0.0:", "127.0.0.1:", 1)

	return &controlClient{
		base:  "http://" + addr,
		token: cfg.Server.Token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *controlClient) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the orchestrator at %s: %w\n"+
			"Is `arc run` running?", c.base, err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}

	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(data)
		return nil
	}
	return json.Unmarshal(data, out)
}
