package management

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cachehistory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	defaultCacheHistoryDays = 7
	maxCacheHistoryDays     = 21
	cacheHistoryPluginName  = "management:cache-history"
)

// startCacheHistory registers the prompt-cache recorder as a usage plugin and
// persists its history into the auth directory until ctx is cancelled.
func (h *Handler) startCacheHistory(ctx context.Context, authDir string) <-chan struct{} {
	done := make(chan struct{})
	recorder := cachehistory.New(authDir)
	h.mu.Lock()
	h.cacheHistory = recorder
	h.mu.Unlock()
	coreusage.RegisterNamedPlugin(cacheHistoryPluginName, recorder)
	go func() {
		defer close(done)
		recorder.Run(ctx, h.cacheHistorySettings)
	}()
	return done
}

func (h *Handler) cacheHistorySettings() cachehistory.Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return cachehistory.Settings{}
	}
	routing := h.cfg.Routing
	strategy := strings.TrimSpace(routing.Strategy)
	if strategy == "" {
		strategy = "round-robin"
	}
	settings := cachehistory.Settings{Strategy: strategy, SessionAffinity: routing.SessionAffinity}
	if routing.SessionAffinity {
		settings.AffinityTTL = routing.SessionAffinityTTL
		settings.RebindOn = routing.SessionAffinityRebindOn
		if strings.TrimSpace(settings.RebindOn) == "" {
			settings.RebindOn = "any"
		}
		if coreauth.ParseExpiringWindow(routing.SessionAffinityExpiringWindow) > 0 {
			settings.ExpiringWindow = strings.TrimSpace(routing.SessionAffinityExpiringWindow)
		}
	}
	return settings
}

// GetCacheHistory returns recorded prompt-cache usage per credential, split by
// why each request did or did not find its cache, plus routing setting changes.
//
// Query: days (1-21, default 7).
func (h *Handler) GetCacheHistory(c *gin.Context) {
	h.mu.Lock()
	recorder := h.cacheHistory
	manager := h.authManager
	h.mu.Unlock()
	days := defaultCacheHistoryDays
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, errParse := strconv.Atoi(raw)
		if errParse != nil || parsed < 1 || parsed > maxCacheHistoryDays {
			c.JSON(http.StatusBadRequest, gin.H{"error": "days must be between 1 and 21"})
			return
		}
		days = parsed
	}
	now := time.Now()
	series, markers := recorder.Snapshot(now.Add(-time.Duration(days) * 24 * time.Hour))
	if series == nil {
		series = []cachehistory.Series{}
	}
	if markers == nil {
		markers = []cachehistory.Marker{}
	}
	if manager != nil {
		labels := make(map[string]string)
		for _, auth := range manager.List() {
			if auth != nil {
				labels[auth.ID] = cacheHistoryAuthLabel(auth)
			}
		}
		for i := range series {
			series[i].Label = labels[series[i].AuthID]
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"now":            now.Unix(),
		"days":           days,
		"bucket_seconds": int64(5 * time.Minute / time.Second),
		"series":         series,
		"markers":        markers,
	})
}

func cacheHistoryAuthLabel(auth *coreauth.Auth) string {
	if _, account := auth.AccountInfo(); account != "" && auth.AuthKind() == coreauth.AuthKindOAuth {
		return account
	}
	return strings.TrimSpace(auth.Label)
}

// resolveHistoryDir returns the auth directory history files are kept in, or
// "" when it cannot be resolved (history then stays in memory only).
func (h *Handler) resolveHistoryDir() string {
	h.mu.Lock()
	authDir := ""
	if h.cfg != nil {
		authDir = h.cfg.AuthDir
	}
	h.mu.Unlock()
	resolved, errResolve := util.ResolveAuthDir(authDir)
	if errResolve != nil {
		log.Warnf("history: %v; history will not persist", errResolve)
		return ""
	}
	return resolved
}
