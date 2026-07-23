// Command odin runs an agent profile.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return // clean shutdown on SIGTERM
		}
		fmt.Fprintln(os.Stderr, "odin:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("a subcommand is required")
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "init":
		return cmdInit(args)
	case "run":
		return cmdRun(args)
	case "once":
		return cmdOnce(args)
	case "ask":
		return cmdAsk(args)
	case "status":
		return cmdStatus(args)
	case "auth":
		return cmdAuth(args)
	case "usage":
		return cmdUsage(args)
	case "models":
		return cmdModels(args)
	case "verify":
		return cmdVerify(args)
	case "validate":
		return cmdValidate(args)
	case "timezone":
		return cmdTimezone(args)
	case "backup":
		return cmdBackup(args)
	case "restore":
		return cmdRestore(args)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `odin - configurable agent

  odin init    --profile NAME --timezone ZONE
  odin run     --profile NAME   scheduler + telegram gateway
  odin once    --profile NAME --job NAME
  odin ask     --profile NAME "question"
  odin status  --profile NAME
  odin auth    --profile NAME --provider NAME [--account NAME]
  odin usage   --profile NAME [--provider NAME]
  odin models  --profile NAME [--provider NAME]
  odin verify  --profile NAME --provider NAME
  odin validate --profile NAME
  odin timezone --profile NAME [get|set ZONE|reset]
  odin backup  --profile NAME [--keep N]
  odin restore --profile NAME --from FILE

Common flags:
  --root DIR    profile root (default $ODIN_ROOT, else current directory)
  --verbose     debug logging
`)
}
