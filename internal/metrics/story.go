package metrics

import (
	"encoding/json"
	"os"
	"time"
)

// Story is the north-star record: one bounded coding task, start to finish.
//
// The metric this benchmark optimizes is the median seconds to a hidden-green
// Story. Not decode tokens per second, which a configuration can improve while
// generating twice as much output and failing the task -- as one already has.
//
// Wall time is decomposed rather than totalled, because "the model was slow"
// and "the model needed four more turns" and "the build is slow" are three
// different problems with three different fixes, and a single number hides
// which one you have.
type Story struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`

	FixtureID      string `json:"fixture_id"`
	FixtureVersion string `json:"fixture_version"`
	Lane           string `json:"lane"`
	PromptLayout   string `json:"prompt_layout"`
	ContextVariant string `json:"context_variant"`
	Engine         string `json:"engine"`
	Model          string `json:"model"`
	ThinkingMode   string `json:"thinking_mode"`
	Thermal        string `json:"thermal"`
	Repetition     int    `json:"repetition"`

	StartedAt time.Time `json:"started_at"`

	// Inference and Task are deliberately separate groups.
	//
	// After the one-shot campaign this is not a presentational choice. A
	// configuration decoded ~5% faster than the baseline, generated 2.1x more
	// output, and failed the task; on a single blended score it would have
	// looked competitive. Engine performance and workload outcome are different
	// questions and are answered in different places.
	Inference InferenceMetrics `json:"inference"`
	Task      TaskMetrics      `json:"task"`

	// --- the verdict ---
	// HiddenGreen is the only success condition. Everything else is diagnosis.
	HiddenGreen bool `json:"hidden_green"`
	// StopReason says why the loop ended: the model declared done, the host
	// found the tree ready for final evaluation, the turn budget ran out, the
	// wall-clock budget ran out, the model returned no diff twice, or the
	// transport failed. A story that ran out of turns and one that finished are
	// not the same result.
	//
	// There is deliberately no apply-failed stop: a diff that does not apply is
	// repairable, and the loop exists to let the model repair it.
	StopReason string `json:"stop_reason"`
	// FinalGates is the gate set as of the last turn, hidden suite included.
	FinalGates []Gate `json:"final_gates"`

	// --- the clock, decomposed ---
	ModelWallMS float64 `json:"model_wall_ms"`
	// ToolWallMS is every piece of host-side execution the benchmark required:
	// patch application, build, tests, the candidate-test discrimination gate,
	// any diagnostic leak, and the final hidden evaluation. Omitting the last
	// of those made time_to_hidden_green_ms exceed total_wall_ms, which is not
	// a rounding difference but a broken identity.
	ToolWallMS float64 `json:"tool_wall_ms"`
	// FinalHiddenWallMS is the once-per-story hidden evaluation, broken out
	// because it is benchmark grading rather than agent work. It is included in
	// ToolWallMS.
	FinalHiddenWallMS float64 `json:"final_hidden_wall_ms"`
	// DiagnosticLeakWallMS is the extra hidden run the diagnostic lane performs.
	// Zero for the canonical lane, and included in ToolWallMS when non-zero.
	DiagnosticLeakWallMS float64 `json:"diagnostic_leak_wall_ms"`
	// ElapsedWallMS is raw wall time from the story's first request to its last
	// evaluation. HarnessOverheadMS is the difference between that and
	// TotalWallMS -- prompt serialization, artifact writing, worktree staging.
	// Recorded so the overhead is visible as a number rather than being either
	// invisible or silently folded into the benchmark's own result.
	ElapsedWallMS     float64 `json:"elapsed_wall_ms"`
	HarnessOverheadMS float64 `json:"harness_overhead_ms"`
	// TotalWallMS is the story's critical path: model time plus tool time.
	// Harness overhead outside those two is excluded and is not measured.
	TotalWallMS float64 `json:"total_wall_ms"`

	// --- how it got there ---
	ModelTurns          int `json:"model_turns"`
	PatchAttempts       int `json:"patch_attempts"`
	FailedApplyAttempts int `json:"failed_apply_attempts"`
	NoDiffTurns         int `json:"no_diff_turns"`

	CompletionTokensTotal *int `json:"completion_tokens_total"`
	ReasoningTokensTotal  *int `json:"reasoning_tokens_total"`
	PromptTokensFinal     *int `json:"prompt_tokens_final"`

	// --- milestones, measured from the story's first request ---
	// Each is null when never reached, never zero: a story that never compiled
	// and one that compiled instantly are not the same story.
	//
	// Measured in the SAME accounting as TotalWallMS -- model time plus tool
	// time, harness overhead excluded -- so a milestone can be compared against
	// the total it is part of. Stamping them from raw elapsed made
	// time_to_hidden_green_ms exceed total_wall_ms.
	TimeToFirstCompilingPatchMS *float64 `json:"time_to_first_compiling_patch_ms"`
	// TimeToDiscriminatingGreenMS is the first turn at which the visible rung
	// AND the candidate-test discrimination gate both pass. Deliberately not
	// "time to visible green": the visible suite alone is satisfied by a patch
	// that implements nothing, so visible-green is not a milestone.
	TimeToDiscriminatingGreenMS *float64 `json:"time_to_discriminating_green_ms"`
	TimeToHiddenGreenMS         *float64 `json:"time_to_hidden_green_ms"`

	// TurnsAtFirstCompile and friends record the same milestones in turns, for
	// the cases where turn count is the interesting axis.
	TurnAtFirstCompile        *int `json:"turn_at_first_compile"`
	TurnAtDiscriminatingGreen *int `json:"turn_at_discriminating_green"`

	// --- provenance ---
	// HiddenLeakedAfterTurn records that this story was run in a diagnostic
	// lane which showed the model the hidden suite's output. A story with this
	// set is not comparable with one without it.
	HiddenLeakedAfterTurn *int `json:"hidden_leaked_after_turn"`

	Experiment StoryExperiment `json:"experiment"`

	Turns     []StoryTurn       `json:"turns"`
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

// InferenceMetrics is how the engine performed. None of it is a success
// criterion; all of it is explanation.
type InferenceMetrics struct {
	ModelWallMS float64 `json:"model_wall_ms"`

	PrefillTokS       *float64 `json:"prefill_tok_s_median"`
	DecodeTokS        *float64 `json:"decode_tok_s_median"`
	DecodeTokSDerived *float64 `json:"decode_tok_s_derived_median"`
	DraftTokS         *float64 `json:"draft_tok_s_median"`
	VerifyTokS        *float64 `json:"verify_tok_s_median"`

	VisibleTTFTMS   *float64 `json:"visible_ttft_ms_median"`
	ReasoningTTFTMS *float64 `json:"reasoning_ttft_ms_median"`

	DFlashTau          *float64 `json:"dflash_tau_median"`
	DFlashAcceptRate   *float64 `json:"dflash_accept_rate_median"`
	SpeculativeWindows *int     `json:"speculative_windows_total"`
	AcceptedTokens     *int     `json:"accepted_tokens_total"`
	RejectedTokens     *int     `json:"rejected_tokens_total"`

	PromptTokensTotal     *int `json:"prompt_tokens_total"`
	CompletionTokensTotal *int `json:"completion_tokens_total"`
	ReasoningTokensTotal  *int `json:"reasoning_tokens_total"`

	// CachedTokensTotal and NewlyPrefilledTokensTotal are the engine's own
	// numbers summed over the story. ReusableSharedTokensTotal is the runner's
	// measurement of what consecutive turns shared, available even against a
	// backend that reports nothing.
	CachedTokensTotal         *int `json:"cached_tokens_total"`
	NewlyPrefilledTokensTotal *int `json:"newly_prefilled_tokens_total"`
	SharedWithPreviousTotal   int  `json:"shared_with_previous_tokens_estimated_total"`
	// CacheVerdicts counts each turn's classification, so "not-exposed" cannot
	// be mistaken for "miss" in aggregate.
	CacheVerdicts map[string]int `json:"cache_verdicts"`

	// FinishReasons counts why each completion stopped. A story whose turns all
	// hit the token ceiling is a different result from one whose turns stopped
	// naturally, however similar the wall clock.
	FinishReasons map[string]int `json:"finish_reasons"`

	KnobFingerprint string `json:"knob_fingerprint,omitempty"`
	ResolvedModel   string `json:"resolved_model,omitempty"`
	KVFormat        string `json:"kv_format,omitempty"`
	ReplicaID       string `json:"replica_id,omitempty"`
	GPU             string `json:"gpu,omitempty"`
}

// TaskMetrics is whether the workload succeeded, and what it cost. This is the
// group the north-star metric comes from.
type TaskMetrics struct {
	Success    bool   `json:"success"`
	StopReason string `json:"stop_reason"`

	ModelTurns          int `json:"model_turns"`
	PatchAttempts       int `json:"patch_attempts"`
	FailedApplyAttempts int `json:"failed_apply_attempts"`
	NoDiffTurns         int `json:"no_diff_turns"`

	ToolWallMS  float64 `json:"tool_wall_ms"`
	ModelWallMS float64 `json:"model_wall_ms"`
	// TotalStoryWallMS is the north star: model time plus every piece of tool
	// execution the benchmark required, the final hidden evaluation included.
	// The identity total == model + tool always holds.
	TotalStoryWallMS float64 `json:"total_story_wall_ms"`
	// FinalHiddenWallMS is broken out of ToolWallMS so grading time can be
	// separated from agent time when that is the question.
	FinalHiddenWallMS float64 `json:"final_hidden_wall_ms"`

	TimeToFirstCompilingPatchMS *float64 `json:"time_to_first_compiling_patch_ms"`
	// TimeToDiscriminatingGreenMS replaces what a spec would naturally call
	// time-to-visible-green. The visible suite alone is satisfied by a patch
	// that implements nothing, so visible-green is not a milestone; this is the
	// first turn at which the visible rung and the candidate-test
	// discrimination gate both pass.
	TimeToDiscriminatingGreenMS *float64 `json:"time_to_discriminating_green_ms"`
	TimeToHiddenGreenMS         *float64 `json:"time_to_hidden_green_ms"`
}

// Experiment groups a story into a sweep, for reporting only.
type StoryExperiment struct {
	Family   string `json:"family,omitempty"`
	Variant  string `json:"variant,omitempty"`
	Baseline string `json:"baseline,omitempty"`
}

// StoryTurn is the per-turn summary. The full record for each turn is in
// request.jsonl; this is the shape a human reads.
type StoryTurn struct {
	Index        int     `json:"index"`
	ModelWallMS  float64 `json:"model_wall_ms"`
	ToolWallMS   float64 `json:"tool_wall_ms"`
	PromptBytes  int     `json:"prompt_bytes"`
	OutputBytes  int     `json:"output_bytes"`
	PatchBytes   int     `json:"patch_bytes"`
	PatchApplied bool    `json:"patch_applied"`
	// Gates is this turn's outcome. The hidden suite appears only on the turn
	// it was actually run, which is once per story.
	Gates []Gate `json:"gates"`
	// Cache is this turn's accounting. In a multi-turn loop this is the number
	// that predicts prefill work, and it is recorded per turn so the shape of
	// reuse across a story is visible rather than only its total.
	Cache CacheAccounting `json:"cache"`
	// TurnRole and TurnMaxTokens say which per-turn budget this turn drew on;
	// FinishReason says whether it hit that budget.
	TurnRole      string `json:"turn_role,omitempty"`
	TurnMaxTokens int    `json:"turn_max_tokens,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`
	Note          string `json:"note,omitempty"`
}

const StorySchema = "agentbench/story/v1"

func (s *Story) Save(path string) error {
	s.Schema = StorySchema
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// SaveStories writes every story in a run to one file. A run with several
// engines or repetitions produces several stories, and they belong together.
func SaveStories(path string, stories []*Story) error {
	for _, s := range stories {
		s.Schema = StorySchema
	}
	b, err := json.MarshalIndent(map[string]any{
		"schema":  StorySchema,
		"stories": stories,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// GateByName returns the named gate from a set, or nil.
func GateByName(gates []Gate, name string) *Gate {
	for i := range gates {
		if gates[i].Name == name {
			return &gates[i]
		}
	}
	return nil
}

// AllPassed reports whether every named gate is present and passing.
func AllPassed(gates []Gate, names ...string) bool {
	for _, n := range names {
		g := GateByName(gates, n)
		if g == nil || g.Result != GatePass {
			return false
		}
	}
	return true
}
