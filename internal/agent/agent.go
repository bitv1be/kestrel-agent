// Package agent implements Kestrel's gateway-independent agent core.
//
// One long-lived actor goroutine owns each chat session. The actor serialises
// messages, starts at most one run at a time, and handles /stop and /new
// without locks shared across chats. Runs build a copy of the history, so the
// actor remains the only writer of session state.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/bitvibe/kestrel/internal/config"
	"github.com/bitvibe/kestrel/internal/logx"
	"github.com/bitvibe/kestrel/internal/provider"
	"github.com/bitvibe/kestrel/internal/store"
	"github.com/bitvibe/kestrel/internal/tools"
)

// ErrBusy is returned when a chat's queue is full.
var ErrBusy = errors.New("agent is busy")

// ErrCancelled indicates a run was stopped by the user or shutdown.
var ErrCancelled = errors.New("run cancelled")

// Task is one item on a run's visible plan.
type Task struct {
	Text   string
	Status string // pending | in_progress | completed
}

// Status is a live snapshot of a run for frontends that render a pinned
// status board. Running is false for the final update.
type RunStatus struct {
	Running   bool
	Phase     string // starting | thinking | tool | compacting | done | error | stopped
	Action    string // short, safe description of the current tool
	Model     string
	Iteration int
	Tasks     []Task
}

// Chat is a frontend conversation surface (Telegram, CLI, tests).
type Chat interface {
	// Send delivers a final message. Implementations split long text.
	Send(ctx context.Context, text string) error
	// Update replaces the in-progress streaming message with the full text
	// accumulated so far. Implementations coalesce and rate-limit edits.
	Update(ctx context.Context, fullText string) error
	// Progress reports a short status line ("compacting history"). The
	// frontend owns the single status message and may replace or clear it.
	Progress(ctx context.Context, text string) error
	// Status reports the run's live phase and plan. Frontends that keep a
	// pinned status board update it; others may ignore it.
	Status(ctx context.Context, status RunStatus) error
	// Action reports one completed agent action (a tool call) as a durable
	// log entry. Unlike Progress, Action messages are never replaced.
	Action(ctx context.Context, text string) error
	// Think reports the model's reasoning for one turn. Frontends may hide it
	// or render it differently from normal output.
	Think(ctx context.Context, text string) error
	// Error reports a failure the user should see: provider errors, tool
	// errors, panics, and timeouts. It is independent of action-log verbosity.
	Error(ctx context.Context, text string) error
	// Typing shows a typing indicator.
	Typing(ctx context.Context) error
	// SendFile uploads a local file.
	SendFile(ctx context.Context, path, caption string) error
}

// NopChat discards UI updates; useful for tests and cron runs without a UI.
type NopChat struct{}

func (NopChat) Send(context.Context, string) error             { return nil }
func (NopChat) Update(context.Context, string) error           { return nil }
func (NopChat) Progress(context.Context, string) error         { return nil }
func (NopChat) Status(context.Context, RunStatus) error        { return nil }
func (NopChat) Action(context.Context, string) error           { return nil }
func (NopChat) Think(context.Context, string) error            { return nil }
func (NopChat) Error(context.Context, string) error            { return nil }
func (NopChat) Typing(context.Context) error                   { return nil }
func (NopChat) SendFile(context.Context, string, string) error { return nil }

// Incoming is a user message entering a session.
type Incoming struct {
	ChatID    int64
	UserID    int64
	Text      string
	Images    []provider.Image
	Scheduled bool
}

// EngineConfig captures the agent-loop settings.
type EngineConfig struct {
	MaxIterations    int
	RunTimeout       int64 // seconds, for tests; zero uses Config
	ContextWindow    int
	CompactThreshold float64
	CompactKeep      int
	MaxHistory       int
	ToolOutputLimit  int
	MaxParallelTools int
	MaxQueue         int
	Interrupt        string
	ShowProgress     bool
	ToolTimeout      int64
	MaxTokens        int
}

// Engine runs the tool-calling loop. It is safe for concurrent use by many
// session actors.
type Engine struct {
	cfg       *config.Config
	store     *store.Store
	tools     *tools.Registry
	providers *provider.Registry
	log       *slog.Logger

	mu          sync.RWMutex
	activeName  string
	activeModel string
	fallbacks   []string
}

// NewEngine builds an engine and resolves the configured active model.
func NewEngine(cfg *config.Config, st *store.Store, reg *tools.Registry, providers *provider.Registry, log *slog.Logger) (*Engine, error) {
	if cfg == nil || providers == nil {
		return nil, errors.New("engine requires config and providers")
	}
	if log == nil {
		log = slog.Default()
	}
	name, model, err := cfg.ActiveProviderModel()
	if err != nil {
		return nil, err
	}
	if _, ok := providers.Get(name); !ok {
		return nil, fmt.Errorf("active provider %q is not configured", name)
	}
	e := &Engine{
		cfg: cfg, store: st, tools: reg, providers: providers, log: log,
		activeName: name, activeModel: model, fallbacks: append([]string(nil), cfg.Fallback...),
	}
	return e, nil
}

// LoadRuntimeOverride restores a previously selected provider/model from the
// database, letting /model survive restarts.
func (e *Engine) LoadRuntimeOverride(ctx context.Context) {
	if e.store == nil {
		return
	}
	v, ok, err := e.store.GetKV(ctx, "active_model")
	if err != nil || !ok {
		return
	}
	name, model, err := config.SplitModel(v)
	if err != nil {
		return
	}
	if _, ok := e.providers.Get(name); !ok {
		return
	}
	e.mu.Lock()
	e.activeName, e.activeModel = name, model
	e.mu.Unlock()
}

// SetModel switches the active provider/model at runtime.
func (e *Engine) SetModel(providerName, model string) error {
	p, ok := e.providers.Get(providerName)
	if !ok {
		return fmt.Errorf("unknown provider %q", providerName)
	}
	if strings.TrimSpace(model) == "" {
		model = p.DefaultModel()
	}
	e.mu.Lock()
	e.activeName, e.activeModel = providerName, model
	e.mu.Unlock()
	if e.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3e9)
		defer cancel()
		if err := e.store.SetKV(ctx, "active_model", providerName+":"+model); err != nil {
			e.log.Warn("persist active model", "error", err)
		}
	}
	return nil
}

// Active returns the current provider and model.
func (e *Engine) Active() (string, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.activeName, e.activeModel
}

// Providers exposes the registry for CLI and gateway commands.
func (e *Engine) Providers() *provider.Registry { return e.providers }

// Tools exposes the tool registry.
func (e *Engine) Tools() *tools.Registry { return e.tools }

// Config returns the immutable configuration.
func (e *Engine) Config() *config.Config { return e.cfg }

// Store returns the persistence layer (may be nil in tests).
func (e *Engine) Store() *store.Store { return e.store }

func (e *Engine) chain() *provider.Chain {
	e.mu.RLock()
	name := e.activeName
	fallbacks := append([]string(nil), e.fallbacks...)
	e.mu.RUnlock()
	primary, ok := e.providers.Get(name)
	if !ok {
		return provider.NewChain(nil, nil, e.log)
	}
	var fb []provider.Provider
	for _, fname := range fallbacks {
		if fname == name {
			continue
		}
		if p, ok := e.providers.Get(fname); ok {
			fb = append(fb, p)
		}
	}
	ch := provider.NewChain(primary, fb, e.log)
	return ch
}

// RunInput is one agent run against a snapshot of history.
type RunInput struct {
	Chat      Chat
	SessionID string
	ChatID    int64
	HomeDir   string
	History   []provider.Message
	User      provider.Message
}

// RunOutput is the new messages and usage produced by a run.
type RunOutput struct {
	// History is the complete post-run in-memory history, possibly compacted.
	History []provider.Message
	// Messages are only the new messages, for persistence.
	Messages   []provider.Message
	Usage      provider.Usage
	Provider   string
	Model      string
	Iterations int
	Err        error
}

// Run executes the tool-calling loop. The caller owns history and must append
// the returned messages.
func (e *Engine) Run(ctx context.Context, in RunInput) RunOutput {
	chat := in.Chat
	if chat == nil {
		chat = NopChat{}
	}
	providerName, activeModel := e.Active()
	modelLabel := providerName + ":" + activeModel
	out := RunOutput{Provider: providerName, Model: activeModel}
	history := make([]provider.Message, len(in.History))
	copy(history, in.History)
	history = append(history, in.User)
	out.Messages = append(out.Messages, in.User)
	finish := func(err error) RunOutput {
		out.Err = err
		out.History = history
		return out
	}

	window := e.contextWindow(providerName)
	threshold := e.cfg.Agent.CompactThreshold
	if threshold <= 0 || threshold > 1 {
		threshold = 0.8
	}
	history = trimToolOutputs(history, e.cfg.Agent.ToolOutputLimit)
	if estimateTokens(history) > int(float64(window)*threshold) {
		e.emitStatus(ctx, chat, RunStatus{Running: true, Phase: "compacting", Model: modelLabel, Tasks: e.taskSnapshot(in.SessionID)})
		_ = chat.Progress(ctx, "Compacting conversation history…")
		compacted, err := e.compact(ctx, history, window)
		if err != nil {
			e.log.Warn("context compaction failed; truncating", "error", err)
			_ = chat.Error(ctx, "Context compaction failed; truncating history: "+logx.Redact(err.Error()))
			history = truncateHistory(history, window/2)
		} else {
			history = compacted
		}
		_ = chat.Progress(ctx, "")
	}
	base := e.systemPrompt(ctx, in.HomeDir)
	// Zero (the default) means unlimited: the run only ends with a final
	// answer, a provider failure, or /stop (which cancels the context).
	maxIter := e.cfg.Agent.MaxIterations

	runCtx := ctx
	if d := e.cfg.Agent.RunTimeout.D(); d > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	if err := chat.Typing(runCtx); err != nil {
		e.log.Debug("typing indicator failed", "error", err)
	}
	e.emitStatus(runCtx, chat, RunStatus{Running: true, Phase: "starting", Model: modelLabel, Tasks: e.taskSnapshot(in.SessionID)})
	toolDefs := e.tools.Definitions()

	for iter := 1; maxIter <= 0 || iter <= maxIter; iter++ {
		if err := runCtx.Err(); err != nil {
			e.emitStatus(runCtx, chat, RunStatus{Running: false, Phase: "stopped", Model: modelLabel, Iteration: iter, Tasks: e.taskSnapshot(in.SessionID)})
			return finish(ErrCancelled)
		}
		out.Iterations = iter
		e.emitStatus(runCtx, chat, RunStatus{Running: true, Phase: "thinking", Model: modelLabel, Iteration: iter, Tasks: e.taskSnapshot(in.SessionID)})
		reqMsgs := make([]provider.Message, 0, len(history)+1)
		reqMsgs = append(reqMsgs, provider.Message{Role: provider.RoleSystem, Content: base})
		reqMsgs = append(reqMsgs, history...)
		reqMsgs = trimToolOutputs(reqMsgs, e.cfg.Agent.ToolOutputLimit)
		reqMsgs = truncateHistory(reqMsgs, window)

		var streamed strings.Builder
		ch := e.chain()
		resp, err := ch.Chat(runCtx, provider.ChatRequest{
			Model:       activeModel,
			Messages:    reqMsgs,
			Tools:       toolDefs,
			MaxTokens:   e.cfg.Agent.MaxTokens,
			Temperature: e.cfg.Agent.Temperature,
			Stream:      true,
		}, func(delta string) error {
			streamed.WriteString(delta)
			return chat.Update(runCtx, streamed.String())
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(runCtx.Err(), context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				e.emitStatus(runCtx, chat, RunStatus{Running: false, Phase: "stopped", Model: modelLabel, Iteration: iter, Tasks: e.taskSnapshot(in.SessionID)})
				return finish(ErrCancelled)
			}
			msg := "The model request failed: " + logx.Redact(err.Error())
			e.emitStatus(runCtx, chat, RunStatus{Running: false, Phase: "error", Action: logx.Redact(err.Error()), Model: modelLabel, Iteration: iter, Tasks: e.taskSnapshot(in.SessionID)})
			_ = chat.Error(runCtx, msg)
			return finish(err)
		}
		out.Provider = resp.Provider
		out.Model = resp.Model
		out.Usage.Add(resp.Usage)
		if reasoning := strings.TrimSpace(resp.Reasoning); reasoning != "" {
			if err := chat.Think(runCtx, reasoning); err != nil {
				e.log.Debug("send reasoning", "error", err)
			}
		}

		assistant := provider.Message{
			Role:      provider.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
			Reasoning: resp.Reasoning,
		}
		history = append(history, assistant)
		out.Messages = append(out.Messages, assistant)

		if len(resp.ToolCalls) > 0 && strings.TrimSpace(resp.Content) != "" {
			// Persist the model's narration for this turn as its own message.
			// The next turn creates a fresh streaming preview, and the final
			// answer is sent separately.
			if err := chat.Send(runCtx, resp.Content); err != nil {
				e.log.Warn("send intermediate message", "error", err)
			}
		}
		if len(resp.ToolCalls) == 0 {
			final := resp.Content
			if strings.TrimSpace(final) == "" {
				final = "(the model returned an empty response)"
			}
			if err := chat.Send(runCtx, final); err != nil {
				e.log.Warn("send final message", "error", err)
			}
			if e.cfg.Agent.ShowProgress == nil || *e.cfg.Agent.ShowProgress {
				_ = chat.Progress(runCtx, "")
			}
			e.emitStatus(runCtx, chat, RunStatus{Running: false, Phase: "done", Model: modelLabel, Iteration: iter, Tasks: e.taskSnapshot(in.SessionID)})
			return finish(nil)
		}
		results := e.executeTools(runCtx, chat, in, resp.ToolCalls, iter)
		for _, r := range results {
			msg := provider.Message{
				Role:       provider.RoleTool,
				Content:    r.content,
				Name:       r.name,
				ToolCallID: r.id,
			}
			history = append(history, msg)
			out.Messages = append(out.Messages, msg)
		}
		if runCtx.Err() != nil {
			e.emitStatus(runCtx, chat, RunStatus{Running: false, Phase: "stopped", Model: modelLabel, Iteration: iter, Tasks: e.taskSnapshot(in.SessionID)})
			return finish(ErrCancelled)
		}
		e.emitStatus(runCtx, chat, RunStatus{Running: true, Phase: "thinking", Model: modelLabel, Iteration: iter, Tasks: e.taskSnapshot(in.SessionID)})
	}
	msg := fmt.Sprintf("I stopped after %d tool iterations without a final answer. Ask me to continue and I will pick up from here.", maxIter)
	_ = chat.Send(ctx, msg)
	e.emitStatus(ctx, chat, RunStatus{Running: false, Phase: "done", Model: modelLabel, Iteration: maxIter, Tasks: e.taskSnapshot(in.SessionID)})
	final := provider.Message{Role: provider.RoleAssistant, Content: msg}
	history = append(history, final)
	out.Messages = append(out.Messages, final)
	return finish(nil)
}

func (e *Engine) contextWindow(providerName string) int {
	if p, ok := e.providers.Get(providerName); ok {
		if w := p.ContextWindow(); w > 0 {
			return w
		}
	}
	if e.cfg.Agent.ContextWindow > 0 {
		return e.cfg.Agent.ContextWindow
	}
	return 32768
}

type toolResult struct {
	index   int
	id      string
	name    string
	content string
	errText string
}

func (e *Engine) executeTools(ctx context.Context, chat Chat, in RunInput, calls []provider.ToolCall, iter int) []toolResult {
	maxPar := e.cfg.Agent.MaxParallelTools
	if maxPar <= 0 {
		maxPar = 4
	}
	results := make([]toolResult, len(calls))
	sem := make(chan struct{}, maxPar)
	var wg sync.WaitGroup

	for i, call := range calls {
		i, call := i, call
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = toolResult{index: i, id: call.ID, name: call.Function.Name, content: "cancelled before execution"}
				return
			}
			if ctx.Err() != nil {
				results[i] = toolResult{index: i, id: call.ID, name: call.Function.Name, content: "cancelled before execution"}
				return
			}
			// Log every action before running it. The frontend decides whether
			// to display the entry, so the engine does not need a UI gate here.
			action := describeToolCall(call)
			e.emitStatus(ctx, chat, RunStatus{Running: true, Phase: "tool", Action: action, Iteration: iter, Tasks: e.taskSnapshot(in.SessionID)})
			if err := chat.Action(ctx, action); err != nil {
				e.log.Warn("send action log", "tool", call.Function.Name, "error", err)
			}
			content, errText := e.runOneTool(ctx, in, call)
			results[i] = toolResult{index: i, id: call.ID, name: call.Function.Name, content: content, errText: errText}
			if errText != "" && ctx.Err() == nil {
				_ = chat.Error(ctx, "tool "+call.Function.Name+" failed: "+clipRunes(oneLine(errText), 1000))
			}
		}()
	}
	wg.Wait()
	return results
}

// emitStatus publishes a live run snapshot. Status updates are best-effort and
// must still be delivered after the run context is cancelled (the final
// "stopped" state), so cancellation and deadlines are detached.
func (e *Engine) emitStatus(ctx context.Context, chat Chat, st RunStatus) {
	if st.Model == "" {
		name, model := e.Active()
		st.Model = name + ":" + model
	}
	if err := chat.Status(context.WithoutCancel(ctx), st); err != nil {
		e.log.Debug("send status", "error", err)
	}
}

// taskSnapshot returns the current plan for a session, if the todo tool is
// registered. The snapshot is copied, so callers own the result.
func (e *Engine) taskSnapshot(sessionID string) []Task {
	tool, ok := e.tools.Get("todo")
	if !ok {
		return nil
	}
	snap, ok := tool.(interface{ Snapshot(string) []tools.TodoItem })
	if !ok {
		return nil
	}
	items := snap.Snapshot(sessionID)
	out := make([]Task, 0, len(items))
	for _, it := range items {
		out = append(out, Task{Text: it.Text, Status: it.Status})
	}
	return out
}

// Tasks returns the current plan for a session for frontends that show status.
func (e *Engine) Tasks(sessionID string) []Task { return e.taskSnapshot(sessionID) }

func (e *Engine) runOneTool(ctx context.Context, in RunInput, call provider.ToolCall) (content, errText string) {
	defer func() {
		if r := recover(); r != nil {
			content = fmt.Sprintf("tool %q panicked: %v", call.Function.Name, r)
			errText = content
		}
	}()
	tool, ok := e.tools.Get(call.Function.Name)
	if !ok {
		content = fmt.Sprintf("unknown tool %q. Available tools: %s", call.Function.Name, strings.Join(e.tools.Names(), ", "))
		return content, content
	}
	args, err := provider.ParseToolArguments(call.Function.Arguments)
	if err != nil {
		content = "Tool call rejected: " + err.Error() + ". Emit valid JSON arguments and try again."
		return content, content
	}
	tctx := tools.WithSession(ctx, tools.SessionInfo{SessionID: in.SessionID, ChatID: in.ChatID, HomeDir: in.HomeDir})
	tctx = tools.WithSender(tctx, func(sctx context.Context, path string) (string, error) {
		if in.Chat == nil {
			return "", errors.New("no chat attached")
		}
		if err := in.Chat.SendFile(sctx, path, ""); err != nil {
			return "", err
		}
		return "sent " + path, nil
	})
	timeout := e.cfg.Agent.ToolTimeout.D()
	if timeout <= 0 {
		timeout = e.cfg.Tools.CommandTimeout.D()
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		tctx, cancel = context.WithTimeout(tctx, timeout)
		defer cancel()
	}
	res, err := tool.Execute(tctx, args)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(tctx.Err(), context.DeadlineExceeded) {
			content = "Tool execution cancelled or timed out."
			return content, content
		}
		content = "Tool error: " + logx.Redact(err.Error())
		return content, content
	}
	content = res.Content
	if res.IsError {
		content = "ERROR: " + content
		errText = res.Content
	}
	limit := e.cfg.Agent.ToolOutputLimit
	if limit > 0 {
		content = truncateMiddle(content, limit)
	}
	return content, errText
}

// truncateMiddle keeps head and tail of an oversized tool result.
func truncateMiddle(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	marker := fmt.Sprintf("... [%d bytes omitted] ...", len(s)-limit)
	if len(marker) >= limit {
		return s[:limit]
	}
	// The marker's byte count can grow when the omitted length crosses a
	// decimal boundary; stabilize it before splitting the remaining budget.
	for {
		budget := limit - len(marker)
		next := fmt.Sprintf("... [%d bytes omitted] ...", len(s)-budget)
		if next == marker {
			break
		}
		marker = next
		if len(marker) >= limit {
			return s[:limit]
		}
	}
	budget := limit - len(marker)
	head := budget * 2 / 3
	tail := budget - head
	return s[:head] + marker + s[len(s)-tail:]
}

// describeToolCall renders a one-line, content-free summary of a tool call for
// the action log (for example "shell: ls -la (working_dir=/tmp)"). File and
// memory bodies are deliberately never included; only the arguments that
// identify the action are shown.
func describeToolCall(call provider.ToolCall) string {
	name := strings.TrimSpace(call.Function.Name)
	if name == "" {
		name = "tool"
	}
	var args map[string]any
	if raw, err := provider.ParseToolArguments(call.Function.Arguments); err == nil {
		_ = json.Unmarshal(raw, &args)
	}
	// Generic pick order; tools below override it with their own key order.
	keys := []string{"command", "path", "url", "query", "prompt", "name"}
	switch name {
	case "shell":
		keys = []string{"command", "working_dir"}
	case "process":
		keys = []string{"action", "id"}
	case "read_file", "write_file", "edit_file", "list_dir", "send_file":
		keys = []string{"path"}
	case "search_files":
		keys = []string{"query", "glob", "path"}
	case "web_fetch":
		keys = []string{"url"}
	case "web_search", "session_search":
		keys = []string{"query"}
	case "memory":
		keys = []string{"action", "scope"}
	case "skills":
		keys = []string{"action", "name", "url", "path", "query"}
	case "schedule":
		keys = []string{"action", "schedule", "id"}
	case "todo":
		keys = []string{"action", "text"}
	}
	var primary string
	var extras []string
	for _, key := range keys {
		v, ok := args[key]
		if !ok {
			continue
		}
		s := clipRunes(oneLine(fmt.Sprint(v)), 160)
		if s == "" {
			continue
		}
		if primary == "" {
			primary = s
			continue
		}
		extras = append(extras, key+"="+s)
	}
	out := name
	if primary != "" {
		out += ": " + primary
	}
	if len(extras) > 0 {
		out += " (" + strings.Join(extras, ", ") + ")"
	}
	return clipRunes(out, 240)
}

// oneLine collapses whitespace so an action summary stays on a single line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clipRunes truncates s to at most n runes.
func clipRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
