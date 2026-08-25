package metrics

import "testing"

func rec(wall float64, mods ...func(*Record)) *Record {
	r := &Record{WallMS: wall}
	for _, m := range mods {
		m(r)
	}
	return r
}

// The separation this exists for: a story that failed must not be able to look
// good because its engine metrics did.
func TestTaskSuccessIsIndependentOfInferenceSpeed(t *testing.T) {
	fast := &Story{HiddenGreen: false, StopReason: "turn budget exhausted", ModelTurns: 6}
	fast.Rollup([]*Record{rec(100, func(r *Record) { r.DecodeTokS = Ptr(105.0) })})
	if fast.Task.Success {
		t.Fatal("a failed story reported success")
	}
	if fast.Inference.DecodeTokS == nil || *fast.Inference.DecodeTokS != 105 {
		t.Fatal("inference metrics were lost")
	}

	slow := &Story{HiddenGreen: true, StopReason: "model declared done", ModelTurns: 2}
	slow.Rollup([]*Record{rec(100, func(r *Record) { r.DecodeTokS = Ptr(31.0) })})
	if !slow.Task.Success {
		t.Fatal("a green story reported failure")
	}
}

// A backend with no telemetry must still produce a complete Task group.
func TestNoTelemetryLeavesInferenceNullAndTaskComplete(t *testing.T) {
	s := &Story{HiddenGreen: true, ModelTurns: 2, TotalWallMS: 5000, ToolWallMS: 3000}
	s.Rollup([]*Record{rec(1000), rec(1000)})
	if s.Inference.DecodeTokS != nil || s.Inference.DFlashTau != nil {
		t.Fatal("a rate was invented where none was reported")
	}
	if !s.Task.Success || s.Task.TotalStoryWallMS != 5000 || s.Task.ToolWallMS != 3000 {
		t.Fatalf("task group incomplete: %+v", s.Task)
	}
	if s.Inference.ModelWallMS != 2000 {
		t.Fatalf("model wall = %v", s.Inference.ModelWallMS)
	}
}

// Rates are medians, counts are totals: a rate averaged over turns of very
// different lengths would be dominated by the shortest.
func TestRatesAreMediansAndCountsAreTotals(t *testing.T) {
	s := &Story{}
	s.Rollup([]*Record{
		rec(10, func(r *Record) { r.DecodeTokS = Ptr(10.0); r.CompletionTokens = Ptr(100) }),
		rec(10, func(r *Record) { r.DecodeTokS = Ptr(30.0); r.CompletionTokens = Ptr(200) }),
		rec(10, func(r *Record) { r.DecodeTokS = Ptr(20.0); r.CompletionTokens = Ptr(300) }),
	})
	if *s.Inference.DecodeTokS != 20 {
		t.Fatalf("decode median = %v", *s.Inference.DecodeTokS)
	}
	if *s.Inference.CompletionTokensTotal != 600 {
		t.Fatalf("completion total = %v", *s.Inference.CompletionTokensTotal)
	}
}

// "not-exposed" must stay distinct from "miss" in aggregate, not just per turn.
func TestCacheVerdictsAreCountedSeparately(t *testing.T) {
	s := &Story{}
	s.Rollup([]*Record{
		rec(10, func(r *Record) { r.Cache = ComputeCache(Ptr(100), nil, 90, 0, 100) }),
		rec(10, func(r *Record) { r.Cache = ComputeCache(Ptr(100), Ptr(0), 90, 90, 10) }),
		rec(10, func(r *Record) { r.Cache = ComputeCache(Ptr(100), Ptr(90), 90, 90, 10) }),
	})
	v := s.Inference.CacheVerdicts
	if v[CacheNotExposed] != 1 || v[CacheMiss] != 1 || v[CachePartial] != 1 {
		t.Fatalf("verdicts = %v", v)
	}
	if s.Inference.SharedWithPreviousTotal != 180 {
		t.Fatalf("shared total = %d", s.Inference.SharedWithPreviousTotal)
	}
}

// A story whose turns all hit the token ceiling is a different result from one
// whose turns stopped naturally, however similar the wall clock.
func TestFinishReasonsAreCounted(t *testing.T) {
	s := &Story{}
	s.Rollup([]*Record{
		rec(10, func(r *Record) { r.FinishReason = "length" }),
		rec(10, func(r *Record) { r.FinishReason = "length" }),
		rec(10, func(r *Record) { r.FinishReason = "stop" }),
	})
	if s.Inference.FinishReasons["length"] != 2 || s.Inference.FinishReasons["stop"] != 1 {
		t.Fatalf("finish reasons = %v", s.Inference.FinishReasons)
	}
}
