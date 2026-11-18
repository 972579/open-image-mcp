// Package testutil provides a fake OpenAI-compatible image upstream for tests.
package testutil

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// PNG returns a w×h PNG with a noisy pattern (so its base64 is long enough to exercise redaction).
func PNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x*7 ^ y*13), G: uint8(x * y), B: uint8(x*31 + y*17), A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// Request is one request the fake upstream received.
type Request struct {
	Method      string
	Path        string
	Auth        string
	ContentType string
	JSON        map[string]any      // decoded JSON body, when JSON
	Form        map[string][]string // multipart text fields
	Files       map[string][]File   // multipart files by field name
}

// File is one uploaded multipart file.
type File struct {
	Name string
	MIME string
	Size int
}

// Upstream is a programmable fake image API.
type Upstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []Request
	// Handler decides the response for each POST; nil means "one b64 PNG".
	Handler func(n int, r Request) (status int, body any)
}

// NewUpstream starts a fake upstream. It also serves GET /v1/models and
// GET /files/<name>.png (a PNG, for URL-download tests).
func NewUpstream() *Upstream {
	u := &Upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(u.serve))
	return u
}

// BaseURL returns the server URL with /v1.
func (u *Upstream) BaseURL() string { return u.URL + "/v1" }

// Requests returns the POST requests received so far.
func (u *Upstream) Requests() []Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Request(nil), u.requests...)
}

// B64Response is a standard OpenAI response with n base64 PNGs.
func B64Response(n int) map[string]any {
	data := make([]any, n)
	for i := range data {
		data[i] = map[string]any{"b64_json": base64.StdEncoding.EncodeToString(PNG(32, 24)), "revised_prompt": "revised"}
	}
	return map[string]any{"created": 1, "data": data}
}

// serve records the request and answers it.
func (u *Upstream) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
		writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"id": "gpt-image-2"}, map[string]any{"id": "flux-dev"}}})
		return
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/files/"):
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(PNG(16, 16))
		return
	}
	req := Request{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type")}
	if strings.HasPrefix(req.ContentType, "multipart/") {
		_ = r.ParseMultipartForm(32 << 20)
		req.Form = r.MultipartForm.Value
		req.Files = map[string][]File{}
		for field, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				req.Files[field] = append(req.Files[field], File{Name: fh.Filename, MIME: fh.Header.Get("Content-Type"), Size: int(fh.Size)})
			}
		}
	} else {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req.JSON)
	}
	u.mu.Lock()
	u.requests = append(u.requests, req)
	n := len(u.requests)
	h := u.Handler
	u.mu.Unlock()
	if h == nil {
		writeJSON(w, 200, B64Response(1))
		return
	}
	status, body := h(n, req)
	writeJSON(w, status, body)
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
