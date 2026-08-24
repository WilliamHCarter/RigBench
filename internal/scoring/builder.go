// Package scoring turns a candidate's artifacts into quality gates.
//
// The primary builder score is wall-clock time to a quality-gated passing
// patch. A run that fails a gate keeps its timing -- the measurement is real --
// but is not eligible to become the champion configuration. That distinction
// lives here: Quality.Passed is the eligibility flag, and it is false whenever
// any required gate is anything other than pass, including skipped.
package scoring

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

// Gate names. These are contract: the reporter and the fixture's
// required_gates list both refer to them.
const (
	GatePatchExtracted = "patch_extracted"
	GatePatchApplies   = "patch_applies"
	GateBuild          = "build"
	GateVisibleTests   = "visible_tests"
	GateHiddenTests    = "hidden_tests"
	GateScope          = "scope"
	GateReturnContract = "return_contract"
)

// BuilderInput is everything needed to score one builder response.
type BuilderInput struct {
	Fixture  *config.Fixture
	Worktree *executor.Worktree
	// Output is the model's visible text.
	Output string
	// ArtifactDir receives the patch, the diff and every command log.
	ArtifactDir string
	// ArtifactPrefix is the run-relative prefix recorded in the Gate.Artifact
	// field, so a report can find the log from the JSONL row.
	ArtifactPrefix string
}

// ScoreBuilder applies the candidate patch and runs every gate.
//
// Ordering matters and is not an optimization: a gate whose precondition failed
// is recorded as skipped rather than run against a tree in an unknown state.
// The scope gate is the exception -- it runs whenever a patch applied at all,
// because "it compiled but it edited the golden" must be visible.
func ScoreBuilder(ctx context.Context, in BuilderInput) (*metrics.Quality, error) {
	q := &metrics.Quality{}
	if err := os.MkdirAll(in.ArtifactDir, 0o755); err != nil {
		return nil, err
	}

	writeLog := func(name string, r *executor.CommandResult) string {
		if r == nil {
			return ""
		}
		rel := name + ".log"
		body := fmt.Sprintf("$ %s\n(cwd %s)\nexit %d  duration %s  timed_out=%v unavailable=%v\n\n%s\n",
			strings.Join(r.Argv, " "), r.Dir, r.ExitCode, r.Duration.Round(time.Millisecond),
			r.TimedOut, r.Unavailable, r.Combined())
		if err := os.WriteFile(filepath.Join(in.ArtifactDir, rel), []byte(body), 0o644); err != nil {
			return ""
		}
		return filepath.ToSlash(filepath.Join(in.ArtifactPrefix, rel))
	}

	skip := func(name, why string) {
		q.Gates = append(q.Gates, metrics.Gate{
			Name: name, Result: metrics.GateSkipped, Detail: why,
		})
	}

	// --- patch extraction ---
	patch, form, err := executor.ExtractPatch(in.Output)
	if err != nil {
		q.Gates = append(q.Gates, metrics.Gate{
			Name: GatePatchExtracted, Result: metrics.GateFail, Detail: err.Error(),
		})
		for _, g := range []string{GatePatchApplies, GateBuild, GateVisibleTests, GateHiddenTests, GateScope} {
			skip(g, "no patch to apply")
		}
		q.Gates = append(q.Gates, checkReturnContract(in.Output))
		q.Passed = allRequiredPassed(in.Fixture, q)
		return q, nil
	}
	q.Gates = append(q.Gates, metrics.Gate{
		Name: GatePatchExtracted, Result: metrics.GatePass,
		Detail: fmt.Sprintf("%s, %d bytes", form, len(patch)),
	})
	q.PatchFiles = executor.PatchFiles(patch)
	_ = os.WriteFile(filepath.Join(in.ArtifactDir, "candidate.patch"), []byte(patch), 0o644)

	// --- apply ---
	ap := in.Worktree.ApplyPatch(ctx, patch)
	applyArtifact := writeLog("git-apply", ap)
	if !ap.OK() {
		q.Gates = append(q.Gates, metrics.Gate{
			Name: GatePatchApplies, Result: metrics.GateFail,
			Detail: firstLine(ap.Combined()), Command: strings.Join(ap.Argv, " "),
			ExitCode: metrics.Ptr(ap.ExitCode), Artifact: applyArtifact,
		})
		for _, g := range []string{GateBuild, GateVisibleTests, GateHiddenTests} {
			skip(g, "patch did not apply")
		}
		q.Gates = append(q.Gates, scopeGate(ctx, in, nil))
		q.Gates = append(q.Gates, checkReturnContract(in.Output))
		q.Passed = allRequiredPassed(in.Fixture, q)
		return q, nil
	}
	q.Gates = append(q.Gates, metrics.Gate{
		Name: GatePatchApplies, Result: metrics.GatePass,
		Command: strings.Join(ap.Argv, " "), ExitCode: metrics.Ptr(0), Artifact: applyArtifact,
	})

	// The applied diff, regenerated from the tree rather than echoed back, is
	// the artifact a reviewer should read.
	if d, err := in.Worktree.Diff(ctx); err == nil {
		_ = os.WriteFile(filepath.Join(in.ArtifactDir, "applied.diff"), []byte(d), 0o644)
	}

	// --- scope, before the oracle is in the tree ---
	changed, _ := in.Worktree.ChangedFiles(ctx)
	scope := scopeGate(ctx, in, changed)
	q.OutOfScopeFiles = outOfScope(in.Fixture, changed)
	q.Gates = append(q.Gates, scope)

	timeout := in.Fixture.CommandTimeout()

	// --- build ---
	bres := executor.Run(ctx, in.Worktree.Dir, in.Fixture.Commands.Build, timeout)
	q.Gates = append(q.Gates, rungGate(GateBuild, bres, writeLog("build", bres)))
	if !bres.OK() {
		for _, g := range []string{GateVisibleTests, GateHiddenTests} {
			skip(g, "build did not succeed")
		}
		q.Gates = append(q.Gates, checkReturnContract(in.Output))
		q.Passed = allRequiredPassed(in.Fixture, q)
		return q, nil
	}

	// --- visible tests ---
	vres := executor.Run(ctx, in.Worktree.Dir, in.Fixture.Commands.Visible, timeout)
	vgate := rungGate(GateVisibleTests, vres, writeLog("visible-tests", vres))
	if p, _ := executor.ParseZigTestCounts(vres.Combined()); p != nil {
		q.VisibleTestsPassed = p
		vgate.Detail = strings.TrimSpace(fmt.Sprintf("%s %d tests passed", vgate.Detail, *p))
	}
	q.Gates = append(q.Gates, vgate)

	// --- hidden tests ---
	// Injected only now: the candidate never saw the oracle in the tree, and
	// the visible rung above ran against exactly what the model was given.
	if err := in.Worktree.InjectHidden(ctx); err != nil {
		skip(GateHiddenTests, "could not inject the hidden suite: "+err.Error())
		q.Gates = append(q.Gates, checkReturnContract(in.Output))
		q.Passed = allRequiredPassed(in.Fixture, q)
		return q, nil
	}
	hres := executor.Run(ctx, in.Worktree.Dir, in.Fixture.Commands.Hidden, timeout)
	hgate := rungGate(GateHiddenTests, hres, writeLog("hidden-tests", hres))
	if p, _ := executor.ParseZigTestCounts(hres.Combined()); p != nil {
		q.HiddenTestsPassed = p
		hgate.Detail = strings.TrimSpace(fmt.Sprintf("%s %d tests passed", hgate.Detail, *p))
	}
	q.Gates = append(q.Gates, hgate)

	q.Gates = append(q.Gates, checkReturnContract(in.Output))
	q.Passed = allRequiredPassed(in.Fixture, q)
	return q, nil
}

func rungGate(name string, r *executor.CommandResult, artifact string) metrics.Gate {
	g := metrics.Gate{
		Name:     name,
		Command:  strings.Join(r.Argv, " "),
		ExitCode: metrics.Ptr(r.ExitCode),
		Artifact: artifact,
	}
	switch {
	case r.Unavailable:
		// A missing toolchain is not a failing patch. It is a rung that did not
		// run, and it must never be scored as a pass.
		g.Result = metrics.GateSkipped
		g.Detail = r.Err
	case r.TimedOut:
		g.Result = metrics.GateFail
		g.Detail = r.Err
	case r.ExitCode != 0:
		g.Result = metrics.GateFail
		g.Detail = firstLine(r.Combined())
	default:
		g.Result = metrics.GatePass
	}
	return g
}

func scopeGate(ctx context.Context, in BuilderInput, changed []string) metrics.Gate {
	if changed == nil {
		var err error
		changed, err = in.Worktree.ChangedFiles(ctx)
		if err != nil {
			return metrics.Gate{Name: GateScope, Result: metrics.GateSkipped, Detail: err.Error()}
		}
	}
	bad := outOfScope(in.Fixture, changed)
	if len(bad) > 0 {
		return metrics.Gate{
			Name: GateScope, Result: metrics.GateFail,
			Detail: fmt.Sprintf("%d out-of-scope path(s): %s", len(bad), strings.Join(bad, ", ")),
		}
	}
	return metrics.Gate{
		Name: GateScope, Result: metrics.GatePass,
		Detail: fmt.Sprintf("%d changed file(s), all owned", len(changed)),
	}
}

func outOfScope(f *config.Fixture, changed []string) []string {
	var bad []string
	for _, p := range changed {
		if strings.HasPrefix(p, ".agentbench-") {
			continue
		}
		if !f.Owns(p) {
			bad = append(bad, p)
		}
	}
	sort.Strings(bad)
	return bad
}

// checkReturnContract asks only for the structural pieces the story requires.
// It deliberately does not read prose: the benchmark score must not depend on
// writing style, so this gate checks that a report exists outside the diff and
// that the commands the story asked for are named in it.
func checkReturnContract(out string) metrics.Gate {
	patch, _, err := executor.ExtractPatch(out)
	if err != nil {
		return metrics.Gate{Name: GateReturnContract, Result: metrics.GateFail,
			Detail: "no diff, so no return contract"}
	}
	report := strings.TrimSpace(strings.Replace(out, patch, "", 1))
	var missing []string
	if len(report) < 40 {
		missing = append(missing, "a report outside the diff")
	}
	lower := strings.ToLower(out)
	for _, want := range []string{"zig build test"} {
		if !strings.Contains(lower, want) {
			missing = append(missing, "the "+want+" result")
		}
	}
	if len(missing) > 0 {
		return metrics.Gate{Name: GateReturnContract, Result: metrics.GateFail,
			Detail: "missing " + strings.Join(missing, "; ")}
	}
	return metrics.Gate{Name: GateReturnContract, Result: metrics.GatePass}
}

// allRequiredPassed is the eligibility rule. A skipped required gate leaves a
// run ineligible, which is the whole reason GateResult is three-valued.
func allRequiredPassed(f *config.Fixture, q *metrics.Quality) bool {
	byName := map[string]metrics.GateResult{}
	for _, g := range q.Gates {
		byName[g.Name] = g.Result
	}
	for _, req := range f.RequiredGates {
		if byName[req] != metrics.GatePass {
			return false
		}
	}
	return true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			if len(l) > 300 {
				return l[:300] + "..."
			}
			return l
		}
	}
	return ""
}
