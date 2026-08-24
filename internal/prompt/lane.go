package prompt

import (
	"fmt"
	"sort"
	"strings"
)

// Sources maps a layout's slot names to their content. The layout decides
// order and stability; this decides what each slot says. Keeping them apart is
// what makes a layout A/B a pure reordering of identical bytes.
type Sources map[string]string

// Known slot names. A layout referring to anything else is an error, so a typo
// cannot silently drop the story out of a prompt.
const (
	SlotAgentContract = "agent_contract"
	SlotDoctrine      = "doctrine"
	SlotToolContracts = "tool_contracts"
	SlotStory         = "story"
	SlotSourceContext = "source_context"
	SlotObjective     = "objective"
	SlotTurns         = "turns"
	// Reviewer slots, declared here so the v0.3 lane cannot invent a second
	// vocabulary. Unused in v0.1 and v0.2.
	SlotReviewContract = "review_contract"
	SlotCandidateDiff  = "candidate_diff"
	SlotTestEvidence   = "test_evidence"
	SlotLens           = "lens"
)

var knownSlots = map[string]bool{
	SlotAgentContract: true, SlotDoctrine: true, SlotToolContracts: true,
	SlotStory: true, SlotSourceContext: true, SlotObjective: true, SlotTurns: true,
	SlotReviewContract: true, SlotCandidateDiff: true, SlotTestEvidence: true,
	SlotLens: true,
}

// LayoutSpec is the subset of a layout config this package needs. It is
// re-declared rather than imported so internal/prompt has no dependency on
// internal/config, keeping the serializer testable in isolation.
type LayoutSpec struct {
	ID                    string
	EnforceStabilityOrder bool
	Coalesce              CoalesceMode
	Blocks                []SlotSpec
}

type SlotSpec struct {
	Source    string
	Stability Stability
	Role      Role
	Optional  bool
}

// ParseStability rejects anything outside the three declared classes.
func ParseStability(s string) (Stability, error) {
	switch s {
	case "stable":
		return Stable, nil
	case "session":
		return Session, nil
	case "volatile":
		return Volatile, nil
	}
	return 0, fmt.Errorf("unknown stability %q; want stable, session or volatile", s)
}

func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case System, User, Assistant:
		return Role(s), nil
	}
	return "", fmt.Errorf("unknown role %q", s)
}

// TailBlock is one message in the volatile multi-turn tail.
//
// The tail is a sequence rather than a single slot because a builder loop
// alternates roles: the model's turn, then the tool or test result, then the
// next objective. A layout names *where* the tail goes with a `turns` slot; the
// caller supplies what is in it.
type TailBlock struct {
	ID   string
	Role Role
	Text string
}

// Resolve turns a layout plus sources into blocks, ready for Build.
//
// A non-optional slot that resolves to nothing is an error rather than an empty
// block: a prompt missing its story would still hash, still stream, and still
// produce a plausible-looking failure.
func Resolve(spec LayoutSpec, src Sources, tail []TailBlock, tok Tokenizer) ([]Block, error) {
	var blocks []Block
	for i, slot := range spec.Blocks {
		if !knownSlots[slot.Source] {
			return nil, fmt.Errorf("layout %s block %d: unknown source %q", spec.ID, i, slot.Source)
		}
		// The tail expands in place, keeping each message's own role. It is
		// always volatile: every byte of it changes as the loop proceeds.
		if slot.Source == SlotTurns {
			for _, tb := range tail {
				blocks = append(blocks, NewBlock(tb.ID, Volatile, tb.Role, tb.Text, tok))
			}
			continue
		}
		text, ok := src[slot.Source]
		if !ok || strings.TrimSpace(text) == "" {
			if slot.Optional {
				continue
			}
			return nil, fmt.Errorf("layout %s: required source %q is empty", spec.ID, slot.Source)
		}
		blocks = append(blocks, NewBlock(slot.Source, slot.Stability, slot.Role, text, tok))
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("layout %s resolved to no blocks", spec.ID)
	}
	return blocks, nil
}

// SameContent reports whether two manifests carry the same block contents, in
// any order and under any message boundaries.
//
// This is what makes a layout A/B honest. A "cache-friendly" layout that also
// quietly reworded the story would show a gain that had nothing to do with
// layout, so the acceptance gate asserts the two layouts are a pure reordering
// of identical bytes.
func SameContent(a, b *Manifest) error {
	count := func(m *Manifest) map[string]int {
		out := map[string]int{}
		for _, blk := range m.Blocks {
			out[blk.SHA256]++
		}
		return out
	}
	ca, cb := count(a), count(b)
	var missing, extra []string
	for h, n := range ca {
		if cb[h] != n {
			missing = append(missing, blockLabel(a, h))
		}
	}
	for h, n := range cb {
		if ca[h] != n {
			extra = append(extra, blockLabel(b, h))
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Errorf("layouts %q and %q do not carry the same bytes: "+
		"only in %s: [%s]; only in %s: [%s]",
		a.Layout, b.Layout,
		a.Layout, strings.Join(missing, ", "),
		b.Layout, strings.Join(extra, ", "))
}

func blockLabel(m *Manifest, sha string) string {
	for _, blk := range m.Blocks {
		if blk.SHA256 == sha {
			return fmt.Sprintf("%s(%s)", blk.ID, sha[:8])
		}
	}
	return sha[:8]
}

// SlotNames lists the slots a layout consumes, sorted. Used by the runner to
// check up front that a fixture can satisfy a layout at all.
func (s LayoutSpec) SlotNames() []string {
	seen := map[string]bool{}
	for _, b := range s.Blocks {
		seen[b.Source] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
