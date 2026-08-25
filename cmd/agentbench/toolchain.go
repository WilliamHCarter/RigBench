package main

import (
	"context"
	"fmt"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
)

// checkToolchain compares the compiler that will actually run against the one
// the fixture was verified under.
//
// The pin was recorded before this existed and never checked, and the cost of
// that showed up on the rig: the fixture declares Zig 0.16.0 and a campaign ran
// under 0.14. One standard-library formatting call behaves differently between
// them, so the frozen Player golden was being computed two different ways on
// two machines. Nothing in the run said so.
//
// A fixture's goldens, mutant controls and hidden suite are only evidence under
// the compiler that produced them. So this is exact-match and it refuses, in the
// same spirit as engine identity: a result whose label is unverifiable is worse
// than no result.
func checkToolchain(ctx context.Context, f *config.Fixture, allowDrift bool) (actual string, err error) {
	want := f.Toolchain.Zig
	actual = executor.ZigVersion(ctx, f.Path(f.RepoDir))

	if want == "" {
		return actual, nil
	}
	if actual == "" {
		if allowDrift {
			return actual, nil
		}
		return actual, fmt.Errorf("fixture %s pins zig %s but the resolved compiler "+
			"could not be determined in %s. Install the pinned toolchain, or pass "+
			"-allow-toolchain-drift to record the run as unverifiable",
			f.ID, want, f.Path(f.RepoDir))
	}
	if actual != want {
		if allowDrift {
			return actual, nil
		}
		return actual, fmt.Errorf("fixture %s was verified under zig %s but zig %s will "+
			"run here.\n"+
			"Its goldens, its twelve mutant controls and its hidden suite are evidence "+
			"only under the compiler that produced them, and results from two compilers "+
			"are not comparable.\n"+
			"Either install zig %s, or re-verify the fixture under %s "+
			"(`agentbench verify-fixture`) and update fixture.json's toolchain.zig "+
			"deliberately, or pass -allow-toolchain-drift to record the run with that "+
			"caveat attached",
			f.ID, want, actual, want, actual)
	}
	return actual, nil
}
