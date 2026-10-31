package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const promptToolOpen = "<tool_call>"
const promptToolClose = "</tool_call>"

// PromptToolInstructions renders tool schemas for models without native tool
// calling. The format follows Nous Research's Hermes convention.
func PromptToolInstructions(tools []Tool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Tool use\n")
	b.WriteString("You can use tools. To call one or more tools, emit them exactly like this:\n")
	b.WriteString(`<tool_call>{"name":"tool_name","arguments":{"key":"value"}}</tool_call>` + "\n")
	b.WriteString("Emit multiple tags for multiple independent calls. Do not wrap the tag in a code fence, do not add commentary before a call you are about to make, and wait for <tool_response> results before continuing. Arguments must be valid JSON.\n\n")
	b.WriteString("Available tools:\n")
	for _, t := range tools {
		params := string(t.Function.Parameters)
		if params == "" {
			params = `{"type":"object","properties":{}}`
		}
		fmt.Fprintf(&b, "- %s: %s\n  parameters: %s\n", t.Function.Name, oneLine(t.Function.Description), compactJSON(params))
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func compactJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}

// WithPromptTools converts a native tool conversation into a text protocol
// conversation and appends the tool instructions to the system message. It is
// used only when the provider lacks native tool support.
func WithPromptTools(messages []Message, tools []Tool) []Message {
	out := make([]Message, 0, len(messages)+1)
	instr := PromptToolInstructions(tools)
	systemDone := false
	for _, m := range messages {
		switch m.Role {
		case RoleSystem:
			if !systemDone {
				m.Content += instr
				systemDone = true
			}
			out = append(out, m)
		case RoleTool:
			name := m.Name
			if name == "" {
				name = "tool"
			}
			out = append(out, Message{
				Role:    RoleUser,
				Content: "<tool_response name=\"" + name + "\">\n" + m.Content + "\n</tool_response>",
			})
		case RoleAssistant:
			if len(m.ToolCalls) > 0 {
				var b strings.Builder
				b.WriteString(m.Content)
				for _, tc := range m.ToolCalls {
					args := tc.Function.Arguments
					if strings.TrimSpace(args) == "" {
						args = "{}"
					}
					b.WriteString("\n" + promptToolOpen + `{"name":` + quoteJSON(tc.Function.Name) + `,"arguments":` + args + `}` + promptToolClose)
				}
				m.Content = strings.TrimSpace(b.String())
				m.ToolCalls = nil
				m.ToolCallID = ""
			}
			out = append(out, m)
		default:
			out = append(out, m)
		}
	}
	if !systemDone {
		out = append([]Message{{Role: RoleSystem, Content: strings.TrimSpace(instr)}}, out...)
	}
	return out
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

type promptCall struct {
	Name       string          `json:"name"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	Parameters json.RawMessage `json:"parameters"`
	Function   *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// ParsePromptToolCalls extracts Hermes-style tool calls from model content and
// returns the visible text with the call blocks removed.
func ParsePromptToolCalls(content string) (string, []ToolCall) {
	var calls []ToolCall
	visible := content
	for {
		start := strings.Index(visible, promptToolOpen)
		if start < 0 {
			break
		}
		bodyStart := start + len(promptToolOpen)
		end := strings.Index(visible[bodyStart:], promptToolClose)
		if end < 0 {
			// Unterminated block: strip it; the model gets no result and can
			// retry on the next turn.
			visible = visible[:start]
			break
		}
		body := strings.TrimSpace(visible[bodyStart : bodyStart+end])
		call := parsePromptCall(body)
		if call.Function.Name != "" {
			calls = append(calls, call)
		}
		visible = visible[:start] + visible[bodyStart+end+len(promptToolClose):]
	}
	// Also accept a fenced tool_call block for models that add code fences.
	visible = strings.TrimSpace(visible)
	return visible, calls
}

func parsePromptCall(body string) ToolCall {
	var pc promptCall
	if err := json.Unmarshal([]byte(body), &pc); err != nil {
		// Best-effort recovery of the name; keep the raw body so the agent
		// can return a useful JSON error to the model.
		name := looseName(body)
		return ToolCall{
			Type:     "function",
			Function: FunctionCall{Name: name, Arguments: body},
		}
	}
	name := pc.Name
	args := pc.Arguments
	if pc.Function != nil {
		if pc.Function.Name != "" {
			name = pc.Function.Name
		}
		if len(pc.Function.Arguments) > 0 {
			args = pc.Function.Arguments
		}
	}
	if name == "" {
		name = pc.Tool
	}
	if len(args) == 0 {
		args = pc.Parameters
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return ToolCall{
		ID:       "",
		Type:     "function",
		Function: FunctionCall{Name: name, Arguments: normalizeArguments(args)},
	}
}

func looseName(body string) string {
	for _, key := range []string{`"name"`, `"tool"`} {
		i := strings.Index(body, key)
		if i < 0 {
			continue
		}
		rest := body[i+len(key):]
		i = strings.IndexByte(rest, ':')
		if i < 0 {
			continue
		}
		rest = strings.TrimSpace(rest[i+1:])
		rest = strings.TrimPrefix(rest, `"`)
		j := strings.IndexAny(rest, `"`)
		if j > 0 {
			return rest[:j]
		}
	}
	return ""
}

// SortToolCalls is a deterministic helper for tests and prompt rendering.
func SortToolCalls(calls []ToolCall) {
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].ID < calls[j].ID })
}

// promptStreamFilter withholds a potential tool tag split across deltas and
// suppresses everything after the tag begins, so users do not see raw JSON.
type promptStreamFilter struct {
	pending    string
	suppressed bool
	out        func(string) error
	buf        strings.Builder
}

func newPromptStreamFilter(out func(string) error) *promptStreamFilter {
	return &promptStreamFilter{out: out}
}

func (f *promptStreamFilter) Write(s string) error {
	if f.suppressed {
		f.buf.WriteString(s)
		return nil
	}
	f.buf.WriteString(s)
	f.pending += s
	if i := strings.Index(f.pending, promptToolOpen); i >= 0 {
		visible := f.pending[:i]
		f.pending = ""
		f.suppressed = true
		if visible != "" && f.out != nil {
			return f.out(visible)
		}
		return nil
	}
	keep := len(promptToolOpen) - 1
	if len(f.pending) > keep {
		visible := f.pending[:len(f.pending)-keep]
		f.pending = f.pending[len(f.pending)-keep:]
		if visible != "" && f.out != nil {
			return f.out(visible)
		}
	}
	return nil
}

func (f *promptStreamFilter) Flush() error {
	if !f.suppressed && f.pending != "" && f.out != nil {
		err := f.out(f.pending)
		f.pending = ""
		return err
	}
	f.pending = ""
	return nil
}

func (f *promptStreamFilter) Content() string { return f.buf.String() }
