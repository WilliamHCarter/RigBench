package scoring

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

// Live scoring differs from the one-shot path in one structural way: the
// worktree persists across turns, and the hidden suite is injected exactly
// once, when the loop has stopped.
//
// That ordering is the whole integrity of the lane. If the hidden suite were in
// the tree during the loop, `zig build test` would compile it, its failures
// would leak into the tool output the model reads, and the benchmark would be
// handing over its own oracle. The diagnostic lane leaks it deliberately and
// says so in its own name; the canonical lane must not leak it by accident.

// TurnInput is one live turn's scoring request.
type TurnInput struct {
	Fixture  *config.Fixture
	Worktree *executor.Worktree
	// Output is the model's visible text for this turn.
	Output string
	// Turn is the 0-based turn index, used for artifact naming.
	Turn int

	ArtifactDir    string
	ArtifactPrefix string
	// DiscriminateDir stages a clean tree for the candidate-test gate.
	DiscriminateDir string
	// MaxToolOutputBytes truncates the log that goes back to the model.
	MaxToolOutputBytes int
}

// TurnResult is what one live turn produced.
type TurnResult struct {
	Gates []metrics.Gate
	// Feedback is the sanitized, truncated tool output to append to the prompt.
	// It is what the model sees, and it is never the raw log.
	Feedback string
	// Patch is the extracted diff, empty when the model returned none.
	Patch      string
	PatchFiles []string
	Applied    bool
	// Done is set when the model declared completion instead of returning a diff.
	Done bool
	// ToolWall is the host-side time this turn cost.
	ToolWall time.Duration
	// Compiles and DiscriminatingGreen are milestone flags the story records.
	//
	// DiscriminatingGreen deliberately excludes scope: it marks the turn at
	// which the implementation and its tests became load-bearing, which is a
	// question about the code and not about which files were touched.
	Compiles            bool
	DiscriminatingGreen bool
	// ReadyForFinal is the stop predicate, and it is NOT the same flag.
	//
	// Story success requires build, visible, discrimination, scope AND hidden.
	// Stopping on DiscriminatingGreen alone would terminate a story whose only
	// remaining defect is a stray out-of-scope edit -- with the host's own
	// "revert those files" feedback computed, formatted, and then never sent.
	// That is precisely the repair the live loop exists to enable.
	ReadyForFinal bool
}

const doneSentinel = "DONE"

// ScoreLiveTurn applies this turn's diff to the persistent worktree, runs the
// visible rungs, and produces the feedback the model will see next.
//
// The hidden suite is NOT run here.
func ScoreLiveTurn(ctx context.Context, in TurnInput) (*TurnResult, error) {
	res := &TurnResult{}
	start := time.Now()
	defer func() { res.ToolWall = time.Since(start) }()

	if err := os.MkdirAll(in.ArtifactDir, 0o755); err != nil {
		return nil, err
	}
	writeLog := logWriter(in.ArtifactDir, in.ArtifactPrefix, fmt.Sprintf("t%d.", in.Turn))

	// A model that declares completion is taken at its word; the host still has
	// the final say, because the hidden suite runs afterwards regardless.
	if declaresDone(in.Output) {
		res.Done = true
		res.Feedback = ""
		return res, nil
	}

	patch, form, err := executor.ExtractPatch(in.Output)
	if err != nil {
		res.Gates = append(res.Gates, metrics.Gate{
			Name: GatePatchExtracted, Result: metrics.GateFail, Detail: err.Error(),
		})
		res.Feedback = "HOST: no unified diff was found in your reply, and you did not " +
			"return DONE. Nothing was applied and the tree is unchanged.\n" +
			"Return only a fenced ```diff block, or DONE if the work is complete.\n"
		return res, nil
	}
	res.Patch = patch
	res.PatchFiles = executor.PatchFiles(patch)
	res.Gates = append(res.Gates, metrics.Gate{
		Name: GatePatchExtracted, Result: metrics.GatePass,
		Detail: fmt.Sprintf("%s, %d bytes", form, len(patch)),
	})
	_ = os.WriteFile(filepath.Join(in.ArtifactDir,
		fmt.Sprintf("t%d.candidate.patch", in.Turn)), []byte(patch), 0o644)

	ap := in.Worktree.ApplyPatch(ctx, patch)
	applyArt := writeLog("git-apply", ap)
	if !ap.OK() {
		res.Gates = append(res.Gates, metrics.Gate{
			Name: GatePatchApplies, Result: metrics.GateFail,
			Detail: firstLine(ap.Combined()), Command: strings.Join(ap.Argv, " "),
			ExitCode: metrics.Ptr(ap.ExitCode), Artifact: applyArt,
		})
		// The tree is unchanged, and the model is told so explicitly: without
		// that it may assume its previous turn landed and send a delta against
		// a tree that never existed.
		res.Feedback = executor.TruncateMiddle(
			"HOST: your diff did not apply. The working tree is UNCHANGED and still "+
				"reflects your previous turns only.\n\n$ git apply\n"+
				executor.Sanitize(ap.Combined(), in.Worktree.Dir)+"\n",
			in.MaxToolOutputBytes)
		return res, nil
	}
	res.Applied = true
	res.Gates = append(res.Gates, metrics.Gate{
		Name: GatePatchApplies, Result: metrics.GatePass,
		Command: strings.Join(ap.Argv, " "), ExitCode: metrics.Ptr(0), Artifact: applyArt,
	})

	changed, _ := in.Worktree.ChangedFiles(ctx)
	scope := scopeGate(ctx, BuilderInput{Fixture: in.Fixture, Worktree: in.Worktree}, changed)
	res.Gates = append(res.Gates, scope)

	timeout := in.Fixture.CommandTimeout()
	var feedback strings.Builder

	build := executor.Run(ctx, in.Worktree.Dir, in.Fixture.Commands.Build, timeout)
	res.Gates = append(res.Gates, rungGate(GateBuild, build, writeLog("build", build)))
	res.Compiles = build.OK()
	appendRung(&feedback, in, build)

	if !build.OK() {
		res.Feedback = trimFeedback(feedback.String(), scope, in)
		return res, nil
	}

	visible := executor.Run(ctx, in.Worktree.Dir, in.Fixture.Commands.Visible, timeout)
	vg := rungGate(GateVisibleTests, visible, writeLog("visible-tests", visible))
	res.Gates = append(res.Gates, vg)
	appendRung(&feedback, in, visible)

	// The discrimination gate runs every turn, because "your tests still pass
	// against a tree without the seam" is exactly the feedback a model needs in
	// order to stop believing it is finished.
	dg := discriminateGate(ctx, BuilderInput{
		Fixture: in.Fixture, Worktree: in.Worktree,
		DiscriminateDir: in.DiscriminateDir,
	}, changed, writeLog)
	res.Gates = append(res.Gates, dg)
	res.DiscriminatingGreen = visible.OK() && dg.Result == metrics.GatePass
	res.ReadyForFinal = res.DiscriminatingGreen &&
		build.OK() && scope.Result == metrics.GatePass
	if dg.Result == metrics.GateFail {
		fmt.Fprintf(&feedback, "\nHOST CHECK: %s\n%s\n",
			GateCandidateTestsDiscriminate, dg.Detail)
	}

	res.Feedback = trimFeedback(feedback.String(), scope, in)
	return res, nil
}

// ScoreHiddenFinal injects the hidden suite and runs it exactly once.
func ScoreHiddenFinal(ctx context.Context, in TurnInput) (metrics.Gate, string, error) {
	writeLog := logWriter(in.ArtifactDir, in.ArtifactPrefix, "final.")
	if err := in.Worktree.InjectHidden(ctx); err != nil {
		return metrics.Gate{
			Name: GateHiddenTests, Result: metrics.GateSkipped,
			Detail: "could not inject the hidden suite: " + err.Error(),
		}, "", err
	}
	r := executor.Run(ctx, in.Worktree.Dir, in.Fixture.Commands.Hidden, in.Fixture.CommandTimeout())
	g := rungGate(GateHiddenTests, r, writeLog("hidden-tests", r))
	if p, _ := executor.ParseZigTestCounts(r.Combined()); p != nil {
		g.Detail = strings.TrimSpace(fmt.Sprintf("%s %d tests passed", g.Detail, *p))
	}
	raw := executor.TruncateMiddle(
		executor.Sanitize(r.Combined(), in.Worktree.Dir), in.MaxToolOutputBytes)
	return g, raw, nil
}

// declaresDone accepts the sentinel only when it is the whole of the reply, or
// stands alone on its own line. A model that writes "I am not DONE yet" has not
// declared completion.
func declaresDone(out string) bool {
	if _, _, err := executor.ExtractPatch(out); err == nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == doneSentinel {
			return true
		}
	}
	return false
}

// appendRung adds one command's result to the feedback under construction.
//
// It does not truncate: the cap belongs to the whole feedback block and is
// applied once, by trimFeedback. Truncating per command let a two-command turn
// send twice the configured ceiling.
func appendRung(b *strings.Builder, in TurnInput, r *executor.CommandResult) {
	fmt.Fprintf(b, "$ %s\nexit %d\n\n%s\n",
		strings.Join(r.Argv, " "), r.ExitCode,
		executor.Sanitize(strings.TrimSpace(r.Combined()), in.Worktree.Dir))
}

// trimFeedback prepends a scope violation, which the model must be told about
// even when everything compiled: a patch that edits a frozen golden is a
// failure the build cannot see.
// trimFeedback prepends a scope violation and applies the single output cap.
//
// The scope note goes first, and before truncation, because a patch that edits
// a frozen golden is a failure the build cannot see -- it must not be the thing
// that gets elided out of the middle of a long build log.
//
// The cap is the lane's max_tool_output_bytes over the WHOLE block. It used to
// be applied per command and then again at double the ceiling, so a 16 KiB lane
// could send 32 KiB. Tool-result size drives the next turn's prompt size,
// prefill work, cache shape and wall clock, so a cap that is not the configured
// cap is a measurement error, not a formatting detail.
func trimFeedback(body string, scope metrics.Gate, in TurnInput) string {
	if scope.Result == metrics.GateFail {
		body = fmt.Sprintf("HOST CHECK: scope violation. %s\nThose files are not yours to "+
			"change; revert them.\n\n%s", scope.Detail, body)
	}
	return executor.TruncateMiddle(body, in.MaxToolOutputBytes)
}

func logWriter(dir, prefix, turnPrefix string) func(string, *executor.CommandResult) string {
	return func(name string, r *executor.CommandResult) string {
		if r == nil {
			return ""
		}
		rel := turnPrefix + name + ".log"
		body := fmt.Sprintf("$ %s\n(cwd %s)\nexit %d  duration %s  timed_out=%v unavailable=%v\n\n%s\n",
			strings.Join(r.Argv, " "), r.Dir, r.ExitCode, r.Duration.Round(time.Millisecond),
			r.TimedOut, r.Unavailable, r.Combined())
		if os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644) != nil {
			return ""
		}
		return filepath.ToSlash(filepath.Join(prefix, rel))
	}
}
