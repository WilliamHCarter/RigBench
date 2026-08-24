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

	// TurnObjectives are the volatile tail, one entry per turn. v0.1 sends
	// exactly one.
	TurnObjectives []string
}

// Result is one completed builder request plus the paths it wrote.
type Result struct {
	Record   *metrics.Record
	Manifest *prompt.Manifest
	Output   string
}

// BuildPrompt assembles and verifies the prompt without sending anything.
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

	if len(o.TurnObjectives) > 0 {
		objective = o.TurnObjectives[0]
	}

	src := prompt.Sources{
		prompt.SlotAgentContract: agentContract,
		prompt.SlotDoctrine:      doctrine,
		prompt.SlotStory:         story,
		prompt.SlotSourceContext: sourceCtx,
		prompt.SlotObjective:     objective,
	}
	if turnIndex > 0 && len(o.TurnObjectives) > turnIndex {
		src[prompt.SlotTurns] = strings.Join(o.TurnObjectives[1:turnIndex+1], "\n")
	}

	spec, err := toSpec(o.Layout)
	if err != nil {
		return nil, err
	}
	blocks, err := prompt.Resolve(spec, src, o.Tokenizer)
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

// RunBuilder performs one builder request and scores it.
func RunBuilder(ctx context.Context, o Options) (*Result, error) {
	f, e := o.Fixture, o.Engine

	m, err := BuildPrompt(o, 0)
	if err != nil {
		return nil, err
	}

	slug := fmt.Sprintf("%s.%s.%s.%s.t0", e.Name, o.Layout.ID, o.ContextPack, o.Thermal)
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
		ThinkingBudgetTokens: e.Sampling.ThinkingBudgetTokens,
		Headers:              e.Headers,
	})
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
		TurnIndex:      0,

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

		Thermal: o.Thermal,

		OutputSHA256:    res.OutputSHA256,
		OutputBytes:     res.OutputBytes,
		TransportStatus: res.TransportStatus,
		HTTPStatus:      metrics.Ptr(res.HTTPStatus),

		Artifacts: map[string]string{
			"prompt":          filepath.ToSlash(filepath.Join("artifacts", slug, "prompt.txt")),
			"prompt_manifest": filepath.ToSlash(filepath.Join("artifacts", slug, "prompt-manifest.json")),
			"output":          filepath.ToSlash(filepath.Join("artifacts", slug, "output.txt")),
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
		rec.Quality = &metrics.Quality{
			Passed: false,
			Gates: []metrics.Gate{{
				Name: "transport", Result: metrics.GateFail,
				Detail: fmt.Sprintf("%s: %v", res.TransportStatus, res.Err),
			}},
		}
		return &Result{Record: rec, Manifest: m, Output: res.Visible}, nil
	}

	wt, err := executor.Stage(ctx, f, filepath.Join(o.WorkDir, slug), false)
	if err != nil {
		return nil, err
	}
	q, err := scoring.ScoreBuilder(ctx, scoring.BuilderInput{
		Fixture:        f,
		Worktree:       wt,
		Output:         res.Visible,
		ArtifactDir:    artDir,
		ArtifactPrefix: filepath.ToSlash(filepath.Join("artifacts", slug)),
	})
	if err != nil {
		return nil, err
	}
	rec.Quality = q
	return &Result{Record: rec, Manifest: m, Output: res.Visible}, nil
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
