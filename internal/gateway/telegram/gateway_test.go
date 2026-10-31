package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitvibe/kestrel/internal/agent"
	"github.com/bitvibe/kestrel/internal/config"
	"github.com/bitvibe/kestrel/internal/netx"
	"github.com/bitvibe/kestrel/internal/provider"
	"github.com/bitvibe/kestrel/internal/tools"
)

func TestUnauthorizedUpdatesAreSilent(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":22}}`))
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Telegram.Token = "123:testing"
	cfg.Telegram.APIBaseURL = srv.URL
	cfg.Telegram.AllowedUsers = []int64{1}
	cfg.Proxy = "none"
	g, err := New(cfg, nil, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	g.handleUpdate(ctx, Update{Message: &Message{Chat: TelegramChat{ID: 99}, From: &User{ID: 2}, Text: "/start"}})
	g.handleUpdate(ctx, Update{CallbackQuery: &CallbackQuery{ID: "q", From: &User{ID: 2}, Data: "apv:abc:yes"}})
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 0 {
		t.Fatalf("unauthorized Bot API calls: %v", methods)
	}
}

func TestPollingMockProviderEnvironmentConfiguration(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var delivered bool
	answer := make(chan string, 1)
	releasePoll := make(chan struct{})
	var replied atomic.Bool
	providerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"mock answer\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer providerAPI.Close()
	botAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		mu.Lock()
		calls = append(calls, method)
		first := !delivered && method == "getUpdates"
		if first {
			delivered = true
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"username":"mockbot"}}`)
		case "getUpdates":
			if first {
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":77},"from":{"id":999},"text":"ignored"}},{"update_id":2,"message":{"chat":{"id":42},"from":{"id":42},"text":"hello"}}]}`)
			} else {
				select {
				case <-releasePoll:
				case <-r.Context().Done():
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			}
		case "sendMessage", "editMessageText":
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if strings.Contains(payload.Text, "mock answer") {
				select {
				case answer <- payload.Text:
				default:
				}
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":3}}`)
			if strings.Contains(payload.Text, "mock answer") {
				replied.Store(true)
			}
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	defer botAPI.Close()
	t.Setenv("AGENT_DATA_DIR", t.TempDir())
	t.Setenv("AGENT_WORKSPACE", t.TempDir())
	t.Setenv("AGENT_TELEGRAM_TOKEN", "123:testing")
	t.Setenv("AGENT_TELEGRAM_ALLOWED_USERS", "42")
	t.Setenv("AGENT_TELEGRAM_API_BASE_URL", botAPI.URL)
	t.Setenv("AGENT_PROVIDER_BASE_URL", providerAPI.URL+"/v1")
	t.Setenv("AGENT_PROVIDER_MODEL", "mock")
	t.Setenv("AGENT_PROXY", "none")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateGateway(); err != nil {
		t.Fatal(err)
	}
	disabled := false
	cfg.Telegram.Progress = &disabled
	cfg.Telegram.Stream = &disabled
	factory := netx.NewFactory(netx.Config{})
	defer factory.CloseIdle()
	httpClient, err := factory.Client("none", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := provider.NewClient(provider.Options{
		Name: cfg.Providers[0].Name, BaseURL: cfg.Providers[0].BaseURL,
		DefaultModel: cfg.Providers[0].DefaultModel, NativeTools: true, Streaming: true,
	}, httpClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	providers := provider.NewRegistry()
	providers.Add(p)
	engine, err := agent.NewEngine(cfg, nil, tools.NewRegistry(), providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg, nil, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := agent.NewManager(ctx, engine, g.Resolver(), nil)
	g.Attach(engine, m)
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()
	select {
	case got := <-answer:
		if !strings.Contains(got, "mock answer") {
			t.Fatalf("unexpected answer: %q", got)
		}
	case err := <-done:
		t.Fatalf("gateway exited before answer: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for !replied.Load() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	close(releasePoll)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not stop")
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := m.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !delivered {
		t.Fatal("polling did not deliver updates")
	}
	for _, call := range calls {
		if call == "answerCallbackQuery" {
			t.Fatal("unauthorized update received callback response")
		}
	}
}

func TestActionLoggingAndGating(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		texts = append(texts, payload.Text)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":5}}`)
	}))
	defer srv.Close()
	newGateway := func(logActions *bool) *Gateway {
		cfg := config.Default()
		cfg.Telegram.Token = "123:testing"
		cfg.Telegram.APIBaseURL = srv.URL
		cfg.Telegram.AllowedUsers = []int64{1}
		cfg.Telegram.LogActions = logActions
		cfg.Proxy = "none"
		g, err := New(cfg, nil, nil, "test")
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	g := newGateway(nil)
	if err := g.chat(42).Action(ctx, "shell: ls -la"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), texts...)
	texts = nil
	mu.Unlock()
	if len(got) != 1 || got[0] != "⚙️ shell: ls -la" {
		t.Fatalf("action messages = %q", got)
	}

	disabled := false
	g2 := newGateway(&disabled)
	if err := g2.chat(43).Action(ctx, "shell: rm -rf /"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 0 {
		t.Fatalf("log_actions=false sent %q", texts)
	}
}

func TestReasoningAndErrorMessages(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		texts = append(texts, payload.Text)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":9}}`)
	}))
	defer srv.Close()
	newGateway := func(logActions, showReasoning *bool) *Gateway {
		cfg := config.Default()
		cfg.Telegram.Token = "123:testing"
		cfg.Telegram.APIBaseURL = srv.URL
		cfg.Telegram.AllowedUsers = []int64{1}
		cfg.Telegram.LogActions = logActions
		cfg.Telegram.ShowReasoning = showReasoning
		cfg.Proxy = "none"
		g, err := New(cfg, nil, nil, "test")
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	g := newGateway(nil, nil)
	chat := g.chat(50)
	if err := chat.Think(ctx, "checking the workspace"); err != nil {
		t.Fatal(err)
	}
	if err := chat.Error(ctx, "tool shell failed: boom"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), texts...)
	texts = nil
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("messages = %q", got)
	}
	if !strings.Contains(got[0], "Thinking") || !strings.Contains(got[0], "checking the workspace") {
		t.Fatalf("reasoning message = %q", got[0])
	}
	if !strings.Contains(got[1], "❌ tool shell failed: boom") {
		t.Fatalf("error message = %q", got[1])
	}

	disabled := false
	g2 := newGateway(&disabled, &disabled)
	chat2 := g2.chat(51)
	if err := chat2.Think(ctx, "hidden reasoning"); err != nil {
		t.Fatal(err)
	}
	if err := chat2.Error(ctx, "still visible"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 1 || !strings.Contains(texts[0], "still visible") {
		t.Fatalf("disabled messages = %q", texts)
	}
}

func TestApprovalCallbackChecksIdentityBeforeConsuming(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "sendMessage") {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 7}})
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Telegram.Token = "123:testing"
	cfg.Telegram.APIBaseURL = srv.URL
	cfg.Telegram.AllowedUsers = []int64{1}
	cfg.Proxy = "none"
	g, err := New(cfg, nil, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	decided := make(chan bool, 1)
	go func() {
		ok, _ := g.approve.Approve(ctx, tools.ApprovalRequest{ChatID: 99, Command: "ls"})
		decided <- ok
	}()
	var token string
	for ctx.Err() == nil {
		g.approve.mu.Lock()
		for key := range g.approve.pending {
			token = key
		}
		g.approve.mu.Unlock()
		if token != "" {
			mu.Lock()
			ready := len(calls) > 0
			mu.Unlock()
			if ready {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	callback := func(id, chatID int64) *CallbackQuery {
		return &CallbackQuery{ID: "q", From: &User{ID: id}, Message: &Message{Chat: TelegramChat{ID: chatID}}, Data: "apv:" + token + ":yes"}
	}
	g.handleUpdate(ctx, Update{CallbackQuery: callback(2, 99)})
	g.approve.mu.Lock()
	answered := g.approve.pending[token].answered
	g.approve.mu.Unlock()
	if answered {
		t.Fatal("unauthorized callback consumed approval")
	}
	g.approve.HandleCallback(ctx, callback(1, 100))
	g.approve.mu.Lock()
	answered = g.approve.pending[token].answered
	g.approve.mu.Unlock()
	if answered {
		t.Fatal("wrong chat consumed approval")
	}
	g.handleUpdate(ctx, Update{CallbackQuery: callback(1, 99)})
	select {
	case ok := <-decided:
		if !ok {
			t.Fatal("approval rejected")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
