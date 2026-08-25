package metrics

import "testing"

// The property the whole file exists for: a backend that reported nothing is
// not a backend that reused nothing.
func TestNotExposedIsNotAMiss(t *testing.T) {
	absent := ComputeCache(Ptr(1000), nil, 800, 0, 1000)
	if absent.Verdict != CacheNotExposed {
		t.Fatalf("verdict = %q", absent.Verdict)
	}
	if absent.NewlyPrefilledTokens != nil || absent.ReuseFraction != nil {
		t.Fatal("a derived number was produced from an absent measurement")
	}

	zero := ComputeCache(Ptr(1000), Ptr(0), 800, 0, 1000)
	if zero.Verdict != CacheMiss {
		t.Fatalf("a reported zero rendered as %q", zero.Verdict)
	}
	if zero.NewlyPrefilledTokens == nil || *zero.NewlyPrefilledTokens != 1000 {
		t.Fatalf("newly prefilled = %v", zero.NewlyPrefilledTokens)
	}
}

func TestPartialAndFullReuse(t *testing.T) {
	p := ComputeCache(Ptr(1000), Ptr(750), 800, 700, 300)
	if p.Verdict != CachePartial {
		t.Fatalf("verdict = %q", p.Verdict)
	}
	if *p.NewlyPrefilledTokens != 250 {
		t.Fatalf("newly = %d", *p.NewlyPrefilledTokens)
	}
	if *p.ReuseFraction < 0.74 || *p.ReuseFraction > 0.76 {
		t.Fatalf("fraction = %v", *p.ReuseFraction)
	}

	f := ComputeCache(Ptr(1000), Ptr(1000), 800, 1000, 0)
	if f.Verdict != CacheFull {
		t.Fatalf("verdict = %q", f.Verdict)
	}
	if *f.NewlyPrefilledTokens != 0 {
		t.Fatalf("newly = %d", *f.NewlyPrefilledTokens)
	}
}

// A server reporting more cached tokens than prompt tokens must not produce a
// negative prefill count.
func TestOverReportedCacheDoesNotGoNegative(t *testing.T) {
	c := ComputeCache(Ptr(100), Ptr(140), 90, 100, 0)
	if *c.NewlyPrefilledTokens != 0 {
		t.Fatalf("newly = %d", *c.NewlyPrefilledTokens)
	}
	if c.Verdict != CacheFull {
		t.Fatalf("verdict = %q", c.Verdict)
	}
}

// The runner-measured numbers must survive even when the engine says nothing,
// because they are the only cache evidence a telemetry-free backend can give.
func TestRunnerMeasuredNumbersSurviveWithoutTelemetry(t *testing.T) {
	c := ComputeCache(nil, nil, 6800, 6500, 900)
	if c.ReusablePrefixTokens != 6800 || c.SharedWithPreviousTokens != 6500 || c.AppendedTokens != 900 {
		t.Fatalf("got %+v", c)
	}
}

// Prompt tokens without a cached count yields no fraction: mixing a server
// counter with a client heuristic produces a number belonging to neither.
func TestNoFractionWithoutBothSides(t *testing.T) {
	c := ComputeCache(nil, Ptr(500), 400, 0, 100)
	if c.ReuseFraction != nil || c.NewlyPrefilledTokens != nil {
		t.Fatal("a fraction was derived from one side only")
	}
	if c.Verdict != CachePartial {
		t.Fatalf("verdict = %q", c.Verdict)
	}
}
