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

	// --- the verdict ---
	// HiddenGreen is the only success condition. Everything else is diagnosis.
	HiddenGreen bool `json:"hidden_green"`
	// StopReason says why the loop ended: done, discriminating-green,
	// max-turns, max-wall, no-diff, apply-failed, transport. A story that ran
	// out of turns and one that finished are not the same result.
	StopReason string `json:"stop_reason"`
	// FinalGates is the gate set as of the last turn, hidden suite included.
	FinalGates []Gate `json:"final_gates"`

	// --- the clock, decomposed ---
	ModelWallMS float64 `json:"model_wall_ms"`
	ToolWallMS  float64 `json:"tool_wall_ms"`
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

	Turns     []StoryTurn       `json:"turns"`
	Artifacts map[string]string `json:"artifacts,omitempty"`
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
	Note  string `json:"note,omitempty"`
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
