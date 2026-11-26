package management

// Server updates from GitHub releases.
//
// GET /server/update checks remote-management.server-update-repository for a
// newer release of this binary. POST /server/update records a request for that
// release in remote-management.server-update-dir, where a privileged helper on
// the host (examples/self-update) picks it up. The server never replaces its
// own binary: the helper pins its own repository, verifies the asset digest
// GitHub publishes, swaps the binary and rolls back an unhealthy restart. Its
// progress comes back through status.json in the same directory.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/regexp"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	serverUpdateRequestFile = "request.json"
	serverUpdateStatusFile  = "status.json"
	serverUpdateMaxStatus   = 64 << 10
)

// githubAPIBaseURL is a variable so tests can point it at a local server.
var githubAPIBaseURL = "https://api.github.com"

var (
	githubRepoPattern     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
	releaseTagPattern     = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
	releaseVersionNumbers = regexp.MustCompile(`[0-9]+`)
)

type serverUpdateRelease struct {
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	PublishedAt string `json:"published_at,omitempty"`
	URL         string `json:"url,omitempty"`
	// Asset is the archive the helper installs on this platform; AssetReady is false while
	// the release workflow is still uploading it.
	Asset      string `json:"asset"`
	AssetReady bool   `json:"asset_ready"`
}

type serverUpdateResponse struct {
	Repository      string               `json:"repository"`
	CurrentVersion  string               `json:"current_version"`
	ApplyEnabled    bool                 `json:"apply_enabled"`
	Latest          *serverUpdateRelease `json:"latest"`
	UpdateAvailable bool                 `json:"update_available"`
	Pending         bool                 `json:"pending"`
	Status          json.RawMessage      `json:"status,omitempty"`
	Error           string               `json:"error,omitempty"`
}

type githubReleaseAsset struct {
	Name string `json:"name"`
}

type githubRelease struct {
	TagName     string               `json:"tag_name"`
	HTMLURL     string               `json:"html_url"`
	PublishedAt string               `json:"published_at"`
	Draft       bool                 `json:"draft"`
	Assets      []githubReleaseAsset `json:"assets"`
}

// parseGitHubRepository accepts owner/repo or a github.com URL and returns owner/repo.
func parseGitHubRepository(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	repo := raw
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(parsed.Host, "github.com") {
			return "", false
		}
		repo = strings.Trim(parsed.Path, "/")
	}
	repo = strings.TrimSuffix(repo, ".git")
	if !githubRepoPattern.MatchString(repo) {
		return "", false
	}
	return repo, true
}

// compareReleaseVersions compares the numeric parts of two versions, so 8.1.5-sr.2 is newer
// than 8.1.5-sr and 8.1.4-sr.10. ok is false when either side has no numbers (e.g. "dev").
func compareReleaseVersions(a, b string) (result int, ok bool) {
	parse := func(version string) []int {
		var parts []int
		for _, match := range releaseVersionNumbers.FindAllString(strings.TrimPrefix(strings.TrimSpace(version), "v"), -1) {
			n, err := strconv.Atoi(match)
			if err != nil {
				return nil
			}
			parts = append(parts, n)
		}
		return parts
	}
	left, right := parse(a), parse(b)
	if len(left) == 0 || len(right) == 0 {
		return 0, false
	}
	for i := 0; i < len(left) || i < len(right); i++ {
		var l, r int
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if l != r {
			if l > r {
				return 1, true
			}
			return -1, true
		}
	}
	return 0, true
}

// serverUpdateAssetName is the release archive for this platform and build flavour, matching
// the names produced by .github/workflows/release.yaml.
func serverUpdateAssetName(version, goos, goarch string, pluginSupport bool) string {
	arch := goarch
	if goarch == "arm64" {
		arch = "aarch64"
	}
	suffix := ""
	if !pluginSupport {
		suffix = "_no-plugin"
	}
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("CLIProxyAPI_%s_%s_%s%s.%s", version, goos, arch, suffix, ext)
}

func (h *Handler) fetchLatestServerRelease(c *gin.Context, repo, proxyURL string) (*serverUpdateRelease, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	if proxyURL != "" {
		util.SetProxy(&sdkconfig.SDKConfig{ProxyURL: proxyURL}, client)
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, githubAPIBaseURL+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	setLatestReleaseRequestHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close server release response body")
		}
	}()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var release githubRelease
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&release); errDecode != nil {
		return nil, fmt.Errorf("decode release: %w", errDecode)
	}
	tag := strings.TrimSpace(release.TagName)
	if release.Draft || !releaseTagPattern.MatchString(tag) {
		return nil, fmt.Errorf("latest release has an unsupported tag %q", tag)
	}
	version := strings.TrimPrefix(tag, "v")
	asset := serverUpdateAssetName(version, runtime.GOOS, runtime.GOARCH, pluginhost.SupportPluginHeaderValue() == "true")
	out := &serverUpdateRelease{
		Tag:         tag,
		Version:     version,
		PublishedAt: release.PublishedAt,
		URL:         release.HTMLURL,
		Asset:       asset,
	}
	for _, item := range release.Assets {
		if item.Name == asset {
			out.AssetReady = true
			break
		}
	}
	return out, nil
}

// serverUpdateState gathers everything both endpoints need. A failed GitHub check is
// reported in Error rather than failing the request, so the helper status stays visible.
func (h *Handler) serverUpdateState(c *gin.Context) (serverUpdateResponse, string) {
	cfg := h.configSnapshot()
	resp := serverUpdateResponse{CurrentVersion: buildinfo.Version}
	if cfg == nil {
		resp.Error = "configuration unavailable"
		return resp, ""
	}
	dir := strings.TrimSpace(cfg.RemoteManagement.ServerUpdateDir)
	repo, okRepo := parseGitHubRepository(cfg.RemoteManagement.ServerUpdateRepository)
	resp.Repository = repo
	resp.ApplyEnabled = okRepo && dir != ""
	if dir != "" {
		if _, err := os.Stat(filepath.Join(dir, serverUpdateRequestFile)); err == nil {
			resp.Pending = true
		}
		resp.Status = readServerUpdateStatus(filepath.Join(dir, serverUpdateStatusFile))
	}
	if !okRepo {
		if strings.TrimSpace(cfg.RemoteManagement.ServerUpdateRepository) != "" {
			resp.Error = "remote-management.server-update-repository is not a GitHub repository"
		}
		return resp, dir
	}
	latest, err := h.fetchLatestServerRelease(c, repo, strings.TrimSpace(cfg.ProxyURL))
	if err != nil {
		resp.Error = err.Error()
		return resp, dir
	}
	resp.Latest = latest
	if latest != nil {
		if cmp, ok := compareReleaseVersions(latest.Version, buildinfo.Version); ok && cmp > 0 {
			resp.UpdateAvailable = true
		}
	}
	return resp, dir
}

// readServerUpdateStatus returns the helper's status.json when it is a JSON object.
func readServerUpdateStatus(path string) json.RawMessage {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close server update status")
		}
	}()
	data, err := io.ReadAll(io.LimitReader(file, serverUpdateMaxStatus))
	if err != nil {
		return nil
	}
	var probe map[string]any
	if json.Unmarshal(data, &probe) != nil {
		return nil
	}
	return json.RawMessage(data)
}

// GetServerUpdate reports the current version, the latest release and the helper's status.
func (h *Handler) GetServerUpdate(c *gin.Context) {
	resp, _ := h.serverUpdateState(c)
	c.JSON(http.StatusOK, resp)
}

// RequestServerUpdate records a request for the latest release for the host helper.
func (h *Handler) RequestServerUpdate(c *gin.Context) {
	var body struct {
		Tag string `json:"tag"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil && !errors.Is(errBind, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": errBind.Error()})
		return
	}
	state, dir := h.serverUpdateState(c)
	switch {
	case !state.ApplyEnabled:
		c.JSON(http.StatusConflict, gin.H{"error": "update_disabled", "message": "set remote-management.server-update-repository and server-update-dir, and install the host helper"})
		return
	case state.Pending:
		c.JSON(http.StatusConflict, gin.H{"error": "update_pending", "message": "an update request is already waiting for the host helper"})
		return
	case state.Latest == nil:
		message := state.Error
		if message == "" {
			message = "the repository has no release"
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "release_unavailable", "message": message})
		return
	}
	tag := strings.TrimSpace(body.Tag)
	if tag != "" && tag != state.Latest.Tag {
		c.JSON(http.StatusConflict, gin.H{"error": "not_latest", "message": fmt.Sprintf("%s is not the latest release (%s)", tag, state.Latest.Tag)})
		return
	}
	if !state.UpdateAvailable {
		c.JSON(http.StatusConflict, gin.H{"error": "no_update", "message": fmt.Sprintf("%s is not newer than %s", state.Latest.Tag, state.CurrentVersion)})
		return
	}
	if !state.Latest.AssetReady {
		c.JSON(http.StatusConflict, gin.H{"error": "asset_not_ready", "message": fmt.Sprintf("%s has no %s yet; the release build may still be running", state.Latest.Tag, state.Latest.Asset)})
		return
	}
	request := map[string]string{
		"tag":          state.Latest.Tag,
		"from":         state.CurrentVersion,
		"requested_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeServerUpdateRequest(dir, request); err != nil {
		log.WithError(err).Warn("server update: failed to write request")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request_write_failed", "message": err.Error()})
		return
	}
	log.Infof("server update: requested %s (running %s)", state.Latest.Tag, state.CurrentVersion)
	c.JSON(http.StatusAccepted, gin.H{"requested": state.Latest.Tag})
}

// writeServerUpdateRequest writes request.json atomically, so the helper never reads a
// partial file.
func writeServerUpdateRequest(dir string, request map[string]string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("update dir: %w", err)
	}
	if !info.IsDir() {
		return errors.New("update dir is not a directory")
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".request-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if errRemove := os.Remove(tmpName); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.WithError(errRemove).Debug("server update: failed to remove temp request")
		}
	}()
	if _, err = tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, serverUpdateRequestFile))
}
