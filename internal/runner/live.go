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
	stopReadyForFinal  = "build, visible, scope and discrimination gate all green"
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

	// The agent budget is a real deadline, not a check between turns.
	//
	// Checking only before starting a turn let a story that had 10 seconds left
	// begin a request with its own 900-second timeout and then run build and
	// test commands with theirs -- sailing past a declared 3600-second budget by
	// most of an hour. The budget now cancels the model request and every tool
	// command in the loop.
	//
	// The FINAL hidden evaluation is deliberately outside it: that is the
	// benchmark grading the result, not the agent working, and a story must not
	// be recorded as un-graded because the agent used all its time. `ctx` rather
	// than `agentCtx` is used for it below, and the lane comment says so.
	agentCtx, cancelAgent := context.WithTimeout(ctx,
		time.Duration(lane.MaxWallSeconds)*time.Second)
	defer cancelAgent()

	for turn := 0; turn < lane.MaxTurns; turn++ {
		if agentCtx.Err() != nil {
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
		res := c.Complete(agentCtx, client.Request{
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
		appendedBytes := 0
		if prev != nil {
			appendedBytes = m.PromptBytes - prev.PromptBytes
		}
		// Only turn 0 of a cold story is cold. Copying the run's declared class
		// onto every turn labelled a resident-server turn as a cold one.
		turnThermal := o.Thermal
		if o.Thermal == "cold" && turn > 0 {
			turnThermal = "warm-resident"
		}
		rec := liveRecord(o, m, res, adapter, liveContext{
			Turn: turn, TurnRole: turnRole, TurnMaxTokens: maxTokens,
			SharedTokens: shared, AppendedTokens: appended,
			AppendedBytes: appendedBytes, Thermal: turnThermal,
			ReusableTokens: o.Tokenizer.Count(prompt.Canonical(m.Messages)[:m.StablePrefixBytes]),
			LogTelemetry:   logTel,
		})
		story.ModelWallMS += res.WallMS
		story.ModelTurns++
		addTokens(&story.CompletionTokensTotal, rec.CompletionTokens)
		addTokens(&story.ReasoningTokensTotal, rec.ReasoningTokens)
		story.PromptTokensFinal = rec.PromptTokens

		if res.TransportStatus != metrics.TransportOK {
			if agentCtx.Err() != nil {
				// The deadline cancelled the request. That is the budget doing
				// its job, not the transport failing, and the two must not
				// aggregate together -- so the gate is named for the budget and
				// the transport error is kept as its detail.
				story.StopReason = stopMaxWall
				rec.Scored = false
				rec.Quality = &metrics.Quality{Passed: false, Gates: []metrics.Gate{{
					Name: "wall_budget", Result: metrics.GateFail,
					Detail: fmt.Sprintf("the agent's %ds budget cancelled this request (%s)",
						lane.MaxWallSeconds, res.TransportStatus),
				}}}
				out.Records = append(out.Records, rec)
				break
			}
			rec.Scored = true
			rec.Quality = &metrics.Quality{Passed: false, Gates: []metrics.Gate{{
				Name: "transport", Result: metrics.GateFail,
				Detail: fmt.Sprintf("%s: %v", res.TransportStatus, res.Err),
			}}}
			out.Records = append(out.Records, rec)
			story.StopReason = stopTransport
			break
		}

		tr, err := scoring.ScoreLiveTurn(agentCtx, scoring.TurnInput{
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
		// An intermediate repair turn is NOT an independent quality trial: a
		// failing turn 0 followed by a repairing turn 1 is one successful
		// story, and counting it as one pass and one failure would report a
		// green three-turn story as 33% passing. The gates are retained on the
		// StoryTurn, where they are diagnosis rather than verdict. Only the
		// final record carries the story's verdict, set below.
		rec.Scored = false
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

		// Milestones are stamped from the SAME accounting as total_wall_ms --
		// model time plus tool time -- not from raw elapsed wall.
		//
		// Stamping them from time.Since(storyStart) made
		// time_to_hidden_green_ms exceed the total_wall_ms it is supposedly
		// part of, by however much harness overhead had accumulated. Two
		// clocks that cannot be compared are worse than one that is slightly
		// incomplete, and this benchmark's own definition of the total is
		// model + tool.
		accounted := story.ModelWallMS + story.ToolWallMS
		if tr.Compiles && story.TimeToFirstCompilingPatchMS == nil {
			story.TimeToFirstCompilingPatchMS = metrics.Ptr(accounted)
			story.TurnAtFirstCompile = metrics.Ptr(turn)
		}
		if tr.DiscriminatingGreen && story.TimeToDiscriminatingGreenMS == nil {
			story.TimeToDiscriminatingGreenMS = metrics.Ptr(accounted)
			story.TurnAtDiscriminatingGreen = metrics.Ptr(turn)
		}
		story.Turns = append(story.Turns, st)

		if tr.Done {
			story.StopReason = stopDone
			break
		}
		// Stops on ReadyForFinal, which includes scope. Stopping on
		// DiscriminatingGreen alone terminated a story whose only remaining
		// defect was a stray out-of-scope edit, with the host's own "revert
		// those files" feedback computed and then never sent.
		if lane.StopWhenDiscriminatingGreen && tr.ReadyForFinal {
			story.StopReason = stopReadyForFinal
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
			leakStart := time.Now()
			g, raw, herr := scoring.ScoreHiddenFinal(agentCtx, scoring.TurnInput{
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
			if rerr := restoreHiddenPlaceholder(agentCtx, f, wt); rerr != nil {
				return nil, rerr
			}
			// The leaked evaluation and the restore are benchmark-required tool
			// execution and belong in the story's clock like any other.
			leakMS := float64(time.Since(leakStart).Nanoseconds()) / 1e6
			story.ToolWallMS += leakMS
			story.DiagnosticLeakWallMS += leakMS
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
	//
	// Uses `ctx`, not `agentCtx`: a story that exhausted its agent budget must
	// still be graded, or "ran out of time" and "was never evaluated" would
	// become the same record.
	finalHiddenStart := time.Now()
	hidden, _, herr := scoring.ScoreHiddenFinal(ctx, scoring.TurnInput{
		Fixture: f, Worktree: wt,
		ArtifactDir: artDir, ArtifactPrefix: artPrefix,
		MaxToolOutputBytes: lane.MaxToolOutputBytes,
	})
	if herr != nil {
		hidden = metrics.Gate{Name: scoring.GateHiddenTests, Result: metrics.GateSkipped,
			Detail: herr.Error()}
	}
	// The clock is closed BEFORE the verdict is stamped.
	//
	// A story cannot be known hidden-green until the hidden suite has finished,
	// so a milestone stamped before its duration is added understates itself by
	// exactly that duration. The final evaluation is tool execution the
	// benchmark required, so it is in the clock; omitting it entirely made
	// time_to_hidden_green_ms exceed total_wall_ms, and adding it after the
	// stamp made the milestone too small instead. Order is the whole fix.
	story.FinalHiddenWallMS = float64(time.Since(finalHiddenStart).Nanoseconds()) / 1e6
	story.ToolWallMS += story.FinalHiddenWallMS
	story.TotalWallMS = story.ModelWallMS + story.ToolWallMS

	story.FinalGates = append(finalGatesOf(story), hidden)
	story.HiddenGreen = hidden.Result == metrics.GatePass &&
		metrics.AllPassed(story.FinalGates, scoring.GateBuild, scoring.GateVisibleTests,
			scoring.GateScope, scoring.GateCandidateTestsDiscriminate)
	if story.HiddenGreen {
		// Equal to TotalWallMS by construction: reaching hidden-green is the
		// last thing a green story does.
		story.TimeToHiddenGreenMS = metrics.Ptr(story.TotalWallMS)
	}
	// Raw elapsed is recorded alongside, so harness overhead is visible as the
	// difference rather than being invisible or silently folded in.
	story.ElapsedWallMS = float64(time.Since(storyStart).Nanoseconds()) / 1e6
	story.HarnessOverheadMS = story.ElapsedWallMS - story.TotalWallMS
	story.Experiment = metrics.StoryExperiment{
		Family:   e.Experiment.Family,
		Variant:  e.Experiment.Variant,
		Baseline: e.Experiment.Baseline,
	}
	story.Rollup(out.Records)

	// Trajectory length is only known once the loop has stopped.
	for _, r := range out.Records {
		r.TurnCount = len(out.Records)
	}

	if len(out.Records) > 0 {
		last := out.Records[len(out.Records)-1]
		if last.Quality == nil {
			// A story cancelled mid-request still gets graded, so its final
			// record still needs somewhere to carry the verdict.
			last.Quality = &metrics.Quality{}
		}
		last.Quality.Gates = append(last.Quality.Gates, hidden)
		last.Quality.Passed = story.HiddenGreen
		last.Scored = true
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
	// Removed wholesale rather than file by file: skipping directories would
	// leave oracle content behind the moment a hidden suite grows a subdirectory,
	// and it would leave it somewhere the next turn's build compiles.
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
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

// nextObjective is deliberately state-neutral.
//
// It used to assert "the host applied your diff and ran the build", which
// directly contradicted the host's own message on a turn where the diff did not
// apply or no diff was found. A model told in one breath that its tree is
// unchanged and in the next that its diff was applied has been handed
// contradictory state, and it will act on one of them.
//
// It also no longer promises "exact output": that output is sanitized of
// host-specific bytes and truncated with an explicit marker, and the contract
// should describe what is actually sent.
func nextObjective(turn int) string {
	return fmt.Sprintf("TURN %d — current objective\n\n"+
		"The host result above is authoritative, including whether anything was\n"+
		"applied. It is the host's own output, sanitized of machine-specific paths\n"+
		"and truncated where marked. The working tree persists exactly as the host\n"+
		"described it.\n\n"+
		"Diagnose only what the host reported and return the next bounded repair as\n"+
		"one fenced diff. Send only the delta; do not repeat unchanged files.\n\n"+
		"If the host reports the tree is green and the work is complete, return DONE\n"+
		"instead.\n", turn)
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
