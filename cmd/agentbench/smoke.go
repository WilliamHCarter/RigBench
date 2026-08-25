package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/mock"
)

// expectation is what the v0.1 acceptance gate requires of one canned variant.
//
// The point of the table is that every gate is individually reachable. A
// benchmark whose scope gate has never gone red is a benchmark with an
// untested scope gate.
type expectation struct {
	variant mock.Variant
	passed  bool
	// failingGate, when set, must be the gate that failed. "" means only that
	// the run must fail somewhere.
	failingGate string
	why         string
}

var acceptance = []expectation{
	{mock.Reference, true, "",
		"the reference solution passes every gate, so a correct patch is recognised as correct"},
	{mock.VisibleGreenHiddenRed, false, "hidden_tests",
		"a patch that builds and passes the visible suite is still refused by the hidden suite"},
	{mock.Broken, false, "hidden_tests",
		"a behaviourally broken patch fails the hidden invariants"},
	{mock.ScopeViolation, false, "scope",
		"a patch that edits the frozen golden fails the scope gate"},
	{mock.Unapplyable, false, "patch_applies",
		"a diff that does not apply cleanly is a failed gate, not a coerced apply"},
	{mock.NoDiff, false, "patch_extracted",
		"a confident report with no diff in it fails at extraction"},
	{mock.Unwired, false, "candidate_tests_discriminate",
		"a patch that adds the new files and wires them into nothing still builds and\n" +
			"still passes the visible suite, because that suite only tests behaviour that\n" +
			"existed before the seam. Something has to catch it."},
	{mock.CommentOnly, false, "candidate_tests_discriminate",
		"and so does a one-line comment. This is the control for the gate stack itself:\n" +
			"without it, doing nothing collects six of seven gates and reads as a near miss."},
}

func cmdSmoke(args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	fixtureDir := fs.String("fixture", "fixtures/zig-playback-v1", "fixture directory")
	layout := fs.String("layout", "configs/layouts/builder-cache-friendly.json", "prompt layout")
	engines := fs.String("engines", "configs/engines/mock-ar.json,configs/engines/mock-dflash2.json",
		"engine configs to run against the mock")
	runsDir := fs.String("runs", "runs", "root directory for run output")
	timeScale := fs.Float64("time-scale", 0.02,
		"mock delay multiplier; the acceptance gate is about gates, not speed")
	skipVerify := fs.Bool("skip-verify", false, "skip the fixture control set (not recommended)")
	only := fs.String("only", "", "comma-separated variants to exercise")
	slice := fs.String("slice", "all", "which acceptance gate to run: v0.1, v0.2 or all")
	fs.Parse(args)
	switch *slice {
	case "v0.1", "v0.2", "all":
	default:
		return fmt.Errorf("-slice must be v0.1, v0.2 or all, not %q", *slice)
	}

	ctx := context.Background()
	f, err := config.LoadFixture(*fixtureDir)
	if err != nil {
		return err
	}

	stamp := time.Now().UTC().Format("20060102T150405Z")
	root := filepath.Join(*runsDir, "smoke-"+stamp)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	fmt.Printf("AgentBench-01 acceptance gate (%s)\n", *slice)
	fmt.Printf("fixture %s v%s   output %s\n\n", f.ID, f.Version, root)

	// --- 1. the fixture must prove itself before it measures anything ---
	if !*skipVerify {
		fmt.Println("== fixture controls ==")
		work := filepath.Join(root, "fixture-verify")
		tw, err := executor.VerifyHiddenSuiteRuns(ctx, f, work)
		if err != nil {
			return err
		}
		for _, t := range tw {
			if !t.Fired {
				return fmt.Errorf("hidden file %s is compiled but never run: %s", t.File, t.Detail)
			}
		}
		fmt.Printf("  %d hidden file(s), every tripwire fired\n", len(tw))

		outcomes, err := executor.VerifyMutants(ctx, f, work, filepath.Join(root, "fixture-controls"), nil)
		if err != nil {
			return err
		}
		invisible := 0
		for _, m := range outcomes {
			if !m.OK {
				return fmt.Errorf("control %s did not behave as declared: %s",
					m.ID, strings.Join(m.Reasons, "; "))
			}
			if m.Got.Visible == "pass" && m.Got.Hidden == "fail" {
				invisible++
			}
		}
		fmt.Printf("  %d control(s) held; %d of them pass the visible suite and are caught only by the hidden suite\n\n",
			len(outcomes), invisible)
		if invisible == 0 {
			return fmt.Errorf("no control is invisible to the visible suite")
		}
	}

	fmt.Println("== no-think is actually requested ==")
	if err := acceptNoThink(ctx, *fixtureDir, *layout, root, *timeScale); err != nil {
		return err
	}
	fmt.Println()

	if *slice == "v0.2" {
		fmt.Println("== v0.2: multi-turn replay and prefix layout ==")
		if _, err := acceptV02(ctx, v02Options{
			fixtureDir: *fixtureDir, engines: *engines,
			layouts: []string{
				"configs/layouts/builder-cache-friendly.json",
				"configs/layouts/builder-current.json",
			},
			root: root, timeScale: *timeScale,
		}); err != nil {
			return err
		}
		fmt.Printf("\nv0.2 acceptance gate: PASS\noutput under %s\n", root)
		return nil
	}

	// --- 2. every gate must be individually reachable ---
	wanted := map[string]bool{}
	for _, v := range splitList(*only) {
		wanted[v] = true
	}

	type row struct {
		variant mock.Variant
		engine  string
		passed  bool
		gates   []string
		ok      bool
		detail  string
	}
	var rows []row
	failures := 0

	for _, exp := range acceptance {
		if len(wanted) > 0 && !wanted[string(exp.variant)] {
			continue
		}
		fmt.Printf("== variant %s ==\n%s\n", exp.variant, indent(exp.why))

		stage, err := os.MkdirTemp("", "agentbench-smoke-")
		if err != nil {
			return err
		}
		body, err := mock.BuildResponse(ctx, f, exp.variant, filepath.Join(stage, "canned"))
		if err != nil {
			os.RemoveAll(stage)
			return err
		}
		srv := &mock.Server{
			TimeScale:  *timeScale,
			ProfileFor: profileFromRequest,
			Respond:    func(int) (string, string) { return body, "" },
		}
		ln, shutdown, err := srv.Listen("127.0.0.1:0")
		if err != nil {
			os.RemoveAll(stage)
			return err
		}
		endpoint := fmt.Sprintf("http://%s/v1", ln.Addr())

		runDir, runErr := doRun(ctx, &runFlags{
			fixtureDir:  *fixtureDir,
			engines:     *engines,
			layout:      *layout,
			contextPack: "base",
			thermal:     "cold",
			runsDir:     root,
			runID:       stamp + "-" + shortHash(string(exp.variant)),
			runSubdir:   string(exp.variant),
			endpoint:    endpoint,
			repeats:     1,
			caveats: []string{
				fmt.Sprintf("Endpoint was the in-repo deterministic mock (variant `%s`, time-scale %g). "+
					"Its timings are a fixture, not a measurement, and must not appear in a champion decision.",
					exp.variant, *timeScale),
				fmt.Sprintf("The time scale multiplies every simulated delay by %g, so the derived "+
					"decode rate in section 3 is inflated by about %.0fx. The engine-reported rates "+
					"are the mock's declared constants and are unaffected.",
					*timeScale, 1 / *timeScale),
			},
		})
		_ = shutdown(ctx)
		os.RemoveAll(stage)
		if runErr != nil {
			return runErr
		}

		recs, err := metrics.ReadRecords(filepath.Join(runDir, "request.jsonl"))
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return fmt.Errorf("variant %s produced no records", exp.variant)
		}
		// The two engine configs must have received identical prompt bytes.
		sha := recs[0].PromptSHA256
		for _, r := range recs {
			if r.PromptSHA256 != sha {
				return fmt.Errorf("variant %s: engines received different prompt bytes (%s vs %s)",
					exp.variant, sha[:12], r.PromptSHA256[:12])
			}
		}

		for _, r := range recs {
			gates := failedGateNames(r.Quality)
			ok, detail := checkExpectation(exp, r.Quality, gates)
			if !ok {
				failures++
			}
			rows = append(rows, row{exp.variant, r.Engine, passedOf(r.Quality), gates, ok, detail})
		}
		fmt.Println()
	}

	// --- 3. the acceptance matrix ---
	fmt.Println("== acceptance ==")
	fmt.Printf("  %-26s %-14s %-6s %-28s %s\n", "variant", "engine", "passed", "failing gates", "verdict")
	for _, r := range rows {
		verdict := "ok"
		if !r.ok {
			verdict = "MISMATCH: " + r.detail
		}
		fmt.Printf("  %-26s %-14s %-6v %-28s %s\n",
			r.variant, r.engine, r.passed, strings.Join(r.gates, ","), verdict)
	}
	fmt.Printf("\n%d row(s), %d mismatch(es)\n", len(rows), failures)
	if failures > 0 {
		return fmt.Errorf("%d acceptance row(s) did not behave as declared", failures)
	}
	fmt.Printf("\nv0.1 acceptance gate: PASS\n")

	if *slice == "all" {
		fmt.Println("\n== v0.2: multi-turn replay and prefix layout ==")
		if _, err := acceptV02(ctx, v02Options{
			fixtureDir: *fixtureDir, engines: *engines,
			layouts: []string{
				"configs/layouts/builder-cache-friendly.json",
				"configs/layouts/builder-current.json",
			},
			root: root, timeScale: *timeScale,
		}); err != nil {
			return err
		}
		fmt.Printf("\nv0.2 acceptance gate: PASS\n")
	}

	fmt.Printf("output under %s\n", root)
	return nil
}

func checkExpectation(exp expectation, q *metrics.Quality, gates []string) (bool, string) {
	got := passedOf(q)
	if got != exp.passed {
		return false, fmt.Sprintf("want passed=%v, got %v", exp.passed, got)
	}
	if exp.failingGate == "" {
		return true, ""
	}
	for _, g := range gates {
		if g == exp.failingGate {
			return true, ""
		}
	}
	return false, fmt.Sprintf("want gate %q to fail, failing gates were [%s]",
		exp.failingGate, strings.Join(gates, ","))
}

func passedOf(q *metrics.Quality) bool { return q != nil && q.Passed }

// shortHash keeps a run id opaque. A variant name is a readable directory name
// and a bad run id; see runFlags.runSubdir.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// failedGateNames lists only genuinely failed gates. A skipped gate is reported
// separately as "name(skipped)" so a cascade of skips cannot be mistaken for
// the gate that actually caught the defect.
func failedGateNames(q *metrics.Quality) []string {
	if q == nil {
		return nil
	}
	var out []string
	for _, g := range q.Gates {
		switch g.Result {
		case metrics.GateFail:
			out = append(out, g.Name)
		case metrics.GateSkipped:
			out = append(out, g.Name+"(skipped)")
		}
	}
	return out
}
