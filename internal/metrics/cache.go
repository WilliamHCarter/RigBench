package metrics

// Cache accounting for one request.
//
// Three different numbers get called "the cache hit" and they are not the same
// measurement:
//
//   - what the *prompt* makes reusable, measured by the runner from the bytes
//     it serialized. Engine-independent, available even against a backend that
//     reports nothing, and the only one that can be compared across engines.
//   - what this turn *shares with the previous turn*, which in a multi-turn
//     loop is the number that actually predicts prefill work.
//   - what the *engine says* it reused, which is the ground truth when exposed
//     and null when not.
//
// All three are recorded. A benchmark that kept only the third would have
// nothing to say about a backend without telemetry; one that kept only the
// first would be reporting a property of the prompt as if it were a property of
// the run.
type CacheAccounting struct {
	// --- runner-measured, always present ---
	// ReusablePrefixTokens is the stable prefix's length, estimated with the
	// recorded tokenizer. Estimated, because the exact tokenizer is not wired
	// until the v0.5 context matrix; the id travels with it.
	ReusablePrefixTokens int `json:"reusable_prefix_tokens_estimated"`
	// SharedWithPreviousTokens is the byte prefix this turn shares with the
	// previous turn of the same story, in estimated tokens. Zero on turn 0.
	SharedWithPreviousTokens int `json:"shared_with_previous_tokens_estimated"`
	// AppendedTokens is what this turn added, in estimated tokens.
	AppendedTokens int `json:"appended_tokens_estimated"`

	// --- engine-reported, nullable ---
	// CachedTokens is what the engine says it reused.
	CachedTokens *int `json:"cached_tokens"`
	// NewlyPrefilledTokens is derived as prompt minus cached, and is null
	// unless both were reported. Never guessed from the runner's own estimate:
	// mixing a server counter with a client heuristic produces a number
	// belonging to neither.
	NewlyPrefilledTokens *int `json:"newly_prefilled_tokens"`
	// ReuseFraction is cached over prompt tokens, in [0,1]. Null when either
	// side is missing.
	ReuseFraction *float64 `json:"reuse_fraction"`

	// Verdict classifies the engine's answer: "full", "partial", "miss", or
	// "not-exposed". A backend that reported nothing is not a backend that
	// reused nothing, and the two must never render the same.
	Verdict string `json:"verdict"`
}

const (
	CacheFull       = "full"
	CachePartial    = "partial"
	CacheMiss       = "miss"
	CacheNotExposed = "not-exposed"
)

// ComputeCache fills the derived fields from what is known.
func ComputeCache(promptTokens *int, cached *int, reusablePrefix, sharedPrev, appended int) CacheAccounting {
	c := CacheAccounting{
		ReusablePrefixTokens:     reusablePrefix,
		SharedWithPreviousTokens: sharedPrev,
		AppendedTokens:           appended,
		CachedTokens:             cached,
		Verdict:                  CacheNotExposed,
	}
	if cached == nil {
		return c
	}
	switch {
	case *cached == 0:
		c.Verdict = CacheMiss
	case promptTokens != nil && *promptTokens > 0 && *cached >= *promptTokens:
		c.Verdict = CacheFull
	default:
		c.Verdict = CachePartial
	}
	if promptTokens != nil && *promptTokens > 0 {
		newly := *promptTokens - *cached
		if newly < 0 {
			newly = 0
		}
		c.NewlyPrefilledTokens = Ptr(newly)
		c.ReuseFraction = Ptr(float64(*cached) / float64(*promptTokens))
	}
	return c
}
