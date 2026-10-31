package telegram

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bitvibe/kestrel/internal/agent"
)

// rateLimiter spaces calls for one Telegram target. Slots are reserved while
// holding the lock, then waiters sleep outside it, which keeps the queue fair
// and bounded.
type rateLimiter struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{interval: interval}
}

func (l *rateLimiter) wait(ctx context.Context) error {
	if l == nil || l.interval <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	now := time.Now()
	target := now
	if l.next.After(now) {
		target = l.next
	}
	l.next = target.Add(l.interval)
	l.mu.Unlock()
	if d := time.Until(target); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return ctx.Err()
}

// Chat is Kestrel's agent.Chat implementation for one Telegram conversation.
// It streams by editing a single message and coalesces updates so bursts of
// tokens never exceed Telegram's edit limits.
type Chat struct {
	g      *Gateway
	chatID int64

	mu     sync.Mutex
	stream *stream

	status *statusBoard

	typingMu   sync.Mutex
	lastTyping time.Time

	progressMu   sync.Mutex
	progressID   int
	lastProgress string
	progressAt   time.Time
}

var _ agent.Chat = (*Chat)(nil)

func newChat(g *Gateway, chatID int64) *Chat {
	c := &Chat{g: g, chatID: chatID}
	c.status = newStatusBoard(g, chatID)
	return c
}

// Send delivers the final answer, reusing the streamed message when it fits.
func (c *Chat) Send(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "(the model returned an empty response)"
	}
	c.mu.Lock()
	st := c.stream
	c.stream = nil
	c.mu.Unlock()

	var msgID int
	if st != nil {
		msgID = st.finish()
	}
	if err := c.clearProgress(ctx); err != nil {
		c.g.log.Debug("clear progress", "chat_id", c.chatID, "error", err)
	}
	err := c.sendFinal(ctx, msgID, text)
	c.releaseHeld(ctx)
	return err
}

// Abort removes a cancelled run's preview before a new run uses this chat.
func (c *Chat) Abort(ctx context.Context) error {
	c.mu.Lock()
	st := c.stream
	c.stream = nil
	c.mu.Unlock()
	if st != nil {
		if id := st.finish(); id != 0 {
			if err := c.g.deleteMessage(ctx, c.chatID, id); err != nil && !IsMessageNotFound(err) {
				return err
			}
		}
	}
	err := c.clearProgress(ctx)
	c.releaseHeld(ctx)
	return err
}

func (c *Chat) sendFinal(ctx context.Context, msgID int, text string) error {
	max := c.g.maxMessage()
	formatted := formatHTML(text)
	fits := utf8.RuneCountInString(formatted) <= max
	if msgID != 0 && fits {
		err := c.g.editText(ctx, c.chatID, msgID, formatted, "HTML", nil)
		if err == nil || IsNotModified(err) {
			return nil
		}
		if IsParseError(err) {
			if plainErr := c.g.editText(ctx, c.chatID, msgID, stripTags(formatted), "", nil); plainErr == nil {
				return nil
			} else {
				err = plainErr
			}
		}
		if !IsMessageNotFound(err) {
			c.g.log.Warn("final edit failed; sending a new message", "chat_id", c.chatID, "error", err)
		}
	}
	if msgID != 0 && !fits {
		// The streamed message only holds a tail preview; remove it before
		// posting the complete answer in chunks.
		if err := c.g.deleteMessage(ctx, c.chatID, msgID); err != nil && !IsMessageNotFound(err) {
			c.g.log.Debug("delete stream preview", "chat_id", c.chatID, "error", err)
		}
	}
	return c.g.sendMarkdown(ctx, c.chatID, text)
}

// Update renders the accumulated partial answer with coalesced edits. In a
// private chat it animates a Bot API draft when draft streaming is enabled;
// groups and rejected drafts fall back to editing one message.
func (c *Chat) Update(ctx context.Context, fullText string) error {
	if !c.g.streamEnabled() || strings.TrimSpace(fullText) == "" {
		return nil
	}
	draft := c.g.draftStreaming() && c.g.isPrivateChat(c.chatID)
	c.mu.Lock()
	if c.stream == nil {
		c.stream = &stream{g: c.g, chatID: c.chatID, ctx: ctx, wake: make(chan struct{}), draft: draft}
		if draft {
			c.stream.draftID = nextDraftID()
		}
	}
	st := c.stream
	c.mu.Unlock()
	st.push(fullText)
	return nil
}

// Progress maintains a small status message which is replaced, not appended.
func (c *Chat) Progress(ctx context.Context, text string) error {
	if !c.g.progressEnabled() {
		return nil
	}
	text = strings.TrimSpace(text)
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	if text == "" {
		id := c.progressID
		c.progressID = 0
		c.lastProgress = ""
		if id == 0 {
			return nil
		}
		err := c.g.deleteMessage(ctx, c.chatID, id)
		if IsMessageNotFound(err) {
			return nil
		}
		return err
	}
	if text == c.lastProgress && time.Since(c.progressAt) < 2*time.Second {
		return nil
	}
	htmlText := "<i>" + escapeHTML(text) + "</i>"
	if c.progressID == 0 {
		m, err := c.g.sendText(ctx, c.chatID, htmlText, "HTML", nil)
		if err != nil {
			if IsParseError(err) {
				m, err = c.g.sendText(ctx, c.chatID, text, "", nil)
			}
			if err != nil {
				return err
			}
		}
		if m.MessageID != 0 {
			c.progressID = m.MessageID
			c.lastProgress = text
			c.progressAt = time.Now()
		}
		return nil
	}
	err := c.g.editText(ctx, c.chatID, c.progressID, htmlText, "HTML", nil)
	if IsParseError(err) {
		err = c.g.editText(ctx, c.chatID, c.progressID, text, "", nil)
	}
	if IsMessageNotFound(err) {
		c.progressID = 0
		return nil
	}
	if err == nil || IsNotModified(err) {
		c.lastProgress = text
		c.progressAt = time.Now()
	}
	return err
}

// Action posts a one-line description of a tool call as its own message, so a
// run leaves a complete, durable log in the chat. Unlike Progress, an action
// message is never replaced by later output.
func (c *Chat) Action(ctx context.Context, text string) error {
	if !c.g.logActions() {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	_, err := c.g.sendText(ctx, c.chatID, "⚙️ "+text, "", nil)
	return err
}

// Think posts the model's reasoning for one turn as a separate message so the
// user can follow how the model reached its answer. Long reasoning is capped
// before Telegram's chunking so a runaway trace cannot flood the chat.
func (c *Chat) Think(ctx context.Context, text string) error {
	if !c.g.reasoningEnabled() {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if r := []rune(text); len(r) > maxReasoningRunes {
		text = string(r[:maxReasoningRunes]) + "\n\n… (reasoning truncated)"
	}
	return c.g.sendMarkdown(ctx, c.chatID, "💭 *Thinking*\n\n"+text)
}

const maxReasoningRunes = 8000

// Error reports a failure as its own message. Errors are always shown, even
// when action logging or reasoning output is disabled.
func (c *Chat) Error(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return c.g.sendMarkdown(ctx, c.chatID, "❌ "+clipRunes(text, 3000))
}

// clipRunes truncates s to at most n runes.
func clipRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// Typing shows the typing indicator, throttled so tool-heavy runs do not call
// sendChatAction for every iteration.
func (c *Chat) Typing(ctx context.Context) error {
	c.typingMu.Lock()
	if time.Since(c.lastTyping) < 4*time.Second {
		c.typingMu.Unlock()
		return nil
	}
	c.lastTyping = time.Now()
	c.typingMu.Unlock()
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.g.limiter(c.chatID).wait(actx); err != nil {
		return err
	}
	return c.g.client.SendChatAction(actx, c.chatID, "typing")
}

// SendFile uploads a workspace file, choosing photo for images.
func (c *Chat) SendFile(ctx context.Context, path, caption string) error {
	ext := strings.ToLower(filepath.Ext(path))
	if err := c.g.limiter(c.chatID).wait(ctx); err != nil {
		return err
	}
	var err error
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp":
		_, err = c.g.client.SendPhoto(ctx, c.chatID, path, caption)
		if err != nil {
			// Telegram refuses some formats/sizes as photos; a document always works.
			_, err = c.g.client.SendDocument(ctx, c.chatID, path, caption)
		}
	default:
		_, err = c.g.client.SendDocument(ctx, c.chatID, path, caption)
	}
	return err
}

func (c *Chat) clearProgress(ctx context.Context) error {
	return c.Progress(ctx, "")
}

// finishPreview removes an in-progress stream. It is used on terminal status
// updates (errors and stops) where no final answer will replace the preview.
func (c *Chat) finishPreview(ctx context.Context) {
	c.mu.Lock()
	st := c.stream
	c.stream = nil
	c.mu.Unlock()
	if st == nil {
		return
	}
	id := st.finish()
	if id != 0 {
		if err := c.g.deleteMessage(ctx, c.chatID, id); err != nil && !IsMessageNotFound(err) {
			c.g.log.Debug("delete stream preview", "chat_id", c.chatID, "error", err)
		}
	}
}

// draftActive reports whether an animated draft is currently owning this
// chat's preview. Regular messages would dismiss the draft, so status updates
// wait until Send or Abort releases them.
func (c *Chat) draftActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stream != nil && c.stream.draft
}

func (c *Chat) releaseHeld(ctx context.Context) {
	if c.status != nil {
		c.status.release(ctx)
	}
}

// stream coalesces partial updates into at most one edit per interval.
type stream struct {
	g      *Gateway
	chatID int64
	ctx    context.Context

	mu           sync.Mutex
	latest       string
	msgID        int
	draft        bool
	draftID      int64
	lastRendered string
	lastEdit     time.Time
	running      bool
	finalized    bool
	wake         chan struct{}
	done         chan struct{}
}

func (s *stream) push(text string) {
	s.mu.Lock()
	if s.finalized {
		s.mu.Unlock()
		return
	}
	s.latest = text
	if !s.running {
		s.running = true
		s.done = make(chan struct{})
		go s.flush(s.done)
	}
	s.mu.Unlock()
}

// finish stops the flusher and returns the streamed message ID, if any.
func (s *stream) finish() int {
	s.mu.Lock()
	if !s.finalized {
		s.finalized = true
		close(s.wake)
	}
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgID
}

func (s *stream) flush(done chan struct{}) {
	defer func() {
		s.mu.Lock()
		if s.done == done {
			s.running = false
			close(done)
			s.done = nil
		}
		s.mu.Unlock()
	}()
	interval := s.g.editInterval()
	for {
		s.mu.Lock()
		if s.finalized {
			s.mu.Unlock()
			return
		}
		text := s.latest
		if text == "" || text == s.lastRendered {
			s.mu.Unlock()
			return
		}
		wait := interval - time.Since(s.lastEdit)
		msgID := s.msgID
		draft := s.draft
		draftID := s.draftID
		s.mu.Unlock()
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-s.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				continue
			}
		}
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		var newID int
		var err error
		if draft {
			err = s.g.renderDraft(ctx, s.chatID, draftID, text)
		} else {
			newID, err = s.g.renderStream(ctx, s.chatID, msgID, text)
		}
		cancel()
		s.mu.Lock()
		s.lastEdit = time.Now()
		switch {
		case err == nil:
			s.lastRendered = text
			s.msgID = newID
		case draft:
			// Drafts are unavailable for this account or chat type (for
			// example an older Bot API). Fall back to editing a normal message
			// and render the current text immediately.
			s.draft = false
			s.draftID = 0
			s.lastRendered = ""
			s.lastEdit = time.Time{}
		case IsMessageNotFound(err):
			s.msgID = 0
			s.lastRendered = ""
		}
		s.mu.Unlock()
		if err != nil && !draft {
			if !IsMessageNotFound(err) {
				s.g.log.Debug("streaming edit failed", "chat_id", s.chatID, "error", err)
			}
		}
	}
}

// renderDraft animates a Bot API draft. Unlike a message edit it has no ID;
// the same draft ID makes Telegram animate consecutive updates.
func (g *Gateway) renderDraft(ctx context.Context, chatID, draftID int64, text string) error {
	rendered := streamHTML(text, g.maxMessage())
	err := g.sendDraft(ctx, chatID, draftID, rendered, "HTML")
	if IsParseError(err) {
		err = g.sendDraft(ctx, chatID, draftID, stripTags(rendered), "")
	}
	return err
}

// sendDraft is the single rate-limited path to sendMessageDraft.
func (g *Gateway) sendDraft(ctx context.Context, chatID, draftID int64, text, parseMode string) error {
	if utf8.RuneCountInString(text) > g.maxMessage() {
		text = truncateRunes(text, g.maxMessage()-1)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := g.global.wait(ctx); err != nil {
			return err
		}
		if err := g.limiter(chatID).wait(ctx); err != nil {
			return err
		}
		err := g.client.SendMessageDraft(ctx, chatID, draftID, text, parseMode, true)
		if err == nil {
			return nil
		}
		if d, ok := RetryAfter(err); ok {
			if waitErr := sleepContext(ctx, d); waitErr != nil {
				return waitErr
			}
			continue
		}
		return err
	}
	return fmt.Errorf("telegram sendMessageDraft: retry limit reached")
}

var draftSeq atomic.Int64

// nextDraftID returns a small positive identifier. Reusing it within one run
// animates updates; a fresh identifier starts a new animated draft.
func nextDraftID() int64 {
	seq := draftSeq.Add(1)
	id := (time.Now().UnixNano() + seq) & 0x7fffffff
	if id == 0 {
		id = seq | 1
	}
	return id
}

// renderStream creates or edits the streaming message, falling back to plain
// text if Telegram rejects the generated HTML.
func (g *Gateway) renderStream(ctx context.Context, chatID int64, msgID int, text string) (int, error) {
	rendered := streamHTML(text, g.maxMessage())
	if msgID == 0 {
		m, err := g.sendText(ctx, chatID, rendered, "HTML", nil)
		if IsParseError(err) {
			m, err = g.sendText(ctx, chatID, stripTags(rendered), "", nil)
		}
		if err != nil {
			return 0, err
		}
		return m.MessageID, nil
	}
	err := g.editText(ctx, chatID, msgID, rendered, "HTML", nil)
	if IsNotModified(err) {
		return msgID, nil
	}
	if IsParseError(err) {
		err = g.editText(ctx, chatID, msgID, stripTags(rendered), "", nil)
		if IsNotModified(err) {
			return msgID, nil
		}
	}
	if err != nil {
		return msgID, err
	}
	return msgID, nil
}

// escapeHTML is a local alias which keeps call sites terse.
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// sendMarkdown formats and chunks a final message.
func (g *Gateway) sendMarkdown(ctx context.Context, chatID int64, text string) error {
	max := g.maxMessage()
	chunks := splitMarkdown(text, max)
	var errs []error
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		formatted := formatHTML(chunk)
		if utf8.RuneCountInString(formatted) > max {
			for utf8.RuneCountInString(chunk) > 0 {
				var part string
				part, chunk = takeRunes(chunk, max)
				if _, err := g.sendText(ctx, chatID, part, "", nil); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}
		if _, err := g.sendText(ctx, chatID, formatted, "HTML", nil); err != nil {
			if IsParseError(err) {
				if _, plainErr := g.sendText(ctx, chatID, stripTags(formatted), "", nil); plainErr != nil {
					errs = append(errs, plainErr)
				}
				continue
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sendText is the single rate-limited path to sendMessage.
func (g *Gateway) sendText(ctx context.Context, chatID int64, text, parseMode string, keyboard any) (Message, error) {
	max := g.maxMessage()
	if utf8.RuneCountInString(text) > max {
		text = truncateRunes(text, max-1)
	}
	for attempt := 0; attempt < 5; attempt++ {
		if err := g.global.wait(ctx); err != nil {
			return Message{}, err
		}
		if err := g.limiter(chatID).wait(ctx); err != nil {
			return Message{}, err
		}
		m, err := g.client.SendMessage(ctx, chatID, text, parseMode, keyboard)
		if err == nil {
			return m, nil
		}
		if d, ok := RetryAfter(err); ok {
			if waitErr := sleepContext(ctx, d); waitErr != nil {
				return Message{}, waitErr
			}
			continue
		}
		return Message{}, err
	}
	return Message{}, fmt.Errorf("telegram sendMessage: retry limit reached")
}

// editText is the single rate-limited path to editMessageText.
func (g *Gateway) editText(ctx context.Context, chatID int64, messageID int, text, parseMode string, keyboard any) error {
	max := g.maxMessage()
	if utf8.RuneCountInString(text) > max {
		text = truncateRunes(text, max-1)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := g.global.wait(ctx); err != nil {
			return err
		}
		if err := g.limiter(chatID).wait(ctx); err != nil {
			return err
		}
		err := g.client.EditMessageText(ctx, chatID, messageID, text, parseMode, keyboard)
		if err == nil || IsNotModified(err) {
			return nil
		}
		if d, ok := RetryAfter(err); ok {
			if waitErr := sleepContext(ctx, d); waitErr != nil {
				return waitErr
			}
			continue
		}
		return err
	}
	return fmt.Errorf("telegram editMessageText: retry limit reached")
}

func (g *Gateway) deleteMessage(ctx context.Context, chatID int64, messageID int) error {
	if messageID == 0 {
		return nil
	}
	if err := g.global.wait(ctx); err != nil {
		return err
	}
	if err := g.limiter(chatID).wait(ctx); err != nil {
		return err
	}
	return g.client.DeleteMessage(ctx, chatID, messageID)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
