package client

import (
	"encoding/json"

	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

// Telemetry is the optional, engine-specific part of a measurement. Every field
// is a pointer and every pointer is nil unless an engine actually reported the
// value. Nothing here is derived, inferred or defaulted.
type Telemetry struct {
	// --- throughput, by stage ---
	PrefillTokS *float64
	DecodeTokS  *float64
	// DraftTokS and VerifyTokS decompose a speculative decode. A configuration
	// whose draft is fast and whose verification is slow, and one where the
	// reverse holds, have the same headline decode rate and different fixes.
	DraftTokS  *float64
	VerifyTokS *float64
	VerifyMS   *float64

	// --- prompt cache ---
	PrefixCacheHitTokens  *int
	PrefixCacheMissTokens *int

	// --- speculative internals ---
	SpeculationMethod string
	DFlashTau         *float64
	DFlashAcceptRate  *float64
	DFlashBlock       *int
	// SpeculativeWindows is how many speculative rounds ran. An acceptance rate
	// over three windows and one over five thousand are not the same evidence,
	// and the rate alone cannot tell them apart.
	SpeculativeWindows *int
	AcceptedTokens     *int
	RejectedTokens     *int

	// --- what was actually loaded ---
	// The requested model alias and the artifact the server resolved it to are
	// different strings. Conflating them has already cost a campaign, so the
	// resolved side is recorded separately and never overwrites the request.
	ResolvedModel string
	KVFormat      string
	TargetFile    string
	TargetSHA256  string
	DraftFile     string
	DraftSHA256   string
	EngineCommit  string
}

// Adapter reads engine-specific fields out of the raw SSE chunks.
//
// This interface is the only place a backend's private telemetry may enter the
// benchmark. Benchmark semantics never fork to match an engine: if a backend
// exposes nothing, the common wall-clock and quality metrics still stand and
// these fields stay null.
type Adapter interface {
	Name() string
	Extract(chunks []json.RawMessage) Telemetry
}

// Generic extracts nothing. It is the correct adapter for any endpoint whose
// telemetry shape has not been confirmed, and it is the default.
type Generic struct{}

func (Generic) Name() string                          { return "generic" }
func (Generic) Extract(_ []json.RawMessage) Telemetry { return Telemetry{} }

// Hipfire reads the fields Hipfire is expected to publish alongside the final
// usage block.
//
// PROVISIONAL: the key names below have not been confirmed against a running
// Hipfire server from this workstation. They are read defensively -- an absent
// or wrongly-typed key yields nil, never zero -- so a wrong guess produces
// "not exposed" rather than a fabricated number. Confirm against the real
// server and update the accepted keys; do not add a default.
type Hipfire struct{}

func (Hipfire) Name() string { return "hipfire" }

func (Hipfire) Extract(chunks []json.RawMessage) Telemetry {
	var t Telemetry
	for _, raw := range chunks {
		var top map[string]json.RawMessage
		if json.Unmarshal(raw, &top) != nil {
			continue
		}
		// Timings may arrive either at the top level or nested under a vendor
		// object. Both are accepted; neither is required.
		for _, key := range []string{"timings", "hipfire", "metrics"} {
			sub, ok := top[key]
			if !ok {
				continue
			}
			var m map[string]json.RawMessage
			if json.Unmarshal(sub, &m) != nil {
				continue
			}
			mergeFloat(&t.PrefillTokS, m, "prefill_tok_s", "prefill_tokens_per_second")
			mergeFloat(&t.DecodeTokS, m, "decode_tok_s", "decode_tokens_per_second")
			mergeFloat(&t.DraftTokS, m, "draft_tok_s", "drafter_tok_s")
			mergeFloat(&t.VerifyTokS, m, "verify_tok_s", "verification_tok_s")
			mergeFloat(&t.VerifyMS, m, "verify_ms", "verification_ms")
			mergeInt(&t.PrefixCacheHitTokens, m, "prefix_cache_hit_tokens", "cached_tokens")
			mergeInt(&t.PrefixCacheMissTokens, m, "prefix_cache_miss_tokens")
			mergeStr(&t.KVFormat, m, "kv_format", "kv_cache", "kv_dtype")
			mergeStr(&t.ResolvedModel, m, "resolved_model", "model_file", "model")

			if spec, ok := m["speculation"]; ok {
				var s map[string]json.RawMessage
				if json.Unmarshal(spec, &s) == nil {
					mergeFloat(&t.DFlashTau, s, "tau")
					mergeFloat(&t.DFlashAcceptRate, s, "accept_rate", "acceptance")
					mergeInt(&t.DFlashBlock, s, "block", "active_block")
					mergeInt(&t.SpeculativeWindows, s, "windows", "n_windows")
					mergeInt(&t.AcceptedTokens, s, "accepted", "accepted_tokens")
					mergeInt(&t.RejectedTokens, s, "rejected", "rejected_tokens")
					mergeStr(&t.SpeculationMethod, s, "mode", "method", "drafter")
					mergeFloat(&t.DraftTokS, s, "draft_tok_s")
					mergeFloat(&t.VerifyTokS, s, "verify_tok_s")
				}
			}
			mergeFloat(&t.DFlashTau, m, "dflash_tau")
			mergeFloat(&t.DFlashAcceptRate, m, "dflash_accept_rate")
			mergeInt(&t.DFlashBlock, m, "dflash_block")
			mergeInt(&t.SpeculativeWindows, m, "speculative_windows")
			mergeInt(&t.AcceptedTokens, m, "accepted_tokens")
			mergeInt(&t.RejectedTokens, m, "rejected_tokens")
		}
	}
	return t
}

func mergeFloat(dst **float64, m map[string]json.RawMessage, keys ...string) {
	if *dst != nil {
		return
	}
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			*dst = metrics.Ptr(v)
			return
		}
	}
}

func mergeStr(dst *string, m map[string]json.RawMessage, keys ...string) {
	if *dst != "" {
		return
	}
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			*dst = v
			return
		}
	}
}

func mergeInt(dst **int, m map[string]json.RawMessage, keys ...string) {
	if *dst != nil {
		return
	}
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var v int
		if json.Unmarshal(raw, &v) == nil {
			*dst = metrics.Ptr(v)
			return
		}
	}
}

// AdapterFor returns the named adapter. An unknown name is an error at the
// caller rather than a silent fallback, because silently degrading to Generic
// would look like an engine that exposes nothing.
func AdapterFor(name string) (Adapter, bool) {
	switch name {
	case "", "generic":
		return Generic{}, true
	case "hipfire":
		return Hipfire{}, true
	}
	return nil, false
}

// Merge folds a second telemetry source into t, preferring values already
// present.
//
// Used to combine what the response stream reported with what a server log line
// said. Neither silently overwrites the other: the stream is preferred because
// it is unambiguously attributable to this request, and the log fills the gaps
// it left. A field neither source reported stays null.
func (t *Telemetry) Merge(o Telemetry) {
	mergeF(&t.PrefillTokS, o.PrefillTokS)
	mergeF(&t.DecodeTokS, o.DecodeTokS)
	mergeF(&t.DraftTokS, o.DraftTokS)
	mergeF(&t.VerifyTokS, o.VerifyTokS)
	mergeF(&t.VerifyMS, o.VerifyMS)
	mergeI(&t.PrefixCacheHitTokens, o.PrefixCacheHitTokens)
	mergeI(&t.PrefixCacheMissTokens, o.PrefixCacheMissTokens)
	mergeF(&t.DFlashTau, o.DFlashTau)
	mergeF(&t.DFlashAcceptRate, o.DFlashAcceptRate)
	mergeI(&t.DFlashBlock, o.DFlashBlock)
	mergeI(&t.SpeculativeWindows, o.SpeculativeWindows)
	mergeI(&t.AcceptedTokens, o.AcceptedTokens)
	mergeI(&t.RejectedTokens, o.RejectedTokens)
	mergeStrField(&t.SpeculationMethod, o.SpeculationMethod)
	mergeStrField(&t.ResolvedModel, o.ResolvedModel)
	mergeStrField(&t.KVFormat, o.KVFormat)
	mergeStrField(&t.TargetFile, o.TargetFile)
	mergeStrField(&t.TargetSHA256, o.TargetSHA256)
	mergeStrField(&t.DraftFile, o.DraftFile)
	mergeStrField(&t.DraftSHA256, o.DraftSHA256)
	mergeStrField(&t.EngineCommit, o.EngineCommit)
}

func mergeF(dst **float64, v *float64) {
	if *dst == nil && v != nil {
		*dst = v
	}
}

func mergeI(dst **int, v *int) {
	if *dst == nil && v != nil {
		*dst = v
	}
}

func mergeStrField(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}
