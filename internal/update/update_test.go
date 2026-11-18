package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewer checks version ordering.
func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.9", true},
		{"2.0", "1.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"1.0.0", "1.0.0-rc1", true},
		{"1.0.0-rc1", "1.0.0", false},
		{"garbage", "1.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestLatest checks parsing of the GitHub response and asset selection.
func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases/latest" || !strings.HasPrefix(r.Header.Get("User-Agent"), "open-image-mcp/") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"tag_name":"v1.2.0","name":"1.2.0","body":"notes","html_url":"https://github.com/o/r/releases/tag/v1.2.0",
		"published_at":"2026-11-20T10:00:00Z","assets":[
		{"name":"genimage-1.2.0-darwin-arm64.dmg","browser_download_url":"https://x/mac.dmg","size":5},
		{"name":"genimage-1.2.0-darwin-arm64.zip","browser_download_url":"https://x/mac.zip","size":6},
		{"name":"genimage-1.2.0-windows-amd64.zip","browser_download_url":"https://x/win.zip","size":7}]}`))
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	r, err := Latest(context.Background(), nil, "o/r", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "1.2.0" || !Newer(r.Version, "1.0.0") || r.Published.IsZero() {
		t.Fatalf("release = %+v", r)
	}
	if a := r.AssetFor("darwin", "arm64"); a == nil || a.URL != "https://x/mac.dmg" {
		t.Errorf("mac asset = %+v", a)
	}
	if a := r.AssetFor("windows", "amd64"); a == nil || a.URL != "https://x/win.zip" {
		t.Errorf("windows asset = %+v", a)
	}
	if a := r.AssetFor("linux", "amd64"); a != nil {
		t.Errorf("linux asset should be nil, got %+v", a)
	}

	if _, err := Latest(context.Background(), nil, "no/release", "1.0.0"); err == nil || !strings.Contains(err.Error(), "还没有发布") {
		t.Errorf("want no-release error, got %v", err)
	}
}
