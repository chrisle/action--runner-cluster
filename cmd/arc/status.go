package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/orchestrator"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to arc.yaml")
	asJSON := fs.Bool("json", false, "emit the raw status document")
	wide := fs.Bool("wide", false, "list individual runner instances")
	watch := fs.Duration("watch", 0, "refresh on an interval, e.g. 2s")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	client := newControlClient(cfg)

	show := func() error {
		var snap orchestrator.Snapshot
		if err := client.do(context.Background(), "GET", "/v1/status", nil, &snap); err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(snap)
		}
		printStatus(snap, *wide)
		return nil
	}

	if *watch <= 0 {
		return show()
	}

	for {
		// Clear and home the cursor so the table refreshes in place.
		fmt.Print("\033[H\033[2J")
		if err := show(); err != nil {
			return err
		}
		time.Sleep(*watch)
	}
}

func printStatus(snap orchestrator.Snapshot, wide bool) {
	if snap.UpdatedAt.IsZero() {
		fmt.Println("No reconcile has completed yet.")
		return
	}

	age := time.Since(snap.UpdatedAt).Round(time.Second)
	fmt.Printf("org %s · %d repos watched · updated %s ago\n", snap.Org, snap.ReposWatched, age)
	if snap.RateLimit.Limit > 0 {
		fmt.Printf("github rate limit: %d/%d remaining, resets %s\n",
			snap.RateLimit.Remaining, snap.RateLimit.Limit,
			time.Until(snap.RateLimit.Reset).Round(time.Second))
	}
	fmt.Println()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POOL\tPROVIDER\tMIN\tMAX\tLIVE\tBUSY\tIDLE\tSTARTING\tQUEUED\tDESIRED\tSTATE")
	for _, p := range snap.Pools {
		st := p.Reason
		switch {
		case p.Error != "":
			st = "ERROR: " + truncate(p.Error, 48)
		case p.Drained:
			st = "drained"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
			p.Name, p.Provider, p.Min, p.Max, p.Live, p.Busy, p.Idle, p.Starting, p.Queued, p.Desired, st)
	}
	tw.Flush()

	if wide {
		for _, p := range snap.Pools {
			if len(p.Instances) == 0 {
				continue
			}
			fmt.Printf("\n%s (%s)\n", p.Name, strings.Join(p.Labels, ", "))
			itw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(itw, "  RUNNER\tINSTANCE\tSTATE\tAGE\tDETAIL")
			for _, inst := range p.Instances {
				st := inst.State
				switch {
				case inst.Busy:
					st = "busy"
				case inst.Online:
					st = "idle"
				}
				fmt.Fprintf(itw, "  %s\t%s\t%s\t%s\t%s\n",
					inst.Name, shortID(inst.ID), st, inst.Age, inst.Detail)
			}
			itw.Flush()
		}
	}

	if len(snap.Unassigned) > 0 {
		fmt.Printf("\n%d queued job(s) match no pool:\n", len(snap.Unassigned))
		for _, j := range snap.Unassigned {
			fmt.Printf("  %s · %s · runs-on: [%s]\n", j.Repo, j.Name, strings.Join(j.Labels, ", "))
		}
		fmt.Println("\nAdd a pool whose labels cover these, or fix the workflow's runs-on.")
	}

	if snap.LastError != "" {
		fmt.Printf("\nlast error: %s\n", snap.LastError)
	}
}

func cmdScale(args []string) error {
	fs := flag.NewFlagSet("scale", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to arc.yaml")
	min := fs.String("min", "", "new minimum runners")
	max := fs.String("max", "", "new maximum runners")
	reset := fs.Bool("reset", false, "clear the override and use the config values")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: arc scale <pool> [-min N] [-max N] [-reset]\n\n"+
			"Changes take effect on the next reconcile and survive a restart.\n"+
			"Leaving one of -min/-max unset keeps its current value.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return fmt.Errorf("expected exactly one pool name")
	}
	pool := positional[0]

	body := map[string]any{"reset": *reset}
	if *min != "" {
		n, err := strconv.Atoi(*min)
		if err != nil {
			return fmt.Errorf("-min: %w", err)
		}
		body["min"] = n
	}
	if *max != "" {
		n, err := strconv.Atoi(*max)
		if err != nil {
			return fmt.Errorf("-max: %w", err)
		}
		body["max"] = n
	}
	if !*reset && body["min"] == nil && body["max"] == nil {
		fs.Usage()
		return fmt.Errorf("specify -min, -max, or -reset")
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	var out struct {
		Pool   string `json:"pool"`
		Min    int    `json:"min"`
		Max    int    `json:"max"`
		Source string `json:"source"`
	}
	client := newControlClient(cfg)
	path := "/v1/pools/" + url.PathEscape(pool) + "/scale"
	if err := client.do(context.Background(), "POST", path, body, &out); err != nil {
		return err
	}
	fmt.Printf("pool %s: min=%d max=%d (%s)\n", out.Pool, out.Min, out.Max, out.Source)
	return nil
}

func cmdDrain(args []string, drain bool) error {
	name := "drain"
	if !drain {
		name = "resume"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to arc.yaml")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: arc %s <pool>", name)
	}
	pool := positional[0]

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	path := "/v1/pools/" + url.PathEscape(pool) + "/" + name
	if err := newControlClient(cfg).do(context.Background(), "POST", path, nil, nil); err != nil {
		return err
	}
	if drain {
		fmt.Printf("pool %s drained: no new runners will be created. "+
			"Jobs already running are left to finish.\n", pool)
	} else {
		fmt.Printf("pool %s resumed.\n", pool)
	}
	return nil
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to arc.yaml")
	lines := fs.Int("n", 200, "number of lines to show")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: arc logs <pool> <instance-id>\n\n"+
			"Instance ids come from `arc status -wide`.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		fs.Usage()
		return fmt.Errorf("expected a pool and an instance id")
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	var out string
	path := fmt.Sprintf("/v1/pools/%s/instances/%s/logs?lines=%d",
		url.PathEscape(positional[0]), url.PathEscape(positional[1]), *lines)
	if err := newControlClient(cfg).do(context.Background(), "GET", path, nil, &out); err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// shortID abbreviates a docker container id; process instance ids are already
// short and are left alone.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
