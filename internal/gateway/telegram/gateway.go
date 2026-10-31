// Package telegram implements Kestrel's primary gateway with no third-party
// Telegram dependency. It supports long polling, optional webhooks, access
// control, inline approvals, rate-limited streaming edits and file transfer.
package telegram

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bitvibe/kestrel/internal/agent"
	"github.com/bitvibe/kestrel/internal/config"
	"github.com/bitvibe/kestrel/internal/logx"
	"github.com/bitvibe/kestrel/internal/netx"
	"github.com/bitvibe/kestrel/internal/provider"
	"github.com/bitvibe/kestrel/internal/store"
)

const (
	maxAttachmentBytes = 20 << 20 // Bot API getFile limit
	maxPhotoBytes      = 10 << 20
	maxTextAttachment  = 128 << 10
	gatewayWorkers     = 2
)

// Gateway wires Telegram to the agent engine and session manager.
type Gateway struct {
	cfg     *config.Config
	log     *slog.Logger
	store   *store.Store
	client  *Client
	allow   *Allowlist
	approve *Approvals
	voice   *provider.Transcriber
	version string
	started time.Time

	httpFactory  *netx.Factory
	voiceFactory *netx.Factory

	engine  *agent.Engine
	manager *agent.Manager

	mu        sync.Mutex
	chats     map[int64]*Chat
	chatTypes map[int64]string
	limiters  map[int64]*rateLimiter
	global    *rateLimiter
	queues    []chan Update
	wg        sync.WaitGroup
	running   bool
	botUser   User
	webhook   string
}

// New builds the gateway, its HTTP transport and Telegram client. The engine
// and session manager are attached after the tool registry is constructed,
// because the registry needs the approval channel that lives here.
func New(cfg *config.Config, st *store.Store, log *slog.Logger, version string) (*Gateway, error) {
	if cfg == nil {
		return nil, errors.New("telegram gateway requires configuration")
	}
	if log == nil {
		log = slog.Default()
	}
	if strings.TrimSpace(cfg.Telegram.Token) == "" {
		return nil, errors.New("telegram token is empty")
	}
	proxySpec := cfg.ResolveProxy(cfg.Telegram.Proxy, "")
	factory := netx.NewFactory(netx.Config{
		UserAgent:       cfg.HTTP.UserAgent,
		MaxIdleConns:    cfg.HTTP.MaxIdleConns,
		IdleConnTimeout: cfg.HTTP.IdleConnTimeout.D(),
		TLSHandshake:    cfg.HTTP.TLSHandshakeTimout.D(),
	})
	hc, err := factory.Client(proxySpec, 0)
	if err != nil {
		return nil, fmt.Errorf("telegram HTTP transport: %w", err)
	}
	g := &Gateway{
		cfg:         cfg,
		log:         log,
		store:       st,
		client:      NewClient(cfg.Telegram.Token, cfg.Telegram.APIBaseURL, hc, log),
		version:     version,
		started:     time.Now(),
		httpFactory: factory,
		chats:       make(map[int64]*Chat),
		chatTypes:   make(map[int64]string),
		limiters:    make(map[int64]*rateLimiter),
		global:      newRateLimiter(30 * time.Millisecond),
	}
	g.allow = NewAllowlist(cfg.Telegram.AllowedUsers, cfg.Telegram.PairingCode, st, log)
	g.approve = NewApprovals(g, log)
	if cfg.Voice.Enabled {
		g.voice = g.buildTranscriber()
	}
	log.Info("telegram gateway configured", "mode", cfg.Telegram.Mode, "proxy", netx.Redact(proxySpec), "allowed_users", len(cfg.Telegram.AllowedUsers))
	return g, nil
}

// Attach connects the agent engine and session manager.
func (g *Gateway) Attach(engine *agent.Engine, manager *agent.Manager) {
	g.engine = engine
	g.manager = manager
}

// Approver exposes the approval channel for the tool registry.
func (g *Gateway) Approver() *Approvals { return g.approve }

// Allowlist returns the access-control list (useful for /status and tests).
func (g *Gateway) Allowlist() *Allowlist { return g.allow }

// Conversation returns the agent.Chat bound to a Telegram chat.
func (g *Gateway) Conversation(chatID int64) agent.Chat { return g.chat(chatID) }

// Resolver returns a chat resolver suitable for agent.NewManager.
func (g *Gateway) Resolver() func(int64) agent.Chat {
	return func(chatID int64) agent.Chat { return g.chat(chatID) }
}

func (g *Gateway) chat(chatID int64) *Chat {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.chats[chatID]; ok {
		return c
	}
	c := newChat(g, chatID)
	g.chats[chatID] = c
	return c
}

func (g *Gateway) limiter(chatID int64) *rateLimiter {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l, ok := g.limiters[chatID]; ok {
		return l
	}
	l := newRateLimiter(350 * time.Millisecond)
	g.limiters[chatID] = l
	return l
}

func (g *Gateway) maxMessage() int {
	max := g.cfg.Telegram.MaxMessageSize
	if max <= 0 || max > telegramMaxMessage {
		max = telegramMaxMessage
	}
	return max
}

func (g *Gateway) editInterval() time.Duration {
	d := g.cfg.Telegram.EditInterval.D()
	if d < time.Second {
		d = time.Second
	}
	return d
}

func (g *Gateway) streamEnabled() bool {
	return g.cfg.Telegram.Stream == nil || *g.cfg.Telegram.Stream
}

func (g *Gateway) progressEnabled() bool {
	return g.cfg.Telegram.Progress == nil || *g.cfg.Telegram.Progress
}

func (g *Gateway) logActions() bool {
	return g.cfg.Telegram.LogActions == nil || *g.cfg.Telegram.LogActions
}

func (g *Gateway) reasoningEnabled() bool {
	return g.cfg.Telegram.ShowReasoning == nil || *g.cfg.Telegram.ShowReasoning
}

func (g *Gateway) draftStreaming() bool {
	return g.cfg.Telegram.DraftStreaming == nil || *g.cfg.Telegram.DraftStreaming
}

func (g *Gateway) pinnedStatus() bool {
	return g.cfg.Telegram.PinnedStatus == nil || *g.cfg.Telegram.PinnedStatus
}

// setChatType remembers the Bot API chat type from the latest update. Drafts
// are only available in private chats, and pinned status boards need to know
// whether pinning is likely to succeed.
func (g *Gateway) setChatType(chatID int64, chatType string) {
	chatType = strings.TrimSpace(chatType)
	if chatID == 0 || chatType == "" {
		return
	}
	g.mu.Lock()
	g.chatTypes[chatID] = chatType
	g.mu.Unlock()
}

func (g *Gateway) chatType(chatID int64) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.chatTypes[chatID]
}

func (g *Gateway) isPrivateChat(chatID int64) bool {
	return g.chatType(chatID) == "private"
}

// Run verifies the bot, publishes commands and then serves updates until ctx
// is cancelled.
func (g *Gateway) Run(ctx context.Context) error {
	if g.engine == nil || g.manager == nil {
		return errors.New("telegram gateway is not attached to an agent")
	}
	g.mu.Lock()
	if g.running {
		g.mu.Unlock()
		return errors.New("telegram gateway is already running")
	}
	g.running = true
	g.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		g.mu.Lock()
		g.running = false
		g.mu.Unlock()
	}()

	g.allow.Load(ctx)
	bot, err := g.getMe(ctx)
	if err != nil {
		return err
	}
	g.botUser = bot
	g.log.Info("telegram bot ready", "username", bot.Username, "id", bot.ID)
	if err := g.client.SetMyCommands(ctx, botCommands); err != nil {
		g.log.Debug("setMyCommands failed", "error", err)
	}

	g.queues = make([]chan Update, gatewayWorkers)
	for i := range g.queues {
		g.queues[i] = make(chan Update, 64)
		g.wg.Add(1)
		go g.worker(ctx, g.queues[i])
	}
	defer func() {
		cancel()
		g.waitWorkers()
	}()

	if strings.EqualFold(g.cfg.Telegram.Mode, "webhook") {
		return g.runWebhook(ctx)
	}
	return g.runPolling(ctx)
}

func (g *Gateway) waitWorkers() {
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		g.log.Warn("timed out waiting for Telegram workers")
	}
	if g.httpFactory != nil {
		g.httpFactory.CloseIdle()
	}
	if g.voiceFactory != nil {
		g.voiceFactory.CloseIdle()
	}
}

func (g *Gateway) getMe(ctx context.Context) (User, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, time.Duration(attempt)*2*time.Second); err != nil {
				return User{}, err
			}
		}
		bot, err := g.client.GetMe(ctx)
		if err == nil {
			return bot, nil
		}
		lastErr = err
		if d, ok := RetryAfter(err); ok {
			if err := sleepContext(ctx, d); err != nil {
				return User{}, err
			}
		}
	}
	return User{}, fmt.Errorf("telegram getMe failed: %w", lastErr)
}

func (g *Gateway) runPolling(ctx context.Context) error {
	if err := g.client.DeleteWebhook(ctx); err != nil {
		g.log.Debug("deleteWebhook failed", "error", err)
	}
	offset := int64(0)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		timeout := g.cfg.Telegram.PollTimeout.D()
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout+20*time.Second)
		updates, err := g.client.GetUpdates(callCtx, offset, timeout)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if d, ok := RetryAfter(err); ok {
				g.log.Warn("telegram rate limited", "retry_after", d)
				if err := sleepContext(ctx, d); err != nil {
					return nil
				}
				continue
			}
			g.log.Warn("telegram polling failed", "error", logx.Redact(err.Error()), "retry_in", backoff)
			if err := sleepContext(ctx, backoff); err != nil {
				return nil
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			g.dispatch(ctx, u)
		}
	}
}

func (g *Gateway) runWebhook(ctx context.Context) error {
	rawURL := strings.TrimSpace(g.cfg.Telegram.WebhookURL)
	if rawURL == "" {
		return errors.New("telegram.webhook_url is required in webhook mode")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid telegram.webhook_url %q", rawURL)
	}
	path := parsed.Path
	if path == "" || path == "/" {
		path = "/telegram/webhook"
	}
	addr := strings.TrimSpace(g.cfg.Telegram.WebhookAddr)
	if addr == "" {
		addr = "0.0.0.0:8443"
	}
	secret := strings.TrimSpace(g.cfg.Telegram.WebhookSecret)
	if secret == "" {
		var b [24]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		secret = hex.EncodeToString(b[:])
		g.log.Warn("generated an ephemeral webhook secret; set telegram.webhook_secret to verify restarts")
	}
	g.webhook = secret

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(secret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		defer r.Body.Close()
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		var u Update
		if err := dec.Decode(&u); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		g.dispatch(r.Context(), u)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	if err := g.client.SetWebhook(ctx, rawURL, secret); err != nil {
		ln.Close()
		return fmt.Errorf("setWebhook: %w", err)
	}
	g.log.Info("telegram webhook listening", "addr", addr, "path", path)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (g *Gateway) dispatch(ctx context.Context, u Update) {
	if len(g.queues) == 0 {
		return
	}
	key := updateChatID(u)
	idx := 0
	if key != 0 {
		idx = int(uint64(key) % uint64(len(g.queues)))
	}
	select {
	case g.queues[idx] <- u:
	case <-ctx.Done():
	}
}

func updateChatID(u Update) int64 {
	switch {
	case u.Message != nil:
		return u.Message.Chat.ID
	case u.EditedMessage != nil:
		return u.EditedMessage.Chat.ID
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil:
		return u.CallbackQuery.Message.Chat.ID
	}
	return 0
}

func (g *Gateway) worker(ctx context.Context, queue <-chan Update) {
	defer g.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case u := <-queue:
			func() {
				defer func() {
					if r := recover(); r != nil {
						g.log.Error("telegram update panicked", "panic", r)
					}
				}()
				uctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
				defer cancel()
				g.handleUpdate(uctx, u)
			}()
		}
	}
}

func (g *Gateway) handleUpdate(ctx context.Context, u Update) {
	if u.MessageGenerationStopped != nil {
		chatID := u.MessageGenerationStopped.Chat.ID
		g.setChatType(chatID, u.MessageGenerationStopped.Chat.Type)
		// Drafts are only available in private chats, where the chat ID is
		// the user ID, so the allowlist still gates the action.
		if chatID == 0 || !g.allow.Allowed(chatID) || g.manager == nil {
			return
		}
		if err := g.manager.Stop(ctx, chatID); err != nil {
			g.log.Debug("stop from draft button", "chat_id", chatID, "error", err)
		}
		return
	}
	if u.CallbackQuery != nil {
		if u.CallbackQuery.From == nil || !g.allow.Allowed(u.CallbackQuery.From.ID) {
			return
		}
		if g.approve.HandleCallback(ctx, u.CallbackQuery) {
			return
		}
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = g.client.AnswerCallbackQuery(actx, u.CallbackQuery.ID, "")
		return
	}
	msg := u.Message
	if msg == nil {
		return
	}
	if msg.From == nil || msg.From.IsBot || msg.Chat.ID == 0 {
		return
	}
	userID := msg.From.ID
	chatID := msg.Chat.ID
	g.setChatType(chatID, msg.Chat.Type)
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Caption)
	}

	if !g.allow.Allowed(userID) {
		name, targeted := g.commandFor(text)
		if targeted && name == "pair" {
			_, code, _ := parseCommand(text)
			if err := g.allow.Pair(ctx, userID, code); err == nil {
				_ = g.sendMarkdown(ctx, chatID, "Paired successfully. You now have access.")
			}
			return
		}
		g.log.Warn("rejected unauthorized user", "user_id", userID, "chat_id", chatID, "username", msg.From.Username)
		return
	}

	if _, targeted := g.commandFor(text); targeted {
		g.handleCommand(ctx, chatID, userID, text)
		return
	}
	if _, _, isCommand := parseCommand(text); isCommand {
		// Command addressed to another bot in a group chat.
		return
	}

	prompt, images, err := g.buildIncoming(ctx, chatID, msg)
	if err != nil {
		g.log.Warn("attachment handling failed", "chat_id", chatID, "error", err)
		_ = g.sendMarkdown(ctx, chatID, "I could not process that attachment: "+logx.Redact(err.Error()))
		return
	}
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		_ = g.sendMarkdown(ctx, chatID, "I received an empty message.")
		return
	}
	if g.manager == nil {
		_ = g.sendMarkdown(ctx, chatID, "The agent is not initialized.")
		return
	}
	if err := g.manager.Submit(ctx, agent.Incoming{
		ChatID: chatID,
		UserID: userID,
		Text:   prompt,
		Images: images,
	}); err != nil {
		if errors.Is(err, agent.ErrBusy) {
			_ = g.sendMarkdown(ctx, chatID, "I'm busy and the queue is full. Use /stop, then resend your message.")
			return
		}
		g.log.Warn("submit message", "chat_id", chatID, "error", err)
		_ = g.sendMarkdown(ctx, chatID, "I could not accept that message: "+logx.Redact(err.Error()))
	}
}

// commandFor recognizes commands addressed to this bot.
func (g *Gateway) commandFor(text string) (string, bool) {
	cmd, _, ok := parseCommand(text)
	if !ok {
		return "", false
	}
	if at := strings.IndexByte(cmd, '@'); at >= 0 {
		mention := cmd[at+1:]
		cmd = cmd[:at]
		if g.botUser.Username != "" && !strings.EqualFold(mention, g.botUser.Username) {
			return "", false
		}
	}
	return strings.ToLower(cmd), true
}

func (g *Gateway) buildIncoming(ctx context.Context, chatID int64, msg *Message) (string, []provider.Image, error) {
	var parts []string
	var images []provider.Image
	if t := strings.TrimSpace(msg.Text); t != "" {
		parts = append(parts, t)
	}
	if c := strings.TrimSpace(msg.Caption); c != "" && c != strings.TrimSpace(msg.Text) {
		parts = append(parts, c)
	}
	if msg.Document != nil {
		doc := msg.Document
		if doc.FileSize > maxAttachmentBytes {
			return "", nil, fmt.Errorf("document is %d bytes; Telegram bots can download at most %d", doc.FileSize, maxAttachmentBytes)
		}
		data, _, err := g.client.DownloadFile(ctx, doc.FileID, maxAttachmentBytes)
		if err != nil {
			return "", nil, err
		}
		path, err := g.saveDownload(chatID, doc.FileName, data)
		if err != nil {
			return "", nil, err
		}
		name := doc.FileName
		if name == "" {
			name = filepath.Base(path)
		}
		if isTextual(doc.MIMEType, path) && len(data) <= maxTextAttachment {
			parts = append(parts, fmt.Sprintf("User sent a document named %q, saved at %s. Contents:\n<<<FILE\n%s\nFILE", name, path, string(data)))
		} else {
			parts = append(parts, fmt.Sprintf("User sent a document named %q (%s, %d bytes), saved at %s. Use read_file to inspect it.", name, doc.MIMEType, len(data), path))
		}
		if g.cfg.Skills.Enabled && looksLikeSkillSource(name, doc.MIMEType, data) {
			parts = append(parts, fmt.Sprintf("This looks like an agent skill. Install it with the skills tool: action=install, path=%q. If the package contains several skills, pass skill=<name>.", path))
		}
	}
	if len(msg.Photo) > 0 {
		photo := msg.Photo[0]
		for _, p := range msg.Photo {
			if p.Width*p.Height > photo.Width*photo.Height {
				photo = p
			}
		}
		data, _, err := g.client.DownloadFile(ctx, photo.FileID, maxPhotoBytes)
		if err != nil {
			return "", nil, err
		}
		if g.visionEnabled() {
			images = append(images, provider.Image{Data: data, MIMEType: "image/jpeg"})
			parts = append(parts, "[The user attached a photo.]")
		} else {
			path, err := g.saveDownload(chatID, fmt.Sprintf("photo-%d.jpg", time.Now().Unix()), data)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, fmt.Sprintf("User sent a photo saved at %s. The active model has no vision support; use tools to inspect it if needed.", path))
		}
	}
	if msg.Voice != nil {
		text, err := g.transcribe(ctx, msg.Voice.FileID, msg.Voice.MIMEType, "voice.ogg", msg.Voice.FileSize)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, "User sent a voice message. Transcript:\n"+text)
	}
	if msg.Audio != nil {
		audio := msg.Audio
		if g.voice != nil && audio.FileSize <= maxAttachmentBytes {
			text, err := g.transcribe(ctx, audio.FileID, audio.MIMEType, "audio."+audioExt(audio.FileName), audio.FileSize)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, "User sent an audio file. Transcript:\n"+text)
		} else {
			if audio.FileSize > maxAttachmentBytes {
				return "", nil, fmt.Errorf("audio file is too large to download")
			}
			data, _, err := g.client.DownloadFile(ctx, audio.FileID, maxAttachmentBytes)
			if err != nil {
				return "", nil, err
			}
			path, err := g.saveDownload(chatID, audio.FileName, data)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, fmt.Sprintf("User sent an audio file saved at %s.", path))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n")), images, nil
}

// looksLikeSkillSource recognizes dropped skill packages and SKILL.md files.
// Plain documents and unrelated archives are left alone so the model is not
// nudged to install every attachment.
func looksLikeSkillSource(name, mime string, data []byte) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case lower == "skill.md":
		return true
	case strings.HasSuffix(lower, ".zip"),
		strings.HasSuffix(lower, ".tar.gz"),
		strings.HasSuffix(lower, ".tgz"),
		strings.HasSuffix(lower, ".tar"):
		return true
	}
	if strings.HasSuffix(lower, ".md") || strings.Contains(strings.ToLower(mime), "markdown") {
		text := string(data)
		if !strings.HasPrefix(text, "---") {
			return false
		}
		rest := text[3:]
		if i := strings.Index(rest, "\n---"); i >= 0 {
			front := strings.ToLower(rest[:i])
			return strings.Contains(front, "name:") && strings.Contains(front, "description:")
		}
	}
	return false
}

func (g *Gateway) visionEnabled() bool {
	if g.engine == nil {
		return false
	}
	name, _ := g.engine.Active()
	if p, ok := g.engine.Providers().Get(name); ok {
		return p.SupportsVision()
	}
	return false
}

func (g *Gateway) transcribe(ctx context.Context, fileID, mime, fallbackName string, size int64) (string, error) {
	if g.voice == nil {
		return "", errors.New("voice transcription is disabled (set voice.enabled and voice.model)")
	}
	if size > maxAttachmentBytes {
		return "", fmt.Errorf("audio file exceeds %d bytes", maxAttachmentBytes)
	}
	data, path, err := g.client.DownloadFile(ctx, fileID, maxAttachmentBytes)
	if err != nil {
		return "", err
	}
	name := filepath.Base(path)
	if name == "" || name == "." {
		name = fallbackName
	}
	text, err := g.voice.Transcribe(ctx, data, name, mime)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("transcription returned no text")
	}
	return text, nil
}

func (g *Gateway) buildTranscriber() *provider.Transcriber {
	cfgProv, ok := g.cfg.Provider(activeProviderName(g.cfg))
	base := strings.TrimSpace(g.cfg.Voice.BaseURL)
	key := g.cfg.Voice.APIKey
	audioPath := "/audio/transcriptions"
	headers := map[string]string(nil)
	if ok {
		if base == "" {
			base = cfgProv.BaseURL
		}
		if key == "" {
			key = cfgProv.APIKey
		}
		if cfgProv.AudioPath != "" {
			audioPath = cfgProv.AudioPath
		}
		if len(cfgProv.Headers) > 0 {
			headers = make(map[string]string, len(cfgProv.Headers))
			for k, v := range cfgProv.Headers {
				headers[k] = v
			}
		}
	}
	if base == "" {
		g.log.Warn("voice transcription enabled but no base_url is configured")
		return nil
	}
	spec := g.cfg.ResolveProxy(g.cfg.Voice.Proxy, "")
	g.voiceFactory = netx.NewFactory(netx.Config{UserAgent: g.cfg.HTTP.UserAgent})
	hc, err := g.voiceFactory.Client(spec, 0)
	if err != nil {
		g.log.Warn("voice HTTP transport", "error", err)
		return nil
	}
	model := strings.TrimSpace(g.cfg.Voice.Model)
	if model == "" {
		model = "whisper-1"
	}
	timeout := g.cfg.Voice.Timeout.D()
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &provider.Transcriber{
		BaseURL:  base,
		APIKey:   key,
		Model:    model,
		Language: g.cfg.Voice.Language,
		Path:     audioPath,
		Headers:  headers,
		HTTP:     hc,
		Timeout:  timeout,
	}
}

func activeProviderName(cfg *config.Config) string {
	if name, _, err := cfg.ActiveProviderModel(); err == nil {
		return name
	}
	return ""
}

func (g *Gateway) saveDownload(chatID int64, name string, data []byte) (string, error) {
	dir := filepath.Join(g.managerHome(chatID), "downloads")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create downloads directory: %w", err)
	}
	name = sanitizeFilename(name)
	path := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, i, ext))
		if i > 1000 {
			return "", errors.New("could not choose a unique download name")
		}
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return "", fmt.Errorf("save download: %w", err)
	}
	return path, nil
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"|?*`, r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 120 {
			break
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return "file"
	}
	return out
}

func isTextual(mime, path string) bool {
	m := strings.ToLower(mime)
	if strings.HasPrefix(m, "text/") || strings.Contains(m, "json") || strings.Contains(m, "xml") ||
		strings.Contains(m, "yaml") || strings.Contains(m, "toml") || strings.Contains(m, "javascript") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md", ".txt", ".log", ".csv", ".json", ".yaml", ".yml", ".toml", ".ini", ".cfg",
		".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".rs", ".c", ".h", ".cpp", ".java", ".kt",
		".sh", ".bash", ".zsh", ".sql", ".html", ".css", ".xml", ".mod", ".sum", ".env":
		return true
	}
	return false
}

func audioExt(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp3", ".m4a", ".ogg", ".oga", ".wav", ".flac", ".webm", ".aac", ".opus":
		return strings.TrimPrefix(ext, ".")
	}
	return "ogg"
}
