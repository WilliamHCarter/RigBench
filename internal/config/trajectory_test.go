package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTrajectory(t *testing.T, body string) *Fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "t.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Fixture{ID: "f", TrajectoryFile: "t.json", dir: dir}
}

const goodTrajectory = `{
  "schema": "agentbench/trajectory/v1",
  "id": "x", "lane": "builder", "note": "", "result_provenance": "",
  "scored_turn": 1,
  "turns": [
    {"index": 0, "objective": "o0", "replay_assistant": "a0", "replay_result": "r0"},
    {"index": 1, "objective": "o1", "replay_assistant": "", "replay_result": ""}
  ]
}`

func TestLoadTrajectoryAcceptsAWellFormedReplay(t *testing.T) {
	tr, err := writeTrajectory(t, goodTrajectory).LoadTrajectory()
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Turns) != 2 || tr.ScoredTurn != 1 {
		t.Fatalf("got %+v", tr)
	}
}

// Turn order is the append order. An out-of-order index would silently produce
// a prompt whose turns do not follow one another.
func TestLoadTrajectoryRefusesAMisorderedIndex(t *testing.T) {
	body := strings.Replace(goodTrajectory, `{"index": 1, "objective": "o1"`, `{"index": 5, "objective": "o1"`, 1)
	_, err := writeTrajectory(t, body).LoadTrajectory()
	if err == nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("got %v", err)
	}
}

// A replayed result on the final turn is appended to nothing: it would be
// recorded in the fixture and never sent, which is a fixture that lies about
// what the model saw.
func TestLoadTrajectoryRefusesATrailingResult(t *testing.T) {
	body := strings.Replace(goodTrajectory,
		`{"index": 1, "objective": "o1", "replay_assistant": "", "replay_result": ""}`,
		`{"index": 1, "objective": "o1", "replay_assistant": "", "replay_result": "orphan"}`, 1)
	_, err := writeTrajectory(t, body).LoadTrajectory()
	if err == nil || !strings.Contains(err.Error(), "final turn") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTrajectoryRefusesAnOutOfRangeScoredTurn(t *testing.T) {
	body := strings.Replace(goodTrajectory, `"scored_turn": 1`, `"scored_turn": 9`, 1)
	_, err := writeTrajectory(t, body).LoadTrajectory()
	if err == nil || !strings.Contains(err.Error(), "scored_turn") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadTrajectoryRefusesAnEmptyObjective(t *testing.T) {
	body := strings.Replace(goodTrajectory, `"objective": "o1"`, `"objective": "   "`, 1)
	_, err := writeTrajectory(t, body).LoadTrajectory()
	if err == nil || !strings.Contains(err.Error(), "objective") {
		t.Fatalf("got %v", err)
	}
}

// The real fixture must load and must satisfy every rule above.
func TestTheShippedTrajectoryLoads(t *testing.T) {
	f, err := LoadFixture("../../fixtures/zig-playback-v1")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := f.LoadTrajectory()
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Turns) != 4 {
		t.Fatalf("want 4 turns, got %d", len(tr.Turns))
	}
	if tr.ResultProvenance == "" {
		t.Fatal("the trajectory does not say where its replayed tool output came from")
	}
	for i, turn := range tr.Turns[:len(tr.Turns)-1] {
		if !strings.Contains(turn.ReplayResult, "TEST_RESULT") {
			t.Fatalf("turn %d's replayed result is not labelled as a test result", i)
		}
	}
}

// A frozen fixture may hold no host-specific bytes: an absolute path would
// change the prompt hash on every machine and destroy prefix reuse.
func TestTheShippedTrajectoryHoldsNoHostPaths(t *testing.T) {
	f, _ := LoadFixture("../../fixtures/zig-playback-v1")
	tr, err := f.LoadTrajectory()
	if err != nil {
		t.Fatal(err)
	}
	for i, turn := range tr.Turns {
		for _, needle := range []string{"/Users/", "/home/", "/private/", "/var/folders"} {
			if strings.Contains(turn.ReplayResult, needle) {
				t.Fatalf("turn %d's replayed result contains a host path %q", i, needle)
			}
		}
	}
}
