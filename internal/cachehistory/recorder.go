// Package cachehistory tracks Claude prompt-cache efficiency per credential and
// attributes lost cache to its likely cause, so the management UI can show how
// often load balancing moves a conversation onto an account that does not hold
// its prompt cache.
//
// Anthropic keeps a prompt cache per account. Each response reports how many
// prompt tokens were read from cache, written to cache, and sent uncached. The
// recorder compares every request with the previous request of the same
// conversation (session and model): the previous request read or wrote roughly
// what the cache held, so whatever part of it was not read back this time was
// lost. The
// loss is then attributed by what changed between the two requests:
//
//   - continued: same account while the cache was alive (model, prompt, or
//     compaction changes)
//   - switched: a different account while the cache was alive (routing)
//   - expired: the gap was longer than the cache lifetime (idle)
//
// First requests of a conversation are counted as "new" and requests without a
// session identity as "untracked"; neither has a previous prompt to lose.
//
// Only per-credential totals in 5-minute buckets are kept. No prompt content,
// session IDs, or client keys are stored.
package cachehistory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	// FileName is the history file stored in the auth directory. It has no
	// .json extension so the auth watcher never treats it as a credential.
	FileName = ".cache-history"

	fileVersion      = 1
	bucketSize       = 5 * time.Minute
	retention        = 21 * 24 * time.Hour
	persistInterval  = 5 * time.Minute
	settingsInterval = time.Minute

	// defaultCacheTTL is Anthropic's default prompt cache lifetime; longCacheTTL
	// applies once a conversation was seen writing 1-hour cache entries.
	defaultCacheTTL = 5 * time.Minute
	longCacheTTL    = time.Hour
	// sessionIdle drops conversation state that can no longer have a live cache.
	sessionIdle = longCacheTTL + 10*time.Minute
	maxSessions = 20000

	// A request is a miss when it lost at least minMissTokens and at least half
	// of what the previous request left in the cache.
	minMissTokens = 1024
)

// Reasons a request is attributed to.
const (
	ReasonContinued = "continued"
	ReasonSwitched  = "switched"
	ReasonExpired   = "expired"
	ReasonNew       = "new"
	ReasonUntracked = "untracked"
)

// Tally sums the requests of one reason. Token fields are prompt tokens: read
// from cache, written to cache, and sent uncached. Lost counts what the
// previous request left in the cache that this request processed again
// instead of reading.
type Tally struct {
	Requests int64 `json:"n"`
	Misses   int64 `json:"m,omitempty"`
	Read     int64 `json:"r,omitempty"`
	Write    int64 `json:"w,omitempty"`
	Input    int64 `json:"i,omitempty"`
	Lost     int64 `json:"l,omitempty"`
}

// Bucket holds the tallies of one credential for one 5-minute interval.
type Bucket struct {
	At      int64             `json:"t"`
	Reasons map[string]*Tally `json:"by"`
}

// Series is the recorded history of one credential.
type Series struct {
	AuthID   string   `json:"auth_id"`
	Provider string   `json:"provider"`
	Label    string   `json:"label,omitempty"`
	Buckets  []Bucket `json:"buckets"`
}

// Settings are the routing settings that decide how often conversations move
// between accounts.
type Settings struct {
	Strategy        string `json:"strategy"`
	SessionAffinity bool   `json:"session_affinity"`
	AffinityTTL     string `json:"affinity_ttl,omitempty"`
	RebindOn        string `json:"rebind_on,omitempty"`
	ExpiringWindow  string `json:"expiring_window,omitempty"`
}

// Marker records when routing settings took effect.
type Marker struct {
	At int64 `json:"t"`
	Settings
}

type fileFormat struct {
	Version int       `json:"version"`
	Series  []*Series `json:"series"`
	Markers []Marker  `json:"markers"`
}

// session is the last request seen for one conversation. cached is the part
// of its prompt that was read from or written to the cache.
type session struct {
	authID string
	at     time.Time
	cached int64
	ttl    time.Duration
}

// Recorder collects and persists cache history. It implements usage.Plugin.
type Recorder struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	series   map[string]*Series
	markers  []Marker
	sessions map[string]*session
	dirty    bool
}

// New creates a recorder persisting to FileName inside authDir and loads any
// existing history. A missing or unreadable file starts an empty history.
func New(authDir string) *Recorder {
	r := &Recorder{
		now:      time.Now,
		series:   make(map[string]*Series),
		sessions: make(map[string]*session),
	}
	if strings.TrimSpace(authDir) != "" {
		r.path = filepath.Join(authDir, FileName)
		if errLoad := r.load(); errLoad != nil && !os.IsNotExist(errLoad) {
			log.Warnf("cache history: failed to load %s: %v", r.path, errLoad)
		}
	}
	return r
}

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	var file fileFormat
	if errUnmarshal := json.Unmarshal(raw, &file); errUnmarshal != nil {
		return fmt.Errorf("decode cache history: %w", errUnmarshal)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range file.Series {
		if s == nil || s.AuthID == "" {
			continue
		}
		r.series[s.AuthID] = s
	}
	r.markers = file.Markers
	return nil
}

// Run records routing settings every minute and persists periodically until
// ctx ends, then saves once more.
func (r *Recorder) Run(ctx context.Context, settings func() Settings) {
	if r == nil {
		return
	}
	settingsTicker := time.NewTicker(settingsInterval)
	persistTicker := time.NewTicker(persistInterval)
	defer settingsTicker.Stop()
	defer persistTicker.Stop()
	if settings != nil {
		r.RecordSettings(settings())
	}
	for {
		select {
		case <-ctx.Done():
			r.saveLogged()
			return
		case <-settingsTicker.C:
			if settings != nil {
				r.RecordSettings(settings())
			}
			r.prune()
		case <-persistTicker.C:
			r.saveLogged()
		}
	}
}

// RecordSettings adds a marker when the routing settings differ from the
// latest marker.
func (r *Recorder) RecordSettings(settings Settings) {
	if r == nil {
		return
	}
	settings.Strategy = strings.ToLower(strings.TrimSpace(settings.Strategy))
	settings.AffinityTTL = strings.TrimSpace(settings.AffinityTTL)
	settings.RebindOn = strings.ToLower(strings.TrimSpace(settings.RebindOn))
	settings.ExpiringWindow = strings.TrimSpace(settings.ExpiringWindow)
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := len(r.markers); n > 0 && r.markers[n-1].Settings == settings {
		return
	}
	r.markers = append(r.markers, Marker{At: r.now().Unix(), Settings: settings})
	r.dirty = true
}

// HandleUsage implements usage.Plugin.
func (r *Recorder) HandleUsage(_ context.Context, record coreusage.Record) {
	if r == nil || record.Failed {
		return
	}
	if strings.ToLower(strings.TrimSpace(record.Provider)) != "claude" {
		return
	}
	authID := strings.TrimSpace(record.AuthID)
	read := max(record.Detail.CacheReadTokens, 0)
	write := max(record.Detail.CacheCreationTokens, 0)
	input := max(record.Detail.InputTokens, 0)
	prompt := read + write + input
	if authID == "" || prompt == 0 {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = r.now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	reason := ReasonUntracked
	var lost int64
	miss := false
	if key := sessionKey(record); key != "" {
		prev := r.sessions[key]
		ttl := defaultCacheTTL
		if prev != nil {
			ttl = prev.ttl
		}
		if writesLongCache(record.Detail) {
			ttl = longCacheTTL
		}
		if prev == nil {
			reason = ReasonNew
		} else {
			switch {
			case at.Sub(prev.at) > prev.ttl:
				reason = ReasonExpired
			case prev.authID != authID:
				reason = ReasonSwitched
			default:
				reason = ReasonContinued
			}
			lost = min(max(prev.cached-read, 0), write+input)
			miss = lost >= minMissTokens && lost*2 >= prev.cached
		}
		if prev == nil || !at.Before(prev.at) {
			r.rememberLocked(key, &session{authID: authID, at: at, cached: read + write, ttl: ttl})
		}
	}

	tally := r.tallyLocked(authID, at, reason)
	tally.Requests++
	if miss {
		tally.Misses++
	}
	tally.Read += read
	tally.Write += write
	tally.Input += input
	tally.Lost += lost
	r.dirty = true
}

// sessionKey identifies one conversation's cache: the prompt cache is per
// model, so side requests on another model (e.g. a small model for titles)
// must not be compared with the main thread.
func sessionKey(record coreusage.Record) string {
	sessionID := strings.TrimSpace(record.SessionID)
	if sessionID == "" {
		return ""
	}
	model := strings.TrimSpace(record.ResponseModel)
	if model == "" {
		model = strings.TrimSpace(record.Model)
	}
	return sessionID + "\x00" + strings.ToLower(model)
}

func writesLongCache(detail coreusage.Detail) bool {
	if detail.CacheCreation1hTokens != nil && *detail.CacheCreation1hTokens > 0 {
		return true
	}
	return detail.CacheDetails != nil && detail.CacheDetails.Ephemeral1hInputTokens != nil &&
		*detail.CacheDetails.Ephemeral1hInputTokens > 0
}

func (r *Recorder) rememberLocked(key string, s *session) {
	if _, exists := r.sessions[key]; !exists && len(r.sessions) >= maxSessions {
		r.pruneSessionsLocked(s.at)
		if len(r.sessions) >= maxSessions {
			r.sessions = make(map[string]*session)
		}
	}
	r.sessions[key] = s
}

func (r *Recorder) pruneSessionsLocked(now time.Time) {
	for key, s := range r.sessions {
		if now.Sub(s.at) > sessionIdle {
			delete(r.sessions, key)
		}
	}
}

// tallyLocked returns the tally for authID's bucket containing at, creating
// the series and bucket when needed. Buckets stay sorted by time.
func (r *Recorder) tallyLocked(authID string, at time.Time, reason string) *Tally {
	s := r.series[authID]
	if s == nil {
		s = &Series{AuthID: authID, Provider: "claude"}
		r.series[authID] = s
	}
	start := at.Unix() - at.Unix()%int64(bucketSize/time.Second)
	// Records arrive close to time order, so search from the end.
	i := len(s.Buckets)
	for i > 0 && s.Buckets[i-1].At > start {
		i--
	}
	if i == 0 || s.Buckets[i-1].At != start {
		s.Buckets = slices.Insert(s.Buckets, i, Bucket{At: start, Reasons: make(map[string]*Tally)})
		i++
	}
	bucket := &s.Buckets[i-1]
	if bucket.Reasons == nil {
		bucket.Reasons = make(map[string]*Tally)
	}
	tally := bucket.Reasons[reason]
	if tally == nil {
		tally = &Tally{}
		bucket.Reasons[reason] = tally
	}
	return tally
}

// prune drops buckets past retention, markers before the one in effect at the
// retention cutoff, and idle conversation state.
func (r *Recorder) prune() {
	if r == nil {
		return
	}
	now := r.now()
	cutoff := now.Add(-retention).Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.series {
		drop := sort.Search(len(s.Buckets), func(i int) bool { return s.Buckets[i].At >= cutoff })
		if drop > 0 {
			s.Buckets = append([]Bucket(nil), s.Buckets[drop:]...)
			r.dirty = true
		}
		if len(s.Buckets) == 0 {
			delete(r.series, id)
		}
	}
	keep := sort.Search(len(r.markers), func(i int) bool { return r.markers[i].At >= cutoff })
	if keep > 1 {
		r.markers = append([]Marker(nil), r.markers[keep-1:]...)
		r.dirty = true
	}
	r.pruneSessionsLocked(now)
}

// Snapshot returns copies of all series with buckets at or after since, and
// the markers in effect during that range.
func (r *Recorder) Snapshot(since time.Time) ([]Series, []Marker) {
	if r == nil {
		return nil, nil
	}
	cutoff := since.Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	series := make([]Series, 0, len(r.series))
	for _, s := range r.series {
		start := sort.Search(len(s.Buckets), func(i int) bool { return s.Buckets[i].At >= cutoff })
		if start == len(s.Buckets) {
			continue
		}
		copySeries := Series{AuthID: s.AuthID, Provider: s.Provider, Label: s.Label}
		copySeries.Buckets = make([]Bucket, 0, len(s.Buckets)-start)
		for _, b := range s.Buckets[start:] {
			reasons := make(map[string]*Tally, len(b.Reasons))
			for reason, tally := range b.Reasons {
				t := *tally
				reasons[reason] = &t
			}
			copySeries.Buckets = append(copySeries.Buckets, Bucket{At: b.At, Reasons: reasons})
		}
		series = append(series, copySeries)
	}
	sort.Slice(series, func(i, j int) bool { return series[i].AuthID < series[j].AuthID })
	// Keep the marker in effect at the range start.
	first := sort.Search(len(r.markers), func(i int) bool { return r.markers[i].At > cutoff })
	if first > 0 {
		first--
	}
	markers := append([]Marker{}, r.markers[first:]...)
	return series, markers
}

func (r *Recorder) saveLogged() {
	if errSave := r.Save(); errSave != nil {
		log.Warnf("cache history: %v", errSave)
	}
}

// Save writes the history to disk when it changed since the last save.
func (r *Recorder) Save() error {
	if r == nil || r.path == "" {
		return nil
	}
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return nil
	}
	file := fileFormat{Version: fileVersion, Series: make([]*Series, 0, len(r.series)), Markers: r.markers}
	for _, s := range r.series {
		file.Series = append(file.Series, s)
	}
	raw, errMarshal := json.Marshal(&file)
	r.dirty = false
	r.mu.Unlock()
	if errMarshal != nil {
		return fmt.Errorf("encode cache history: %w", errMarshal)
	}
	if errWrite := writeFileAtomic(r.path, raw); errWrite != nil {
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
		return fmt.Errorf("write cache history: %w", errWrite)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), FileName+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
