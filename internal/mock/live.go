package mock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
)

// LiveScript is a scripted multi-turn candidate for the live builder lane.
//
// The turns are generated from the fixture's own bytes, so a script cannot
// drift out of sync with the repository it patches. Turn 1's diff is computed
// against the tree turn 0 produces, which is what a real incremental repair
// looks like and what the loop's persistent worktree requires.
type LiveScript struct {
	Turns []string
	// Loop repeats the last turn forever once the script is exhausted. Used by
	// the stuck variant, where the point is that the loop terminates on its own
	// budget rather than on the model running out of things to say.
	Loop bool
}

// Reply returns the response for a given number of completed assistant turns.
func (s LiveScript) Reply(turn int) string {
	if turn < len(s.Turns) {
		return s.Turns[turn]
	}
	if s.Loop && len(s.Turns) > 0 {
		return s.Turns[len(s.Turns)-1]
	}
	return "I have nothing further to add.\n"
}

// LiveVariant names a scripted live candidate.
type LiveVariant string

const (
	// LiveConverges is the shape the lane exists to measure: turn 0 lands the
	// new files and wires nothing -- which builds and passes the visible suite,
	// exactly like the one-shot rig result -- and turn 1 repairs it into a
	// wired, tested implementation once the host says what is wrong.
	LiveConverges LiveVariant = "live-converges"
	// LiveStuck returns the same non-repairing diff every turn. The loop must
	// terminate on its turn budget and record why.
	LiveStuck LiveVariant = "live-stuck"
	// LiveNoDiff returns confident prose and never a diff.
	LiveNoDiff LiveVariant = "live-no-diff"
	// LiveImmediateDone declares completion on turn 0 without doing anything.
	// The host must still run the hidden suite and must still fail the story.
	LiveImmediateDone LiveVariant = "live-immediate-done"
	// LiveScopeThenRepair implements everything correctly on turn 0 AND touches
	// one out-of-scope file. Build, visible and the discrimination gate all
	// pass; only scope is red. A loop that stopped on discrimination alone would
	// terminate this story as a failure with the host's own "revert those files"
	// feedback computed and never sent. Turn 1 reverts it.
	LiveScopeThenRepair LiveVariant = "live-scope-then-repair"
	// LiveApplyFailThenRecover sends a corrupted diff on turn 0 and a good one
	// on turn 1. The tree must be unchanged after the failure and the story must
	// still converge.
	LiveApplyFailThenRecover LiveVariant = "live-apply-fail-then-recover"
)

var AllLiveVariants = []LiveVariant{
	LiveConverges, LiveStuck, LiveNoDiff, LiveImmediateDone,
	LiveScopeThenRepair, LiveApplyFailThenRecover,
}

// BuildLiveScript generates the scripted turns for a variant.
func BuildLiveScript(ctx context.Context, f *config.Fixture, v LiveVariant, stageDir string) (LiveScript, error) {
	switch v {
	case LiveNoDiff:
		return LiveScript{Loop: true, Turns: []string{
			"I have implemented the seam. FrameRange is half-open, Tone is cold state,\n" +
				"Hot is unchanged at 72 bytes, and the cursor checks legality before it\n" +
				"commits. The build is green.\n",
		}}, nil
	case LiveImmediateDone:
		return LiveScript{Loop: true, Turns: []string{"DONE\n"}}, nil
	}

	if v == LiveScopeThenRepair {
		return scopeThenRepairScript(ctx, f, stageDir)
	}
	if v == LiveApplyFailThenRecover {
		return applyFailThenRecoverScript(ctx, f, stageDir)
	}

	// Turn 0: the new files, wired into nothing. This is deliberately the
	// `unwired` control -- the shape the one-shot campaign actually produced.
	wt, err := executor.Stage(ctx, f, filepath.Join(stageDir, "t0"), false)
	if err != nil {
		return LiveScript{}, err
	}
	for _, name := range []string{"range.zig", "tone.zig"} {
		b, err := os.ReadFile(filepath.Join(f.Path(f.ReferenceDir), "src/voice", name))
		if err != nil {
			return LiveScript{}, err
		}
		if err := os.WriteFile(filepath.Join(wt.Dir, "src/voice", name), b, 0o644); err != nil {
			return LiveScript{}, err
		}
	}
	t0, err := wt.Diff(ctx)
	if err != nil {
		return LiveScript{}, err
	}

	if v == LiveStuck {
		// The same diff again. It will not apply a second time, which is itself
		// realistic: a model that repeats itself produces a failed apply.
		return LiveScript{Loop: true, Turns: []string{
			fence(t0, "Added the two new modules."),
		}}, nil
	}

	// Turn 1: computed against the tree turn 0 produced. Commit turn 0's state
	// as the baseline, then overlay the full reference and diff.
	if err := wt.Commit(ctx, "turn 0"); err != nil {
		return LiveScript{}, err
	}
	if err := overlay(f.Path(f.ReferenceDir), wt.Dir); err != nil {
		return LiveScript{}, err
	}
	t1, err := wt.Diff(ctx)
	if err != nil {
		return LiveScript{}, err
	}
	if t1 == "" {
		return LiveScript{}, fmt.Errorf("mock: the repair turn is empty; turn 0 already " +
			"produced the reference tree, so the script would prove nothing")
	}

	return LiveScript{Turns: []string{
		fence(t0, "Added src/voice/range.zig and src/voice/tone.zig with the signatures\n"+
			"the story specifies.\nExpecting: zig build test"),
		fence(t1, "Wired range and tone into Cold, moved the cursor onto FrameRange, and\n"+
			"added the invariant tests.\nAddresses: candidate_tests_discriminate\n"+
			"Expecting: zig build test"),
		"DONE\n",
	}}, nil
}

func fence(patch, note string) string {
	return "```diff\n" + patch + "```\n\n" + note + "\n"
}

// scopeThenRepairScript produces a correct implementation that also edits one
// out-of-scope file, then a turn that reverts only that edit.
func scopeThenRepairScript(ctx context.Context, f *config.Fixture, stageDir string) (LiveScript, error) {
	wt, err := executor.Stage(ctx, f, filepath.Join(stageDir, "scope"), false)
	if err != nil {
		return LiveScript{}, err
	}
	if err := overlay(f.Path(f.ReferenceDir), wt.Dir); err != nil {
		return LiveScript{}, err
	}
	// AGENTS.md is explicitly forbidden by the fixture and is not something the
	// build can see. Exactly the defect the scope gate exists for.
	doctrine := filepath.Join(wt.Dir, "AGENTS.md")
	orig, err := os.ReadFile(doctrine)
	if err != nil {
		return LiveScript{}, err
	}
	if err := os.WriteFile(doctrine, append(orig, []byte("\n<!-- clarified while implementing -->\n")...), 0o644); err != nil {
		return LiveScript{}, err
	}
	t0, err := wt.Diff(ctx)
	if err != nil {
		return LiveScript{}, err
	}

	if err := wt.Commit(ctx, "turn 0"); err != nil {
		return LiveScript{}, err
	}
	if err := os.WriteFile(doctrine, orig, 0o644); err != nil {
		return LiveScript{}, err
	}
	t1, err := wt.Diff(ctx)
	if err != nil {
		return LiveScript{}, err
	}
	if t1 == "" {
		return LiveScript{}, fmt.Errorf("mock: the scope repair turn is empty")
	}
	return LiveScript{Turns: []string{
		fence(t0, "Implemented the seam and clarified a line of AGENTS.md while reading it."),
		fence(t1, "Reverted the AGENTS.md edit.\nAddresses: scope"),
		"DONE\n",
	}}, nil
}

// applyFailThenRecoverScript sends a diff whose hunk header is corrupted, then
// a good one. The tree must be untouched by the first.
func applyFailThenRecoverScript(ctx context.Context, f *config.Fixture, stageDir string) (LiveScript, error) {
	wt, err := executor.Stage(ctx, f, filepath.Join(stageDir, "applyfail"), false)
	if err != nil {
		return LiveScript{}, err
	}
	if err := overlay(f.Path(f.ReferenceDir), wt.Dir); err != nil {
		return LiveScript{}, err
	}
	good, err := wt.Diff(ctx)
	if err != nil {
		return LiveScript{}, err
	}
	return LiveScript{Turns: []string{
		fence(corruptHunkHeader(good), "Implemented the seam."),
		fence(good, "Resent the diff after the host reported it did not apply.\nAddresses: patch_applies"),
		"DONE\n",
	}}, nil
}
