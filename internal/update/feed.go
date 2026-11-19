package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	vupdate "github.com/vivaui/viva/update"

	"github.com/972579/open-image-mcp/internal/version"
)

// AppID is [app] id in viva.toml, the macOS bundle identifier. The signed feed
// and the bundle inside a downloaded archive must both name it; a test keeps
// the two equal.
const AppID = "local.genimage.mcp"

// FeedURL is where the signed feed is read from. GitHub redirects
// releases/latest/download/<name> to that file of the newest published
// release, so the URL never changes between releases. Tests point it at a TLS
// test server.
var FeedURL = "https://github.com/" + version.Repo + "/releases/latest/download/latest.json"

// PublicKeys are the base64 Ed25519 public keys a feed may be signed with
// (made by `viva update-keygen`; the private key stays on the release machine,
// see scripts/release.sh). A feed signed by any other key is refused. To
// rotate, ship one release that lists the old and the new key, then sign with
// the new one. Tests replace them with a key of their own.
var PublicKeys = []string{"UDmX04n0qCrpU7rfoIMnoCJ9TBoI8Dfvoc3QSf2968E="}

// HTTPClient, when set, carries the feed and archive requests instead of the
// updater's default clients. Tests set it to the TLS test server's client.
var HTTPClient *http.Client

// Update is a newer release the signed feed offers.
type Update struct {
	Version  string
	Date     time.Time
	NotesURL string // the release page (https), may be empty
	// Asset is the archive the feed lists for this platform, or nil when the
	// release ships none for it.
	Asset *vupdate.Asset
}

// Installable reports whether the app can download u's archive and replace
// itself with it: a zip holding the .app on macOS, or an NSIS installer on
// Windows. Anything else (the Windows portable zip, other systems) has to be
// downloaded and installed by hand.
func (u *Update) Installable() bool {
	return u != nil && installable(runtime.GOOS, u.Asset)
}

// installable is Installable for an explicit goos, so tests can cover every
// platform. a may be nil, which is never installable.
func installable(goos string, a *vupdate.Asset) bool {
	if a == nil {
		return false
	}
	switch goos {
	case "darwin":
		return a.Format == vupdate.FormatZIP
	case "windows":
		return a.Format == vupdate.FormatNSIS
	}
	return false
}

// newClient builds the feed client: it trusts only PublicKeys, only a feed for
// AppID, and compares against current. It does no I/O.
//
// current is the running version. It returns an error when a built-in key or
// the version cannot be parsed, which is a build mistake.
func newClient(current string) (*vupdate.Client, error) {
	keys := make([]ed25519.PublicKey, 0, len(PublicKeys))
	for _, text := range PublicKeys {
		key, err := vupdate.ParsePublicKey(text)
		if err != nil {
			return nil, fmt.Errorf("内置的更新公钥无效：%v", err)
		}
		keys = append(keys, key)
	}
	return vupdate.New(vupdate.Config{
		FeedURL:    FeedURL,
		PublicKeys: keys,
		App:        AppID,
		Current:    current,
		HTTP:       HTTPClient,
	})
}

// Check reads the signed feed (two HTTPS GETs: latest.json and its .sig),
// verifies the signature and reports the release when it is newer than
// current. It changes nothing locally.
//
// ctx bounds the requests; current is the running version. It returns
// (nil, nil) when the app is up to date, the newer release otherwise (with a
// nil Asset when the release has nothing for this platform), or an error with
// a readable message when the feed cannot be read or is not trusted.
func Check(ctx context.Context, current string) (*Update, error) {
	client, err := newClient(current)
	if err != nil {
		return nil, err
	}
	feed, err := client.Check(ctx)
	if err != nil {
		return nil, describeCheck(err)
	}
	if feed == nil {
		return nil, nil
	}
	u := &Update{Version: feed.Latest.Version, Date: feed.Latest.Date, NotesURL: feed.Latest.NotesURL}
	if a, err := feed.Asset(runtime.GOOS, runtime.GOARCH); err == nil {
		u.Asset = &a
	}
	return u, nil
}

// describeCheck turns a feed error into the message the about page shows.
func describeCheck(err error) error {
	switch {
	case errors.Is(err, vupdate.ErrSignature):
		return errors.New("更新源的签名校验失败，已忽略这次更新")
	case errors.Is(err, vupdate.ErrManifest):
		return fmt.Errorf("更新源内容无效：%v", err)
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("连接 GitHub 超时，请稍后再试")
	}
	return fmt.Errorf("无法读取更新源：%v", err)
}

// Download fetches u's archive into a folder for its version under base and
// returns the file's path. The file is kept only when its size and SHA-256
// match the signed feed. A partial file left by an interrupted or cancelled
// attempt is resumed. Folders of other versions under base (finished or
// abandoned downloads) are deleted first.
//
// ctx cancels the transfer; current is the running version; base is a
// directory the app owns; progress, when not nil, receives the bytes written
// so far and the total on the downloading goroutine and must not block.
func Download(ctx context.Context, u *Update, current, base string, progress func(done, total int64)) (string, error) {
	if u == nil || u.Asset == nil {
		return "", errors.New("这个版本没有适用于本平台的安装包")
	}
	client, err := newClient(current)
	if err != nil {
		return "", err
	}
	pruneDownloads(base, u.Version)
	path, err := client.Download(ctx, *u.Asset, filepath.Join(base, u.Version), progress)
	switch {
	case err == nil:
		return path, nil
	case errors.Is(err, context.Canceled):
		return "", errors.New("已取消下载，再次更新会从断点继续")
	case errors.Is(err, vupdate.ErrInterrupted):
		return "", fmt.Errorf("下载中断，再次更新会从断点继续：%v", err)
	case errors.Is(err, vupdate.ErrChecksum), errors.Is(err, vupdate.ErrSize):
		return "", errors.New("安装包与更新源记录的大小或校验值不一致，已丢弃")
	}
	return "", fmt.Errorf("下载失败：%v", err)
}

// Cleanup deletes downloads under base that are no longer needed: the folder
// of every version up to and including current, which is either installed or
// older. A newer version's folder is kept, so a download interrupted before a
// restart can still resume. Errors are ignored: a stale download only costs
// disk space.
//
// base is the downloads directory given to Download; current is the running
// version.
func Cleanup(base, current string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if order, err := vupdate.Compare(e.Name(), current); err != nil || order <= 0 {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

// pruneDownloads deletes everything in base except the folder named keep.
// Errors are ignored: a stale download only costs disk space.
func pruneDownloads(base, keep string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() != keep {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

// Install replaces the running app with the downloaded archive and starts the
// new version. On macOS it unpacks the zip beside the running bundle, checks
// the bundle identifier, the version and the code signature, then starts a
// detached helper and calls quit; the helper waits for this process to exit,
// swaps the .app with two renames and reopens it. On Windows the NSIS
// installer runs silently and starts the new program.
//
// artifact is the path Download returned for u; quit must make the app end
// (stop the MCP server and leave the event loop) and is called on this
// goroutine. On error nothing was changed and quit was not called; the
// message says why, for example when the app runs from a read-only or
// translocated location.
func Install(artifact string, u *Update, quit func()) error {
	if u == nil || u.Asset == nil {
		return errors.New("这个版本没有适用于本平台的安装包")
	}
	app := ""
	if runtime.GOOS == "darwin" {
		app = AppID
	}
	err := vupdate.Install(vupdate.InstallOptions{Artifact: artifact, Asset: *u.Asset, Version: u.Version, App: app}, quit)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vupdate.ErrNotWritable):
		return fmt.Errorf("无法替换当前应用，请把 GenImage MCP 放进「应用程序」文件夹后再更新（%v）", err)
	case errors.Is(err, vupdate.ErrUntrusted):
		return fmt.Errorf("新版本的代码签名校验失败，已取消更新（%v）", err)
	case errors.Is(err, vupdate.ErrUnsupported), errors.Is(err, vupdate.ErrManagedExternally):
		return fmt.Errorf("当前安装方式不支持自动更新（%v）", err)
	}
	return fmt.Errorf("安装更新失败：%v", err)
}
