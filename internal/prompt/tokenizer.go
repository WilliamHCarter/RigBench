package prompt

import "unicode"

// Tokenizer counts tokens for a prompt.
//
// The benchmark contract says a fixture is never labelled "8K" from a character
// count, so every implementation must answer Exact() honestly. A heuristic
// counter is useful for sizing and for spotting a prompt that grew unexpectedly;
// it is not evidence about context length, and the v0.5 context matrix requires
// an exact tokenizer for the target model before it can label a variant.
type Tokenizer interface {
	// ID names the counter in the run record.
	ID() string
	// Count returns the token count for s.
	Count(s string) int
	// Exact is true only for a real tokenizer for the model under test.
	Exact() bool
}

// Approx is a deterministic, dependency-free estimator.
//
// It splits text into runs -- alphanumeric words, whitespace, and everything
// else one character at a time -- and charges ceil(len/4) for a word run, one
// per non-space character, and one per newline. On the fixture's Zig sources
// this lands within roughly 10-15% of a BPE tokenizer, which is close enough to
// size a context pack and nowhere near close enough to label one.
type Approx struct{}

func (Approx) ID() string  { return "approx-runs-v1" }
func (Approx) Exact() bool { return false }

func (Approx) Count(s string) int {
	total := 0
	rs := []rune(s)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case r == '\n':
			total++
			i++
		case unicode.IsSpace(r):
			for i < len(rs) && unicode.IsSpace(rs[i]) && rs[i] != '\n' {
				i++
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			j := i
			for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j]) || rs[j] == '_') {
				j++
			}
			n := j - i
			total += (n + 3) / 4
			i = j
		default:
			total++
			i++
		}
	}
	return total
}
