// Package netx builds proxy-aware HTTP transports for every component.
//
// Proxy resolution precedence is applied by the caller (config.ResolveProxy):
// provider > component > global > standard environment. The special value
// "none" disables proxying. HTTP, HTTPS and SOCKS5 proxies (with optional
// credentials) are supported; Go's http.Transport handles all three schemes.
package netx

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/bitvibe/kestrel/internal/logx"
)

// Chain is the ordered proxy override chain for one component.
type Chain struct {
	Global    string
	Component string
	Provider  string
}

// Resolve returns the most specific non-empty value.
func (c Chain) Resolve() string {
	for _, v := range []string{c.Provider, c.Component, c.Global} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// IsNone reports whether the spec explicitly disables proxying.
func IsNone(spec string) bool {
	switch strings.ToLower(strings.TrimSpace(spec)) {
	case "none", "off", "direct", "disable":
		return true
	}
	return false
}

// Redact masks credentials in a proxy URL for logs.
func Redact(spec string) string {
	if spec == "" {
		return "env"
	}
	if IsNone(spec) {
		return "none"
	}
	return logx.Redact(spec)
}

// Config tunes transports.
type Config struct {
	UserAgent       string
	MaxIdleConns    int
	IdleConnTimeout time.Duration
	TLSHandshake    time.Duration
	ConnectTimeout  time.Duration
	DisableHTTP2    bool
	Env             func(string) string
}

// Factory caches one transport per proxy specification so connections are
// reused across calls from the same component.
type Factory struct {
	cfg Config

	mu         sync.Mutex
	transports map[string]*http.Transport
}

// NewFactory returns a transport factory.
func NewFactory(cfg Config) *Factory {
	if cfg.Env == nil {
		cfg.Env = os.Getenv
	}
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = 16
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = 90 * time.Second
	}
	if cfg.TLSHandshake <= 0 {
		cfg.TLSHandshake = 10 * time.Second
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	return &Factory{cfg: cfg, transports: make(map[string]*http.Transport)}
}

// Transport returns a cached transport for the proxy spec.
func (f *Factory) Transport(spec string) (*http.Transport, error) {
	spec = strings.TrimSpace(spec)
	f.mu.Lock()
	defer f.mu.Unlock()
	if tr, ok := f.transports[spec]; ok {
		return tr, nil
	}
	tr, err := f.build(spec)
	if err != nil {
		return nil, err
	}
	f.transports[spec] = tr
	return tr, nil
}

func (f *Factory) build(spec string) (*http.Transport, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConns = f.cfg.MaxIdleConns
	base.MaxIdleConnsPerHost = max(2, f.cfg.MaxIdleConns/2)
	base.IdleConnTimeout = f.cfg.IdleConnTimeout
	base.TLSHandshakeTimeout = f.cfg.TLSHandshake
	base.ExpectContinueTimeout = time.Second
	base.ForceAttemptHTTP2 = !f.cfg.DisableHTTP2
	base.DialContext = (&net.Dialer{
		Timeout:   f.cfg.ConnectTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	if base.TLSClientConfig == nil {
		base.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	switch {
	case spec == "":
		base.Proxy = envProxyFunc(f.cfg.Env)
	case IsNone(spec):
		base.Proxy = nil
	default:
		u, err := url.Parse(spec)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy %q: %w", Redact(spec), err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q (use http, https, socks5 or socks5h)", u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("invalid proxy %q: missing host", Redact(spec))
		}
		base.Proxy = http.ProxyURL(u)
	}
	return base, nil
}

// Client returns an HTTP client using the cached transport. A zero timeout
// means no client-level timeout; callers should always pass a context.
func (f *Factory) Client(spec string, timeout time.Duration) (*http.Client, error) {
	tr, err := f.Transport(spec)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

// Do performs a request with the User-Agent default applied.
func (f *Factory) Do(req *http.Request) (*http.Response, error) {
	spec, _ := ProxySpecFromContext(req.Context())
	client, err := f.Client(spec, 0)
	if err != nil {
		return nil, err
	}
	if f.cfg.UserAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", f.cfg.UserAgent)
	}
	return client.Do(req)
}

// CloseIdle closes pooled connections. Useful in tests and on shutdown.
func (f *Factory) CloseIdle() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tr := range f.transports {
		tr.CloseIdleConnections()
	}
}

type proxyCtxKey struct{}

// WithProxySpec attaches the proxy spec to a context for Do.
func WithProxySpec(ctx context.Context, spec string) context.Context {
	return context.WithValue(ctx, proxyCtxKey{}, spec)
}

// ProxySpecFromContext extracts a proxy spec previously stored.
func ProxySpecFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(proxyCtxKey{}).(string)
	return v, ok
}

func envProxyFunc(getenv func(string) string) func(*http.Request) (*url.URL, error) {
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(getenv(k)); v != "" {
				return v
			}
		}
		return ""
	}
	noProxy := first("NO_PROXY", "no_proxy")
	std := (&httpproxy.Config{
		HTTPProxy:  first("HTTP_PROXY", "http_proxy"),
		HTTPSProxy: first("HTTPS_PROXY", "https_proxy"),
		NoProxy:    noProxy,
	}).ProxyFunc()
	all := (&httpproxy.Config{
		HTTPProxy:  first("ALL_PROXY", "all_proxy"),
		HTTPSProxy: first("ALL_PROXY", "all_proxy"),
		NoProxy:    noProxy,
	}).ProxyFunc()
	return func(req *http.Request) (*url.URL, error) {
		if u, err := std(req.URL); u != nil || err != nil {
			return u, err
		}
		return all(req.URL)
	}
}

// SplitEnvList is used for NO_PROXY merging in subprocess environments.
func SplitEnvList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SubprocessEnv returns environment entries to add to a child process so that
// curl, git, pip, npm and friends use the same proxy as the agent. A nil
// return means "inherit the parent environment unchanged".
func SubprocessEnv(spec string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	if IsNone(spec) {
		return []string{
			"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
			"http_proxy=", "https_proxy=", "all_proxy=",
			"NO_PROXY=*", "no_proxy=*",
		}
	}
	noProxy := ""
	if v := os.Getenv("NO_PROXY"); v != "" {
		noProxy = v
	} else if v := os.Getenv("no_proxy"); v != "" {
		noProxy = v
	}
	env := []string{
		"HTTP_PROXY=" + spec, "HTTPS_PROXY=" + spec, "ALL_PROXY=" + spec,
		"http_proxy=" + spec, "https_proxy=" + spec, "all_proxy=" + spec,
	}
	if noProxy != "" {
		env = append(env, "NO_PROXY="+noProxy, "no_proxy="+noProxy)
	}
	return env
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
