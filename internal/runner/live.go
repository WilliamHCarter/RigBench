package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/client"
	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/prompt"
	"github.com/WilliamHCarter/RigBench/internal/scoring"
)

// The live builder loop.
//
// v0.2's replay was exogenous: the assistant turns and tool results were fixture
// bytes, so every engine received identical prompts and prefix behaviour could
// be isolated. This loop is endogenous -- the model's own output becomes the
// next prompt -- which buys realism and costs cross-engine prompt equality. Two
// engines running this lane will legitimately diverge at turn 1, and the report
// must say that rather than flagging it as a fault.
//
// What survives from v0.2 is the append-only property *within* one story: turn
// N's prompt is turn N-1's prompt plus the model's own reply plus the host's
// tool output plus the next objective. Nothing earlier is rewritten. That is
// still asserted before every request.

// LiveOptions configures one story.
type LiveOptions struct {
	Options
	LaneName string
	Lane     *config.Lane
}

// LiveResult is a completed story plus the per-turn records it produced.
type LiveResult struct {
	Story   *metrics.Story
	Records []*metrics.Record
}

// stop reasons
const (
	stopDone           = "model declared done"
	stopDiscriminating = "visible rung and discrimination gate both green"
	stopMaxTurns       = "turn budget exhausted"
	stopMaxWall        = "wall-clock budget exhausted"
	stopTransport      = "transport failure"
	stopRepeatedNoDiff = "the model returned no diff twice in a row"
	stopHarnessFailure = "harness failure"
)

// RunLive executes one live builder story.
func RunLive(ctx context.Context, o LiveOptions) (*LiveResult, error) {
	f, e, lane := o.Fixture, o.Engine, o.Lane

	slug := fmt.Sprintf("%s.%s.%s.%s.r%d", e.Name, o.LaneName, o.Layout.ID, o.Thermal, o.Repetition)
	artDir := filepath.Join(o.RunDir, "artifacts", slug)
	artPrefix := filepath.ToSlash(filepath.Join("artifacts", slug))
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return nil, err
	}

	// One persistent worktree for the whole story. Each turn's diff lands on
	// the tree the previous turn produced; that is what makes this live.
	wt, err := executor.Stage(ctx, f, filepath.Join(o.WorkDir, slug), false)
	if err != nil {
		return nil, err
	}

	story := &metrics.Story{
		RunID: o.RunID, FixtureID: f.ID, FixtureVersion: f.Version,
		Lane: o.LaneName, PromptLayout: o.Layout.ID, ContextVariant: o.ContextPack,
		Engine: e.Name, Model: e.Model, ThinkingMode: e.Sampling.Thinking,
		Thermal: o.Thermal, Repetition: o.Repetition,
		StartedAt:             time.Now().UTC(),
		HiddenLeakedAfterTurn: lane.LeakHiddenAfterTurn,
		Artifacts:             map[string]string{"dir": artPrefix},
	}
	out := &LiveResult{Story: story}

	adapter, ok := client.AdapterFor(e.TelemetryAdapter)
	if !ok {
		return nil, fmt.Errorf("engine %s: unknown telemetry adapter %q", e.Name, e.TelemetryAdapter)
	}
	endpoint := e.Endpoint
	if o.EndpointOverride != "" {
		endpoint = o.EndpointOverride
	}
	apiKey := ""
	if e.APIKeyEnv != "" {
		apiKey = os.Getenv(e.APIKeyEnv)
	}
	c := client.New(endpoint, apiKey, f.Timeout())

	var tail []prompt.TailBlock
	var prevManifest *prompt.Manifest
	consecutiveNoDiff := 0
	storyStart := time.Now()

	for turn := 0; turn < lane.MaxTurns; turn++ {
		if elapsed := time.Since(storyStart); elapsed > time.Duration(lane.MaxWallSeconds)*time.Second {
			story.StopReason = stopMaxWall
			break
		}

		m, err := buildLivePrompt(o, tail)
		if err != nil {
			return nil, fmt.Errorf("turn %d: %w", turn, err)
		}
		prev := prevManifest
		if err := m.AppendsOnto(prev); err != nil {
			return nil, fmt.Errorf("turn %d: %w", turn, err)
		}
		prevManifest = m

		turnArt := filepath.Join(artDir, fmt.Sprintf("t%d", turn))
		if err := os.MkdirAll(turnArt, 0o755); err != nil {
			return nil, err
		}
		_ = os.WriteFile(filepath.Join(turnArt, "prompt.txt"),
			[]byte(prompt.Canonical(m.Messages)), 0o644)

		// Per-turn generation budget. A repair is a delta and does not need an
		// initial implementation's ceiling; one global cap on every turn invites
		// the length pathology the one-shot campaign measured.
		turnRole := lane.TurnLimits.Role(turn, lane.MaxTurns)
		maxTokens := lane.TurnLimits.For(turn, lane.MaxTurns)
		if maxTokens == 0 {
			maxTokens = e.Sampling.MaxTokens
		}

		o.ServerLog.Mark()
		res := c.Complete(ctx, client.Request{
			Model: e.Model, Messages: m.Messages,
			Temperature: e.Sampling.Temperature, MaxTokens: maxTokens,
			Seed: e.Sampling.Seed, TopP: e.Sampling.TopP,
			Thinking:             e.Sampling.Thinking,
			ThinkingOffMechanism: string(e.Sampling.ThinkingOffMechanism),
			ThinkingBudgetTokens: e.Sampling.ThinkingBudgetTokens,
			Headers:              e.Headers, ExtraBody: e.ExtraBody,
		})
		_ = os.WriteFile(filepath.Join(turnArt, "output.txt"), []byte(res.Visible), 0o644)
		if len(res.RequestBody) > 0 {
			_ = os.WriteFile(filepath.Join(turnArt, "request.json"), res.RequestBody, 0o644)
		}

		logTel, logSlice := o.ServerLog.Since()
		if logSlice != "" {
			_ = os.WriteFile(filepath.Join(turnArt, "server.log"), []byte(logSlice), 0o644)
		}

		shared, appended := turnReuse(prev, m)
		rec := liveRecord(o, m, res, adapter, liveContext{
			Turn: turn, TurnRole: turnRole, TurnMaxTokens: maxTokens,
			SharedTokens: shared, AppendedTokens: appended,
			ReusableTokens: o.Tokenizer.Count(prompt.Canonical(m.Messages)[:m.StablePrefixBytes]),
			LogTelemetry:   logTel,
		})
		story.ModelWallMS += res.WallMS
		story.ModelTurns++
		addTokens(&story.CompletionTokensTotal, rec.CompletionTokens)
		addTokens(&story.ReasoningTokensTotal, rec.ReasoningTokens)
		story.PromptTokensFinal = rec.PromptTokens

		if res.TransportStatus != metrics.TransportOK {
			rec.Scored = true
			rec.Quality = &metrics.Quality{Passed: false, Gates: []metrics.Gate{{
				Name: "transport", Result: metrics.GateFail,
				Detail: fmt.Sprintf("%s: %v", res.TransportStatus, res.Err),
			}}}
			out.Records = append(out.Records, rec)
			story.StopReason = stopTransport
			break
		}

		tr, err := scoring.ScoreLiveTurn(ctx, scoring.TurnInput{
			Fixture: f, Worktree: wt, Output: res.Visible, Turn: turn,
			ArtifactDir: turnArt, ArtifactPrefix: filepath.ToSlash(filepath.Join(artPrefix, fmt.Sprintf("t%d", turn))),
			DiscriminateDir:    filepath.Join(o.WorkDir, slug+fmt.Sprintf(".discriminate.t%d", turn)),
			MaxToolOutputBytes: lane.MaxToolOutputBytes,
		})
		if err != nil {
			story.StopReason = stopHarnessFailure
			return nil, err
		}

		rec.ToolWallMS = float64(tr.ToolWall.Nanoseconds()) / 1e6
		rec.PatchApplied = tr.Applied
		rec.Scored = true
		rec.Quality = &metrics.Quality{Gates: tr.Gates, PatchFiles: tr.PatchFiles}
		story.ToolWallMS += rec.ToolWallMS
		out.Records = append(out.Records, rec)

		st := metrics.StoryTurn{
			Index: turn, ModelWallMS: res.WallMS, ToolWallMS: rec.ToolWallMS,
			PromptBytes: m.PromptBytes, OutputBytes: res.OutputBytes,
			PatchBytes: len(tr.Patch), PatchApplied: tr.Applied, Gates: tr.Gates,
			Cache:         rec.Cache,
			TurnRole:      turnRole,
			TurnMaxTokens: maxTokens,
			FinishReason:  res.FinishReason,
		}

		if tr.Patch != "" {
			story.PatchAttempts++
			if !tr.Applied {
				story.FailedApplyAttempts++
			}
		}
		if tr.Done {
			st.Note = "model declared DONE"
			consecutiveNoDiff = 0
		} else if tr.Patch == "" {
			story.NoDiffTurns++
			consecutiveNoDiff++
		} else {
			consecutiveNoDiff = 0
		}

		elapsed := float64(time.Since(storyStart).Nanoseconds()) / 1e6
		if tr.Compiles && story.TimeToFirstCompilingPatchMS == nil {
			story.TimeToFirstCompilingPatchMS = metrics.Ptr(elapsed)
			story.TurnAtFirstCompile = metrics.Ptr(turn)
		}
		if tr.DiscriminatingGreen && story.TimeToDiscriminatingGreenMS == nil {
			story.TimeToDiscriminatingGreenMS = metrics.Ptr(elapsed)
			story.TurnAtDiscriminatingGreen = metrics.Ptr(turn)
		}
		story.Turns = append(story.Turns, st)

		if tr.Done {
			story.StopReason = stopDone
			break
		}
		if lane.StopWhenDiscriminatingGreen && tr.DiscriminatingGreen {
			story.StopReason = stopDiscriminating
			break
		}
		if consecutiveNoDiff >= 2 {
			story.StopReason = stopRepeatedNoDiff
			break
		}

		// Append this turn to the volatile tail: the model's own reply, then
		// the host's actual tool output, then the next objective.
		tail = append(tail, prompt.TailBlock{
			ID: fmt.Sprintf("turn%d_assistant", turn), Role: prompt.Assistant, Text: res.Visible,
		})
		feedback := tr.Feedback

		// The diagnostic lane shows the model the hidden suite's real output.
		// Marked in the story, because a story that saw the oracle is not
		// comparable with one that did not.
		if lane.LeakHiddenAfterTurn != nil && *lane.LeakHiddenAfterTurn == turn {
			g, raw, herr := scoring.ScoreHiddenFinal(ctx, scoring.TurnInput{
				Fixture: f, Worktree: wt,
				ArtifactDir:        turnArt,
				ArtifactPrefix:     filepath.ToSlash(filepath.Join(artPrefix, fmt.Sprintf("t%d", turn))),
				MaxToolOutputBytes: lane.MaxToolOutputBytes,
			})
			if herr == nil && g.Result != metrics.GatePass {
				feedback += "\n\nHOST (diagnostic lane): the benchmark's hidden invariant " +
					"suite was run against your tree and it failed. Its exact output " +
					"follows. Repair only what it reports.\n\n" +
					"$ zig build test-hidden\n" + raw + "\n"
			}
			// Remove it again so the next turn's visible rung does not compile it.
			if rerr := restoreHiddenPlaceholder(ctx, f, wt); rerr != nil {
				return nil, rerr
			}
		}

		if strings.TrimSpace(feedback) != "" {
			tail = append(tail, prompt.TailBlock{
				ID: fmt.Sprintf("turn%d_result", turn), Role: prompt.User, Text: feedback,
			})
		}
		tail = append(tail, prompt.TailBlock{
			ID:   fmt.Sprintf("turn%d_objective", turn+1),
			Role: prompt.User, Text: nextObjective(turn + 1),
		})
	}

	if story.StopReason == "" {
		story.StopReason = stopMaxTurns
	}

	// The hidden suite runs exactly once, now that the loop has stopped.
	hidden, _, herr := scoring.ScoreHiddenFinal(ctx, scoring.TurnInput{
		Fixture: f, Worktree: wt,
		ArtifactDir: artDir, ArtifactPrefix: artPrefix,
		MaxToolOutputBytes: lane.MaxToolOutputBytes,
	})
	if herr != nil {
		hidden = metrics.Gate{Name: scoring.GateHiddenTests, Result: metrics.GateSkipped,
			Detail: herr.Error()}
	}
	story.FinalGates = append(finalGatesOf(story), hidden)
	story.HiddenGreen = hidden.Result == metrics.GatePass &&
		metrics.AllPassed(story.FinalGates, scoring.GateBuild, scoring.GateVisibleTests,
			scoring.GateScope, scoring.GateCandidateTestsDiscriminate)
	if story.HiddenGreen {
		story.TimeToHiddenGreenMS = metrics.Ptr(float64(time.Since(storyStart).Nanoseconds()) / 1e6)
	}
	story.TotalWallMS = story.ModelWallMS + story.ToolWallMS
	story.Experiment = metrics.StoryExperiment{
		Family:   e.Experiment.Family,
		Variant:  e.Experiment.Variant,
		Baseline: e.Experiment.Baseline,
	}
	story.Rollup(out.Records)

	if len(out.Records) > 0 {
		last := out.Records[len(out.Records)-1]
		last.Quality.Gates = append(last.Quality.Gates, hidden)
		last.Quality.Passed = story.HiddenGreen
	}
	return out, nil
}

// finalGatesOf returns the last turn's gates, which describe the tree the
// hidden suite is about to run against.
func finalGatesOf(s *metrics.Story) []metrics.Gate {
	for i := len(s.Turns) - 1; i >= 0; i-- {
		if len(s.Turns[i].Gates) > 0 {
			return s.Turns[i].Gates
		}
	}
	return nil
}

// restoreHiddenPlaceholder puts the empty placeholder back after a diagnostic
// leak, so the next turn's `zig build test` does not compile the oracle.
func restoreHiddenPlaceholder(ctx context.Context, f *config.Fixture, wt *executor.Worktree) error {
	dst := filepath.Join(wt.Dir, "hidden")
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, en := range entries {
		if !en.IsDir() {
			if err := os.Remove(filepath.Join(dst, en.Name())); err != nil {
				return err
			}
		}
	}
	src := filepath.Join(f.Path(f.RepoDir), "hidden")
	srcEntries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, en := range srcEntries {
		if en.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, en.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, en.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// turnReuse is what this turn shares with the previous one and what it added,
// in estimated tokens.
//
// The append property is asserted before every request, so the previous turn's
// entire prompt IS the shared prefix -- there is no substring search to do.
// Turn 0 shares nothing and appends everything, and saying so is the point:
// reporting a turn-0 prompt as mostly reused would make every story look
// cache-friendly from the first request.
func turnReuse(prev, cur *prompt.Manifest) (shared, appended int) {
	if prev == nil {
		return 0, cur.PromptTokensEstimated
	}
	shared = prev.PromptTokensEstimated
	appended = cur.PromptTokensEstimated - shared
	if appended < 0 {
		appended = 0
	}
	return shared, appended
}

func nextObjective(turn int) string {
	return fmt.Sprintf("TURN %d — current objective\n\n"+
		"The host applied your diff and ran the build. Its exact output is above.\n\n"+
		"Diagnose only the failures reported there and return the next bounded repair\n"+
		"as one fenced diff. Send only the delta; do not repeat unchanged files.\n\n"+
		"If the build is green and the work is complete, return DONE instead.\n", turn)
}

func addTokens(dst **int, v *int) {
	if v == nil {
		return
	}
	if *dst == nil {
		*dst = metrics.Ptr(*v)
		return
	}
	**dst += *v
}
