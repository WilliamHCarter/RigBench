package config

import (
	"os"
	"strings"
	"testing"
)

func fixture() *Fixture {
	return &Fixture{
		OwnedFiles: []string{
			"src/voice/engine.zig",
			"src/voice/range.zig",
		},
		ForbiddenPaths: []string{
			"build.zig",
			"products/**",
			"hidden/**",
		},
	}
}

func TestOwnsAllowsListedFiles(t *testing.T) {
	f := fixture()
	if !f.Owns("src/voice/engine.zig") {
		t.Fatal("an owned file was refused")
	}
	if f.Owns("src/voice/other.zig") {
		t.Fatal("an unlisted file was allowed")
	}
}

func TestOwnsRefusesDirectoryPrefixes(t *testing.T) {
	f := fixture()
	for _, p := range []string{"products/player/golden.zig", "hidden/root.zig", "build.zig"} {
		if f.Owns(p) {
			t.Fatalf("%s was allowed", p)
		}
	}
}

// The two lists must not be able to disagree in the permissive direction: a
// forbidden path stays forbidden even if somebody adds it to the allowlist.
func TestForbiddenBeatsOwned(t *testing.T) {
	f := fixture()
	f.OwnedFiles = append(f.OwnedFiles, "products/player/golden.zig")
	if f.Owns("products/player/golden.zig") {
		t.Fatal("an allowlist entry overrode a forbidden path")
	}
}

func TestOwnsIsNotPrefixMatchingByAccident(t *testing.T) {
	f := fixture()
	// "src/voice/engine.zig" must not admit "src/voice/engine.zig.bak".
	if f.Owns("src/voice/engine.zig.bak") {
		t.Fatal("an exact-path entry matched a longer path")
	}
}

// --- thinking-off mechanism ----------------------------------------------

func engineJSON(extra string) string {
	return `{
  "schema": "agentbench/engine/v1",
  "name": "e", "endpoint": "http://x/v1", "model": "m",
  "telemetry_adapter": "hipfire",
  "sampling": {"temperature": 0, "max_tokens": 8, ` + extra + `}
}`
}

func loadEngineString(t *testing.T, body string) (*Engine, error) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/e.json"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadEngine(path)
}

// The P0: a config may not claim "off" without saying how off is expressed.
// Omitting a reasoning field is not the same as disabling reasoning.
func TestThinkingOffRequiresAMechanism(t *testing.T) {
	_, err := loadEngineString(t, engineJSON(`"thinking": "off"`))
	if err == nil {
		t.Fatal("a config claiming no-think with no mechanism was accepted")
	}
	if !strings.Contains(err.Error(), "not the same as disabling reasoning") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestThinkingOffAcceptsEachDeclaredMechanism(t *testing.T) {
	for _, m := range []ThinkingOffMechanism{
		ThinkingOffOmit, ThinkingOffChatTemplate, ThinkingOffReasoningEffortNone,
	} {
		e, err := loadEngineString(t, engineJSON(
			`"thinking": "off", "thinking_off_mechanism": "`+string(m)+`"`))
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if e.Sampling.ThinkingOffMechanism != m {
			t.Fatalf("%s round-tripped as %q", m, e.Sampling.ThinkingOffMechanism)
		}
	}
}

func TestThinkingOffMechanismOnAThinkingConfigIsRefused(t *testing.T) {
	_, err := loadEngineString(t, engineJSON(
		`"thinking": "xhigh", "thinking_off_mechanism": "chat_template_kwargs"`))
	if err == nil {
		t.Fatal("a no-think mechanism was accepted alongside a reasoning budget")
	}
}

func TestUnknownThinkingOffMechanismIsRefused(t *testing.T) {
	_, err := loadEngineString(t, engineJSON(
		`"thinking": "off", "thinking_off_mechanism": "hope"`))
	if err == nil {
		t.Fatal("an unknown mechanism was accepted")
	}
}

// extra_body must not be able to overwrite a field the client already sends:
// the recorded request would then be a fiction.
func TestExtraBodyCannotOverrideAReservedKey(t *testing.T) {
	body := `{
  "schema": "agentbench/engine/v1",
  "name": "e", "endpoint": "http://x/v1", "model": "m",
  "telemetry_adapter": "generic",
  "extra_body": {"temperature": 0.9},
  "sampling": {"temperature": 0, "max_tokens": 8, "thinking": "off",
               "thinking_off_mechanism": "omit"}
}`
	_, err := loadEngineString(t, body)
	if err == nil || !strings.Contains(err.Error(), "extra_body") {
		t.Fatalf("got %v", err)
	}
}

// --- the shipped rig configs ---------------------------------------------

// The rig configs must be executable claims, not decorative ones. Each of these
// would have been true of the configs that shipped with v0.2 and wrong.
func TestShippedRigConfigsAreAttestable(t *testing.T) {
	for _, path := range []string{
		"../../configs/engines/ar.json",
		"../../configs/engines/dflash2.json",
	} {
		e, err := LoadEngine(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if e.IdentityProbe == nil {
			t.Errorf("%s declares a process-level engine state with no identity_probe; "+
				"its label would be an unchecked assertion", path)
		}
		if e.TelemetryAdapter != "hipfire" {
			t.Errorf("%s uses adapter %q; the same backend should get the same "+
				"instrumentation on both sides of an A/B", path, e.TelemetryAdapter)
		}
		if e.Sampling.Thinking == "off" &&
			e.Sampling.ThinkingOffMechanism == ThinkingOffOmit {
			t.Errorf("%s claims no-think by omission against a server whose default "+
				"is to reason", path)
		}
	}
}

// Speculation mechanism and drafter architecture are different things, and
// conflating them would make an F16/MQ6/MQ4 drafter sweep unnameable.
func TestSpeculationModeNamesTheMechanismNotTheDrafter(t *testing.T) {
	e, err := LoadEngine("../../configs/engines/dflash2.json")
	if err != nil {
		t.Fatal(err)
	}
	if e.SpeculationMode != "dflash" {
		t.Errorf("speculation_mode = %q; it names the engine's mechanism, "+
			"while the drafter belongs in non_default_knobs", e.SpeculationMode)
	}
	if e.NonDefaultKnobs["draft_arch"] != "DFlash2" {
		t.Errorf("draft_arch = %q, want DFlash2", e.NonDefaultKnobs["draft_arch"])
	}
}
