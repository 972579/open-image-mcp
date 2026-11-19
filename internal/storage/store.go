// Package storage saves generated images to disk under dated folders, keeps
// an append-only history index, and makes previews and thumbnails.
package storage

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "image/gif"  // register decoders for DecodeConfig
	_ "image/jpeg" //
	_ "image/png"  //

	_ "golang.org/x/image/webp" //
)

// Blob is one image to save, with what is known about where it came from.
type Blob struct {
	Data          []byte
	SourceURL     string
	RevisedPrompt string
}

// Meta describes the call that produced a batch of images; it is copied into
// each history record.
type Meta struct {
	Kind     string // "generate" or "edit"
	Source   string // "mcp" or "debug"
	Prompt   string
	Model    string
	Provider string
	Size     string
}

// Saved is one image written to disk.
type Saved struct {
	ID   string `json:"id"`
	Path string `json:"path"` // where the caller asked for it (the output directory)
	// GalleryPath is the copy kept in the gallery directory when the output
	// directory is another one; empty when Path is already in the gallery or
	// the copy could not be written.
	GalleryPath   string `json:"gallery_path,omitempty"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	Bytes         int    `json:"bytes"`
	MIME          string `json:"mime"`
	SourceURL     string `json:"source_url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// Store writes images and the history index. It is safe for concurrent use.
type Store struct {
	historyPath string
	mu          sync.Mutex
	now         func() time.Time
}

// New returns a Store whose history index lives at historyPath.
func New(historyPath string) *Store {
	return &Store{historyPath: historyPath, now: time.Now}
}

// Save writes each blob under dir/YYYY-MM-DD/ with a name built from prefix,
// time, model and a random suffix, then appends one history record per image.
// When galleryDir is set and is not dir, each image is also copied under
// galleryDir/YYYY-MM-DD/ with the same name, so the gallery keeps every image
// even after the file in dir is moved or deleted; the history record then
// points at the gallery copy (Record.Path) and remembers the requested file
// (Record.OutputPath). A gallery copy that cannot be written is skipped and
// the record points at the file in dir instead. Files are written via temp
// file + rename so a crash never leaves half an image behind.
//
// dir must be absolute; galleryDir is absolute or empty (no copy); prefix is
// sanitized for file names; meta is recorded in the history. It returns the
// saved images in order, or the first error (images saved before the error
// stay on disk and in the history).
func (s *Store) Save(blobs []Blob, dir, galleryDir, prefix string, meta Meta) ([]Saved, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("输出目录必须是绝对路径: %q", dir)
	}
	now := s.now()
	stamp := now.Format("2006-01-02")
	day := filepath.Join(dir, stamp)
	if err := os.MkdirAll(day, 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}
	galleryDay := ""
	if galleryDir != "" && filepath.IsAbs(galleryDir) && filepath.Clean(galleryDir) != filepath.Clean(dir) {
		galleryDay = filepath.Join(galleryDir, stamp)
	}
	base := fmt.Sprintf("%s_%s_%s_%s", SanitizeName(prefix, "img"), now.Format("20060102-150405"), SanitizeName(meta.Model, "model"), randomHex(3))

	var out []Saved
	for i, b := range blobs {
		if len(b.Data) == 0 {
			return out, fmt.Errorf("第 %d 张图片内容为空", i+1)
		}
		mime := DetectMIME(b.Data)
		name := fmt.Sprintf("%s_%02d%s", base, i+1, ExtFor(mime))
		path := filepath.Join(day, name)
		if err := writeAtomic(path, b.Data); err != nil {
			return out, fmt.Errorf("保存图片失败: %w", err)
		}
		w, h := Dimensions(b.Data)
		saved := Saved{
			ID: randomHex(8), Path: path, Width: w, Height: h, Bytes: len(b.Data), MIME: mime,
			SourceURL: b.SourceURL, RevisedPrompt: b.RevisedPrompt,
		}
		if galleryDay != "" {
			copyPath := filepath.Join(galleryDay, name)
			if os.MkdirAll(galleryDay, 0o755) == nil && writeAtomic(copyPath, b.Data) == nil {
				saved.GalleryPath = copyPath
			}
		}
		out = append(out, saved)
		rec := Record{
			ID: saved.ID, Path: path, Kind: meta.Kind, Source: meta.Source, Prompt: meta.Prompt,
			RevisedPrompt: b.RevisedPrompt, Model: meta.Model, Provider: meta.Provider, Size: meta.Size,
			Width: w, Height: h, Bytes: len(b.Data), MIME: mime, CreatedAt: now,
		}
		if saved.GalleryPath != "" {
			rec.Path, rec.OutputPath = saved.GalleryPath, path
		}
		if err := s.append(rec); err != nil {
			return out, fmt.Errorf("写入历史记录失败: %w", err)
		}
	}
	return out, nil
}

// DetectMIME sniffs the image type from its magic bytes, ignoring what the
// upstream claimed.
func DetectMIME(data []byte) string {
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return mime
	}
	return "image/png"
}

// ExtFor maps an image MIME type to a file extension.
func ExtFor(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}

// Dimensions returns the pixel size of an encoded image, or 0, 0 when the
// format cannot be read.
func Dimensions(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N}_-]+`)

// SanitizeName makes s safe as part of a file name: path separators, dots and other
// punctuation become "-", length is capped at 40 runes, and an empty result
// becomes fallback.
func SanitizeName(s, fallback string) string {
	s = strings.Trim(unsafeName.ReplaceAllString(strings.TrimSpace(s), "-"), "-")
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40])
	}
	if s == "" {
		return fallback
	}
	return s
}

// writeAtomic writes an image file (mode 0644) via writeFileAtomic.
func writeAtomic(path string, data []byte) error {
	return writeFileAtomic(path, data, 0o644)
}

// writeFileAtomic writes data to a temp file in the target directory, sets
// mode, and renames it into place, so readers see the old file or the new
// one, never a partial write.
//
// path is the final file; data its content; mode its permissions. It returns
// the first error; the temp file is removed either way.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// randomHex returns 2n random hex characters.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
