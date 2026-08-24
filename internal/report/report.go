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
	"fmt"
	"math"
	"os"
	"sort"
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

	PrefixHit         Stat
	PrefixMiss        Stat
	StablePrefixBytes Stat

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

	var wall, vttft, rttft, dec, decd, pre, pt, ct, rt, hit, miss, tau, acc, spb []float64
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
		spb = append(spb, float64(r.PromptBytes))
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
	c.StablePrefixBytes = stat(spb, 0)

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

// WriteCSV emits one row per cell for downstream analysis.
func WriteCSV(path string, cells []Cell) error {
	var b strings.Builder
	b.WriteString("lane,context,layout,engine,thermal,n,passed,failed," +
		"wall_ms_median,wall_ms_min,wall_ms_max,visible_ttft_ms_median," +
		"reasoning_ttft_ms_median,prompt_tokens_median,completion_tokens_median," +
		"reasoning_tokens_median,prefill_tok_s_median,decode_tok_s_median," +
		"decode_tok_s_derived_median," +
		"prefix_hit_tokens_median,prefix_miss_tokens_median,dflash_tau_median," +
		"dflash_accept_rate_median,prompt_sha256,incomparable\n")
	for _, c := range cells {
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%d,%d,%d,"+
			"%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%q\n",
			c.Key.Lane, c.Key.ContextVariant, c.Key.PromptLayout, c.Key.Engine, c.Key.Thermal,
			c.N, c.Passed, c.Failed,
			csvStat(c.WallMS, "median"), csvStat(c.WallMS, "min"), csvStat(c.WallMS, "max"),
			csvStat(c.VisibleTTFT, "median"), csvStat(c.ReasonTTFT, "median"),
			csvStat(c.PromptTokens, "median"), csvStat(c.CompletionTokens, "median"),
			csvStat(c.ReasoningTokens, "median"),
			csvStat(c.PrefillTokS, "median"), csvStat(c.DecodeTokS, "median"),
			csvStat(c.DecodeTokSDerived, "median"),
			csvStat(c.PrefixHit, "median"), csvStat(c.PrefixMiss, "median"),
			csvStat(c.Tau, "median"), csvStat(c.AcceptRate, "median"),
			c.PromptSHA, strings.Join(c.Incomparable, "; "))
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
