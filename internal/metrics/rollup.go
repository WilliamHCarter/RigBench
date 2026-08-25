package metrics

import "sort"

// Rollup fills a story's Inference and Task groups from its per-turn records.
//
// Medians for rates, totals for counts. A rate averaged over turns of very
// different lengths would be dominated by the shortest one, and a count is only
// meaningful summed.
//
// Every nullable field stays null when no turn reported it. A story against a
// backend with no telemetry has an Inference group full of nulls and a Task
// group that is complete, which is exactly the right shape: the workload
// outcome does not depend on the engine being instrumented.
func (s *Story) Rollup(records []*Record) {
	inf := InferenceMetrics{
		CacheVerdicts: map[string]int{},
		FinishReasons: map[string]int{},
	}
	task := TaskMetrics{
		Success:             s.HiddenGreen,
		StopReason:          s.StopReason,
		ModelTurns:          s.ModelTurns,
		PatchAttempts:       s.PatchAttempts,
		FailedApplyAttempts: s.FailedApplyAttempts,
		NoDiffTurns:         s.NoDiffTurns,
		ToolWallMS:          s.ToolWallMS,
		ModelWallMS:         s.ModelWallMS,
		TotalStoryWallMS:    s.TotalWallMS,

		TimeToFirstCompilingPatchMS: s.TimeToFirstCompilingPatchMS,
		TimeToDiscriminatingGreenMS: s.TimeToDiscriminatingGreenMS,
		TimeToHiddenGreenMS:         s.TimeToHiddenGreenMS,
		FinalHiddenWallMS:           s.FinalHiddenWallMS,
	}

	var prefill, decode, decodeDerived, draft, verify, vttft, rttft, tau, accept []float64
	for _, r := range records {
		inf.ModelWallMS += r.WallMS
		collect(&prefill, r.PrefillTokS)
		collect(&decode, r.DecodeTokS)
		collect(&decodeDerived, r.DecodeTokSDerived)
		collect(&draft, r.DraftTokS)
		collect(&verify, r.VerifyTokS)
		collect(&vttft, r.VisibleTTFTMS)
		collect(&rttft, r.ReasoningTTFTMS)
		collect(&tau, r.DFlashTau)
		collect(&accept, r.DFlashAcceptRate)

		sumInto(&inf.SpeculativeWindows, r.SpeculativeWindows)
		sumInto(&inf.AcceptedTokens, r.AcceptedTokens)
		sumInto(&inf.RejectedTokens, r.RejectedTokens)
		sumInto(&inf.PromptTokensTotal, r.PromptTokens)
		sumInto(&inf.CompletionTokensTotal, r.CompletionTokens)
		sumInto(&inf.ReasoningTokensTotal, r.ReasoningTokens)
		sumInto(&inf.CachedTokensTotal, r.Cache.CachedTokens)
		sumInto(&inf.NewlyPrefilledTokensTotal, r.Cache.NewlyPrefilledTokens)
		inf.SharedWithPreviousTotal += r.Cache.SharedWithPreviousTokens

		if r.Cache.Verdict != "" {
			inf.CacheVerdicts[r.Cache.Verdict]++
		}
		if r.FinishReason != "" {
			inf.FinishReasons[r.FinishReason]++
		}
		if inf.KnobFingerprint == "" {
			inf.KnobFingerprint = r.KnobFingerprint
		}
		if inf.ResolvedModel == "" {
			inf.ResolvedModel = r.ResolvedModel
		}
		if inf.KVFormat == "" {
			inf.KVFormat = r.KVFormatObserved
		}
		if inf.ReplicaID == "" {
			inf.ReplicaID = r.ReplicaID
		}
		if inf.GPU == "" {
			inf.GPU = r.GPU
		}
	}

	inf.PrefillTokS = median(prefill)
	inf.DecodeTokS = median(decode)
	inf.DecodeTokSDerived = median(decodeDerived)
	inf.DraftTokS = median(draft)
	inf.VerifyTokS = median(verify)
	inf.VisibleTTFTMS = median(vttft)
	inf.ReasoningTTFTMS = median(rttft)
	inf.DFlashTau = median(tau)
	inf.DFlashAcceptRate = median(accept)

	s.Inference = inf
	s.Task = task
}

func collect(dst *[]float64, v *float64) {
	if v != nil {
		*dst = append(*dst, *v)
	}
}

func sumInto(dst **int, v *int) {
	if v == nil {
		return
	}
	if *dst == nil {
		*dst = Ptr(*v)
		return
	}
	**dst += *v
}

// median returns nil for an empty set. Nil is not zero: a rate no turn reported
// and a rate of zero are different facts.
func median(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	sort.Float64s(v)
	mid := len(v) / 2
	if len(v)%2 == 1 {
		return Ptr(v[mid])
	}
	return Ptr((v[mid-1] + v[mid]) / 2)
}
