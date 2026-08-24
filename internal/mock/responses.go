package mock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
)

// Variant names a canned candidate response.
type Variant string

const (
	// Reference is the gold solution. Every gate passes.
	Reference Variant = "reference"
	// VisibleGreenHiddenRed is the reference with the "coefficient resolved
	// inside the frame loop" mutation. It builds, and the visible suite passes,
	// and the hidden structural control refuses it. This is the variant that
	// demonstrates why the hidden suite exists.
	VisibleGreenHiddenRed Variant = "visible-green-hidden-red"
	// Broken fails the hidden invariant suite behaviourally: the range end is
	// treated as inclusive.
	Broken Variant = "broken"
	// ScopeViolation is a correct patch that also edits the frozen golden.
	ScopeViolation Variant = "scope-violation"
	// Unapplyable is a diff with a corrupted hunk header.
	Unapplyable Variant = "unapplyable"
	// NoDiff is a plausible-sounding report with no diff in it at all.
	NoDiff Variant = "no-diff"
)

// AllVariants is the order the self-test exercises them in.
var AllVariants = []Variant{
	Reference, VisibleGreenHiddenRed, Broken, ScopeViolation, Unapplyable, NoDiff,
}

// BuildResponse produces a canned candidate response for a fixture.
//
// The diffs are generated from the fixture's own bytes at call time rather than
// stored, so a canned response cannot drift out of sync with the frozen
// repository. That also means this function needs a staging directory and the
// git and zig-adjacent plumbing the executor already owns.
func BuildResponse(ctx context.Context, f *config.Fixture, v Variant, stageDir string) (string, error) {
	if v == NoDiff {
		return noDiffReport, nil
	}

	patch, err := referenceDiff(ctx, f, stageDir, v)
	if err != nil {
		return "", err
	}
	if v == Unapplyable {
		patch = corruptHunkHeader(patch)
	}
	return fmt.Sprintf("```diff\n%s```\n\n%s", patch, reportFor(v)), nil
}

// referenceDiff stages HEAD, overlays the reference solution, applies any
// mutation the variant calls for, and returns the resulting unified diff.
func referenceDiff(ctx context.Context, f *config.Fixture, stageDir string, v Variant) (string, error) {
	wt, err := executor.Stage(ctx, f, stageDir, false)
	if err != nil {
		return "", err
	}
	if err := overlay(f.Path(f.ReferenceDir), wt.Dir); err != nil {
		return "", err
	}

	switch v {
	case VisibleGreenHiddenRed:
		if err := applyNamedMutant(f, wt, "m04-coefficient-inside-frame-loop"); err != nil {
			return "", err
		}
	case Broken:
		if err := applyNamedMutant(f, wt, "m01-range-end-inclusive"); err != nil {
			return "", err
		}
	case ScopeViolation:
		// A correct patch that also relaxes the frozen golden -- the shape of a
		// change that "passes" by editing the oracle.
		p := filepath.Join(wt.Dir, "products/player/golden.zig")
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		s := strings.Replace(string(b),
			"pub const reference_written: usize = 86;",
			"pub const reference_written: usize = 86; // widened by the candidate",
			1)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			return "", err
		}
	}

	return wt.Diff(ctx)
}

func applyNamedMutant(f *config.Fixture, wt *executor.Worktree, id string) error {
	ms, err := executor.LoadMutants(f)
	if err != nil {
		return err
	}
	for _, mu := range ms.Mutants {
		if mu.ID == id {
			return wt.ApplyEdits(mu.Edits)
		}
	}
	return fmt.Errorf("mock: fixture has no mutant %q", id)
}

func overlay(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// corruptHunkHeader breaks the first hunk's line counts so `git apply` refuses
// the patch. The diff still *looks* like a diff, which is the point: the gate
// under test is "applies cleanly", not "contains the word diff".
func corruptHunkHeader(patch string) string {
	lines := strings.Split(patch, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "@@ ") {
			lines[i] = "@@ -1,9999 +1,9999 @@"
			break
		}
	}
	return strings.Join(lines, "\n")
}

func reportFor(v Variant) string {
	common := "" +
		"Files changed: src/voice/range.zig (new), src/voice/tone.zig (new),\n" +
		"src/voice/cursor.zig, src/voice/engine.zig, src/voice/root.zig,\n" +
		"src/voice/range_test.zig (new), src/voice/tone_test.zig (new),\n" +
		"src/voice/cursor_test.zig, src/voice/engine_test.zig.\n\n" +
		"Invariants demonstrated: half-open range validity and refusal by name;\n" +
		"forward end exclusion and reverse start exclusion; refused-advance\n" +
		"non-mutation; default Tone byte compatibility; explicit bypass as an\n" +
		"exact zero coefficient; explicit coefficient override; non-finite gain\n" +
		"and coefficient refusals; Hot still 72 bytes at unchanged offsets.\n\n" +
		"Commands run:\n" +
		"  zig build test          exit 0\n" +
		"  zig build test-release   exit 0\n\n" +
		"Unmeasured: whether the coefficient resolution is observably outside the\n" +
		"frame loop; ordinary tests cannot see where a computation happened, so\n" +
		"that criterion rests on the structural control rather than on these tests.\n"

	switch v {
	case VisibleGreenHiddenRed:
		return common + "\nNote: the coefficient is resolved per frame for clarity. The output is\n" +
			"bit-identical, so this is a readability choice rather than a behaviour change.\n"
	case Broken:
		return common
	case ScopeViolation:
		return common + "\nNote: also annotated products/player/golden.zig, which was needed to make\n" +
			"the frozen value legible.\n"
	case Unapplyable:
		return common
	}
	return common
}

const noDiffReport = `I have implemented the S2 playback seam.

FrameRange is now a half-open interval with full() and validate(), Tone carries
gain and a three-case filter override, and both live in Cold so Hot is untouched
at 72 bytes. The cursor checks legality before committing, forward playback stops
before range.end and reverse never reads below range.start. Invalid ranges and
out-of-domain coefficients are refused by name.

Commands run:
  zig build test          exit 0
  zig build test-release  exit 0

All invariants are covered and the Player golden is unchanged.
`
