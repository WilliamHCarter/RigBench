// Package runner drives one lane of the benchmark end to end: assemble the
// prompt, stream the completion, score the result, write the artifacts.
//
// The ordering here is part of the contract. The prompt is serialized and its
// hash verified *before* the request is sent, so a run can be rejected for a
// bad prompt without spending a completion. The hidden suite is injected only
// after the candidate's own build and visible tests have run, so the model's
// rung is the rung it was given.
package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/client"
	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/prompt"
	"github.com/WilliamHCarter/RigBench/internal/scoring"
)

// Options configures one builder request.
type Options struct {
	Fixture *config.Fixture
	Engine  *config.Engine
	Layout  *config.Layout

	ContextPack string
	// Thermal is "cold", "first-capture" or "steady". It is supplied by the
	// operator and never inferred from elapsed time.
	Thermal string

	RunID  string
	RunDir string
	// WorkDir holds staged worktrees. Kept out of RunDir so a run's artifacts
	// stay small and readable.
	WorkDir string

	Tokenizer prompt.Tokenizer

	// EndpointOverride replaces the engine config's endpoint, which is how one
	// engine config is pointed at a mock or at a different host without editing
	// a committed file.
	EndpointOverride string

	// Trajectory, when set, drives a multi-turn replay. When nil the runner
	// sends the fixture's single turn-0 objective.
	Trajectory *config.Trajectory
	// Repetition is which steady-state repeat this call belongs to.
	Repetition int
	// ServerLog, when set, is read between requests for telemetry the engine
	// writes to its own log rather than to the response stream.
	ServerLog *client.ServerLog
}

// Result is one completed builder request plus the paths it wrote.
type Result struct {
	Record   *metrics.Record
	Manifest *prompt.Manifest
	Output   string
}

// BuildPrompt assembles and verifies the prompt for one turn without sending
// anything.
//
// Turn N's prompt is turn N-1's prompt plus the previous turn's replayed
// assistant message, the previous turn's replayed tool result, and this turn's
// objective. Nothing earlier is rewritten, summarized or reordered.
func BuildPrompt(o Options, turnIndex int) (*prompt.Manifest, error) {
	f := o.Fixture

	read := func(rel string) (string, error) {
		if rel == "" {
			return "", nil
		}
		b, err := os.ReadFile(f.Path(rel))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	agentContract, err := read(f.AgentContractFile)
	if err != nil {
		return nil, err
	}
	doctrine, err := read(f.DoctrineFile)
	if err != nil {
		return nil, err
	}
	story, err := read(f.StoryFile)
	if err != nil {
		return nil, err
	}
	objective, err := read(f.ObjectiveFile)
	if err != nil {
		return nil, err
	}

	pack, err := f.LoadContextPack(o.ContextPack)
	if err != nil {
		return nil, err
	}
	sourceCtx, _, err := prompt.SerializeFiles(f.Path(f.RepoDir), pack.Files)
	if err != nil {
		return nil, err
	}

	// The volatile tail. Turn 0's objective occupies the layout's `objective`
	// slot; everything after it is appended through the `turns` slot, so a
	// layout that places the objective early and the tail late keeps working.
	var tail []prompt.TailBlock
	if t := o.Trajectory; t != nil {
		if turnIndex < 0 || turnIndex >= len(t.Turns) {
			return nil, fmt.Errorf("turn %d is outside trajectory %s (%d turns)",
				turnIndex, t.ID, len(t.Turns))
		}
		objective = t.Turns[0].Objective
		for i := 0; i < turnIndex; i++ {
			prev := t.Turns[i]
			if strings.TrimSpace(prev.ReplayAssistant) != "" {
				tail = append(tail, prompt.TailBlock{
					ID:   fmt.Sprintf("turn%d_assistant", i),
					Role: prompt.Assistant, Text: prev.ReplayAssistant,
				})
			}
			if strings.TrimSpace(prev.ReplayResult) != "" {
				tail = append(tail, prompt.TailBlock{
					ID:   fmt.Sprintf("turn%d_result", i),
					Role: prompt.User, Text: prev.ReplayResult,
				})
			}
			tail = append(tail, prompt.TailBlock{
				ID:   fmt.Sprintf("turn%d_objective", i+1),
				Role: prompt.User, Text: t.Turns[i+1].Objective,
			})
		}
	}

	src := prompt.Sources{
		prompt.SlotAgentContract: agentContract,
		prompt.SlotDoctrine:      doctrine,
		prompt.SlotStory:         story,
		prompt.SlotSourceContext: sourceCtx,
		prompt.SlotObjective:     objective,
	}

	spec, err := toSpec(o.Layout)
	if err != nil {
		return nil, err
	}
	blocks, err := prompt.Resolve(spec, src, tail, o.Tokenizer)
	if err != nil {
		return nil, err
	}
	m, err := prompt.Build(blocks, prompt.BuildOptions{
		Layout:                spec.ID,
		EnforceStabilityOrder: spec.EnforceStabilityOrder,
		Coalesce:              spec.Coalesce,
		Tokenizer:             o.Tokenizer,
	})
	if err != nil {
		return nil, err
	}

	// The run id, the run directory and today's date are exactly the strings
	// that must never enter a reusable prefix. Checked, not assumed: a prefix
	// that silently never reuses looks like an engine problem.
	if err := m.AssertNoVolatileTokens(o.RunID, o.RunDir, o.WorkDir,
		time.Now().Format("2006-01-02")); err != nil {
		return nil, err
	}
	return m, nil
}

func toSpec(l *config.Layout) (prompt.LayoutSpec, error) {
	spec := prompt.LayoutSpec{
		ID:                    l.ID,
		EnforceStabilityOrder: l.EnforceStabilityOrder,
		Coalesce:              prompt.CoalesceMode(l.Coalesce),
	}
	for i, b := range l.Blocks {
		st, err := prompt.ParseStability(b.Stability)
		if err != nil {
			return spec, fmt.Errorf("layout %s block %d: %w", l.ID, i, err)
		}
		role, err := prompt.ParseRole(b.Role)
		if err != nil {
			return spec, fmt.Errorf("layout %s block %d: %w", l.ID, i, err)
		}
		spec.Blocks = append(spec.Blocks, prompt.SlotSpec{
			Source: b.Source, Stability: st, Role: role, Optional: b.Optional,
		})
	}
	return spec, nil
}

// TurnOptions carries the per-turn state RunTurn needs beyond Options.
type TurnOptions struct {
	Index int
	// Prev is the previous turn's manifest, used to assert this turn is a pure
	// append and to measure how many bytes it added.
	Prev *prompt.Manifest
	// Score decides whether this turn's output goes through the builder gates.
	// A replay turn before the scored one is a context-growth measurement and
	// carries no quality verdict.
	Score bool
	// Thermal overrides Options.Thermal for this turn. Turn 0 of a cold run is
	// cold; the turns that follow it are warm against a resident server.
	Thermal string
	// TurnCount is the trajectory length, recorded on every row.
	TurnCount int
}

// RunTurn performs one request and, when asked, scores it.
func RunTurn(ctx context.Context, o Options, t TurnOptions) (*Result, error) {
	f, e := o.Fixture, o.Engine

	m, err := BuildPrompt(o, t.Index)
	if err != nil {
		return nil, err
	}
	// The append property is asserted before the request is sent. A turn that
	// rewrote earlier bytes makes every prefix-cache number downstream
	// meaningless, so it is a hard error rather than a note in the report.
	if err := m.AppendsOnto(t.Prev); err != nil {
		return nil, err
	}
	appended := 0
	if t.Prev != nil {
		appended = m.PromptBytes - t.Prev.PromptBytes
	}

	thermal := t.Thermal
	if thermal == "" {
		thermal = o.Thermal
	}

	slug := fmt.Sprintf("%s.%s.%s.%s.t%d", e.Name, o.Layout.ID, o.ContextPack, thermal, t.Index)
	artDir := filepath.Join(o.RunDir, "artifacts", slug)
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return nil, err
	}
	if err := writeManifest(filepath.Join(artDir, "prompt-manifest.json"), m); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(artDir, "prompt.txt"),
		[]byte(prompt.Canonical(m.Messages)), 0o644); err != nil {
		return nil, err
	}

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
	res := c.Complete(ctx, client.Request{
		Model:                e.Model,
		Messages:             m.Messages,
		Temperature:          e.Sampling.Temperature,
		MaxTokens:            e.Sampling.MaxTokens,
		Seed:                 e.Sampling.Seed,
		TopP:                 e.Sampling.TopP,
		Thinking:             e.Sampling.Thinking,
		ThinkingOffMechanism: string(e.Sampling.ThinkingOffMechanism),
		ThinkingBudgetTokens: e.Sampling.ThinkingBudgetTokens,
		Headers:              e.Headers,
		ExtraBody:            e.ExtraBody,
	})
	// The exact request bytes are an artifact: a claim that a run was no-think
	// is checkable here rather than inferred from a config file.
	if len(res.RequestBody) > 0 {
		_ = os.WriteFile(filepath.Join(artDir, "request.json"), res.RequestBody, 0o644)
	}
	_ = os.WriteFile(filepath.Join(artDir, "output.txt"), []byte(res.Visible), 0o644)
	if res.Reasoning != "" {
		_ = os.WriteFile(filepath.Join(artDir, "reasoning.txt"), []byte(res.Reasoning), 0o644)
	}

	tel := adapter.Extract(res.RawChunks)

	rec := &metrics.Record{
		RunID:          o.RunID,
		FixtureID:      f.ID,
		FixtureVersion: f.Version,
		Lane:           metrics.LaneBuilder,
		ContextVariant: o.ContextPack,
		TurnIndex:      t.Index,
		TurnCount:      t.TurnCount,

		Engine:       e.Name,
		Model:        e.Model,
		ThinkingMode: e.Sampling.Thinking,
		Temperature:  e.Sampling.Temperature,
		MaxTokens:    e.Sampling.MaxTokens,

		EngineCommit:    nilIfEmpty(e.EngineCommit),
		ModelHash:       nilIfEmpty(e.ModelHash),
		DraftHash:       nilIfEmpty(e.DraftHash),
		TargetQuant:     nilIfEmpty(e.TargetQuant),
		KVMode:          nilIfEmpty(e.KVMode),
		SpeculationMode: nilIfEmpty(e.SpeculationMode),

		PromptLayout:          o.Layout.ID,
		PromptSHA256:          m.PromptSHA256,
		PromptBytes:           m.PromptBytes,
		StablePrefixSHA256:    m.StablePrefixSHA256,
		StablePrefixBytes:     m.StablePrefixBytes,
		AppendedBytes:         appended,
		TokenizerID:           m.TokenizerID,
		PromptTokensEstimated: m.PromptTokensEstimated,

		RequestStartedAt: time.Now().Add(-time.Duration(res.WallMS) * time.Millisecond),
		HTTPConnectedMS:  res.HTTPConnectedMS,
		ReasoningTTFTMS:  res.ReasoningTTFTMS,
		VisibleTTFTMS:    res.VisibleTTFTMS,
		WallMS:           res.WallMS,

		PrefillTokS:           tel.PrefillTokS,
		DecodeTokS:            tel.DecodeTokS,
		DecodeTokSDerived:     res.DerivedDecodeTokS(),
		PrefixCacheHitTokens:  tel.PrefixCacheHitTokens,
		PrefixCacheMissTokens: tel.PrefixCacheMissTokens,
		DFlashTau:             tel.DFlashTau,
		DFlashAcceptRate:      tel.DFlashAcceptRate,
		DFlashBlock:           tel.DFlashBlock,

		Thermal: thermal,

		OutputSHA256:    res.OutputSHA256,
		OutputBytes:     res.OutputBytes,
		TransportStatus: res.TransportStatus,
		HTTPStatus:      metrics.Ptr(res.HTTPStatus),
		Scored:          t.Score,

		Artifacts: map[string]string{
			"prompt":          filepath.ToSlash(filepath.Join("artifacts", slug, "prompt.txt")),
			"prompt_manifest": filepath.ToSlash(filepath.Join("artifacts", slug, "prompt-manifest.json")),
			"output":          filepath.ToSlash(filepath.Join("artifacts", slug, "output.txt")),
			"request_body":    filepath.ToSlash(filepath.Join("artifacts", slug, "request.json")),
		},
	}
	if res.Usage != nil {
		rec.PromptTokens = res.Usage.PromptTokens
		rec.CompletionTokens = res.Usage.CompletionTokens
		rec.ReasoningTokens = res.ReasoningTokens()
	}
	if res.Err != nil {
		rec.Error = metrics.Ptr(res.Err.Error())
	}

	// A transport failure is retained as a measurement and scored as a failure.
	// It is never retried into green.
	if res.TransportStatus != metrics.TransportOK {
		rec.Scored = true
		rec.Quality = &metrics.Quality{
			Passed: false,
			Gates: []metrics.Gate{{
				Name: "transport", Result: metrics.GateFail,
				Detail: fmt.Sprintf("%s: %v", res.TransportStatus, res.Err),
			}},
		}
		return &Result{Record: rec, Manifest: m, Output: res.Visible}, nil
	}

	if !t.Score {
		return &Result{Record: rec, Manifest: m, Output: res.Visible}, nil
	}

	wt, err := executor.Stage(ctx, f, filepath.Join(o.WorkDir, slug), false)
	if err != nil {
		return nil, err
	}
	q, err := scoring.ScoreBuilder(ctx, scoring.BuilderInput{
		Fixture:         f,
		Worktree:        wt,
		Output:          res.Visible,
		ArtifactDir:     artDir,
		ArtifactPrefix:  filepath.ToSlash(filepath.Join("artifacts", slug)),
		DiscriminateDir: filepath.Join(o.WorkDir, slug+".discriminate"),
	})
	if err != nil {
		return nil, err
	}
	rec.Quality = q
	return &Result{Record: rec, Manifest: m, Output: res.Visible}, nil
}

// RunBuilder sends the fixture's single turn-0 objective and scores it.
func RunBuilder(ctx context.Context, o Options) (*Result, error) {
	return RunTurn(ctx, o, TurnOptions{Index: 0, Score: true, TurnCount: 1})
}

// RunTrajectory replays a multi-turn builder loop against one engine.
//
// Thermal classification is per turn and is derived from the run's declared
// class rather than from elapsed time: turn 0 of a cold run is cold, and the
// turns that follow it hit a server that is now resident, so they are
// warm-resident. A run declared steady is steady throughout.
func RunTrajectory(ctx context.Context, o Options) ([]*Result, error) {
	t := o.Trajectory
	if t == nil {
		return nil, fmt.Errorf("RunTrajectory needs a trajectory")
	}
	var out []*Result
	var prev *prompt.Manifest
	for i := range t.Turns {
		thermal := o.Thermal
		if o.Thermal == "cold" && i > 0 {
			thermal = "warm-resident"
		}
		res, err := RunTurn(ctx, o, TurnOptions{
			Index:     i,
			Prev:      prev,
			Score:     i == t.ScoredTurn,
			Thermal:   thermal,
			TurnCount: len(t.Turns),
		})
		if err != nil {
			return out, fmt.Errorf("turn %d: %w", i, err)
		}
		prev = res.Manifest
		out = append(out, res)
	}
	return out, nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func writeManifest(path string, m *prompt.Manifest) error {
	b, err := marshalIndent(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// GoVersion is recorded in run identity.
func GoVersion() string { return runtime.Version() }
