package testutil

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"time"

	vupdate "github.com/vivaui/viva/update"
)

// Feed is a fake release host for the updater: a TLS test server that serves
// a signed latest.json, its .sig and one archive for the running platform.
// The updater only accepts HTTPS, so the server is TLS and callers use
// Client(), which trusts its certificate.
type Feed struct {
	srv *httptest.Server
	// PublicKey is the base64 Ed25519 public key that verifies the feed.
	PublicKey string
	// Archive is the archive's content, as listed in the feed.
	Archive []byte

	mu        sync.Mutex
	body, sig []byte
	downloads int
}

// NewFeed starts a server whose feed offers version of app, with archive as
// the download for runtime.GOOS/GOARCH (a zip, or a tar.gz on Linux, the
// formats the feed allows there). It signs the feed with a fresh key. Close
// it when done.
func NewFeed(app, version string, archive []byte) *Feed {
	f := &Feed{Archive: archive}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	f.PublicKey = base64.StdEncoding.EncodeToString(pub)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))

	format := vupdate.FormatZIP
	if runtime.GOOS == "linux" {
		format = vupdate.FormatTarGz
	}
	sum := sha256.Sum256(archive)
	feed := vupdate.Feed{
		Schema: vupdate.SchemaVersion,
		App:    app,
		Latest: vupdate.Release{
			Version:  version,
			Date:     time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			NotesURL: "https://github.com/o/r/releases/tag/v" + version,
			Assets: []vupdate.Asset{{
				OS: runtime.GOOS, Arch: runtime.GOARCH, Format: format,
				URL: f.srv.URL + "/archive", Size: int64(len(archive)), SHA256: hex.EncodeToString(sum[:]),
			}},
		},
	}
	if f.body, err = json.Marshal(feed); err != nil {
		panic(err)
	}
	sig, err := vupdate.Sign(f.body, priv)
	if err != nil {
		panic(err)
	}
	f.sig = []byte(sig)
	return f
}

// serve answers the three paths the updater asks for and 404 otherwise.
func (f *Feed) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/latest.json":
		w.Write(f.body)
	case "/latest.json.sig":
		w.Write(f.sig)
	case "/archive":
		f.downloads++
		w.Write(f.Archive)
	default:
		http.NotFound(w, r)
	}
}

// URL is the feed address to give the updater.
func (f *Feed) URL() string { return f.srv.URL + "/latest.json" }

// Client is an HTTP client that trusts the server's certificate.
func (f *Feed) Client() *http.Client { return f.srv.Client() }

// Downloads is how many times the archive was requested.
func (f *Feed) Downloads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downloads
}

// BreakSignature makes the server answer with a signature by another key, as
// a tampered or foreign feed would have.
func (f *Feed) BreakSignature() {
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	sig, err := vupdate.Sign(f.body, other)
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	f.sig = []byte(sig)
	f.mu.Unlock()
}

// Close stops the server.
func (f *Feed) Close() { f.srv.Close() }
