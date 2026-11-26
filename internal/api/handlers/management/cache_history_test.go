package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cachehistory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestGetCacheHistoryReturnsSeriesWithLabelsAndSettings(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-a.json",
		Provider: "claude",
		Label:    "Team A",
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.Strategy = "soonest-reset"
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionAffinityTTL = "1h"
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	recorder := cachehistory.New("")
	recorder.RecordSettings(h.cacheHistorySettings())
	recorder.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "claude",
		AuthID:      "claude-a.json",
		SessionID:   "s1",
		Model:       "claude-opus-5-5",
		RequestedAt: time.Now(),
		Detail:      coreusage.Detail{CacheCreationTokens: 12_000},
	})
	h.cacheHistory = recorder

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/observability/cache-history?days=2", nil)
	h.GetCacheHistory(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Days    int                   `json:"days"`
		Series  []cachehistory.Series `json:"series"`
		Markers []cachehistory.Marker `json:"markers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Days != 2 || len(payload.Series) != 1 || payload.Series[0].Label != "Team A" {
		t.Fatalf("payload = %+v", payload)
	}
	if len(payload.Markers) != 1 || payload.Markers[0].RebindOn != "any" || !payload.Markers[0].SessionAffinity {
		t.Fatalf("markers = %+v, want soonest-reset with affinity and default rebind", payload.Markers)
	}
}

func TestGetCacheHistoryRejectsBadDays(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/observability/cache-history?days=30", nil)
	h.GetCacheHistory(ginCtx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
