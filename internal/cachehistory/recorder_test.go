package cachehistory

import (
	"context"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var baseTime = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

func newTestRecorder(t *testing.T, dir string) (*Recorder, *time.Time) {
	t.Helper()
	now := baseTime
	r := New(dir)
	r.now = func() time.Time { return now }
	return r, &now
}

func claudeRecord(auth, sessionID string, at time.Time, read, write, input int64) coreusage.Record {
	return coreusage.Record{
		Provider:    "claude",
		AuthID:      auth,
		SessionID:   sessionID,
		Model:       "claude-opus-5-5",
		RequestedAt: at,
		Detail: coreusage.Detail{
			CacheReadTokens:     read,
			CacheCreationTokens: write,
			InputTokens:         input,
		},
	}
}

// totals sums one reason across every bucket of authID.
func totals(t *testing.T, r *Recorder, authID, reason string) Tally {
	t.Helper()
	series, _ := r.Snapshot(baseTime.Add(-time.Hour))
	var sum Tally
	for _, s := range series {
		if s.AuthID != authID {
			continue
		}
		for _, b := range s.Buckets {
			if tally := b.Reasons[reason]; tally != nil {
				sum.Requests += tally.Requests
				sum.Misses += tally.Misses
				sum.Read += tally.Read
				sum.Write += tally.Write
				sum.Input += tally.Input
				sum.Lost += tally.Lost
			}
		}
	}
	return sum
}

func TestSwitchToAnotherAccountCountsLostCache(t *testing.T) {
	r, _ := newTestRecorder(t, "")
	ctx := context.Background()
	r.HandleUsage(ctx, claudeRecord("a", "s1", baseTime, 0, 50_000, 10))
	r.HandleUsage(ctx, claudeRecord("a", "s1", baseTime.Add(time.Minute), 50_000, 2_000, 10))
	// Moved to b while a's cache is still alive: b only has the shared system prompt.
	r.HandleUsage(ctx, claudeRecord("b", "s1", baseTime.Add(2*time.Minute), 8_000, 46_000, 10))

	if got := totals(t, r, "a", ReasonNew); got.Requests != 1 || got.Lost != 0 || got.Misses != 0 {
		t.Fatalf("new = %+v, want one request without loss", got)
	}
	if got := totals(t, r, "a", ReasonContinued); got.Requests != 1 || got.Lost != 0 || got.Misses != 0 {
		t.Fatalf("continued = %+v, want a hit", got)
	}
	got := totals(t, r, "b", ReasonSwitched)
	// The previous request left 52,000 tokens in the cache; 8,000 were read back.
	if got.Requests != 1 || got.Misses != 1 || got.Lost != 44_000 {
		t.Fatalf("switched = %+v, want one miss losing 44000", got)
	}
}

func TestSwitchBackToWarmAccountIsNotAMiss(t *testing.T) {
	r, _ := newTestRecorder(t, "")
	ctx := context.Background()
	r.HandleUsage(ctx, claudeRecord("a", "s1", baseTime, 0, 40_000, 0))
	r.HandleUsage(ctx, claudeRecord("b", "s1", baseTime.Add(time.Minute), 0, 41_000, 0))
	r.HandleUsage(ctx, claudeRecord("a", "s1", baseTime.Add(2*time.Minute), 40_000, 2_000, 0))

	got := totals(t, r, "a", ReasonSwitched)
	if got.Requests != 1 || got.Misses != 0 || got.Lost != 1_000 {
		t.Fatalf("switch back = %+v, want no miss and 1000 lost", got)
	}
}

func TestGapLongerThanCacheLifetimeIsExpired(t *testing.T) {
	r, _ := newTestRecorder(t, "")
	ctx := context.Background()
	r.HandleUsage(ctx, claudeRecord("a", "s1", baseTime, 0, 30_000, 0))
	r.HandleUsage(ctx, claudeRecord("b", "s1", baseTime.Add(10*time.Minute), 0, 31_000, 0))

	if got := totals(t, r, "b", ReasonExpired); got.Requests != 1 || got.Misses != 1 || got.Lost != 30_000 {
		t.Fatalf("expired = %+v, want one miss losing 30000", got)
	}
	if got := totals(t, r, "b", ReasonSwitched); got.Requests != 0 {
		t.Fatalf("switched = %+v, want none after the cache expired", got)
	}
}

func TestOneHourCacheExtendsLifetime(t *testing.T) {
	r, _ := newTestRecorder(t, "")
	ctx := context.Background()
	first := claudeRecord("a", "s1", baseTime, 0, 30_000, 0)
	first.Detail.CacheCreation1hTokens = coreusage.Int64Ptr(30_000)
	r.HandleUsage(ctx, first)
	r.HandleUsage(ctx, claudeRecord("b", "s1", baseTime.Add(20*time.Minute), 0, 31_000, 0))

	if got := totals(t, r, "b", ReasonSwitched); got.Requests != 1 || got.Misses != 1 {
		t.Fatalf("switched = %+v, want a miss within the 1h lifetime", got)
	}
}

func TestSessionsAreKeyedByModelAndUntrackedHasNoSession(t *testing.T) {
	r, _ := newTestRecorder(t, "")
	ctx := context.Background()
	r.HandleUsage(ctx, claudeRecord("a", "s1", baseTime, 0, 60_000, 0))
	side := claudeRecord("a", "s1", baseTime.Add(time.Second), 0, 500, 20)
	side.Model = "claude-haiku-5-5"
	r.HandleUsage(ctx, side)
	r.HandleUsage(ctx, claudeRecord("a", "s1", baseTime.Add(time.Minute), 60_000, 1_000, 0))
	r.HandleUsage(ctx, claudeRecord("a", "", baseTime.Add(time.Minute), 0, 5_000, 0))

	if got := totals(t, r, "a", ReasonNew); got.Requests != 2 {
		t.Fatalf("new = %+v, want main thread and side model counted separately", got)
	}
	if got := totals(t, r, "a", ReasonContinued); got.Requests != 1 || got.Lost != 0 {
		t.Fatalf("continued = %+v, want the main thread to stay a hit", got)
	}
	if got := totals(t, r, "a", ReasonUntracked); got.Requests != 1 || got.Write != 5_000 {
		t.Fatalf("untracked = %+v", got)
	}
}

func TestIgnoresFailuresOtherProvidersAndEmptyUsage(t *testing.T) {
	r, _ := newTestRecorder(t, "")
	ctx := context.Background()
	failed := claudeRecord("a", "s1", baseTime, 0, 1_000, 0)
	failed.Failed = true
	r.HandleUsage(ctx, failed)
	codex := claudeRecord("a", "s1", baseTime, 0, 1_000, 0)
	codex.Provider = "codex"
	r.HandleUsage(ctx, codex)
	r.HandleUsage(ctx, claudeRecord("a", "s1", baseTime, 0, 0, 0))

	if series, _ := r.Snapshot(baseTime.Add(-time.Hour)); len(series) != 0 {
		t.Fatalf("series = %+v, want none", series)
	}
}

func TestSettingsMarkersAndSnapshotRange(t *testing.T) {
	r, now := newTestRecorder(t, "")
	r.RecordSettings(Settings{Strategy: "round-robin"})
	*now = baseTime.Add(2 * time.Hour)
	r.RecordSettings(Settings{Strategy: "Soonest-Reset "})
	r.RecordSettings(Settings{Strategy: "soonest-reset"})
	*now = baseTime.Add(3 * time.Hour)
	r.RecordSettings(Settings{Strategy: "soonest-reset", SessionAffinity: true, AffinityTTL: "1h", RebindOn: "quota"})
	*now = baseTime.Add(4 * time.Hour)
	r.RecordSettings(Settings{Strategy: "soonest-reset", SessionAffinity: true, AffinityTTL: "1h", RebindOn: "quota", ExpiringWindow: " 24h "})

	_, markers := r.Snapshot(baseTime.Add(150 * time.Minute))
	if len(markers) != 3 {
		t.Fatalf("markers = %+v, want the one in effect at the range start plus the later change", markers)
	}
	if markers[0].Strategy != "soonest-reset" || markers[0].SessionAffinity {
		t.Fatalf("first marker = %+v", markers[0])
	}
	if !markers[1].SessionAffinity || markers[1].RebindOn != "quota" || markers[1].ExpiringWindow != "" {
		t.Fatalf("second marker = %+v", markers[1])
	}
	if markers[2].ExpiringWindow != "24h" {
		t.Fatalf("third marker = %+v, want the expiring window change recorded", markers[2])
	}
}

func TestSaveLoadAndPrune(t *testing.T) {
	dir := t.TempDir()
	r, now := newTestRecorder(t, dir)
	r.RecordSettings(Settings{Strategy: "soonest-reset"})
	r.HandleUsage(context.Background(), claudeRecord("a", "s1", baseTime, 0, 10_000, 0))
	if errSave := r.Save(); errSave != nil {
		t.Fatalf("save: %v", errSave)
	}

	loaded := New(dir)
	series, markers := loaded.Snapshot(baseTime.Add(-time.Hour))
	if len(series) != 1 || len(series[0].Buckets) != 1 || len(markers) != 1 {
		t.Fatalf("loaded series=%+v markers=%+v", series, markers)
	}
	if got := series[0].Buckets[0].Reasons[ReasonNew]; got == nil || got.Write != 10_000 {
		t.Fatalf("loaded tally = %+v", got)
	}

	*now = baseTime.Add(retention + time.Hour)
	r.prune()
	series, markers = r.Snapshot(baseTime.Add(-time.Hour))
	if len(series) != 0 {
		t.Fatalf("series after prune = %+v, want none", series)
	}
	if len(markers) != 1 {
		t.Fatalf("markers after prune = %+v, want the marker still in effect", markers)
	}
}

func TestOutOfOrderRecordsLandInTheirBucket(t *testing.T) {
	r, _ := newTestRecorder(t, "")
	ctx := context.Background()
	r.HandleUsage(ctx, claudeRecord("a", "", baseTime.Add(20*time.Minute), 0, 1_000, 0))
	r.HandleUsage(ctx, claudeRecord("a", "", baseTime, 0, 1_000, 0))
	r.HandleUsage(ctx, claudeRecord("a", "", baseTime.Add(10*time.Minute), 0, 1_000, 0))

	series, _ := r.Snapshot(baseTime.Add(-time.Hour))
	if len(series) != 1 || len(series[0].Buckets) != 3 {
		t.Fatalf("series = %+v", series)
	}
	for i := 1; i < len(series[0].Buckets); i++ {
		if series[0].Buckets[i-1].At >= series[0].Buckets[i].At {
			t.Fatalf("buckets out of order: %+v", series[0].Buckets)
		}
	}
}
