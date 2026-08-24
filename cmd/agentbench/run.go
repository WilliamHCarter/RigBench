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
	turns         bool
	warmup        int
	verifyFixture bool
	caveats       []string
	// layoutRows, when set, is rendered as the summary's layout A/B section.
	layoutRows []report.LayoutRow
	// preEngine runs before each engine's turns. A run declared cold must
	// actually start cold for every config it compares, and only the caller
	// knows how to produce that for its endpoint -- restarting a server,
	// dropping a cache -- so the runner asks rather than assumes.
	preEngine func(engineName string) error
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
	fs.BoolVar(&rf.turns, "turns", false,
		"replay the fixture's multi-turn builder trajectory instead of a single turn")
	fs.IntVar(&rf.warmup, "warmup", 0,
		"discarded priming requests sent before the first measured turn. This is the "+
			"resident-server warm protocol: warm is produced deliberately and recorded, "+
			"never inferred from elapsed time.")
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
	if rf.warmup > 0 && rf.thermal == "cold" {
		return "", fmt.Errorf("a cold run cannot be preceded by %d warmup request(s); "+
			"declare -thermal steady, or drop -warmup", rf.warmup)
	}

	var traj *config.Trajectory
	if rf.turns {
		traj, err = f.LoadTrajectory()
		if err != nil {
			return "", err
		}
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
		WarmupPolicy: warmupPolicy(rf, traj),
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

			if rf.preEngine != nil {
				if err := rf.preEngine(e.Name); err != nil {
					return "", fmt.Errorf("engine %s: preparing a %s start: %w",
						e.Name, rf.thermal, err)
				}
			}

			opts := runner.Options{
				Fixture: f, Engine: e, Layout: layout,
				ContextPack: rf.contextPack, Thermal: rf.thermal,
				RunID: runID, RunDir: runDir, WorkDir: workDir,
				Tokenizer: tok, EndpointOverride: rf.endpoint,
				Trajectory: traj,
			}

			// The resident-server warm protocol: send and discard priming
			// requests, then measure. The discarded requests are not recorded,
			// but that they happened is, in run.json's warmup policy.
			for i := 0; i < rf.warmup; i++ {
				fmt.Printf("   warmup %d/%d (discarded)\n", i+1, rf.warmup)
				if _, err := runner.RunTurn(ctx, opts, runner.TurnOptions{
					Index: 0, Score: false, TurnCount: 1,
					Thermal: "warmup-discarded",
				}); err != nil {
					return "", fmt.Errorf("engine %s warmup: %w", e.Name, err)
				}
			}

			var results []*runner.Result
			if traj != nil {
				results, err = runner.RunTrajectory(ctx, opts)
			} else {
				var one *runner.Result
				one, err = runner.RunBuilder(ctx, opts)
				if one != nil {
					results = []*runner.Result{one}
				}
			}
			if err != nil {
				return "", fmt.Errorf("engine %s: %w", e.Name, err)
			}

			for _, res := range results {
				r := res.Record
				promptHashes[r.PromptSHA256] = append(promptHashes[r.PromptSHA256],
					fmt.Sprintf("%s t%d", e.Name, r.TurnIndex))
				if err := w.Write(r); err != nil {
					return "", err
				}
				records = append(records, *r)
				printTurn(r, traj != nil)
			}
			fmt.Println()
		}
	}

	caveats := append([]string{}, rf.caveats...)
	// Engines being compared must have received identical prompt bytes at the
	// same turn index. Across turns the bytes legitimately differ -- that is the
	// point of a multi-turn replay -- so the check is per turn.
	if bad := mismatchedTurns(records); len(bad) > 0 {
		caveats = append(caveats, "**The compared configs did not receive the same prompt bytes**: "+
			strings.Join(bad, "; ")+". The comparison is not valid.")
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
		LayoutComparison: rf.layoutRows,
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

// printTurn renders one request's outcome on the terminal.
func printTurn(r *metrics.Record, multiTurn bool) {
	verdict := "unscored"
	if r.Scored {
		verdict = "FAIL"
		if r.Quality != nil && r.Quality.Passed {
			verdict = "PASS"
		}
	}
	turn := ""
	if multiTurn {
		turn = fmt.Sprintf(" t%d/%d", r.TurnIndex, r.TurnCount-1)
	}
	fmt.Printf("  %s%s  %s  wall %.0f ms", r.Thermal, turn, verdict, r.WallMS)
	if r.VisibleTTFTMS != nil {
		fmt.Printf("  ttft %.0f ms", *r.VisibleTTFTMS)
	}
	fmt.Printf("  prompt %d B", r.PromptBytes)
	if r.TurnIndex > 0 {
		fmt.Printf(" (+%d)", r.AppendedBytes)
	}
	fmt.Printf("  prefix %d B", r.StablePrefixBytes)
	if r.PrefixCacheHitTokens != nil {
		fmt.Printf("  cache hit %d tok", *r.PrefixCacheHitTokens)
	}
	fmt.Println()
	if r.Scored && r.Quality != nil {
		for _, g := range r.Quality.Gates {
			mark := map[metrics.GateResult]string{
				metrics.GatePass: "pass", metrics.GateFail: "FAIL", metrics.GateSkipped: "skip",
			}[g.Result]
			detail := g.Detail
			if len(detail) > 100 {
				detail = detail[:100] + "..."
			}
			fmt.Printf("     %-4s %-18s %s\n", mark, g.Name, detail)
		}
	}
}

// warmupPolicy records how warm was produced, so a reader never has to infer it.
func warmupPolicy(rf *runFlags, traj *config.Trajectory) string {
	var parts []string
	if rf.warmup > 0 {
		parts = append(parts, fmt.Sprintf("%d discarded priming request(s) before the first measured turn",
			rf.warmup))
	} else {
		parts = append(parts, "no priming requests")
	}
	if traj != nil {
		parts = append(parts, fmt.Sprintf(
			"trajectory %s replayed over %d turns; in a cold run turn 0 is recorded cold "+
				"and turns 1..%d as warm-resident, because the server is resident by then",
			traj.ID, len(traj.Turns), len(traj.Turns)-1))
	} else {
		parts = append(parts, "single turn per engine")
	}
	return strings.Join(parts, "; ")
}

// mismatchedTurns reports turn indices where the engines under comparison did
// not receive identical prompt bytes.
func mismatchedTurns(records []metrics.Record) []string {
	type key struct {
		layout string
		turn   int
	}
	byTurn := map[key]map[string][]string{}
	for _, r := range records {
		if r.Thermal == "warmup-discarded" {
			continue
		}
		k := key{r.PromptLayout, r.TurnIndex}
		if byTurn[k] == nil {
			byTurn[k] = map[string][]string{}
		}
		byTurn[k][r.PromptSHA256] = append(byTurn[k][r.PromptSHA256], r.Engine)
	}
	var out []string
	for k, hashes := range byTurn {
		if len(hashes) <= 1 {
			continue
		}
		var lines []string
		for h, engines := range hashes {
			lines = append(lines, fmt.Sprintf("%s -> %s", h[:12], strings.Join(engines, ", ")))
		}
		sort.Strings(lines)
		out = append(out, fmt.Sprintf("layout %s turn %d: %s", k.layout, k.turn, strings.Join(lines, " / ")))
	}
	sort.Strings(out)
	return out
}
