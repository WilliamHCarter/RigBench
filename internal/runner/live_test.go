package runner

import (
	"testing"

	"github.com/WilliamHCarter/RigBench/internal/prompt"
)

func manifest(tokens int) *prompt.Manifest {
	return &prompt.Manifest{PromptTokensEstimated: tokens}
}

// Turn 0 shares nothing and appends everything. Reporting a turn-0 prompt as
// mostly reused would make every story look cache-friendly from the first
// request, which is exactly the number this instrumentation exists to get right.
func TestTurnZeroSharesNothing(t *testing.T) {
	shared, appended := turnReuse(nil, manifest(10000))
	if shared != 0 {
		t.Fatalf("turn 0 shared %d tokens with a turn that does not exist", shared)
	}
	if appended != 10000 {
		t.Fatalf("turn 0 appended %d, want the whole prompt", appended)
	}
}

func TestLaterTurnsShareThePreviousPromptEntirely(t *testing.T) {
	shared, appended := turnReuse(manifest(10000), manifest(11500))
	if shared != 10000 {
		t.Fatalf("shared = %d", shared)
	}
	if appended != 1500 {
		t.Fatalf("appended = %d", appended)
	}
}

// A prompt that somehow shrank must not report negative growth.
func TestShrinkingPromptDoesNotAppendNegatively(t *testing.T) {
	_, appended := turnReuse(manifest(10000), manifest(9000))
	if appended != 0 {
		t.Fatalf("appended = %d", appended)
	}
}
