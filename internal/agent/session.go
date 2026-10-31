package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bitvibe/kestrel/internal/config"
	"github.com/bitvibe/kestrel/internal/provider"
	"github.com/bitvibe/kestrel/internal/store"
	"github.com/bitvibe/kestrel/internal/tools"
)

// Manager owns one actor goroutine per active chat.
type Manager struct {
	engine  *Engine
	cfg     *config.Config
	store   *store.Store
	resolve func(chatID int64) Chat
	log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	sessions  map[int64]*sessionActor
	closed    bool
	closeDone chan struct{}
}

// Status is a snapshot for /status.
type Status struct {
	ChatID    int64
	SessionID string
	Running   bool
	Queued    int
	History   int
	Home      string
	Provider  string
	Model     string
}

// NewManager creates a session manager. resolve maps a chat to its frontend.
func NewManager(parent context.Context, engine *Engine, resolve func(int64) Chat, log *slog.Logger) *Manager {
	if parent == nil {
		parent = context.Background()
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	if resolve == nil {
		resolve = func(int64) Chat { return NopChat{} }
	}
	return &Manager{
		engine: engine, cfg: engine.cfg, store: engine.store,
		resolve: resolve, log: log,
		ctx: ctx, cancel: cancel,
		sessions: make(map[int64]*sessionActor),
	}
}

func (m *Manager) getOrCreate(chatID int64) (*sessionActor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrCancelled
	}
	if a, ok := m.sessions[chatID]; ok {
		return a, nil
	}
	a := &sessionActor{
		mgr:     m,
		chatID:  chatID,
		inbox:   make(chan event, max(16, m.cfg.Agent.MaxQueue*3)),
		homeDir: m.cfg.Workspace,
	}
	m.sessions[chatID] = a
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		a.loop()
	}()
	return a, nil
}

// Submit enqueues a user message. It returns ErrBusy instead of blocking when
// the actor's bounded inbox is full.
func (m *Manager) Submit(ctx context.Context, in Incoming) error {
	if strings.TrimSpace(in.Text) == "" && len(in.Images) == 0 {
		return errors.New("empty message")
	}
	a, err := m.getOrCreate(in.ChatID)
	if err != nil {
		return err
	}
	ev := event{kind: evUser, in: in}
	select {
	case a.inbox <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrBusy
	}
}

// SubmitScheduled implements sched.Runner.
func (m *Manager) SubmitScheduled(ctx context.Context, chatID int64, prompt string) error {
	return m.Submit(ctx, Incoming{ChatID: chatID, Text: prompt, Scheduled: true})
}

// Stop cancels the current run and clears the queue.
func (m *Manager) Stop(ctx context.Context, chatID int64) error {
	a, err := m.getOrCreate(chatID)
	if err != nil {
		return err
	}
	select {
	case a.inbox <- event{kind: evStop}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reset starts a fresh conversation, keeping the visible transcript in the DB.
func (m *Manager) Reset(ctx context.Context, chatID int64) error {
	a, err := m.getOrCreate(chatID)
	if err != nil {
		return err
	}
	ack := make(chan error, 1)
	select {
	case a.inbox <- event{kind: evReset, ack: ack}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetHome changes the per-chat working directory for shell commands.
func (m *Manager) SetHome(ctx context.Context, chatID int64, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = m.cfg.Workspace
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(m.cfg.Workspace, path)
	}
	resolved, err := tools.SecurePath(m.cfg.Tools.Roots, path)
	if err != nil {
		return "", err
	}
	st, err := osStatDir(resolved)
	if err != nil || !st {
		return "", fmt.Errorf("%s is not an existing directory", resolved)
	}
	if m.store != nil {
		if err := m.store.SetKV(ctx, fmt.Sprintf("home:%d", chatID), resolved); err != nil {
			return "", err
		}
	}
	a, err := m.getOrCreate(chatID)
	if err != nil {
		return "", err
	}
	ack := make(chan error, 1)
	select {
	case a.inbox <- event{kind: evHome, path: resolved, ack: ack}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-ack:
		return resolved, nil
	case <-ctx.Done():
		return resolved, ctx.Err()
	}
}

// Home returns the current chat home directory.
func (m *Manager) Home(chatID int64) string {
	m.mu.Lock()
	a, ok := m.sessions[chatID]
	m.mu.Unlock()
	if !ok {
		return m.cfg.Workspace
	}
	s := a.snapshot()
	if s.Home == "" {
		return m.cfg.Workspace
	}
	return s.Home
}

// Status returns a session snapshot for /status.
func (m *Manager) Status(chatID int64) Status {
	m.mu.Lock()
	a, ok := m.sessions[chatID]
	m.mu.Unlock()
	providerName, model := m.engine.Active()
	st := Status{ChatID: chatID, Provider: providerName, Model: model, Home: m.cfg.Workspace}
	if !ok {
		return st
	}
	s := a.snapshot()
	st.SessionID = s.SessionID
	st.Running = s.Running
	st.Queued = s.Queued
	st.History = s.History
	st.Home = s.Home
	return st
}

// Close stops all actors and waits for runs and persistence to finish.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
		m.closeDone = make(chan struct{})
		go func() {
			m.wg.Wait()
			close(m.closeDone)
		}()
	}
	done := m.closeDone
	m.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type eventKind int

const (
	evUser eventKind = iota
	evStop
	evReset
	evHome
)

type event struct {
	kind eventKind
	in   Incoming
	path string
	ack  chan error
}

type runResult struct {
	gen int
	out RunOutput
}

type statusSnapshot struct {
	SessionID string
	Running   bool
	Queued    int
	History   int
	Home      string
}

type sessionActor struct {
	mgr     *Manager
	chatID  int64
	inbox   chan event
	homeDir string

	// Owned by the actor goroutine:
	history       []provider.Message
	sessionID     string
	queue         []Incoming
	running       bool
	cancel        context.CancelFunc
	runDone       chan runResult
	generation    int
	stopRequested bool
	interruptNote bool
	resetAcks     []chan error

	mu   sync.Mutex
	snap statusSnapshot
}

func (s *sessionActor) updateSnap() {
	s.mu.Lock()
	s.snap = statusSnapshot{
		SessionID: s.sessionID, Running: s.running,
		Queued: len(s.queue), History: len(s.history), Home: s.homeDir,
	}
	s.mu.Unlock()
}

func (s *sessionActor) snapshot() statusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

func (s *sessionActor) loop() {
	ctx := s.mgr.ctx
	if s.mgr.store != nil {
		sess, err := s.mgr.store.EnsureSession(ctx, s.chatID, "")
		if err != nil {
			s.mgr.log.Warn("load session", "chat_id", s.chatID, "error", err)
		} else {
			s.sessionID = sess.ID
			msgs, err := s.mgr.store.Messages(ctx, sess.ID, s.mgr.cfg.Agent.MaxHistoryMessages)
			if err != nil {
				s.mgr.log.Warn("load history", "session", sess.ID, "error", err)
			}
			s.history = recordsToMessages(msgs)
		}
		if v, ok, err := s.mgr.store.GetKV(ctx, fmt.Sprintf("home:%d", s.chatID)); err == nil && ok {
			if p, err := tools.SecurePath(s.mgr.cfg.Tools.Roots, v); err == nil {
				s.homeDir = p
			}
		}
	}
	if s.homeDir == "" {
		s.homeDir = s.mgr.cfg.Workspace
	}
	s.updateSnap()

	for {
		var doneCh <-chan runResult
		if s.running {
			doneCh = s.runDone
		}
		select {
		case <-ctx.Done():
			if s.cancel != nil {
				s.cancel()
			}
			if s.running {
				res := <-s.runDone
				s.persist(res.out)
			}
			return
		case ev := <-s.inbox:
			if ctx.Err() != nil {
				continue
			}
			s.handle(ctx, ev)
		case res := <-doneCh:
			s.finish(ctx, res)
		}
	}
}

func (s *sessionActor) handle(ctx context.Context, ev event) {
	switch ev.kind {
	case evUser:
		s.enqueue(ctx, ev.in)
	case evStop:
		if s.running && s.cancel != nil {
			s.stopRequested = true
			s.queue = nil
			s.cancel()
			s.updateSnap()
		} else {
			s.chat().Send(ctx, "Nothing is running.")
		}
	case evReset:
		s.resetAcks = append(s.resetAcks, ev.ack)
		s.queue = nil
		if s.cancel != nil {
			s.cancel()
		}
		s.stopRequested = false
		s.updateSnap()
		if !s.running {
			s.reset(ctx)
		}
	case evHome:
		s.homeDir = ev.path
		s.updateSnap()
		if ev.ack != nil {
			ev.ack <- nil
		}
	}
}

func (s *sessionActor) enqueue(ctx context.Context, in Incoming) {
	if s.running {
		maxQueue := s.mgr.cfg.Agent.MaxQueue
		if maxQueue <= 0 {
			maxQueue = 8
		}
		if len(s.queue) >= maxQueue {
			s.chat().Send(ctx, "I'm busy and the queue is full. Use /stop, then resend your message.")
			return
		}
		if strings.EqualFold(s.mgr.cfg.Agent.Interrupt, "interrupt") && s.cancel != nil {
			s.interruptNote = true
			s.cancel()
		}
		s.queue = append(s.queue, in)
		s.updateSnap()
		return
	}
	s.startRun(ctx, in)
}

func (s *sessionActor) startRun(ctx context.Context, in Incoming) {
	s.running = true
	s.stopRequested = false
	s.interruptNote = false
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	done := make(chan runResult, 1)
	s.runDone = done
	gen := s.generation
	s.updateSnap()

	text := in.Text
	if in.Scheduled {
		text = "Scheduled task: " + text
	}
	userMsg := provider.Message{Role: provider.RoleUser, Content: text, Images: in.Images}
	history := make([]provider.Message, len(s.history))
	copy(history, s.history)
	sessionID, homeDir := s.sessionID, s.homeDir
	chat := s.chat()
	m := s.mgr
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		var out RunOutput
		func() {
			defer func() {
				if r := recover(); r != nil {
					out = RunOutput{History: append(history, userMsg), Err: fmt.Errorf("agent panic: %v", r)}
					m.log.Error("agent run panicked", "chat_id", s.chatID, "panic", r)
					_ = chat.Status(context.WithoutCancel(runCtx), RunStatus{Running: false, Phase: "error", Action: "the agent panicked"})
				}
			}()
			out = m.engine.Run(runCtx, RunInput{
				Chat:      chat,
				SessionID: sessionID,
				ChatID:    s.chatID,
				HomeDir:   homeDir,
				History:   history,
				User:      userMsg,
			})
		}()
		done <- runResult{gen: gen, out: out}
	}()
}

func (s *sessionActor) finish(ctx context.Context, res runResult) {
	s.running = false
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if res.out.History != nil {
		s.history = res.out.History
	}
	s.persist(res.out)
	s.updateSnap()
	if errors.Is(res.out.Err, ErrCancelled) {
		if aborter, ok := s.chat().(interface{ Abort(context.Context) error }); ok {
			_ = aborter.Abort(ctx)
		}
	}
	if len(s.resetAcks) > 0 {
		s.reset(ctx)
		return
	}

	switch {
	case errors.Is(res.out.Err, ErrCancelled):
		if s.stopRequested {
			s.chat().Send(ctx, "Stopped.")
		}
	case res.out.Err != nil:
		s.mgr.log.Warn("agent run failed", "chat_id", s.chatID, "error", res.out.Err)
	}
	s.stopRequested = false
	s.interruptNote = false
	if len(s.queue) > 0 && ctx.Err() == nil {
		next := s.queue[0]
		s.queue = s.queue[1:]
		s.startRun(ctx, next)
	}
	s.updateSnap()
}

func (s *sessionActor) reset(ctx context.Context) {
	var err error
	if s.mgr.store != nil {
		var sess store.Session
		sess, err = s.mgr.store.CreateSession(ctx, s.chatID, "")
		if err == nil {
			s.sessionID = sess.ID
		}
	}
	if err == nil {
		s.history = nil
		s.generation++
		s.chat().Send(ctx, "Started a new conversation.")
	} else {
		s.mgr.log.Warn("reset session", "error", err)
	}
	s.updateSnap()
	for _, ack := range s.resetAcks {
		if ack != nil {
			ack <- err
		}
	}
	s.resetAcks = nil
	if len(s.queue) > 0 && ctx.Err() == nil {
		next := s.queue[0]
		s.queue = s.queue[1:]
		s.startRun(ctx, next)
	}
}

func (s *sessionActor) persist(out RunOutput) {
	if s.mgr.store == nil || len(out.Messages) == 0 || s.sessionID == "" {
		return
	}
	sessionID := s.sessionID
	recs := messagesToRecords(sessionID, out.Messages)
	usage := out.Usage
	chatID := s.chatID
	m := s.mgr
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.store.AppendMessages(ctx, sessionID, recs); err != nil {
		m.log.Warn("persist messages", "chat_id", chatID, "error", err)
		return
	}
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
		if err := m.store.AddUsage(ctx, sessionID, out.Provider, out.Model, usage.PromptTokens, usage.CompletionTokens); err != nil {
			m.log.Warn("persist usage", "chat_id", chatID, "error", err)
		}
	}
}

func (s *sessionActor) chat() Chat {
	c := s.mgr.resolve(s.chatID)
	if c == nil {
		return NopChat{}
	}
	return c
}

func recordsToMessages(recs []store.MessageRecord) []provider.Message {
	out := make([]provider.Message, 0, len(recs))
	for _, r := range recs {
		m := provider.Message{
			Role:       provider.Role(r.Role),
			Content:    r.Content,
			Name:       r.Name,
			ToolCallID: r.ToolCallID,
		}
		if r.ToolCalls != "" {
			_ = json.Unmarshal([]byte(r.ToolCalls), &m.ToolCalls)
		}
		out = append(out, m)
	}
	return out
}

func messagesToRecords(sessionID string, msgs []provider.Message) []store.MessageRecord {
	out := make([]store.MessageRecord, 0, len(msgs))
	for _, m := range msgs {
		rec := store.MessageRecord{
			SessionID:  sessionID,
			Role:       string(m.Role),
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			if b, err := json.Marshal(m.ToolCalls); err == nil {
				rec.ToolCalls = string(b)
			}
		}
		out = append(out, rec)
	}
	return out
}

func osStatDir(path string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return st.IsDir(), nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
