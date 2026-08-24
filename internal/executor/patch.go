package executor

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ExtractPatch pulls the candidate unified diff out of a model's visible output.
//
// The return contract asks for one fenced ```diff block. Reality is messier, so
// three forms are accepted in this order of preference: a fenced block tagged
// diff or patch; a fenced block whose contents look like a unified diff; and a
// bare unified diff with no fence at all. Anything else is a failed gate.
//
// What is deliberately *not* done: no repair, no reflowing, no guessing at
// hunk headers. If the model produced something that only nearly applies, the
// benchmark's answer is that it did not produce an applicable diff.
func ExtractPatch(out string) (string, string, error) {
	out = strings.ReplaceAll(out, "\r\n", "\n")

	if p, ok := fencedByTag(out, "diff", "patch"); ok {
		return normalizePatch(p), "fenced-tagged", nil
	}
	if p, ok := fencedLookingLikeDiff(out); ok {
		return normalizePatch(p), "fenced-untagged", nil
	}
	if p, ok := bareDiff(out); ok {
		return normalizePatch(p), "bare", nil
	}
	return "", "", fmt.Errorf("no unified diff found in %d bytes of output", len(out))
}

// fence is one fenced code block found in model output.
type fence struct {
	info string
	body string
}

// scanFences finds fenced blocks without a regexp, because Go's RE2 has no
// backreference and a fence's closing delimiter must match its opening one.
// Fences of four or more backticks are supported, which is how a diff that
// itself contains a triple fence is quoted.
func scanFences(out string) []fence {
	lines := strings.Split(out, "\n")
	var found []fence
	for i := 0; i < len(lines); i++ {
		open, info, ok := fenceDelim(lines[i])
		if !ok {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			close, extra, ok := fenceDelim(lines[j])
			// A closing fence is at least as long as the opening one and carries
			// no info string.
			if ok && close >= open && strings.TrimSpace(extra) == "" {
				found = append(found, fence{
					info: strings.ToLower(strings.TrimSpace(info)),
					body: strings.Join(lines[i+1:j], "\n"),
				})
				i = j
				break
			}
		}
	}
	return found
}

// fenceDelim reports the backtick run length and info string of a fence line.
// Leading whitespace is tolerated; anything else before the backticks is not.
func fenceDelim(line string) (int, string, bool) {
	t := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(t) && t[n] == '`' {
		n++
	}
	if n < 3 {
		return 0, "", false
	}
	rest := t[n:]
	if strings.Contains(rest, "`") {
		return 0, "", false
	}
	return n, rest, true
}

func fencedByTag(out string, tags ...string) (string, bool) {
	for _, f := range scanFences(out) {
		for _, t := range tags {
			// The tag is a claim; the contents are the fact. A fence labelled
			// "diff" that holds no unified diff must fail extraction, not pass
			// extraction and then fail apply -- one defect, one gate.
			if f.info == t && looksLikeUnifiedDiff(f.body) {
				return f.body, true
			}
		}
	}
	return "", false
}

func fencedLookingLikeDiff(out string) (string, bool) {
	for _, f := range scanFences(out) {
		if looksLikeUnifiedDiff(f.body) {
			return f.body, true
		}
	}
	return "", false
}

func bareDiff(out string) (string, bool) {
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "diff --git ") ||
			(strings.HasPrefix(l, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ")) {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	body := strings.Join(lines[start:], "\n")
	if !looksLikeUnifiedDiff(body) {
		return "", false
	}
	return body, true
}

func looksLikeUnifiedDiff(s string) bool {
	hasHeader := strings.Contains(s, "diff --git ") ||
		(strings.Contains(s, "\n--- ") || strings.HasPrefix(s, "--- ")) && strings.Contains(s, "\n+++ ")
	return hasHeader && strings.Contains(s, "\n@@")
}

// normalizePatch makes the diff acceptable to `git apply`: LF endings and
// exactly one trailing newline. It does not touch hunk contents.
func normalizePatch(p string) string {
	p = strings.ReplaceAll(p, "\r\n", "\n")
	p = strings.TrimRight(p, "\n")
	if p == "" {
		return ""
	}
	return p + "\n"
}

var (
	diffGitRe = regexp.MustCompile(`(?m)^diff --git a/(\S+) b/(\S+)`)
	minusRe   = regexp.MustCompile(`(?m)^--- (?:a/)?(\S+)`)
	plusRe    = regexp.MustCompile(`(?m)^\+\+\+ (?:b/)?(\S+)`)
)

// PatchFiles lists the repo-relative paths a diff claims to touch, sorted and
// deduplicated. /dev/null is dropped, so a file creation reports only the file
// it creates.
//
// This is used for reporting. The authoritative scope check reads
// `git status` after the patch is applied, because a diff header is a claim and
// the working tree is the fact.
func PatchFiles(patch string) []string {
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || p == "/dev/null" {
			return
		}
		seen[p] = true
	}
	for _, m := range diffGitRe.FindAllStringSubmatch(patch, -1) {
		add(m[1])
		add(m[2])
	}
	if len(seen) == 0 {
		for _, m := range minusRe.FindAllStringSubmatch(patch, -1) {
			add(m[1])
		}
		for _, m := range plusRe.FindAllStringSubmatch(patch, -1) {
			add(m[1])
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
