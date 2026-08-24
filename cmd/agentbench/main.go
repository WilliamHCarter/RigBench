// Command agentbench is the AgentBench-01 runner.
//
// Subcommands:
//
//	verify-fixture  prove the fixture's controls fire before trusting any result
//	run             run one or more engine configs against a fixture lane
//	mock            serve the deterministic in-repo mock endpoint
//	report          re-render summary.md and summary.csv from a run's JSONL
//	smoke           the v0.1 acceptance gate, end to end, against the mock
package main

import (
	"fmt"
	"os"
)

const usage = `agentbench — AgentBench-01 runner

usage:
  agentbench verify-fixture [flags]   prove the fixture's anti-vacuity controls fire
  agentbench run            [flags]   run engine configs against a fixture lane
  agentbench mock           [flags]   serve the deterministic mock endpoint
  agentbench report         [flags]   re-render a run's summary from its JSONL
  agentbench smoke          [flags]   v0.1 acceptance gate, end to end, on the mock

run "agentbench <subcommand> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "verify-fixture":
		err = cmdVerifyFixture(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "mock":
		err = cmdMock(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "smoke":
		err = cmdSmoke(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentbench: %v\n", err)
		os.Exit(1)
	}
}
