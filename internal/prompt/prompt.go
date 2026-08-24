// Package prompt turns fixture content into a request payload deterministically.
//
// Prompt layout is a benchmark *input*, not an incidental prompting choice. Two
// rules make the layout measurable:
//
//  1. Stable information precedes volatile information. Nothing that changes
//     between runs -- a timestamp, a run id, a hostname -- may appear before
//     the reusable prefix, and the serializer refuses a manifest that violates
//     this rather than trusting the caller.
//  2. Later turns append. Turn N's serialization is byte-identical to turn
//     N-1's followed by new bytes, which is what makes prefix-reuse telemetry
//     interpretable at all.
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Stability classifies a block by how often its bytes change. The ordering of
// these constants is the ordering the serializer enforces.
type Stability int

const (
	// Stable across every request the benchmark makes: agent contract, repo
	// doctrine, tool contracts.
	Stable Stability = iota
	// Stable for one builder or reviewer session: story, scope, invariants,
	// definition of done, initial source context.
	Session
	// Changes per turn: current objective, tool results, test output.
	Volatile
)

func (s Stability) String() string {
	switch s {
	case Stable:
		return "stable"
	case Session:
		return "session"
	case Volatile:
		return "volatile"
	}
	return "unknown"
}

// Role is the chat role a block is emitted under.
type Role string

const (
	System    Role = "system"
	User      Role = "user"
	Assistant Role = "assistant"
)

// Block is one addressable span of prompt text.
type Block struct {
	ID            string    `json:"id"`
	Stability     Stability `json:"-"`
	StabilityName string    `json:"stability"`
	Role          Role      `json:"role"`
	SHA256        string    `json:"sha256"`
	Bytes         int       `json:"bytes"`
	Tokens        int       `json:"tokens_estimated"`

	text string
}

// Text returns the block's normalized content.
func (b Block) Text() string { return b.text }

// NewBlock normalizes line endings, trims trailing whitespace on each line, and
// hashes the result. Once a fixture is frozen these bytes must not move, so all
// normalization happens exactly here.
func NewBlock(id string, st Stability, role Role, text string, tok Tokenizer) Block {
	n := Normalize(text)
	sum := sha256.Sum256([]byte(n))
	return Block{
		ID:            id,
		Stability:     st,
		StabilityName: st.String(),
		Role:          role,
		SHA256:        hex.EncodeToString(sum[:]),
		Bytes:         len(n),
		Tokens:        tok.Count(n),
		text:          n,
	}
}

// Normalize is the single normalization rule for every byte the benchmark sends:
// CRLF and CR become LF, trailing whitespace is stripped per line, and the text
// ends in exactly one newline. Applying it twice is the same as applying it once.
func Normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s = strings.Join(lines, "\n")
	s = strings.TrimRight(s, "\n")
	return s + "\n"
}

// Message is one OpenAI-compatible chat message.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// Manifest is the record of exactly what was serialized. It is written beside
// every request so a prompt can be reconstructed and re-hashed later.
type Manifest struct {
	Schema string `json:"schema"`
	Layout string `json:"layout"`

	Coalesce               string `json:"coalesce"`
	StabilityOrderEnforced bool   `json:"stability_order_enforced"`

	Blocks   []Block   `json:"blocks"`
	Messages []Message `json:"messages"`

	// PromptSHA256 covers the canonical serialization defined by Canonical.
	PromptSHA256 string `json:"prompt_sha256"`
	PromptBytes  int    `json:"prompt_bytes"`

	// StablePrefix covers every leading block classified Stable or Session --
	// the span a served prefix cache can reuse across turns.
	StablePrefixSHA256 string `json:"stable_prefix_sha256"`
	StablePrefixBytes  int    `json:"stable_prefix_bytes"`

	TokenizerID           string `json:"tokenizer_id"`
	PromptTokensEstimated int    `json:"prompt_tokens_estimated"`
	// TokenCountIsExact is false whenever TokenizerID is a heuristic. A context
	// variant is never labelled from an inexact count.
	TokenCountIsExact bool `json:"token_count_is_exact"`
}

// recordSeparator delimits messages in the canonical serialization. It is a
// byte that cannot occur in normalized prompt text, so the concatenation is
// unambiguous and a prompt hash cannot collide across a message boundary.
const recordSeparator = "\x1e"

// Canonical is the byte string the prompt hash is taken over. It is defined
// here and nowhere else, because two different canonicalizations produce two
// incomparable benchmarks.
func Canonical(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(string(m.Role))
		b.WriteByte('\n')
		b.WriteString(m.Content)
		b.WriteString(recordSeparator)
	}
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// CoalesceMode decides where one chat message ends and the next begins.
type CoalesceMode string

const (
	// ByRoleAndStability starts a new message whenever the role *or* the
	// stability class changes. This keeps the reusable prefix on a message
	// boundary, which is what makes its digest a genuine byte prefix of the
	// whole prompt.
	ByRoleAndStability CoalesceMode = "by_role_and_stability"
	// ByRole merges every consecutive same-role block into one message. This is
	// what an ordinary agent dispatch does, and it is offered so the current
	// layout can be measured rather than assumed bad.
	ByRole CoalesceMode = "by_role"
)

// BuildOptions carries the parts of a layout that change how bytes are laid
// out rather than what they say.
type BuildOptions struct {
	Layout string
	// EnforceStabilityOrder rejects a layout that places volatile bytes before
	// stable ones. Cache-friendly layouts set it; a layout that deliberately
	// reproduces a volatile-first dispatch does not, so that its (near zero)
	// reusable prefix can be measured instead of refused.
	EnforceStabilityOrder bool
	Coalesce              CoalesceMode
	Tokenizer             Tokenizer
}

// Build assembles blocks into messages and hashes the result.
//
// Blocks are emitted in the order given -- the layout decides the order, not
// this function. What Build guarantees is that the manifest's stable-prefix
// digest is the digest of a real byte prefix of the prompt, so a prefix-reuse
// number computed from it means something.
func Build(blocks []Block, opts BuildOptions) (*Manifest, error) {
	if opts.Tokenizer == nil {
		return nil, fmt.Errorf("prompt: no tokenizer")
	}
	if opts.Coalesce == "" {
		opts.Coalesce = ByRoleAndStability
	}
	if opts.EnforceStabilityOrder {
		if err := checkStabilityOrder(blocks); err != nil {
			return nil, err
		}
	}

	msgs := coalesce(blocks, opts.Coalesce)
	canonical := Canonical(msgs)

	// The stable prefix is the leading run of non-volatile blocks, measured over
	// the same canonical form so its digest is a genuine prefix of the whole.
	var stableRun []Block
	for _, b := range blocks {
		if b.Stability == Volatile {
			break
		}
		stableRun = append(stableRun, b)
	}
	prefixCanonical := Canonical(coalesce(stableRun, opts.Coalesce))
	if !strings.HasPrefix(canonical, prefixCanonical) {
		return nil, fmt.Errorf("prompt: layout %q claims a %d-byte reusable prefix that is "+
			"not a byte prefix of the prompt -- a volatile block was coalesced into the "+
			"same message as a stable one; use coalesce=%q or move the volatile block to "+
			"its own role", opts.Layout, len(prefixCanonical), ByRoleAndStability)
	}

	return &Manifest{
		Schema:                 "agentbench/prompt/v1",
		Layout:                 opts.Layout,
		Coalesce:               string(opts.Coalesce),
		StabilityOrderEnforced: opts.EnforceStabilityOrder,
		Blocks:                 blocks,
		Messages:               msgs,
		PromptSHA256:           sha256Hex(canonical),
		PromptBytes:            len(canonical),
		StablePrefixSHA256:     sha256Hex(prefixCanonical),
		StablePrefixBytes:      len(prefixCanonical),
		TokenizerID:            opts.Tokenizer.ID(),
		PromptTokensEstimated:  opts.Tokenizer.Count(canonical),
		TokenCountIsExact:      opts.Tokenizer.Exact(),
	}, nil
}

func checkStabilityOrder(blocks []Block) error {
	seen := Stable
	var seenID string
	for _, b := range blocks {
		if b.Stability < seen {
			return fmt.Errorf("prompt: block %q is %s but follows %s block %q; "+
				"stable information must precede volatile information",
				b.ID, b.Stability, seen, seenID)
		}
		seen, seenID = b.Stability, b.ID
	}
	return nil
}

// coalesce merges consecutive blocks into chat messages. The wire format has no
// notion of a block, so this is where the block model meets the chat protocol.
func coalesce(blocks []Block, mode CoalesceMode) []Message {
	var out []Message
	var lastStability Stability
	for i, b := range blocks {
		same := len(out) > 0 && out[len(out)-1].Role == b.Role
		if same && mode == ByRoleAndStability {
			same = b.Stability == lastStability
		}
		if same && i > 0 {
			out[len(out)-1].Content += "\n" + b.text
			lastStability = b.Stability
			continue
		}
		out = append(out, Message{Role: b.Role, Content: b.text})
		lastStability = b.Stability
	}
	return out
}

// AssertNoVolatileTokens fails if any stable or session block contains one of
// the given strings. Callers pass the run id, the run directory and the
// timestamp, which are exactly the values that must never enter a reusable
// prefix. This is checked rather than assumed because the failure mode -- a
// prefix that silently never reuses -- looks like an engine problem.
func (m *Manifest) AssertNoVolatileTokens(needles ...string) error {
	for _, b := range m.Blocks {
		if b.Stability == Volatile {
			continue
		}
		for _, n := range needles {
			if n == "" {
				continue
			}
			if strings.Contains(b.text, n) {
				return fmt.Errorf("prompt: %s block %q contains volatile string %q; "+
					"it would break prefix reuse on every run", b.Stability, b.ID, n)
			}
		}
	}
	return nil
}

// AppendsOnto reports whether m is a pure append of prev: prev's canonical
// serialization must be a byte prefix of m's. This is the v0.2 acceptance
// property and it is cheap enough to assert on every turn.
func (m *Manifest) AppendsOnto(prev *Manifest) error {
	if prev == nil {
		return nil
	}
	cur := Canonical(m.Messages)
	old := Canonical(prev.Messages)
	if !strings.HasPrefix(cur, old) {
		n := commonPrefixLen(cur, old)
		return fmt.Errorf("prompt: turn is not an append of the previous turn; "+
			"they diverge at byte %d of %d (previous prompt was %d bytes)",
			n, len(cur), len(old))
	}
	return nil
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// FileEntry records one serialized source file.
type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// SerializeFiles renders files in deterministic path order with normalized line
// endings. Paths are sorted here rather than by the caller so two fixtures that
// list the same files in different orders produce the same bytes.
func SerializeFiles(root string, paths []string) (string, []FileEntry, error) {
	sorted := make([]string, len(paths))
	copy(sorted, paths)
	sort.Strings(sorted)

	var b strings.Builder
	entries := make([]FileEntry, 0, len(sorted))
	for _, p := range sorted {
		raw, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return "", nil, fmt.Errorf("serialize %s: %w", p, err)
		}
		n := Normalize(string(raw))
		entries = append(entries, FileEntry{Path: p, SHA256: sha256Hex(n), Bytes: len(n)})
		fmt.Fprintf(&b, "--- FILE: %s ---\n%s\n", p, n)
	}
	return b.String(), entries, nil
}
