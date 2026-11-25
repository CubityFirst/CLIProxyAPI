// Package quotahistory records how much of each credential's quota windows was
// used over time, so the management UI can chart remaining pool capacity.
//
// Samples are taken from the passive quota snapshot that the auth manager
// already keeps from upstream response headers (auth.Quota.Signals), so
// recording costs no extra upstream requests. A credential only gets a new
// sample when a newer upstream response was observed; between observations its
// usage cannot have changed except through a reset, which the UI derives from
// the recorded reset times.
package quotahistory

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// FileName is the history file stored in the auth directory. It has no
	// .json extension so the auth watcher never treats it as a credential.
	FileName = ".quota-history"

	fileVersion     = 1
	bucketSize      = 5 * time.Minute
	retention       = 21 * 24 * time.Hour
	sampleInterval  = time.Minute
	persistInterval = 5 * time.Minute
)

// Sample is one observation of a credential's quota windows. Used values are
// percentages (0-100) of the window consumed; resets are unix seconds. Zero
// reset values mean the upstream did not report that window.
type Sample struct {
	At          int64   `json:"t"`
	WeeklyUsed  float64 `json:"w"`
	ShortUsed   float64 `json:"s"`
	WeeklyReset int64   `json:"wr,omitempty"`
	ShortReset  int64   `json:"sr,omitempty"`
}

// Series is the recorded history of one credential.
type Series struct {
	AuthID   string   `json:"auth_id"`
	Provider string   `json:"provider"`
	Label    string   `json:"label,omitempty"`
	Samples  []Sample `json:"samples"`
	// lastObserved is the upstream observation time of the newest sample.
	lastObserved time.Time
}

type fileFormat struct {
	Version int       `json:"version"`
	Series  []*Series `json:"series"`
}

// Recorder collects and persists quota history.
type Recorder struct {
	path string

	mu     sync.Mutex
	series map[string]*Series
	dirty  bool
}

// New creates a recorder persisting to FileName inside authDir and loads any
// existing history. A missing or unreadable file starts an empty history.
func New(authDir string) *Recorder {
	r := &Recorder{series: make(map[string]*Series)}
	if strings.TrimSpace(authDir) != "" {
		r.path = filepath.Join(authDir, FileName)
		if errLoad := r.load(); errLoad != nil && !os.IsNotExist(errLoad) {
			log.Warnf("quota history: failed to load %s: %v", r.path, errLoad)
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
		return fmt.Errorf("decode quota history: %w", errUnmarshal)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range file.Series {
		if s == nil || s.AuthID == "" {
			continue
		}
		if n := len(s.Samples); n > 0 {
			s.lastObserved = time.Unix(s.Samples[n-1].At, 0)
		}
		r.series[s.AuthID] = s
	}
	return nil
}

// Run samples auths every minute and persists periodically until ctx ends,
// then saves once more.
func (r *Recorder) Run(ctx context.Context, list func() []*coreauth.Auth) {
	if r == nil || list == nil {
		return
	}
	sampleTicker := time.NewTicker(sampleInterval)
	persistTicker := time.NewTicker(persistInterval)
	defer sampleTicker.Stop()
	defer persistTicker.Stop()
	r.Record(time.Now(), list())
	for {
		select {
		case <-ctx.Done():
			r.saveLogged()
			return
		case now := <-sampleTicker.C:
			r.Record(now, list())
		case <-persistTicker.C:
			r.saveLogged()
		}
	}
}

// Record appends a sample for every credential whose quota snapshot was
// observed after its newest recorded sample, and drops expired samples.
func (r *Recorder) Record(now time.Time, auths []*coreauth.Auth) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, auth := range auths {
		sample, ok := sampleFromAuth(auth)
		if !ok {
			continue
		}
		s := r.series[auth.ID]
		if s == nil {
			s = &Series{AuthID: auth.ID}
			r.series[auth.ID] = s
		}
		s.Provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		if label := authLabel(auth); label != "" {
			s.Label = label
		}
		if !auth.Quota.ObservedAt.After(s.lastObserved) {
			continue
		}
		s.lastObserved = auth.Quota.ObservedAt
		appendSample(s, sample)
		r.dirty = true
	}
	cutoff := now.Add(-retention).Unix()
	for id, s := range r.series {
		drop := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].At >= cutoff })
		if drop > 0 {
			s.Samples = append([]Sample(nil), s.Samples[drop:]...)
			r.dirty = true
		}
		if len(s.Samples) == 0 {
			delete(r.series, id)
		}
	}
}

// appendSample adds sample, replacing the newest sample when both fall in the
// same bucket so storage stays bounded.
func appendSample(s *Series, sample Sample) {
	if n := len(s.Samples); n > 0 {
		last := s.Samples[n-1]
		if sample.At < last.At {
			return
		}
		if sample.At/int64(bucketSize/time.Second) == last.At/int64(bucketSize/time.Second) {
			s.Samples[n-1] = sample
			return
		}
	}
	s.Samples = append(s.Samples, sample)
}

// Snapshot returns copies of all series, keeping samples at or after since.
func (r *Recorder) Snapshot(since time.Time) []Series {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Series, 0, len(r.series))
	cutoff := since.Unix()
	for _, s := range r.series {
		start := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].At >= cutoff })
		// Keep one earlier sample so charts know the level at the window start.
		if start > 0 {
			start--
		}
		copySeries := Series{AuthID: s.AuthID, Provider: s.Provider, Label: s.Label}
		copySeries.Samples = append([]Sample(nil), s.Samples[start:]...)
		out = append(out, copySeries)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthID < out[j].AuthID })
	return out
}

func (r *Recorder) saveLogged() {
	if errSave := r.Save(); errSave != nil {
		log.Warnf("quota history: %v", errSave)
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
	file := fileFormat{Version: fileVersion, Series: make([]*Series, 0, len(r.series))}
	for _, s := range r.series {
		file.Series = append(file.Series, s)
	}
	raw, errMarshal := json.Marshal(&file)
	r.dirty = false
	r.mu.Unlock()
	if errMarshal != nil {
		return fmt.Errorf("encode quota history: %w", errMarshal)
	}
	if errWrite := writeFileAtomic(r.path, raw); errWrite != nil {
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
		return fmt.Errorf("write quota history: %w", errWrite)
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

// sampleFromAuth extracts a sample from the auth's latest quota snapshot.
func sampleFromAuth(auth *coreauth.Auth) (Sample, bool) {
	if auth == nil || auth.ID == "" || auth.Quota.ObservedAt.IsZero() || len(auth.Quota.Signals) == 0 {
		return Sample{}, false
	}
	signals := auth.Quota.Signals
	sample := Sample{At: auth.Quota.ObservedAt.Unix()}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		weekly, okWeekly := parseFloat(signal(signals, "Anthropic-Ratelimit-Unified-7d-Utilization"))
		short, okShort := parseFloat(signal(signals, "Anthropic-Ratelimit-Unified-5h-Utilization"))
		if !okWeekly && !okShort {
			return Sample{}, false
		}
		sample.WeeklyUsed = clampPercent(weekly * 100)
		sample.ShortUsed = clampPercent(short * 100)
		sample.WeeklyReset = parseUnix(signal(signals, "Anthropic-Ratelimit-Unified-7d-Reset"))
		sample.ShortReset = parseUnix(signal(signals, "Anthropic-Ratelimit-Unified-5h-Reset"))
	case "codex":
		weekly, okWeekly := parseFloat(signal(signals, "X-Codex-Secondary-Used-Percent"))
		short, okShort := parseFloat(signal(signals, "X-Codex-Primary-Used-Percent"))
		if !okWeekly && !okShort {
			return Sample{}, false
		}
		sample.WeeklyUsed = clampPercent(weekly)
		sample.ShortUsed = clampPercent(short)
		sample.WeeklyReset = parseUnix(signal(signals, "X-Codex-Secondary-Reset-At"))
		sample.ShortReset = parseUnix(signal(signals, "X-Codex-Primary-Reset-At"))
	default:
		return Sample{}, false
	}
	return sample, true
}

func authLabel(auth *coreauth.Auth) string {
	if _, account := auth.AccountInfo(); account != "" && auth.AuthKind() == coreauth.AuthKindOAuth {
		return account
	}
	return strings.TrimSpace(auth.Label)
}

func signal(signals map[string]string, name string) string {
	if v, ok := signals[name]; ok {
		return v
	}
	for key, v := range signals {
		if strings.EqualFold(key, name) {
			return v
		}
	}
	return ""
}

func parseFloat(raw string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func parseUnix(raw string) int64 {
	v, ok := parseFloat(raw)
	if !ok || v <= 0 {
		return 0
	}
	return int64(v)
}

func clampPercent(v float64) float64 {
	return math.Round(math.Min(100, math.Max(0, v))*10) / 10
}
