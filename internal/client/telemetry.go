package client

import (
	"encoding/json"

	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

// Telemetry is the optional, engine-specific part of a measurement. Every field
// is a pointer and every pointer is nil unless an engine actually reported the
// value. Nothing here is derived, inferred or defaulted.
type Telemetry struct {
	PrefillTokS *float64
	DecodeTokS  *float64

	PrefixCacheHitTokens  *int
	PrefixCacheMissTokens *int

	DFlashTau        *float64
	DFlashAcceptRate *float64
	DFlashBlock      *int
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
			mergeInt(&t.PrefixCacheHitTokens, m, "prefix_cache_hit_tokens", "cached_tokens")
			mergeInt(&t.PrefixCacheMissTokens, m, "prefix_cache_miss_tokens")

			if spec, ok := m["speculation"]; ok {
				var s map[string]json.RawMessage
				if json.Unmarshal(spec, &s) == nil {
					mergeFloat(&t.DFlashTau, s, "tau")
					mergeFloat(&t.DFlashAcceptRate, s, "accept_rate", "acceptance")
					mergeInt(&t.DFlashBlock, s, "block", "active_block")
				}
			}
			mergeFloat(&t.DFlashTau, m, "dflash_tau")
			mergeFloat(&t.DFlashAcceptRate, m, "dflash_accept_rate")
			mergeInt(&t.DFlashBlock, m, "dflash_block")
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
