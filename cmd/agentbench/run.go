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
	beforeEngine  string
	allowDrift    bool
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
	fs.BoolVar(&rf.allowDrift, "allow-toolchain-drift", false,
		"record the run even though the compiler differs from the one the fixture was "+
			"verified under. The mismatch becomes a caveat on every row rather than an "+
			"error; the fixture's goldens and mutant controls are not evidence under a "+
			"compiler that did not produce them.")
	fs.StringVar(&rf.beforeEngine, "before-engine", "",
		"script invoked as `<script> <engine-name>` before each engine's requests. It "+
			"owns all server lifecycle -- stopping, reconfiguring, restarting and waiting "+
			"for readiness -- because an OpenAI-compatible request cannot select a "+
			"speculation mode or a KV quantization. Without it, two engine configs sent "+
			"to one running daemon produce two differently labelled rows from one actual "+
			"configuration. Its stdout and stderr are kept as run artifacts.")
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
	// A repeated cold measurement is only cold if something returns the server
	// to a cold state between repeats. Without that, repeats 2..N are warm rows
	// wearing a cold label -- which is the failure this whole harness exists to
	// prevent, so it is refused rather than warned about.
	if rf.thermal == "cold" && rf.repeats > 1 && rf.beforeEngine == "" && rf.preEngine == nil {
		return "", fmt.Errorf("-thermal cold with -repeats %d needs a way to return the "+
			"server to a cold state between repetitions. Pass -before-engine, or use "+
			"-repeats 1, or declare -thermal steady with -warmup 1", rf.repeats)
	}

	zigActual, err := checkToolchain(ctx, f, rf.allowDrift)
	if err != nil {
		return "", err
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
			Zig:    zigActual,
			ZigPin: f.Toolchain.Zig,
		},
		Thermal:      rf.thermal,
		WarmupPolicy: warmupPolicy(rf, traj),
	}
	engineIdentity := map[string]*metrics.EngineIdentity{}
	for _, e := range engines {
		ep := e.Endpoint
		if rf.endpoint != "" {
			ep = rf.endpoint
		}
		id := &metrics.EngineIdentity{
			Name: e.Name, Endpoint: ep, Model: e.Model,
			EngineCommit: e.EngineCommit, ModelHash: e.ModelHash, DraftHash: e.DraftHash,
			TargetQuant: e.TargetQuant, KVMode: e.KVMode, SpeculationMode: e.SpeculationMode,
			NonDefaultKnobs: e.NonDefaultKnobs, TelemetryAdapter: e.TelemetryAdapter,
			AttestationMethod: "unattested: the config's engine state was asserted, not produced or checked",
		}
		engineIdentity[e.Name] = id
		run.Engines = append(run.Engines, *id)
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

	// Engine outer, repetition inner. See schedule.go: with the loops the other
	// way round, preparing engine B leaves the server in B's state and every
	// later repetition labelled A is measured against B.
	//
	// A cold measurement is only cold if the server was actually returned to a
	// cold state, so repeated cold measurements are prepared individually and
	// are never warmed.
	preparePerRep := rf.thermal == "cold" && rf.repeats > 1
	steps := schedule(scheduleParams{
		Engines: len(engines), Repeats: rf.repeats, Warmups: rf.warmup,
		PreparePerRepetition: preparePerRep,
	})
	// Belt and braces: the topology is asserted, not trusted. This is the exact
	// invariant a plausible-looking loop rewrite breaks, and breaking it
	// produces mislabelled rows rather than an error.
	if err := checkSchedule(steps); err != nil {
		return "", fmt.Errorf("internal: bad run schedule: %w", err)
	}

	optsFor := func(e *config.Engine) runner.Options {
		return runner.Options{
			Fixture: f, Engine: e, Layout: layout,
			ContextPack: rf.contextPack, Thermal: rf.thermal,
			RunID: runID, RunDir: runDir, WorkDir: workDir,
			Tokenizer: tok, EndpointOverride: rf.endpoint,
			Trajectory: traj,
		}
	}

	for _, st := range steps {
		e := engines[st.Engine]

		switch st.Kind {
		case stepPrepare:
			if rf.preEngine != nil {
				if err := rf.preEngine(e.Name); err != nil {
					return "", fmt.Errorf("engine %s: preparing a %s start: %w",
						e.Name, rf.thermal, err)
				}
			}
			if rf.beforeEngine == "" && e.IdentityProbe == nil {
				continue
			}
			ep := e.Endpoint
			if rf.endpoint != "" {
				ep = rf.endpoint
			}
			suffix := ""
			if preparePerRep {
				suffix = fmt.Sprintf(".rep%d", st.PrepareOrdinal+1)
			}
			fmt.Printf("-> preparing %s%s\n", e.Name, suffix)
			att, err := prepareEngine(ctx, e, rf.beforeEngine, ep, runDir, suffix, f.CommandTimeout())
			if err != nil {
				return "", err
			}
			applyAttestation(engineIdentity[e.Name], att)
			fmt.Printf("   identity: %s\n", att.Method)
			for i := range run.Engines {
				if run.Engines[i].Name == e.Name {
					run.Engines[i] = *engineIdentity[e.Name]
				}
			}
			if err := run.Save(filepath.Join(runDir, "run.json")); err != nil {
				return "", err
			}

		case stepWarmup:
			// The resident-server warm protocol: send and discard priming
			// requests, then measure. Once per engine, not once per
			// repetition -- priming immediately before every measurement would
			// measure an identical request replayed against a fully populated
			// prefix cache rather than a resident engine.
			fmt.Printf("   warmup for %s (discarded)\n", e.Name)
			if _, err := runner.RunTurn(ctx, optsFor(e), runner.TurnOptions{
				Index: 0, Score: false, TurnCount: 1,
				Thermal: "warmup-discarded",
			}); err != nil {
				return "", fmt.Errorf("engine %s warmup: %w", e.Name, err)
			}

		case stepMeasure:
			label := e.Name
			if rf.repeats > 1 {
				label = fmt.Sprintf("%s rep %d/%d", e.Name, st.Rep+1, rf.repeats)
			}
			fmt.Printf("-> %s (%s, %s, %s)\n", label, layout.ID, rf.contextPack, rf.thermal)

			opts := optsFor(e)
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
				r.Repetition = st.Rep
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
	if f.Toolchain.Zig != "" && zigActual != f.Toolchain.Zig {
		caveats = append(caveats, fmt.Sprintf(
			"**The fixture was verified under zig `%s` and this run used zig `%s`.** "+
				"Its goldens, its mutant controls and its hidden suite are evidence only "+
				"under the compiler that produced them, so every quality verdict below "+
				"rests on an unverified toolchain.", f.Toolchain.Zig, zigActual))
	}
	if unattested := unattestedEngines(run.Engines); len(unattested) > 0 && len(engines) > 1 {
		caveats = append(caveats, fmt.Sprintf(
			"**Engine identity is unattested for %s.** An OpenAI-compatible request cannot "+
				"select a speculation mode or a KV quantization, so these rows are labelled "+
				"from their config files and not from anything this run produced or checked. "+
				"If more than one config was sent to a single running server, the labels "+
				"below may all describe the same actual configuration. Use `-before-engine` "+
				"and an `identity_probe` before treating this as an A/B.",
			"`"+strings.Join(unattested, "`, `")+"`"))
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
		parts = append(parts, fmt.Sprintf(
			"%d discarded priming request(s) per engine, sent once after that engine was "+
				"prepared and before its first measured repetition -- not before each "+
				"repetition, which would measure an identical request replayed against a "+
				"fully populated prefix cache", rf.warmup))
	} else {
		parts = append(parts, "no priming requests")
	}
	if rf.thermal == "cold" && rf.repeats > 1 {
		parts = append(parts, fmt.Sprintf(
			"each of the %d cold repetitions was preceded by its own engine preparation",
			rf.repeats))
	} else if rf.repeats > 1 {
		parts = append(parts, fmt.Sprintf(
			"each engine was prepared once, then measured %d times in a row; engines are "+
				"never interleaved across repetitions", rf.repeats))
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

// unattestedEngines names configs whose engine state this run neither produced
// nor verified.
func unattestedEngines(ids []metrics.EngineIdentity) []string {
	var out []string
	for _, id := range ids {
		if !id.Attested {
			out = append(out, id.Name)
		}
	}
	sort.Strings(out)
	return out
}
