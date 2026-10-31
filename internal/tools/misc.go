package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bitvibe/kestrel/internal/sched"
	"github.com/bitvibe/kestrel/internal/store"
)

// SessionSearchTool searches past conversations through SQLite FTS5.
type SessionSearchTool struct{ Store *store.Store }

func (t *SessionSearchTool) Name() string { return "session_search" }
func (t *SessionSearchTool) Description() string {
	return "Full-text search past conversations, including earlier sessions. Returns message snippets with session IDs and timestamps. Use scope=chat to limit to the current chat."
}
func (t *SessionSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"query":{"type":"string","description":"Keywords; FTS5 syntax is applied to quoted words."},
			"scope":{"type":"string","enum":["chat","all"],"description":"Default all."},
			"limit":{"type":"integer","description":"Default 10, max 50."}
		},
		"required":["query"]
	}`)
}

func (t *SessionSearchTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Query string `json:"query"`
		Scope string `json:"scope"`
		Limit int    `json:"limit"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return ErrorResult("query is required"), nil
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	results, err := t.Store.SearchMessages(ctx, a.Query, limit*3)
	if err != nil {
		return ErrorResult("search: %v", err), nil
	}
	info, _ := SessionFromContext(ctx)
	if strings.EqualFold(a.Scope, "chat") && info.ChatID != 0 {
		filtered := results[:0]
		for _, r := range results {
			if r.ChatID == info.ChatID {
				filtered = append(filtered, r)
			}
		}
		results = filtered
	}
	if len(results) > limit {
		results = results[:limit]
	}
	if len(results) == 0 {
		return TextResult("no matching messages"), nil
	}
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "[%s] session %s (%s, chat %d)\n%s\n\n",
			r.CreatedAt.Format("2006-01-02 15:04"), r.SessionID, r.Role, r.ChatID, strings.TrimSpace(r.Snippet))
	}
	return Result{Content: strings.TrimSpace(b.String())}, nil
}

// ScheduleTool manages cron jobs that run an agent prompt and deliver the
// answer to this chat.
type ScheduleTool struct{ Store *store.Store }

func (t *ScheduleTool) Name() string { return "schedule" }
func (t *ScheduleTool) Description() string {
	return "Create, list and delete cron jobs. A job runs an agent prompt on a 5-field cron schedule (minute hour day-of-month month day-of-week) and sends the result to this chat."
}
func (t *ScheduleTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["create","list","delete"]},
			"schedule":{"type":"string","description":"Cron expression, e.g. '0 9 * * 1-5'."},
			"prompt":{"type":"string","description":"Agent prompt to run."},
			"id":{"type":"string","description":"Job ID for delete."}
		},
		"required":["action"]
	}`)
}

func (t *ScheduleTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Action   string `json:"action"`
		Schedule string `json:"schedule"`
		Prompt   string `json:"prompt"`
		ID       string `json:"id"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	info, ok := SessionFromContext(ctx)
	if !ok || info.ChatID == 0 {
		return ErrorResult("scheduling requires a chat context"), nil
	}
	switch a.Action {
	case "create":
		if strings.TrimSpace(a.Prompt) == "" {
			return ErrorResult("prompt is required"), nil
		}
		schedule, err := sched.Parse(a.Schedule)
		if err != nil {
			return ErrorResult("invalid schedule: %v", err), nil
		}
		jobs, err := t.Store.ListCronJobs(ctx, info.ChatID)
		if err != nil {
			return ErrorResult("list jobs: %v", err), nil
		}
		if len(jobs) >= 25 {
			return ErrorResult("this chat already has 25 scheduled jobs; delete one first"), nil
		}
		next := schedule.Next(time.Now())
		if next.IsZero() {
			return ErrorResult("schedule never fires"), nil
		}
		job := store.CronJob{
			ID:       store.NewID()[:10],
			ChatID:   info.ChatID,
			Prompt:   strings.TrimSpace(a.Prompt),
			Schedule: schedule.String(),
			Enabled:  true,
			NextRun:  next,
		}
		if err := t.Store.UpsertCronJob(ctx, job); err != nil {
			return ErrorResult("create job: %v", err), nil
		}
		return TextResult("created job %s (%s), next run %s", job.ID, job.Schedule, next.Format(time.RFC3339)), nil
	case "list":
		jobs, err := t.Store.ListCronJobs(ctx, info.ChatID)
		if err != nil {
			return ErrorResult("list jobs: %v", err), nil
		}
		if len(jobs) == 0 {
			return TextResult("no scheduled jobs"), nil
		}
		var b strings.Builder
		for _, j := range jobs {
			fmt.Fprintf(&b, "%s  %-16s next=%s  %s\n", j.ID, j.Schedule, j.NextRun.Format("2006-01-02 15:04"), j.Prompt)
		}
		return Result{Content: strings.TrimSpace(b.String())}, nil
	case "delete":
		if a.ID == "" {
			return ErrorResult("id is required"), nil
		}
		found, err := t.Store.DeleteCronJob(ctx, info.ChatID, a.ID)
		if err != nil {
			return ErrorResult("delete job: %v", err), nil
		}
		if !found {
			return ErrorResult("job %s not found", a.ID), nil
		}
		return TextResult("deleted job %s", a.ID), nil
	default:
		return ErrorResult("action must be create, list or delete"), nil
	}
}

// TodoItem is one plan step.
type TodoItem struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

// TodoTool maintains a session-scoped plan.
type TodoTool struct {
	mu    sync.Mutex
	items map[string][]TodoItem
	next  map[string]int
}

func (t *TodoTool) Name() string { return "todo" }
func (t *TodoTool) Description() string {
	return "Maintain a structured plan for multi-step tasks. Actions: list, add (one or more items), update status, clear. Keep one item in_progress at a time."
}
func (t *TodoTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["list","add","update","clear"]},
			"items":{"type":"array","items":{"type":"string"},"description":"Items to add."},
			"id":{"type":"integer","description":"Item ID for update."},
			"status":{"type":"string","enum":["pending","in_progress","completed"]},
			"text":{"type":"string","description":"New text for update."}
		},
		"required":["action"]
	}`)
}

func (t *TodoTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Action string   `json:"action"`
		Items  []string `json:"items"`
		ID     int      `json:"id"`
		Status string   `json:"status"`
		Text   string   `json:"text"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	info, _ := SessionFromContext(ctx)
	key := info.SessionID
	if key == "" {
		key = "default"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.items == nil {
		t.items = make(map[string][]TodoItem)
		t.next = make(map[string]int)
	}
	switch a.Action {
	case "add":
		if len(a.Items) == 0 {
			return ErrorResult("items is required"), nil
		}
		for _, text := range a.Items {
			if strings.TrimSpace(text) == "" {
				continue
			}
			t.next[key]++
			t.items[key] = append(t.items[key], TodoItem{ID: t.next[key], Text: strings.TrimSpace(text), Status: "pending"})
		}
	case "update":
		found := false
		for i := range t.items[key] {
			if t.items[key][i].ID == a.ID {
				if a.Text != "" {
					t.items[key][i].Text = a.Text
				}
				if a.Status != "" {
					if a.Status != "pending" && a.Status != "in_progress" && a.Status != "completed" {
						return ErrorResult("status must be pending, in_progress or completed"), nil
					}
					t.items[key][i].Status = a.Status
				}
				found = true
				break
			}
		}
		if !found {
			return ErrorResult("todo item %d not found", a.ID), nil
		}
	case "clear":
		t.items[key] = nil
	case "list":
	default:
		return ErrorResult("action must be list, add, update or clear"), nil
	}
	items := t.items[key]
	if len(items) == 0 {
		return TextResult("(todo list is empty)"), nil
	}
	var b strings.Builder
	done := 0
	for _, it := range items {
		mark := "[ ]"
		switch it.Status {
		case "in_progress":
			mark = "[~]"
		case "completed":
			mark = "[x]"
			done++
		}
		fmt.Fprintf(&b, "%s %d. %s\n", mark, it.ID, it.Text)
	}
	fmt.Fprintf(&b, "\n%d/%d complete", done, len(items))
	return Result{Content: b.String()}, nil
}

// Snapshot returns a copy of one session's plan for status displays.
func (t *TodoTool) Snapshot(sessionID string) []TodoItem {
	key := sessionID
	if key == "" {
		key = "default"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	items := t.items[key]
	out := make([]TodoItem, len(items))
	copy(out, items)
	return out
}

// SendFileTool sends a workspace file back through the active chat.
type SendFileTool struct {
	Roots    []string
	MaxBytes int64
}

func (t *SendFileTool) Name() string { return "send_file" }
func (t *SendFileTool) Description() string {
	return "Send a file from the workspace to the user's chat (Telegram document or CLI path). Use for reports, images, archives and generated artifacts."
}
func (t *SendFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string"},
			"caption":{"type":"string","description":"Optional short caption."}
		},
		"required":["path"]
	}`)
}

func (t *SendFileTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Path    string `json:"path"`
		Caption string `json:"caption"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	sender, ok := SenderFromContext(ctx)
	if !ok {
		return ErrorResult("this frontend does not support sending files"), nil
	}
	p, err := SecurePath(t.Roots, a.Path)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	st, err := os.Stat(p)
	if err != nil {
		return ErrorResult("stat %s: %v", p, err), nil
	}
	if st.IsDir() {
		return ErrorResult("path is a directory; archive it first"), nil
	}
	if t.MaxBytes > 0 && st.Size() > t.MaxBytes {
		return ErrorResult("file is too large to send (%d bytes, max %d)", st.Size(), t.MaxBytes), nil
	}
	msg, err := sender(ctx, p)
	if err != nil {
		return ErrorResult("send file: %v", err), nil
	}
	if a.Caption != "" {
		msg += " (" + a.Caption + ")"
	}
	return TextResult("sent %s: %s", p, msg), nil
}

// SortTodoItems is a helper for tests.
func SortTodoItems(items []TodoItem) {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
}
