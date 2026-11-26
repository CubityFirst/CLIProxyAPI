package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
)

func TestParseGitHubRepository(t *testing.T) {
	cases := map[string]string{
		"CubityFirst/CLIProxyAPI":                          "CubityFirst/CLIProxyAPI",
		" https://github.com/CubityFirst/CLIProxyAPI ":     "CubityFirst/CLIProxyAPI",
		"https://github.com/CubityFirst/CLIProxyAPI.git/":  "CubityFirst/CLIProxyAPI",
		"https://gitlab.com/CubityFirst/CLIProxyAPI":       "",
		"https://github.com/CubityFirst/CLIProxyAPI/pulls": "",
		"../etc/passwd": "",
		"":              "",
	}
	for raw, want := range cases {
		got, ok := parseGitHubRepository(raw)
		if got != want || ok != (want != "") {
			t.Fatalf("parseGitHubRepository(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

func TestCompareReleaseVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v8.1.5-sr", "8.1.4-sr.2", 1, true},
		{"8.1.5-sr.2", "8.1.5-sr", 1, true},
		{"8.1.5-sr.10", "8.1.5-sr.9", 1, true},
		{"v8.1.5-sr", "8.1.5-sr", 0, true},
		{"8.1.4", "8.1.5-sr", -1, true},
		{"v8.1.5", "dev", 0, false},
	}
	for _, tc := range cases {
		got, ok := compareReleaseVersions(tc.a, tc.b)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("compareReleaseVersions(%q, %q) = %d, %v; want %d, %v", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}

func TestServerUpdateAssetName(t *testing.T) {
	if got := serverUpdateAssetName("8.1.5-sr", "linux", "amd64", false); got != "CLIProxyAPI_8.1.5-sr_linux_amd64_no-plugin.tar.gz" {
		t.Fatalf("no-plugin linux asset = %q", got)
	}
	if got := serverUpdateAssetName("8.1.5-sr", "linux", "arm64", true); got != "CLIProxyAPI_8.1.5-sr_linux_aarch64.tar.gz" {
		t.Fatalf("plugin arm64 asset = %q", got)
	}
	if got := serverUpdateAssetName("8.1.5", "windows", "amd64", true); got != "CLIProxyAPI_8.1.5_windows_amd64.zip" {
		t.Fatalf("windows asset = %q", got)
	}
}

type serverUpdateFixture struct {
	handler *Handler
	dir     string
	release map[string]any
	status  int
	// requests counts calls to the fake GitHub API.
	requests atomic.Int32
}

// newServerUpdateFixture serves the latest release of CubityFirst/CLIProxyAPI from a local
// fake GitHub API and runs as version 8.1.4-sr.2.
func newServerUpdateFixture(t *testing.T, withDir bool) *serverUpdateFixture {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	gin.SetMode(gin.TestMode)

	fixture := &serverUpdateFixture{status: http.StatusOK}
	asset := serverUpdateAssetName("8.1.5-sr", runtime.GOOS, runtime.GOARCH, pluginhost.SupportPluginHeaderValue() == "true")
	fixture.release = map[string]any{
		"tag_name":     "v8.1.5-sr",
		"html_url":     "https://github.com/CubityFirst/CLIProxyAPI/releases/tag/v8.1.5-sr",
		"published_at": "2026-11-26T12:00:00Z",
		"assets":       []map[string]any{{"name": "checksums.txt"}, {"name": asset}},
	}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.requests.Add(1)
		if r.URL.Path != "/repos/CubityFirst/CLIProxyAPI/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(fixture.status)
		_ = json.NewEncoder(w).Encode(fixture.release)
	}))
	t.Cleanup(github.Close)

	previousBase, previousVersion := githubAPIBaseURL, buildinfo.Version
	githubAPIBaseURL, buildinfo.Version = github.URL, "8.1.4-sr.2"
	t.Cleanup(func() { githubAPIBaseURL, buildinfo.Version = previousBase, previousVersion })

	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.RemoteManagement.ServerUpdateRepository = "https://github.com/CubityFirst/CLIProxyAPI"
	if withDir {
		fixture.dir = t.TempDir()
		cfg.RemoteManagement.ServerUpdateDir = fixture.dir
	}
	fixture.handler = NewHandlerWithoutConfigFilePath(cfg, nil)
	return fixture
}

func (f *serverUpdateFixture) call(t *testing.T, method, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(method, "/v8/management/server/update", strings.NewReader(body))
	if body != "" {
		ginCtx.Request.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet {
		f.handler.GetServerUpdate(ginCtx)
	} else {
		f.handler.RequestServerUpdate(ginCtx)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s response: %v (%s)", method, err, rec.Body.String())
	}
	return rec.Code, payload
}

func TestGetServerUpdateReportsLatestReleaseAndHelperStatus(t *testing.T) {
	f := newServerUpdateFixture(t, true)
	status := `{"state":"succeeded","tag":"v8.1.4-sr.2","updated_at":"2026-11-26T09:15:30Z"}`
	if err := os.WriteFile(filepath.Join(f.dir, "status.json"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}

	code, payload := f.call(t, http.MethodGet, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, payload %v", code, payload)
	}
	latest, _ := payload["latest"].(map[string]any)
	if payload["repository"] != "CubityFirst/CLIProxyAPI" || payload["current_version"] != "8.1.4-sr.2" ||
		payload["update_available"] != true || payload["apply_enabled"] != true || payload["pending"] != false ||
		latest["tag"] != "v8.1.5-sr" || latest["version"] != "8.1.5-sr" || latest["asset_ready"] != true {
		t.Fatalf("payload = %v", payload)
	}
	helper, _ := payload["status"].(map[string]any)
	if helper["state"] != "succeeded" {
		t.Fatalf("helper status = %v", payload["status"])
	}
}

func TestGetServerUpdateReportsGitHubErrorsWithoutFailing(t *testing.T) {
	f := newServerUpdateFixture(t, false)
	f.status = http.StatusForbidden
	code, payload := f.call(t, http.MethodGet, "")
	if code != http.StatusOK || payload["latest"] != nil || !strings.Contains(payload["error"].(string), "403") || payload["apply_enabled"] != false {
		t.Fatalf("status %d, payload = %v", code, payload)
	}
}

func TestServerUpdateReusesGitHubLookupForAMinute(t *testing.T) {
	f := newServerUpdateFixture(t, true)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	f.handler.serverRelease.nowFunc = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if code, payload := f.call(t, http.MethodGet, ""); code != http.StatusOK || payload["update_available"] != true {
			t.Fatalf("GET %d: status %d, payload = %v", i, code, payload)
		}
	}
	now = now.Add(serverReleaseCacheTTL - time.Second)
	if code, payload := f.call(t, http.MethodPost, ""); code != http.StatusAccepted {
		t.Fatalf("POST: status %d, payload = %v", code, payload)
	}
	if got := f.requests.Load(); got != 1 {
		t.Fatalf("GitHub requests = %d, want 1", got)
	}

	// Once it expires the next check asks again, and an error is kept the same way, so a
	// rate-limited check isn't retried on every poll.
	now = now.Add(time.Second)
	f.status = http.StatusForbidden
	for i := 0; i < 2; i++ {
		if _, payload := f.call(t, http.MethodGet, ""); payload["latest"] != nil || !strings.Contains(payload["error"].(string), "403") {
			t.Fatalf("GET after expiry %d: payload = %v", i, payload)
		}
	}
	if got := f.requests.Load(); got != 2 {
		t.Fatalf("GitHub requests = %d, want 2", got)
	}
}

func TestRequestServerUpdateWritesRequestOnce(t *testing.T) {
	f := newServerUpdateFixture(t, true)
	code, payload := f.call(t, http.MethodPost, `{"tag":"v8.1.5-sr"}`)
	if code != http.StatusAccepted || payload["requested"] != "v8.1.5-sr" {
		t.Fatalf("status %d, payload = %v", code, payload)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "request.json"))
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	var request map[string]string
	if err = json.Unmarshal(data, &request); err != nil || request["tag"] != "v8.1.5-sr" || request["from"] != "8.1.4-sr.2" || request["requested_at"] == "" {
		t.Fatalf("request = %s (%v)", data, err)
	}
	entries, _ := os.ReadDir(f.dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}

	if code, payload = f.call(t, http.MethodPost, ""); code != http.StatusConflict || payload["error"] != "update_pending" {
		t.Fatalf("second request: status %d, payload = %v", code, payload)
	}
	if _, payload = f.call(t, http.MethodGet, ""); payload["pending"] != true {
		t.Fatalf("pending not reported: %v", payload)
	}
}

func TestRequestServerUpdateRefusals(t *testing.T) {
	cases := []struct {
		name    string
		withDir bool
		body    string
		setup   func(f *serverUpdateFixture)
		code    int
		errCode string
	}{
		{name: "no update dir", withDir: false, code: http.StatusConflict, errCode: "update_disabled"},
		{name: "not the latest tag", withDir: true, body: `{"tag":"v8.1.4-sr.9"}`, code: http.StatusConflict, errCode: "not_latest"},
		{name: "already current", withDir: true, setup: func(*serverUpdateFixture) { buildinfo.Version = "8.1.5-sr" }, code: http.StatusConflict, errCode: "no_update"},
		{name: "asset not uploaded yet", withDir: true, setup: func(f *serverUpdateFixture) {
			f.release["assets"] = []map[string]any{{"name": "checksums.txt"}}
		}, code: http.StatusConflict, errCode: "asset_not_ready"},
		{name: "github unavailable", withDir: true, setup: func(f *serverUpdateFixture) { f.status = http.StatusBadGateway }, code: http.StatusBadGateway, errCode: "release_unavailable"},
		{name: "malformed body", withDir: true, body: `{"tag":`, code: http.StatusBadRequest, errCode: "invalid_body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newServerUpdateFixture(t, tc.withDir)
			if tc.setup != nil {
				tc.setup(f)
			}
			code, payload := f.call(t, http.MethodPost, tc.body)
			if code != tc.code || payload["error"] != tc.errCode {
				t.Fatalf("status %d, payload = %v; want %d %s", code, payload, tc.code, tc.errCode)
			}
			if f.dir != "" {
				if _, err := os.Stat(filepath.Join(f.dir, "request.json")); !os.IsNotExist(err) {
					t.Fatalf("request.json written despite refusal (%v)", err)
				}
			}
		})
	}
}
