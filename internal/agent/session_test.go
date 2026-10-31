package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitvibe/kestrel/internal/config"
	"github.com/bitvibe/kestrel/internal/provider"
	"github.com/bitvibe/kestrel/internal/store"
	"github.com/bitvibe/kestrel/internal/tools"
)

type blockingProvider struct {
	started chan struct{}
	exited  chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Name() string         { return "test" }
func (p *blockingProvider) DefaultModel() string { return "model" }
func (p *blockingProvider) SupportsTools() bool  { return true }
func (p *blockingProvider) SupportsVision() bool { return false }
func (p *blockingProvider) ContextWindow() int   { return 32768 }
func (p *blockingProvider) ListModels(context.Context) ([]string, error) {
	return []string{"model"}, nil
}
func (p *blockingProvider) Chat(ctx context.Context, _ provider.ChatRequest, _ func(string) error) (provider.ChatResponse, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	close(p.exited)
	return provider.ChatResponse{}, ctx.Err()
}

func TestResetWaitsForRunAndPersistsOldSession(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.Tools.Roots = []string{cfg.Workspace}
	cfg.Agent.ShowProgress = boolPtr(false)
	cfg.Providers = []config.Provider{{Name: "test", BaseURL: "http://localhost", DefaultModel: "model"}}
	st, err := store.Open(store.Config{Path: filepath.Join(t.TempDir(), "store.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	p := &blockingProvider{started: make(chan struct{}), exited: make(chan struct{})}
	providers := provider.NewRegistry()
	providers.Add(p)
	engine, err := NewEngine(cfg, st, tools.NewRegistry(), providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), engine, nil, nil)
	defer func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Submit(ctx, Incoming{ChatID: 12, Text: "old message"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
	case <-ctx.Done():
		t.Fatal("provider did not start")
	}
	oldID := m.Status(12).SessionID
	if err := m.Reset(ctx, 12); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.exited:
	default:
		t.Fatal("reset returned before provider exited")
	}
	newID := m.Status(12).SessionID
	if oldID == newID || newID == "" {
		t.Fatalf("session ids: old=%q new=%q", oldID, newID)
	}
	old, err := st.Messages(ctx, oldID, 10)
	if err != nil || len(old) != 1 || old[0].Content != "old message" {
		t.Fatalf("old session: %+v %v", old, err)
	}
	newMsgs, err := st.Messages(ctx, newID, 10)
	if err != nil || len(newMsgs) != 0 {
		t.Fatalf("new session: %+v %v", newMsgs, err)
	}
}

func boolPtr(v bool) *bool { return &v }

type fixedProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *fixedProvider) Name() string                                 { return "test" }
func (p *fixedProvider) DefaultModel() string                         { return "model" }
func (p *fixedProvider) SupportsTools() bool                          { return true }
func (p *fixedProvider) SupportsVision() bool                         { return false }
func (p *fixedProvider) ContextWindow() int                           { return 32768 }
func (p *fixedProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *fixedProvider) Chat(_ context.Context, _ provider.ChatRequest, _ func(string) error) (provider.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		return provider.ChatResponse{ToolCalls: []provider.ToolCall{
			{ID: "a", Function: provider.FunctionCall{Name: "parallel", Arguments: `{"index":1}`}},
			{ID: "b", Function: provider.FunctionCall{Name: "parallel", Arguments: `{"index":2}`}},
		}}, nil
	}
	return provider.ChatResponse{Content: "done"}, nil
}

type parallelTool struct {
	started chan int
	release chan struct{}
}

func (p *parallelTool) Name() string                { return "parallel" }
func (p *parallelTool) Description() string         { return "test" }
func (p *parallelTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *parallelTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var v struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(args, &v); err != nil {
		return tools.Result{}, err
	}
	p.started <- v.Index
	select {
	case <-p.release:
		return tools.TextResult("result %d", v.Index), nil
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
}

func TestParallelToolsKeepResponseOrder(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.Providers = []config.Provider{{Name: "test", BaseURL: "http://localhost", DefaultModel: "model"}}
	cfg.Agent.ShowProgress = boolPtr(false)
	reg := tools.NewRegistry()
	tool := &parallelTool{started: make(chan int, 2), release: make(chan struct{})}
	reg.Register(tool)
	providers := provider.NewRegistry()
	providers.Add(&fixedProvider{})
	engine, err := NewEngine(cfg, nil, reg, providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan RunOutput, 1)
	go func() {
		done <- engine.Run(ctx, RunInput{Chat: NopChat{}, HomeDir: cfg.Workspace, User: provider.Message{Role: provider.RoleUser, Content: "go"}})
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-tool.started:
		case <-ctx.Done():
			t.Fatal("tools did not run concurrently")
		}
	}
	close(tool.release)
	select {
	case out := <-done:
		if out.Err != nil {
			t.Fatal(out.Err)
		}
		if len(out.Messages) != 5 || out.Messages[2].Content != "result 1" || out.Messages[3].Content != "result 2" || out.Messages[4].Content != "done" {
			t.Fatalf("unordered response: %+v", out.Messages)
		}
	case <-ctx.Done():
		t.Fatal(errors.New("agent loop timed out"))
	}
}

func TestManagerCloseCancelsRunAndCanBeRetried(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.Tools.Roots = []string{cfg.Workspace}
	cfg.Agent.ShowProgress = boolPtr(false)
	cfg.Providers = []config.Provider{{Name: "test", BaseURL: "http://localhost", DefaultModel: "model"}}
	p := &blockingProvider{started: make(chan struct{}), exited: make(chan struct{})}
	providers := provider.NewRegistry()
	providers.Add(p)
	engine, err := NewEngine(cfg, nil, tools.NewRegistry(), providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), engine, nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Submit(ctx, Incoming{ChatID: 12, Text: "wait"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
	case <-ctx.Done():
		t.Fatal("provider did not start")
	}
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := m.Close(expired); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("first close: %v", err)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	select {
	case <-p.exited:
	default:
		t.Fatal("close returned before provider exited")
	}
	if err := m.Submit(ctx, Incoming{ChatID: 12, Text: "late"}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("submit after close: %v", err)
	}
}

func BenchmarkTrimToolOutputs(b *testing.B) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "inspect output"},
		{Role: provider.RoleTool, Content: strings.Repeat("output line\n", 8192)},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		trimmed := trimToolOutputs(msgs, 4096)
		if len(trimmed[1].Content) > 4096 {
			b.Fatal("tool output was not trimmed")
		}
	}
}

func TestTruncateMiddleHonorsByteLimit(t *testing.T) {
	content := strings.Repeat("0123456789", 100)
	for _, limit := range []int{1, 8, 32, 64, 100, 999} {
		got := truncateMiddle(content, limit)
		if len(got) > limit {
			t.Errorf("limit %d: got %d bytes", limit, len(got))
		}
		if limit >= 64 && (!strings.HasPrefix(got, "012") || !strings.HasSuffix(got, "789")) {
			t.Errorf("limit %d: head or tail missing: %q", limit, got)
		}
	}
}

func TestDescribeToolCall(t *testing.T) {
	cases := []struct {
		name string
		call provider.ToolCall
		want string
	}{
		{
			name: "shell shows command and directory",
			call: provider.ToolCall{Function: provider.FunctionCall{Name: "shell", Arguments: `{"command":"ls -la","working_dir":"/tmp"}`}},
			want: "shell: ls -la (working_dir=/tmp)",
		},
		{
			name: "write_file hides content",
			call: provider.ToolCall{Function: provider.FunctionCall{Name: "write_file", Arguments: `{"path":"notes.txt","content":"TOP SECRET"}`}},
			want: "write_file: notes.txt",
		},
		{
			name: "multiline arguments collapse",
			call: provider.ToolCall{Function: provider.FunctionCall{Name: "web_search", Arguments: `{"query":"go\n  modules"}`}},
			want: "web_search: go modules",
		},
		{
			name: "repaired json is accepted",
			call: provider.ToolCall{Function: provider.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt",}`}},
			want: "read_file: a.txt",
		},
		{
			name: "invalid json falls back to the name",
			call: provider.ToolCall{Function: provider.FunctionCall{Name: "shell", Arguments: "not-json"}},
			want: "shell",
		},
		{
			name: "skills shows action and install target",
			call: provider.ToolCall{Function: provider.FunctionCall{Name: "skills", Arguments: `{"action":"install","name":"demo","url":"owner/repo","path":"/tmp/skill","query":"pdf"}`}},
			want: "skills: install (name=demo, url=owner/repo, path=/tmp/skill, query=pdf)",
		},
		{
			name: "missing name becomes tool",
			call: provider.ToolCall{},
			want: "tool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeToolCall(tc.call); got != tc.want {
				t.Fatalf("describeToolCall = %q, want %q", got, tc.want)
			}
		})
	}
	long := describeToolCall(provider.ToolCall{Function: provider.FunctionCall{
		Name:      "shell",
		Arguments: `{"command":"` + strings.Repeat("x", 500) + `"}`,
	}})
	if n := len([]rune(long)); n > 240 {
		t.Fatalf("action summary too long: %d runes", n)
	}
}

type loopProvider struct {
	mu    sync.Mutex
	calls int
	// limit is the number of tool-call turns before the provider answers
	// with a final message. Zero means it never finishes on its own.
	limit int
}

func (p *loopProvider) Name() string                                 { return "test" }
func (p *loopProvider) DefaultModel() string                         { return "model" }
func (p *loopProvider) SupportsTools() bool                          { return true }
func (p *loopProvider) SupportsVision() bool                         { return false }
func (p *loopProvider) ContextWindow() int                           { return 32768 }
func (p *loopProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *loopProvider) Chat(_ context.Context, _ provider.ChatRequest, _ func(string) error) (provider.ChatResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if p.limit > 0 && n > p.limit {
		return provider.ChatResponse{Content: "final"}, nil
	}
	return provider.ChatResponse{ToolCalls: []provider.ToolCall{
		{ID: "call", Function: provider.FunctionCall{Name: "noop", Arguments: "{}"}},
	}}, nil
}

type noopTool struct{}

func (noopTool) Name() string                { return "noop" }
func (noopTool) Description() string         { return "test" }
func (noopTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (noopTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.TextResult("ok"), nil
}

func newLoopEngine(t *testing.T, cfg *config.Config, limit int) *Engine {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(noopTool{})
	providers := provider.NewRegistry()
	providers.Add(&loopProvider{limit: limit})
	engine, err := NewEngine(cfg, nil, reg, providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestZeroMaxIterationsMeansUnlimited(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.Providers = []config.Provider{{Name: "test", BaseURL: "http://localhost", DefaultModel: "model"}}
	cfg.Agent.MaxIterations = 0
	engine := newLoopEngine(t, cfg, 25)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := engine.Run(ctx, RunInput{Chat: NopChat{}, HomeDir: cfg.Workspace, User: provider.Message{Role: provider.RoleUser, Content: "go"}})
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	if out.Iterations != 26 {
		t.Fatalf("iterations = %d, want 26; the run stopped early", out.Iterations)
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Content != "final" {
		t.Fatalf("final message = %q", last.Content)
	}
}

func TestPositiveMaxIterationsStillCaps(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.Providers = []config.Provider{{Name: "test", BaseURL: "http://localhost", DefaultModel: "model"}}
	cfg.Agent.MaxIterations = 3
	engine := newLoopEngine(t, cfg, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := engine.Run(ctx, RunInput{Chat: NopChat{}, HomeDir: cfg.Workspace, User: provider.Message{Role: provider.RoleUser, Content: "go"}})
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	if out.Iterations != 3 {
		t.Fatalf("iterations = %d, want 3", out.Iterations)
	}
	last := out.Messages[len(out.Messages)-1]
	if !strings.Contains(last.Content, "stopped after 3") {
		t.Fatalf("cap message = %q", last.Content)
	}
}

type planProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *planProvider) Name() string                                 { return "test" }
func (p *planProvider) DefaultModel() string                         { return "model" }
func (p *planProvider) SupportsTools() bool                          { return true }
func (p *planProvider) SupportsVision() bool                         { return false }
func (p *planProvider) ContextWindow() int                           { return 32768 }
func (p *planProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *planProvider) Chat(_ context.Context, _ provider.ChatRequest, _ func(string) error) (provider.ChatResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	switch n {
	case 1:
		return provider.ChatResponse{ToolCalls: []provider.ToolCall{
			{ID: "plan", Function: provider.FunctionCall{Name: "todo", Arguments: `{"action":"add","items":["Inspect repository","Write tests"]}`}},
		}}, nil
	case 2:
		return provider.ChatResponse{ToolCalls: []provider.ToolCall{
			{ID: "done-1", Function: provider.FunctionCall{Name: "todo", Arguments: `{"action":"update","id":1,"status":"completed"}`}},
		}}, nil
	default:
		return provider.ChatResponse{Content: "final"}, nil
	}
}

func TestRunEmitsLiveStatusAndTaskPlan(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.Providers = []config.Provider{{Name: "test", BaseURL: "http://localhost", DefaultModel: "model"}}
	cfg.Agent.ShowProgress = boolPtr(false)
	reg := tools.NewRegistry()
	reg.Register(&tools.TodoTool{})
	providers := provider.NewRegistry()
	providers.Add(&planProvider{})
	engine, err := NewEngine(cfg, nil, reg, providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	chat := &recordingChat{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := engine.Run(ctx, RunInput{
		Chat:      chat,
		SessionID: "session-1",
		HomeDir:   cfg.Workspace,
		User:      provider.Message{Role: provider.RoleUser, Content: "go"},
	})
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	statuses := chat.statusSnapshot()
	if len(statuses) < 4 {
		t.Fatalf("status updates = %d, want at least 4", len(statuses))
	}
	seen := map[string]bool{}
	for _, st := range statuses {
		seen[st.Phase] = true
		if st.Model == "" {
			t.Error("status update has no model")
		}
	}
	for _, phase := range []string{"starting", "thinking", "tool", "done"} {
		if !seen[phase] {
			t.Errorf("missing status phase %q (got %v)", phase, seen)
		}
	}
	last := statuses[len(statuses)-1]
	if last.Running || last.Phase != "done" {
		t.Fatalf("final status = %+v", last)
	}
	if len(last.Tasks) != 2 {
		t.Fatalf("final tasks = %+v", last.Tasks)
	}
	if last.Tasks[0].Status != "completed" || last.Tasks[1].Status != "pending" {
		t.Fatalf("final task states = %+v", last.Tasks)
	}
	if tasks := engine.Tasks("session-1"); len(tasks) != 2 || tasks[0].Text != "Inspect repository" {
		t.Fatalf("engine.Tasks = %+v", tasks)
	}
}

type recordingChat struct {
	mu       sync.Mutex
	events   []string
	statuses []RunStatus
}

func (c *recordingChat) add(kind, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, kind+": "+text)
}

func (c *recordingChat) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func (c *recordingChat) statusSnapshot() []RunStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]RunStatus(nil), c.statuses...)
}

func (c *recordingChat) Send(_ context.Context, text string) error { c.add("send", text); return nil }
func (c *recordingChat) Update(context.Context, string) error      { return nil }
func (c *recordingChat) Progress(context.Context, string) error    { return nil }
func (c *recordingChat) Status(_ context.Context, st RunStatus) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses = append(c.statuses, st)
	return nil
}
func (c *recordingChat) Action(_ context.Context, text string) error {
	c.add("action", text)
	return nil
}
func (c *recordingChat) Think(_ context.Context, text string) error     { c.add("think", text); return nil }
func (c *recordingChat) Error(_ context.Context, text string) error     { c.add("error", text); return nil }
func (c *recordingChat) Typing(context.Context) error                   { return nil }
func (c *recordingChat) SendFile(context.Context, string, string) error { return nil }

type activityProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *activityProvider) Name() string                                 { return "test" }
func (p *activityProvider) DefaultModel() string                         { return "model" }
func (p *activityProvider) SupportsTools() bool                          { return true }
func (p *activityProvider) SupportsVision() bool                         { return false }
func (p *activityProvider) ContextWindow() int                           { return 32768 }
func (p *activityProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *activityProvider) Chat(context.Context, provider.ChatRequest, func(string) error) (provider.ChatResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		return provider.ChatResponse{
			Reasoning: "I should write the report.",
			Content:   "Let me write the report.",
			ToolCalls: []provider.ToolCall{{
				ID:       "call-1",
				Function: provider.FunctionCall{Name: "failtool", Arguments: `{"path":"report.txt"}`},
			}},
		}, nil
	}
	return provider.ChatResponse{
		Reasoning: "The write failed; I will report it.",
		Content:   "The write failed.",
	}, nil
}

type failTool struct{}

func (failTool) Name() string                { return "failtool" }
func (failTool) Description() string         { return "test" }
func (failTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (failTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.ErrorResult("disk is full"), nil
}

func TestRunShowsReasoningNarrationActionsAndErrors(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.Providers = []config.Provider{{Name: "test", BaseURL: "http://localhost", DefaultModel: "model"}}
	reg := tools.NewRegistry()
	reg.Register(failTool{})
	providers := provider.NewRegistry()
	providers.Add(&activityProvider{})
	engine, err := NewEngine(cfg, nil, reg, providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	chat := &recordingChat{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := engine.Run(ctx, RunInput{Chat: chat, HomeDir: cfg.Workspace, User: provider.Message{Role: provider.RoleUser, Content: "go"}})
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	want := []string{
		"think: I should write the report.",
		"send: Let me write the report.",
		"action: failtool: report.txt",
		"error: tool failtool failed: disk is full",
		"think: The write failed; I will report it.",
		"send: The write failed.",
	}
	got := chat.snapshot()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("activity feed:\n got: %q\nwant: %q", got, want)
	}
}

type failingProvider struct{}

func (failingProvider) Name() string                                 { return "test" }
func (failingProvider) DefaultModel() string                         { return "model" }
func (failingProvider) SupportsTools() bool                          { return true }
func (failingProvider) SupportsVision() bool                         { return false }
func (failingProvider) ContextWindow() int                           { return 32768 }
func (failingProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (failingProvider) Chat(context.Context, provider.ChatRequest, func(string) error) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errors.New("provider is down")
}

func TestProviderErrorIsSentToChat(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.Providers = []config.Provider{{Name: "test", BaseURL: "http://localhost", DefaultModel: "model"}}
	providers := provider.NewRegistry()
	providers.Add(failingProvider{})
	engine, err := NewEngine(cfg, nil, tools.NewRegistry(), providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	chat := &recordingChat{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := engine.Run(ctx, RunInput{Chat: chat, HomeDir: cfg.Workspace, User: provider.Message{Role: provider.RoleUser, Content: "go"}})
	if out.Err == nil {
		t.Fatal("expected provider failure")
	}
	got := chat.snapshot()
	if len(got) != 1 || !strings.HasPrefix(got[0], "error: The model request failed: ") {
		t.Fatalf("error feed = %q", got)
	}
}
