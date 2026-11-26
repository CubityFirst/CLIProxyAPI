package management

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	defaultQuotaHistoryDays = 14
	maxQuotaHistoryDays     = 21
)

// StartHistory begins recording quota and prompt-cache history into the auth
// directory. The returned channel closes once both recorders stopped and saved
// their history after ctx is cancelled.
func (h *Handler) StartHistory(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if h == nil {
		close(done)
		return done
	}
	dir := h.resolveHistoryDir()
	recorder := quotahistory.New(dir)
	h.mu.Lock()
	h.quotaHistory = recorder
	h.mu.Unlock()
	quotaDone := make(chan struct{})
	go func() {
		defer close(quotaDone)
		recorder.Run(ctx, h.quotaHistoryAuths)
	}()
	cacheDone := h.startCacheHistory(ctx, dir)
	go func() {
		defer close(done)
		<-quotaDone
		<-cacheDone
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
