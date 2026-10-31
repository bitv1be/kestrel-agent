package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// Registry holds named providers for runtime model switching.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	order     []string
}

// NewRegistry returns an empty provider registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Add registers a provider. Re-adding the same name replaces it.
func (r *Registry) Add(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[p.Name()]; !exists {
		r.order = append(r.order, p.Name())
	}
	r.providers[p.Name()] = p
}

// Get returns a provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Names returns provider names in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Chain tries the primary provider and then an ordered fallback list.
// Fallback happens only for retryable failures and only when no visible output
// has been streamed yet, to avoid duplicate messages.
type Chain struct {
	Primary   Provider
	Fallbacks []Provider
	Log       *slog.Logger
}

// NewChain builds a chain from a primary and optional fallbacks.
func NewChain(primary Provider, fallbacks []Provider, log *slog.Logger) *Chain {
	if log == nil {
		log = slog.Default()
	}
	return &Chain{Primary: primary, Fallbacks: fallbacks, Log: log}
}

// Providers returns the ordered attempt list.
func (c *Chain) Providers() []Provider {
	if c.Primary == nil {
		return c.Fallbacks
	}
	return append([]Provider{c.Primary}, c.Fallbacks...)
}

// Chat runs the request through the fallback chain.
func (c *Chain) Chat(ctx context.Context, req ChatRequest, onDelta func(string) error) (ChatResponse, error) {
	if c.Primary == nil {
		return ChatResponse{}, errors.New("no active provider configured")
	}
	var errs []error
	emitted := false
	for i, p := range c.Providers() {
		if ctx.Err() != nil {
			return ChatResponse{}, ctx.Err()
		}
		if i > 0 {
			// The selected model belongs to the primary; fallback providers
			// use their own default unless explicitly overridden.
			req.Model = p.DefaultModel()
			c.Log.Warn("falling back to provider", "provider", p.Name(), "model", req.Model)
		}
		delta := onDelta
		if delta != nil {
			delta = func(s string) error {
				emitted = true
				return onDelta(s)
			}
		}
		resp, err := p.Chat(ctx, req, delta)
		if err == nil {
			return resp, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
		if ctx.Err() != nil {
			return resp, ctx.Err()
		}
		if emitted || !IsRetryable(err) {
			return resp, err
		}
	}
	return ChatResponse{}, fmt.Errorf("all providers failed: %w", errors.Join(errs...))
}

// ParseSelection parses "provider:model" and resolves the provider. A bare
// provider name selects its default model.
func ParseSelection(reg *Registry, selection string) (Provider, string, error) {
	selection = strings.TrimSpace(selection)
	if selection == "" {
		return nil, "", errors.New("empty model selection")
	}
	name, model := selection, ""
	if i := strings.IndexByte(selection, ':'); i > 0 {
		name, model = selection[:i], selection[i+1:]
	}
	p, ok := reg.Get(name)
	if !ok {
		return nil, "", fmt.Errorf("unknown provider %q", name)
	}
	if model == "" {
		model = p.DefaultModel()
	}
	return p, model, nil
}
