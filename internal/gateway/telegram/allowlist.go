package telegram

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitvibe/kestrel/internal/store"
)

// ErrPairingDisabled is returned when no pairing code is configured.
var ErrPairingDisabled = errors.New("pairing is disabled")

// ErrPairingInvalid is returned for a wrong pairing code.
var ErrPairingInvalid = errors.New("invalid pairing code")

// Allowlist is the mandatory access-control list. Additional users can pair
// themselves with a configured one-time code; successful pairings are stored
// in SQLite so they survive restarts.
type Allowlist struct {
	mu          sync.RWMutex
	allowed     map[int64]struct{}
	pairingCode string
	store       *store.Store
	log         *slog.Logger
}

// NewAllowlist builds an allowlist from configuration.
func NewAllowlist(userIDs []int64, pairingCode string, st *store.Store, log *slog.Logger) *Allowlist {
	if log == nil {
		log = slog.Default()
	}
	a := &Allowlist{
		allowed:     make(map[int64]struct{}, len(userIDs)),
		pairingCode: strings.TrimSpace(pairingCode),
		store:       st,
		log:         log,
	}
	for _, id := range userIDs {
		if id != 0 {
			a.allowed[id] = struct{}{}
		}
	}
	return a
}

// Load restores paired users from the database.
func (a *Allowlist) Load(ctx context.Context) {
	if a.store == nil || a.pairingCode == "" {
		return
	}
	kv, err := a.store.ListKV(ctx, "allow_user:")
	if err != nil {
		a.log.Warn("load paired users", "error", err)
		return
	}
	a.mu.Lock()
	for key := range kv {
		id, err := strconv.ParseInt(strings.TrimPrefix(key, "allow_user:"), 10, 64)
		if err == nil && id != 0 {
			a.allowed[id] = struct{}{}
		}
	}
	n := len(a.allowed)
	a.mu.Unlock()
	if len(kv) > 0 {
		a.log.Info("loaded paired users", "count", n)
	}
}

// Allowed reports whether a Telegram user may use the bot.
func (a *Allowlist) Allowed(userID int64) bool {
	a.mu.RLock()
	_, ok := a.allowed[userID]
	a.mu.RUnlock()
	return ok
}

// IDs returns a sorted snapshot for diagnostics.
func (a *Allowlist) IDs() []int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]int64, 0, len(a.allowed))
	for id := range a.allowed {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Pair verifies the pairing code and, on success, grants access and persists
// the user.
func (a *Allowlist) Pair(ctx context.Context, userID int64, code string) error {
	if a.pairingCode == "" {
		return ErrPairingDisabled
	}
	given := []byte(strings.TrimSpace(code))
	want := []byte(a.pairingCode)
	if subtle.ConstantTimeCompare(given, want) != 1 {
		return ErrPairingInvalid
	}
	if a.store != nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := a.store.SetKV(ctx, fmt.Sprintf("allow_user:%d", userID), "1"); err != nil {
			a.log.Warn("persist paired user", "user_id", userID, "error", err)
			return fmt.Errorf("pairing could not be persisted: %w", err)
		}
	}
	a.mu.Lock()
	a.allowed[userID] = struct{}{}
	a.mu.Unlock()
	a.log.Info("user paired", "user_id", userID)
	return nil
}
