package executor

import (
	"strings"
	"testing"
)

const minimalDiff = `diff --git a/src/voice/range.zig b/src/voice/range.zig
new file mode 100644
--- /dev/null
+++ b/src/voice/range.zig
@@ -0,0 +1,2 @@
+pub const FrameRange = struct { start: u64, end: u64 };
+
`

func TestExtractPatchFromTaggedFence(t *testing.T) {
	out := "Here is the change.\n\n```diff\n" + minimalDiff + "```\n\nAnd the report.\n"
	p, form, err := ExtractPatch(out)
	if err != nil {
		t.Fatal(err)
	}
	if form != "fenced-tagged" {
		t.Fatalf("form = %q", form)
	}
	if !strings.HasPrefix(p, "diff --git") || !strings.HasSuffix(p, "\n") {
		t.Fatalf("patch not normalized: %q", p[:min(40, len(p))])
	}
}

func TestExtractPatchFromUntaggedFence(t *testing.T) {
	out := "```\n" + minimalDiff + "```\n"
	_, form, err := ExtractPatch(out)
	if err != nil {
		t.Fatal(err)
	}
	if form != "fenced-untagged" {
		t.Fatalf("form = %q", form)
	}
}

func TestExtractPatchFromBareDiff(t *testing.T) {
	_, form, err := ExtractPatch("Report first.\n\n" + minimalDiff)
	if err != nil {
		t.Fatal(err)
	}
	if form != "bare" {
		t.Fatalf("form = %q", form)
	}
}

// A four-backtick fence is how a diff that itself contains a triple fence gets
// quoted. Go's regexp has no backreference, so this exercises the hand-rolled
// scanner's matching of a closing fence to its opening one.
func TestExtractPatchFromLongFenceContainingATripleFence(t *testing.T) {
	inner := minimalDiff + "+// ```\n"
	out := "````diff\n" + inner + "````\n"
	p, form, err := ExtractPatch(out)
	if err != nil {
		t.Fatal(err)
	}
	if form != "fenced-tagged" {
		t.Fatalf("form = %q", form)
	}
	if !strings.Contains(p, "+// ```") {
		t.Fatal("the inner triple fence was truncated")
	}
}

func TestExtractPatchRefusesProseThatMerelyTalksAboutDiffs(t *testing.T) {
	out := "I produced a diff for src/voice/range.zig with the @@ hunks you asked for.\n" +
		"```zig\npub const FrameRange = struct {};\n```\n"
	if _, _, err := ExtractPatch(out); err == nil {
		t.Fatal("prose about a diff was accepted as a diff")
	}
}

func TestExtractPatchRefusesADiffHeaderWithNoHunk(t *testing.T) {
	out := "```diff\n--- a/x\n+++ b/x\n```\n"
	if _, _, err := ExtractPatch(out); err == nil {
		t.Fatal("a header with no hunk was accepted")
	}
}

func TestPatchFilesDropsDevNull(t *testing.T) {
	got := PatchFiles(minimalDiff)
	if len(got) != 1 || got[0] != "src/voice/range.zig" {
		t.Fatalf("got %v", got)
	}
}

func TestPatchFilesFallsBackToMinusPlusHeaders(t *testing.T) {
	p := "--- a/one.zig\n+++ b/one.zig\n@@ -1 +1 @@\n-a\n+b\n" +
		"--- a/two.zig\n+++ b/two.zig\n@@ -1 +1 @@\n-a\n+b\n"
	got := PatchFiles(p)
	if len(got) != 2 || got[0] != "one.zig" || got[1] != "two.zig" {
		t.Fatalf("got %v", got)
	}
}

// A parser that returned 0 for "no count found" would be indistinguishable from
// a suite that ran and passed nothing, which is exactly the confusion the
// anti-vacuity discipline exists to prevent.
func TestParseZigTestCountsReturnsNilWhenAbsent(t *testing.T) {
	p, f := ParseZigTestCounts("Build Summary: 3/3 steps succeeded\n")
	if p != nil || f != nil {
		t.Fatalf("want nil, nil for output with no test count; got %v %v", p, f)
	}
}

func TestParseZigTestCountsReadsTheSummaryLine(t *testing.T) {
	p, f := ParseZigTestCounts("Build Summary: 7/7 steps succeeded; 62/62 tests passed\n")
	if p == nil || *p != 62 {
		t.Fatalf("passed = %v", p)
	}
	if f == nil || *f != 0 {
		t.Fatalf("failed = %v", f)
	}
}

func TestParseZigTestCountsSumsPerArtifactLines(t *testing.T) {
	p, f := ParseZigTestCounts("+- run test 9 pass (9 total)\n+- run test 43 pass, 1 fail (44 total)\n")
	if p == nil || *p != 52 {
		t.Fatalf("passed = %v", p)
	}
	if f == nil || *f != 1 {
		t.Fatalf("failed = %v", f)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
