// Package store persists sessions, messages, FTS5 search, usage, cron jobs and
// key/value settings in SQLite. A single writer goroutine owns all writes and
// batches them into transactions; reads use a separate pool of read-only
// connections. This is the only package that knows about SQL.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite with FTS5
)

// ErrClosed is returned after Close.
var ErrClosed = errors.New("store is closed")

// Config tunes the database.
type Config struct {
	Path          string
	ReadConns     int
	BusyTimeoutMS int
	MaxBatch      int
	FlushInterval time.Duration
	Logger        *slog.Logger
}

// Session is a conversation with one chat.
type Session struct {
	ID        string    `json:"id"`
	ChatID    int64     `json:"chat_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Active    bool      `json:"active"`
}

// MessageRecord is one persisted conversation message.
type MessageRecord struct {
	ID               int64     `json:"id"`
	SessionID        string    `json:"session_id"`
	Seq              int       `json:"seq"`
	Role             string    `json:"role"`
	Content          string    `json:"content"`
	Name             string    `json:"name,omitempty"`
	ToolCalls        string    `json:"tool_calls,omitempty"` // JSON array
	ToolCallID       string    `json:"tool_call_id,omitempty"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// SearchResult is one FTS hit.
type SearchResult struct {
	MessageID int64     `json:"message_id"`
	SessionID string    `json:"session_id"`
	ChatID    int64     `json:"chat_id"`
	Role      string    `json:"role"`
	Snippet   string    `json:"snippet"`
	CreatedAt time.Time `json:"created_at"`
}

// CronJob is a persisted scheduled prompt.
type CronJob struct {
	ID        string    `json:"id"`
	ChatID    int64     `json:"chat_id"`
	Prompt    string    `json:"prompt"`
	Schedule  string    `json:"schedule"`
	Enabled   bool      `json:"enabled"`
	LastRun   time.Time `json:"last_run,omitempty"`
	NextRun   time.Time `json:"next_run,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// MemoryNote is a curated note.
type MemoryNote struct {
	ID        int64     `json:"id"`
	Scope     string    `json:"scope"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Store owns the SQLite database.
type Store struct {
	write *sql.DB
	read  *sql.DB

	ch         chan *writeOp
	done       chan struct{}
	closedDone chan struct{}
	closeMu    sync.Mutex
	closed     bool
	closeErr   error
	wg         sync.WaitGroup

	maxBatch  int
	flushEach time.Duration
	log       *slog.Logger
}

type writeOp struct {
	fn   func(*sql.Tx) error
	done chan error
}

// Open opens (and if necessary creates) the database.
func Open(cfg Config) (*Store, error) {
	if cfg.Path == "" {
		return nil, errors.New("store path is required")
	}
	if cfg.ReadConns <= 0 {
		cfg.ReadConns = 3
	}
	if cfg.BusyTimeoutMS <= 0 {
		cfg.BusyTimeoutMS = 5000
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 128
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	write, err := sql.Open("sqlite", dsn(cfg.Path, false, cfg.BusyTimeoutMS))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := write.PingContext(ctx); err != nil {
		write.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := initSchema(ctx, write); err != nil {
		write.Close()
		return nil, fmt.Errorf("initialize schema: %w", err)
	}
	read, err := sql.Open("sqlite", dsn(cfg.Path, true, cfg.BusyTimeoutMS))
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	read.SetMaxOpenConns(cfg.ReadConns)
	read.SetMaxIdleConns(cfg.ReadConns)
	read.SetConnMaxLifetime(0)
	if err := read.PingContext(ctx); err != nil {
		read.Close()
		write.Close()
		return nil, fmt.Errorf("ping read pool: %w", err)
	}
	s := &Store{
		write:      write,
		read:       read,
		ch:         make(chan *writeOp, cfg.MaxBatch*2),
		done:       make(chan struct{}),
		closedDone: make(chan struct{}),
		maxBatch:   cfg.MaxBatch,
		flushEach:  cfg.FlushInterval,
		log:        cfg.Logger,
	}
	s.wg.Add(1)
	go s.writerLoop()
	return s, nil
}

func dsn(path string, readonly bool, busyMS int) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	p := filepath.ToSlash(abs)
	q := url.Values{}
	if readonly {
		q.Add("mode", "ro")
	}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyMS))
	if !readonly {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "temp_store(MEMORY)")
		q.Add("_pragma", "wal_autocheckpoint(1000)")
	} else {
		q.Add("_pragma", "query_only(1)")
	}
	return "file:" + p + "?" + q.Encode()
}

func initSchema(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	chat_id    INTEGER NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	active     INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_sessions_chat_active ON sessions(chat_id, active, updated_at DESC);

CREATE TABLE IF NOT EXISTS messages (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id        TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	seq               INTEGER NOT NULL,
	role              TEXT NOT NULL,
	content           TEXT NOT NULL DEFAULT '',
	name              TEXT NOT NULL DEFAULT '',
	tool_calls        TEXT NOT NULL DEFAULT '',
	tool_call_id      TEXT NOT NULL DEFAULT '',
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_session_seq ON messages(session_id, seq);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
	content,
	content='messages',
	content_rowid='id',
	tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.id, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE OF content ON messages BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.id, old.content);
	INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content);
END;

CREATE TABLE IF NOT EXISTS usage (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id        TEXT NOT NULL,
	provider          TEXT NOT NULL DEFAULT '',
	model             TEXT NOT NULL DEFAULT '',
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_session ON usage(session_id, created_at);

CREATE TABLE IF NOT EXISTS memories (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	scope      TEXT NOT NULL DEFAULT 'MEMORY',
	content    TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS cron_jobs (
	id         TEXT PRIMARY KEY,
	chat_id    INTEGER NOT NULL,
	prompt     TEXT NOT NULL,
	schedule   TEXT NOT NULL,
	enabled    INTEGER NOT NULL DEFAULT 1,
	last_run   INTEGER NOT NULL DEFAULT 0,
	next_run   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cron_next ON cron_jobs(enabled, next_run);

CREATE TABLE IF NOT EXISTS kv (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
PRAGMA user_version = 1;
`
	_, err := db.ExecContext(ctx, schema)
	return err
}

func nowMillis() int64 { return time.Now().UTC().UnixMilli() }

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// NewID returns a random identifier.
func NewID() string { return newID() }

// Close stops the writer, flushes queued writes and closes the database.
func (s *Store) Close(ctx context.Context) error {
	s.closeMu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
		go s.finishClose()
	}
	s.closeMu.Unlock()

	select {
	case <-s.closedDone:
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) finishClose() {
	<-s.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, checkpointErr := s.write.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	readErr := s.read.Close()
	writeErr := s.write.Close()
	s.closeErr = errors.Join(checkpointErr, readErr, writeErr)
	close(s.closedDone)
}

// health is used by tests and the health endpoint.
func (s *Store) Ping(ctx context.Context) error { return s.read.PingContext(ctx) }

func (s *Store) writerLoop() {
	defer s.wg.Done()
	defer close(s.done)
	for {
		op, ok := <-s.ch
		if !ok {
			return
		}
		batch := []*writeOp{op}
		timer := time.NewTimer(s.flushEach)
	fill:
		for len(batch) < s.maxBatch {
			select {
			case op2, ok2 := <-s.ch:
				if !ok2 {
					break fill
				}
				batch = append(batch, op2)
			case <-timer.C:
				break fill
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		s.flush(batch)
	}
}

func (s *Store) flush(batch []*writeOp) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		for _, op := range batch {
			op.done <- fmt.Errorf("begin write transaction: %w", err)
		}
		return
	}
	errs := make([]error, len(batch))
	for i, op := range batch {
		sp := fmt.Sprintf("op_%d", i)
		if _, err := tx.ExecContext(ctx, "SAVEPOINT "+sp); err != nil {
			errs[i] = err
			continue
		}
		if err := op.fn(tx); err != nil {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO "+sp)
			_, _ = tx.ExecContext(ctx, "RELEASE "+sp)
			errs[i] = err
			continue
		}
		if _, err := tx.ExecContext(ctx, "RELEASE "+sp); err != nil {
			errs[i] = err
		}
	}
	if err := tx.Commit(); err != nil {
		s.log.Error("sqlite batch commit failed", "error", err, "ops", len(batch))
		for i := range batch {
			if errs[i] == nil {
				errs[i] = fmt.Errorf("commit: %w", err)
			}
		}
	}
	for i, op := range batch {
		op.done <- errs[i]
	}
}

// Write runs fn inside the next batch transaction.
func (s *Store) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	op := &writeOp{fn: fn, done: make(chan error, 1)}
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return ErrClosed
	}
	select {
	case s.ch <- op:
		s.closeMu.Unlock()
	case <-ctx.Done():
		s.closeMu.Unlock()
		return ctx.Err()
	}
	select {
	case err := <-op.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CreateSession deactivates previous sessions for chatID and creates a new one.
func (s *Store) CreateSession(ctx context.Context, chatID int64, title string) (Session, error) {
	sess := Session{ID: newID(), ChatID: chatID, Title: title, Active: true}
	now := nowMillis()
	sess.CreatedAt, sess.UpdatedAt = fromMillis(now), fromMillis(now)
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active=0 WHERE chat_id=? AND active=1`, chatID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO sessions(id, chat_id, title, created_at, updated_at, active) VALUES(?,?,?,?,?,1)`,
			sess.ID, chatID, title, now, now)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// ActiveSession returns the current session for a chat, if any.
func (s *Store) ActiveSession(ctx context.Context, chatID int64) (Session, bool, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT id, chat_id, title, created_at, updated_at, active FROM sessions
		 WHERE chat_id=? AND active=1 ORDER BY updated_at DESC, rowid DESC LIMIT 1`, chatID)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	return sess, true, nil
}

// EnsureSession returns the active session or creates one.
func (s *Store) EnsureSession(ctx context.Context, chatID int64, title string) (Session, error) {
	if sess, ok, err := s.ActiveSession(ctx, chatID); err == nil && ok {
		return sess, nil
	}
	return s.CreateSession(ctx, chatID, title)
}

// GetSession returns a session by ID.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT id, chat_id, title, created_at, updated_at, active FROM sessions WHERE id=?`, id)
	return scanSession(row)
}

// ListSessions lists recent sessions for a chat (or all chats when chatID=0).
func (s *Store) ListSessions(ctx context.Context, chatID int64, limit int) ([]Session, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	var rows *sql.Rows
	var err error
	if chatID == 0 {
		rows, err = s.read.QueryContext(ctx,
			`SELECT id, chat_id, title, created_at, updated_at, active FROM sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	} else {
		rows, err = s.read.QueryContext(ctx,
			`SELECT id, chat_id, title, created_at, updated_at, active FROM sessions WHERE chat_id=? ORDER BY updated_at DESC LIMIT ?`, chatID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSession(row rowScanner) (Session, error) {
	var sess Session
	var created, updated int64
	var active int
	if err := row.Scan(&sess.ID, &sess.ChatID, &sess.Title, &created, &updated, &active); err != nil {
		return Session{}, err
	}
	sess.CreatedAt, sess.UpdatedAt = fromMillis(created), fromMillis(updated)
	sess.Active = active == 1
	return sess, nil
}

// TouchSession updates the session timestamp and optionally its title.
func (s *Store) TouchSession(ctx context.Context, id, title string) error {
	now := nowMillis()
	return s.Write(ctx, func(tx *sql.Tx) error {
		if title != "" {
			_, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at=?, title=? WHERE id=?`, now, title, id)
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at=? WHERE id=?`, now, id)
		return err
	})
}

// AppendMessages appends messages with increasing sequence numbers.
func (s *Store) AppendMessages(ctx context.Context, sessionID string, msgs []MessageRecord) error {
	if len(msgs) == 0 {
		return nil
	}
	now := nowMillis()
	return s.Write(ctx, func(tx *sql.Tx) error {
		var maxSeq int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM messages WHERE session_id=?`, sessionID).Scan(&maxSeq); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO messages(session_id, seq, role, content, name, tool_calls, tool_call_id, prompt_tokens, completion_tokens, created_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i, m := range msgs {
			created := now
			if !m.CreatedAt.IsZero() {
				created = m.CreatedAt.UnixMilli()
			}
			if _, err := stmt.ExecContext(ctx, sessionID, maxSeq+i+1, m.Role, m.Content, m.Name, m.ToolCalls, m.ToolCallID, m.PromptTokens, m.CompletionTokens, created); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at=? WHERE id=?`, now, sessionID)
		return err
	})
}

// Messages returns up to limit most recent messages in chronological order.
func (s *Store) Messages(ctx context.Context, sessionID string, limit int) ([]MessageRecord, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, session_id, seq, role, content, name, tool_calls, tool_call_id, prompt_tokens, completion_tokens, created_at
		 FROM messages WHERE session_id=? ORDER BY seq DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageRecord
	for rows.Next() {
		var m MessageRecord
		var created int64
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Seq, &m.Role, &m.Content, &m.Name, &m.ToolCalls, &m.ToolCallID, &m.PromptTokens, &m.CompletionTokens, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = fromMillis(created)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// AddUsage records token usage for a run.
func (s *Store) AddUsage(ctx context.Context, sessionID, providerName, model string, prompt, completion int) error {
	now := nowMillis()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO usage(session_id, provider, model, prompt_tokens, completion_tokens, created_at) VALUES(?,?,?,?,?,?)`,
			sessionID, providerName, model, prompt, completion, now)
		return err
	})
}

// UsageTotals returns lifetime totals for a session.
func (s *Store) UsageTotals(ctx context.Context, sessionID string) (prompt, completion int, err error) {
	err = s.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0) FROM usage WHERE session_id=?`,
		sessionID).Scan(&prompt, &completion)
	return
}

// SearchMessages runs an FTS5 query over past conversation messages.
func (s *Store) SearchMessages(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, errors.New("search query is empty")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT m.id, m.session_id, s.chat_id, m.role,
		       snippet(messages_fts, 0, '[', ']', '…', 16), m.created_at
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE messages_fts MATCH ? AND m.role IN ('user','assistant')
		ORDER BY bm25(messages_fts) LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		var created int64
		if err := rows.Scan(&r.MessageID, &r.SessionID, &r.ChatID, &r.Role, &r.Snippet, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = fromMillis(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

func ftsQuery(q string) string {
	var toks []string
	for _, f := range strings.FieldsFunc(q, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' && r > 127
	}) {
		if len([]rune(f)) < 2 {
			continue
		}
		toks = append(toks, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
		if len(toks) >= 12 {
			break
		}
	}
	return strings.Join(toks, " ")
}

// SetKV stores a runtime setting.
func (s *Store) SetKV(ctx context.Context, key, value string) error {
	now := nowMillis()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO kv(key, value, updated_at) VALUES(?,?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			key, value, now)
		return err
	})
}

// GetKV reads a runtime setting.
func (s *Store) GetKV(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ListKV returns all runtime settings whose key starts with prefix.
func (s *Store) ListKV(ctx context.Context, prefix string) (map[string]string, error) {
	pattern := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix) + "%"
	rows, err := s.read.QueryContext(ctx, `SELECT key, value FROM kv WHERE key LIKE ? ESCAPE '\'`, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// AddMemory appends a curated note.
func (s *Store) AddMemory(ctx context.Context, scope, content string) error {
	now := nowMillis()
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO memories(scope, content, created_at) VALUES(?,?,?)`, scope, content, now)
		return err
	})
}

// ListMemories lists curated notes, newest last.
func (s *Store) ListMemories(ctx context.Context, scope string, limit int) ([]MemoryNote, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, scope, content, created_at FROM memories WHERE scope=? ORDER BY id ASC LIMIT ?`, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryNote
	for rows.Next() {
		var n MemoryNote
		var created int64
		if err := rows.Scan(&n.ID, &n.Scope, &n.Content, &created); err != nil {
			return nil, err
		}
		n.CreatedAt = fromMillis(created)
		out = append(out, n)
	}
	return out, rows.Err()
}

// UpsertCronJob creates or updates a schedule.
func (s *Store) UpsertCronJob(ctx context.Context, j CronJob) error {
	now := nowMillis()
	return s.Write(ctx, func(tx *sql.Tx) error {
		if j.ID == "" {
			j.ID = newID()
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO cron_jobs(id, chat_id, prompt, schedule, enabled, last_run, next_run, created_at)
			VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET chat_id=excluded.chat_id, prompt=excluded.prompt,
			  schedule=excluded.schedule, enabled=excluded.enabled, next_run=excluded.next_run`,
			j.ID, j.ChatID, j.Prompt, j.Schedule, boolInt(j.Enabled), j.LastRun.UnixMilli(), j.NextRun.UnixMilli(), now)
		return err
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ListCronJobs returns scheduled jobs, optionally only for one chat.
func (s *Store) ListCronJobs(ctx context.Context, chatID int64) ([]CronJob, error) {
	var rows *sql.Rows
	var err error
	if chatID == 0 {
		rows, err = s.read.QueryContext(ctx,
			`SELECT id, chat_id, prompt, schedule, enabled, last_run, next_run, created_at FROM cron_jobs ORDER BY created_at`)
	} else {
		rows, err = s.read.QueryContext(ctx,
			`SELECT id, chat_id, prompt, schedule, enabled, last_run, next_run, created_at FROM cron_jobs WHERE chat_id=? ORDER BY created_at`, chatID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CronJob
	for rows.Next() {
		var j CronJob
		var enabled, last, next, created int64
		if err := rows.Scan(&j.ID, &j.ChatID, &j.Prompt, &j.Schedule, &enabled, &last, &next, &created); err != nil {
			return nil, err
		}
		j.Enabled = enabled == 1
		j.LastRun, j.NextRun, j.CreatedAt = fromMillis(last), fromMillis(next), fromMillis(created)
		out = append(out, j)
	}
	return out, rows.Err()
}

// DeleteCronJob removes a job for a chat, reporting whether it existed.
func (s *Store) DeleteCronJob(ctx context.Context, chatID int64, id string) (bool, error) {
	var found bool
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM cron_jobs WHERE id=? AND chat_id=?`, id, chatID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		found = n > 0
		return nil
	})
	return found, err
}

// MarkCronRun records last/next run times.
func (s *Store) MarkCronRun(ctx context.Context, id string, last, next time.Time) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE cron_jobs SET last_run=?, next_run=? WHERE id=?`, last.UnixMilli(), next.UnixMilli(), id)
		return err
	})
}
