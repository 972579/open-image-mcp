// Package update keeps the app up to date from its GitHub Releases.
//
// The decision and the download come from the signed update feed (feed.go):
// latest.json and its Ed25519 signature, published with every release by
// scripts/release.sh. The app trusts only a feed signed with the key whose
// public half is in PublicKeys, and installs only an archive whose size and
// SHA-256 the feed lists. This file reads the release notes shown next to an
// update from the public GitHub API; they are decoration, never trusted.
//
// Nothing is sent about the user or their configuration: the requests carry a
// generic User-Agent with the app version.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APIBase is the GitHub API root; tests point it at a fake server.
var APIBase = "https://api.github.com"

// Release is the latest published (non-draft, non-prerelease) GitHub release,
// as far as the about page needs it.
type Release struct {
	Tag       string // e.g. "v1.2.0"
	Version   string // tag without the leading "v"
	Name      string
	Notes     string // release body (Markdown)
	URL       string // release page
	Published time.Time
}

// Latest fetches the latest GitHub release of repo ("owner/name"), for its
// release notes. It makes one GET request to the public API and changes
// nothing locally.
//
// ctx bounds the request; client may be nil for a default client with a
// 15-second timeout; current is the running version, sent only in the
// User-Agent. It returns the release, or an error with a readable message on
// network failures, rate limiting (HTTP 403/429) or when the repo has no
// release yet.
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
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("无法解析 GitHub 返回的版本信息：%v", err)
	}
	return &Release{
		Tag: raw.TagName, Version: strings.TrimPrefix(raw.TagName, "v"), Name: raw.Name,
		Notes: raw.Body, URL: raw.HTMLURL, Published: raw.PublishedAt,
	}, nil
}
