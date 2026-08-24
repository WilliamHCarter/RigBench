package main

import (
	"fmt"
	"strings"
	"testing"
)

func render(steps []step) string {
	var b strings.Builder
	for _, s := range steps {
		if s.Rep >= 0 && s.Kind == stepMeasure {
			fmt.Fprintf(&b, "%s(e%d,rep%d) ", s.Kind, s.Engine, s.Rep)
		} else {
			fmt.Fprintf(&b, "%s(e%d) ", s.Kind, s.Engine)
		}
	}
	return strings.TrimSpace(b.String())
}

// The exact shape the campaign needs: prepare once, warm once, then the
// repetitions, engine by engine.
func TestSteadyStateScheduleIsEngineOuterRepetitionInner(t *testing.T) {
	got := render(schedule(scheduleParams{Engines: 2, Repeats: 3, Warmups: 1}))
	want := "prepare(e0) warmup(e0) measure(e0,rep0) measure(e0,rep1) measure(e0,rep2) " +
		"prepare(e1) warmup(e1) measure(e1,rep0) measure(e1,rep1) measure(e1,rep2)"
	if got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

// The regression this file exists for. Every measurement must be preceded by a
// preparation of its own engine; the bug being fixed measured two thirds of the
// AR samples against a daemon left in DFlash mode.
func TestEveryMeasurementFollowsItsOwnEnginesPreparation(t *testing.T) {
	for _, p := range []scheduleParams{
		{Engines: 1, Repeats: 1, Warmups: 0},
		{Engines: 2, Repeats: 3, Warmups: 1},
		{Engines: 3, Repeats: 5, Warmups: 2},
		{Engines: 2, Repeats: 1, Warmups: 0},
		{Engines: 2, Repeats: 3, PreparePerRepetition: true},
		{Engines: 4, Repeats: 2, PreparePerRepetition: true},
	} {
		steps := schedule(p)
		if err := checkSchedule(steps); err != nil {
			t.Fatalf("%+v: %v\n%s", p, err, render(steps))
		}
	}
}

// And the checker must actually catch the bug, or it proves nothing. This is
// the schedule the old loop produced.
func TestCheckScheduleCatchesTheRepetitionOuterBug(t *testing.T) {
	var bad []step
	for rep := 0; rep < 3; rep++ {
		for e := 0; e < 2; e++ {
			if rep == 0 {
				bad = append(bad, step{Kind: stepPrepare, Engine: e, Rep: -1})
			}
			bad = append(bad, step{Kind: stepMeasure, Engine: e, Rep: rep})
		}
	}
	err := checkSchedule(bad)
	if err == nil {
		t.Fatal("the repetition-outer schedule was accepted")
	}
	if !strings.Contains(err.Error(), "measured against") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// -warmup 1 -repeats 3 must mean one warmup, not three. A warmup before every
// measurement would turn the benchmark into "identical request replayed against
// a fully populated prefix cache".
func TestWarmupHappensOncePerEngineNotPerRepetition(t *testing.T) {
	steps := schedule(scheduleParams{Engines: 2, Repeats: 3, Warmups: 1})
	counts := map[int]int{}
	for _, s := range steps {
		if s.Kind == stepWarmup {
			counts[s.Engine]++
		}
	}
	for e, n := range counts {
		if n != 1 {
			t.Fatalf("engine %d was warmed %d times, want 1", e, n)
		}
	}
	if len(counts) != 2 {
		t.Fatalf("warmed %d engines, want 2", len(counts))
	}
}

func TestWarmupPrecedesEveryMeasurementOfItsEngine(t *testing.T) {
	steps := schedule(scheduleParams{Engines: 2, Repeats: 3, Warmups: 2})
	warmed := map[int]bool{}
	for _, s := range steps {
		switch s.Kind {
		case stepWarmup:
			warmed[s.Engine] = true
		case stepMeasure:
			if !warmed[s.Engine] {
				t.Fatalf("engine %d was measured before it was warmed:\n%s", s.Engine, render(steps))
			}
		}
	}
}

// A repeated cold measurement needs its own preparation, or only the first one
// is cold and the rest are warm rows wearing a cold label.
func TestColdRepetitionsArePreparedIndividuallyAndNeverWarmed(t *testing.T) {
	steps := schedule(scheduleParams{Engines: 2, Repeats: 3, Warmups: 1, PreparePerRepetition: true})
	prepares, measures := map[int]int{}, map[int]int{}
	for _, s := range steps {
		switch s.Kind {
		case stepPrepare:
			prepares[s.Engine]++
		case stepMeasure:
			measures[s.Engine]++
		case stepWarmup:
			t.Fatalf("a cold schedule contains a warmup:\n%s", render(steps))
		}
	}
	for e := 0; e < 2; e++ {
		if prepares[e] != measures[e] {
			t.Fatalf("engine %d: %d preparations for %d measurements",
				e, prepares[e], measures[e])
		}
	}
}

// Repeated preparation must write distinct artifacts rather than overwrite one.
func TestPrepareOrdinalIsDistinctPerPreparation(t *testing.T) {
	steps := schedule(scheduleParams{Engines: 2, Repeats: 3, PreparePerRepetition: true})
	seen := map[[2]int]bool{}
	for _, s := range steps {
		if s.Kind != stepPrepare {
			continue
		}
		key := [2]int{s.Engine, s.PrepareOrdinal}
		if seen[key] {
			t.Fatalf("engine %d prepared twice with ordinal %d", s.Engine, s.PrepareOrdinal)
		}
		seen[key] = true
	}
	if len(seen) != 6 {
		t.Fatalf("want 6 distinct preparations, got %d", len(seen))
	}
}
