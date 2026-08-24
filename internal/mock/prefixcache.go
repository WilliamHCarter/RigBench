package mock

import (
	"strings"
	"sync"
)

// PrefixCache is a deterministic simulation of a served prefix cache.
//
// It remembers the prompts it has already been shown and, for each new prompt,
// reports the longest common byte prefix with any of them. That is a crude but
// honest model of what a real KV prefix cache can reuse, and it is enough to
// make a prompt-layout A/B produce a difference the runner can read:
//
//   - a cache-friendly layout appends to a stable head, so turn N shares
//     essentially all of turn N-1's bytes;
//   - a layout that leads with the volatile objective diverges near byte zero,
//     so almost nothing is reusable however large the shared material is.
//
// This is a *simulation* and every engine config pointing at the mock says so.
// A real engine reports its own hit counts and the adapter reads those instead;
// nothing here is ever presented as a measurement of a real cache.
type PrefixCache struct {
	mu    sync.Mutex
	seen  []string
	limit int
}

func NewPrefixCache(limit int) *PrefixCache {
	if limit <= 0 {
		limit = 16
	}
	return &PrefixCache{limit: limit}
}

// Observe records a prompt and returns the number of bytes it shared with the
// longest matching previously-seen prompt.
//
// Block granularity is deliberate: a real cache reuses whole KV blocks, so a
// single differing byte invalidates the block containing it rather than only
// that byte. `blockBytes` approximates that, and means a one-character change
// near the head cannot report a 99% hit.
func (c *PrefixCache) Observe(prompt string, blockBytes int) int {
	if blockBytes <= 0 {
		blockBytes = 256
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	best := 0
	for _, prev := range c.seen {
		if n := commonPrefix(prev, prompt); n > best {
			best = n
		}
	}
	best -= best % blockBytes

	c.seen = append(c.seen, prompt)
	if len(c.seen) > c.limit {
		c.seen = c.seen[len(c.seen)-c.limit:]
	}
	return best
}

// Reset drops every remembered prompt. A cold run must start from a cache that
// genuinely holds nothing; "cold" is never simulated by waiting.
func (c *PrefixCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = nil
}

func commonPrefix(a, b string) int {
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

// concatMessages rebuilds the byte string the cache compares. It mirrors the
// runner's canonical form closely enough for a prefix comparison; it does not
// need to match it exactly, because the cache is a simulation of the server's
// own view rather than a second implementation of the prompt hash.
func concatMessages(msgs []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteByte('\n')
		b.WriteString(m.Content)
		b.WriteByte(0x1e)
	}
	return b.String()
}
