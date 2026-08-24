package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/prompt"
)

// capture serves one canned SSE response and hands back the request body it
// received, so a claim about what was *sent* can be checked rather than assumed.
func capture(t *testing.T, req Request) (map[string]any, *Result) {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	req.Model = "m"
	req.Messages = []prompt.Message{{Role: prompt.User, Content: "hi"}}
	res := New(srv.URL, "", 10*time.Second).Complete(context.Background(), req)
	return body, res
}

// The P0 this test exists for: omitting a reasoning field is *not* the same as
// disabling reasoning. A config saying "off" must produce a request that
// actually says so.
func TestThinkingOffSendsARealSwitch(t *testing.T) {
	body, res := capture(t, Request{Thinking: "off", ThinkingOffMechanism: "chat_template_kwargs"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	kw, ok := body["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("no chat_template_kwargs in %v", body)
	}
	if v, ok := kw["enable_thinking"].(bool); !ok || v {
		t.Fatalf("enable_thinking = %v, want false", kw["enable_thinking"])
	}
	if _, present := body["reasoning_effort"]; present {
		t.Fatal("reasoning_effort was sent alongside an explicit no-think switch")
	}
}

func TestThinkingOffReasoningEffortNone(t *testing.T) {
	body, _ := capture(t, Request{Thinking: "off", ThinkingOffMechanism: "reasoning_effort_none"})
	if body["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %v", body["reasoning_effort"])
	}
	if _, present := body["chat_template_kwargs"]; present {
		t.Fatal("chat_template_kwargs was sent for the reasoning_effort mechanism")
	}
}

// "omit" must remain available, but only as a deliberate choice.
func TestThinkingOffOmitSendsNothing(t *testing.T) {
	body, _ := capture(t, Request{Thinking: "off", ThinkingOffMechanism: "omit"})
	for _, k := range []string{"reasoning_effort", "chat_template_kwargs"} {
		if _, present := body[k]; present {
			t.Fatalf("%q was sent for the omit mechanism", k)
		}
	}
}

// A request that claims no-think without saying how must fail loudly rather
// than silently degrading to "send nothing and hope".
func TestThinkingOffWithNoMechanismIsRefused(t *testing.T) {
	_, res := capture(t, Request{Thinking: "off"})
	if res.Err == nil {
		t.Fatal("an unspecified no-think mechanism was accepted")
	}
	if !strings.Contains(res.Err.Error(), "does not disable reasoning") {
		t.Fatalf("unhelpful error: %v", res.Err)
	}
}

func TestNonOffThinkingSendsReasoningEffort(t *testing.T) {
	body, _ := capture(t, Request{Thinking: "xhigh"})
	if body["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort = %v", body["reasoning_effort"])
	}
}

func TestExtraBodyIsMergedVerbatim(t *testing.T) {
	body, _ := capture(t, Request{
		Thinking:             "off",
		ThinkingOffMechanism: "omit",
		ExtraBody:            map[string]any{"vendor_knob": "abc", "n_probe": float64(3)},
	})
	if body["vendor_knob"] != "abc" || body["n_probe"] != float64(3) {
		t.Fatalf("extra body not merged: %v", body)
	}
	if body["model"] != "m" {
		t.Fatal("merging extra body damaged the typed fields")
	}
}

func TestRequestBodyIsRetainedAsEvidence(t *testing.T) {
	_, res := capture(t, Request{Thinking: "off", ThinkingOffMechanism: "chat_template_kwargs"})
	if len(res.RequestBody) == 0 {
		t.Fatal("the request bytes were not retained")
	}
	if !strings.Contains(string(res.RequestBody), "\"enable_thinking\":false") {
		t.Fatalf("retained body does not show the no-think switch: %s", res.RequestBody)
	}
}

func TestHeadersAreSent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-AgentBench-Profile")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	New(srv.URL, "", time.Second).Complete(context.Background(), Request{
		Model: "m", Thinking: "off", ThinkingOffMechanism: "omit",
		Headers: map[string]string{"X-AgentBench-Profile": "dflash2"},
	})
	if got != "dflash2" {
		t.Fatalf("header = %q", got)
	}
}

// --- telemetry adapters --------------------------------------------------

func chunks(t *testing.T, docs ...string) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(docs))
	for i, d := range docs {
		out[i] = json.RawMessage(d)
	}
	return out
}

// The generic adapter must extract nothing at all: a backend whose telemetry
// shape is unconfirmed reports "not exposed", never a plausible-looking zero.
func TestGenericAdapterExtractsNothing(t *testing.T) {
	tel := Generic{}.Extract(chunks(t, `{"timings":{"decode_tok_s":105.08,"speculation":{"tau":6.39}}}`))
	if tel.DecodeTokS != nil || tel.DFlashTau != nil || tel.PrefixCacheHitTokens != nil {
		t.Fatalf("generic adapter extracted something: %+v", tel)
	}
}

func TestHipfireAdapterReadsWhatIsThere(t *testing.T) {
	tel := Hipfire{}.Extract(chunks(t,
		`{"timings":{"prefill_tok_s":414.5,"decode_tok_s":105.08,`+
			`"prefix_cache_hit_tokens":8683,"prefix_cache_miss_tokens":1075,`+
			`"speculation":{"tau":6.39,"accept_rate":0.426,"block":8}}}`))
	if tel.PrefillTokS == nil || *tel.PrefillTokS != 414.5 {
		t.Fatalf("prefill = %v", tel.PrefillTokS)
	}
	if tel.DFlashTau == nil || *tel.DFlashTau != 6.39 {
		t.Fatalf("tau = %v", tel.DFlashTau)
	}
	if tel.PrefixCacheHitTokens == nil || *tel.PrefixCacheHitTokens != 8683 {
		t.Fatalf("hit = %v", tel.PrefixCacheHitTokens)
	}
}

// A reported zero and an absent field must not collapse into each other.
func TestHipfireAdapterKeepsZeroDistinctFromAbsent(t *testing.T) {
	zero := Hipfire{}.Extract(chunks(t, `{"timings":{"prefix_cache_hit_tokens":0}}`))
	if zero.PrefixCacheHitTokens == nil || *zero.PrefixCacheHitTokens != 0 {
		t.Fatalf("a reported zero was lost: %v", zero.PrefixCacheHitTokens)
	}
	absent := Hipfire{}.Extract(chunks(t, `{"timings":{}}`))
	if absent.PrefixCacheHitTokens != nil {
		t.Fatalf("an absent field became %v", *absent.PrefixCacheHitTokens)
	}
}

// A wrong guess at a key name must yield nil, not a fabricated number. The
// Hipfire key names are provisional, so this is the property that keeps a bad
// guess honest.
func TestHipfireAdapterIgnoresUnknownShapes(t *testing.T) {
	tel := Hipfire{}.Extract(chunks(t,
		`{"timings":{"decode_tok_s":"not a number","speculation":"not an object"}}`,
		`{"something_else":{"tau":6.39}}`))
	if tel.DecodeTokS != nil || tel.DFlashTau != nil {
		t.Fatalf("a wrongly-typed or wrongly-placed field was read: %+v", tel)
	}
}

func TestAdapterForRefusesAnUnknownName(t *testing.T) {
	if _, ok := AdapterFor("hipfyre"); ok {
		t.Fatal("a misspelled adapter silently fell back to generic")
	}
	if a, ok := AdapterFor(""); !ok || a.Name() != "generic" {
		t.Fatal("the empty adapter name should be generic")
	}
}

// The derived decode rate must not be produced when the engine reported no
// completion count, and must be measured over the streaming window only.
func TestDerivedDecodeRate(t *testing.T) {
	ttft := 100.0
	n := 90
	r := &Result{WallMS: 1100, VisibleTTFTMS: &ttft,
		Usage: &Usage{CompletionTokens: &n}}
	got := r.DerivedDecodeTokS()
	if got == nil || *got != 90 {
		t.Fatalf("got %v, want 90 tok/s over the 1000 ms streaming window", got)
	}
	if (&Result{WallMS: 1100, VisibleTTFTMS: &ttft}).DerivedDecodeTokS() != nil {
		t.Fatal("a derived rate was produced with no completion-token count")
	}
	if (&Result{WallMS: 1100, Usage: &Usage{CompletionTokens: &n}}).DerivedDecodeTokS() != nil {
		t.Fatal("a derived rate was produced with no visible TTFT")
	}
}
