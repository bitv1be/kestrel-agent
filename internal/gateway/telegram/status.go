package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitvibe/kestrel/internal/agent"
)

const (
	statusKVPrefix    = "telegram:status:"
	statusMinInterval = time.Second
	statusMaxTasks    = 12
	statusTaskRunes   = 140
	statusActionRunes = 300
	statusDetailRunes = 500
)

// statusBoard owns one pinned message per chat. The first and final updates are
// written synchronously so the board appears immediately and always settles on
// the real outcome; intermediate updates are coalesced to at most one edit per
// second.
type statusBoard struct {
	g      *Gateway
	chatID int64

	sendMu sync.Mutex

	mu        sync.Mutex
	messageID int
	loaded    bool
	started   time.Time
	lastSent  time.Time
	lastText  string
	latest    agent.RunStatus
	hasLatest bool
	scheduled bool
	final     bool
	epoch     int
	held      *agent.RunStatus
}

func newStatusBoard(g *Gateway, chatID int64) *statusBoard {
	return &statusBoard{g: g, chatID: chatID}
}

// Status publishes a run snapshot. It is called by the engine on every phase
// change and may be called concurrently by parallel tool goroutines.
func (c *Chat) Status(ctx context.Context, st agent.RunStatus) error {
	// A terminal status means no final answer is coming (error or stop), so a
	// partial preview would otherwise linger forever.
	if !st.Running {
		c.finishPreview(ctx)
	}
	if c.g == nil || !c.g.pinnedStatus() {
		return nil
	}
	if c.draftActive() {
		c.status.hold(st)
		return nil
	}
	return c.status.publish(ctx, st)
}

// hold remembers an update without sending it. Telegram dismisses a draft
// whenever the bot posts a regular message, so the board waits until the draft
// is finalized.
func (b *statusBoard) hold(st agent.RunStatus) {
	b.mu.Lock()
	held := st
	b.held = &held
	b.mu.Unlock()
}

// release flushes the last held status after Send or Abort.
func (b *statusBoard) release(ctx context.Context) {
	b.mu.Lock()
	held := b.held
	b.held = nil
	b.mu.Unlock()
	if held == nil || !held.Running {
		return
	}
	if err := b.publish(ctx, *held); err != nil {
		b.g.log.Debug("release status board", "chat_id", b.chatID, "error", err)
	}
}

func (b *statusBoard) publish(ctx context.Context, st agent.RunStatus) error {
	b.mu.Lock()
	if st.Phase == "starting" || b.started.IsZero() {
		b.started = time.Now()
	}
	b.latest = st
	b.hasLatest = true

	synchronous := st.Phase == "starting" || !st.Running || b.lastText == ""
	if synchronous {
		if !b.loaded {
			b.loaded = true
			b.messageID = b.loadID(ctx)
		}
		// Invalidate any in-flight coalesced update from an earlier phase.
		b.epoch++
		epoch := b.epoch
		b.final = !st.Running
		b.scheduled = false
		b.held = nil
		text := b.renderLocked(st)
		b.lastText = text
		b.lastSent = time.Now()
		b.mu.Unlock()

		err := b.sendBounded(ctx, text, true)
		if err != nil {
			b.mu.Lock()
			if b.epoch == epoch && b.lastText == text {
				// Let the next phase retry instead of treating a failed render
				// as current.
				b.lastText = ""
			}
			b.mu.Unlock()
			b.g.log.Debug("status board update", "chat_id", b.chatID, "error", err)
		}
		return err
	}

	if !b.loaded {
		b.loaded = true
		b.messageID = b.loadID(ctx)
	}
	if b.final {
		// A new run began without a "starting" phase (for example a scheduled
		// run resumed from history); allow coalesced updates again.
		b.final = false
		b.epoch++
	}
	if b.scheduled {
		b.mu.Unlock()
		return nil
	}
	b.scheduled = true
	epoch := b.epoch
	b.mu.Unlock()

	go b.flushLater(epoch)
	return nil
}

// flushLater writes the latest snapshot once the throttle window has passed.
// It exits when a synchronous update (start/final) starts a new epoch.
func (b *statusBoard) flushLater(epoch int) {
	for {
		b.mu.Lock()
		if b.epoch != epoch || b.final {
			// A synchronous update owns the board now and manages the
			// scheduled flag itself.
			b.mu.Unlock()
			return
		}
		wait := statusMinInterval - time.Since(b.lastSent)
		if wait > 0 {
			b.mu.Unlock()
			timer := time.NewTimer(wait)
			<-timer.C
			continue
		}
		if !b.hasLatest {
			b.scheduled = false
			b.mu.Unlock()
			return
		}
		text := b.renderLocked(b.latest)
		if text == b.lastText {
			b.scheduled = false
			b.mu.Unlock()
			return
		}
		b.lastText = text
		b.lastSent = time.Now()
		b.mu.Unlock()

		b.sendMu.Lock()
		b.mu.Lock()
		if b.epoch != epoch || b.final {
			b.mu.Unlock()
			b.sendMu.Unlock()
			return
		}
		b.mu.Unlock()
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := b.sendNow(sctx, text, false)
		cancel()
		b.sendMu.Unlock()

		if err != nil {
			b.g.log.Debug("status board update", "chat_id", b.chatID, "error", err)
			b.mu.Lock()
			if b.epoch == epoch {
				if b.lastText == text {
					// Force the next update to retry the edit instead of
					// treating the failed render as current.
					b.lastText = ""
				}
				b.scheduled = false
			}
			b.mu.Unlock()
			return
		}

		b.mu.Lock()
		if b.epoch != epoch || b.final {
			b.mu.Unlock()
			return
		}
		if !b.hasLatest || b.renderLocked(b.latest) == b.lastText {
			b.scheduled = false
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
		// A newer snapshot arrived while the edit was in flight.
	}
}

// sendBounded gives a status update a deadline even when the run context has
// been detached for a final update.
func (b *statusBoard) sendBounded(ctx context.Context, text string, pin bool) error {
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return b.send(sctx, text, pin)
}

func (b *statusBoard) renderLocked(st agent.RunStatus) string {
	var out strings.Builder
	header := "🟢 <b>Active</b>"
	switch {
	case st.Phase == "done":
		header = "✅ <b>Done</b>"
	case st.Phase == "error":
		header = "❌ <b>Error</b>"
	case st.Phase == "stopped":
		header = "⏹ <b>Stopped</b>"
	}
	fmt.Fprintf(&out, "%s · %s\n", header, formatElapsed(time.Since(b.started)))
	if st.Model != "" {
		fmt.Fprintf(&out, "Model: <code>%s</code>\n", escapeHTML(st.Model))
	}
	if st.Iteration > 0 && st.Running {
		fmt.Fprintf(&out, "Step %d\n", st.Iteration)
	}
	if st.Running {
		switch st.Phase {
		case "compacting":
			out.WriteString("🧠 Compacting context…\n")
		case "starting":
			out.WriteString("🚀 Starting…\n")
		case "tool":
			if action := strings.TrimSpace(st.Action); action != "" {
				fmt.Fprintf(&out, "⚙️ %s\n", escapeHTML(clipRunes(action, statusActionRunes)))
			}
		default:
			out.WriteString("💭 Thinking…\n")
		}
	} else if st.Phase == "error" {
		if detail := strings.TrimSpace(st.Action); detail != "" {
			fmt.Fprintf(&out, "⚠️ %s\n", escapeHTML(clipRunes(detail, statusDetailRunes)))
		}
	}
	if len(st.Tasks) > 0 {
		out.WriteString("\n<b>Plan</b>\n")
		for i, task := range st.Tasks {
			if i >= statusMaxTasks {
				fmt.Fprintf(&out, "… and %d more\n", len(st.Tasks)-statusMaxTasks)
				break
			}
			text := escapeHTML(clipRunes(strings.TrimSpace(task.Text), statusTaskRunes))
			switch task.Status {
			case "completed":
				fmt.Fprintf(&out, "✅ <s>%s</s>\n", text)
			case "in_progress":
				fmt.Fprintf(&out, "🔄 %s\n", text)
			default:
				fmt.Fprintf(&out, "⬜ %s\n", text)
			}
		}
	}
	return strings.TrimSpace(out.String())
}

func (b *statusBoard) send(ctx context.Context, text string, pin bool) error {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.sendNow(ctx, text, pin)
}

func (b *statusBoard) sendNow(ctx context.Context, text string, pin bool) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	b.mu.Lock()
	messageID := b.messageID
	b.mu.Unlock()

	if messageID != 0 {
		err := b.g.editText(ctx, b.chatID, messageID, text, "HTML", nil)
		if err == nil || IsNotModified(err) {
			if pin {
				b.pin(ctx, messageID)
			}
			return nil
		}
		if IsParseError(err) {
			plain := stripTags(text)
			if plainErr := b.g.editText(ctx, b.chatID, messageID, plain, "", nil); plainErr == nil || IsNotModified(plainErr) {
				if pin {
					b.pin(ctx, messageID)
				}
				return nil
			}
		}
		if !IsMessageNotFound(err) {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || (apiErr.Code != 403 && apiErr.Code != 404) {
				return err
			}
		}
		b.mu.Lock()
		if b.messageID == messageID {
			b.messageID = 0
		}
		b.mu.Unlock()
	}

	m, err := b.g.sendText(ctx, b.chatID, text, "HTML", nil)
	if IsParseError(err) {
		m, err = b.g.sendText(ctx, b.chatID, stripTags(text), "", nil)
	}
	if err != nil {
		return err
	}
	if m.MessageID == 0 {
		return nil
	}
	b.mu.Lock()
	b.messageID = m.MessageID
	b.mu.Unlock()
	b.saveID(ctx, m.MessageID)
	if pin {
		b.pin(ctx, m.MessageID)
	}
	return nil
}

func (b *statusBoard) pin(ctx context.Context, messageID int) {
	if err := b.g.client.PinChatMessage(ctx, b.chatID, messageID); err != nil {
		// Pinning requires administrator rights in groups. The board is still
		// useful there; only keep it pinned when Telegram allows it.
		b.g.log.Debug("pin status board", "chat_id", b.chatID, "error", err)
	}
}

func (b *statusBoard) loadID(ctx context.Context) int {
	if b.g.store == nil {
		return 0
	}
	value, ok, err := b.g.store.GetKV(ctx, statusKVKey(b.chatID))
	if err != nil {
		b.g.log.Debug("load status message", "chat_id", b.chatID, "error", err)
		return 0
	}
	if !ok {
		return 0
	}
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func (b *statusBoard) saveID(ctx context.Context, id int) {
	if b.g.store == nil {
		return
	}
	if err := b.g.store.SetKV(ctx, statusKVKey(b.chatID), strconv.Itoa(id)); err != nil {
		b.g.log.Debug("save status message", "chat_id", b.chatID, "error", err)
	}
}

func statusKVKey(chatID int64) string {
	return statusKVPrefix + strconv.FormatInt(chatID, 10)
}

// formatElapsed renders a compact, stable duration for the status board.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int(d / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
