package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bitvibe/kestrel/internal/tools"
)

// Approvals implements tools.Approver with inline Telegram buttons.
type Approvals struct {
	g  *Gateway
	mu sync.Mutex
	// pending maps short tokens to outstanding requests.
	pending map[string]*pendingApproval
	log     *slog.Logger
}

type pendingApproval struct {
	chatID   int64
	token    string
	command  string
	reason   string
	decision chan bool
	answered bool
}

// NewApprovals creates an empty approval manager.
func NewApprovals(g *Gateway, log *slog.Logger) *Approvals {
	if log == nil {
		log = slog.Default()
	}
	return &Approvals{g: g, pending: make(map[string]*pendingApproval), log: log}
}

var _ tools.Approver = (*Approvals)(nil)

// Approve asks the chat to approve a dangerous command and waits for a button
// press or timeout.
func (a *Approvals) Approve(ctx context.Context, req tools.ApprovalRequest) (bool, error) {
	if req.ChatID == 0 {
		return false, fmt.Errorf("approval request has no chat")
	}
	token, err := approvalToken()
	if err != nil {
		return false, err
	}
	p := &pendingApproval{
		chatID:   req.ChatID,
		token:    token,
		command:  req.Command,
		reason:   req.Reason,
		decision: make(chan bool, 1),
	}
	a.mu.Lock()
	if len(a.pending) > 100 {
		a.mu.Unlock()
		return false, fmt.Errorf("too many pending approvals")
	}
	a.pending[token] = p
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, token)
		a.mu.Unlock()
	}()

	text := approvalText(req.Reason, req.Command)
	keyboard := map[string]any{
		"inline_keyboard": [][]map[string]string{{
			{"text": "✅ Approve", "callback_data": "apv:" + token + ":yes"},
			{"text": "🚫 Deny", "callback_data": "apv:" + token + ":no"},
		}},
	}
	msg, err := a.g.sendText(ctx, req.ChatID, text, "HTML", keyboard)
	if err != nil {
		return false, fmt.Errorf("send approval request: %w", err)
	}

	select {
	case ok := <-p.decision:
		a.finalize(p, msg.MessageID, ok)
		return ok, nil
	case <-ctx.Done():
		a.finalize(p, msg.MessageID, false)
		return false, ctx.Err()
	}
}

func (a *Approvals) finalize(p *pendingApproval, messageID int, approved bool) {
	verdict := "🚫 <b>Denied</b>"
	if approved {
		verdict = "✅ <b>Approved</b>"
	}
	text := approvalText(p.reason, p.command) + "\n\n" + verdict
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.g.editText(ctx, p.chatID, messageID, text, "HTML", emptyKeyboard()); err != nil {
		a.log.Debug("finalize approval message", "error", err)
	}
}

// HandleCallback resolves a button press. It returns true when the update was
// an approval callback, including expired or unauthorized ones.
func (a *Approvals) HandleCallback(ctx context.Context, q *CallbackQuery) bool {
	if q == nil || !strings.HasPrefix(q.Data, "apv:") {
		return false
	}
	answer := func(text string) {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := a.g.client.AnswerCallbackQuery(actx, q.ID, text); err != nil {
			a.log.Debug("answer callback", "error", err)
		}
	}
	parts := strings.Split(q.Data, ":")
	if len(parts) != 3 || (parts[2] != "yes" && parts[2] != "no") {
		answer("Malformed approval.")
		return true
	}
	token := parts[1]
	approve := parts[2] == "yes"
	a.mu.Lock()
	p, ok := a.pending[token]
	if ok && p.answered {
		a.mu.Unlock()
		answer("This request was already answered.")
		return true
	}
	if !ok {
		a.mu.Unlock()
		answer("This approval request expired.")
		return true
	}
	if q.From == nil || !a.g.allow.Allowed(q.From.ID) {
		a.mu.Unlock()
		answer("You are not authorized to approve this request.")
		return true
	}
	if q.Message == nil || q.Message.Chat.ID != p.chatID {
		a.mu.Unlock()
		answer("This approval belongs to a different chat.")
		return true
	}
	p.answered = true
	a.mu.Unlock()
	select {
	case p.decision <- approve:
	default:
	}
	if approve {
		answer("Approved.")
	} else {
		answer("Denied.")
	}
	return true
}

func approvalText(reason, command string) string {
	command = truncateRunes(command, 450)
	var b strings.Builder
	b.WriteString("🔐 <b>Approval required</b>\n\n")
	if strings.TrimSpace(reason) != "" {
		b.WriteString("Reason: " + html.EscapeString(truncateRunes(reason, 100)) + "\n\n")
	}
	b.WriteString("Command:\n<pre>" + html.EscapeString(command) + "</pre>\n\n")
	b.WriteString("The request expires automatically. Only allowlisted users can decide.")
	return b.String()
}

func emptyKeyboard() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{}}
}

func approvalToken() (string, error) {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate approval token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
