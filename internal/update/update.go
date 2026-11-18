// Package update checks GitHub Releases for a newer version of the app.
//
// It only reads public release metadata from api.github.com: the request
// carries a generic User-Agent with the app version and nothing about the
// user or their configuration. Downloading and installing stay manual: the
// GUI opens the release asset or page in the browser.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIBase is the GitHub API root; tests point it at a fake server.
var APIBase = "https://api.github.com"

// Asset is one downloadable file of a release.
type Asset struct {
	Name string
	URL  string // browser download URL
	Size int64
}

// Release is the latest published (non-draft, non-prerelease) release.
type Release struct {
	Tag       string // e.g. "v1.2.0"
	Version   string // tag without the leading "v"
	Name      string
	Notes     string // release body (Markdown)
	URL       string // release page
	Published time.Time
	Assets    []Asset
}

// Latest fetches the latest release of repo ("owner/name").
//
// ctx bounds the request; client may be nil for a default client with a
// 15-second timeout; current is the running version, sent only in the
// User-Agent. It returns an error with a readable message on network
// failures, rate limiting (HTTP 403/429) or when the repo has no release yet.
func Latest(ctx context.Context, client *http.Client, repo, current string) (*Release, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(APIBase, "/"), repo), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "open-image-mcp/"+current)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接 GitHub：%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("仓库 %s 还没有发布任何版本", repo)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub API 请求过于频繁（HTTP %d），请稍后再试", resp.StatusCode)
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}
	var raw struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("无法解析 GitHub 返回的版本信息：%v", err)
	}
	r := &Release{
		Tag: raw.TagName, Version: strings.TrimPrefix(raw.TagName, "v"), Name: raw.Name,
		Notes: raw.Body, URL: raw.HTMLURL, Published: raw.PublishedAt,
	}
	for _, a := range raw.Assets {
		r.Assets = append(r.Assets, Asset{Name: a.Name, URL: a.URL, Size: a.Size})
	}
	return r, nil
}

// Newer reports whether version a is newer than b. Both are dotted numbers
// with an optional leading "v" and optional "-suffix"; a version with a
// suffix (a pre-release) is older than the same version without one.
// Unparseable versions never count as newer.
func Newer(a, b string) bool {
	pa, sa, okA := parse(a)
	pb, sb, okB := parse(b)
	if !okA || !okB {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return sa == "" && sb != ""
}

// parse splits "v1.2.3-rc1" into [1 2 3] and "rc1".
func parse(v string) ([3]int, string, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, suffix, _ := strings.Cut(v, "-")
	v, _, _ = strings.Cut(v, "+")
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, "", false
		}
		out[i] = n
	}
	return out, suffix, true
}

// AssetFor picks the download for a platform. On macOS it prefers the .dmg
// for the CPU architecture, then a .zip; on Windows it prefers a setup .exe,
// then a .zip. It returns nil when the release ships nothing for the platform.
//
// goos and goarch are runtime.GOOS and runtime.GOARCH values.
func (r *Release) AssetFor(goos, goarch string) *Asset {
	var prefer []string
	switch goos {
	case "darwin":
		prefer = []string{".dmg", ".zip"}
	case "windows":
		prefer = []string{"setup.exe", ".zip", ".exe"}
	default:
		prefer = []string{".tar.gz", ".zip"}
	}
	platform := goos + "-" + goarch
	for _, ext := range prefer {
		for i := range r.Assets {
			name := strings.ToLower(r.Assets[i].Name)
			if strings.Contains(name, platform) && strings.HasSuffix(name, ext) {
				return &r.Assets[i]
			}
		}
	}
	return nil
}
