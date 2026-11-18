package gui

import (
	"image"
	"sync/atomic"

	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"

	"github.com/972579/open-image-mcp/internal/storage"
)

// bitmapIDs gives every decoded bitmap its own ID, which viva's renderer uses
// to know when pixels changed.
var bitmapIDs atomic.Uint64

// toBitmap converts an image into viva's premultiplied BGRA bitmap, scaled so
// its longer side is at most maxPx. image.RGBA is already premultiplied, so
// only the red and blue bytes swap.
func toBitmap(img image.Image, maxPx int) gfx.Bitmap {
	rgba := storage.Fit(img, maxPx)
	pix := rgba.Pix
	for i := 0; i+3 < len(pix); i += 4 {
		pix[i], pix[i+2] = pix[i+2], pix[i]
	}
	b := rgba.Bounds()
	return gfx.Bitmap{Pix: pix, Width: b.Dx(), Height: b.Dy(), Stride: rgba.Stride, ID: bitmapIDs.Add(1), Version: 1}
}

// thumbKey names one decoded size of one file.
type thumbKey struct {
	path string
	px   int
}

// thumb is a cache entry: still loading, loaded, or failed.
type thumb struct {
	bmp    gfx.Bitmap
	ready  bool
	failed bool
}

// thumbCache decodes thumbnails on two background workers. The map is only
// touched on the window thread: Get inserts a loading entry and queues the
// key; the worker decodes and posts the finished bitmap back.
type thumbCache struct {
	items map[thumbKey]*thumb
	queue chan thumbKey
	post  func(func())
}

// newThumbCache starts the workers. post delivers results to the window thread.
func newThumbCache(post func(func())) *thumbCache {
	t := &thumbCache{items: map[thumbKey]*thumb{}, queue: make(chan thumbKey, 512), post: post}
	for i := 0; i < 2; i++ {
		go t.worker()
	}
	return t
}

// worker decodes queued files. It blocks on the queue while idle.
func (t *thumbCache) worker() {
	for key := range t.queue {
		img, err := storage.DecodeFile(key.path)
		var bmp gfx.Bitmap
		if err == nil {
			bmp = toBitmap(img, key.px)
		}
		k, b, failed := key, bmp, err != nil
		t.post(func() {
			if e := t.items[k]; e != nil {
				e.bmp, e.ready, e.failed = b, !failed, failed
			}
		})
	}
}

// get returns the bitmap for path at px pixels, queueing a decode the first
// time. Call only from the window thread.
func (t *thumbCache) get(path string, px int) *thumb {
	key := thumbKey{path, px}
	if e := t.items[key]; e != nil {
		return e
	}
	if len(t.items) > 400 {
		for k, e := range t.items {
			if e.ready || e.failed {
				delete(t.items, k)
			}
		}
	}
	e := &thumb{}
	select {
	case t.queue <- key:
		t.items[key] = e
	default: // queue full: try again on a later paint
	}
	return e
}

// forget drops every cached size of path (after the file changed).
func (t *thumbCache) forget(path string) {
	for k := range t.items {
		if k.path == path {
			delete(t.items, k)
		}
	}
}

// imageBox declares a picture fitted inside a w×h box, centered, with a muted
// placeholder while loading or when the file cannot be decoded.
func (m *model) imageBox(c ui.C, path string, w, h float32) {
	theme := c.Theme()
	px := int(max(w, h) * 2) // decode at 2x for Retina
	th := m.thumbs.get(path, px)
	ui.Stack(c).Size(w, h).Shrink(0).Radius(theme.Metrics.RadiusControl).Background(theme.Muted).Clip().Children(func() {
		switch {
		case th.ready:
			bw, bh := float32(th.bmp.Width), float32(th.bmp.Height)
			scale := min(w/bw, h/bh)
			dw, dh := bw*scale, bh*scale
			ui.Column(c).Fill().Center().Children(func() { ui.Image(c, th.bmp).Size(dw, dh) })
		case th.failed:
			ui.Column(c).Fill().Center().Children(func() { ui.Text(c, "无法预览").Muted().FontSize(12) })
		default:
			ui.Column(c).Fill().Center().Children(func() { ui.Spinner(c) })
		}
	})
}
