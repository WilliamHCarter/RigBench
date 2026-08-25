// Package client is an OpenAI-compatible streaming chat client with a timing
// contract.
//
// The portable baseline is deliberately plain: POST /chat/completions with
// stream=true and parse text/event-stream. Nothing in this file knows which
// engine is on the other end. Engine-specific telemetry is confined to the
// Adapter interface, and the default adapter extracts nothing.
//
// Four instants are recorded separately because collapsing them hides where
// time went: the connection, the first reasoning token, the first visible
// token, and completion.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/prompt"
)

type Client struct {
	http     *http.Client
	endpoint string
	apiKey   string
}

func New(endpoint, apiKey string, timeout time.Duration) *Client {
	return &Client{
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				// A fresh connection per request keeps http_connected_ms
				// meaningful instead of silently reporting a pooled zero.
				DisableKeepAlives: true,
			},
		},
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
	}
}

type Request struct {
	Model       string
	Messages    []prompt.Message
	Temperature float64
	MaxTokens   int
	Seed        *int
	TopP        *float64
	// Thinking is passed through to the engine and is always recorded, even
	// when it is "off".
	Thinking string
	// ThinkingOffMechanism decides how "off" is expressed on the wire. There is
	// no default: omitting a reasoning parameter is not the same as disabling
	// reasoning, and a request that quietly left the model reasoning would
	// contaminate the timing, the token counts and the derived decode rate.
	ThinkingOffMechanism string
	ThinkingBudgetTokens *int
	// Tools are included only when non-empty, and are sorted by the caller.
	Tools []json.RawMessage
	// Headers are extra request headers declared by the engine config.
	Headers map[string]string
	// ExtraBody is merged into the request body verbatim.
	ExtraBody map[string]any
}

type Usage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
	// Reasoning tokens are reported under different names by different servers;
	// both are accepted and neither is invented.
	ReasoningTokens         *int `json:"reasoning_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *Usage) reasoning() *int {
	if u == nil {
		return nil
	}
	if u.ReasoningTokens != nil {
		return u.ReasoningTokens
	}
	if u.CompletionTokensDetails != nil {
		return u.CompletionTokensDetails.ReasoningTokens
	}
	return nil
}

// Result is what one streamed completion produced, including the raw chunks so
// a telemetry adapter can read fields this package does not model.
type Result struct {
	Visible   string
	Reasoning string

	HTTPStatus      int
	TransportStatus metrics.TransportStatus
	Err             error

	HTTPConnectedMS *float64
	ReasoningTTFTMS *float64
	VisibleTTFTMS   *float64
	WallMS          float64

	Usage     *Usage
	RawChunks []json.RawMessage

	OutputSHA256 string
	OutputBytes  int

	// FinishReason is the engine's own stop reason. A completion that hit its
	// token ceiling and one that stopped naturally are different results.
	FinishReason string

	// ChunkCount and StreamedBytes describe the transport itself, which is how
	// a server that buffers the whole answer into one chunk is told apart from
	// one that genuinely streams.
	ChunkCount    int
	StreamedBytes int

	// RequestBody is the exact bytes sent. Stored as an artifact so a claim
	// about what was requested -- no-think in particular -- is checkable after
	// the fact rather than inferred from a config file.
	RequestBody []byte
}

type wireRequest struct {
	Model         string            `json:"model"`
	Messages      []wireMessage     `json:"messages"`
	Temperature   float64           `json:"temperature"`
	MaxTokens     int               `json:"max_tokens"`
	Stream        bool              `json:"stream"`
	StreamOptions *streamOptions    `json:"stream_options,omitempty"`
	Seed          *int              `json:"seed,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	Tools         []json.RawMessage `json:"tools,omitempty"`
	// Reasoning controls are non-standard across servers. The benchmark sends
	// the one field it can record faithfully and records the mode either way.
	ReasoningEffort    string         `json:"reasoning_effort,omitempty"`
	MaxReasoningTokens *int           `json:"max_reasoning_tokens,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// Complete performs one streaming completion. It returns a Result even on
// failure: a failed measurement is retained, never discarded.
func (c *Client) Complete(ctx context.Context, req Request) *Result {
	res := &Result{TransportStatus: metrics.TransportOK}

	msgs := make([]wireMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = wireMessage{Role: string(m.Role), Content: m.Content}
	}
	wr := wireRequest{
		Model:              req.Model,
		Messages:           msgs,
		Temperature:        req.Temperature,
		MaxTokens:          req.MaxTokens,
		Stream:             true,
		StreamOptions:      &streamOptions{IncludeUsage: true},
		Seed:               req.Seed,
		TopP:               req.TopP,
		Tools:              req.Tools,
		MaxReasoningTokens: req.ThinkingBudgetTokens,
	}
	if req.Thinking != "" && req.Thinking != "off" {
		wr.ReasoningEffort = req.Thinking
	}
	if req.Thinking == "off" {
		switch req.ThinkingOffMechanism {
		case "omit":
			// Deliberately nothing. Correct only where the endpoint's default is
			// genuinely non-reasoning, and the config had to say so.
		case "chat_template_kwargs":
			wr.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
		case "reasoning_effort_none":
			wr.ReasoningEffort = "none"
		default:
			res.TransportStatus = metrics.TransportStreamErr
			res.Err = fmt.Errorf("thinking is \"off\" but no mechanism was given; "+
				"omitting a reasoning field does not disable reasoning (got %q)",
				req.ThinkingOffMechanism)
			return res
		}
	}

	body, err := encodeBody(wr, req.ExtraBody)
	if err != nil {
		res.TransportStatus = metrics.TransportStreamErr
		res.Err = err
		return res
	}
	res.RequestBody = body

	start := time.Now()
	var connectedAt time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			if connectedAt.IsZero() {
				connectedAt = time.Now()
			}
		},
	}
	hreq, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(ctx, trace),
		http.MethodPost, c.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		res.TransportStatus = metrics.TransportStreamErr
		res.Err = err
		res.WallMS = msSince(start)
		return res
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}

	resp, err := c.http.Do(hreq)
	if err != nil {
		res.TransportStatus = classify(err)
		res.Err = err
		res.WallMS = msSince(start)
		return res
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	if !connectedAt.IsZero() {
		res.HTTPConnectedMS = metrics.Ptr(connectedAt.Sub(start).Seconds() * 1000)
	}

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		res.TransportStatus = metrics.TransportHTTPErr
		res.Err = fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
		res.WallMS = msSince(start)
		return res
	}

	var visible, reasoning strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		line := sc.Text()
		res.StreamedBytes += len(line) + 1
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		raw := json.RawMessage(append([]byte(nil), payload...))
		res.RawChunks = append(res.RawChunks, raw)
		res.ChunkCount++

		var ch chunk
		if err := json.Unmarshal(raw, &ch); err != nil {
			res.TransportStatus = metrics.TransportStreamErr
			res.Err = fmt.Errorf("chunk %d: %w", res.ChunkCount, err)
			break
		}
		if ch.Usage != nil {
			res.Usage = ch.Usage
		}
		for _, c0 := range ch.Choices {
			if c0.FinishReason != nil && *c0.FinishReason != "" {
				res.FinishReason = *c0.FinishReason
			}
		}
		for _, c0 := range ch.Choices {
			r := c0.Delta.Reasoning
			if r == "" {
				r = c0.Delta.ReasoningContent
			}
			if r != "" {
				if res.ReasoningTTFTMS == nil {
					res.ReasoningTTFTMS = metrics.Ptr(msSince(start))
				}
				reasoning.WriteString(r)
			}
			if c0.Delta.Content != "" {
				if res.VisibleTTFTMS == nil {
					res.VisibleTTFTMS = metrics.Ptr(msSince(start))
				}
				visible.WriteString(c0.Delta.Content)
			}
		}
	}
	if err := sc.Err(); err != nil && res.Err == nil {
		res.TransportStatus = classify(err)
		res.Err = err
	}

	res.WallMS = msSince(start)
	res.Visible = visible.String()
	res.Reasoning = reasoning.String()
	sum := sha256.Sum256([]byte(res.Visible))
	res.OutputSHA256 = hex.EncodeToString(sum[:])
	res.OutputBytes = len(res.Visible)
	return res
}

// ReasoningTokens is the engine's count, or nil when it did not report one.
// It is never estimated from the reasoning text.
func (r *Result) ReasoningTokens() *int { return r.Usage.reasoning() }

// DerivedDecodeTokS is completion tokens over the streaming window, measured by
// this client rather than reported by the engine.
//
// It exists because an engine that exposes no telemetry otherwise leaves
// nothing to explain a wall-clock difference with. It is recorded in its own
// field and never merged with an engine-reported rate: the two are measured at
// different places and a report that conflated them would be comparing a
// server's internal counter with a client's stopwatch. Nil unless the engine
// reported a completion-token count and a visible token actually arrived.
func (r *Result) DerivedDecodeTokS() *float64 {
	if r.Usage == nil || r.Usage.CompletionTokens == nil || r.VisibleTTFTMS == nil {
		return nil
	}
	window := r.WallMS - *r.VisibleTTFTMS
	if window <= 0 {
		return nil
	}
	v := float64(*r.Usage.CompletionTokens) / (window / 1000)
	return &v
}

// encodeBody marshals the typed request and merges any engine-specific extra
// fields. Merging happens on the decoded map so a caller cannot produce
// malformed JSON, and reserved keys were already refused at config load.
func encodeBody(wr wireRequest, extra map[string]any) ([]byte, error) {
	b, err := json.Marshal(wr)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return b, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		m[k] = v
	}
	return json.Marshal(m)
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Nanoseconds()) / 1e6
}

func classify(err error) metrics.TransportStatus {
	if errors.Is(err, context.DeadlineExceeded) || os_isTimeout(err) {
		return metrics.TransportTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return metrics.TransportTimeout
	}
	if strings.Contains(err.Error(), "connection refused") {
		return metrics.TransportRefused
	}
	return metrics.TransportStreamErr
}

func os_isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
