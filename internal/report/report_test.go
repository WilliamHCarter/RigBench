package report

import (
	"encoding/csv"
	"os"
	"strings"
	"testing"

	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

func rec(engine string, wall float64, passed bool, mods ...func(*metrics.Record)) metrics.Record {
	r := metrics.Record{
		Schema: metrics.SchemaVersion,
		Lane:   metrics.LaneBuilder, ContextVariant: "base", PromptLayout: "l",
		Engine: engine, Thermal: "cold", Model: "m", ThinkingMode: "off",
		PromptSHA256: "aaaa", WallMS: wall,
		Scored:  true,
		Quality: &metrics.Quality{Passed: passed},
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

// "not exposed" and zero must never render the same. This is the single
// property that keeps a telemetry-free backend from looking like a backend
// reporting zeros.
func TestNotExposedIsDistinctFromZero(t *testing.T) {
	cells := Aggregate([]metrics.Record{
		rec("no-telemetry", 100, true),
		rec("reports-zero", 100, true, func(r *metrics.Record) {
			r.DFlashTau = metrics.Ptr(0.0)
			r.Engine = "reports-zero"
		}),
	})
	var absent, zero string
	for _, c := range cells {
		if c.Key.Engine == "no-telemetry" {
			absent = c.Tau.String()
		} else {
			zero = c.Tau.String()
		}
	}
	if absent != "not exposed" {
		t.Fatalf("absent telemetry rendered as %q", absent)
	}
	if zero != "0" {
		t.Fatalf("a reported zero rendered as %q", zero)
	}
}

func TestMedianAndSpreadOverRepetitions(t *testing.T) {
	cells := Aggregate([]metrics.Record{
		rec("e", 100, true), rec("e", 300, true), rec("e", 200, true),
	})
	if len(cells) != 1 {
		t.Fatalf("want one cell, got %d", len(cells))
	}
	s := cells[0].WallMS
	if s.Median != 200 || s.Min != 100 || s.Max != 300 || s.Spread != 200 {
		t.Fatalf("got %+v", s)
	}
	if !strings.Contains(s.String(), "(100..300)") {
		t.Fatalf("dispersion is not shown: %q", s.String())
	}
}

// The reporter must refuse to present rows with different prompt bytes as a
// comparison, and must say so rather than silently picking one.
func TestMixedPromptHashesAreFlaggedIncomparable(t *testing.T) {
	cells := Aggregate([]metrics.Record{
		rec("e", 100, true),
		rec("e", 100, true, func(r *metrics.Record) { r.PromptSHA256 = "bbbb" }),
	})
	if len(cells[0].Incomparable) == 0 {
		t.Fatal("two prompt hashes in one cell were not flagged")
	}
	if cells[0].PromptSHA != "" {
		t.Fatal("a mixed cell reported a single prompt hash")
	}
}

func TestMixedReasoningBudgetsAreFlagged(t *testing.T) {
	cells := Aggregate([]metrics.Record{
		rec("e", 100, true),
		rec("e", 100, true, func(r *metrics.Record) { r.ThinkingMode = "high" }),
	})
	found := false
	for _, s := range cells[0].Incomparable {
		if strings.Contains(s, "reasoning budget") {
			found = true
		}
	}
	if !found {
		t.Fatalf("got %v", cells[0].Incomparable)
	}
}

// Cold and steady rows are different cells, never one average.
func TestThermalClassesDoNotAggregateTogether(t *testing.T) {
	cells := Aggregate([]metrics.Record{
		rec("e", 100, true),
		rec("e", 100, true, func(r *metrics.Record) { r.Thermal = "steady" }),
	})
	if len(cells) != 2 {
		t.Fatalf("want two cells, got %d", len(cells))
	}
}

func TestFailedGatesAreCountedByName(t *testing.T) {
	cells := Aggregate([]metrics.Record{
		rec("e", 100, false, func(r *metrics.Record) {
			r.Quality = &metrics.Quality{Passed: false, Gates: []metrics.Gate{
				{Name: "hidden_tests", Result: metrics.GateFail},
				{Name: "build", Result: metrics.GatePass},
			}}
		}),
	})
	if cells[0].FailedGateCounts["hidden_tests=fail"] != 1 {
		t.Fatalf("got %v", cells[0].FailedGateCounts)
	}
	if _, ok := cells[0].FailedGateCounts["build=pass"]; ok {
		t.Fatal("a passing gate was counted as a failure")
	}
}

func TestCSVLeavesUnexposedFieldsEmptyRatherThanZero(t *testing.T) {
	cells := Aggregate([]metrics.Record{rec("e", 100, true)})
	if got := csvStat(cells[0].Tau, "median"); got != "" {
		t.Fatalf("want an empty CSV field for unexposed telemetry, got %q", got)
	}
}

// A replay turn carries no verdict. Counting it as a failure would report a
// fully green four-turn run as one pass and three failures.
func TestUnscoredTurnsAreNotFailures(t *testing.T) {
	cells := Aggregate([]metrics.Record{
		rec("e", 100, false, func(r *metrics.Record) { r.Scored = false; r.Quality = nil; r.TurnIndex = 0 }),
		rec("e", 100, false, func(r *metrics.Record) { r.Scored = false; r.Quality = nil; r.TurnIndex = 1 }),
		rec("e", 100, true, func(r *metrics.Record) { r.TurnIndex = 2 }),
	})
	c := cells[0]
	if c.Unscored != 2 {
		t.Fatalf("unscored = %d, want 2", c.Unscored)
	}
	if c.Failed != 0 {
		t.Fatalf("failed = %d, want 0", c.Failed)
	}
	if c.Passed != 1 {
		t.Fatalf("passed = %d, want 1", c.Passed)
	}
}

// Turn 0 appends to nothing; including its zero would halve the median.
func TestAppendedBytesExcludesTurnZero(t *testing.T) {
	cells := Aggregate([]metrics.Record{
		rec("e", 100, true, func(r *metrics.Record) { r.TurnIndex = 0; r.AppendedBytes = 0 }),
		rec("e", 100, true, func(r *metrics.Record) { r.TurnIndex = 1; r.AppendedBytes = 400 }),
		rec("e", 100, true, func(r *metrics.Record) { r.TurnIndex = 2; r.AppendedBytes = 600 }),
	})
	if got := cells[0].AppendedBytes; got.N != 2 || got.Median != 500 {
		t.Fatalf("got %+v", got)
	}
}

func TestCSVRowWidthMatchesTheHeader(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/summary.csv"
	cells := Aggregate([]metrics.Record{rec("e", 100, true)})
	if err := WriteCSV(path, cells); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want a header and one row, got %d", len(rows))
	}
	if len(rows[0]) != len(Columns()) || len(rows[1]) != len(Columns()) {
		t.Fatalf("header %d, row %d, Columns() %d", len(rows[0]), len(rows[1]), len(Columns()))
	}
}
