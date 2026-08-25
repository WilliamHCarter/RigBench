// Package executor stages a resettable copy of the fixture repository, applies
// a candidate patch, and runs the build and test rungs.
//
// Three rules shape this package:
//
//   - The staged repository is a git repository with one commit at the frozen
//     HEAD. That makes `git apply` the patch tool, `git status --porcelain` the
//     authoritative list of changed files (creations and deletions included),
//     and reset an operation with no ambiguity.
//   - Every command's stdout and stderr is captured and kept, including for
//     failures. A failed measurement is evidence.
//   - A command that could not run is reported as skipped, never as passed. A
//     timeout is a benchmark failure and is never retried into green.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/config"
)

// CommandResult is one external command and everything it produced.
type CommandResult struct {
	Argv     []string      `json:"argv"`
	Dir      string        `json:"dir"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration_ns"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	// TimedOut separates "the rung failed" from "the rung did not finish".
	TimedOut bool `json:"timed_out"`
	// Unavailable is set when the command itself could not be started, for
	// example a missing toolchain. Such a rung is skipped, not failed.
	Unavailable bool   `json:"unavailable"`
	Err         string `json:"error,omitempty"`
}

func (r *CommandResult) OK() bool {
	return r != nil && r.ExitCode == 0 && !r.TimedOut && !r.Unavailable
}

func (r *CommandResult) Combined() string {
	if r.Stderr == "" {
		return r.Stdout
	}
	return r.Stdout + "\n--- stderr ---\n" + r.Stderr
}

// Run executes argv in dir with a hard timeout.
func Run(ctx context.Context, dir string, argv []string, timeout time.Duration) *CommandResult {
	return RunEnv(ctx, dir, argv, nil, timeout)
}

// RunEnv is Run with additional environment variables, which is how an engine
// config's tuning axes reach a preparation hook without the harness knowing
// what any of them mean.
func RunEnv(ctx context.Context, dir string, argv, env []string, timeout time.Duration) *CommandResult {
	res := &CommandResult{Argv: argv, Dir: dir}
	if len(argv) == 0 {
		res.Unavailable = true
		res.Err = "empty command"
		return res
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		res.Unavailable = true
		res.Err = fmt.Sprintf("%s not found on PATH: %v", argv[0], err)
		return res
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	// A deterministic, minimal environment: a benchmark whose result depends on
	// the operator's shell is not reproducible.
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "CLICOLOR=0", "TERM=dumb")
	cmd.Env = append(cmd.Env, env...)

	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb

	start := time.Now()
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout = out.String()
	res.Stderr = errb.String()

	if cctx.Err() != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		res.Err = fmt.Sprintf("timed out after %s", timeout)
		return res
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Err = err.Error()
		}
	}
	return res
}

// --- staging -------------------------------------------------------------

// Worktree is a staged, resettable copy of the fixture repository.
type Worktree struct {
	Dir     string
	fixture *config.Fixture
}

// Stage copies the frozen repository into dir and commits it, so the baseline
// is a real git tree.
//
// It does NOT inject the hidden suite. That is InjectHidden, called later and
// exactly once per story, so a candidate's own build never compiles the oracle.
//
// withReference overlays the reference solution, which is how the fixture
// verifies itself: the mutant controls and the gold path both start from here.
func Stage(ctx context.Context, f *config.Fixture, dir string, withReference bool) (*Worktree, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := copyTree(f.Path(f.RepoDir), dir, skipBuildArtifacts); err != nil {
		return nil, fmt.Errorf("stage repo: %w", err)
	}
	if withReference {
		if err := copyTree(f.Path(f.ReferenceDir), dir, nil); err != nil {
			return nil, fmt.Errorf("stage reference: %w", err)
		}
	}

	wt := &Worktree{Dir: dir, fixture: f}
	for _, argv := range [][]string{
		{"git", "init", "--quiet", "-b", "fixture"},
		{"git", "config", "user.email", "agentbench@localhost"},
		{"git", "config", "user.name", "AgentBench"},
		{"git", "config", "core.autocrlf", "false"},
		{"git", "add", "-A"},
		{"git", "commit", "--quiet", "-m", "frozen fixture HEAD"},
	} {
		if argv[1] == "add" {
			// Build artifacts must be invisible to git before anything is
			// added. The live lane builds inside this worktree repeatedly, so
			// `zig build` leaves .zig-cache/ behind and the scope gate would
			// report thirty out-of-scope files that the candidate never wrote.
			//
			// Written to .git/info/exclude rather than to a .gitignore in the
			// tree: a .gitignore would be a fixture byte the candidate could
			// see, diff against, and be blamed for.
			if err := writeGitExclude(dir); err != nil {
				return nil, err
			}
		}
		if r := Run(ctx, dir, argv, 2*time.Minute); !r.OK() {
			return nil, fmt.Errorf("stage: %v failed (exit %d): %s",
				argv, r.ExitCode, strings.TrimSpace(r.Combined()))
		}
	}
	return wt, nil
}

// writeGitExclude hides build artifacts from git inside a staged worktree.
func writeGitExclude(dir string) error {
	info := filepath.Join(dir, ".git", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return err
	}
	const body = "# Written by AgentBench when staging this worktree.\n" +
		"# Build artifacts are not candidate changes and must not read as\n" +
		"# out-of-scope edits when the live lane builds in place.\n" +
		".zig-cache/\nzig-out/\n.agentbench-candidate.patch\n"
	return os.WriteFile(filepath.Join(info, "exclude"), []byte(body), 0o644)
}

// InjectHidden replaces the placeholder hidden suite with the real one. It is a
// separate step from Stage so a run can record the prompt and the candidate
// patch before the oracle is ever present in the tree.
func (w *Worktree) InjectHidden(ctx context.Context) error {
	dst := filepath.Join(w.Dir, "hidden")
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			if err := os.Remove(filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
	}
	return copyTree(w.fixture.Path(w.fixture.HiddenDir), dst, nil)
}

// Reset returns the worktree to the frozen HEAD, discarding everything a
// candidate did. Between-run contamination is the one failure that would
// invalidate every number in the report, so this is `reset --hard` plus a
// `clean -fdx` and it is verified.
func (w *Worktree) Reset(ctx context.Context) error {
	for _, argv := range [][]string{
		{"git", "reset", "--hard", "--quiet", "HEAD"},
		{"git", "clean", "-fdxq"},
	} {
		if r := Run(ctx, w.Dir, argv, 2*time.Minute); !r.OK() {
			return fmt.Errorf("reset: %v failed: %s", argv, strings.TrimSpace(r.Combined()))
		}
	}
	dirty, err := w.ChangedFiles(ctx)
	if err != nil {
		return err
	}
	if len(dirty) != 0 {
		return fmt.Errorf("reset left %d changed files: %v", len(dirty), dirty)
	}
	return nil
}

// Commit records the current tree as a new baseline, so a later Diff describes
// changes made after this point rather than since the frozen HEAD.
func (w *Worktree) Commit(ctx context.Context, msg string) error {
	for _, argv := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "--quiet", "--allow-empty", "-m", msg},
	} {
		if r := Run(ctx, w.Dir, argv, 2*time.Minute); !r.OK() {
			return fmt.Errorf("commit: %v failed: %s", argv, strings.TrimSpace(r.Combined()))
		}
	}
	return nil
}

// ChangedFiles lists repo-relative paths that differ from the frozen HEAD,
// sorted. Renames are reported as both sides so a scope check cannot be evaded
// by moving a file.
func (w *Worktree) ChangedFiles(ctx context.Context) ([]string, error) {
	r := Run(ctx, w.Dir, []string{"git", "status", "--porcelain=v1", "-uall", "-z"}, time.Minute)
	if !r.OK() {
		return nil, fmt.Errorf("git status failed: %s", strings.TrimSpace(r.Combined()))
	}
	var out []string
	for _, rec := range strings.Split(r.Stdout, "\x00") {
		if len(rec) < 4 {
			continue
		}
		path := rec[3:]
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

// Diff returns the unified diff of the worktree against the frozen HEAD.
func (w *Worktree) Diff(ctx context.Context) (string, error) {
	r := Run(ctx, w.Dir, []string{"git", "add", "-A", "-N"}, time.Minute)
	if !r.OK() {
		return "", fmt.Errorf("git add -N failed: %s", strings.TrimSpace(r.Combined()))
	}
	d := Run(ctx, w.Dir, []string{"git", "diff", "--no-color", "HEAD"}, time.Minute)
	if !d.OK() {
		return "", fmt.Errorf("git diff failed: %s", strings.TrimSpace(d.Combined()))
	}
	return d.Stdout, nil
}

// ApplyPatch writes the candidate patch and applies it. A patch that does not
// apply cleanly is a failed gate, not something to coerce with fuzz: the
// benchmark measures whether a model produced an applicable diff.
func (w *Worktree) ApplyPatch(ctx context.Context, patch string) *CommandResult {
	// The name is passed bare, not joined: the command's working directory is
	// the worktree, so a repo-relative path would be resolved a second time.
	const name = ".agentbench-candidate.patch"
	path := filepath.Join(w.Dir, name)
	if err := os.WriteFile(path, []byte(patch), 0o644); err != nil {
		return &CommandResult{Unavailable: true, Err: err.Error()}
	}
	defer os.Remove(path)
	return Run(ctx, w.Dir, []string{
		"git", "apply", "--whitespace=nowarn", "-p1", "--verbose", name,
	}, 2*time.Minute)
}

// ApplyEdits performs literal find/replace edits. This is how mutants are
// applied: an exact-string edit cannot drift against patch context, and the
// edit itself is a readable description of the defect.
//
// Each find string must occur exactly once, so an edit that would apply in two
// places is an error rather than an ambiguity resolved by luck.
func (w *Worktree) ApplyEdits(edits []Edit) error {
	for i, e := range edits {
		p := filepath.Join(w.Dir, e.File)
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("edit %d: %w", i, err)
		}
		s := string(b)
		if n := strings.Count(s, e.Find); n != 1 {
			return fmt.Errorf("edit %d in %s: find string occurs %d times, want exactly 1",
				i, e.File, n)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(s, e.Find, e.Replace, 1)), 0o644); err != nil {
			return fmt.Errorf("edit %d: %w", i, err)
		}
	}
	return nil
}

type Edit struct {
	File    string `json:"file"`
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

// ZigVersion asks the toolchain which compiler will actually run in this tree.
// Under anyzig the answer comes from build.zig.zon, which is why it must be
// asked inside the worktree and not globally.
func ZigVersion(ctx context.Context, dir string) string {
	r := Run(ctx, dir, []string{"zig", "version"}, 2*time.Minute)
	if !r.OK() {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

// --- output parsing ------------------------------------------------------

var (
	zigPassFail = regexp.MustCompile(`(\d+) pass(?:ed)?(?:, (\d+) fail(?:ed)?)?`)
	zigSummary  = regexp.MustCompile(`(\d+)/(\d+) tests passed`)
)

// ParseZigTestCounts extracts a passing-test count from `zig build` output.
//
// It returns nil rather than zero when it cannot find a count. A zero would be
// indistinguishable from a suite that ran and passed nothing, and the whole
// point of the anti-vacuity discipline is that those are different.
func ParseZigTestCounts(out string) (passed *int, failed *int) {
	if m := zigSummary.FindStringSubmatch(out); m != nil {
		p, err1 := strconv.Atoi(m[1])
		total, err2 := strconv.Atoi(m[2])
		if err1 == nil && err2 == nil {
			f := total - p
			return &p, &f
		}
	}
	var sumP, sumF int
	found := false
	for _, m := range zigPassFail.FindAllStringSubmatch(out, -1) {
		p, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		found = true
		sumP += p
		if m[2] != "" {
			if f, err := strconv.Atoi(m[2]); err == nil {
				sumF += f
			}
		}
	}
	if !found {
		return nil, nil
	}
	return &sumP, &sumF
}

// --- file copying --------------------------------------------------------

func skipBuildArtifacts(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".zig-cache" || seg == "zig-out" || seg == ".git" {
			return true
		}
	}
	return false
}

func copyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
