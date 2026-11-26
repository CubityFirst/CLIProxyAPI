package auth

import (
	"strconv"
	"strings"
	"time"
)

const (
	claudeWeeklyUtilizationSignal = "Anthropic-Ratelimit-Unified-7d-Utilization"
	claudeShortUtilizationSignal  = "Anthropic-Ratelimit-Unified-5h-Utilization"

	// expiringQuotaMaxUtilization skips nearly spent candidates: a session moved
	// there would hit the limit almost at once and pay a second cache rewrite on
	// the way back.
	expiringQuotaMaxUtilization = 0.9
)

// ParseExpiringWindow parses routing.session-affinity-expiring-window. Empty,
// invalid, and non-positive values disable the feature and return 0.
func ParseExpiringWindow(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	parsed, errParse := time.ParseDuration(raw)
	if errParse != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

// expiringQuotaTarget returns the credential a bound session should move to so
// weekly quota that is about to reset is spent before it expires, or nil to keep
// the binding. A candidate qualifies when its weekly quota resets within window
// and before the bound credential's, and it has headroom left in both its
// weekly and 5-hour windows. The earliest weekly reset wins.
func expiringQuotaTarget(candidates []*Auth, bound *Auth, now time.Time, window time.Duration) *Auth {
	if window <= 0 || bound == nil {
		return nil
	}
	boundWeekly, _, boundKnown := QuotaResetTimes(bound, now)
	var target *Auth
	var targetWeekly time.Time
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID == bound.ID || candidate.AuthKind() != AuthKindOAuth {
			continue
		}
		weekly, _, ok := QuotaResetTimes(candidate, now)
		if !ok || weekly.Sub(now) > window {
			continue
		}
		if boundKnown && !weekly.Before(boundWeekly) {
			continue
		}
		if quotaUtilization(candidate, claudeWeeklyResetSignal, claudeWeeklyUtilizationSignal, now) >= expiringQuotaMaxUtilization ||
			quotaUtilization(candidate, claudeShortResetSignal, claudeShortUtilizationSignal, now) >= expiringQuotaMaxUtilization {
			continue
		}
		if target == nil || weekly.Before(targetWeekly) {
			target, targetWeekly = candidate, weekly
		}
	}
	return target
}

// quotaUtilization returns the last reported utilization (0-1) of one quota
// window, or 0 when the window has reset since it was reported.
func quotaUtilization(auth *Auth, resetSignal, utilizationSignal string, now time.Time) float64 {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return 0
	}
	if reset, ok := parseResetSignal(auth.Quota.Signals, resetSignal); !ok || !reset.After(now) {
		return 0
	}
	raw, ok := auth.Quota.Signals[quotaSignalKey(auth.Quota.Signals, utilizationSignal)]
	if !ok {
		return 0
	}
	value, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if errParse != nil {
		return 0
	}
	return value
}

// quotaSignalKey returns the key under which name is stored in signals,
// matching case-insensitively.
func quotaSignalKey(signals map[string]string, name string) string {
	if _, ok := signals[name]; ok {
		return name
	}
	for key := range signals {
		if strings.EqualFold(key, name) {
			return key
		}
	}
	return name
}
