// Command arc is a GitHub Actions self-hosted runner cluster orchestrator.
//
// It watches an organization for queued jobs and keeps each pool of ephemeral
// runners between its configured minimum and maximum, creating runners as work
// arrives and destroying them completely when it drains away.
package main

import (
	"fmt"
	"os"
)

// version is stamped at build time with -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `arc — GitHub Actions runner cluster orchestrator

Usage:
  arc <command> [flags]

Commands:
  run       Start the orchestrator
  config    Create or edit the configuration with a wizard
  status    Show pools and runners
  scale     Change a pool's min/max at runtime
  drain     Stop a pool from creating runners
  resume    Undo a drain
  logs      Show a runner instance's output
  doctor    Check configuration, credentials and providers
  version   Print version information

Run "arc <command> -h" for command flags.

Configuration is read from, in order of precedence:
  -config <path>
  $ARC_CONFIG
  ./arc.yaml
  ~/.config/arc/arc.yaml   (written by "arc config")
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "config":
		err = cmdConfig(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "scale":
		err = cmdScale(os.Args[2:])
	case "drain":
		err = cmdDrain(os.Args[2:], true)
	case "resume":
		err = cmdDrain(os.Args[2:], false)
	case "logs":
		err = cmdLogs(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("arc %s (commit %s, built %s)\n", version, commit, date)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "arc: %v\n", err)
		os.Exit(1)
	}
}
