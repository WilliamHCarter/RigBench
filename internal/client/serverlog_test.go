package client

import (
	"os"
	"path/filepath"
	"testing"
)

// The line shape that was being grepped out of serve.log by hand.
func TestParseServerLogReadsTheHipfireSummaryLine(t *testing.T) {
	tel := ParseServerLog(
		"[info] request done drafter=dflash tau=4.86 tok/s=32.9 windows=5593 " +
			"accepted=27184 rejected=5584 prefill_tok/s=597.2 kv=q8\n")
	if tel.DFlashTau == nil || *tel.DFlashTau != 4.86 {
		t.Fatalf("tau = %v", tel.DFlashTau)
	}
	if tel.DecodeTokS == nil || *tel.DecodeTokS != 32.9 {
		t.Fatalf("decode = %v", tel.DecodeTokS)
	}
	if tel.SpeculativeWindows == nil || *tel.SpeculativeWindows != 5593 {
		t.Fatalf("windows = %v", tel.SpeculativeWindows)
	}
	if tel.AcceptedTokens == nil || *tel.AcceptedTokens != 27184 {
		t.Fatalf("accepted = %v", tel.AcceptedTokens)
	}
	if tel.RejectedTokens == nil || *tel.RejectedTokens != 5584 {
		t.Fatalf("rejected = %v", tel.RejectedTokens)
	}
	if tel.SpeculationMethod != "dflash" || tel.KVFormat != "q8" {
		t.Fatalf("method=%q kv=%q", tel.SpeculationMethod, tel.KVFormat)
	}
}

// A server that changes representation between releases must not silently move
// a column by two orders of magnitude.
func TestParseServerLogNormalizesAcceptanceRate(t *testing.T) {
	for _, in := range []string{"acceptance=0.426", "acceptance=42.6%"} {
		tel := ParseServerLog(in)
		if tel.DFlashAcceptRate == nil {
			t.Fatalf("%s: nil", in)
		}
		if got := *tel.DFlashAcceptRate; got < 0.42 || got > 0.43 {
			t.Fatalf("%s: got %v", in, got)
		}
	}
}

// An unknown key must be ignored, not guessed at.
func TestParseServerLogIgnoresUnknownKeys(t *testing.T) {
	tel := ParseServerLog("mystery_metric=99 tau=1.5\n")
	if tel.DFlashTau == nil || *tel.DFlashTau != 1.5 {
		t.Fatal("the known key was lost")
	}
	if tel.DecodeTokS != nil || tel.AcceptedTokens != nil {
		t.Fatal("an unknown key filled a field")
	}
}

func TestParseServerLogOnNoiseYieldsNothing(t *testing.T) {
	tel := ParseServerLog("listening on 127.0.0.1:11435\nloading model...\n")
	if tel.DFlashTau != nil || tel.DecodeTokS != nil || tel.SpeculationMethod != "" {
		t.Fatalf("got %+v", tel)
	}
}

// Telemetry must be attributed to the request that produced it, not to the
// whole session.
func TestServerLogReadsOnlyWhatAppearedSinceTheMark(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.log")
	if err := os.WriteFile(path, []byte("old line tau=1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &ServerLog{Path: path}
	l.Mark()

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("new line tau=9.9\n")
	f.Close()

	tel, raw := l.Since()
	if tel.DFlashTau == nil || *tel.DFlashTau != 9.9 {
		t.Fatalf("tau = %v, want the value that appeared after the mark", tel.DFlashTau)
	}
	if len(raw) == 0 || raw == "old line tau=1.0\n" {
		t.Fatalf("raw slice is wrong: %q", raw)
	}
}

// A rotated log must not leave the reader seeked past a new file's end,
// silently returning nothing forever.
func TestServerLogSurvivesRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.log")
	os.WriteFile(path, []byte("a lot of old content tau=1.0\n"), 0o644)
	l := &ServerLog{Path: path}
	l.Mark()

	os.WriteFile(path, []byte("tau=2.0\n"), 0o644) // truncated and rewritten
	tel, _ := l.Since()
	if tel.DFlashTau == nil || *tel.DFlashTau != 2.0 {
		t.Fatalf("tau = %v after rotation", tel.DFlashTau)
	}
}

func TestServerLogWithNoPathIsHarmless(t *testing.T) {
	var l *ServerLog
	l.Mark()
	tel, raw := l.Since()
	if tel.DFlashTau != nil || raw != "" {
		t.Fatal("a nil log produced something")
	}
}
