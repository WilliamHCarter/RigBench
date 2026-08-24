package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/config"
)

// MutantSet is the fixture's anti-vacuity control set.
//
// A mutant is not a defect model for its own sake; it is a control on the hidden
// suite. If the hidden suite stays green under a mutant, the suite is vacuous
// for that invariant and the fixture is broken -- which is a stop condition, not
// a warning.
type MutantSet struct {
	Schema    string   `json:"schema"`
	Fixture   string   `json:"fixture"`
	AppliesTo string   `json:"applies_to"`
	Note      string   `json:"note"`
	Mutants   []Mutant `json:"mutants"`
}

type Mutant struct {
	ID          string       `json:"id"`
	Lens        string       `json:"lens"`
	Description string       `json:"description"`
	Note        string       `json:"note,omitempty"`
	Expect      MutantExpect `json:"expect"`
	Edits       []Edit       `json:"edits"`
}

// MutantExpect records what each rung must do. `visible` is part of the
// contract because a mutant the visible suite catches proves nothing about the
// hidden suite's value; a mutant it misses proves everything.
type MutantExpect struct {
	Build   string `json:"build"`
	Visible string `json:"visible"`
	Hidden  string `json:"hidden"`
}

func LoadMutants(f *config.Fixture) (*MutantSet, error) {
	b, err := os.ReadFile(f.Path(f.MutantsFile))
	if err != nil {
		return nil, err
	}
	var ms MutantSet
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ms); err != nil {
		return nil, fmt.Errorf("%s: %w", f.MutantsFile, err)
	}
	if ms.Schema != "agentbench/mutants/v1" {
		return nil, fmt.Errorf("%s: unsupported schema %q", f.MutantsFile, ms.Schema)
	}
	return &ms, nil
}

// MutantOutcome is one control's observed behaviour next to its declared one.
type MutantOutcome struct {
	ID         string       `json:"id"`
	Lens       string       `json:"lens"`
	Note       string       `json:"note,omitempty"`
	Expect     MutantExpect `json:"expect"`
	Got        MutantExpect `json:"got"`
	OK         bool         `json:"ok"`
	Reasons    []string     `json:"reasons,omitempty"`
	HiddenLog  string       `json:"hidden_log,omitempty"`
	VisibleLog string       `json:"visible_log,omitempty"`
}

// VerifyMutants runs the whole control set against the reference solution.
//
// Each mutant gets its own staged worktree and its own zig cache, because a
// shared cache can silently re-run a stale binary and report six different
// mutations as identical -- a failure that has actually happened, and the
// reason the set includes a deliberate syntax-error control.
func VerifyMutants(ctx context.Context, f *config.Fixture, workDir, logDir string, only []string) ([]MutantOutcome, error) {
	ms, err := LoadMutants(f)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range only {
		want[id] = true
	}

	timeout := f.CommandTimeout()
	var out []MutantOutcome
	for _, mu := range ms.Mutants {
		if len(want) > 0 && !want[mu.ID] {
			continue
		}
		dir := filepath.Join(workDir, mu.ID)
		wt, err := Stage(ctx, f, dir, true)
		if err != nil {
			return nil, fmt.Errorf("mutant %s: %w", mu.ID, err)
		}
		if err := wt.InjectHidden(ctx); err != nil {
			return nil, fmt.Errorf("mutant %s: inject hidden: %w", mu.ID, err)
		}
		if err := wt.ApplyEdits(mu.Edits); err != nil {
			out = append(out, MutantOutcome{
				ID: mu.ID, Lens: mu.Lens, Note: mu.Note, Expect: mu.Expect,
				OK: false, Reasons: []string{"edit did not apply: " + err.Error()},
			})
			continue
		}

		vres := Run(ctx, dir, f.Commands.Visible, timeout)
		hres := Run(ctx, dir, f.Commands.Hidden, timeout)

		got := MutantExpect{
			Build:   buildVerdict(vres),
			Visible: passFail(vres),
			Hidden:  passFail(hres),
		}
		oc := MutantOutcome{
			ID: mu.ID, Lens: mu.Lens, Note: mu.Note,
			Expect: mu.Expect, Got: got,
			VisibleLog: writeMutantLog(logDir, mu.ID+".visible", vres),
			HiddenLog:  writeMutantLog(logDir, mu.ID+".hidden", hres),
		}
		if mu.Expect.Build != got.Build {
			oc.Reasons = append(oc.Reasons, fmt.Sprintf("build: want %s, got %s", mu.Expect.Build, got.Build))
		}
		if mu.Expect.Visible != got.Visible {
			oc.Reasons = append(oc.Reasons, fmt.Sprintf("visible: want %s, got %s", mu.Expect.Visible, got.Visible))
		}
		if mu.Expect.Hidden != got.Hidden {
			oc.Reasons = append(oc.Reasons, fmt.Sprintf("hidden: want %s, got %s", mu.Expect.Hidden, got.Hidden))
		}
		oc.OK = len(oc.Reasons) == 0
		out = append(out, oc)
	}
	return out, nil
}

// buildVerdict distinguishes a compile failure from a test failure in the
// *candidate's own tree*. It is deliberately not told about the hidden rung: a
// hidden suite that refuses to compile against a mutated tree has caught the
// mutant, and calling that "the build failed" would blame the candidate for the
// oracle doing its job.
func buildVerdict(results ...*CommandResult) string {
	for _, r := range results {
		if r == nil {
			continue
		}
		c := r.Combined()
		if strings.Contains(c, "compilation errors") || strings.Contains(c, "error: expected") {
			return "fail"
		}
	}
	return "ok"
}

func passFail(r *CommandResult) string {
	if r.OK() {
		return "pass"
	}
	return "fail"
}

func writeMutantLog(dir, name string, r *CommandResult) string {
	path := filepath.Join(dir, name+".log")
	body := fmt.Sprintf("$ %s\nexit %d  duration %s  timed_out=%v unavailable=%v\n\n%s\n",
		strings.Join(r.Argv, " "), r.ExitCode, r.Duration.Round(time.Millisecond),
		r.TimedOut, r.Unavailable, r.Combined())
	if os.WriteFile(path, []byte(body), 0o644) != nil {
		return ""
	}
	return filepath.ToSlash(path)
}

// TripwireOutcome is the result of proving one hidden file actually executes.
type TripwireOutcome struct {
	File   string `json:"file"`
	Fired  bool   `json:"fired"`
	Detail string `json:"detail,omitempty"`
}

// VerifyHiddenSuiteRuns proves the hidden suite was executed rather than merely
// compiled and reported.
//
// For each hidden source file it appends a test that cannot pass and confirms
// the rung goes red. A file whose tripwire does not fire is unreachable from
// hidden/root.zig, which means its invariants were never checked -- exactly the
// failure that motivated this control, and one that has happened twice in the
// source material this fixture is modelled on.
func VerifyHiddenSuiteRuns(ctx context.Context, f *config.Fixture, workDir string) ([]TripwireOutcome, error) {
	dir := filepath.Join(workDir, "tripwire")
	wt, err := Stage(ctx, f, dir, true)
	if err != nil {
		return nil, err
	}
	if err := wt.InjectHidden(ctx); err != nil {
		return nil, err
	}

	// The rung must be green before a tripwire means anything.
	if base := Run(ctx, dir, f.Commands.Hidden, f.CommandTimeout()); !base.OK() {
		return nil, fmt.Errorf("tripwire baseline is already red; fix the reference or the hidden suite first:\n%s",
			strings.TrimSpace(base.Combined()))
	}

	entries, err := os.ReadDir(filepath.Join(dir, "hidden"))
	if err != nil {
		return nil, err
	}
	var out []TripwireOutcome
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zig") {
			continue
		}
		path := filepath.Join(dir, "hidden", e.Name())
		orig, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		tripwire := string(orig) + "\ntest \"agentbench tripwire\" {\n" +
			"    try @import(\"std\").testing.expect(false);\n}\n"
		if err := os.WriteFile(path, []byte(tripwire), 0o644); err != nil {
			return nil, err
		}
		r := Run(ctx, dir, f.Commands.Hidden, f.CommandTimeout())
		oc := TripwireOutcome{File: "hidden/" + e.Name(), Fired: !r.OK()}
		if !oc.Fired {
			oc.Detail = "the rung stayed green with a failing test in this file; it is not reached from hidden/root.zig"
		}
		out = append(out, oc)
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			return nil, err
		}
	}
	return out, nil
}
