package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreamingToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request %s %q", r.URL, r.Header.Get("Authorization"))
		}
		var body struct {
			Stream bool   `json:"stream"`
			Tools  []Tool `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Stream || len(body.Tools) != 1 {
			t.Errorf("request: %+v %v", body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"world\",\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"type\":\"function\",\"function\":{\"name\":\"ping\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c, err := NewClient(Options{Name: "fake", BaseURL: srv.URL + "/v1", APIKey: "test-key", DefaultModel: "m", NativeTools: true, Streaming: true}, srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	resp, err := c.Chat(context.Background(), ChatRequest{Stream: true, Tools: []Tool{{Type: "function", Function: FunctionDef{Name: "ping", Parameters: json.RawMessage(`{"type":"object"}`)}}}}, func(s string) error {
		deltas.WriteString(s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if deltas.String() != "Hello world" || resp.Content != "Hello world" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("deltas=%q, response=%+v", deltas.String(), resp)
	}
}

func TestChatStreamingReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step one \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"step two\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c, err := NewClient(Options{Name: "fake", BaseURL: srv.URL + "/v1", APIKey: "test-key", DefaultModel: "m", NativeTools: true, Streaming: true}, srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Chat(context.Background(), ChatRequest{Stream: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reasoning != "step one step two" || resp.Content != "answer" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestMalformedToolArguments(t *testing.T) {
	args, err := ParseToolArguments(`{"value": 1,}`)
	if err != nil || !json.Valid(args) {
		t.Fatalf("repair failed: %s %v", args, err)
	}
	if _, err := ParseToolArguments(`{"value":`); err == nil {
		t.Fatal("expected unrecoverable arguments error")
	}
}

func BenchmarkSSEDecoder(b *testing.B) {
	sample := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = NewSSEDecoder(strings.NewReader(sample)).Next()
	}
}

func BenchmarkPromptToolCalls(b *testing.B) {
	sample := `Review complete. <tool_call>{"name":"read_file","arguments":{"path":"notes.txt"}}</tool_call>`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, calls := ParsePromptToolCalls(sample)
		if len(calls) != 1 {
			b.Fatal("expected one tool call")
		}
	}
}
