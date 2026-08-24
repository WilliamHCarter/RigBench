package prompt

import (
	"strings"
	"testing"
)

var tok = Approx{}

func blk(id string, st Stability, role Role, text string) Block {
	return NewBlock(id, st, role, text, tok)
}

func cacheFriendly(objective string) []Block {
	return []Block{
		blk("contract", Stable, System, "global agent contract"),
		blk("doctrine", Stable, System, "repository doctrine"),
		blk("story", Session, User, "story and invariants"),
		blk("context", Session, User, "initial source context"),
		blk("objective", Volatile, User, objective),
	}
}

func TestNormalizeIsIdempotentAndEndsInOneNewline(t *testing.T) {
	for _, in := range []string{
		"a\r\nb\r\n", "a\rb", "a  \n b\t\n\n\n", "", "\n\n", "x",
	} {
		once := Normalize(in)
		twice := Normalize(once)
		if once != twice {
			t.Fatalf("not idempotent for %q: %q vs %q", in, once, twice)
		}
		if !strings.HasSuffix(once, "\n") || strings.HasSuffix(once, "\n\n") {
			t.Fatalf("want exactly one trailing newline, got %q", once)
		}
		if strings.Contains(once, "\r") {
			t.Fatalf("carriage return survived: %q", once)
		}
	}
}

func TestNormalizeStripsTrailingWhitespacePerLine(t *testing.T) {
	got := Normalize("a   \nb\t\t\nc")
	if got != "a\nb\nc\n" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildIsByteReproducible(t *testing.T) {
	a, err := Build(cacheFriendly("turn 0"), BuildOptions{
		Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(cacheFriendly("turn 0"), BuildOptions{
		Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.PromptSHA256 != b.PromptSHA256 {
		t.Fatalf("same input hashed differently: %s vs %s", a.PromptSHA256, b.PromptSHA256)
	}
	if a.PromptBytes != len(Canonical(a.Messages)) {
		t.Fatalf("PromptBytes %d disagrees with the canonical form %d",
			a.PromptBytes, len(Canonical(a.Messages)))
	}
}

// The strongest property in the package: the digest reported as reusable must
// be the digest of an actual byte prefix. A stubbed implementation returning a
// constant fails here.
func TestStablePrefixIsARealBytePrefix(t *testing.T) {
	m, err := Build(cacheFriendly("turn 0"), BuildOptions{
		Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok,
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical := Canonical(m.Messages)
	if m.StablePrefixBytes <= 0 || m.StablePrefixBytes >= len(canonical) {
		t.Fatalf("implausible prefix length %d of %d", m.StablePrefixBytes, len(canonical))
	}
	prefix := canonical[:m.StablePrefixBytes]
	if sha256Hex(prefix) != m.StablePrefixSHA256 {
		t.Fatal("StablePrefixSHA256 is not the digest of the first StablePrefixBytes bytes")
	}
}

// Changing only the volatile tail must leave the reusable prefix untouched.
func TestVolatileTailDoesNotDisturbTheStablePrefix(t *testing.T) {
	a, _ := Build(cacheFriendly("turn 0"), BuildOptions{Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok})
	b, _ := Build(cacheFriendly("a completely different objective"), BuildOptions{Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok})
	if a.StablePrefixSHA256 != b.StablePrefixSHA256 {
		t.Fatal("the reusable prefix moved when only the tail changed")
	}
	if a.PromptSHA256 == b.PromptSHA256 {
		t.Fatal("two different prompts hashed the same")
	}
}

func TestVolatileBeforeStableIsRefusedWhenEnforced(t *testing.T) {
	blocks := []Block{
		blk("objective", Volatile, User, "do the thing"),
		blk("doctrine", Stable, User, "doctrine"),
	}
	if _, err := Build(blocks, BuildOptions{Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok}); err == nil {
		t.Fatal("expected a refusal for volatile-before-stable")
	}
}

// The volatile-first layout is measurable, not refused: its reusable prefix is
// simply empty, and that is the number the A/B needs.
func TestVolatileFirstLayoutHasNoReusablePrefix(t *testing.T) {
	blocks := []Block{
		blk("objective", Volatile, User, "do the thing"),
		blk("doctrine", Stable, User, "doctrine"),
	}
	m, err := Build(blocks, BuildOptions{Layout: "current", Coalesce: ByRole, Tokenizer: tok})
	if err != nil {
		t.Fatal(err)
	}
	if m.StablePrefixBytes != 0 {
		t.Fatalf("want a zero-byte reusable prefix, got %d", m.StablePrefixBytes)
	}
	if len(m.Messages) != 1 {
		t.Fatalf("by_role should have produced one message, got %d", len(m.Messages))
	}
}

// A cache-friendly ordering coalesced by role alone would put the volatile tail
// inside the same message as the stable body, which silently destroys the very
// property being measured. That must be an error, not a quiet zero.
func TestByRoleCoalescingOfAMixedMessageIsRefused(t *testing.T) {
	blocks := []Block{
		blk("story", Session, User, "story"),
		blk("objective", Volatile, User, "objective"),
	}
	_, err := Build(blocks, BuildOptions{Layout: "l", Coalesce: ByRole, Tokenizer: tok})
	if err == nil {
		t.Fatal("expected a refusal: the claimed prefix is not a byte prefix")
	}
	if !strings.Contains(err.Error(), "not a byte prefix") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestAppendsOntoAcceptsAnAppendAndRejectsARewrite(t *testing.T) {
	t0, _ := Build(cacheFriendly("turn 0"), BuildOptions{Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok})

	appended := append(cacheFriendly("turn 0"),
		blk("result1", Volatile, Assistant, "assistant reply"),
		blk("tool1", Volatile, User, "TEST_RESULT 1"),
	)
	t1, err := Build(appended, BuildOptions{Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok})
	if err != nil {
		t.Fatal(err)
	}
	if err := t1.AppendsOnto(t0); err != nil {
		t.Fatalf("an append was rejected: %v", err)
	}

	rewritten, _ := Build(cacheFriendly("turn 0 REWRITTEN"), BuildOptions{Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok})
	if err := rewritten.AppendsOnto(t0); err == nil {
		t.Fatal("a rewrite was accepted as an append")
	}
}

func TestAssertNoVolatileTokensCatchesARunIDInTheStablePrefix(t *testing.T) {
	blocks := []Block{
		blk("contract", Stable, System, "contract for run 20260824T010203Z-abc"),
		blk("objective", Volatile, User, "run 20260824T010203Z-abc objective"),
	}
	m, err := Build(blocks, BuildOptions{Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AssertNoVolatileTokens("20260824T010203Z-abc"); err == nil {
		t.Fatal("a run id in the stable prefix was not caught")
	}
	// The same string in the volatile tail is fine.
	clean := []Block{
		blk("contract", Stable, System, "contract"),
		blk("objective", Volatile, User, "run 20260824T010203Z-abc objective"),
	}
	m2, _ := Build(clean, BuildOptions{Layout: "l", EnforceStabilityOrder: true, Tokenizer: tok})
	if err := m2.AssertNoVolatileTokens("20260824T010203Z-abc"); err != nil {
		t.Fatalf("a volatile-only run id was rejected: %v", err)
	}
}

func TestCanonicalSeparatorCannotBeForged(t *testing.T) {
	// Two different message splits must not canonicalize to the same bytes.
	a := Canonical([]Message{{User, "ab\n"}, {User, "c\n"}})
	b := Canonical([]Message{{User, "ab\nc\n"}})
	if a == b {
		t.Fatal("message boundaries are not represented in the canonical form")
	}
}

func TestApproxTokenizerIsDeterministicAndNotClaimedExact(t *testing.T) {
	if tok.Exact() {
		t.Fatal("the heuristic counter must not claim to be exact")
	}
	s := "pub fn advance(self: *Cursor, range: FrameRange) AdvanceError!void {\n"
	if tok.Count(s) != tok.Count(s) {
		t.Fatal("not deterministic")
	}
	if tok.Count(s) == 0 {
		t.Fatal("counted nothing")
	}
}
