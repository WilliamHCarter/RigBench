package executor

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Sanitize rewrites host-specific bytes out of tool output before it enters a
// prompt.
//
// This is not cosmetic. A build log carries the absolute path of the staged
// worktree, the toolchain's cache directory, runtime addresses and durations.
// Every one of those differs between machines and between runs, so leaving them
// in makes the prompt host-dependent: two engines on one rig would receive
// different bytes, the prompt hash would move on every run, and prefix reuse
// would be destroyed in a way that looks like an engine problem rather than a
// harness one.
//
// The v0.2 trajectory fixture was sanitized once, by hand, when it was frozen.
// A live loop generates its tool output at runtime, so it has to happen here.
func Sanitize(text, worktree string) string {
	if worktree != "" {
		text = strings.ReplaceAll(text, worktree+"/", "")
		text = strings.ReplaceAll(text, worktree, ".")
	}
	if cache := os.Getenv("ZIG_GLOBAL_CACHE_DIR"); cache != "" {
		text = strings.ReplaceAll(text, cache, "<zig-toolchain>")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		text = strings.ReplaceAll(text, home, "~")
	}
	for _, re := range sanitizers {
		text = re.re.ReplaceAllString(text, re.with)
	}
	return text
}

var sanitizers = []struct {
	re   *regexp.Regexp
	with string
}{
	// Toolchain and absolute paths, longest-lived first.
	{regexp.MustCompile(`~?/[^\s:]*\.cache/zig\S*`), "<zig-toolchain>"},
	{regexp.MustCompile(`/usr/lib/zig[^\s:]*`), "<zig-toolchain>"},
	{regexp.MustCompile(`/(?:Users|home|private|var|tmp)/\S+`), "<path>"},
	// Runtime detail that differs per execution.
	{regexp.MustCompile(`0x[0-9a-f]{6,}`), "0x<addr>"},
	{regexp.MustCompile(`MaxRSS:\d+M`), "MaxRSS:<rss>"},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?(ms|s)\b`), "<t>$1"},
	{regexp.MustCompile(`--seed 0x[0-9a-f]+`), "--seed 0x<seed>"},
	{regexp.MustCompile(`-Z[0-9a-f]{8,}`), "-Z<id>"},
}

// TruncateMiddle shortens output to at most n bytes total, marker included,
// keeping the head and the tail and saying how much was dropped.
//
// The head carries the first error, which is usually the real one; the tail
// carries the summary and the exit status. The middle of a long build log is
// mostly repeats of the first failure. Real agents truncate, and an
// untruncated failing build can exceed the context window -- which would end a
// run for a reason that has nothing to do with the model.
func TruncateMiddle(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	const marker = "\n\n... [%d bytes elided by the benchmark harness] ...\n\n"
	// The marker counts against the budget. A cap that is exceeded by however
	// long the explanation happens to be is not a cap, and this one feeds
	// directly into the next turn's prompt size.
	reserve := len(fmt.Sprintf(marker, len(s)))
	budget := n - reserve
	if budget < 2 {
		// No room for head, tail and an honest explanation. Say so rather than
		// returning a silently over-budget string.
		return fmt.Sprintf("... [%d bytes elided by the benchmark harness] ...", len(s))
	}
	head := budget * 2 / 3
	tail := budget - head
	dropped := len(s) - head - tail
	if dropped <= 0 {
		return s
	}
	return s[:head] + fmt.Sprintf(marker, dropped) + s[len(s)-tail:]
}
