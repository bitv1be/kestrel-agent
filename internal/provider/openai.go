package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bitvibe/kestrel/internal/logx"
)

// Options configures an OpenAI-compatible client.
type Options struct {
	Name          string
	BaseURL       string
	APIKey        string
	DefaultModel  string
	ChatPath      string
	AudioPath     string
	Headers       map[string]string
	Timeout       time.Duration
	NativeTools   bool
	Streaming     bool
	Vision        bool
	ContextWindow int
	MaxTokens     int
	Temperature   *float64
	ExtraBody     map[string]any
	ProxySpec     string
	HTTPClient    *http.Client
	Logger        *slog.Logger
	Retry         RetryPolicy
}

// RetryPolicy controls exponential backoff with jitter.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Sleep       func(context.Context, time.Duration) error
}

// DefaultRetry is used when Options.Retry is zero.
var DefaultRetry = RetryPolicy{MaxAttempts: 4, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second}

// Client talks to one OpenAI-compatible endpoint.
type Client struct {
	opts   Options
	http   *http.Client
	log    *slog.Logger
	retry  RetryPolicy
	stream bool
}

var callSeq atomic.Uint64

// NewClient builds a client. The supplied HTTP client should have no global
// timeout (per-attempt timeouts are applied here) and should come from a
// proxy-aware transport.
func NewClient(opts Options, hc *http.Client, logger *slog.Logger) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("provider %q: base_url is required", opts.Name)
	}
	if opts.ChatPath == "" {
		opts.ChatPath = "/chat/completions"
	}
	if opts.AudioPath == "" {
		opts.AudioPath = "/audio/transcriptions"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.ContextWindow <= 0 {
		opts.ContextWindow = 32768
	}
	if hc == nil {
		hc = &http.Client{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := opts.Retry
	if r.MaxAttempts <= 0 {
		r = DefaultRetry
	}
	if r.Sleep == nil {
		r.Sleep = sleepCtx
	}
	return &Client{opts: opts, http: hc, log: logger, retry: r, stream: opts.Streaming}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) Name() string          { return c.opts.Name }
func (c *Client) DefaultModel() string  { return c.opts.DefaultModel }
func (c *Client) SupportsTools() bool   { return c.opts.NativeTools }
func (c *Client) SupportsVision() bool  { return c.opts.Vision }
func (c *Client) ContextWindow() int    { return c.opts.ContextWindow }
func (c *Client) ProxySpec() string     { return c.opts.ProxySpec }
func (c *Client) Streaming() bool       { return c.opts.Streaming }
func (c *Client) BaseURL() string       { return c.opts.BaseURL }
func (c *Client) ChatEndpoint() string  { return c.endpoint(c.opts.ChatPath) }
func (c *Client) AudioEndpoint() string { return c.endpoint(c.opts.AudioPath) }

func (c *Client) endpoint(path string) string {
	base := strings.TrimRight(c.opts.BaseURL, "/")
	if path == "" {
		return base
	}
	if strings.HasSuffix(base, path) {
		return base
	}
	return base + "/" + strings.TrimLeft(path, "/")
}

func (c *Client) modelsEndpoint() string {
	base := strings.TrimRight(c.opts.BaseURL, "/")
	base = strings.TrimSuffix(base, c.opts.ChatPath)
	base = strings.TrimSuffix(base, "/")
	return base + "/models"
}

// Chat executes a completion, retrying 429/5xx/network failures with jitter.
// onDelta (optional) receives visible content deltas when streaming.
func (c *Client) Chat(ctx context.Context, req ChatRequest, onDelta func(string) error) (ChatResponse, error) {
	if req.Model == "" {
		req.Model = c.opts.DefaultModel
	}
	if req.Model == "" {
		return ChatResponse{}, fmt.Errorf("provider %q: no model selected", c.opts.Name)
	}
	native := c.opts.NativeTools && len(req.Tools) > 0
	msgs := req.Messages
	tools := req.Tools
	if len(req.Tools) > 0 && !c.opts.NativeTools {
		msgs = WithPromptTools(msgs, req.Tools)
	}
	stream := req.Stream && c.opts.Streaming
	var lastErr error
	var lastResp ChatResponse
	attempts := 0
	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		attempts++
		emitted := false
		delta := onDelta
		if delta != nil {
			delta = func(s string) error {
				emitted = true
				return onDelta(s)
			}
		}
		start := time.Now()
		promptMode := !c.opts.NativeTools && len(req.Tools) > 0
		resp, err := c.attempt(ctx, req, msgs, toolsIf(native, tools), stream, promptMode, delta)
		if err == nil {
			resp.Provider = c.opts.Name
			resp.Attempts = attempts
			resp.Latency = time.Since(start)
			if !native {
				// Prompt-based tools: parse calls out of the visible text.
				if strings.Contains(resp.Content, promptToolOpen) {
					visible, calls := ParsePromptToolCalls(resp.Content)
					if len(calls) > 0 {
						resp.Content = visible
						resp.ToolCalls = calls
					}
				}
			} else if len(resp.ToolCalls) == 0 && strings.Contains(resp.Content, promptToolOpen) {
				// Native tools advertised but the model used the text protocol.
				visible, calls := ParsePromptToolCalls(resp.Content)
				if len(calls) > 0 {
					resp.Content = visible
					resp.ToolCalls = calls
				}
			}
			ensureCallIDs(resp.ToolCalls)
			if resp.Usage.TotalTokens == 0 {
				resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
			}
			return resp, nil
		}
		lastErr = err
		lastResp = resp
		if ctxErr := ctx.Err(); ctxErr != nil {
			return resp, ctxErr
		}
		if !IsRetryable(err) || emitted || attempt == c.retry.MaxAttempts {
			break
		}
		delay := c.backoff(attempt, err)
		c.log.Warn("provider request failed; retrying",
			"provider", c.opts.Name, "attempt", attempt, "delay", delay, "error", logx.Redact(err.Error()))
		if serr := c.retry.Sleep(ctx, delay); serr != nil {
			return ChatResponse{}, serr
		}
	}
	if lastErr == nil {
		return ChatResponse{}, fmt.Errorf("provider %q: request failed", c.opts.Name)
	}
	return lastResp, lastErr
}

func toolsIf(native bool, tools []Tool) []Tool {
	if !native || len(tools) == 0 {
		return nil
	}
	return tools
}

func (c *Client) backoff(attempt int, err error) time.Duration {
	var pe *Error
	if asErr(err, &pe) && pe.RetryAfter > 0 {
		if pe.RetryAfter > 30*time.Second {
			return 30 * time.Second
		}
		return pe.RetryAfter
	}
	d := c.retry.BaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= c.retry.MaxDelay {
			d = c.retry.MaxDelay
			break
		}
	}
	if d > c.retry.MaxDelay {
		d = c.retry.MaxDelay
	}
	if d > 0 {
		d += time.Duration(rand.Int63n(int64(d/2) + 1))
	}
	return d
}

func asErr(err error, target **Error) bool {
	return errors.As(err, target)
}

func (c *Client) attempt(ctx context.Context, req ChatRequest, msgs []Message, tools []Tool, stream, promptMode bool, onDelta func(string) error) (ChatResponse, error) {
	body, err := c.requestBody(req, msgs, tools, stream)
	if err != nil {
		return ChatResponse{}, err
	}
	actx := ctx
	if c.opts.Timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, c.opts.Timeout)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(actx, http.MethodPost, c.endpoint(c.opts.ChatPath), bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, permanent(0, "build request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if c.opts.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
	}
	for k, v := range c.opts.Headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResponse{}, retryable(0, "request failed", 0, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, c.statusError(resp)
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if stream && strings.Contains(ct, "text/event-stream") {
		return c.readStream(actx, resp.Body, promptMode, onDelta)
	}
	return c.readJSON(resp.Body, req.Model)
}

func (c *Client) statusError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	msg := strings.TrimSpace(string(raw))
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	var apiErr struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message    string  `json:"message"`
		RetryAfter float64 `json:"retry_after"`
		Parameters struct {
			RetryAfter float64 `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(raw, &apiErr) == nil {
		if apiErr.Error.Message != "" {
			msg = apiErr.Error.Message
		} else if apiErr.Message != "" {
			msg = apiErr.Message
		}
		if apiErr.RetryAfter > 0 {
			retryAfter = time.Duration(apiErr.RetryAfter * float64(time.Second))
		} else if apiErr.Parameters.RetryAfter > 0 {
			retryAfter = time.Duration(apiErr.Parameters.RetryAfter * float64(time.Second))
		}
	}
	msg = truncate(logx.Redact(msg), 2000)
	retryableStatus := resp.StatusCode == 408 || resp.StatusCode == 409 || resp.StatusCode == 425 ||
		resp.StatusCode == 429 || resp.StatusCode >= 500
	if retryableStatus {
		return retryable(resp.StatusCode, msg, retryAfter, nil)
	}
	return permanent(resp.StatusCode, msg, nil)
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// requestBody marshals an OpenAI chat completion request without a needless
// JSON round-trip through a large generic map.
func (c *Client) requestBody(req ChatRequest, msgs []Message, tools []Tool, stream bool) ([]byte, error) {
	apiMsgs := c.apiMessages(msgs)
	body := make(map[string]any, 10+len(c.opts.ExtraBody)+len(req.Extra))
	body["model"] = req.Model
	body["messages"] = apiMsgs
	if stream {
		body["stream"] = true
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	mt := req.MaxTokens
	if mt == 0 {
		mt = c.opts.MaxTokens
	}
	if mt > 0 {
		body["max_tokens"] = mt
	}
	temp := req.Temperature
	if temp == nil {
		temp = c.opts.Temperature
	}
	if temp != nil {
		body["temperature"] = *temp
	}
	for k, v := range c.opts.ExtraBody {
		body[k] = v
	}
	for k, v := range req.Extra {
		body[k] = v
	}
	return json.Marshal(body)
}

type apiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`
	Name       string        `json:"name,omitempty"`
	ToolCalls  []apiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type apiToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type apiContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

func (c *Client) apiMessages(msgs []Message) []apiMessage {
	out := make([]apiMessage, 0, len(msgs))
	for _, m := range msgs {
		am := apiMessage{Role: string(m.Role), Name: m.Name, ToolCallID: m.ToolCallID}
		if len(m.Images) > 0 && (m.Role == RoleUser || m.Role == RoleSystem) {
			parts := make([]apiContentPart, 0, len(m.Images)+1)
			if m.Content != "" {
				parts = append(parts, apiContentPart{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				u := img.URL
				if u == "" && len(img.Data) > 0 {
					mt := img.MIMEType
					if mt == "" {
						mt = "image/jpeg"
					}
					u = "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
				}
				if u != "" {
					p := apiContentPart{Type: "image_url"}
					p.ImageURL = &struct {
						URL string `json:"url"`
					}{URL: u}
					parts = append(parts, p)
				}
			}
			am.Content = parts
		} else {
			am.Content = m.Content
		}
		for _, tc := range m.ToolCalls {
			var at apiToolCall
			at.ID = tc.ID
			at.Type = "function"
			at.Function.Name = tc.Function.Name
			at.Function.Arguments = tc.Function.Arguments
			if strings.TrimSpace(at.Function.Arguments) == "" {
				at.Function.Arguments = "{}"
			}
			am.ToolCalls = append(am.ToolCalls, at)
		}
		out = append(out, am)
	}
	return out
}

type apiChoice struct {
	Message      apiRespMessage `json:"message"`
	Delta        apiRespMessage `json:"delta"`
	FinishReason string         `json:"finish_reason"`
}

type apiRespMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
	ToolCalls        []apiStreamCall `json:"tool_calls"`
}

type apiStreamCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type apiCompletion struct {
	Model   string      `json:"model"`
	Choices []apiChoice `json:"choices"`
	Usage   *Usage      `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (c *Client) readJSON(r io.Reader, model string) (ChatResponse, error) {
	dec := json.NewDecoder(io.LimitReader(r, 64<<20))
	var out apiCompletion
	if err := dec.Decode(&out); err != nil {
		return ChatResponse{}, retryable(0, "decode response", 0, err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return ChatResponse{}, retryable(0, out.Error.Message, 0, nil)
	}
	resp := ChatResponse{Model: firstNonEmpty(out.Model, model)}
	if out.Usage != nil {
		resp.Usage = *out.Usage
	}
	if len(out.Choices) == 0 {
		return resp, nil
	}
	ch := out.Choices[0]
	resp.FinishReason = ch.FinishReason
	resp.Content = decodeContent(ch.Message.Content)
	resp.Reasoning = firstNonEmpty(ch.Message.ReasoningContent, ch.Message.Reasoning)
	for _, tc := range ch.Message.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: FunctionCall{
				Name:      tc.Function.Name,
				Arguments: normalizeArguments(tc.Function.Arguments),
			},
		})
	}
	return resp, nil
}

func (c *Client) readStream(ctx context.Context, r io.Reader, promptMode bool, onDelta func(string) error) (ChatResponse, error) {
	dec := NewSSEDecoder(r)
	var content, reasoning strings.Builder
	var finish string
	calls := make(map[int]*ToolCall)
	var order []int
	var usage Usage
	var filter *promptStreamFilter
	if onDelta != nil && promptMode {
		filter = newPromptStreamFilter(onDelta)
	}
	for {
		if err := ctx.Err(); err != nil {
			return ChatResponse{Content: content.String(), Reasoning: reasoning.String()}, err
		}
		ev, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ChatResponse{Content: content.String(), Reasoning: reasoning.String()}, retryable(0, "read stream", 0, err)
		}
		if ev.Data == "" {
			continue
		}
		data := strings.TrimSpace(ev.Data)
		if data == "[DONE]" {
			break
		}
		var chunk apiCompletion
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Tolerate vendor keep-alives and noise rather than killing a
			// partially streamed answer.
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return ChatResponse{Content: content.String()}, retryable(0, chunk.Error.Message, 0, nil)
		}
		if chunk.Usage != nil {
			usage.Add(*chunk.Usage)
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			txt := decodeContent(ch.Delta.Content)
			if txt != "" {
				content.WriteString(txt)
				if onDelta != nil {
					if filter != nil {
						if err := filter.Write(txt); err != nil {
							return ChatResponse{}, err
						}
					} else if err := onDelta(txt); err != nil {
						return ChatResponse{}, err
					}
				}
			}
			if rc := firstNonEmpty(ch.Delta.ReasoningContent, ch.Delta.Reasoning); rc != "" {
				reasoning.WriteString(rc)
			}
			for _, tc := range ch.Delta.ToolCalls {
				idx := tc.Index
				cur, ok := calls[idx]
				if !ok {
					// Some providers repeat index 0 for every tool call in a
					// single complete delta. Detect a fresh call with a new ID.
					if tc.ID != "" && len(calls) > 0 {
						if existing := calls[idx]; existing != nil && existing.ID != "" && existing.ID != tc.ID && existing.Function.Name != "" && tc.Function.Name != "" {
							idx = len(calls)
						}
					}
					cur = &ToolCall{Type: "function"}
					calls[idx] = cur
					order = append(order, idx)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Function.Name += tc.Function.Name
				}
				args := normalizeArguments(tc.Function.Arguments)
				if args != "" {
					cur.Function.Arguments += args
				}
			}
		}
	}
	if filter != nil {
		if err := filter.Flush(); err != nil {
			return ChatResponse{}, err
		}
	}
	resp := ChatResponse{
		Content:      content.String(),
		Reasoning:    reasoning.String(),
		FinishReason: finish,
		Usage:        usage,
	}
	for _, idx := range order {
		tc := calls[idx]
		if tc.Function.Name == "" && tc.Function.Arguments == "" {
			continue
		}
		resp.ToolCalls = append(resp.ToolCalls, *tc)
	}
	return resp, nil
}

func decodeContent(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	if raw[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				if p.Text != "" {
					b.WriteString(p.Text)
				}
			}
			return b.String()
		}
	}
	return string(raw)
}

func ensureCallIDs(calls []ToolCall) {
	for i := range calls {
		if calls[i].Type == "" {
			calls[i].Type = "function"
		}
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), callSeq.Add(1))
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ListModels queries GET /models when supported.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	actx := ctx
	if c.opts.Timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, c.opts.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(actx, http.MethodGet, c.modelsEndpoint(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
	}
	for k, v := range c.opts.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.statusError(resp)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

// ProbeResult reports the outcome of a real provider test.
type ProbeResult struct {
	Latency     time.Duration
	Model       string
	Content     string
	ToolCalls   []ToolCall
	NativeTools bool
	Usage       Usage
}

// Probe performs a tiny real request and, when tools are enabled, asks the
// model to call a ping tool so tool support can be observed.
func (c *Client) Probe(ctx context.Context, model string) (ProbeResult, error) {
	if model == "" {
		model = c.opts.DefaultModel
	}
	req := ChatRequest{
		Model:     model,
		MaxTokens: 64,
		Messages: []Message{
			{Role: RoleUser, Content: "Reply with the single word OK. If a ping tool is available, call it with {\"text\":\"ok\"} instead."},
		},
	}
	if c.opts.NativeTools {
		req.Tools = []Tool{{
			Type: "function",
			Function: FunctionDef{
				Name:        "ping",
				Description: "Health check.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
			},
		}}
	}
	start := time.Now()
	resp, err := c.Chat(ctx, req, nil)
	if err != nil {
		return ProbeResult{Latency: time.Since(start)}, err
	}
	return ProbeResult{
		Latency:     time.Since(start),
		Model:       firstNonEmpty(resp.Model, model),
		Content:     resp.Content,
		ToolCalls:   resp.ToolCalls,
		NativeTools: c.opts.NativeTools,
		Usage:       resp.Usage,
	}, nil
}

// EndpointForTest exposes URL joining for tests.
func (c *Client) EndpointForTest() (string, error) {
	return url.JoinPath(c.opts.BaseURL, c.opts.ChatPath)
}
