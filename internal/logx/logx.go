// Package logx provides structured logging with secret redaction.
package logx

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

var (
	// Patterns that commonly carry credentials. We redact broadly; false
	// positives are preferable to leaking a key into a log aggregator.
	reURLUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)([^/@\s:]+)(:[^/@\s]*)?@`)
	reBotToken    = regexp.MustCompile(`\b\d{6,12}:[A-Za-z0-9_-]{20,}\b`)
	reAPIKey      = regexp.MustCompile(`\b(?:sk|rk|pk|api|key|token)[-_][A-Za-z0-9_-]{8,}\b`)
	reBearer      = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/-]+=*`)
	reQueryKey    = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|key|token|access_token|password)=)[^&\s]+`)
	reAuthHeader  = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?)[^"'\s,}]+`)
)

// Redact removes or masks likely secrets from s. It is safe to call on any
// string and is used by the logging handler and by user-facing errors.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = reURLUserinfo.ReplaceAllString(s, "${1}***:***@")
	s = reBotToken.ReplaceAllString(s, "***BOT_TOKEN***")
	s = reBearer.ReplaceAllString(s, "${1}***")
	s = reAPIKey.ReplaceAllString(s, "***KEY***")
	s = reQueryKey.ReplaceAllString(s, "${1}***")
	s = reAuthHeader.ReplaceAllString(s, "${1}***")
	return s
}

// Close quietens the linter about unused imports on some Go versions.
var _ = strings.TrimSpace

type redactingHandler struct {
	next slog.Handler
}

func (h redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		rec.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, rec)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a)
	}
	return redactingHandler{next: h.next.WithAttrs(out)}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{next: h.next.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(Redact(a.Value.String()))
	case slog.KindGroup:
		g := a.Value.Group()
		ng := make([]any, 0, len(g))
		for _, ga := range g {
			ng = append(ng, redactAttr(ga))
		}
		a.Value = slog.GroupValue(g...)
	}
	return a
}

// New creates a logger writing structured logs to stderr.
func New(level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	var h slog.Handler
	opts := &slog.HandlerOptions{Level: lv}
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(redactingHandler{next: h})
}

// Attr returns a redacted string attribute.
func Attr(key, val string) slog.Attr { return slog.String(key, Redact(val)) }
