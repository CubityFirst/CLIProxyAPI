package auth

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	claudeWeeklyResetSignal = "Anthropic-Ratelimit-Unified-7d-Reset"
	claudeShortResetSignal  = "Anthropic-Ratelimit-Unified-5h-Reset"
	claudeWeeklyWindow      = 7 * 24 * time.Hour
)

// SoonestResetSelector drains the credential whose weekly quota window resets
// soonest. Quota that is about to reset is "use it or lose it", so spending it
// first keeps the credentials with the most distant reset in reserve.
//
// Ordering among available credentials of the highest priority tier:
//  1. OAuth credentials that have never reported quota headers, so a single
//     request learns their window.
//  2. OAuth credentials ordered by weekly reset (earliest first), then by
//     short-window reset, then by the original candidate order.
//  3. All remaining credentials (API keys, providers without reset signals),
//     in the original candidate order.
//
// Exhausted credentials are already excluded by the cooldown logic in
// getSelectorAvailableAuths, so the selector moves on to the next credential as
// soon as the current one is rate limited.
type SoonestResetSelector struct {
	// nowFunc overrides the clock in tests.
	nowFunc func() time.Time

	// lastWeekly remembers the last observed weekly reset per credential, so a
	// response that replaces the quota snapshot without unified headers (for
	// example a bare Retry-After) does not drop the credential out of order.
	mu         sync.Mutex
	lastWeekly map[string]time.Time
}

func (s *SoonestResetSelector) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// Pick selects the available credential with the earliest weekly quota reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastWeekly == nil {
		s.lastWeekly = make(map[string]time.Time)
	}
	ranked := rankAuthsBySoonestReset(available, now, s.lastWeekly)
	return ranked[0], nil
}

// soonestResetRank is the sort key computed for one credential.
type soonestResetRank struct {
	tier   int
	weekly time.Time
	short  time.Time
}

const (
	soonestResetTierUnobserved = iota
	soonestResetTierKnown
	soonestResetTierOther
)

// RankAuthsBySoonestReset returns a copy of auths in SoonestResetSelector order.
func RankAuthsBySoonestReset(auths []*Auth, now time.Time) []*Auth {
	return rankAuthsBySoonestReset(auths, now, nil)
}

// rankAuthsBySoonestReset ranks auths, reading and updating the optional map of
// remembered weekly resets keyed by auth ID.
func rankAuthsBySoonestReset(auths []*Auth, now time.Time, remembered map[string]time.Time) []*Auth {
	ranked := make([]*Auth, len(auths))
	copy(ranked, auths)
	keys := make(map[*Auth]soonestResetRank, len(ranked))
	for _, auth := range ranked {
		keys[auth] = soonestResetRankFor(auth, now, remembered)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := keys[ranked[i]], keys[ranked[j]]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.tier != soonestResetTierKnown {
			return false
		}
		if !a.weekly.Equal(b.weekly) {
			return a.weekly.Before(b.weekly)
		}
		return timeBeforeZeroLast(a.short, b.short)
	})
	return ranked
}

func soonestResetRankFor(auth *Auth, now time.Time, remembered map[string]time.Time) soonestResetRank {
	if auth == nil || auth.AuthKind() != AuthKindOAuth {
		return soonestResetRank{tier: soonestResetTierOther}
	}
	weekly, short, ok := QuotaResetTimes(auth, now)
	if remembered != nil {
		if ok {
			remembered[auth.ID] = weekly
		} else if last, found := remembered[auth.ID]; found {
			weekly, ok = rollWeeklyReset(last, now), true
		}
	}
	if !ok {
		if auth.Quota.ObservedAt.IsZero() {
			return soonestResetRank{tier: soonestResetTierUnobserved}
		}
		// Observed, but the upstream never sent a weekly reset (for example a
		// provider without unified quota headers). Treat it like an API key.
		return soonestResetRank{tier: soonestResetTierOther}
	}
	return soonestResetRank{tier: soonestResetTierKnown, weekly: weekly, short: short}
}

// QuotaResetTimes returns the next weekly and short-window reset of auth as
// reported by the most recent upstream response. A weekly reset that already
// passed is rolled forward by whole weeks, because the window restarts on the
// same schedule. A short-window reset that already passed is dropped since that
// window only restarts on the next request. ok is false when no weekly reset was
// observed.
func QuotaResetTimes(auth *Auth, now time.Time) (weekly, short time.Time, ok bool) {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return time.Time{}, time.Time{}, false
	}
	weekly, ok = parseResetSignal(auth.Quota.Signals, claudeWeeklyResetSignal)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	weekly = rollWeeklyReset(weekly, now)
	if parsed, okShort := parseResetSignal(auth.Quota.Signals, claudeShortResetSignal); okShort && parsed.After(now) {
		short = parsed
	}
	return weekly, short, true
}

// rollWeeklyReset advances a past weekly reset by whole weeks until it lies in
// the future.
func rollWeeklyReset(reset, now time.Time) time.Time {
	if reset.After(now) {
		return reset
	}
	periods := now.Sub(reset)/claudeWeeklyWindow + 1
	return reset.Add(periods * claudeWeeklyWindow)
}

func parseResetSignal(signals map[string]string, name string) (time.Time, bool) {
	raw, ok := signals[http.CanonicalHeaderKey(name)]
	if !ok {
		for key, val := range signals {
			if strings.EqualFold(key, name) {
				raw, ok = val, true
				break
			}
		}
	}
	if !ok {
		return time.Time{}, false
	}
	raw = strings.TrimSpace(raw)
	if sec, err := strconv.ParseInt(raw, 10, 64); err == nil && sec > 0 {
		return time.Unix(sec, 0), true
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed, true
	}
	return time.Time{}, false
}

// timeBeforeZeroLast orders times ascending with zero values last.
func timeBeforeZeroLast(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return !a.IsZero() && b.IsZero()
	}
	return a.Before(b)
}
