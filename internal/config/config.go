// Package config loads the benchmark's declarative inputs: fixture manifests,
// engine configurations, prompt layouts and context packs.
//
// Nothing here knows about a specific inference engine. An engine config is a
// URL, a model alias, sampling parameters, and a named telemetry adapter; the
// runner must work against any OpenAI-compatible endpoint with the adapter set
// to "generic".
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// --- fixture -------------------------------------------------------------

type Fixture struct {
	Schema   string `json:"schema"`
	ID       string `json:"id"`
	Version  string `json:"version"`
	Language string `json:"language"`

	// Paths, all relative to the fixture directory.
	RepoDir           string `json:"repo_dir"`
	HiddenDir         string `json:"hidden_dir"`
	ReferenceDir      string `json:"reference_dir"`
	MutantsFile       string `json:"mutants_file"`
	StoryFile         string `json:"story_file"`
	DoctrineFile      string `json:"doctrine_file"`
	AgentContractFile string `json:"agent_contract_file"`
	ObjectiveFile     string `json:"objective_file"`
	TrajectoryFile    string `json:"trajectory_file"`

	// Ownership. OwnedFiles is the allowlist a candidate patch may touch;
	// ForbiddenPaths is called out separately so a scope violation can name the
	// rule it broke rather than only "not in the allowlist".
	OwnedFiles     []string `json:"owned_files"`
	ForbiddenPaths []string `json:"forbidden_paths"`

	// Toolchain is the exact compiler this fixture's goldens, mutant controls
	// and hidden suite were verified under. Not a minimum: a frozen fixture's
	// bytes are only meaningful under the compiler that produced them, and two
	// runs on different compilers are not comparable however similar the
	// results look.
	Toolchain FixtureToolchain `json:"toolchain"`

	ContextPacks  map[string]string `json:"context_packs"`
	Commands      Commands          `json:"commands"`
	Limits        Limits            `json:"limits"`
	RequiredGates []string          `json:"required_gates"`

	// Unmeasured records criteria the fixture deliberately does not check, so
	// they appear in the report as gaps rather than as silent greens.
	Unmeasured []string `json:"unmeasured"`

	dir string
}

type FixtureToolchain struct {
	Zig string `json:"zig"`
}

type Commands struct {
	Build   []string `json:"build"`
	Visible []string `json:"visible"`
	Hidden  []string `json:"hidden"`
	Release []string `json:"release"`
}

type Limits struct {
	BuilderMaxTokens      int `json:"builder_max_tokens"`
	TimeoutSeconds        int `json:"timeout_seconds"`
	CommandTimeoutSeconds int `json:"command_timeout_seconds"`
}

func (f *Fixture) Dir() string          { return f.dir }
func (f *Fixture) Path(p string) string { return filepath.Join(f.dir, p) }
func (f *Fixture) Timeout() time.Duration {
	return time.Duration(f.Limits.TimeoutSeconds) * time.Second
}
func (f *Fixture) CommandTimeout() time.Duration {
	if f.Limits.CommandTimeoutSeconds == 0 {
		return 10 * time.Minute
	}
	return time.Duration(f.Limits.CommandTimeoutSeconds) * time.Second
}

// Owns reports whether a repo-relative path may be changed by a candidate
// patch. A forbidden path is refused even if an allowlist entry would match it,
// so the two lists cannot disagree in the permissive direction.
func (f *Fixture) Owns(path string) bool {
	path = filepath.ToSlash(path)
	for _, bad := range f.ForbiddenPaths {
		if matchPath(bad, path) {
			return false
		}
	}
	for _, ok := range f.OwnedFiles {
		if matchPath(ok, path) {
			return true
		}
	}
	return false
}

// matchPath supports an exact path or a trailing "/**" directory prefix. Kept
// deliberately small: a glob dialect nobody can predict is worse than a rule
// everybody can read.
func matchPath(pattern, path string) bool {
	pattern = filepath.ToSlash(pattern)
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "**"))
	}
	return pattern == path
}

func LoadFixture(dir string) (*Fixture, error) {
	var f Fixture
	if err := loadJSON(filepath.Join(dir, "fixture.json"), &f); err != nil {
		return nil, err
	}
	if f.Schema != "agentbench/fixture/v1" {
		return nil, fmt.Errorf("fixture %s: unsupported schema %q", dir, f.Schema)
	}
	f.dir = dir
	sort.Strings(f.OwnedFiles)
	sort.Strings(f.ForbiddenPaths)
	return &f, nil
}

// --- context pack --------------------------------------------------------

type ContextPack struct {
	Schema string `json:"schema"`
	ID     string `json:"id"`
	// Files are repo-relative and are sorted at load time; the serializer sorts
	// again, so the order in the file is documentation rather than contract.
	Files []string `json:"files"`
	// SizeLabelVerified must be true before this pack may be described by a
	// token-count name such as "8K". Until the exact target tokenizer is wired
	// (v0.5) it stays false and the pack keeps a neutral id.
	SizeLabelVerified bool   `json:"size_label_verified"`
	Notes             string `json:"notes,omitempty"`
}

func (f *Fixture) LoadContextPack(name string) (*ContextPack, error) {
	rel, ok := f.ContextPacks[name]
	if !ok {
		return nil, fmt.Errorf("fixture %s has no context pack %q", f.ID, name)
	}
	var p ContextPack
	if err := loadJSON(f.Path(rel), &p); err != nil {
		return nil, err
	}
	sort.Strings(p.Files)
	return &p, nil
}

// --- trajectory ----------------------------------------------------------

// Trajectory is a deterministic multi-turn replay.
//
// The assistant messages and tool results here are fixture bytes and are
// appended regardless of what the model under test actually said. That is not a
// shortcut: it is the only way turn N's prompt can be a byte-exact prefix of
// turn N+1's, which is the property that makes prefix-reuse telemetry
// interpretable. v0.4 replaces the replay with a live edit/test loop, and that
// is a different lane with a different determinism story.
type Trajectory struct {
	Schema string `json:"schema"`
	ID     string `json:"id"`
	Lane   string `json:"lane"`
	Note   string `json:"note"`
	// ResultProvenance records where the replayed tool output came from. A
	// fabricated TEST_RESULT would make the context realistic-looking and the
	// benchmark dishonest, so the fixture says which real run produced each.
	ResultProvenance string `json:"result_provenance"`
	// ScoredTurn is the turn whose model output goes through the builder gates.
	// Earlier turns are context-growth measurements and carry no quality verdict.
	ScoredTurn int    `json:"scored_turn"`
	Turns      []Turn `json:"turns"`
}

type Turn struct {
	Index     int    `json:"index"`
	Objective string `json:"objective"`
	// ReplayAssistant and ReplayResult are appended to the volatile tail after
	// this turn. Empty on the final turn, which has no successor.
	ReplayAssistant string `json:"replay_assistant"`
	ReplayResult    string `json:"replay_result"`
}

func (f *Fixture) LoadTrajectory() (*Trajectory, error) {
	if f.TrajectoryFile == "" {
		return nil, fmt.Errorf("fixture %s declares no trajectory", f.ID)
	}
	var t Trajectory
	if err := loadJSON(f.Path(f.TrajectoryFile), &t); err != nil {
		return nil, err
	}
	if t.Schema != "agentbench/trajectory/v1" {
		return nil, fmt.Errorf("trajectory %s: unsupported schema %q", f.TrajectoryFile, t.Schema)
	}
	if len(t.Turns) == 0 {
		return nil, fmt.Errorf("trajectory %s has no turns", t.ID)
	}
	for i, turn := range t.Turns {
		if turn.Index != i {
			return nil, fmt.Errorf("trajectory %s: turn %d declares index %d; "+
				"turn order is the append order and must not be implicit", t.ID, i, turn.Index)
		}
		if strings.TrimSpace(turn.Objective) == "" {
			return nil, fmt.Errorf("trajectory %s: turn %d has no objective", t.ID, i)
		}
	}
	last := len(t.Turns) - 1
	if strings.TrimSpace(t.Turns[last].ReplayResult) != "" {
		return nil, fmt.Errorf("trajectory %s: the final turn has a replayed result, "+
			"which nothing consumes; it would be recorded and never sent", t.ID)
	}
	if t.ScoredTurn < 0 || t.ScoredTurn > last {
		return nil, fmt.Errorf("trajectory %s: scored_turn %d is outside 0..%d",
			t.ID, t.ScoredTurn, last)
	}
	return &t, nil
}

// --- engine --------------------------------------------------------------

type Engine struct {
	Schema string `json:"schema"`
	// Name is the row label in every report. Two configs that differ in any
	// measured knob must not share a name.
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	Endpoint  string `json:"endpoint"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	Model     string `json:"model"`

	// Recorded identity. Empty means the engine did not tell us, never zero.
	EngineCommit    string `json:"engine_commit,omitempty"`
	ModelHash       string `json:"model_hash,omitempty"`
	DraftHash       string `json:"draft_hash,omitempty"`
	TargetQuant     string `json:"target_quant,omitempty"`
	KVMode          string `json:"kv_mode,omitempty"`
	SpeculationMode string `json:"speculation_mode,omitempty"`

	// TelemetryAdapter names the optional, engine-specific extractor for fields
	// like speculation acceptance. "generic" leaves every such field null.
	TelemetryAdapter string `json:"telemetry_adapter"`

	// Headers are sent verbatim with every request. This is where an endpoint
	// that needs a routing or profile header declares it, so the runner stays
	// free of endpoint-specific knowledge.
	Headers map[string]string `json:"headers,omitempty"`

	// ExtraBody is merged into the request body verbatim. It is the escape
	// hatch for a backend-specific field the portable schema does not model.
	// Keys that collide with a field the client already sets are refused at
	// load time rather than silently winning.
	ExtraBody map[string]any `json:"extra_body,omitempty"`

	// IdentityProbe optionally verifies that the server is actually running the
	// configuration this file claims. Without it, `speculation_mode` and friends
	// are metadata the benchmark asserts and has not checked -- and a row
	// labelled `ar` against a daemon left in speculation mode is worse than no
	// row at all.
	IdentityProbe *IdentityProbe `json:"identity_probe,omitempty"`

	Sampling Sampling `json:"sampling"`

	NonDefaultKnobs map[string]string `json:"non_default_knobs,omitempty"`
}

// IdentityProbe is a GET against the endpoint host whose JSON response is
// stored as a run artifact and, optionally, asserted against.
type IdentityProbe struct {
	// Path is resolved against the endpoint's scheme and host, not against the
	// endpoint path: a health route usually sits outside /v1.
	Path string `json:"path"`
	// Require maps a dotted JSON path to the value it must have. A mismatch
	// aborts the run; there is no warn-and-continue, because the whole point is
	// to stop a mislabelled row from being recorded.
	Require map[string]string `json:"require,omitempty"`
	// Note is documentation for whoever fills Require in. It is not read.
	Note string `json:"note,omitempty"`
	// Record maps a dotted JSON path to an engine-identity field name
	// ("engine_commit", "model_hash", "draft_hash"). Values found there are
	// recorded in run.json, which is how reproducibility identity stops being
	// something a human has to remember to paste in.
	Record map[string]string `json:"record,omitempty"`
}

// ThinkingOffMechanism names how "no thinking" is actually requested.
//
// This exists because omitting a reasoning parameter is *not* the same as
// disabling reasoning, and a config that says "off" while the model still
// reasons contaminates both the timing and the derived decode rate. The
// mechanism must be stated, the same way the reasoning budget itself must be.
type ThinkingOffMechanism string

const (
	// ThinkingOffOmit sends no reasoning field at all. Correct only for an
	// endpoint whose default is genuinely non-reasoning -- say so deliberately.
	ThinkingOffOmit ThinkingOffMechanism = "omit"
	// ThinkingOffChatTemplate sends chat_template_kwargs.enable_thinking=false,
	// which is the hard per-request no-think switch on Hipfire and vLLM-style
	// servers.
	ThinkingOffChatTemplate ThinkingOffMechanism = "chat_template_kwargs"
	// ThinkingOffReasoningEffortNone sends reasoning_effort="none".
	ThinkingOffReasoningEffortNone ThinkingOffMechanism = "reasoning_effort_none"
)

type Sampling struct {
	Temperature float64  `json:"temperature"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   int      `json:"max_tokens"`
	Seed        *int     `json:"seed,omitempty"`
	// Thinking is recorded explicitly. Two runs with different reasoning
	// budgets are different variants and the reporter refuses to merge them.
	Thinking string `json:"thinking"`
	// ThinkingOffMechanism is required whenever Thinking is "off". See the type:
	// omitting a reasoning parameter is not the same as disabling reasoning.
	ThinkingOffMechanism ThinkingOffMechanism `json:"thinking_off_mechanism,omitempty"`
	ThinkingBudgetTokens *int                 `json:"thinking_budget_tokens,omitempty"`
}

func LoadEngine(path string) (*Engine, error) {
	var e Engine
	if err := loadJSON(path, &e); err != nil {
		return nil, err
	}
	if e.Schema != "agentbench/engine/v1" {
		return nil, fmt.Errorf("engine %s: unsupported schema %q", path, e.Schema)
	}
	if e.TelemetryAdapter == "" {
		e.TelemetryAdapter = "generic"
	}
	if e.Name == "" || e.Endpoint == "" || e.Model == "" {
		return nil, fmt.Errorf("engine %s: name, endpoint and model are all required", path)
	}
	if e.Sampling.Thinking == "" {
		return nil, fmt.Errorf("engine %s: sampling.thinking must be stated explicitly, "+
			"even if it is \"off\"; an unrecorded reasoning budget makes runs incomparable", path)
	}
	if e.Sampling.Thinking == "off" {
		switch e.Sampling.ThinkingOffMechanism {
		case ThinkingOffOmit, ThinkingOffChatTemplate, ThinkingOffReasoningEffortNone:
		case "":
			return nil, fmt.Errorf("engine %s: sampling.thinking is \"off\" but "+
				"sampling.thinking_off_mechanism is not set. Omitting a reasoning "+
				"parameter is not the same as disabling reasoning: state %q, %q or %q "+
				"so the claim is something the request actually makes",
				path, ThinkingOffChatTemplate, ThinkingOffReasoningEffortNone, ThinkingOffOmit)
		default:
			return nil, fmt.Errorf("engine %s: unknown thinking_off_mechanism %q",
				path, e.Sampling.ThinkingOffMechanism)
		}
	} else if e.Sampling.ThinkingOffMechanism != "" {
		return nil, fmt.Errorf("engine %s: thinking_off_mechanism is set but thinking is %q, "+
			"not \"off\"", path, e.Sampling.Thinking)
	}
	for k := range e.ExtraBody {
		if reservedBodyKeys[k] {
			return nil, fmt.Errorf("engine %s: extra_body may not set %q, which the client "+
				"already sends; a silent override would make the recorded request a fiction",
				path, k)
		}
	}
	if p := e.IdentityProbe; p != nil && p.Path == "" {
		return nil, fmt.Errorf("engine %s: identity_probe needs a path", path)
	}
	return &e, nil
}

// reservedBodyKeys are the request fields the client owns. ExtraBody may not
// collide with them.
var reservedBodyKeys = map[string]bool{
	"model": true, "messages": true, "temperature": true, "max_tokens": true,
	"stream": true, "stream_options": true, "seed": true, "top_p": true,
	"tools": true, "reasoning_effort": true, "chat_template_kwargs": true,
	"max_reasoning_tokens": true,
}

// --- layout --------------------------------------------------------------

type Layout struct {
	Schema      string `json:"schema"`
	ID          string `json:"id"`
	Lane        string `json:"lane"`
	Description string `json:"description,omitempty"`

	EnforceStabilityOrder bool   `json:"enforce_stability_order"`
	Coalesce              string `json:"coalesce"`

	Blocks []LayoutBlock `json:"blocks"`
}

type LayoutBlock struct {
	// Source names the content this slot draws from. The runner resolves it;
	// an unknown source is an error, so a layout cannot silently omit content.
	Source    string `json:"source"`
	Stability string `json:"stability"`
	Role      string `json:"role"`
	// Optional marks a slot that may resolve to nothing (a lane with no tool
	// contracts, say). A non-optional slot resolving to nothing is an error.
	Optional bool `json:"optional,omitempty"`
}

func LoadLayout(path string) (*Layout, error) {
	var l Layout
	if err := loadJSON(path, &l); err != nil {
		return nil, err
	}
	if l.Schema != "agentbench/layout/v1" {
		return nil, fmt.Errorf("layout %s: unsupported schema %q", path, l.Schema)
	}
	if l.Coalesce == "" {
		l.Coalesce = "by_role_and_stability"
	}
	return &l, nil
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
