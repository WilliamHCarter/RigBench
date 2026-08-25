package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/mock"
	"github.com/WilliamHCarter/RigBench/internal/prompt"
	"github.com/WilliamHCarter/RigBench/internal/runner"
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
		hiddenGreen: true, stopReason: stopReadyForFinalReason, minTurns: 2, maxTurns: 2,
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
		variant: mock.LiveScopeThenRepair, lane: "builder-live",
		hiddenGreen: true, stopReason: stopReadyForFinalReason, minTurns: 2, maxTurns: 2,
		why: "turn 0 implements everything correctly AND edits one out-of-scope file, so\n" +
			"build, visible and the discrimination gate all pass and only scope is red.\n" +
			"A loop that stopped on discrimination alone would end this story as a failure\n" +
			"with the host's own \"revert those files\" feedback computed and never sent",
		check: func(s *metrics.Story) error {
			if len(s.Turns) < 2 {
				return fmt.Errorf("stopped after %d turn(s); the scope feedback was never sent",
					len(s.Turns))
			}
			g := metrics.GateByName(s.Turns[0].Gates, "scope")
			if g == nil || g.Result != metrics.GateFail {
				return fmt.Errorf("turn 0's scope gate was %v, want fail", g)
			}
			if d := metrics.GateByName(s.Turns[0].Gates, "candidate_tests_discriminate"); d == nil ||
				d.Result != metrics.GatePass {
				return fmt.Errorf("turn 0's discrimination gate was %v, want pass -- the "+
					"variant is not reproducing the scope-red/discrimination-green case", d)
			}
			return nil
		},
	},
	{
		variant: mock.LiveApplyFailThenRecover, lane: "builder-live",
		hiddenGreen: true, stopReason: stopReadyForFinalReason, minTurns: 2, maxTurns: 2,
		why: "a diff that does not apply must leave the tree untouched, must be reported\n" +
			"as such, and must be repairable rather than terminal",
		check: func(s *metrics.Story) error {
			if s.FailedApplyAttempts != 1 {
				return fmt.Errorf("failed applies = %d, want 1", s.FailedApplyAttempts)
			}
			if len(s.Turns) > 0 && s.Turns[0].PatchApplied {
				return fmt.Errorf("a corrupted diff was recorded as applied")
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
	stopReadyForFinalReason  = "build, visible, scope and discrimination gate all green"
	stopMaxTurnsReason       = "turn budget exhausted"
	stopRepeatedNoDiffReason = "the model returned no diff twice in a row"
	stopDoneReason           = "model declared done"
	stopMaxWallReason        = "wall-clock budget exhausted"
)

func acceptV03(ctx context.Context, fixtureDir, layout, engines, root string, timeScale float64) error {
	f, err := config.LoadFixture(fixtureDir)
	if err != nil {
		return err
	}

	fmt.Printf("== wall-clock deadline ==\n%s\n",
		indent("max_wall_seconds must cancel an in-flight request, not merely be checked\n"+
			"between turns. A story with seconds left could otherwise start a request\n"+
			"carrying its own 900-second timeout and overrun by most of an hour"))
	if err := checkWallDeadline(ctx, fixtureDir, layout, root); err != nil {
		return err
	}
	fmt.Println()

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
		if err := checkStoryInvariants(s, runDir); err != nil {
			return fmt.Errorf("%s on %s: %w", exp.variant, exp.lane, err)
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

// checkStoryInvariants asserts the properties every story must satisfy,
// whatever the variant did.
func checkStoryInvariants(s *metrics.Story, runDir string) error {
	// The clock identity. It used to fail because the final hidden evaluation
	// was omitted from tool time, which let time_to_hidden_green exceed the
	// total wall it is supposedly part of.
	const eps = 0.5
	if diff := s.TotalWallMS - (s.ModelWallMS + s.ToolWallMS); diff > eps || diff < -eps {
		return fmt.Errorf("total_wall_ms %.1f != model %.1f + tool %.1f",
			s.TotalWallMS, s.ModelWallMS, s.ToolWallMS)
	}
	if s.FinalHiddenWallMS <= 0 {
		return fmt.Errorf("the final hidden evaluation was not timed")
	}
	if s.FinalHiddenWallMS > s.ToolWallMS+eps {
		return fmt.Errorf("final hidden %.1f exceeds all tool time %.1f",
			s.FinalHiddenWallMS, s.ToolWallMS)
	}
	// A green story reaches hidden-green as the last thing it does, so the
	// milestone equals the total. Asserted as equality rather than as an upper
	// bound: stamping it before the hidden suite's own duration was added made
	// it pass an upper-bound check while understating itself by exactly that
	// duration.
	if s.TimeToHiddenGreenMS != nil {
		if diff := *s.TimeToHiddenGreenMS - s.TotalWallMS; diff > eps || diff < -eps {
			return fmt.Errorf("time_to_hidden_green %.1f != total_wall %.1f; a story cannot "+
				"be known green before the hidden suite finishes, so the milestone must "+
				"include its duration", *s.TimeToHiddenGreenMS, s.TotalWallMS)
		}
	}
	if s.Task.TotalStoryWallMS != s.TotalWallMS {
		return fmt.Errorf("task group disagrees with the story on total wall")
	}

	recs, err := loadRecords(runDir)
	if err != nil {
		return err
	}
	scored := 0
	for _, r := range recs {
		// Trajectory length must be recorded, not left at zero while the report
		// prints it as prompt-growth telemetry.
		if r.TurnCount != len(recs) {
			return fmt.Errorf("turn %d records turn_count=%d, want %d",
				r.TurnIndex, r.TurnCount, len(recs))
		}
		if r.TurnIndex > 0 && r.AppendedBytes <= 0 {
			return fmt.Errorf("turn %d recorded appended_bytes=%d despite append-only growth",
				r.TurnIndex, r.AppendedBytes)
		}
		if r.TurnIndex > 0 && r.Thermal == "cold" {
			return fmt.Errorf("turn %d is labelled cold; only turn 0 can be", r.TurnIndex)
		}
		if r.Scored {
			scored++
		}
	}
	// Exactly one record carries the story's verdict. An intermediate repair
	// turn is not an independent quality trial: counting one would report a
	// green three-turn story as 33%% passing.
	if scored != 1 {
		return fmt.Errorf("%d records are marked scored, want exactly 1", scored)
	}
	return nil
}

func loadRecords(runDir string) ([]metrics.Record, error) {
	return metrics.ReadRecords(filepath.Join(runDir, "request.jsonl"))
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

// checkWallDeadline proves max_wall_seconds is a deadline rather than a
// between-turns check.
//
// The old implementation tested the budget only before starting a turn, so a
// story with ten seconds left could begin a request carrying its own
// 900-second client timeout and then run build and test commands carrying
// theirs. A declared 3600-second budget could be exceeded by most of an hour,
// and nothing in the acceptance suite exercised the claim.
//
// Driven directly rather than through doRun so the fixture does not need a
// throwaway lane with a one-second budget.
func checkWallDeadline(ctx context.Context, fixtureDir, layout, root string) error {
	f, err := config.LoadFixture(fixtureDir)
	if err != nil {
		return err
	}
	lay, err := config.LoadLayout(layout)
	if err != nil {
		return err
	}
	e, err := config.LoadEngine("configs/engines/mock-ar.json")
	if err != nil {
		return err
	}

	stage, err := os.MkdirTemp("", "agentbench-wall-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	script, err := mock.BuildLiveScript(ctx, f, mock.LiveConverges, filepath.Join(stage, "live"))
	if err != nil {
		return err
	}
	// Deliberately slow: each request takes far longer than the budget.
	srv := &mock.Server{
		TimeScale: 4.0, ProfileFor: profileFromRequest,
		Respond: func(i mock.RequestInfo) (string, string) { return script.Reply(i.AssistantTurns), "" },
	}
	ln, shutdown, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		return err
	}
	defer shutdown(ctx)

	const budget = 3
	lane := &config.Lane{
		AgentContractFile:           f.Lanes["builder-live"].AgentContractFile,
		ObjectiveFile:               f.Lanes["builder-live"].ObjectiveFile,
		MaxTurns:                    6,
		MaxWallSeconds:              budget,
		MaxToolOutputBytes:          16384,
		StopWhenDiscriminatingGreen: true,
	}

	runDir := filepath.Join(root, "wall-deadline")
	started := time.Now()
	lr, err := runner.RunLive(ctx, runner.LiveOptions{
		Options: runner.Options{
			Fixture: f, Engine: e, Layout: lay, ContextPack: "base", Thermal: "cold",
			RunID: "walldeadline", RunDir: runDir, WorkDir: filepath.Join(runDir, "work"),
			Tokenizer:        prompt.Approx{},
			EndpointOverride: fmt.Sprintf("http://%s/v1", ln.Addr()),
		},
		LaneName: "builder-live-wall-probe", Lane: lane,
	})
	if err != nil {
		return fmt.Errorf("wall-deadline probe: %w", err)
	}
	elapsed := time.Since(started)

	if lr.Story.StopReason != stopMaxWallReason {
		return fmt.Errorf("wall-deadline probe stopped with %q, want %q",
			lr.Story.StopReason, stopMaxWallReason)
	}
	// Generous ceiling: the final hidden evaluation is deliberately outside the
	// agent budget, and it compiles a Zig test binary. What must not happen is
	// the story running for a full request timeout past its budget.
	if limit := time.Duration(budget)*time.Second + 90*time.Second; elapsed > limit {
		return fmt.Errorf("wall-deadline probe ran %s against a %ds budget; the deadline "+
			"is not being enforced", elapsed.Round(time.Second), budget)
	}
	fmt.Printf("  budget %ds, stopped after %s with %q\n",
		budget, elapsed.Round(time.Second), lr.Story.StopReason)
	return nil
}
