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

	// LayoutComparison is populated when a run measured more than one prompt
	// layout over the same content.
	LayoutComparison []LayoutRow
}

// LayoutRow is one layout's reusable-prefix result in an A/B.
type LayoutRow struct {
	Layout            string
	StablePrefixBytes int
	// Share is StablePrefixBytes over the whole prompt, in [0,1].
	Share float64
	// CrossTaskReuseBytes is how much of a *second* task's prompt a cache
	// holding the first task's prompt could reuse. This is where the layouts
	// actually differ: within one story both append cleanly, because a
	// volatile-first layout's leading objective does not change between turns
	// of the same task.
	CrossTaskReuseBytes int
	Note                string
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
		w("| Config | Layout | Thermal | n | Pass | Fail | Unscored | Failing gates |\n")
		w("|---|---|---|---|---|---|---|---|\n")
		for _, c := range in.Cells {
			w("| `%s` | `%s` | %s | %d | %d | %d | %d | %s |\n",
				c.Key.Engine, c.Key.PromptLayout, c.Key.Thermal, c.N,
				c.Passed, c.Failed, c.Unscored,
				gateSummary(c.FailedGateCounts))
		}
		w("\nUnscored rows are replay turns that carry no quality verdict. They are ")
		w("not failures; a four-turn replay judges only its scored turn.\n\n")
	}

	// --- 2. wall clock -----------------------------------------------------
	w("## 2. Wall clock\n\n")
	w("Median, with min..max where a cell has more than one repetition. ")
	w("The builder score is time to a quality-gated passing patch; a failing ")
	w("row keeps its timing but is not eligible to be a champion.\n\n")
	w("| Config | Layout | Thermal | Eligible | Wall ms | Visible TTFT ms | Reasoning TTFT ms |\n")
	w("|---|---|---|---|---|---|---|\n")
	for _, c := range in.Cells {
		eligible := "no"
		switch {
		case c.Passed == 0 && c.Failed == 0:
			eligible = "unscored"
		case c.Passed > 0 && c.Failed == 0:
			eligible = "yes"
		case c.Passed > 0:
			eligible = "partial"
		}
		w("| `%s` | `%s` | %s | %s | %s | %s | %s |\n",
			c.Key.Engine, c.Key.PromptLayout, c.Key.Thermal, eligible,
			c.WallMS, c.VisibleTTFT, c.ReasonTTFT)
	}
	w("\nCold and warm rows are separate cells and are never averaged together. ")
	w("The thermal class is stated by the operator; it is not inferred from ")
	w("elapsed time.\n\n")

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

	// --- 3b. prompt growth and reusable prefix ----------------------------
	w("### Prompt growth and reusable prefix\n\n")
	w("`Stable prefix` is measured by the runner from the prompt bytes it sent, ")
	w("independently of any engine: it is the leading span of non-volatile ")
	w("blocks, verified at serialization time to be a real byte prefix of the ")
	w("whole prompt. `Prefix hit` is what the engine reported, and is `not ")
	w("exposed` when it reported nothing.\n\n")
	w("| Config | Layout | Thermal | Prompt bytes | Stable prefix bytes | Reusable | Appended per turn | Prefix hit tok |\n")
	w("|---|---|---|---|---|---|---|---|\n")
	for _, c := range in.Cells {
		reusable := "-"
		if c.PromptBytes.N > 0 && c.PromptBytes.Median > 0 {
			reusable = fmt.Sprintf("%.0f%%", 100*c.StablePrefixBytes.Median/c.PromptBytes.Median)
		}
		w("| `%s` | `%s` | %s | %s | %s | %s | %s | %s |\n",
			c.Key.Engine, c.Key.PromptLayout, c.Key.Thermal,
			c.PromptBytes, c.StablePrefixBytes, reusable, c.AppendedBytes, c.PrefixHit)
	}
	w("\n")

	if len(in.LayoutComparison) > 0 {
		w("### Layout A/B\n\n")
		w("The compared layouts carry identical block bytes and differ only in ")
		w("order and message boundaries, so any difference below is attributable ")
		w("to layout and not to a reworded prompt.\n\n")
		w("| Layout | Reusable prefix bytes | Share of prompt | Cross-task reuse bytes | Verdict |\n")
		w("|---|---|---|---|---|\n")
		for _, l := range in.LayoutComparison {
			w("| `%s` | %d | %.0f%% | %d | %s |\n",
				l.Layout, l.StablePrefixBytes, 100*l.Share, l.CrossTaskReuseBytes, l.Note)
		}
		w("\nWithin a single story both layouts append cleanly, so turn-to-turn reuse ")
		w("is **not** where they differ: a volatile-first layout's leading objective ")
		w("does not change between turns of the same task. The difference is the ")
		w("cross-task column — what a second story could reuse from the first.\n\n")
	}

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

	// Engine identity: produced and checked, or merely asserted.
	if in.Run != nil && len(in.Run.Engines) > 0 {
		w("### Engine identity\n\n")
		w("An OpenAI-compatible request cannot select a speculation mode, a KV ")
		w("quantization or a draft model: those are process-level settings. A row ")
		w("whose identity is **asserted** was labelled from a config file, not from ")
		w("anything this run produced or verified.\n\n")
		w("| Config | Identity | How | Engine commit | Model hash | Draft hash |\n")
		w("|---|---|---|---|---|---|\n")
		gaps := 0
		for _, e := range in.Run.Engines {
			state := "**asserted**"
			if e.Attested {
				state = "attested"
			}
			w("| `%s` | %s | %s | %s | %s | %s |\n",
				e.Name, state, e.AttestationMethod,
				orNotRecorded(e.EngineCommit), orNotRecorded(e.ModelHash), orNotRecorded(e.DraftHash))
			if e.EngineCommit == "" || e.ModelHash == "" {
				gaps++
			}
		}
		w("\n")
		if gaps > 0 {
			w("**%d config(s) record no engine commit or model hash.** Two runs cannot be ", gaps)
			w("compared across time without them, and a champion selected on these rows ")
			w("could not be reproduced. Populate them in the engine config, or have an ")
			w("`identity_probe` record them from the server.\n\n")
		}
	}

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
	w("| # | Engine | Layout | Turn | Thermal | Wall ms | Visible TTFT | Prompt B | Appended B | Prefix B | Transport | Verdict | Failing gates |\n")
	w("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for i, r := range in.Records {
		verdict := "unscored"
		if r.Scored {
			verdict = "FAIL"
			if r.Quality != nil && r.Quality.Passed {
				verdict = "pass"
			}
		}
		turn := fmt.Sprintf("%d", r.TurnIndex)
		if r.TurnCount > 1 {
			turn = fmt.Sprintf("%d/%d", r.TurnIndex, r.TurnCount-1)
		}
		appended := "-"
		if r.TurnIndex > 0 {
			appended = fmt.Sprintf("%d", r.AppendedBytes)
		}
		ttft := "-"
		if r.VisibleTTFTMS != nil {
			ttft = fmt.Sprintf("%.0f", *r.VisibleTTFTMS)
		}
		gates := "—"
		if r.Scored {
			gates = strings.Join(qualityFails(r.Quality), " ")
		}
		w("| %d | `%s` | `%s` | %s | %s | %.0f | %s | %d | %s | %d | %s | %s | %s |\n",
			i+1, r.Engine, r.PromptLayout, turn, r.Thermal, r.WallMS, ttft,
			r.PromptBytes, appended, r.StablePrefixBytes, r.TransportStatus, verdict, gates)
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
		if e.Attested {
			w(", identity attested")
		} else {
			w(", **identity asserted only**")
		}
		w("\n")
		if e.PreparationLog != "" {
			w("  - preparation log: `%s`\n", e.PreparationLog)
		}
		if e.ProbeArtifact != "" {
			w("  - identity probe: `%s`\n", e.ProbeArtifact)
		}
	}
	w("\n")

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func orNotRecorded(s string) string {
	if s == "" {
		return "*not recorded*"
	}
	if len(s) > 16 {
		return "`" + s[:16] + "`"
	}
	return "`" + s + "`"
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
