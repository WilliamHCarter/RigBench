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

	// Ownership. OwnedFiles is the allowlist a candidate patch may touch;
	// ForbiddenPaths is called out separately so a scope violation can name the
	// rule it broke rather than only "not in the allowlist".
	OwnedFiles     []string `json:"owned_files"`
	ForbiddenPaths []string `json:"forbidden_paths"`

	ContextPacks  map[string]string `json:"context_packs"`
	Commands      Commands          `json:"commands"`
	Limits        Limits            `json:"limits"`
	RequiredGates []string          `json:"required_gates"`

	// Unmeasured records criteria the fixture deliberately does not check, so
	// they appear in the report as gaps rather than as silent greens.
	Unmeasured []string `json:"unmeasured"`

	dir string
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

	Sampling Sampling `json:"sampling"`

	NonDefaultKnobs map[string]string `json:"non_default_knobs,omitempty"`
}

type Sampling struct {
	Temperature float64  `json:"temperature"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   int      `json:"max_tokens"`
	Seed        *int     `json:"seed,omitempty"`
	// Thinking is recorded explicitly. Two runs with different reasoning
	// budgets are different variants and the reporter refuses to merge them.
	Thinking             string `json:"thinking"`
	ThinkingBudgetTokens *int   `json:"thinking_budget_tokens,omitempty"`
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
	return &e, nil
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
