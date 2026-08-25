// Package metrics defines the per-request measurement record and its writer.
//
// One rule governs every field here: engine-specific telemetry is nullable and
// is never fabricated. If Hipfire exposes a speculation acceptance rate and
// another backend does not, the field is null for that backend rather than
// zero. A result that cannot distinguish "not exposed" from "measured zero" is
// not a measurement.
package metrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// SchemaVersion is bumped when a field changes meaning. Rows from different
// schema versions are not comparable and the reporter refuses to mix them.
const SchemaVersion = "agentbench/request/v1"

// Lane names the workload being measured.
type Lane string

const (
	LaneBuilder Lane = "builder"
	// LaneBuilderLive is the canonical live edit/test/repair workload.
	LaneBuilderLive Lane = "builder-live"
	// LaneBuilderRepairDiagnostic shows the model the hidden suite's failure
	// after a turn. Diagnostic only; never comparable with builder-live.
	LaneBuilderRepairDiagnostic Lane = "builder-repair-diagnostic"
	LaneReviewer                Lane = "reviewer"
)

// TransportStatus separates a model that answered badly from a request that
// never completed. They are different failures and must not aggregate together.
type TransportStatus string

const (
	TransportOK        TransportStatus = "ok"
	TransportTimeout   TransportStatus = "timeout"
	TransportHTTPErr   TransportStatus = "http_error"
	TransportStreamErr TransportStatus = "stream_error"
	TransportRefused   TransportStatus = "connection_refused"
)

// GateResult is deliberately three-valued. A gate that could not run is not a
// gate that passed, and it is not a gate that failed either.
type GateResult string

const (
	GatePass    GateResult = "pass"
	GateFail    GateResult = "fail"
	GateSkipped GateResult = "skipped"
)

// Gate is one quality gate outcome with the evidence that produced it.
type Gate struct {
	Name     string     `json:"name"`
	Result   GateResult `json:"result"`
	Detail   string     `json:"detail,omitempty"`
	Command  string     `json:"command,omitempty"`
	ExitCode *int       `json:"exit_code"`
	// Artifact is the run-relative path to the captured stdout/stderr, if any.
	Artifact string `json:"artifact,omitempty"`
}

// Quality carries the post-generation verdict for one request.
type Quality struct {
	// Passed is true only when every required gate passed. A skipped required
	// gate leaves this false.
	Passed bool   `json:"passed"`
	Gates  []Gate `json:"gates"`
	// PatchFiles are the paths the candidate patch touches, sorted.
	PatchFiles []string `json:"patch_files,omitempty"`
	// OutOfScopeFiles are the subset of PatchFiles the fixture does not own.
	OutOfScopeFiles []string `json:"out_of_scope_files,omitempty"`
	// VisibleTestsPassed / HiddenTestsPassed are counts when the runner could
	// parse them from the build output, and null when it could not.
	VisibleTestsPassed *int `json:"visible_tests_passed"`
	HiddenTestsPassed  *int `json:"hidden_tests_passed"`
}

// FailedGates returns the names of gates that did not pass, in order.
func (q *Quality) FailedGates() []string {
	if q == nil {
		return nil
	}
	var out []string
	for _, g := range q.Gates {
		if g.Result != GatePass {
			out = append(out, fmt.Sprintf("%s=%s", g.Name, g.Result))
		}
	}
	return out
}

// Record is one model call. Field order follows the benchmark contract so a
// hand-read JSONL row is legible top to bottom: what was run, on what, with
// what prompt, how fast, and whether it was any good.
type Record struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`

	// --- what workload ---
	FixtureID      string  `json:"fixture_id"`
	FixtureVersion string  `json:"fixture_version"`
	Lane           Lane    `json:"lane"`
	ContextVariant string  `json:"context_variant"`
	TurnIndex      int     `json:"turn_index"`
	ReviewerLens   *string `json:"reviewer_lens"`

	// --- on what engine ---
	Engine          string  `json:"engine"`
	EngineCommit    *string `json:"engine_commit"`
	Model           string  `json:"model"`
	ModelHash       *string `json:"model_hash"`
	TargetQuant     *string `json:"target_quant"`
	KVMode          *string `json:"kv_mode"`
	SpeculationMode *string `json:"speculation_mode"`
	DraftHash       *string `json:"draft_hash"`
	ThinkingMode    string  `json:"thinking_mode"`
	Temperature     float64 `json:"temperature"`
	MaxTokens       int     `json:"max_tokens"`

	// --- with what prompt ---
	PromptLayout string `json:"prompt_layout"`
	PromptSHA256 string `json:"prompt_sha256"`
	PromptBytes  int    `json:"prompt_bytes"`
	// StablePrefix describes the leading span a served prefix cache could reuse
	// across turns. It is verified at serialization time to be a digest of a
	// real byte prefix of the prompt, so a layout A/B can be read from it.
	StablePrefixSHA256 string `json:"stable_prefix_sha256"`
	StablePrefixBytes  int    `json:"stable_prefix_bytes"`
	// AppendedBytes is how many bytes this turn added to the previous turn's
	// prompt. Zero on turn 0. A turn that is not a pure append is a failure, not
	// a large number here.
	AppendedBytes int `json:"appended_bytes"`
	// TurnCount is the length of the trajectory this row belongs to.
	TurnCount int `json:"turn_count"`
	// ToolWallMS is the host-side work this turn caused: applying the patch,
	// building, and running tests. Kept separate from WallMS, which is the
	// model request alone, because "the model is slow" and "the build is slow"
	// are different problems.
	ToolWallMS float64 `json:"tool_wall_ms"`
	// PatchApplied is whether this turn's diff reached the working tree.
	PatchApplied bool `json:"patch_applied"`
	// Repetition is which steady-state repeat this row came from, 0-based.
	// Recorded so a cell's dispersion can be traced back to individual repeats,
	// and so a repeat that drifted can be found rather than averaged away.
	Repetition int `json:"repetition"`
	// TokenizerID names what produced PromptTokensEstimated. It is not the
	// target tokenizer until the v0.5 verifier lands, and a context variant is
	// never labelled from this number.
	TokenizerID           string `json:"tokenizer_id"`
	PromptTokensEstimated int    `json:"prompt_tokens_estimated"`
	// PromptTokens and below come from the engine's own usage block, so they are
	// null when the engine did not report them.
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	ReasoningTokens  *int `json:"reasoning_tokens"`

	// --- how fast ---
	RequestStartedAt time.Time `json:"request_started_at"`
	HTTPConnectedMS  *float64  `json:"http_connected_ms"`
	ReasoningTTFTMS  *float64  `json:"reasoning_ttft_ms"`
	VisibleTTFTMS    *float64  `json:"visible_ttft_ms"`
	WallMS           float64   `json:"wall_ms"`
	PrefillTokS      *float64  `json:"prefill_tok_s"`
	DecodeTokS       *float64  `json:"decode_tok_s"`
	// DecodeTokSDerived is the runner's own completion-tokens-over-stream-window
	// figure. Kept separate from DecodeTokS, which is whatever the engine
	// reported: one is a client stopwatch and the other a server counter, and
	// merging them would produce a number belonging to neither.
	DecodeTokSDerived *float64 `json:"decode_tok_s_derived"`

	DraftTokS  *float64 `json:"draft_tok_s"`
	VerifyTokS *float64 `json:"verify_tok_s"`
	VerifyMS   *float64 `json:"verify_ms"`

	PrefixCacheHitTokens  *int `json:"prefix_cache_hit_tokens"`
	PrefixCacheMissTokens *int `json:"prefix_cache_miss_tokens"`
	// Cache is the three-way accounting: what the prompt makes reusable, what
	// this turn shares with the previous one, and what the engine says it
	// reused. See cache.go for why one number is not enough.
	Cache CacheAccounting `json:"cache"`

	DFlashTau        *float64 `json:"dflash_tau"`
	DFlashAcceptRate *float64 `json:"dflash_accept_rate"`
	DFlashBlock      *int     `json:"dflash_block"`
	// Speculative internals. An acceptance rate over three windows and one over
	// five thousand are not the same evidence, and the rate cannot tell them
	// apart on its own.
	SpeculativeWindows        *int   `json:"speculative_windows"`
	AcceptedTokens            *int   `json:"accepted_tokens"`
	RejectedTokens            *int   `json:"rejected_tokens"`
	SpeculationMethodObserved string `json:"speculation_method_observed,omitempty"`
	KVFormatObserved          string `json:"kv_format_observed,omitempty"`

	// TurnRole and TurnMaxTokens record which per-turn budget this request drew
	// on, so a truncated answer can be attributed to the ceiling that caused it.
	TurnRole      string `json:"turn_role,omitempty"`
	TurnMaxTokens int    `json:"turn_max_tokens,omitempty"`
	// FinishReason is the engine's own stop reason. A completion that hit its
	// ceiling and one that stopped naturally are different results, and the
	// one-shot campaign turned on exactly that distinction.
	FinishReason string `json:"finish_reason,omitempty"`

	// Experiment groups this row into a sweep. Metadata, never logic.
	ExperimentFamily   string `json:"experiment_family,omitempty"`
	ExperimentVariant  string `json:"experiment_variant,omitempty"`
	ExperimentBaseline string `json:"experiment_baseline,omitempty"`
	// KnobFingerprint is a stable summary of every set tuning axis. Two rows
	// with the same fingerprint asked the server for the same thing.
	KnobFingerprint string `json:"knob_fingerprint,omitempty"`
	ReplicaID       string `json:"replica_id,omitempty"`
	GPU             string `json:"gpu,omitempty"`

	// ResolvedModel is what the server said it actually loaded, which is not
	// always what was requested. The `fast` alias resolving to a different
	// artifact has already cost a campaign.
	ResolvedModel string `json:"resolved_model,omitempty"`

	// Thermal classifies the measurement. Cold, first-capture and steady-state
	// rows are never aggregated together.
	Thermal string `json:"thermal"`

	// --- and was it any good ---
	// Scored is false for a replay turn that carries no quality verdict. A nil
	// Quality on an unscored turn means "not judged"; on a scored turn it would
	// mean the scorer never ran, which is a different thing, so the reporter is
	// told which it is rather than guessing.
	Scored          bool            `json:"scored"`
	OutputSHA256    string          `json:"output_sha256"`
	OutputBytes     int             `json:"output_bytes"`
	TransportStatus TransportStatus `json:"transport_status"`
	HTTPStatus      *int            `json:"http_status"`
	Error           *string         `json:"error"`
	Quality         *Quality        `json:"quality"`

	// Artifacts are run-relative paths to everything needed to reproduce the
	// verdict: prompt, raw output, patch, test logs.
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

// Writer appends records to a JSONL file. Safe for concurrent use, which the
// reviewer fan-out in v0.3 will need.
type Writer struct {
	mu  sync.Mutex
	f   *os.File
	bw  *bufio.Writer
	enc *json.Encoder
}

func NewWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	return &Writer{f: f, bw: bw, enc: enc}, nil
}

func (w *Writer) Write(r *Record) error {
	r.Schema = SchemaVersion
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(r); err != nil {
		return err
	}
	return w.bw.Flush()
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// ReadRecords loads a request.jsonl. It refuses rows from a different schema
// version rather than silently coercing them.
func ReadRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if r.Schema != SchemaVersion {
			return nil, fmt.Errorf("%s:%d: schema %q is not %q; rows are not comparable",
				path, line, r.Schema, SchemaVersion)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Ptr is a helper for the nullable telemetry fields.
func Ptr[T any](v T) *T { return &v }
