// Package tools implements the built-in tools available to the agent.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bitvibe/kestrel/internal/config"
	"github.com/bitvibe/kestrel/internal/netx"
	"github.com/bitvibe/kestrel/internal/provider"
	"github.com/bitvibe/kestrel/internal/store"
)

// Result is the model-visible outcome of a tool call.
type Result struct {
	Content string
	IsError bool
}

// TextResult builds a successful text result.
func TextResult(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...)}
}

// ErrorResult builds a result the model should see as a recoverable error.
func ErrorResult(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// Tool is a model-callable function.
type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

// Registry holds tools by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds or replaces a tool.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// Get looks up a tool.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names returns tool names in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Definitions converts tools to OpenAI function definitions.
func (r *Registry) Definitions() []provider.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		params := t.Parameters()
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, provider.Tool{
			Type: "function",
			Function: provider.FunctionDef{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  params,
			},
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Function.Name < out[j].Function.Name })
	return out
}

// Close stops resources owned by registered tools. Tools sharing a process
// manager are safe to close more than once.
func (r *Registry) Close() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var errs []error
	for _, name := range r.order {
		if closer, ok := r.tools[name].(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Deps contains shared dependencies for built-in tools.
type Deps struct {
	Config    *config.Config
	Store     *store.Store
	HTTP      *netx.Factory
	ProxySpec string
	Logger    *slog.Logger
	Approver  Approver
}

// SessionInfo identifies the conversation for session-scoped tools.
type SessionInfo struct {
	SessionID string
	ChatID    int64
	HomeDir   string
}

type sessionKey struct{}

// WithSession attaches session metadata to a tool context.
func WithSession(ctx context.Context, info SessionInfo) context.Context {
	return context.WithValue(ctx, sessionKey{}, info)
}

// SessionFromContext returns session metadata.
func SessionFromContext(ctx context.Context) (SessionInfo, bool) {
	v, ok := ctx.Value(sessionKey{}).(SessionInfo)
	return v, ok
}

// Sender delivers a local file to the user (Telegram/CLI).
type Sender func(ctx context.Context, path string) (string, error)

type senderKey struct{}

// WithSender attaches a file sender to the context.
func WithSender(ctx context.Context, s Sender) context.Context {
	return context.WithValue(ctx, senderKey{}, s)
}

// SenderFromContext returns the attached sender.
func SenderFromContext(ctx context.Context) (Sender, bool) {
	v, ok := ctx.Value(senderKey{}).(Sender)
	return v, ok && v != nil
}

// ApprovalRequest asks a human to approve one dangerous action.
type ApprovalRequest struct {
	ChatID    int64
	SessionID string
	Command   string
	Reason    string
	Timeout   time.Duration
}

// Approver presents approval requests to the user. A nil Approver denies.
type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) (bool, error)
}

// ApprovalPolicy implements ask/allowlist/off semantics.
type ApprovalPolicy struct {
	Mode      string
	Timeout   time.Duration
	AllowList []string
	Approver  Approver
}

// Authorize decides whether a command may run. In allowlist mode, dangerous
// commands must match one of the allow patterns; everything else is denied.
// Safe commands are always allowed.
func (p ApprovalPolicy) Authorize(ctx context.Context, info SessionInfo, command string) (bool, string) {
	reason, dangerous := DetectDangerous(command)
	if !dangerous {
		return true, ""
	}
	switch strings.ToLower(p.Mode) {
	case "off":
		return true, reason
	case "allowlist":
		for _, pattern := range p.AllowList {
			if pattern == "" {
				continue
			}
			if matched, _ := matchPattern(pattern, command); matched {
				return true, reason
			}
		}
		return false, "command blocked by allowlist policy: " + reason
	default: // ask
		if p.Approver == nil {
			return false, "approval required but no approval channel is available: " + reason
		}
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = 2 * time.Minute
		}
		actx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ok, err := p.Approver.Approve(actx, ApprovalRequest{
			ChatID:    info.ChatID,
			SessionID: info.SessionID,
			Command:   command,
			Reason:    reason,
			Timeout:   timeout,
		})
		if err != nil || !ok {
			return false, "command denied: " + reason
		}
		return true, reason
	}
}

func matchPattern(pattern, command string) (bool, error) {
	// Patterns are regular expressions; a plain substring is also accepted.
	if strings.Contains(command, pattern) {
		return true, nil
	}
	return matchRegexp(pattern, command)
}

// DecodeArgs unmarshals tool arguments, returning a model-visible error on
// failure.
func DecodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// NewBuiltins constructs the standard toolset.
func NewBuiltins(deps Deps) *Registry {
	r := NewRegistry()
	cfg := deps.Config
	if cfg == nil {
		return r
	}
	policy := ApprovalPolicy{
		Mode:      cfg.Tools.Approval,
		Timeout:   cfg.Tools.ApprovalTimeout.D(),
		AllowList: cfg.Tools.AllowCommands,
		Approver:  deps.Approver,
	}
	if !cfg.Tools.DisableShell {
		var processes *ProcessManager
		if cfg.Tools.Background == nil || *cfg.Tools.Background {
			processes = NewProcessManager(cfg.Workspace, deps.ProxySpec, 8)
		}
		r.Register(&ShellTool{Deps: deps, Policy: policy, Processes: processes})
		if processes != nil {
			r.Register(&ProcessTool{Deps: deps, Processes: processes})
		}
	}
	roots := cfg.Tools.Roots
	r.Register(&ReadFileTool{Roots: roots})
	if !cfg.Tools.DisableFileWrites {
		r.Register(&WriteFileTool{Roots: roots, Enabled: true})
		r.Register(&EditFileTool{Roots: roots, Enabled: true})
	}
	r.Register(&ListDirTool{Roots: roots})
	r.Register(&SearchFilesTool{Roots: roots, Workspace: cfg.Workspace})
	r.Register(&WebFetchTool{Deps: deps, MaxBytes: cfg.Tools.WebFetchMaxBytes, Timeout: cfg.Tools.WebFetchTimeout.D()})
	if cfg.Tools.SearchBackend != "" && cfg.Tools.SearchBackend != "none" {
		r.Register(&WebSearchTool{Deps: deps})
	}
	if cfg.Memory.Enabled {
		r.Register(&MemoryTool{Store: deps.Store, Dir: cfg.Memory.Dir, MaxBytes: 32 * 1024})
	}
	if cfg.Skills.Enabled {
		r.Register(&SkillsTool{Dir: cfg.Skills.Dir, Deps: deps})
	}
	if deps.Store != nil {
		r.Register(&SessionSearchTool{Store: deps.Store})
		if cfg.Cron.Enabled {
			r.Register(&ScheduleTool{Store: deps.Store})
		}
	}
	r.Register(&TodoTool{})
	if cfg.Tools.AllowSendFile == nil || *cfg.Tools.AllowSendFile {
		r.Register(&SendFileTool{Roots: roots, MaxBytes: 48 * 1024 * 1024})
	}
	return r
}
