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

// --- multi-turn tail -----------------------------------------------------

// friendlySpec mirrors configs/layouts/builder-cache-friendly.json: stable
// first, and a message boundary at every stability change.
func friendlySpec() LayoutSpec {
	return LayoutSpec{
		ID: "cache-friendly", EnforceStabilityOrder: true, Coalesce: ByRoleAndStability,
		Blocks: []SlotSpec{
			{Source: SlotAgentContract, Stability: Stable, Role: System},
			{Source: SlotStory, Stability: Session, Role: User},
			{Source: SlotObjective, Stability: Volatile, Role: User},
			{Source: SlotTurns, Stability: Volatile, Role: User, Optional: true},
		},
	}
}

// currentSpec mirrors configs/layouts/builder-current.json: the volatile
// objective leads and everything else follows it inside one user message.
func currentSpec() LayoutSpec {
	return LayoutSpec{
		ID: "current", Coalesce: ByRole,
		Blocks: []SlotSpec{
			{Source: SlotAgentContract, Stability: Stable, Role: System},
			{Source: SlotObjective, Stability: Volatile, Role: User},
			{Source: SlotStory, Stability: Session, Role: User},
			{Source: SlotTurns, Stability: Volatile, Role: User, Optional: true},
		},
	}
}

var srcs = Sources{
	SlotAgentContract: "contract",
	SlotStory:         "story",
	SlotObjective:     "objective 0",
}

func tailTo(turn int) []TailBlock {
	var out []TailBlock
	for i := 0; i < turn; i++ {
		out = append(out,
			TailBlock{ID: "a", Role: Assistant, Text: "assistant " + string(rune('0'+i))},
			TailBlock{ID: "r", Role: User, Text: "TEST_RESULT " + string(rune('0'+i))},
			TailBlock{ID: "o", Role: User, Text: "objective " + string(rune('1'+i))},
		)
	}
	return out
}

func buildTurn(t *testing.T, sp LayoutSpec, turn int) *Manifest {
	t.Helper()
	blocks, err := Resolve(sp, srcs, tailTo(turn), tok)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Build(blocks, BuildOptions{
		Layout: sp.ID, EnforceStabilityOrder: sp.EnforceStabilityOrder,
		Coalesce: sp.Coalesce, Tokenizer: tok,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The v0.2 acceptance property, at the unit level: every turn is a byte-exact
// append of the one before it, under both coalescing modes.
func TestEveryTurnIsAnAppendOfThePrevious(t *testing.T) {
	for _, sp := range []LayoutSpec{friendlySpec(), currentSpec()} {
		var prev *Manifest
		for turn := 0; turn < 4; turn++ {
			m := buildTurn(t, sp, turn)
			if err := m.AppendsOnto(prev); err != nil {
				t.Fatalf("layout %s turn %d: %v", sp.ID, turn, err)
			}
			if prev != nil && m.PromptBytes <= prev.PromptBytes {
				t.Fatalf("layout %s turn %d did not grow", sp.ID, turn)
			}
			prev = m
		}
	}
}

// Pairing a stable-first ordering with by-role coalescing merges the volatile
// tail into the stable body, silently destroying the property being measured.
// That combination must be refused, not quietly reported as a zero.
func TestStableFirstOrderingWithByRoleCoalescingIsRefused(t *testing.T) {
	sp := friendlySpec()
	sp.Coalesce = ByRole
	blocks, err := Resolve(sp, srcs, tailTo(1), tok)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(blocks, BuildOptions{Layout: sp.ID, Coalesce: ByRole, Tokenizer: tok})
	if err == nil {
		t.Fatal("an invalid ordering/coalescing pair was accepted")
	}
	if !strings.Contains(err.Error(), "not a byte prefix") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// The layouts differ in cross-task reuse, not in turn-to-turn reuse: within one
// story a volatile-first layout still appends cleanly, because its leading
// objective does not change between turns. Pinned so the A/B is not read as
// claiming more than it shows.
func TestCurrentLayoutStillAppendsWithinAStory(t *testing.T) {
	a := buildTurn(t, currentSpec(), 1)
	b := buildTurn(t, currentSpec(), 2)
	if err := b.AppendsOnto(a); err != nil {
		t.Fatalf("the current layout failed to append within one story: %v", err)
	}
	// And yet its reusable prefix is far smaller than the cache-friendly one's.
	friendly := buildTurn(t, friendlySpec(), 2)
	if b.StablePrefixBytes >= friendly.StablePrefixBytes {
		t.Fatalf("current layout prefix %d is not smaller than cache-friendly %d",
			b.StablePrefixBytes, friendly.StablePrefixBytes)
	}
}

// The tail keeps each message's own role: a replayed assistant turn must not be
// flattened into the user's message, or the model sees a transcript in which it
// never spoke.
func TestTailPreservesAssistantTurns(t *testing.T) {
	m := buildTurn(t, friendlySpec(), 2)
	var roles []Role
	for _, msg := range m.Messages {
		roles = append(roles, msg.Role)
	}
	assistants := 0
	for _, r := range roles {
		if r == Assistant {
			assistants++
		}
	}
	if assistants != 2 {
		t.Fatalf("want 2 assistant messages, got %d in %v", assistants, roles)
	}
}

// A "turns" slot the layout marked optional must be allowed to be empty on
// turn 0, and a required slot must not.
func TestOptionalTurnsSlotIsEmptyOnTurnZero(t *testing.T) {
	m := buildTurn(t, friendlySpec(), 0)
	if len(m.Blocks) != 3 {
		t.Fatalf("want three blocks on turn 0, got %d", len(m.Blocks))
	}
}

func TestSameContentAcceptsAReorderAndRejectsAReword(t *testing.T) {
	friendly := buildTurn(t, friendlySpec(), 2)

	// Same bytes, different order and message boundaries.
	reordered := LayoutSpec{
		ID: "current", Coalesce: ByRole,
		Blocks: []SlotSpec{
			{Source: SlotObjective, Stability: Volatile, Role: User},
			{Source: SlotStory, Stability: Session, Role: User},
			{Source: SlotAgentContract, Stability: Stable, Role: System},
			{Source: SlotTurns, Stability: Volatile, Role: User, Optional: true},
		},
	}
	blocks, err := Resolve(reordered, srcs, tailTo(2), tok)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Build(blocks, BuildOptions{Layout: "current", Coalesce: ByRole, Tokenizer: tok})
	if err != nil {
		t.Fatal(err)
	}
	if err := SameContent(friendly, other); err != nil {
		t.Fatalf("a pure reorder was rejected: %v", err)
	}

	// One reworded block must be caught, or a layout A/B could smuggle in a
	// prompt change and call the result a layout effect.
	reworded := Sources{
		SlotAgentContract: "contract",
		SlotStory:         "story, but reworded",
		SlotObjective:     "objective 0",
	}
	b2, err := Resolve(friendlySpec(), reworded, tailTo(2), tok)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Build(b2, BuildOptions{Layout: "reworded", Coalesce: ByRoleAndStability, Tokenizer: tok})
	if err != nil {
		t.Fatal(err)
	}
	if err := SameContent(friendly, m2); err == nil {
		t.Fatal("a reworded block passed as the same content")
	}
}

// The stable prefix must not move as turns are appended: that is the span a
// served cache reuses, and a layout whose head drifted per turn would reuse
// nothing however stable its content looked.
func TestStablePrefixIsConstantAcrossTurns(t *testing.T) {
	first := buildTurn(t, friendlySpec(), 0)
	for turn := 1; turn < 4; turn++ {
		m := buildTurn(t, friendlySpec(), turn)
		if m.StablePrefixSHA256 != first.StablePrefixSHA256 {
			t.Fatalf("turn %d moved the stable prefix", turn)
		}
		if m.StablePrefixBytes != first.StablePrefixBytes {
			t.Fatalf("turn %d changed the stable prefix length", turn)
		}
	}
}
