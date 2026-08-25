package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Knobs are the engine tuning axes, as data.
//
// The benchmark does not know which values are good and must not acquire an
// opinion. Its job is to make an axis expressible, recorded, and comparable, so
// a sweep can be driven from outside without editing the harness. Every field
// is optional and nothing is defaulted: an unset knob means "whatever the
// server was already doing", which is exactly what an unattested run is.
//
// None of these are sent on the request. They are process-level settings on
// every local engine worth benchmarking, so they reach the server through the
// preparation hook, which receives each one as an environment variable. That
// keeps the split clean: the config declares the axis, the hook knows how to
// apply it, and the benchmark only records what was asked for and what the
// server said it resolved to.
type Knobs struct {
	// --- weights ---
	TargetModel  string `json:"target_model,omitempty"`
	TargetQuant  string `json:"target_quant,omitempty"`
	TargetFile   string `json:"target_file,omitempty"`
	TargetSHA256 string `json:"target_sha256,omitempty"`

	DraftModel  string `json:"draft_model,omitempty"`
	DraftQuant  string `json:"draft_quant,omitempty"`
	DraftFile   string `json:"draft_file,omitempty"`
	DraftSHA256 string `json:"draft_sha256,omitempty"`

	// --- KV ---
	KVDtype string `json:"kv_dtype,omitempty"`

	// --- speculation ---
	// SpeculationMethod names the engine's mechanism ("off", "dflash", ...).
	// The drafter architecture is a separate axis and belongs in DraftModel.
	SpeculationMethod string `json:"speculation_method,omitempty"`
	SpeculativeBlock  *int   `json:"speculative_block,omitempty"`
	SpeculativeBudget *int   `json:"speculative_budget,omitempty"`
	AdaptiveBlock     *bool  `json:"adaptive_block,omitempty"`
	VerifyMode        string `json:"verify_mode,omitempty"`
	PM4Verify         *bool  `json:"pm4_verify,omitempty"`

	// --- prefill and cache ---
	PrefillSpeculation  string `json:"prefill_speculation,omitempty"`
	PromptCacheMode     string `json:"prompt_cache_mode,omitempty"`
	PromptCacheCapacity *int   `json:"prompt_cache_capacity,omitempty"`
	// PromptCacheCapacityUnit is required whenever a capacity is given. A bare
	// number that might be tokens, megabytes or sequences is not a measurement.
	PromptCacheCapacityUnit string `json:"prompt_cache_capacity_unit,omitempty"`

	// --- shape ---
	ContextTokens   *int `json:"context_tokens,omitempty"`
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`

	// --- placement ---
	Concurrency *int   `json:"concurrency,omitempty"`
	ReplicaID   string `json:"replica_id,omitempty"`
	// GPU is free-form because the right spelling is the engine's, not ours:
	// "0", "0,1", a UUID. It is recorded verbatim and compared as a string.
	GPU string `json:"gpu,omitempty"`
	// GPUEnv is passed to the preparation hook verbatim, which is how
	// HIP_VISIBLE_DEVICES and friends get set without the harness knowing what
	// they mean.
	GPUEnv map[string]string `json:"gpu_env,omitempty"`

	// Extra is any axis this struct has not grown a field for yet. Recorded and
	// swept exactly like the named ones, so a new knob never blocks on a
	// harness change.
	Extra map[string]string `json:"extra,omitempty"`
}

// Experiment groups runs for reporting. It is metadata, never logic: the
// benchmark does not decide what to sweep, it only lets a sweep be named so the
// eventual report can group by it.
type Experiment struct {
	// Family is the sweep, e.g. "draft-quant", "kv", "block", "pm4-verify".
	Family string `json:"family,omitempty"`
	// Variant is this config's point in the sweep, e.g. "mq6".
	Variant string `json:"variant,omitempty"`
	// Baseline names the variant this one is compared against.
	Baseline string `json:"baseline,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// Validate rejects only the combinations that would produce an uninterpretable
// record. It has no opinion about values.
func (k *Knobs) Validate(where string) error {
	if k.PromptCacheCapacity != nil && k.PromptCacheCapacityUnit == "" {
		return fmt.Errorf("%s: prompt_cache_capacity is set but "+
			"prompt_cache_capacity_unit is not. A bare number that might be tokens, "+
			"megabytes or sequences is not a measurement", where)
	}
	if k.PromptCacheCapacityUnit != "" && k.PromptCacheCapacity == nil {
		return fmt.Errorf("%s: prompt_cache_capacity_unit is set with no capacity", where)
	}
	return nil
}

// Env renders the knobs as environment variables for the preparation hook.
//
// Named AGENTBENCH_KNOB_* so a hook can enumerate them, and sorted so the
// preparation log is byte-stable between runs that asked for the same thing.
// Unset knobs are absent rather than empty: a hook must be able to tell "leave
// it alone" from "set it to nothing".
func (k *Knobs) Env() []string {
	kv := map[string]string{}
	put := func(name, val string) {
		if val != "" {
			kv["AGENTBENCH_KNOB_"+name] = val
		}
	}
	putI := func(name string, v *int) {
		if v != nil {
			kv["AGENTBENCH_KNOB_"+name] = strconv.Itoa(*v)
		}
	}
	putB := func(name string, v *bool) {
		if v != nil {
			kv["AGENTBENCH_KNOB_"+name] = strconv.FormatBool(*v)
		}
	}

	put("TARGET_MODEL", k.TargetModel)
	put("TARGET_QUANT", k.TargetQuant)
	put("TARGET_FILE", k.TargetFile)
	put("TARGET_SHA256", k.TargetSHA256)
	put("DRAFT_MODEL", k.DraftModel)
	put("DRAFT_QUANT", k.DraftQuant)
	put("DRAFT_FILE", k.DraftFile)
	put("DRAFT_SHA256", k.DraftSHA256)
	put("KV_DTYPE", k.KVDtype)
	put("SPECULATION_METHOD", k.SpeculationMethod)
	putI("SPECULATIVE_BLOCK", k.SpeculativeBlock)
	putI("SPECULATIVE_BUDGET", k.SpeculativeBudget)
	putB("ADAPTIVE_BLOCK", k.AdaptiveBlock)
	put("VERIFY_MODE", k.VerifyMode)
	putB("PM4_VERIFY", k.PM4Verify)
	put("PREFILL_SPECULATION", k.PrefillSpeculation)
	put("PROMPT_CACHE_MODE", k.PromptCacheMode)
	putI("PROMPT_CACHE_CAPACITY", k.PromptCacheCapacity)
	put("PROMPT_CACHE_CAPACITY_UNIT", k.PromptCacheCapacityUnit)
	putI("CONTEXT_TOKENS", k.ContextTokens)
	putI("MAX_OUTPUT_TOKENS", k.MaxOutputTokens)
	putI("CONCURRENCY", k.Concurrency)
	put("REPLICA_ID", k.ReplicaID)
	put("GPU", k.GPU)
	for name, v := range k.Extra {
		put(strings.ToUpper(name), v)
	}
	// GPU env is passed through under its own names, not prefixed: the engine
	// reads HIP_VISIBLE_DEVICES, not AGENTBENCH_KNOB_HIP_VISIBLE_DEVICES.
	for name, v := range k.GPUEnv {
		if v != "" {
			kv[name] = v
		}
	}

	out := make([]string, 0, len(kv))
	for name, v := range kv {
		out = append(out, name+"="+v)
	}
	sort.Strings(out)
	return out
}

// Fingerprint is a stable, human-readable summary of every set knob. Two
// configs with the same fingerprint asked the server for the same thing.
func (k *Knobs) Fingerprint() string {
	env := k.Env()
	parts := make([]string, 0, len(env))
	for _, e := range env {
		parts = append(parts, strings.TrimPrefix(e, "AGENTBENCH_KNOB_"))
	}
	return strings.Join(parts, " ")
}
