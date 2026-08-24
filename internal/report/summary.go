package report

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/WilliamHCarter/RigBench/internal/executor"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

// SummaryInput is everything a summary.md needs beyond the records themselves.
type SummaryInput struct {
	Run     *metrics.RunIdentity
	Records []metrics.Record
	Cells   []Cell

	// Mutants and Tripwires are the fixture's self-checks. They appear near the
	// top, because a result from a fixture whose controls did not fire is not a
	// result.
	Mutants   []executor.MutantOutcome
	Tripwires []executor.TripwireOutcome

	// Unmeasured is copied from the fixture manifest so every report states its
	// own gaps rather than implying they are covered.
	Unmeasured []string

	// Caveats are run-specific warnings, for example that the endpoint was the
	// in-repo mock.
	Caveats []string
}

// WriteSummary renders summary.md.
func WriteSummary(path string, in SummaryInput) error {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }

	w("# AgentBench-01 run %s\n\n", in.Run.RunID)

	if len(in.Caveats) > 0 {
		w("> **Read this first**\n>\n")
		for _, c := range in.Caveats {
			w("> - %s\n", c)
		}
		w("\n")
	}

	// --- 1. quality first --------------------------------------------------
	w("## 1. Quality gates\n\n")
	if len(in.Cells) == 0 {
		w("No records.\n\n")
	} else {
		w("| Config | Thermal | n | Pass | Fail | Failing gates |\n")
		w("|---|---|---|---|---|---|\n")
		for _, c := range in.Cells {
			w("| `%s` | %s | %d | %d | %d | %s |\n",
				c.Key.Engine, c.Key.Thermal, c.N, c.Passed, c.Failed,
				gateSummary(c.FailedGateCounts))
		}
		w("\n")
	}

	// --- 2. wall clock -----------------------------------------------------
	w("## 2. Wall clock\n\n")
	w("Median, with min..max where a cell has more than one repetition. ")
	w("The builder score is time to a quality-gated passing patch; a failing ")
	w("row keeps its timing but is not eligible to be a champion.\n\n")
	w("| Config | Thermal | Eligible | Wall ms | Visible TTFT ms | Reasoning TTFT ms |\n")
	w("|---|---|---|---|---|---|\n")
	for _, c := range in.Cells {
		eligible := "no"
		if c.Passed > 0 && c.Failed == 0 {
			eligible = "yes"
		} else if c.Passed > 0 {
			eligible = "partial"
		}
		w("| `%s` | %s | %s | %s | %s | %s |\n",
			c.Key.Engine, c.Key.Thermal, eligible,
			c.WallMS, c.VisibleTTFT, c.ReasonTTFT)
	}
	w("\n")

	// --- 3. explanatory telemetry ------------------------------------------
	w("## 3. Explanatory telemetry\n\n")
	w("These explain the wall clock; they are not the optimization target. ")
	w("`not exposed` means the engine did not report the field -- it is not zero.\n\n")
	w("| Config | Prompt tok | Completion tok | Reasoning tok | Prefill tok/s | Decode tok/s (engine) | Decode tok/s (derived) | Prefix hit | Prefix miss | tau | Accept |\n")
	w("|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range in.Cells {
		w("| `%s` | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			c.Key.Engine, c.PromptTokens, c.CompletionTokens, c.ReasoningTokens,
			c.PrefillTokS, c.DecodeTokS, c.DecodeTokSDerived,
			c.PrefixHit, c.PrefixMiss, c.Tau, c.AcceptRate)
	}
	w("\nThe derived decode rate is this runner's own completion-tokens over the ")
	w("streaming window. It is shown beside the engine's own figure and never ")
	w("merged with it: one is a client stopwatch, the other a server counter.\n\n")

	// --- 4. comparability --------------------------------------------------
	w("## 4. Comparability\n\n")
	anyIncomparable := false
	for _, c := range in.Cells {
		if len(c.Incomparable) > 0 {
			anyIncomparable = true
			w("- **`%s` (%s)**: %s\n", c.Key.Engine, c.Key.Thermal, strings.Join(c.Incomparable, "; "))
		}
	}
	if !anyIncomparable {
		w("Every cell aggregates rows that share a prompt hash, a model, an engine commit and a reasoning budget.\n\n")
	} else {
		w("\n")
	}
	w("| Config | Prompt sha256 | Model | Reasoning |\n|---|---|---|---|\n")
	for _, c := range in.Cells {
		sha := c.PromptSHA
		if sha == "" {
			sha = "**mixed**"
		} else if len(sha) > 16 {
			sha = sha[:16]
		}
		w("| `%s` | `%s` | %s | %s |\n", c.Key.Engine, sha, c.Model, c.ThinkingMode)
	}
	w("\n")

	// --- 5. fixture self-checks -------------------------------------------
	w("## 5. Fixture self-checks\n\n")
	if len(in.Mutants) == 0 && len(in.Tripwires) == 0 {
		w("**Not run for this run.** The fixture's anti-vacuity controls and the ")
		w("hidden-suite tripwires were not executed here, so nothing below rests on ")
		w("a demonstration that the hidden suite is non-vacuous. Run `agentbench ")
		w("verify-fixture`, or pass `-verify-fixture` to `run`, to produce that evidence.\n\n")
	}
	if len(in.Tripwires) > 0 {
		bad := 0
		for _, t := range in.Tripwires {
			if !t.Fired {
				bad++
			}
		}
		w("**Hidden suite executes.** A deliberately failing test was appended to each hidden ")
		w("source file in turn; each must turn the rung red, or that file is compiled but never run.\n\n")
		w("| Hidden file | Tripwire fired |\n|---|---|\n")
		for _, t := range in.Tripwires {
			mark := "yes"
			if !t.Fired {
				mark = "**NO — " + t.Detail + "**"
			}
			w("| `%s` | %s |\n", t.File, mark)
		}
		if bad > 0 {
			w("\n**%d hidden file(s) are not reached. Every hidden-test result in this run is void.**\n", bad)
		}
		w("\n")
	}
	if len(in.Mutants) > 0 {
		bad := 0
		for _, m := range in.Mutants {
			if !m.OK {
				bad++
			}
		}
		w("**Anti-vacuity controls.** Each mutant is applied to the reference solution. ")
		w("A mutant the hidden suite fails to catch means the suite is vacuous for that invariant.\n\n")
		w("| Mutant | Lens | Build | Visible | Hidden | Control |\n|---|---|---|---|---|---|\n")
		for _, m := range in.Mutants {
			verdict := "held"
			if !m.OK {
				verdict = "**BROKEN — " + strings.Join(m.Reasons, "; ") + "**"
			}
			w("| `%s` | %s | %s | %s | %s | %s |\n",
				m.ID, m.Lens, m.Got.Build, m.Got.Visible, m.Got.Hidden, verdict)
		}
		w("\n")

		var invisible []string
		for _, m := range in.Mutants {
			if m.OK && m.Got.Visible == "pass" && m.Got.Hidden == "fail" {
				invisible = append(invisible, m.ID)
			}
		}
		if len(invisible) > 0 {
			w("Of these, **%d pass the visible suite and are caught only by the hidden suite**: %s. ",
				len(invisible), "`"+strings.Join(invisible, "`, `")+"`")
			w("That is the hidden suite earning its place rather than restating the visible one.\n\n")
		}
		if bad > 0 {
			w("**%d control(s) did not behave as declared. Fix the fixture before interpreting any result above.**\n\n", bad)
		}
	}

	// --- 6. what this run does not measure --------------------------------
	w("## 6. Not measured\n\n")
	if len(in.Unmeasured) == 0 && len(in.Caveats) == 0 {
		w("Nothing recorded.\n\n")
	}
	for _, u := range in.Unmeasured {
		w("- %s\n", u)
	}
	w("\n")

	// --- 7. per-request appendix ------------------------------------------
	w("## 7. Requests\n\n")
	w("| # | Engine | Thermal | Wall ms | Transport | Passed | Failing gates | Output sha256 |\n")
	w("|---|---|---|---|---|---|---|---|\n")
	for i, r := range in.Records {
		passed := "no"
		if r.Quality != nil && r.Quality.Passed {
			passed = "yes"
		}
		sha := r.OutputSHA256
		if len(sha) > 12 {
			sha = sha[:12]
		}
		w("| %d | `%s` | %s | %.0f | %s | %s | %s | `%s` |\n",
			i+1, r.Engine, r.Thermal, r.WallMS, r.TransportStatus, passed,
			strings.Join(qualityFails(r.Quality), " "), sha)
	}
	w("\n")

	w("## 8. Run identity\n\n")
	w("- benchmark: `%s`", short(in.Run.BenchmarkGitSHA))
	if in.Run.BenchmarkGitDirty {
		w(" **(dirty working tree)**")
	}
	w("\n")
	w("- fixture: `%s` v%s, manifest `%s`\n",
		in.Run.FixtureID, in.Run.FixtureVersion, short(in.Run.FixtureManifest))
	w("- host: %s/%s, %d cpu, %s\n", in.Run.Host.OS, in.Run.Host.Arch, in.Run.Host.CPUs, in.Run.Host.Kernel)
	if in.Run.Host.GPU != "" {
		w("- gpu: %s\n", in.Run.Host.GPU)
	}
	w("- toolchain: go %s, zig %s (pinned %s)\n",
		in.Run.Toolchain.Go, in.Run.Toolchain.Zig, in.Run.Toolchain.ZigPin)
	w("- thermal: %s (%s)\n", in.Run.Thermal, in.Run.WarmupPolicy)
	for _, e := range in.Run.Engines {
		w("- engine `%s`: %s, model `%s`, telemetry adapter `%s`",
			e.Name, e.Endpoint, e.Model, e.TelemetryAdapter)
		if e.SpeculationMode != "" {
			w(", speculation `%s`", e.SpeculationMode)
		}
		w("\n")
	}
	w("\n")

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func qualityFails(q *metrics.Quality) []string {
	if q == nil {
		return []string{"—"}
	}
	f := q.FailedGates()
	if len(f) == 0 {
		return []string{"—"}
	}
	for i := range f {
		f[i] = "`" + f[i] + "`"
	}
	return f
}

func gateSummary(m map[string]int) string {
	if len(m) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("`%s`x%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "unknown"
	}
	return s
}
