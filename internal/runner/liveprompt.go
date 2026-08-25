package runner

import (
	"fmt"
	"os"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/client"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/prompt"
)

// buildLivePrompt assembles one live turn from the lane's own contract and
// objective plus the tail accumulated so far.
//
// The lane supplies its own agent contract and turn-0 objective, so the terse
// diff-only interaction contract can differ from the one-shot lane's without
// forking the fixture. Everything else -- doctrine, story, source context -- is
// the same bytes, which is what keeps the two lanes comparable on prompt shape.
func buildLivePrompt(o LiveOptions, tail []prompt.TailBlock) (*prompt.Manifest, error) {
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

	agentContract, err := read(o.Lane.AgentContractFile)
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
	objective, err := read(o.Lane.ObjectiveFile)
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

	spec, err := toSpec(o.Layout)
	if err != nil {
		return nil, err
	}
	blocks, err := prompt.Resolve(spec, prompt.Sources{
		prompt.SlotAgentContract: agentContract,
		prompt.SlotDoctrine:      doctrine,
		prompt.SlotStory:         story,
		prompt.SlotSourceContext: sourceCtx,
		prompt.SlotObjective:     objective,
	}, tail, o.Tokenizer)
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
	// The source context is serialized from the *frozen* fixture, never from
	// the live worktree, so the stable prefix cannot drift as the model edits.
	// Checked here rather than trusted, along with the usual volatile strings.
	if err := m.AssertNoVolatileTokens(o.RunID, o.RunDir, o.WorkDir,
		time.Now().Format("2006-01-02")); err != nil {
		return nil, err
	}
	return m, nil
}

// liveRecord builds the per-request row for one live turn.
// liveContext carries the per-turn measurements the record needs beyond the
// prompt and the response.
type liveContext struct {
	Turn          int
	TurnRole      string
	TurnMaxTokens int
	// SharedTokens is how much of this prompt the previous turn's prompt
	// already contained, in estimated tokens. Runner-measured, so it exists
	// even against a backend that reports nothing.
	SharedTokens   int
	AppendedTokens int
	ReusableTokens int
	// LogTelemetry is what the engine wrote to its own log during this request.
	LogTelemetry client.Telemetry
}

func liveRecord(o LiveOptions, m *prompt.Manifest, res *client.Result,
	adapter client.Adapter, lc liveContext) *metrics.Record {

	e := o.Engine
	turn := lc.Turn
	tel := adapter.Extract(res.RawChunks)
	tel.Merge(lc.LogTelemetry)

	rec := &metrics.Record{
		RunID:          o.RunID,
		FixtureID:      o.Fixture.ID,
		FixtureVersion: o.Fixture.Version,
		Lane:           metrics.Lane(o.LaneName),
		ContextVariant: o.ContextPack,
		TurnIndex:      turn,
		Repetition:     o.Repetition,

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
		DraftTokS:             tel.DraftTokS,
		VerifyTokS:            tel.VerifyTokS,
		VerifyMS:              tel.VerifyMS,
		PrefixCacheHitTokens:  tel.PrefixCacheHitTokens,
		PrefixCacheMissTokens: tel.PrefixCacheMissTokens,
		DFlashTau:             tel.DFlashTau,
		DFlashAcceptRate:      tel.DFlashAcceptRate,
		DFlashBlock:           tel.DFlashBlock,
		SpeculativeWindows:    tel.SpeculativeWindows,
		AcceptedTokens:        tel.AcceptedTokens,
		RejectedTokens:        tel.RejectedTokens,

		SpeculationMethodObserved: tel.SpeculationMethod,
		KVFormatObserved:          tel.KVFormat,
		ResolvedModel:             tel.ResolvedModel,

		TurnRole:      lc.TurnRole,
		TurnMaxTokens: lc.TurnMaxTokens,
		FinishReason:  res.FinishReason,

		ExperimentFamily:   e.Experiment.Family,
		ExperimentVariant:  e.Experiment.Variant,
		ExperimentBaseline: e.Experiment.Baseline,
		KnobFingerprint:    e.Knobs.Fingerprint(),
		ReplicaID:          e.Knobs.ReplicaID,
		GPU:                e.Knobs.GPU,

		Thermal:         o.Thermal,
		OutputSHA256:    res.OutputSHA256,
		OutputBytes:     res.OutputBytes,
		TransportStatus: res.TransportStatus,
		HTTPStatus:      metrics.Ptr(res.HTTPStatus),

		Artifacts: map[string]string{
			"turn_dir": fmt.Sprintf("artifacts/%s.%s.%s.%s.r%d/t%d",
				e.Name, o.LaneName, o.Layout.ID, o.Thermal, o.Repetition, turn),
		},
	}
	if res.Usage != nil {
		rec.PromptTokens = res.Usage.PromptTokens
		rec.CompletionTokens = res.Usage.CompletionTokens
		rec.ReasoningTokens = res.ReasoningTokens()
	}
	rec.Cache = metrics.ComputeCache(rec.PromptTokens, tel.PrefixCacheHitTokens,
		lc.ReusableTokens, lc.SharedTokens, lc.AppendedTokens)
	if res.Err != nil {
		rec.Error = metrics.Ptr(res.Err.Error())
	}
	return rec
}
