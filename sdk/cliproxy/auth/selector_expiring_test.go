package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func expiringTestAuth(id string, weekly, short time.Time, weeklyUsed, shortUsed string) *Auth {
	auth := soonestResetOAuth(id, weekly, short)
	if weeklyUsed != "" {
		auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = weeklyUsed
	}
	if shortUsed != "" {
		auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = shortUsed
	}
	return auth
}

func TestParseExpiringWindow(t *testing.T) {
	cases := map[string]time.Duration{"": 0, "0": 0, "-1h": 0, "nope": 0, "6h": 6 * time.Hour, " 24h ": 24 * time.Hour}
	for raw, want := range cases {
		if got := ParseExpiringWindow(raw); got != want {
			t.Fatalf("ParseExpiringWindow(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestExpiringQuotaTarget(t *testing.T) {
	now := soonestResetTestNow
	day := 24 * time.Hour
	bound := expiringTestAuth("bound", now.Add(4*day), now.Add(2*time.Hour), "0.2", "0.4")
	soon := expiringTestAuth("soon", now.Add(3*time.Hour), time.Time{}, "0.64", "")
	sooner := expiringTestAuth("sooner", now.Add(time.Hour), time.Time{}, "0.5", "")
	far := expiringTestAuth("far", now.Add(2*day), time.Time{}, "0.1", "")
	weeklySpent := expiringTestAuth("weekly-spent", now.Add(time.Hour), time.Time{}, "0.95", "")
	shortSpent := expiringTestAuth("short-spent", now.Add(time.Hour), now.Add(30*time.Minute), "0.5", "0.92")
	shortReset := expiringTestAuth("short-reset", now.Add(2*time.Hour), now.Add(-time.Minute), "0.5", "1.0")
	apiKey := &Auth{ID: "api-key", Provider: "claude", Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey}}

	cases := []struct {
		name       string
		bound      *Auth
		candidates []*Auth
		window     time.Duration
		want       string
	}{
		{name: "disabled", bound: bound, candidates: []*Auth{bound, soon}, window: 0, want: ""},
		{name: "moves to expiring credential", bound: bound, candidates: []*Auth{bound, soon}, window: 24 * time.Hour, want: "soon"},
		{name: "earliest reset wins", bound: bound, candidates: []*Auth{soon, bound, sooner}, window: 24 * time.Hour, want: "sooner"},
		{name: "outside the window", bound: bound, candidates: []*Auth{bound, far}, window: 24 * time.Hour, want: ""},
		{name: "never moves to a later reset", bound: sooner, candidates: []*Auth{sooner, soon}, window: 24 * time.Hour, want: ""},
		{name: "skips nearly spent weekly", bound: bound, candidates: []*Auth{bound, weeklySpent}, window: 24 * time.Hour, want: ""},
		{name: "skips nearly spent 5-hour window", bound: bound, candidates: []*Auth{bound, shortSpent}, window: 24 * time.Hour, want: ""},
		{name: "5-hour window that already reset counts as fresh", bound: bound, candidates: []*Auth{bound, shortReset}, window: 24 * time.Hour, want: "short-reset"},
		{name: "bound without reset data", bound: apiKey, candidates: []*Auth{apiKey, soon}, window: 24 * time.Hour, want: "soon"},
		{name: "ignores non-OAuth candidates", bound: bound, candidates: []*Auth{bound, apiKey}, window: 24 * time.Hour, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expiringQuotaTarget(tc.candidates, tc.bound, now, tc.window)
			gotID := ""
			if got != nil {
				gotID = got.ID
			}
			if gotID != tc.want {
				t.Fatalf("target = %q, want %q", gotID, tc.want)
			}
		})
	}
}

func TestSessionAffinityMovesBoundSessionToExpiringQuota(t *testing.T) {
	now := time.Now()
	late := expiringTestAuth("late", now.Add(4*24*time.Hour), time.Time{}, "0.2", "0.5")
	expiring := expiringTestAuth("expiring", now.Add(20*time.Minute), time.Time{}, "0.64", "")
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"claude-session"}}}

	pick := func(selector *SessionAffinitySelector, auths ...*Auth) string {
		t.Helper()
		got, err := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, auths)
		if err != nil || got == nil {
			t.Fatalf("Pick() = %v, %v", got, err)
		}
		return got.ID
	}

	for _, tc := range []struct {
		window time.Duration
		want   string
	}{
		{window: 0, want: "late"},
		{window: 24 * time.Hour, want: "expiring"},
	} {
		selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
			Fallback:       &SoonestResetSelector{},
			ExpiringWindow: tc.window,
		})
		// The expiring credential is unavailable when the session starts.
		if got := pick(selector, late); got != "late" {
			t.Fatalf("window %v: first pick = %q, want late", tc.window, got)
		}
		// It recovers; the session either stays or moves, and then sticks.
		for i := 0; i < 3; i++ {
			if got := pick(selector, late, expiring); got != tc.want {
				t.Fatalf("window %v: pick %d = %q, want %q", tc.window, i, got, tc.want)
			}
		}
	}
}
