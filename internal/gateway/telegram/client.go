// Package telegram implements Kestrel's primary gateway with no third-party
// Telegram dependency. It supports long polling, optional webhooks, access
// control, inline approvals, rate-limited streaming edits and file transfer.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bitvibe/kestrel/internal/logx"
)

// User is a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// TelegramChat is a chat in a Bot API update.
type TelegramChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

// Message is the subset of Telegram message fields Kestrel uses.
type Message struct {
	MessageID int          `json:"message_id"`
	From      *User        `json:"from"`
	Chat      TelegramChat `json:"chat"`
	Date      int64        `json:"date"`
	Text      string       `json:"text"`
	Caption   string       `json:"caption"`
	Document  *Document    `json:"document"`
	Photo     []PhotoSize  `json:"photo"`
	Voice     *Voice       `json:"voice"`
	Audio     *Audio       `json:"audio"`
}

// Document is a Telegram document.
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// PhotoSize is one resolution of a photo.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

// Voice is a voice note.
type Voice struct {
	FileID   string `json:"file_id"`
	MIMEType string `json:"mime_type"`
	Duration int    `json:"duration"`
	FileSize int64  `json:"file_size"`
}

// Audio is an audio file.
type Audio struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// CallbackQuery is an inline button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// Update is a Telegram update.
type Update struct {
	UpdateID                 int64                     `json:"update_id"`
	Message                  *Message                  `json:"message"`
	EditedMessage            *Message                  `json:"edited_message"`
	CallbackQuery            *CallbackQuery            `json:"callback_query"`
	MessageGenerationStopped *MessageGenerationStopped `json:"stopped_message_generation"`
}

// MessageGenerationStopped reports that a user pressed the stop button shown
// on a streaming draft.
type MessageGenerationStopped struct {
	Chat            TelegramChat `json:"chat"`
	MessageThreadID int          `json:"message_thread_id"`
	DraftID         int64        `json:"draft_id"`
}

// FileInfo describes a downloadable file.
type FileInfo struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"`
}

// APIError is a Bot API failure.
type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("telegram %s failed with code %d", e.Method, e.Code)
	}
	return fmt.Sprintf("telegram %s: %s", e.Method, e.Description)
}

// Client is a minimal Telegram Bot API client.
type Client struct {
	token   string
	baseURL string
	http    *http.Client
	log     *slog.Logger
}

// NewClient builds a client using a proxy-aware HTTP client.
func NewClient(token, baseURL string, hc *http.Client, log *slog.Logger) *Client {
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{token: token, baseURL: strings.TrimRight(baseURL, "/"), http: hc, log: log}
}

func (c *Client) methodURL(method string) string {
	return c.baseURL + "/bot" + c.token + "/" + method
}

// call executes a JSON Bot API method.
func (c *Client) call(ctx context.Context, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, method, result)
}

func (c *Client) do(req *http.Request, method string, result any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		// Network errors embed the URL, which contains the bot token.
		return fmt.Errorf("telegram %s: %s", method, logx.Redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: read response: %w", method, err)
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode >= 400 {
			return &APIError{Method: method, Code: resp.StatusCode, Description: strings.TrimSpace(string(raw))}
		}
		return fmt.Errorf("telegram %s: decode response: %w", method, err)
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		retry := time.Duration(env.Parameters.RetryAfter) * time.Second
		if retry == 0 {
			if h := req.Header.Get("Retry-After"); h != "" {
				if secs, err := strconv.Atoi(h); err == nil {
					retry = time.Duration(secs) * time.Second
				}
			}
		}
		return &APIError{Method: method, Code: code, Description: env.Description, RetryAfter: retry}
	}
	if result != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return fmt.Errorf("telegram %s: decode result: %w", method, err)
		}
	}
	return nil
}

// GetMe verifies the token.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", map[string]any{}, &u)
	return u, err
}

// GetUpdates long-polls for updates.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	payload := map[string]any{
		"offset":          offset,
		"limit":           100,
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query", "stopped_message_generation"},
	}
	var out []Update
	err := c.call(ctx, "getUpdates", payload, &out)
	return out, err
}

// SendMessage sends text with optional parse mode and keyboard.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text, parseMode string, keyboard any) (Message, error) {
	payload := map[string]any{
		"chat_id":              chatID,
		"text":                 text,
		"link_preview_options": map[string]any{"is_disabled": false},
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	var m Message
	err := c.call(ctx, "sendMessage", payload, &m)
	return m, err
}

// SendMessageDraft streams a partial message in a private chat. Reusing the
// same draft ID animates the update in place. The draft is ephemeral; callers
// must send the final message with SendMessage.
func (c *Client) SendMessageDraft(ctx context.Context, chatID, draftID int64, text, parseMode string, canStop bool) error {
	payload := map[string]any{
		"chat_id":  chatID,
		"draft_id": draftID,
		"text":     text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if canStop {
		payload["can_stop"] = true
	}
	return c.call(ctx, "sendMessageDraft", payload, nil)
}

// PinChatMessage pins a message. In private chats notifications are always
// disabled, so this is silent. The bot must be an administrator in groups.
func (c *Client) PinChatMessage(ctx context.Context, chatID int64, messageID int) error {
	return c.call(ctx, "pinChatMessage", map[string]any{
		"chat_id":              chatID,
		"message_id":           messageID,
		"disable_notification": true,
	}, nil)
}

// EditMessageText edits an existing message.
func (c *Client) EditMessageText(ctx context.Context, chatID int64, messageID int, text, parseMode string, keyboard any) error {
	payload := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	return c.call(ctx, "editMessageText", payload, nil)
}

// DeleteMessage removes a message (used for progress cleanup).
func (c *Client) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	return c.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil)
}

// SendChatAction shows typing/upload indicators.
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, nil)
}

// AnswerCallbackQuery acknowledges a button press.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	payload := map[string]any{"callback_query_id": id}
	if text != "" {
		payload["text"] = text
	}
	return c.call(ctx, "answerCallbackQuery", payload, nil)
}

// SetMyCommands publishes the command menu.
func (c *Client) SetMyCommands(ctx context.Context, commands []map[string]string) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": commands}, nil)
}

// SetWebhook configures webhook mode.
func (c *Client) SetWebhook(ctx context.Context, webhookURL, secret string) error {
	payload := map[string]any{"url": webhookURL, "allowed_updates": []string{"message", "callback_query", "stopped_message_generation"}}
	if secret != "" {
		payload["secret_token"] = secret
	}
	return c.call(ctx, "setWebhook", payload, nil)
}

// DeleteWebhook removes a webhook.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	return c.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// GetFile resolves a file ID.
func (c *Client) GetFile(ctx context.Context, fileID string) (FileInfo, error) {
	var info FileInfo
	err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &info)
	return info, err
}

// DownloadFile downloads a Telegram file with a size cap.
func (c *Client) DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, string, error) {
	info, err := c.GetFile(ctx, fileID)
	if err != nil {
		return nil, "", err
	}
	if info.FilePath == "" {
		return nil, "", errors.New("telegram file has no path")
	}
	u := c.baseURL + "/file/bot" + c.token + "/" + strings.TrimPrefix(info.FilePath, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("telegram download: %s", logx.Redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("telegram download: HTTP %d", resp.StatusCode)
	}
	if maxBytes <= 0 {
		maxBytes = 20 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("telegram file exceeds %d bytes", maxBytes)
	}
	return data, info.FilePath, nil
}

// SendDocument uploads a local file.
func (c *Client) SendDocument(ctx context.Context, chatID int64, path, caption string) (Message, error) {
	return c.uploadFile(ctx, "sendDocument", "document", chatID, path, caption)
}

// SendPhoto uploads a photo.
func (c *Client) SendPhoto(ctx context.Context, chatID int64, path, caption string) (Message, error) {
	return c.uploadFile(ctx, "sendPhoto", "photo", chatID, path, caption)
}

func (c *Client) uploadFile(ctx context.Context, method, field string, chatID int64, path, caption string) (Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return Message{}, err
	}
	defer f.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		_ = mw.WriteField("caption", truncateRunes(caption, 1000))
	}
	part, err := mw.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return Message{}, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return Message{}, err
	}
	if err := mw.Close(); err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), &buf)
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var m Message
	if err := c.do(req, method, &m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// FileURL builds a download URL for tests and diagnostics (redacted by callers).
func (c *Client) FileURL(path string) string {
	u, _ := url.JoinPath(c.baseURL, "file", "bot"+c.token, path)
	return u
}

// RetryAfter returns the 429 delay from err.
func RetryAfter(err error) (time.Duration, bool) {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return ae.RetryAfter, true
	}
	return 0, false
}

// IsParseError reports a Telegram entity parsing failure so callers can fall
// back to plain text.
func IsParseError(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	d := strings.ToLower(ae.Description)
	return ae.Code == 400 && (strings.Contains(d, "parse entities") || strings.Contains(d, "can't parse") || strings.Contains(d, "unsupported start tag") || strings.Contains(d, "unclosed"))
}

// IsMessageNotFound reports an edit/delete of a message that no longer exists.
func IsMessageNotFound(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	d := strings.ToLower(ae.Description)
	return ae.Code == 400 && (strings.Contains(d, "message to edit not found") ||
		strings.Contains(d, "message to delete not found") ||
		strings.Contains(d, "message can't be deleted") ||
		strings.Contains(d, "message_id_invalid"))
}

// IsNotModified reports Telegram's benign "nothing changed" edit response.
func IsNotModified(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Code == 400 && strings.Contains(strings.ToLower(ae.Description), "message is not modified")
}

// IsTooLong reports a message exceeding Telegram's size limit.
func IsTooLong(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	d := strings.ToLower(ae.Description)
	return ae.Code == 400 && (strings.Contains(d, "message is too long") || strings.Contains(d, "entities too long"))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
