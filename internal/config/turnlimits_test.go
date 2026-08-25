package config

import "testing"

// A repair is a delta and does not need an initial implementation's budget. A
// single global ceiling on every turn invites the length pathology the one-shot
// campaign measured.
func TestTurnLimitsByRole(t *testing.T) {
	l := TurnLimits{Initial: 8192, Repair: 4096, Final: 2048}
	const maxTurns = 6
	for turn, want := range map[int]int{0: 8192, 1: 4096, 4: 4096, 5: 2048} {
		if got := l.For(turn, maxTurns); got != want {
			t.Fatalf("turn %d: got %d, want %d", turn, got, want)
		}
	}
	for turn, want := range map[int]string{0: "initial", 3: "repair", 5: "final"} {
		if got := l.Role(turn, maxTurns); got != want {
			t.Fatalf("turn %d: role %q, want %q", turn, got, want)
		}
	}
}

// An unset lane keeps working: zero means "use the engine's max_tokens".
func TestUnsetTurnLimitsFallThrough(t *testing.T) {
	var l TurnLimits
	for turn := 0; turn < 4; turn++ {
		if got := l.For(turn, 4); got != 0 {
			t.Fatalf("turn %d: got %d, want 0 (engine default)", turn, got)
		}
	}
}

// A lane with no final budget must fall back to repair rather than to the
// engine default, which would silently give the last turn the largest ceiling.
func TestFinalFallsBackToRepairNotToTheEngineDefault(t *testing.T) {
	l := TurnLimits{Initial: 8192, Repair: 4096}
	if got := l.For(5, 6); got != 4096 {
		t.Fatalf("got %d, want the repair budget", got)
	}
}

// A two-turn lane has an initial turn and a final turn and no repair turns.
func TestShortLaneHasNoRepairTurn(t *testing.T) {
	l := TurnLimits{Initial: 8192, Repair: 4096, Final: 2048}
	if got := l.For(0, 2); got != 8192 {
		t.Fatalf("turn 0: %d", got)
	}
	if got := l.For(1, 2); got != 2048 {
		t.Fatalf("turn 1 of 2 should be final, got %d", got)
	}
}
