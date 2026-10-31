package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/bitvibe/kestrel/internal/provider"
)

// estimateTokens is a deliberately conservative, allocation-free estimate.
// It errs high for JSON/tool traffic by counting per-message overhead.
func estimateTokens(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += 8
		total += len(m.Content) / 4
		total += len(m.Name) / 4
		for _, tc := range m.ToolCalls {
			total += 12 + len(tc.Function.Name)/4 + len(tc.Function.Arguments)/4
		}
		total += len(m.Images) * 800
	}
	return total
}

// trimToolOutputs bounds oversized tool results before they re-enter the
// context, preserving the beginning and end.
func trimToolOutputs(msgs []provider.Message, limit int) []provider.Message {
	if limit <= 0 {
		return msgs
	}
	out := make([]provider.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if out[i].Role == provider.RoleTool && len(out[i].Content) > limit {
			out[i].Content = truncateMiddle(out[i].Content, limit)
		}
	}
	return out
}

// truncateHistory drops oldest complete turns until the estimate fits within
// 75% of the context window. System messages and the newest turn are kept.
func truncateHistory(msgs []provider.Message, window int) []provider.Message {
	if window <= 0 || len(msgs) <= 2 {
		return msgs
	}
	target := window * 3 / 4
	if estimateTokens(msgs) <= target {
		return msgs
	}
	var system []provider.Message
	body := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == provider.RoleSystem {
			if len(body) == 0 {
				system = append(system, m)
				continue
			}
		}
		body = append(body, m)
	}
	for len(body) > 2 && estimateTokens(body) > target {
		cut := 1
		for cut < len(body) && body[cut].Role != provider.RoleUser {
			cut++
		}
		if cut >= len(body) {
			cut = len(body) - 1
		}
		body = body[cut:]
	}
	if len(body) == 0 {
		body = msgs[len(msgs)-1:]
	}
	return append(system, body...)
}

// compact summarises older history with the active model, returning a short
// system summary followed by recent turns. If the model call fails the caller
// falls back to mechanical truncation.
func (e *Engine) compact(ctx context.Context, history []provider.Message, window int) ([]provider.Message, error) {
	if len(history) < 4 {
		return history, nil
	}
	keep := e.cfg.Agent.CompactKeep
	if keep <= 0 {
		keep = 8
	}
	if keep >= len(history) {
		keep = len(history) / 2
	}
	split := len(history) - keep
	// Never split in the middle of a tool exchange: move forward to a user turn.
	for split < len(history)-1 && history[split].Role != provider.RoleUser {
		split++
	}
	if split <= 0 || split >= len(history) {
		return truncateHistory(history, window/2), nil
	}
	older := history[:split]
	recent := history[split:]
	transcript := renderTranscript(older, 60000)
	if strings.TrimSpace(transcript) == "" {
		return truncateHistory(history, window/2), nil
	}
	system := "You compress conversation history for an autonomous assistant. Preserve: user goals and preferences, decisions, file paths, commands and their outcomes, errors, open tasks, and facts needed to continue. Discard greetings and redundancy. Reply with concise bullet points only."
	user := "Conversation to summarise:\n\n" + transcript
	ch := e.chain()
	resp, err := ch.Chat(ctx, provider.ChatRequest{
		Model:     "",
		Messages:  []provider.Message{{Role: provider.RoleSystem, Content: system}, {Role: provider.RoleUser, Content: user}},
		MaxTokens: 1200,
		Stream:    false,
	}, nil)
	if err != nil {
		return nil, err
	}
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return nil, fmt.Errorf("compaction returned an empty summary")
	}
	out := make([]provider.Message, 0, len(recent)+1)
	out = append(out, provider.Message{
		Role:    provider.RoleSystem,
		Content: "Summary of earlier conversation (older messages omitted to save context):\n" + summary,
	})
	out = append(out, recent...)
	if estimateTokens(out) > window*2/3 {
		out = truncateHistory(out, window/2)
	}
	return out, nil
}

func renderTranscript(msgs []provider.Message, max int) string {
	var b strings.Builder
	for _, m := range msgs {
		if b.Len() > max {
			break
		}
		content := m.Content
		if len(content) > 2000 {
			content = content[:1000] + "\n...[truncated]...\n" + content[len(content)-1000:]
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, content)
		if len(m.ToolCalls) > 0 {
			var names []string
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			fmt.Fprintf(&b, "  tool calls: %s\n", strings.Join(names, ", "))
		}
	}
	if b.Len() > max {
		return b.String()[:max] + "\n...[transcript truncated]..."
	}
	return b.String()
}
