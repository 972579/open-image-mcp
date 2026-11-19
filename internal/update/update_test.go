package update

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	vupdate "github.com/vivaui/viva/update"

	"github.com/972579/open-image-mcp/internal/testutil"
)

// TestLatest checks parsing of the GitHub response used for release notes.
func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases/latest" || !strings.HasPrefix(r.Header.Get("User-Agent"), "open-image-mcp/") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"tag_name":"v1.2.0","name":"1.2.0","body":"notes","html_url":"https://github.com/o/r/releases/tag/v1.2.0",
		"published_at":"2026-11-20T10:00:00Z"}`))
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	r, err := Latest(context.Background(), nil, "o/r", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "1.2.0" || r.Notes != "notes" || r.Published.IsZero() {
		t.Fatalf("release = %+v", r)
	}
	if _, err := Latest(context.Background(), nil, "no/release", "1.0.0"); err == nil || !strings.Contains(err.Error(), "还没有发布") {
		t.Errorf("want no-release error, got %v", err)
	}
}

// TestAppIDMatchesVivaToml keeps AppID equal to viva.toml's [app] id: the
// feed viva writes and the bundle it packages both carry that id, and the
// updater refuses anything else.
func TestAppIDMatchesVivaToml(t *testing.T) {
	data, err := os.ReadFile("../../viva.toml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^id\s*=\s*"([^"]+)"`).FindSubmatch(data)
	if m == nil || string(m[1]) != AppID {
		t.Fatalf("viva.toml id = %q, AppID = %q", m, AppID)
	}
}

// TestBuiltInKey checks the embedded public key parses, so a release build
// can verify feeds at all.
func TestBuiltInKey(t *testing.T) {
	if _, err := newClient("1.0.0"); err != nil {
		t.Fatal(err)
	}
}

// useFeed points the updater at f for the rest of the test.
func useFeed(t *testing.T, f *testutil.Feed) {
	t.Helper()
	oldURL, oldKeys, oldClient := FeedURL, PublicKeys, HTTPClient
	FeedURL, PublicKeys, HTTPClient = f.URL(), []string{f.PublicKey}, f.Client()
	t.Cleanup(func() { FeedURL, PublicKeys, HTTPClient = oldURL, oldKeys, oldClient })
}

// TestCheckAndDownload walks the happy path against a fake signed feed: a
// newer release is found with this platform's archive, the download matches
// the feed and lands in a per-version folder, stale folders are pruned, and a
// second download is served from disk.
func TestCheckAndDownload(t *testing.T) {
	archive := bytes.Repeat([]byte("genimage"), 4096)
	f := testutil.NewFeed(AppID, "9.9.9", archive)
	defer f.Close()
	useFeed(t, f)

	if u, err := Check(context.Background(), "9.9.9"); err != nil || u != nil {
		t.Fatalf("same version: update = %+v, err = %v", u, err)
	}
	u, err := Check(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.Version != "9.9.9" || u.Asset == nil || u.NotesURL == "" {
		t.Fatalf("update = %+v", u)
	}

	base := t.TempDir()
	stale := filepath.Join(base, "1.0.0")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	var last int64
	path, err := Download(context.Background(), u, "1.0.0", base, func(done, total int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, archive) {
		t.Fatalf("downloaded %d bytes, err %v", len(got), err)
	}
	if filepath.Dir(path) != filepath.Join(base, "9.9.9") || last != int64(len(archive)) {
		t.Errorf("path = %s, last progress = %d", path, last)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale download folder kept: %v", err)
	}
	if _, err := Download(context.Background(), u, "1.0.0", base, nil); err != nil || f.Downloads() != 1 {
		t.Errorf("second download: err %v, requests %d", err, f.Downloads())
	}
}

// TestCheckRejectsForeignSignature checks a feed signed by another key is
// refused with a readable message.
func TestCheckRejectsForeignSignature(t *testing.T) {
	f := testutil.NewFeed(AppID, "9.9.9", []byte("x"))
	defer f.Close()
	useFeed(t, f)
	f.BreakSignature()
	if _, err := Check(context.Background(), "1.0.0"); err == nil || !strings.Contains(err.Error(), "签名") {
		t.Fatalf("want signature error, got %v", err)
	}
}

// TestCheckRejectsOtherApp checks a validly signed feed for another app id is
// refused.
func TestCheckRejectsOtherApp(t *testing.T) {
	f := testutil.NewFeed("other.app", "9.9.9", []byte("x"))
	defer f.Close()
	useFeed(t, f)
	if _, err := Check(context.Background(), "1.0.0"); err == nil {
		t.Fatal("feed for another app accepted")
	}
}

// TestCleanup checks that the installed and older downloads are removed at
// startup while a newer, possibly partial, download is kept.
func TestCleanup(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"1.0.0", "1.0.2", "1.1.0", "junk"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	Cleanup(base, "1.0.2")
	entries, _ := os.ReadDir(base)
	if len(entries) != 1 || entries[0].Name() != "1.1.0" {
		t.Fatalf("left %v, want only 1.1.0", entries)
	}
	Cleanup(filepath.Join(base, "missing"), "1.0.2") // no folder yet: nothing to do
}

// TestInstallable checks which archives the app installs itself.
func TestInstallable(t *testing.T) {
	zip := &vupdate.Asset{Format: vupdate.FormatZIP}
	nsis := &vupdate.Asset{Format: vupdate.FormatNSIS}
	cases := []struct {
		goos string
		a    *vupdate.Asset
		want bool
	}{
		{"darwin", zip, true},
		{"windows", zip, false},
		{"windows", nsis, true},
		{"linux", &vupdate.Asset{Format: vupdate.FormatTarGz}, false},
		{"darwin", nil, false},
	}
	for _, c := range cases {
		if got := installable(c.goos, c.a); got != c.want {
			t.Errorf("installable(%s, %+v) = %v, want %v", c.goos, c.a, got, c.want)
		}
	}
	if (*Update)(nil).Installable() {
		t.Error("nil update installable")
	}
}
