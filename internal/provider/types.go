// Package provider implements an OpenAI-compatible chat completions client
// with streaming, tool calling, retries, fallback providers and a prompt-based
// tool protocol for models without native function calling.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Role is an OpenAI chat role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Image is an image attachment for vision-capable models.
type Image struct {
	URL      string // remote URL or data: URI
	Data     []byte
	MIMEType string
}

// ToolCall is a single function call requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the tool name and its JSON arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one conversation message.
type Message struct {
	Role       Role
	Content    string
	Name       string
	ToolCalls  []ToolCall
	ToolCallID string
	Images     []Image
	// Reasoning is kept for diagnostics but never re-sent to the model.
	Reasoning string
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef is a JSON-schema function definition.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ChatRequest is a single completion request.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Tools       []Tool
	MaxTokens   int
	Temperature *float64
	Stream      bool
	Extra       map[string]any
}

// Usage mirrors the OpenAI usage object, tolerating missing fields.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
}

// Add accumulates usage.
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
	u.ReasoningTokens += o.ReasoningTokens
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
}

// ChatResponse is a complete model turn.
type ChatResponse struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	Model        string
	Provider     string
	// Attempts is the number of HTTP attempts made (including retries).
	Attempts int
	// Latency is the wall-clock time of the successful attempt.
	Latency time.Duration
}

// Provider is the small interface the agent loop depends on.
type Provider interface {
	Name() string
	DefaultModel() string
	Chat(ctx context.Context, req ChatRequest, onDelta func(string) error) (ChatResponse, error)
	ListModels(ctx context.Context) ([]string, error)
	SupportsTools() bool
	SupportsVision() bool
	ContextWindow() int
}

// Error is a provider HTTP/stream error with retry metadata.
type Error struct {
	StatusCode int
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("provider error %d: %s", e.StatusCode, e.Message)
	}
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// IsRetryable reports whether err is worth retrying or failing over.
func IsRetryable(err error) bool {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	// Network-level errors (DNS, connection reset) are retryable unless the
	// context was cancelled.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// Retryable builds an error that a chain may fail over on.
func retryable(status int, msg string, after time.Duration, err error) *Error {
	return &Error{StatusCode: status, Message: msg, Retryable: true, RetryAfter: after, Err: err}
}

func permanent(status int, msg string, err error) *Error {
	return &Error{StatusCode: status, Message: msg, Retryable: false, Err: err}
}
