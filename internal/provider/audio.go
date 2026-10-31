package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/bitvibe/kestrel/internal/logx"
)

// Transcriber calls an OpenAI-compatible audio transcription endpoint.
type Transcriber struct {
	BaseURL  string
	APIKey   string
	Model    string
	Language string
	Path     string
	Headers  map[string]string
	HTTP     *http.Client
	Timeout  time.Duration
}

// Transcribe sends audio bytes and returns the transcript text.
func (t *Transcriber) Transcribe(ctx context.Context, data []byte, filename, mime string) (string, error) {
	if t == nil {
		return "", fmt.Errorf("transcription is not configured")
	}
	if t.BaseURL == "" {
		return "", fmt.Errorf("transcription base URL is not configured")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("audio file is empty")
	}
	if t.Model == "" {
		t.Model = "whisper-1"
	}
	path := t.Path
	if path == "" {
		path = "/audio/transcriptions"
	}
	if filename == "" {
		filename = "audio.ogg"
	}
	filename = sanitizeUploadName(filename)
	if mime == "" {
		mime = "application/octet-stream"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	header.Set("Content-Type", mime)
	part, err := mw.CreatePart(header)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.WriteField("model", t.Model); err != nil {
		return "", err
	}
	if t.Language != "" {
		if err := mw.WriteField("language", t.Language); err != nil {
			return "", err
		}
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	endpoint := strings.TrimRight(t.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	actx := ctx
	if t.Timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(actx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	if t.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.APIKey)
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	hc := t.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcription request: %s", logx.Redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("transcription response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
		return "", fmt.Errorf("transcription HTTP %d: %s", resp.StatusCode, logx.Redact(msg))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// Some servers ignore response_format and return plain text.
		text := strings.TrimSpace(string(raw))
		if text != "" && !strings.HasPrefix(text, "{") {
			return text, nil
		}
		return "", fmt.Errorf("decode transcription response: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

func sanitizeUploadName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_', r == ' ', r == '(', r == ')':
			b.WriteRune(r)
		}
		if b.Len() >= 120 {
			break
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return "audio.ogg"
	}
	return out
}
