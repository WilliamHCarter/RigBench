package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/report"
)

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	runDir := fs.String("run", "", "run directory containing request.jsonl (required)")
	fixtureDir := fs.String("fixture", "fixtures/zig-playback-v1", "fixture directory, for its unmeasured list")
	fs.Parse(args)
	if *runDir == "" {
		return fmt.Errorf("-run is required")
	}

	records, err := metrics.ReadRecords(filepath.Join(*runDir, "request.jsonl"))
	if err != nil {
		return err
	}
	run := &metrics.RunIdentity{RunID: filepath.Base(*runDir)}
	if b, err := os.ReadFile(filepath.Join(*runDir, "run.json")); err == nil {
		_ = unmarshal(b, run)
	}

	var unmeasured []string
	if f, err := config.LoadFixture(*fixtureDir); err == nil {
		unmeasured = f.Unmeasured
	}

	cells := report.Aggregate(records)
	if err := report.WriteCSV(filepath.Join(*runDir, "summary.csv"), cells); err != nil {
		return err
	}
	if err := report.WriteSummary(filepath.Join(*runDir, "summary.md"), report.SummaryInput{
		Run: run, Records: records, Cells: cells, Unmeasured: unmeasured,
		Caveats: []string{"Re-rendered from request.jsonl; the fixture self-check " +
			"sections are absent because they are produced by a run, not by a report."},
	}); err != nil {
		return err
	}
	fmt.Printf("re-rendered %d record(s) into %s\n", len(records), filepath.Join(*runDir, "summary.md"))
	return nil
}
