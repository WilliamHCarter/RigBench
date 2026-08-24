package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/prompt"
	"github.com/WilliamHCarter/RigBench/internal/report"
	"github.com/WilliamHCarter/RigBench/internal/runner"
)

type runFlags struct {
	fixtureDir  string
	engines     string
	layout      string
	contextPack string
	thermal     string
	runsDir     string
	runID       string
	// runSubdir names the output directory when it should differ from the run
	// id. The run id itself must stay timestamp-shaped: it is one of the
	// volatile strings the serializer refuses to find in a reusable prefix, and
	// a run id that is an ordinary English word would either false-positive
	// against the fixture's own prose or force that check to be weakened.
	runSubdir     string
	endpoint      string
	repeats       int
	verifyFixture bool
	caveats       []string
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	rf := &runFlags{}
	fs.StringVar(&rf.fixtureDir, "fixture", "fixtures/zig-playback-v1", "fixture directory")
	fs.StringVar(&rf.engines, "engines", "configs/engines/ar.json,configs/engines/dflash2.json",
		"comma-separated engine config paths")
	fs.StringVar(&rf.layout, "layout", "configs/layouts/builder-cache-friendly.json", "prompt layout config")
	fs.StringVar(&rf.contextPack, "context", "base", "context pack name")
	fs.StringVar(&rf.thermal, "thermal", "cold",
		"thermal class of this run: cold, first-capture or steady. Stated by you, never inferred.")
	fs.StringVar(&rf.runsDir, "runs", "runs", "root directory for run output")
	fs.StringVar(&rf.runID, "run-id", "", "run id (default: a UTC timestamp)")
	fs.StringVar(&rf.endpoint, "endpoint", "", "override every engine config's endpoint")
	fs.IntVar(&rf.repeats, "repeats", 1, "repetitions per engine")
	fs.BoolVar(&rf.verifyFixture, "verify-fixture", false,
		"run the fixture's anti-vacuity controls first and refuse to run if any is broken")
	fs.Parse(args)

	_, err := doRun(context.Background(), rf)
	return err
}

// doRun is the body of `run`, shared with `smoke`.
func doRun(ctx context.Context, rf *runFlags) (string, error) {
	f, err := config.LoadFixture(rf.fixtureDir)
	if err != nil {
		return "", err
	}
	layout, err := config.LoadLayout(rf.layout)
	if err != nil {
		return "", err
	}
	var engines []*config.Engine
	for _, p := range splitList(rf.engines) {
		e, err := config.LoadEngine(p)
		if err != nil {
			return "", err
		}
		engines = append(engines, e)
	}
	if len(engines) == 0 {
		return "", fmt.Errorf("no engine configs given")
	}
	switch rf.thermal {
	case "cold", "first-capture", "steady":
	default:
		return "", fmt.Errorf("thermal must be cold, first-capture or steady, not %q; "+
			"warm is never inferred from elapsed time", rf.thermal)
	}

	runID := rf.runID
	if runID == "" {
		runID = time.Now().UTC().Format("20060102T150405Z")
	}
	subdir := rf.runSubdir
	if subdir == "" {
		subdir = runID
	}
	runDir := filepath.Join(rf.runsDir, subdir)
	workDir := filepath.Join(runDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", err
	}

	tok := prompt.Approx{}

	// Run identity, collected before anything is measured.
	sha, dirty := metrics.GitDescribe(".")
	manifestSHA, fileHashes, err := hashFixture(f)
	if err != nil {
		return "", err
	}
	run := &metrics.RunIdentity{
		RunID:             runID,
		StartedAt:         time.Now().UTC(),
		BenchmarkGitSHA:   sha,
		BenchmarkGitDirty: dirty,
		FixtureID:         f.ID,
		FixtureVersion:    f.Version,
		FixtureManifest:   manifestSHA,
		FixtureFiles:      fileHashes,
		Host:              metrics.CollectHost(),
		Toolchain: metrics.Toolchain{
			Go: runtime.Version(),
			// Asked inside the fixture repository: under anyzig the compiler
			// version is resolved from build.zig.zon, so a global `zig version`
			// would answer for the wrong tree or not at all.
			Zig:    executor.ZigVersion(ctx, f.Path(f.RepoDir)),
			ZigPin: zigPin(f),
		},
		Thermal:      rf.thermal,
		WarmupPolicy: "none; v0.1 sends one request per engine and states its thermal class explicitly",
	}
	for _, e := range engines {
		ep := e.Endpoint
		if rf.endpoint != "" {
			ep = rf.endpoint
		}
		run.Engines = append(run.Engines, metrics.EngineIdentity{
			Name: e.Name, Endpoint: ep, Model: e.Model,
			EngineCommit: e.EngineCommit, ModelHash: e.ModelHash, DraftHash: e.DraftHash,
			TargetQuant: e.TargetQuant, KVMode: e.KVMode, SpeculationMode: e.SpeculationMode,
			NonDefaultKnobs: e.NonDefaultKnobs, TelemetryAdapter: e.TelemetryAdapter,
		})
	}

	// Optional fixture verification, before any completion is spent.
	var mutants []executor.MutantOutcome
	var tripwires []executor.TripwireOutcome
	if rf.verifyFixture {
		fmt.Println("verifying fixture controls before measuring...")
		tripwires, err = executor.VerifyHiddenSuiteRuns(ctx, f, filepath.Join(workDir, "verify"))
		if err != nil {
			return "", err
		}
		mutants, err = executor.VerifyMutants(ctx, f, filepath.Join(workDir, "verify"),
			filepath.Join(runDir, "artifacts", "fixture-controls"), nil)
		if err != nil {
			return "", err
		}
		for _, t := range tripwires {
			if !t.Fired {
				return "", fmt.Errorf("hidden file %s is compiled but never run; refusing to measure", t.File)
			}
		}
		for _, m := range mutants {
			if !m.OK {
				return "", fmt.Errorf("anti-vacuity control %s did not behave as declared (%s); "+
					"refusing to measure", m.ID, strings.Join(m.Reasons, "; "))
			}
		}
		fmt.Printf("  %d controls held, %d tripwires fired\n\n", len(mutants), len(tripwires))
	}

	if err := run.Save(filepath.Join(runDir, "run.json")); err != nil {
		return "", err
	}

	w, err := metrics.NewWriter(filepath.Join(runDir, "request.jsonl"))
	if err != nil {
		return "", err
	}
	defer w.Close()

	var records []metrics.Record
	// One prompt hash is expected across engines: the same frozen task, byte
	// for byte. Checked, because that is the whole basis of the comparison.
	promptHashes := map[string][]string{}

	for rep := 0; rep < rf.repeats; rep++ {
		for _, e := range engines {
			label := e.Name
			if rf.repeats > 1 {
				label = fmt.Sprintf("%s rep %d", e.Name, rep+1)
			}
			fmt.Printf("-> %s (%s, %s, %s)\n", label, layout.ID, rf.contextPack, rf.thermal)

			res, err := runner.RunBuilder(ctx, runner.Options{
				Fixture: f, Engine: e, Layout: layout,
				ContextPack: rf.contextPack, Thermal: rf.thermal,
				RunID: runID, RunDir: runDir, WorkDir: workDir,
				Tokenizer: tok, EndpointOverride: rf.endpoint,
			})
			if err != nil {
				return "", fmt.Errorf("engine %s: %w", e.Name, err)
			}
			promptHashes[res.Record.PromptSHA256] = append(
				promptHashes[res.Record.PromptSHA256], e.Name)
			if err := w.Write(res.Record); err != nil {
				return "", err
			}
			records = append(records, *res.Record)

			q := res.Record.Quality
			verdict := "FAIL"
			if q != nil && q.Passed {
				verdict = "PASS"
			}
			fmt.Printf("   %s  wall %.0f ms", verdict, res.Record.WallMS)
			if res.Record.VisibleTTFTMS != nil {
				fmt.Printf("  visible ttft %.0f ms", *res.Record.VisibleTTFTMS)
			}
			fmt.Printf("  prompt %d bytes / ~%d tok (%s)\n",
				res.Record.PromptBytes, res.Record.PromptTokensEstimated, res.Record.TokenizerID)
			if q != nil {
				for _, g := range q.Gates {
					mark := map[metrics.GateResult]string{
						metrics.GatePass: "pass", metrics.GateFail: "FAIL", metrics.GateSkipped: "skip",
					}[g.Result]
					detail := g.Detail
					if len(detail) > 110 {
						detail = detail[:110] + "..."
					}
					fmt.Printf("     %-4s %-18s %s\n", mark, g.Name, detail)
				}
			}
			fmt.Println()
		}
	}

	caveats := append([]string{}, rf.caveats...)
	if len(promptHashes) > 1 {
		var lines []string
		for h, names := range promptHashes {
			lines = append(lines, fmt.Sprintf("%s -> %s", h[:12], strings.Join(names, ", ")))
		}
		sort.Strings(lines)
		caveats = append(caveats, "**The compared configs did not receive the same prompt bytes**: "+
			strings.Join(lines, "; ")+". The comparison is not valid.")
	}
	if rf.endpoint != "" {
		caveats = append(caveats, fmt.Sprintf(
			"Every engine config was pointed at `%s`, overriding its own endpoint.", rf.endpoint))
	}

	cells := report.Aggregate(records)
	if err := report.WriteCSV(filepath.Join(runDir, "summary.csv"), cells); err != nil {
		return "", err
	}
	if err := report.WriteSummary(filepath.Join(runDir, "summary.md"), report.SummaryInput{
		Run: run, Records: records, Cells: cells,
		Mutants: mutants, Tripwires: tripwires,
		Unmeasured: f.Unmeasured, Caveats: caveats,
	}); err != nil {
		return "", err
	}

	fmt.Printf("wrote %s\n", filepath.Join(runDir, "request.jsonl"))
	fmt.Printf("wrote %s\n", filepath.Join(runDir, "summary.md"))
	fmt.Printf("wrote %s\n", filepath.Join(runDir, "summary.csv"))
	return runDir, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
