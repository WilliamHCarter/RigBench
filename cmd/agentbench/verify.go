package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
)

func cmdVerifyFixture(args []string) error {
	fs := flag.NewFlagSet("verify-fixture", flag.ExitOnError)
	fixtureDir := fs.String("fixture", "fixtures/zig-playback-v1", "fixture directory")
	workDir := fs.String("work", "", "staging directory (default: a temp dir)")
	only := fs.String("only", "", "comma-separated mutant ids to run")
	skipTripwire := fs.Bool("skip-tripwire", false, "skip the hidden-suite-executes proof")
	keep := fs.Bool("keep", false, "keep staged worktrees for inspection")
	fs.Parse(args)

	f, err := config.LoadFixture(*fixtureDir)
	if err != nil {
		return err
	}
	work := *workDir
	if work == "" {
		work, err = os.MkdirTemp("", "agentbench-verify-")
		if err != nil {
			return err
		}
		if !*keep {
			defer os.RemoveAll(work)
		}
	}
	ctx := context.Background()

	fmt.Printf("fixture %s v%s  (%s)\n", f.ID, f.Version, f.Dir())
	fmt.Printf("staging in %s\n\n", work)

	// 1. The gold path must be green before a control means anything.
	fmt.Println("== reference solution ==")
	ref := filepath.Join(work, "reference")
	wt, err := executor.Stage(ctx, f, ref, true)
	if err != nil {
		return err
	}
	if err := wt.InjectHidden(ctx); err != nil {
		return err
	}
	zig := executor.ZigVersion(ctx, ref)
	fmt.Printf("zig resolved from build.zig.zon: %s\n", orUnknown(zig))

	refOK := true
	for _, rung := range []struct {
		name string
		argv []string
	}{
		{"build", f.Commands.Build},
		{"visible", f.Commands.Visible},
		{"hidden", f.Commands.Hidden},
		{"release", f.Commands.Release},
	} {
		if len(rung.argv) == 0 {
			continue
		}
		r := executor.Run(ctx, ref, rung.argv, f.CommandTimeout())
		p, _ := executor.ParseZigTestCounts(r.Combined())
		count := ""
		if p != nil {
			count = fmt.Sprintf("  %d tests passed", *p)
		}
		switch {
		case r.Unavailable:
			fmt.Printf("  %-8s SKIPPED  %s\n", rung.name, r.Err)
			refOK = false
		case r.OK():
			fmt.Printf("  %-8s exit 0%s\n", rung.name, count)
		default:
			fmt.Printf("  %-8s exit %d\n%s\n", rung.name, r.ExitCode, indent(r.Combined()))
			refOK = false
		}
	}
	if !refOK {
		return fmt.Errorf("the reference solution does not pass its own rungs; " +
			"repair the fixture before interpreting any control")
	}

	// 2. Prove the hidden suite executes rather than merely compiles.
	if !*skipTripwire {
		fmt.Println("\n== hidden suite executes ==")
		tw, err := executor.VerifyHiddenSuiteRuns(ctx, f, work)
		if err != nil {
			return err
		}
		bad := 0
		for _, t := range tw {
			if t.Fired {
				fmt.Printf("  %-32s tripwire fired\n", t.File)
			} else {
				fmt.Printf("  %-32s NOT REACHED — %s\n", t.File, t.Detail)
				bad++
			}
		}
		if bad > 0 {
			return fmt.Errorf("%d hidden file(s) are compiled but never run", bad)
		}
	}

	// 3. The anti-vacuity control set.
	fmt.Println("\n== anti-vacuity controls ==")
	var ids []string
	if *only != "" {
		ids = strings.Split(*only, ",")
	}
	outcomes, err := executor.VerifyMutants(ctx, f, work, filepath.Join(work, "logs"), ids)
	if err != nil {
		return err
	}
	bad, invisible := 0, 0
	for _, m := range outcomes {
		status := "held"
		if !m.OK {
			status = "BROKEN: " + strings.Join(m.Reasons, "; ")
			bad++
		}
		flag := " "
		if m.OK && m.Got.Visible == "pass" && m.Got.Hidden == "fail" {
			flag = "*"
			invisible++
		}
		fmt.Printf("  %s %-38s build=%-4s visible=%-4s hidden=%-4s  %s\n",
			flag, m.ID, m.Got.Build, m.Got.Visible, m.Got.Hidden, status)
	}
	fmt.Printf("\n  %d control(s), %d broken\n", len(outcomes), bad)
	fmt.Printf("  * = passes the visible suite and is caught only by the hidden suite (%d)\n", invisible)
	if bad > 0 {
		return fmt.Errorf("%d anti-vacuity control(s) did not behave as declared", bad)
	}
	if invisible == 0 && len(ids) == 0 {
		return fmt.Errorf("no control is invisible to the visible suite; " +
			"the hidden suite may only be restating tests the candidate already has")
	}
	fmt.Println("\nfixture OK")
	return nil
}

func indent(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("      " + l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
