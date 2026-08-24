package scoring

import (
	"testing"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

func req(names ...string) *config.Fixture {
	return &config.Fixture{RequiredGates: names}
}

func gates(pairs ...any) *metrics.Quality {
	q := &metrics.Quality{}
	for i := 0; i < len(pairs); i += 2 {
		q.Gates = append(q.Gates, metrics.Gate{
			Name:   pairs[i].(string),
			Result: pairs[i+1].(metrics.GateResult),
		})
	}
	return q
}

func TestAllRequiredPassed(t *testing.T) {
	f := req(GateBuild, GateHiddenTests)
	if !allRequiredPassed(f, gates(GateBuild, metrics.GatePass, GateHiddenTests, metrics.GatePass)) {
		t.Fatal("all-pass was not eligible")
	}
	if allRequiredPassed(f, gates(GateBuild, metrics.GatePass, GateHiddenTests, metrics.GateFail)) {
		t.Fatal("a failing required gate was eligible")
	}
}

// The three-valued gate result exists for exactly this: a rung that could not
// run must not be scored as a rung that passed. Losing this makes a machine
// without the toolchain report green.
func TestASkippedRequiredGateIsNotEligible(t *testing.T) {
	f := req(GateBuild, GateHiddenTests)
	q := gates(GateBuild, metrics.GatePass, GateHiddenTests, metrics.GateSkipped)
	if allRequiredPassed(f, q) {
		t.Fatal("a skipped required gate was scored as eligible")
	}
}

// A gate the fixture never declared required must not be able to sink a run,
// and a required gate that never appeared at all must.
func TestAMissingRequiredGateIsNotEligible(t *testing.T) {
	f := req(GateBuild, GateScope)
	if allRequiredPassed(f, gates(GateBuild, metrics.GatePass)) {
		t.Fatal("a required gate that was never recorded was treated as passing")
	}
	f2 := req(GateBuild)
	if !allRequiredPassed(f2, gates(GateBuild, metrics.GatePass, GateScope, metrics.GateFail)) {
		t.Fatal("a non-required failing gate blocked eligibility")
	}
}

func TestOutOfScopeIgnoresTheRunnersOwnScratchFile(t *testing.T) {
	f := &config.Fixture{OwnedFiles: []string{"src/voice/engine.zig"}}
	got := outOfScope(f, []string{"src/voice/engine.zig", ".agentbench-candidate.patch"})
	if len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestOutOfScopeNamesEveryOffender(t *testing.T) {
	f := &config.Fixture{
		OwnedFiles:     []string{"src/voice/engine.zig"},
		ForbiddenPaths: []string{"products/**"},
	}
	got := outOfScope(f, []string{"products/player/golden.zig", "build.zig", "src/voice/engine.zig"})
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestReturnContractRefusesAReportWithNoDiff(t *testing.T) {
	g := checkReturnContract("I implemented everything and ran zig build test, exit 0.")
	if g.Result != metrics.GateFail {
		t.Fatalf("got %s", g.Result)
	}
}

func TestReturnContractRefusesADiffWithNoReport(t *testing.T) {
	out := "```diff\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n```\n"
	g := checkReturnContract(out)
	if g.Result != metrics.GateFail {
		t.Fatalf("a bare diff with no report passed: %+v", g)
	}
}

func TestReturnContractAcceptsADiffPlusTheCommandsTheStoryAsked(t *testing.T) {
	out := "```diff\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n```\n\n" +
		"Changed src/voice/engine.zig. Invariants: half-open range refusal.\n" +
		"Commands: zig build test exit 0; zig build test-release exit 0.\n" +
		"Unmeasured: where the coefficient was computed.\n"
	g := checkReturnContract(out)
	if g.Result != metrics.GatePass {
		t.Fatalf("got %s: %s", g.Result, g.Detail)
	}
}
