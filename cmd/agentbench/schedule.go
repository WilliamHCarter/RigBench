package main

import "fmt"

// Run scheduling.
//
// The engine loop is OUTER and the repetition loop is INNER. That is not a
// stylistic choice, it is the correctness property:
//
//	for each engine:          for each repetition:
//	  prepare once              for each engine:            <-- WRONG
//	  warm once                   measure
//	  measure * repeats
//
// With the loops the other way round, preparing engine B leaves the server in
// B's state, and every later repetition labelled A is measured against B. A
// two-engine `-repeats 3` A/B would silently record four correct rows and two
// mislabelled ones.
//
// The warmup also belongs outside the repetition loop. Priming immediately
// before every measurement does not measure "resident engine, hot execution";
// it measures "identical request replayed against a fully populated prefix
// cache", which is a different and much rosier thing.

type stepKind int

const (
	stepPrepare stepKind = iota
	stepWarmup
	stepMeasure
)

func (k stepKind) String() string {
	switch k {
	case stepPrepare:
		return "prepare"
	case stepWarmup:
		return "warmup"
	case stepMeasure:
		return "measure"
	}
	return "unknown"
}

// step is one action in a run's schedule.
type step struct {
	Kind   stepKind
	Engine int
	// Rep is the measurement repetition this step belongs to, 0-based.
	// -1 for preparation and warmup that serve every repetition.
	Rep int
	// PrepareOrdinal counts preparations of this engine so far, so repeated
	// preparation writes distinct artifacts instead of overwriting them.
	PrepareOrdinal int
}

type scheduleParams struct {
	Engines int
	Repeats int
	Warmups int
	// PreparePerRepetition re-prepares before every measurement. A cold
	// measurement is only cold if the server was actually returned to a cold
	// state, so repeated cold measurements each need their own preparation.
	PreparePerRepetition bool
}

// schedule lays out preparation, warmup and measurement in execution order.
func schedule(p scheduleParams) []step {
	var out []step
	for e := 0; e < p.Engines; e++ {
		if p.PreparePerRepetition {
			for rep := 0; rep < p.Repeats; rep++ {
				out = append(out, step{Kind: stepPrepare, Engine: e, Rep: rep, PrepareOrdinal: rep})
				// No warmup: priming a server we just cold-started would make
				// the measurement that follows something other than cold.
				out = append(out, step{Kind: stepMeasure, Engine: e, Rep: rep})
			}
			continue
		}
		out = append(out, step{Kind: stepPrepare, Engine: e, Rep: -1})
		for w := 0; w < p.Warmups; w++ {
			out = append(out, step{Kind: stepWarmup, Engine: e, Rep: -1})
		}
		for rep := 0; rep < p.Repeats; rep++ {
			out = append(out, step{Kind: stepMeasure, Engine: e, Rep: rep})
		}
	}
	return out
}

// checkSchedule verifies the property the topology exists to guarantee: every
// measurement is preceded by a preparation of its own engine, with no other
// engine's preparation in between.
//
// Exported as a check rather than left to inspection because this is exactly
// the invariant a plausible-looking loop rewrite breaks, and breaking it
// produces mislabelled rows rather than an error.
func checkSchedule(steps []step) error {
	lastPrepared := -1
	for i, s := range steps {
		switch s.Kind {
		case stepPrepare:
			lastPrepared = s.Engine
		case stepWarmup, stepMeasure:
			if lastPrepared != s.Engine {
				return fmt.Errorf("step %d is a %s of engine %d, but the most recently "+
					"prepared engine is %d; the row would be labelled %d and measured "+
					"against %d", i, s.Kind, s.Engine, lastPrepared, s.Engine, lastPrepared)
			}
		}
	}
	return nil
}
