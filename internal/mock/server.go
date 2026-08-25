// Package mock is a deterministic OpenAI-compatible streaming server.
//
// It exists so the whole benchmark -- prompt serialization, streaming timing,
// patch extraction, apply, build, hidden tests, scope, reporting -- can be
// exercised end to end without an inference rig, and so the fixture's central
// claim can be demonstrated on any machine: a correct patch goes green and a
// planted broken one does not.
//
// Its timings are a *fixture*, not a measurement. They are shaped from the
// recorded AR and DFlash2 numbers so the plumbing is exercised at a realistic
// scale, and every engine config that points here says so in its description.
// No number produced against this server may appear in a champion decision.
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Profile is a synthetic engine timing shape.
type Profile struct {
	Name string
	// PrefillTokS and DecodeTokS drive the simulated delays.
	PrefillTokS float64
	DecodeTokS  float64
	// BaseTTFTMS is fixed per-request overhead before the first token.
	BaseTTFTMS float64
	// Speculation, when set, is published as Hipfire-shaped telemetry so the
	// vendor adapter path is exercised rather than assumed.
	Speculation *Speculation
}

type Speculation struct {
	Mode       string
	Tau        float64
	AcceptRate float64
	Block      int
}

// Profiles are named after the measured paths they are shaped from.
var Profiles = map[string]Profile{
	"ar": {
		Name: "ar", PrefillTokS: 414.5, DecodeTokS: 34.0, BaseTTFTMS: 57.9,
	},
	"dflash2": {
		Name: "dflash2", PrefillTokS: 414.5, DecodeTokS: 105.08, BaseTTFTMS: 57.9,
		Speculation: &Speculation{Mode: "dflash2", Tau: 6.39, AcceptRate: 0.426, Block: 8},
	},
}

// RequestInfo is what a responder may condition on.
//
// AssistantTurns is derived from the request rather than from a counter on the
// server, so a scripted multi-turn responder stays correct when several stories
// share one mock process -- which they do, since engines are compared against
// the same endpoint.
type RequestInfo struct {
	PromptTokens   int
	AssistantTurns int
	Model          string
}

// Responder decides what text the mock returns for a request. The runner never
// sees how it was chosen, so the mock is a stand-in for a model and not a
// back channel into the scoring path.
type Responder func(RequestInfo) (visible string, reasoning string)

type Server struct {
	// ProfileFor picks a timing profile from the requested model alias or from
	// the X-AgentBench-Profile header. Defaults to "ar".
	ProfileFor func(r *http.Request, model string) Profile
	Respond    Responder
	// TimeScale multiplies every simulated delay. 1.0 reproduces the recorded
	// throughputs; a small value makes a self-test fast. It is recorded in the
	// response so a fast run cannot be mistaken for a measurement.
	TimeScale float64

	// Cache, when set, simulates a served prefix cache: cached prompt bytes are
	// reported as hit tokens and are not charged prefill time. Leave it nil to
	// serve a cacheless endpoint, which is what a cold run wants.
	Cache *PrefixCache
	// CacheBlockBytes is the granularity a simulated cache reuses at.
	CacheBlockBytes int

	mu       sync.Mutex
	requests int
}

func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

type wireRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream             bool           `json:"stream"`
	ReasoningEffort    string         `json:"reasoning_effort"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
	StreamOptions      *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens int `json:"max_tokens"`
}

// Handler returns the HTTP handler. Only the one endpoint the benchmark's
// portable baseline needs is served; anything else is a 404, so an accidental
// dependency on a vendor route fails loudly here rather than on the rig.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.completions)
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "mock-qwen3.8-27b-fast", "object": "model"},
		}})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return mux
}

func (s *Server) completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req wireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !req.Stream {
		http.Error(w, "this mock only serves stream=true, which is the benchmark's "+
			"portable baseline", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	prof := Profiles["ar"]
	if s.ProfileFor != nil {
		prof = s.ProfileFor(r, req.Model)
	}
	scale := s.TimeScale
	if scale <= 0 {
		scale = 1
	}

	promptTokens := 0
	for _, m := range req.Messages {
		promptTokens += approxTokens(m.Content)
	}

	// Simulated prefix reuse. Hit bytes are converted to tokens with the same
	// crude ratio the prompt count uses, so hit + miss adds up to the prompt
	// count this server reports and a reader can check that it does.
	hitTokens, missTokens := 0, promptTokens
	if s.Cache != nil {
		whole := concatMessages(req.Messages)
		hitBytes := s.Cache.Observe(whole, s.CacheBlockBytes)
		if len(whole) > 0 {
			hitTokens = promptTokens * hitBytes / len(whole)
		}
		if hitTokens > promptTokens {
			hitTokens = promptTokens
		}
		missTokens = promptTokens - hitTokens
	}

	assistantTurns := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			assistantTurns++
		}
	}

	visible, reasoning := "", ""
	if s.Respond != nil {
		visible, reasoning = s.Respond(RequestInfo{
			PromptTokens: promptTokens, AssistantTurns: assistantTurns, Model: req.Model,
		})
	}

	// Reasoning is emitted unless the request actually asked for it to stop.
	// A server that silently produced no reasoning would make a broken
	// "thinking: off" config look correct, so the mock behaves like a model
	// whose default is to think.
	if !thinkingDisabled(req) {
		reasoning = defaultReasoning
	} else {
		reasoning = ""
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-AgentBench-Mock-Profile", prof.Name)
	w.Header().Set("X-AgentBench-Mock-Time-Scale", fmt.Sprintf("%g", scale))
	if s.Cache != nil {
		w.Header().Set("X-AgentBench-Mock-Cache", "simulated")
	}
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()

	// Prefill: one delay before any token, sized by the prompt bytes that were
	// *not* already resident. A cache hit buys prefill time, not decode time,
	// which is the whole reason TTFT and wall are recorded separately.
	prefill := time.Duration((float64(missTokens)/prof.PrefillTokS*1000+prof.BaseTTFTMS)*scale) * time.Millisecond
	if !sleepCtx(ctx, prefill) {
		return
	}

	perToken := time.Duration(1000 / prof.DecodeTokS * scale * float64(time.Millisecond))

	reasoningTokens := 0
	for _, piece := range chunkText(reasoning) {
		reasoningTokens++
		if !sleepCtx(ctx, perToken) {
			return
		}
		writeChunk(w, flusher, map[string]any{
			"object": "chat.completion.chunk",
			"model":  req.Model,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"reasoning_content": piece},
			}},
		})
	}

	completionTokens := 0
	for _, piece := range chunkText(visible) {
		completionTokens++
		if !sleepCtx(ctx, perToken) {
			return
		}
		writeChunk(w, flusher, map[string]any{
			"object": "chat.completion.chunk",
			"model":  req.Model,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": piece},
			}},
		})
	}

	// Final chunk: finish reason, usage, and -- for a speculating profile --
	// vendor telemetry in the shape the Hipfire adapter reads.
	final := map[string]any{
		"object": "chat.completion.chunk",
		"model":  req.Model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens + reasoningTokens,
			"total_tokens":      promptTokens + completionTokens + reasoningTokens,
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": reasoningTokens,
			},
		},
	}
	timings := map[string]any{
		"prefill_tok_s": prof.PrefillTokS,
		"decode_tok_s":  prof.DecodeTokS,
	}
	if s.Cache != nil {
		timings["prefix_cache_hit_tokens"] = hitTokens
		timings["prefix_cache_miss_tokens"] = missTokens
	}
	if prof.Speculation != nil {
		timings["speculation"] = map[string]any{
			"mode":        prof.Speculation.Mode,
			"tau":         prof.Speculation.Tau,
			"accept_rate": prof.Speculation.AcceptRate,
			"block":       prof.Speculation.Block,
		}
	}
	final["timings"] = timings
	writeChunk(w, flusher, final)

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// defaultReasoning is what the mock emits when nothing disabled thinking. Its
// content does not matter; that it is non-empty does.
const defaultReasoning = "Let me work through the seam. FrameRange is half-open, " +
	"so the forward path must stop before end and the reverse path must not " +
	"step below start. Tone is cold state, so the coefficient resolves at " +
	"note-on and the frame loop reads only Hot.filter_coeff."

// thinkingDisabled reports whether the request carried a real no-think switch.
// Omitting a reasoning field is not one of them.
func thinkingDisabled(req wireRequest) bool {
	if req.ReasoningEffort == "none" {
		return true
	}
	if v, ok := req.ChatTemplateKwargs["enable_thinking"]; ok {
		if b, ok := v.(bool); ok && !b {
			return true
		}
	}
	return false
}

func writeChunk(w http.ResponseWriter, f http.Flusher, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("mock: marshal: %v", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
	f.Flush()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// chunkText splits text into stream pieces at a granularity close to a BPE
// token, so a per-token delay produces a realistic number of SSE events. It is
// deliberately not a tokenizer.
func chunkText(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	const width = 4
	for i := 0; i < len(s); i += width {
		j := i + width
		if j > len(s) {
			j = len(s)
		}
		out = append(out, s[i:j])
	}
	return out
}

func approxTokens(s string) int {
	n := len(strings.Fields(s))
	if n == 0 {
		return 1
	}
	// Roughly a token per 4 bytes, floored at the word count.
	byBytes := len(s) / 4
	if byBytes > n {
		return byBytes
	}
	return n
}

// Listen starts the server on addr and returns the listener and a shutdown
// function. Passing ":0" picks a free port, which is how a self-test avoids
// colliding with a real server.
func (s *Server) Listen(addr string) (net.Listener, func(context.Context) error, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Handler: s.Handler()}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("mock: serve: %v", err)
		}
	}()
	return ln, srv.Shutdown, nil
}
