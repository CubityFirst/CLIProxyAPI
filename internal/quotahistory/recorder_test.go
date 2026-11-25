package quotahistory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var testNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func claudeAuth(id string, observed time.Time, weekly, short string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "claude",
		Quota: coreauth.QuotaState{
			ObservedAt: observed,
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization": weekly,
				"Anthropic-Ratelimit-Unified-5h-Utilization": short,
				"Anthropic-Ratelimit-Unified-7d-Reset":       "1795000000",
				"Anthropic-Ratelimit-Unified-5h-Reset":       "1794500000",
			},
		},
	}
}

func TestRecordOnlyNewObservationsAndBuckets(t *testing.T) {
	r := New("")
	auth := claudeAuth("a", testNow, "0.25", "0.5")
	r.Record(testNow, []*coreauth.Auth{auth})
	// Same observation again: no new sample.
	r.Record(testNow.Add(time.Minute), []*coreauth.Auth{auth})
	// Newer observation in the same 5 minute bucket replaces the sample.
	r.Record(testNow.Add(2*time.Minute), []*coreauth.Auth{claudeAuth("a", testNow.Add(time.Minute), "0.3", "0.6")})
	// Observation in a later bucket appends.
	r.Record(testNow.Add(10*time.Minute), []*coreauth.Auth{claudeAuth("a", testNow.Add(10*time.Minute), "0.4", "0.7")})

	series := r.Snapshot(testNow.Add(-time.Hour))
	if len(series) != 1 || len(series[0].Samples) != 2 {
		t.Fatalf("snapshot = %+v, want one series with 2 samples", series)
	}
	first, second := series[0].Samples[0], series[0].Samples[1]
	if first.WeeklyUsed != 30 || first.ShortUsed != 60 {
		t.Fatalf("bucketed sample = %+v, want 30/60", first)
	}
	if second.WeeklyUsed != 40 || second.WeeklyReset != 1795000000 || second.ShortReset != 1794500000 {
		t.Fatalf("second sample = %+v", second)
	}
}

func TestRecordIgnoresUnsupportedProvidersAndDropsExpired(t *testing.T) {
	r := New("")
	gemini := &coreauth.Auth{ID: "g", Provider: "gemini", Quota: coreauth.QuotaState{ObservedAt: testNow, Signals: map[string]string{"Retry-After": "1"}}}
	old := claudeAuth("old", testNow.Add(-30*24*time.Hour), "0.1", "0.1")
	r.Record(testNow, []*coreauth.Auth{gemini, old})
	if got := r.Snapshot(time.Time{}); len(got) != 0 {
		t.Fatalf("snapshot = %+v, want empty", got)
	}
}

func TestCodexSignalsUsePercent(t *testing.T) {
	r := New("")
	auth := &coreauth.Auth{ID: "c", Provider: "codex", Quota: coreauth.QuotaState{
		ObservedAt: testNow,
		Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":   "12.5",
			"X-Codex-Secondary-Used-Percent": "40",
			"X-Codex-Secondary-Reset-At":     "1795000000",
		},
	}}
	r.Record(testNow, []*coreauth.Auth{auth})
	got := r.Snapshot(time.Time{})
	if len(got) != 1 || got[0].Samples[0].WeeklyUsed != 40 || got[0].Samples[0].ShortUsed != 12.5 {
		t.Fatalf("codex snapshot = %+v", got)
	}
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	r.Record(testNow, []*coreauth.Auth{claudeAuth("a", testNow, "0.5", "0.1")})
	if err := r.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("history file missing: %v", err)
	}
	reloaded := New(dir)
	got := reloaded.Snapshot(time.Time{})
	if len(got) != 1 || len(got[0].Samples) != 1 || got[0].Samples[0].WeeklyUsed != 50 {
		t.Fatalf("reloaded snapshot = %+v", got)
	}
	// The reloaded series must not re-record the already stored observation.
	reloaded.Record(testNow.Add(time.Minute), []*coreauth.Auth{claudeAuth("a", testNow, "0.9", "0.1")})
	if got := reloaded.Snapshot(time.Time{}); got[0].Samples[0].WeeklyUsed != 50 {
		t.Fatalf("stale observation re-recorded: %+v", got)
	}
}
