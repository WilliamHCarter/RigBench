package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/mock"
)

// v0.3 acceptance.
//
// Two things are being proved. First, that the loop terminates correctly on
// every path -- converge, stick, refuse to diff, declare done -- and records
// which one happened, because a story that ran out of turns and one that
// finished are not the same result. Second, and more important, that the
// canonical lane does not leak its own oracle: if the hidden suite is in the
// tree while the loop runs, `zig build test` compiles it, its failures reach
// the model through the tool output, and the benchmark is grading a model on
// evidence it handed over.

type liveExpectation struct {
	variant     mock.LiveVariant
	lane        string
	hiddenGreen bool
	stopReason  string
	// minTurns and maxTurns bound the loop length.
	minTurns, maxTurns int
	why                string
	// check is an extra assertion on the finished story.
	check func(*metrics.Story) error
}

var liveAcceptance = []liveExpectation{
	{
		variant: mock.LiveConverges, lane: "builder-live",
		hiddenGreen: true, stopReason: stopDiscriminatingGreen, minTurns: 2, maxTurns: 2,
		why: "turn 0 lands the new files and wires nothing -- which builds and passes the\n" +
			"visible suite, exactly like the one-shot rig result -- and turn 1 repairs it\n" +
			"once the host reports the discrimination failure",
		check: func(s *metrics.Story) error {
			if s.TimeToFirstCompilingPatchMS == nil {
				return fmt.Errorf("no first-compile milestone")
			}
			if s.TimeToDiscriminatingGreenMS == nil {
				return fmt.Errorf("no discriminating-green milestone")
			}
			if s.TurnAtDiscriminatingGreen == nil || *s.TurnAtDiscriminatingGreen != 1 {
				return fmt.Errorf("discriminating green at turn %v, want 1", s.TurnAtDiscriminatingGreen)
			}
			// Turn 0 must NOT have been discriminating green, or the fixture is
			// not reproducing the failure this lane exists to study.
			if len(s.Turns) > 0 {
				g := metrics.GateByName(s.Turns[0].Gates, "candidate_tests_discriminate")
				if g == nil || g.Result != metrics.GateFail {
					return fmt.Errorf("turn 0's discrimination gate was %v, want fail", g)
				}
			}
			if s.TotalWallMS <= 0 || s.ModelWallMS <= 0 || s.ToolWallMS <= 0 {
				return fmt.Errorf("wall clock not decomposed: total=%.0f model=%.0f tool=%.0f",
					s.TotalWallMS, s.ModelWallMS, s.ToolWallMS)
			}
			return nil
		},
	},
	{
		variant: mock.LiveStuck, lane: "builder-live",
		hiddenGreen: false, stopReason: stopMaxTurnsReason, minTurns: 2, maxTurns: 99,
		why: "a model that repeats itself must exhaust its turn budget and say so, not\n" +
			"loop forever and not be recorded as anything other than a failure",
		check: func(s *metrics.Story) error {
			if s.FailedApplyAttempts == 0 {
				return fmt.Errorf("re-sending an applied diff produced no failed apply")
			}
			return nil
		},
	},
	{
		variant: mock.LiveNoDiff, lane: "builder-live",
		hiddenGreen: false, stopReason: stopRepeatedNoDiffReason, minTurns: 2, maxTurns: 2,
		why: "confident prose with no diff must stop the loop rather than burning the budget",
		check: func(s *metrics.Story) error {
			if s.NoDiffTurns < 2 {
				return fmt.Errorf("no-diff turns = %d", s.NoDiffTurns)
			}
			if s.PatchAttempts != 0 {
				return fmt.Errorf("patch attempts = %d, want 0", s.PatchAttempts)
			}
			return nil
		},
	},
	{
		variant: mock.LiveImmediateDone, lane: "builder-live",
		hiddenGreen: false, stopReason: stopDoneReason, minTurns: 1, maxTurns: 1,
		why: "a model declaring completion does not make the story green: the host still\n" +
			"runs the hidden suite, and it still fails",
		check: func(s *metrics.Story) error {
			g := metrics.GateByName(s.FinalGates, "hidden_tests")
			if g == nil {
				return fmt.Errorf("the hidden suite was never run on a DONE story")
			}
			if g.Result != metrics.GateFail {
				return fmt.Errorf("hidden gate is %s on an empty tree", g.Result)
			}
			return nil
		},
	},
	{
		variant: mock.LiveConverges, lane: "builder-repair-diagnostic",
		hiddenGreen: true, stopReason: stopDoneReason, minTurns: 2, maxTurns: 3,
		why: "the diagnostic lane returns the hidden suite's real failure after turn 0 and\n" +
			"records that it did, so its stories can never be quoted as builder-live ones",
		check: func(s *metrics.Story) error {
			if s.HiddenLeakedAfterTurn == nil {
				return fmt.Errorf("the diagnostic lane did not record that it leaked")
			}
			return nil
		},
	},
}

// Stop-reason constants mirrored here so the expectations read as data.
const (
	stopDiscriminatingGreen  = "visible rung and discrimination gate both green"
	stopMaxTurnsReason       = "turn budget exhausted"
	stopRepeatedNoDiffReason = "the model returned no diff twice in a row"
	stopDoneReason           = "model declared done"
)

func acceptV03(ctx context.Context, fixtureDir, layout, engines, root string, timeScale float64) error {
	f, err := config.LoadFixture(fixtureDir)
	if err != nil {
		return err
	}

	for _, exp := range liveAcceptance {
		fmt.Printf("== %s on %s ==\n%s\n", exp.variant, exp.lane, indent(exp.why))

		stage, err := os.MkdirTemp("", "agentbench-v03-")
		if err != nil {
			return err
		}
		script, err := mock.BuildLiveScript(ctx, f, exp.variant, filepath.Join(stage, "live"))
		if err != nil {
			os.RemoveAll(stage)
			return err
		}
		srv := &mock.Server{
			TimeScale: timeScale, ProfileFor: profileFromRequest,
			Respond: func(i mock.RequestInfo) (string, string) {
				return script.Reply(i.AssistantTurns), ""
			},
		}
		ln, shutdown, err := srv.Listen("127.0.0.1:0")
		if err != nil {
			os.RemoveAll(stage)
			return err
		}
		subdir := fmt.Sprintf("%s.%s", exp.lane, exp.variant)
		runDir, runErr := doRun(ctx, &runFlags{
			fixtureDir: fixtureDir, engines: engines, layout: layout,
			contextPack: "base", thermal: "cold", runsDir: root,
			runID: "v03-" + shortHash(subdir), runSubdir: subdir,
			endpoint: fmt.Sprintf("http://%s/v1", ln.Addr()), repeats: 1,
			lane: exp.lane,
			caveats: []string{"Scripted live candidate against the in-repo mock; " +
				"timings are not measurements."},
		})
		_ = shutdown(ctx)
		os.RemoveAll(stage)
		if runErr != nil {
			return runErr
		}

		stories, err := loadStories(filepath.Join(runDir, "story.json"))
		if err != nil {
			return err
		}
		if len(stories) != 1 {
			return fmt.Errorf("%s: want one story, got %d", exp.variant, len(stories))
		}
		s := stories[0]

		if s.HiddenGreen != exp.hiddenGreen {
			return fmt.Errorf("%s on %s: hidden_green=%v, want %v (stop reason: %s)",
				exp.variant, exp.lane, s.HiddenGreen, exp.hiddenGreen, s.StopReason)
		}
		if s.StopReason != exp.stopReason {
			return fmt.Errorf("%s on %s: stop reason %q, want %q",
				exp.variant, exp.lane, s.StopReason, exp.stopReason)
		}
		if s.ModelTurns < exp.minTurns || s.ModelTurns > exp.maxTurns {
			return fmt.Errorf("%s on %s: %d turns, want %d..%d",
				exp.variant, exp.lane, s.ModelTurns, exp.minTurns, exp.maxTurns)
		}
		if exp.check != nil {
			if err := exp.check(s); err != nil {
				return fmt.Errorf("%s on %s: %w", exp.variant, exp.lane, err)
			}
		}

		leaked, err := hiddenLeakedInto(runDir)
		if err != nil {
			return err
		}
		wantLeak := exp.lane == "builder-repair-diagnostic"
		if leaked && !wantLeak {
			return fmt.Errorf("%s on %s: the hidden suite's content reached a prompt in "+
				"the canonical lane. The benchmark would be grading the model on evidence "+
				"it handed over", exp.variant, exp.lane)
		}
		if !leaked && wantLeak {
			return fmt.Errorf("%s on %s: the diagnostic lane never actually showed the "+
				"model the hidden failure, so the leak check above proves nothing",
				exp.variant, exp.lane)
		}

		fmt.Printf("  hidden_green=%-5v  stop=%-45s  turns=%d  leak=%v\n",
			s.HiddenGreen, s.StopReason, s.ModelTurns, leaked)
		fmt.Printf("  total %.1fs = model %.1fs + tool %.1fs   first compile %s   discriminating %s\n\n",
			s.TotalWallMS/1000, s.ModelWallMS/1000, s.ToolWallMS/1000,
			milestone(s.TimeToFirstCompilingPatchMS, s.TurnAtFirstCompile),
			milestone(s.TimeToDiscriminatingGreenMS, s.TurnAtDiscriminatingGreen))
	}
	return nil
}

// hiddenMarkers are strings that appear only in the hidden suite. If one shows
// up in a prompt, the oracle reached the model.
var hiddenMarkers = []string{
	"hidden/invariants_test.zig",
	"hidden/layout_test.zig",
	"hidden/structural_test.zig",
	"hot_frozen",
	"forbidden in frame loop",
}

// hiddenLeakedInto scans every prompt a run sent for hidden-suite content.
func hiddenLeakedInto(runDir string) (bool, error) {
	found := false
	err := filepath.WalkDir(filepath.Join(runDir, "artifacts"),
		func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Base(path) != "prompt.txt" {
				return err
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, m := range hiddenMarkers {
				if strings.Contains(string(b), m) {
					found = true
					return nil
				}
			}
			return nil
		})
	if os.IsNotExist(err) {
		return false, nil
	}
	return found, err
}

func loadStories(path string) ([]*metrics.Story, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Stories []*metrics.Story `json:"stories"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc.Stories, nil
}
