// Package report aggregates request records into human-readable and
// machine-readable summaries.
//
// The reporter never changes a raw measurement. It groups comparable rows,
// reports medians with dispersion, and refuses to merge rows that differ in
// anything that makes them incomparable -- prompt hash, model hash, engine
// commit, reasoning budget or thermal class. Where it cannot merge, it says so
// in the output rather than picking one.
//
// Reporting order is fixed by the benchmark contract: quality gate result
// first, wall time next, and explanatory telemetry last. A summary that leads
// with tokens per second invites optimizing the wrong thing.
package report

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

// Key identifies a set of rows that may be aggregated together.
type Key struct {
	Lane           string
	ContextVariant string
	PromptLayout   string
	Engine         string
	Thermal        string
}

func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", k.Lane, k.ContextVariant, k.PromptLayout, k.Engine, k.Thermal)
}

// Cell is one aggregated group.
type Cell struct {
	Key Key
	N   int

	Passed int
	Failed int
	// Unscored counts replay turns that carry no quality verdict. They are not
	// failures and must never be counted as any: a four-turn replay has three
	// turns nobody judged, and a report that called them failures would show a
	// green run as 25% passing.
	Unscored int
	// FailedGateCounts is why the failures failed, so a regression is
	// attributable without opening the logs.
	FailedGateCounts map[string]int

	WallMS            Stat
	VisibleTTFT       Stat
	ReasonTTFT        Stat
	DecodeTokS        Stat
	DecodeTokSDerived Stat
	PrefillTokS       Stat

	PromptTokens     Stat
	CompletionTokens Stat
	ReasoningTokens  Stat

	PrefixHit  Stat
	PrefixMiss Stat

	PromptBytes       Stat
	StablePrefixBytes Stat
	// AppendedBytes covers turns after the first only. Turn 0 appends to
	// nothing, and including its zero would halve the median of a four-turn
	// replay.
	AppendedBytes Stat

	Tau        Stat
	AcceptRate Stat

	Model        string
	ThinkingMode string

	// Incomparable lists reasons this cell mixes rows that should not be mixed.
	// A non-empty list is printed next to the numbers, never suppressed.
	Incomparable []string

	// PromptSHA is the shared prompt hash, or "" when rows disagreed.
	PromptSHA string

	rows []metrics.Record
}

// Stat is a median-first summary. Mean is deliberately absent: a cold
// first-capture outlier should move the dispersion, not the headline.
type Stat struct {
	N      int
	Median float64
	Min    float64
	Max    float64
	// Spread is max-min, the honest dispersion for the 3-5 repetitions the run
	// protocol asks for. A standard deviation over n=3 would be decoration.
	Spread float64
	// Missing counts rows where the value was null -- not exposed by the
	// engine. A Stat with N=0 and Missing>0 means "never reported", which is
	// different from zero.
	Missing int
}

func (s Stat) String() string {
	if s.N == 0 {
		if s.Missing > 0 {
			return "not exposed"
		}
		return "-"
	}
	if s.N == 1 {
		return trim(s.Median)
	}
	return fmt.Sprintf("%s (%s..%s)", trim(s.Median), trim(s.Min), trim(s.Max))
}

func trim(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return fmt.Sprintf("%.0f", f)
	}
	return fmt.Sprintf("%.2f", f)
}

func stat(vals []float64, missing int) Stat {
	s := Stat{N: len(vals), Missing: missing}
	if len(vals) == 0 {
		return s
	}
	sort.Float64s(vals)
	s.Min = vals[0]
	s.Max = vals[len(vals)-1]
	s.Spread = s.Max - s.Min
	mid := len(vals) / 2
	if len(vals)%2 == 1 {
		s.Median = vals[mid]
	} else {
		s.Median = (vals[mid-1] + vals[mid]) / 2
	}
	return s
}

// Aggregate groups records into cells.
func Aggregate(records []metrics.Record) []Cell {
	groups := map[Key][]metrics.Record{}
	var order []Key
	for _, r := range records {
		k := Key{
			Lane:           string(r.Lane),
			ContextVariant: r.ContextVariant,
			PromptLayout:   r.PromptLayout,
			Engine:         r.Engine,
			Thermal:        r.Thermal,
		}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}

	cells := make([]Cell, 0, len(order))
	for _, k := range order {
		cells = append(cells, buildCell(k, groups[k]))
	}
	sort.SliceStable(cells, func(i, j int) bool {
		if cells[i].Key.Thermal != cells[j].Key.Thermal {
			return cells[i].Key.Thermal < cells[j].Key.Thermal
		}
		return cells[i].Key.Engine < cells[j].Key.Engine
	})
	return cells
}

func buildCell(k Key, rows []metrics.Record) Cell {
	c := Cell{Key: k, N: len(rows), rows: rows, FailedGateCounts: map[string]int{}}

	var wall, vttft, rttft, dec, decd, pre, pt, ct, rt, hit, miss, tau, acc []float64
	var pbytes, spb, appended []float64
	var missWall, missV, missR, missDec, missDecD, missPre, missPT, missCT, missRT, missHit, missMiss, missTau, missAcc int

	promptSHAs := map[string]bool{}
	models := map[string]bool{}
	thinking := map[string]bool{}
	commits := map[string]bool{}
	modelHashes := map[string]bool{}

	addF := func(dst *[]float64, missing *int, v *float64) {
		if v == nil {
			*missing++
			return
		}
		*dst = append(*dst, *v)
	}
	addI := func(dst *[]float64, missing *int, v *int) {
		if v == nil {
			*missing++
			return
		}
		*dst = append(*dst, float64(*v))
	}

	for _, r := range rows {
		promptSHAs[r.PromptSHA256] = true
		models[r.Model] = true
		thinking[r.ThinkingMode] = true
		commits[deref(r.EngineCommit)] = true
		modelHashes[deref(r.ModelHash)] = true

		wall = append(wall, r.WallMS)
		pbytes = append(pbytes, float64(r.PromptBytes))
		spb = append(spb, float64(r.StablePrefixBytes))
		if r.TurnIndex > 0 {
			appended = append(appended, float64(r.AppendedBytes))
		}
		addF(&vttft, &missV, r.VisibleTTFTMS)
		addF(&rttft, &missR, r.ReasoningTTFTMS)
		addF(&dec, &missDec, r.DecodeTokS)
		addF(&decd, &missDecD, r.DecodeTokSDerived)
		addF(&pre, &missPre, r.PrefillTokS)
		addI(&pt, &missPT, r.PromptTokens)
		addI(&ct, &missCT, r.CompletionTokens)
		addI(&rt, &missRT, r.ReasoningTokens)
		addI(&hit, &missHit, r.PrefixCacheHitTokens)
		addI(&miss, &missMiss, r.PrefixCacheMissTokens)
		addF(&tau, &missTau, r.DFlashTau)
		addF(&acc, &missAcc, r.DFlashAcceptRate)

		if !r.Scored {
			c.Unscored++
			continue
		}
		if r.Quality != nil && r.Quality.Passed {
			c.Passed++
		} else {
			c.Failed++
			if r.Quality != nil {
				for _, g := range r.Quality.Gates {
					if g.Result != metrics.GatePass {
						c.FailedGateCounts[fmt.Sprintf("%s=%s", g.Name, g.Result)]++
					}
				}
			}
		}
	}
	_ = missWall

	c.WallMS = stat(wall, 0)
	c.VisibleTTFT = stat(vttft, missV)
	c.ReasonTTFT = stat(rttft, missR)
	c.DecodeTokS = stat(dec, missDec)
	c.DecodeTokSDerived = stat(decd, missDecD)
	c.PrefillTokS = stat(pre, missPre)
	c.PromptTokens = stat(pt, missPT)
	c.CompletionTokens = stat(ct, missCT)
	c.ReasoningTokens = stat(rt, missRT)
	c.PrefixHit = stat(hit, missHit)
	c.PrefixMiss = stat(miss, missMiss)
	c.Tau = stat(tau, missTau)
	c.AcceptRate = stat(acc, missAcc)
	c.PromptBytes = stat(pbytes, 0)
	c.StablePrefixBytes = stat(spb, 0)
	c.AppendedBytes = stat(appended, 0)

	c.Model = joinSet(models)
	c.ThinkingMode = joinSet(thinking)
	if len(promptSHAs) == 1 {
		for s := range promptSHAs {
			c.PromptSHA = s
		}
	} else {
		c.Incomparable = append(c.Incomparable,
			fmt.Sprintf("%d different prompt hashes in one cell", len(promptSHAs)))
	}
	if len(models) > 1 {
		c.Incomparable = append(c.Incomparable, "more than one model")
	}
	if len(thinking) > 1 {
		c.Incomparable = append(c.Incomparable, "more than one reasoning budget")
	}
	if len(commits) > 1 {
		c.Incomparable = append(c.Incomparable, "more than one engine commit")
	}
	if len(modelHashes) > 1 {
		c.Incomparable = append(c.Incomparable, "more than one model hash")
	}
	return c
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func joinSet(m map[string]bool) string {
	var out []string
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// column pairs a CSV header with the function that produces its value.
//
// Built as pairs rather than as a header string plus a positional Fprintf: the
// positional form silently drifts the moment a column is inserted, and a
// benchmark whose CSV columns are off by one is worse than one with no CSV.
type column struct {
	name string
	get  func(Cell) string
}

var columns = []column{
	{"lane", func(c Cell) string { return c.Key.Lane }},
	{"context", func(c Cell) string { return c.Key.ContextVariant }},
	{"layout", func(c Cell) string { return c.Key.PromptLayout }},
	{"engine", func(c Cell) string { return c.Key.Engine }},
	{"thermal", func(c Cell) string { return c.Key.Thermal }},
	{"n", func(c Cell) string { return strconv.Itoa(c.N) }},
	{"passed", func(c Cell) string { return strconv.Itoa(c.Passed) }},
	{"failed", func(c Cell) string { return strconv.Itoa(c.Failed) }},
	{"unscored", func(c Cell) string { return strconv.Itoa(c.Unscored) }},
	{"wall_ms_median", func(c Cell) string { return csvStat(c.WallMS, "median") }},
	{"wall_ms_min", func(c Cell) string { return csvStat(c.WallMS, "min") }},
	{"wall_ms_max", func(c Cell) string { return csvStat(c.WallMS, "max") }},
	{"visible_ttft_ms_median", func(c Cell) string { return csvStat(c.VisibleTTFT, "median") }},
	{"reasoning_ttft_ms_median", func(c Cell) string { return csvStat(c.ReasonTTFT, "median") }},
	{"prompt_tokens_median", func(c Cell) string { return csvStat(c.PromptTokens, "median") }},
	{"completion_tokens_median", func(c Cell) string { return csvStat(c.CompletionTokens, "median") }},
	{"reasoning_tokens_median", func(c Cell) string { return csvStat(c.ReasoningTokens, "median") }},
	{"prefill_tok_s_median", func(c Cell) string { return csvStat(c.PrefillTokS, "median") }},
	{"decode_tok_s_median", func(c Cell) string { return csvStat(c.DecodeTokS, "median") }},
	{"decode_tok_s_derived_median", func(c Cell) string { return csvStat(c.DecodeTokSDerived, "median") }},
	{"prefix_hit_tokens_median", func(c Cell) string { return csvStat(c.PrefixHit, "median") }},
	{"prefix_miss_tokens_median", func(c Cell) string { return csvStat(c.PrefixMiss, "median") }},
	{"dflash_tau_median", func(c Cell) string { return csvStat(c.Tau, "median") }},
	{"dflash_accept_rate_median", func(c Cell) string { return csvStat(c.AcceptRate, "median") }},
	{"prompt_bytes_median", func(c Cell) string { return csvStat(c.PromptBytes, "median") }},
	{"stable_prefix_bytes_median", func(c Cell) string { return csvStat(c.StablePrefixBytes, "median") }},
	{"appended_bytes_median", func(c Cell) string { return csvStat(c.AppendedBytes, "median") }},
	{"prompt_sha256", func(c Cell) string { return c.PromptSHA }},
	{"incomparable", func(c Cell) string { return strings.Join(c.Incomparable, "; ") }},
}

// Columns lists the CSV header, exported so a test can assert every row has as
// many fields as the header has names.
func Columns() []string {
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = c.name
	}
	return out
}

// WriteCSV emits one row per cell for downstream analysis.
func WriteCSV(path string, cells []Cell) error {
	var b strings.Builder
	w := csv.NewWriter(&b)
	if err := w.Write(Columns()); err != nil {
		return err
	}
	for _, c := range cells {
		row := make([]string, len(columns))
		for i, col := range columns {
			row[i] = col.get(c)
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// csvStat writes an empty field for a value no engine reported. An empty CSV
// cell is unambiguous; a 0 would not be.
func csvStat(s Stat, which string) string {
	if s.N == 0 {
		return ""
	}
	switch which {
	case "min":
		return trim(s.Min)
	case "max":
		return trim(s.Max)
	}
	return trim(s.Median)
}
