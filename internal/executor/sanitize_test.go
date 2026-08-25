package executor

import (
	"strings"
	"testing"
)

// A host path in a prompt makes the prompt host-dependent: the hash moves every
// run and prefix reuse dies in a way that looks like an engine problem.
func TestSanitizeRemovesHostPaths(t *testing.T) {
	in := "src/voice/engine.zig:178:38: error: nope\n" +
		"failed: /Users/wcair25/Documents/Github/RigBench/runs/x/work/y/src/voice/engine.zig\n" +
		"cache /home/willy/.cache/zig/p/N-V-abc/zig test\n" +
		"/usr/lib/zig0.14/bin/zig test -ODebug\n"
	got := Sanitize(in, "/Users/wcair25/Documents/Github/RigBench/runs/x/work/y")
	for _, bad := range []string{"/Users/", "/home/", "/usr/lib/zig0.14"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q survived:\n%s", bad, got)
		}
	}
	// The part that matters must survive.
	if !strings.Contains(got, "src/voice/engine.zig:178:38: error: nope") {
		t.Fatalf("the diagnostic was destroyed:\n%s", got)
	}
}

func TestSanitizeRemovesPerRunDetail(t *testing.T) {
	in := "0x1050b83af in expectEqual\nMaxRSS:322M\n342ms\n--seed 0xf4dead11\n-Z1c82bdf236996b2d\n"
	got := Sanitize(in, "")
	for _, bad := range []string{"1050b83af", "322M", "342ms", "f4dead11", "1c82bdf236996b2d"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q survived:\n%s", bad, got)
		}
	}
}

// Sanitizing twice must be the same as once, or a log that passes through two
// turns would drift.
func TestSanitizeIsIdempotent(t *testing.T) {
	in := "err at /home/willy/x/y.zig:1 0xdeadbeef99 342ms MaxRSS:12M"
	once := Sanitize(in, "")
	if twice := Sanitize(once, ""); once != twice {
		t.Fatalf("not idempotent:\n once: %q\ntwice: %q", once, twice)
	}
}

func TestTruncateMiddleKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("A", 100) + strings.Repeat("B", 800) + strings.Repeat("C", 100)
	got := TruncateMiddle(s, 300)
	if len(got) > 400 {
		t.Fatalf("length %d", len(got))
	}
	if !strings.HasPrefix(got, "AAA") {
		t.Fatal("head lost")
	}
	if !strings.HasSuffix(got, "CCC") {
		t.Fatal("tail lost")
	}
	if !strings.Contains(got, "elided by the benchmark harness") {
		t.Fatal("truncation was silent")
	}
}

func TestTruncateMiddleLeavesShortOutputAlone(t *testing.T) {
	s := "short"
	if got := TruncateMiddle(s, 100); got != s {
		t.Fatalf("got %q", got)
	}
}
