package client

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

// ServerLog reads telemetry a server writes to its own log rather than to the
// response stream.
//
// Hipfire emits lines like
//
//	drafter=dflash tau=4.86 tok/s=32.9 windows=5593 accepted=27184 ...
//
// which were being grepped out of serve.log by hand. A number a human copies
// between a terminal and a spreadsheet is a number that will eventually be
// copied wrong, so the harness reads it.
//
// It tracks a byte offset and reads only what appeared since the previous call,
// so telemetry is attributed to the request that produced it rather than to the
// whole session. Nothing here is required: a missing or unreadable log yields
// an empty Telemetry, and every field stays null.
type ServerLog struct {
	Path string

	mu     sync.Mutex
	offset int64
}

// Mark records the current end of the log. Call before a request.
func (l *ServerLog) Mark() {
	if l == nil || l.Path == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if fi, err := os.Stat(l.Path); err == nil {
		l.offset = fi.Size()
	}
}

// Since reads everything appended after the last Mark and extracts telemetry
// from it, returning the raw slice too so it can be kept as an artifact.
func (l *ServerLog) Since() (Telemetry, string) {
	if l == nil || l.Path == "" {
		return Telemetry{}, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.Open(l.Path)
	if err != nil {
		return Telemetry{}, ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return Telemetry{}, ""
	}
	// A truncated or rotated log is read from the start rather than seeked past
	// its own end, which would silently return nothing forever.
	if fi.Size() < l.offset {
		l.offset = 0
	}
	if _, err := f.Seek(l.offset, 0); err != nil {
		return Telemetry{}, ""
	}
	const maxSlice = 4 << 20
	size := fi.Size() - l.offset
	if size <= 0 {
		return Telemetry{}, ""
	}
	if size > maxSlice {
		size = maxSlice
	}
	buf := make([]byte, size)
	n, _ := f.Read(buf)
	l.offset = fi.Size()

	slice := string(buf[:n])
	return ParseServerLog(slice), slice
}

// kvRe matches `key=value` with an optional unit-ish suffix, which is how
// Hipfire's summary lines are shaped.
var kvRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_/]*)\s*=\s*([^\s,;]+)`)

// logKeys maps a log key onto the telemetry field it fills. Unknown keys are
// ignored rather than guessed at: a wrong mapping produces a fabricated number,
// which is the one outcome this whole package exists to prevent.
var logKeys = map[string]string{
	"tau":           "tau",
	"acceptance":    "accept",
	"accept_rate":   "accept",
	"acc":           "accept",
	"tok/s":         "decode",
	"decode_tok/s":  "decode",
	"decode":        "decode",
	"prefill_tok/s": "prefill",
	"prefill":       "prefill",
	"draft_tok/s":   "draft",
	"verify_tok/s":  "verify",
	"verify_ms":     "verify_ms",
	"windows":       "windows",
	"n_windows":     "windows",
	"accepted":      "accepted",
	"rejected":      "rejected",
	"drafter":       "method",
	"speculation":   "method",
	"kv":            "kv",
	"kv_cache":      "kv",
	"cached":        "cached",
	"cached_tokens": "cached",
	"reused":        "cached",
	"model":         "model",
	"draft":         "draft_file",
	"commit":        "commit",
}

// ParseServerLog extracts telemetry from a slice of server log text.
//
// Later lines win, because a per-request summary is usually printed after the
// request it describes and the slice may contain several.
func ParseServerLog(text string) Telemetry {
	var t Telemetry
	for _, line := range strings.Split(text, "\n") {
		for _, m := range kvRe.FindAllStringSubmatch(line, -1) {
			key := strings.ToLower(m[1])
			field, known := logKeys[key]
			if !known {
				continue
			}
			val := strings.TrimRight(m[2], ".,;")
			switch field {
			case "tau":
				setF(&t.DFlashTau, val)
			case "accept":
				if f, ok := parseRate(val); ok {
					t.DFlashAcceptRate = metrics.Ptr(f)
				}
			case "decode":
				setF(&t.DecodeTokS, val)
			case "prefill":
				setF(&t.PrefillTokS, val)
			case "draft":
				setF(&t.DraftTokS, val)
			case "verify":
				setF(&t.VerifyTokS, val)
			case "verify_ms":
				setF(&t.VerifyMS, val)
			case "windows":
				setI(&t.SpeculativeWindows, val)
			case "accepted":
				setI(&t.AcceptedTokens, val)
			case "rejected":
				setI(&t.RejectedTokens, val)
			case "method":
				t.SpeculationMethod = val
			case "kv":
				t.KVFormat = val
			case "cached":
				setI(&t.PrefixCacheHitTokens, val)
			case "model":
				t.ResolvedModel = val
			case "draft_file":
				t.DraftFile = val
			case "commit":
				t.EngineCommit = val
			}
		}
	}
	return t
}

// parseRate accepts both 0.426 and 42.6% and normalizes to a fraction, because
// a server that switches representation between releases must not silently
// change a column by two orders of magnitude.
func parseRate(v string) (float64, bool) {
	pct := strings.HasSuffix(v, "%")
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
	if err != nil {
		return 0, false
	}
	if pct {
		return f / 100, true
	}
	return f, true
}

func setF(dst **float64, v string) {
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		*dst = metrics.Ptr(f)
	}
}

func setI(dst **int, v string) {
	if i, err := strconv.Atoi(v); err == nil {
		*dst = metrics.Ptr(i)
	}
}
