package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitvibe/kestrel/internal/agent"
	"github.com/bitvibe/kestrel/internal/config"
	"github.com/bitvibe/kestrel/internal/store"
)

type botCall struct {
	Method              string `json:"-"`
	ChatID              int64  `json:"chat_id"`
	MessageID           int    `json:"message_id"`
	DraftID             int64  `json:"draft_id"`
	Text                string `json:"text"`
	ParseMode           string `json:"parse_mode"`
	CanStop             bool   `json:"can_stop"`
	DisableNotification bool   `json:"disable_notification"`
}

type botRecorder struct {
	mu         sync.Mutex
	calls      []botCall
	nextID     int
	draftError bool
}

func (r *botRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		method := req.URL.Path[strings.LastIndexByte(req.URL.Path, '/')+1:]
		body, _ := io.ReadAll(req.Body)
		call := botCall{Method: method}
		_ = json.Unmarshal(body, &call)

		r.mu.Lock()
		r.calls = append(r.calls, call)
		id := 0
		if method == "sendMessage" {
			r.nextID++
			id = r.nextID
		}
		draftErr := r.draftError
		r.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if method == "sendMessageDraft" && draftErr {
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: method is not available"}`)
			return
		}
		if method == "sendMessage" {
			_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, id)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}
}

func (r *botRecorder) snapshot() []botCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]botCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *botRecorder) callsOf(method string) []botCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []botCall
	for _, call := range r.calls {
		if call.Method == method {
			out = append(out, call)
		}
	}
	return out
}

func (r *botRecorder) waitFor(t *testing.T, method string, n int, timeout time.Duration) botCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		calls := r.callsOf(method)
		if len(calls) >= n {
			return calls[n-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s call %d; got %d", method, n, len(calls))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newTestGateway(t *testing.T, rec *botRecorder, st *store.Store, tweak func(*config.Config)) *Gateway {
	t.Helper()
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Telegram.Token = "123:testing"
	cfg.Telegram.APIBaseURL = srv.URL
	cfg.Proxy = "none"
	if tweak != nil {
		tweak(cfg)
	}
	g, err := New(cfg, st, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(store.Config{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := st.Close(ctx); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func TestStoppedMessageGenerationUpdateDecodes(t *testing.T) {
	raw := `{"update_id":9,"stopped_message_generation":{"chat":{"id":42,"type":"private"},"message_thread_id":3,"draft_id":77}}`
	var update Update
	if err := json.Unmarshal([]byte(raw), &update); err != nil {
		t.Fatal(err)
	}
	if update.MessageGenerationStopped == nil {
		t.Fatal("stopped_message_generation was not decoded")
	}
	if update.MessageGenerationStopped.Chat.ID != 42 || update.MessageGenerationStopped.DraftID != 77 {
		t.Fatalf("decoded update = %+v", update.MessageGenerationStopped)
	}
}

func TestAllowedUpdatesIncludeStoppedGeneration(t *testing.T) {
	var mu sync.Mutex
	var allowed [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Allowed []string `json:"allowed_updates"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		allowed = append(allowed, payload.Allowed)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	defer srv.Close()

	client := NewClient("123:testing", srv.URL, srv.Client(), nil)
	ctx := context.Background()
	if _, err := client.GetUpdates(ctx, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := client.SetWebhook(ctx, "https://example.com/hook", "secret"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(allowed) != 2 {
		t.Fatalf("allowed_updates requests = %d, want 2", len(allowed))
	}
	for _, list := range allowed {
		found := false
		for _, name := range list {
			if name == "stopped_message_generation" {
				found = true
			}
		}
		if !found {
			t.Fatalf("allowed_updates = %v, missing stopped_message_generation", list)
		}
	}
}

func TestStatusBoardRendering(t *testing.T) {
	board := newStatusBoard(&Gateway{}, 1)
	board.started = time.Now().Add(-65 * time.Second)

	active := board.renderLocked(agent.RunStatus{
		Running:   true,
		Phase:     "tool",
		Action:    "shell: go test ./...",
		Model:     "cloud:gpt-test",
		Iteration: 3,
		Tasks: []agent.Task{
			{Text: "Inspect <repo>", Status: "completed"},
			{Text: "Write tests", Status: "in_progress"},
			{Text: "Ship", Status: "pending"},
		},
	})
	for _, want := range []string{
		"🟢 <b>Active</b>",
		"1m 05s",
		"Model: <code>cloud:gpt-test</code>",
		"Step 3",
		"⚙️ shell: go test ./...",
		"✅ <s>Inspect &lt;repo&gt;</s>",
		"🔄 Write tests",
		"⬜ Ship",
	} {
		if !strings.Contains(active, want) {
			t.Errorf("active status missing %q:\n%s", want, active)
		}
	}

	failed := board.renderLocked(agent.RunStatus{Running: false, Phase: "error", Action: "provider failed"})
	if !strings.Contains(failed, "❌ <b>Error</b>") || !strings.Contains(failed, "⚠️ provider failed") {
		t.Fatalf("error status = %q", failed)
	}
	stopped := board.renderLocked(agent.RunStatus{Running: false, Phase: "stopped"})
	if !strings.Contains(stopped, "⏹ <b>Stopped</b>") {
		t.Fatalf("stopped status = %q", stopped)
	}

	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{65 * time.Second, "1m 05s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h 02m 03s"},
	}
	for _, tc := range cases {
		if got := formatElapsed(tc.d); got != tc.want {
			t.Errorf("formatElapsed(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestStatusBoardPinnedLifecycleAndPersistence(t *testing.T) {
	rec := &botRecorder{nextID: 100}
	st := newTestStore(t)
	g := newTestGateway(t, rec, st, nil)
	ctx := context.Background()
	chat := g.chat(1001)

	starting := agent.RunStatus{Running: true, Phase: "starting", Model: "gpt-test"}
	if err := chat.Status(ctx, starting); err != nil {
		t.Fatalf("starting status: %v", err)
	}
	sends := rec.callsOf("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("status messages = %d, want 1", len(sends))
	}
	if sends[0].ChatID != 1001 {
		t.Fatalf("status chat = %d", sends[0].ChatID)
	}
	if !strings.Contains(sends[0].Text, "🟢 <b>Active</b>") || !strings.Contains(sends[0].Text, "🚀 Starting…") {
		t.Fatalf("starting status text = %q", sends[0].Text)
	}
	pins := rec.callsOf("pinChatMessage")
	if len(pins) != 1 || pins[0].MessageID != 101 {
		t.Fatalf("pins = %+v, want message 101", pins)
	}

	final := agent.RunStatus{
		Running: false,
		Phase:   "done",
		Model:   "gpt-test",
		Tasks: []agent.Task{
			{Text: "Completed", Status: "completed"},
			{Text: "Working", Status: "in_progress"},
			{Text: "Pending", Status: "pending"},
		},
	}
	if err := chat.Status(ctx, final); err != nil {
		t.Fatalf("final status: %v", err)
	}
	edits := rec.callsOf("editMessageText")
	if len(edits) != 1 || edits[0].MessageID != 101 {
		t.Fatalf("final edits = %+v, want edit of 101", edits)
	}
	for _, want := range []string{
		"✅ <b>Done</b>",
		"<b>Plan</b>",
		"✅ <s>Completed</s>",
		"🔄 Working",
		"⬜ Pending",
	} {
		if !strings.Contains(edits[0].Text, want) {
			t.Errorf("final status missing %q:\n%s", want, edits[0].Text)
		}
	}
	if got := len(rec.callsOf("sendMessage")); got != 1 {
		t.Fatalf("final status created a new message (sends=%d)", got)
	}

	// A fresh gateway (as after a restart) reuses the persisted message ID.
	g2 := newTestGateway(t, rec, st, nil)
	if err := g2.chat(1001).Status(ctx, starting); err != nil {
		t.Fatalf("restarted status: %v", err)
	}
	edits = rec.callsOf("editMessageText")
	if len(edits) != 2 || edits[1].MessageID != 101 {
		t.Fatalf("restarted edits = %+v, want edit of 101", edits)
	}
	if got := len(rec.callsOf("sendMessage")); got != 1 {
		t.Fatalf("restart created a new status message (sends=%d)", got)
	}
}

func TestStatusBoardHeldWhileDraftActive(t *testing.T) {
	rec := &botRecorder{nextID: 100}
	g := newTestGateway(t, rec, nil, nil)
	g.setChatType(1001, "private")
	chat := g.chat(1001)
	ctx := context.Background()

	if err := chat.Update(ctx, "partial answer"); err != nil {
		t.Fatal(err)
	}
	if err := chat.Status(ctx, agent.RunStatus{Running: true, Phase: "thinking", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if got := len(rec.callsOf("sendMessage")); got != 0 {
		t.Fatalf("status was not held during the draft (sendMessage=%d)", got)
	}
	if got := len(rec.callsOf("pinChatMessage")); got != 0 {
		t.Fatalf("pinned while a draft was active (pins=%d)", got)
	}

	if err := chat.Send(ctx, "final answer"); err != nil {
		t.Fatal(err)
	}
	msgs := rec.callsOf("sendMessage")
	if len(msgs) != 2 {
		t.Fatalf("after Send messages = %d, want final answer and released status", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "final answer") {
		t.Fatalf("first message = %q, want final answer", msgs[0].Text)
	}
	found := false
	for _, m := range msgs[1:] {
		if strings.Contains(m.Text, "🟢 <b>Active</b>") {
			found = true
		}
	}
	if !found {
		t.Fatalf("released status missing from %+v", msgs)
	}

	if err := chat.Status(ctx, agent.RunStatus{Running: false, Phase: "done"}); err != nil {
		t.Fatal(err)
	}
}

func TestDraftStreamingInPrivateChat(t *testing.T) {
	rec := &botRecorder{nextID: 100}
	g := newTestGateway(t, rec, nil, nil)
	g.setChatType(1001, "private")
	chat := g.chat(1001)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := chat.Update(ctx, "streaming *draft*"); err != nil {
		t.Fatal(err)
	}
	draft := rec.waitFor(t, "sendMessageDraft", 1, 3*time.Second)
	if draft.ChatID != 1001 || draft.DraftID == 0 {
		t.Fatalf("draft call = %+v", draft)
	}
	if !draft.CanStop {
		t.Fatal("draft does not expose the stop button")
	}
	if draft.ParseMode != "HTML" {
		t.Fatalf("draft parse mode = %q", draft.ParseMode)
	}
	if !strings.Contains(draft.Text, "streaming") {
		t.Fatalf("draft text = %q", draft.Text)
	}
	if got := len(rec.callsOf("sendMessage")); got != 0 {
		t.Fatalf("draft mode sent %d regular messages", got)
	}

	if err := chat.Send(ctx, "streaming draft"); err != nil {
		t.Fatal(err)
	}
	msgs := rec.callsOf("sendMessage")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "streaming draft") {
		t.Fatalf("final messages = %+v", msgs)
	}
	if got := len(rec.callsOf("editMessageText")); got != 0 {
		t.Fatalf("draft mode used %d message edits", got)
	}
}

func TestDraftStreamingFallsBackToEdits(t *testing.T) {
	rec := &botRecorder{nextID: 100, draftError: true}
	g := newTestGateway(t, rec, nil, nil)
	g.setChatType(1001, "private")
	chat := g.chat(1001)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if err := chat.Update(ctx, "first chunk"); err != nil {
		t.Fatal(err)
	}
	msg := rec.waitFor(t, "sendMessage", 1, 3*time.Second)
	if !strings.Contains(msg.Text, "first chunk") {
		t.Fatalf("fallback message = %q", msg.Text)
	}
	if got := len(rec.callsOf("sendMessageDraft")); got == 0 {
		t.Fatal("draft was not attempted before the fallback")
	}

	if err := chat.Update(ctx, "first chunk second"); err != nil {
		t.Fatal(err)
	}
	edit := rec.waitFor(t, "editMessageText", 1, 4*time.Second)
	if edit.MessageID != 101 {
		t.Fatalf("fallback edit message_id = %d, want 101", edit.MessageID)
	}
	if !strings.Contains(edit.Text, "first chunk second") {
		t.Fatalf("fallback edit text = %q", edit.Text)
	}

	if err := chat.Send(ctx, "first chunk second final"); err != nil {
		t.Fatal(err)
	}
	edits := rec.callsOf("editMessageText")
	if len(edits) < 2 || !strings.Contains(edits[len(edits)-1].Text, "final") {
		t.Fatalf("final edit = %+v", edits)
	}
}

func TestDraftStreamingDisabledInGroups(t *testing.T) {
	rec := &botRecorder{nextID: 100}
	g := newTestGateway(t, rec, nil, nil)
	g.setChatType(-100200, "supergroup")
	chat := g.chat(-100200)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := chat.Update(ctx, "group text"); err != nil {
		t.Fatal(err)
	}
	msg := rec.waitFor(t, "sendMessage", 1, 3*time.Second)
	if !strings.Contains(msg.Text, "group text") {
		t.Fatalf("group stream message = %q", msg.Text)
	}
	if got := len(rec.callsOf("sendMessageDraft")); got != 0 {
		t.Fatalf("group chat used %d drafts", got)
	}
	if err := chat.Send(ctx, "group text"); err != nil {
		t.Fatal(err)
	}
}
