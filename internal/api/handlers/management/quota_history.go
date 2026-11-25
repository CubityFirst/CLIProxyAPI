package management

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	defaultQuotaHistoryDays = 14
	maxQuotaHistoryDays     = 21
)

// StartQuotaHistory begins recording quota history into the auth directory.
// The returned channel closes once recording stopped and history was saved
// after ctx is cancelled.
func (h *Handler) StartQuotaHistory(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if h == nil {
		close(done)
		return done
	}
	h.mu.Lock()
	authDir := ""
	if h.cfg != nil {
		authDir = h.cfg.AuthDir
	}
	h.mu.Unlock()
	resolved, errResolve := util.ResolveAuthDir(authDir)
	if errResolve != nil {
		log.Warnf("quota history: %v; history will not persist", errResolve)
		resolved = ""
	}
	recorder := quotahistory.New(resolved)
	h.mu.Lock()
	h.quotaHistory = recorder
	h.mu.Unlock()
	go func() {
		defer close(done)
		recorder.Run(ctx, h.quotaHistoryAuths)
	}()
	return done
}

func (h *Handler) quotaHistoryAuths() []*coreauth.Auth {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return nil
	}
	return manager.List()
}

// GetQuotaHistory returns recorded quota usage per credential.
//
// Query: days (1-21, default 14).
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	h.mu.Lock()
	recorder := h.quotaHistory
	h.mu.Unlock()
	days := defaultQuotaHistoryDays
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, errParse := strconv.Atoi(raw)
		if errParse != nil || parsed < 1 || parsed > maxQuotaHistoryDays {
			c.JSON(http.StatusBadRequest, gin.H{"error": "days must be between 1 and 21"})
			return
		}
		days = parsed
	}
	now := time.Now()
	series := recorder.Snapshot(now.Add(-time.Duration(days) * 24 * time.Hour))
	if series == nil {
		series = []quotahistory.Series{}
	}
	c.JSON(http.StatusOK, gin.H{
		"now":    now.Unix(),
		"days":   days,
		"series": series,
	})
}
