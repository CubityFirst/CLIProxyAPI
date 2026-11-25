package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var soonestResetTestNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func soonestResetOAuth(id string, weekly, short time.Time) *Auth {
	auth := &Auth{
		ID:         id,
		Provider:   "claude",
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth},
	}
	signals := map[string]string{}
	if !weekly.IsZero() {
		signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(weekly.Unix(), 10)
	}
	if !short.IsZero() {
		signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(short.Unix(), 10)
	}
	if len(signals) > 0 {
		auth.Quota.Signals = signals
		auth.Quota.ObservedAt = soonestResetTestNow.Add(-time.Minute)
	}
	return auth
}

func soonestResetIDs(auths []*Auth) []string {
	ids := make([]string, len(auths))
	for i, auth := range auths {
		ids[i] = auth.ID
	}
	return ids
}

func assertSoonestResetOrder(t *testing.T, got []*Auth, want ...string) {
	t.Helper()
	ids := soonestResetIDs(got)
	if len(ids) != len(want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order = %v, want %v", ids, want)
		}
	}
}

func TestRankAuthsBySoonestReset_OrdersByWeeklyReset(t *testing.T) {
	now := soonestResetTestNow
	auths := []*Auth{
		soonestResetOAuth("late", now.Add(6*24*time.Hour), time.Time{}),
		soonestResetOAuth("soon", now.Add(20*time.Hour), time.Time{}),
		soonestResetOAuth("mid", now.Add(3*24*time.Hour), time.Time{}),
	}
	assertSoonestResetOrder(t, RankAuthsBySoonestReset(auths, now), "soon", "mid", "late")
}

func TestRankAuthsBySoonestReset_TiebreaksOnShortWindow(t *testing.T) {
	now := soonestResetTestNow
	weekly := now.Add(2 * 24 * time.Hour)
	auths := []*Auth{
		soonestResetOAuth("no-short", weekly, time.Time{}),
		soonestResetOAuth("short-late", weekly, now.Add(4*time.Hour)),
		soonestResetOAuth("short-soon", weekly, now.Add(time.Hour)),
	}
	assertSoonestResetOrder(t, RankAuthsBySoonestReset(auths, now), "short-soon", "short-late", "no-short")
}

func TestRankAuthsBySoonestReset_TiersUnobservedKnownOther(t *testing.T) {
	now := soonestResetTestNow
	apiKey := &Auth{ID: "api-key", Provider: "claude", Attributes: map[string]string{AttributeAPIKey: "sk-test"}}
	observedNoWeekly := soonestResetOAuth("observed-no-weekly", time.Time{}, time.Time{})
	observedNoWeekly.Quota.Signals = map[string]string{"Retry-After": "30"}
	observedNoWeekly.Quota.ObservedAt = now.Add(-time.Minute)
	auths := []*Auth{
		apiKey,
		soonestResetOAuth("known", now.Add(24*time.Hour), time.Time{}),
		observedNoWeekly,
		soonestResetOAuth("fresh", time.Time{}, time.Time{}),
	}
	assertSoonestResetOrder(t, RankAuthsBySoonestReset(auths, now), "fresh", "known", "api-key", "observed-no-weekly")
}

func TestRankAuthsBySoonestReset_RollsPastWeeklyResetForward(t *testing.T) {
	now := soonestResetTestNow
	// Reset 1h ago means the next reset is in 7d-1h, later than a reset in 2 days.
	auths := []*Auth{
		soonestResetOAuth("rolled", now.Add(-time.Hour), time.Time{}),
		soonestResetOAuth("two-days", now.Add(2*24*time.Hour), time.Time{}),
	}
	assertSoonestResetOrder(t, RankAuthsBySoonestReset(auths, now), "two-days", "rolled")

	weekly, _, ok := QuotaResetTimes(auths[0], now)
	if !ok || !weekly.Equal(now.Add(7*24*time.Hour-time.Hour)) {
		t.Fatalf("rolled weekly reset = %v ok=%v", weekly, ok)
	}
}

func TestSoonestResetSelector_RemembersWeeklyResetWhenSnapshotLosesIt(t *testing.T) {
	now := soonestResetTestNow
	selector := &SoonestResetSelector{nowFunc: func() time.Time { return now }}
	soon := soonestResetOAuth("soon", now.Add(10*time.Hour), time.Time{})
	late := soonestResetOAuth("late", now.Add(5*24*time.Hour), time.Time{})
	auths := []*Auth{late, soon}

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "soon" {
		t.Fatalf("first pick = %v, %v; want soon", got, err)
	}

	// A later response replaced the snapshot with only Retry-After.
	soon.Quota.Signals = map[string]string{"Retry-After": "5"}
	got, err = selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil || got.ID != "soon" {
		t.Fatalf("second pick = %v, %v; want soon (remembered)", got, err)
	}
}

func TestSoonestResetSelector_SkipsCooledDownCredential(t *testing.T) {
	now := soonestResetTestNow
	selector := &SoonestResetSelector{nowFunc: func() time.Time { return now }}
	soon := soonestResetOAuth("soon", now.Add(10*time.Hour), time.Time{})
	soon.Unavailable = true
	soon.Quota.Exceeded = true
	soon.Quota.NextRecoverAt = now.Add(2 * time.Hour)
	soon.NextRetryAfter = now.Add(2 * time.Hour)
	late := soonestResetOAuth("late", now.Add(5*24*time.Hour), time.Time{})

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{soon, late})
	if err != nil {
		t.Fatalf("pick error: %v", err)
	}
	if got.ID != "late" {
		t.Fatalf("pick = %s, want late while soon is cooling down", got.ID)
	}
}

func TestManagerSelectAuth_SoonestResetDrainsSoonestCredential(t *testing.T) {
	const model = "claude-soonest-reset-test-model"
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude"})

	now := time.Now()
	auths := []*Auth{
		soonestResetOAuth("claude-late", now.Add(6*24*time.Hour), time.Time{}),
		soonestResetOAuth("claude-soon", now.Add(30*time.Hour), time.Time{}),
		soonestResetOAuth("claude-mid", now.Add(3*24*time.Hour), time.Time{}),
	}
	reg := registry.GetGlobalRegistry()
	for _, auth := range auths {
		auth.Status = StatusActive
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
		reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
		authID := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(authID) })
		manager.RefreshSchedulerEntry(auth.ID)
	}

	for i := 0; i < 3; i++ {
		selected, errSelect := manager.SelectAuth(context.Background(), "claude", model, cliproxyexecutor.Options{})
		if errSelect != nil {
			t.Fatalf("SelectAuth() error = %v", errSelect)
		}
		if selected.ID != "claude-soon" {
			t.Fatalf("SelectAuth() = %s, want claude-soon", selected.ID)
		}
	}
}
