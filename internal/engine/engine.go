// Package engine is the business core shared by the MCP tools and the GUI's
// debug console: it validates a request, routes it to providers with retry and
// fallback, downloads and saves the images, and records history.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/imageapi"
	"github.com/972579/open-image-mcp/internal/provider"
	"github.com/972579/open-image-mcp/internal/storage"
)

// Engine serves image requests from the current effective configuration. It is
// safe for concurrent use; Reload swaps the configuration atomically so calls
// already running finish with the configuration they started with.
type Engine struct {
	cfg   atomic.Pointer[config.Config]
	api   *imageapi.Client
	store *storage.Store
	log   *slog.Logger
	// sleep waits between retries; tests replace it to run instantly.
	sleep func(ctx context.Context, d time.Duration) error
}

// New returns an Engine serving cfg, calling upstreams through api and saving
// through store. log may be nil.
func New(cfg *config.Config, api *imageapi.Client, store *storage.Store, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	e := &Engine{api: api, store: store, log: log, sleep: sleepCtx}
	e.Reload(cfg)
	return e
}

// Config returns the configuration currently in use. Callers must not modify it.
func (e *Engine) Config() *config.Config { return e.cfg.Load() }

// Reload switches to a new effective configuration. Running calls keep the old
// one; the next call uses the new one. No restart of the MCP server is needed.
//
// cfg is stored as a private clone.
func (e *Engine) Reload(cfg *config.Config) {
	c := cfg.Clone()
	e.cfg.Store(&c)
}

// Store returns the image store, for the history listing.
func (e *Engine) Store() *storage.Store { return e.store }

// Client returns the upstream client, for connection tests.
func (e *Engine) Client() *imageapi.Client { return e.api }

// GenerateRequest is a text-to-image call. Empty fields take the configured
// defaults.
type GenerateRequest struct {
	Prompt         string
	Model          string
	Provider       string
	Size           string
	N              int
	Quality        string
	Style          string
	Seed           *int64
	ResponseFormat string
	OutputFormat   string
	Background     string
	NegativePrompt string
	OutputDir      string
	FilenamePrefix string
	Extra          map[string]any
	// Source is recorded in history: "mcp" or "debug".
	Source string
}

// EditRequest is an image-to-image call.
type EditRequest struct {
	GenerateRequest
	// Images are absolute file paths, data URLs or bare base64 strings.
	Images []string
	// Mask is optional, in the same forms as Images.
	Mask string
}

// Attempt records one try against one provider.
type Attempt struct {
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Error    string        `json:"error,omitempty"`
	Elapsed  time.Duration `json:"-"`
}

// Result is a successful call.
type Result struct {
	Images        []storage.Saved
	Model         string
	Provider      string
	RevisedPrompt string
	Seed          *int64
	Elapsed       time.Duration
	Attempts      []Attempt
	// Debug is the HTTP exchange of the successful attempt.
	Debug imageapi.Debug
}

// Progress receives human-readable status updates during a call. It may be nil.
type Progress func(msg string)

// Failure wraps the final error of a call together with every attempt and the
// last HTTP exchange, so the debug console can show what was sent.
type Failure struct {
	Err      error
	Attempts []Attempt
	Debug    imageapi.Debug
}

// Error returns the final error's message.
func (f *Failure) Error() string { return f.Err.Error() }

// Unwrap returns the final error.
func (f *Failure) Unwrap() error { return f.Err }

// Generate runs a text-to-image call: validate, route, call with retry and
// fallback, download URL results, save files and append history.
//
// ctx cancels the whole call; req holds the parameters; progress receives
// status updates. On failure the error is a *Failure wrapping an
// *imageapi.Error (or a validation error).
func (e *Engine) Generate(ctx context.Context, req GenerateRequest, progress Progress) (*Result, error) {
	cfg := e.Config()
	params, dir, prefix, err := e.prepare(cfg, &req)
	if err != nil {
		return nil, err
	}
	cands, err := provider.Resolve(cfg, req.Model, req.Provider, provider.Generate)
	if err != nil {
		return nil, err
	}
	call := func(ctx context.Context, t imageapi.Target) (*imageapi.Response, imageapi.Debug, error) {
		return e.api.Generate(ctx, t, params)
	}
	return e.run(ctx, cfg, cands, call, dir, prefix, storage.Meta{Kind: "generate", Source: req.Source, Prompt: req.Prompt, Size: params.Size}, progress)
}

// Edit runs an image-to-image call. Reference images and the mask are loaded
// from paths or decoded from base64 before routing.
//
// ctx cancels the whole call; req holds the parameters and images; progress
// receives status updates.
func (e *Engine) Edit(ctx context.Context, req EditRequest, progress Progress) (*Result, error) {
	cfg := e.Config()
	params, dir, prefix, err := e.prepare(cfg, &req.GenerateRequest)
	if err != nil {
		return nil, err
	}
	if len(req.Images) == 0 {
		return nil, badRequest("images 至少需要一张参考图（绝对路径或 base64）")
	}
	if len(req.Images) > 16 {
		return nil, badRequest("images 最多 16 张")
	}
	edit := imageapi.EditParams{GenerateParams: params}
	for i, src := range req.Images {
		img, err := LoadInputImage(src, fmt.Sprintf("image-%d", i+1))
		if err != nil {
			return nil, err
		}
		edit.Images = append(edit.Images, img)
	}
	if strings.TrimSpace(req.Mask) != "" {
		m, err := LoadInputImage(req.Mask, "mask")
		if err != nil {
			return nil, err
		}
		edit.Mask = &m
	}
	cands, err := provider.Resolve(cfg, req.Model, req.Provider, provider.Edit)
	if err != nil {
		return nil, err
	}
	call := func(ctx context.Context, t imageapi.Target) (*imageapi.Response, imageapi.Debug, error) {
		return e.api.Edit(ctx, t, edit)
	}
	return e.run(ctx, cfg, cands, call, dir, prefix, storage.Meta{Kind: "edit", Source: req.Source, Prompt: req.Prompt, Size: params.Size}, progress)
}

// Models lists configured models, optionally for one provider and capability
// ("generate", "edit" or "").
func (e *Engine) Models(providerName, capability string) []provider.ModelInfo {
	return provider.List(e.Config(), providerName, provider.Capability(capability))
}

// History lists saved images newest first.
func (e *Engine) History(q storage.Query) (int, []storage.Record, error) {
	return e.store.List(q)
}

// DeleteImage removes one image from the gallery: its history record and the
// gallery's file. A copy saved to an MCP output_dir elsewhere is kept.
//
// id is the history record's ID. It returns the deleted record, or
// storage.ErrNotFound or a file error (see storage.Store.Delete).
func (e *Engine) DeleteImage(id string) (storage.Record, error) {
	rec, err := e.store.Delete(id)
	if err == nil {
		e.log.Info("image deleted from gallery", "path", rec.Path)
	}
	return rec, err
}

// prepare fills defaults into req, validates it and builds the upstream
// parameters. It returns the parameters, the absolute output directory and the
// file name prefix.
func (e *Engine) prepare(cfg *config.Config, req *GenerateRequest) (imageapi.GenerateParams, string, string, error) {
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return imageapi.GenerateParams{}, "", "", badRequest("prompt 不能为空")
	}
	if req.N == 0 {
		req.N = cfg.Defaults.N
	}
	if req.N < 1 || req.N > 10 {
		return imageapi.GenerateParams{}, "", "", badRequest("n 必须在 1-10 之间")
	}
	if strings.TrimSpace(req.Size) == "" {
		req.Size = cfg.Defaults.Size
	}
	if req.Source == "" {
		req.Source = "mcp"
	}
	dir := strings.TrimSpace(req.OutputDir)
	if dir == "" {
		dir = cfg.Defaults.OutputDir
	}
	dir = config.ExpandHome(dir)
	if !filepath.IsAbs(dir) {
		return imageapi.GenerateParams{}, "", "", badRequest(fmt.Sprintf("output_dir 必须是绝对路径: %q", req.OutputDir))
	}
	prefix := strings.TrimSpace(req.FilenamePrefix)
	if prefix == "" {
		prefix = cfg.Defaults.FilenamePrefix
	}
	return imageapi.GenerateParams{
		Prompt: req.Prompt, Size: strings.TrimSpace(req.Size), N: req.N,
		Quality: req.Quality, Style: req.Style, Seed: req.Seed,
		ResponseFormat: req.ResponseFormat, OutputFormat: req.OutputFormat,
		Background: req.Background, NegativePrompt: req.NegativePrompt, Extra: req.Extra,
	}, filepath.Clean(dir), prefix, nil
}

// callFunc performs one upstream request against a target.
type callFunc func(ctx context.Context, t imageapi.Target) (*imageapi.Response, imageapi.Debug, error)

// run tries each candidate in order. Retryable errors (network, timeout, 429,
// 5xx) are retried on the same provider up to request.max_retries times with
// exponential backoff; then, if the error allows fallback, the next candidate
// is tried. The first success is downloaded, saved and returned.
func (e *Engine) run(ctx context.Context, cfg *config.Config, cands []provider.Candidate, call callFunc, dir, prefix string, meta storage.Meta, progress Progress) (*Result, error) {
	report := func(format string, args ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, args...))
		}
	}
	start := time.Now()
	var attempts []Attempt
	var lastErr error
	var lastDebug imageapi.Debug
	timeout := cfg.Request.Timeout.Std()

	for ci, cand := range cands {
		target := cand.Target()
		for try := 0; try <= cfg.Request.MaxRetries; try++ {
			if try > 0 {
				wait := time.Duration(1<<(try-1)) * time.Second
				report("%s 第 %d 次重试，%v 后开始", cand.Provider.Name, try, wait)
				if err := e.sleep(ctx, wait); err != nil {
					return nil, &Failure{Err: classifyCtx(err), Attempts: attempts, Debug: lastDebug}
				}
			}
			report("正在请求 %s / %s", cand.Provider.Name, cand.Model.ID)
			attemptStart := time.Now()
			actx, cancel := context.WithTimeout(ctx, timeout)
			resp, dbg, err := call(actx, target)
			cancel()
			lastDebug = dbg
			att := Attempt{Provider: cand.Provider.Name, Model: cand.Model.ID, Elapsed: time.Since(attemptStart)}
			if err == nil {
				attempts = append(attempts, att)
				report("上游已返回 %d 张图片，正在保存", len(resp.Images))
				res, serr := e.finish(ctx, cand, target, resp, dir, galleryDir(cfg), prefix, meta)
				if serr != nil {
					return nil, &Failure{Err: serr, Attempts: attempts, Debug: dbg}
				}
				res.Attempts, res.Debug, res.Elapsed = attempts, dbg, time.Since(start)
				e.log.Info("image call succeeded", "kind", meta.Kind, "provider", cand.Provider.Name, "model", cand.Model.ID, "images", len(res.Images), "elapsed", res.Elapsed.Round(time.Millisecond))
				return res, nil
			}
			if ctx.Err() != nil {
				err = classifyCtx(ctx.Err())
			}
			att.Error = err.Error()
			attempts = append(attempts, att)
			lastErr = err
			e.log.Warn("image call failed", "kind", meta.Kind, "provider", cand.Provider.Name, "model", cand.Model.ID, "error", err.Error())
			apiErr, ok := imageapi.AsError(err)
			if !ok || apiErr.Kind == imageapi.KindCanceled {
				return nil, &Failure{Err: err, Attempts: attempts, Debug: lastDebug}
			}
			if !apiErr.Retryable() {
				break
			}
		}
		apiErr, _ := imageapi.AsError(lastErr)
		if apiErr == nil || !apiErr.Fallbackable() {
			break
		}
		if ci+1 < len(cands) {
			report("%s 失败，切换到 %s", cand.Provider.Name, cands[ci+1].Provider.Name)
		}
	}
	if lastErr == nil {
		lastErr = &imageapi.Error{Kind: imageapi.KindConfig, Message: "没有可用的服务商"}
	}
	if len(attempts) > 1 {
		lastErr = fmt.Errorf("%w（共尝试 %d 次，详情见 attempts）", lastErr, len(attempts))
	}
	return nil, &Failure{Err: lastErr, Attempts: attempts, Debug: lastDebug}
}

// galleryDir is the gallery's directory: the configured default output
// directory, absolute and cleaned, or "" when it is not usable.
//
// cfg is the configuration the call runs with.
func galleryDir(cfg *config.Config) string {
	d := config.ExpandHome(strings.TrimSpace(cfg.Defaults.OutputDir))
	if !filepath.IsAbs(d) {
		return ""
	}
	return filepath.Clean(d)
}

// finish downloads URL images, saves everything and builds the result. Images
// are saved in dir and, when dir is not the gallery directory, also copied to
// gallery so the gallery keeps the full history; a copy that cannot be
// written is logged and the call still succeeds.
//
// ctx bounds the downloads; cand and target are the provider that answered;
// resp is its response; dir is the requested output directory; gallery is
// the gallery directory ("" for no copy); prefix and meta name and describe
// the files. It returns the result or the first download or save error.
func (e *Engine) finish(ctx context.Context, cand provider.Candidate, target imageapi.Target, resp *imageapi.Response, dir, gallery, prefix string, meta storage.Meta) (*Result, error) {
	blobs := make([]storage.Blob, 0, len(resp.Images))
	revised := ""
	for i, img := range resp.Images {
		data := img.Data
		if len(data) == 0 && img.URL != "" {
			dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			d, err := e.api.Download(dctx, target, img.URL)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("下载第 %d 张图片失败: %w", i+1, err)
			}
			data = d
		}
		if revised == "" {
			revised = img.RevisedPrompt
		}
		blobs = append(blobs, storage.Blob{Data: data, SourceURL: img.URL, RevisedPrompt: img.RevisedPrompt})
	}
	meta.Model, meta.Provider = cand.Model.ID, cand.Provider.Name
	saved, err := e.store.Save(blobs, dir, gallery, prefix, meta)
	if err != nil {
		return nil, err
	}
	if gallery != "" && gallery != dir {
		for _, s := range saved {
			if s.GalleryPath == "" {
				e.log.Warn("gallery copy not written; the gallery shows the output file", "path", s.Path, "gallery", gallery)
			}
		}
	}
	return &Result{Images: saved, Model: cand.Model.ID, Provider: cand.Provider.Name, RevisedPrompt: revised, Seed: resp.Seed}, nil
}

// LoadInputImage reads a reference image from an absolute path, a data URL or
// bare base64, and sniffs its MIME type. Files larger than 50 MiB are refused.
//
// src is the value given by the caller; name is the fallback upload file name
// (without extension).
func LoadInputImage(src, name string) (imageapi.InputImage, error) {
	src = strings.TrimSpace(src)
	var data []byte
	fileName := ""
	switch {
	case strings.HasPrefix(src, "data:"):
		d, err := imageapi.DecodeBase64(src)
		if err != nil {
			return imageapi.InputImage{}, badRequest(fmt.Sprintf("%s 的 data URL 无法解码: %v", name, err))
		}
		data = d
	case filepath.IsAbs(config.ExpandHome(src)):
		path := config.ExpandHome(src)
		info, err := os.Stat(path)
		if err != nil {
			return imageapi.InputImage{}, badRequest(fmt.Sprintf("读取参考图 %s 失败: %v", path, err))
		}
		if info.Size() > 50<<20 {
			return imageapi.InputImage{}, badRequest(fmt.Sprintf("参考图 %s 超过 50MB", path))
		}
		d, err := os.ReadFile(path)
		if err != nil {
			return imageapi.InputImage{}, badRequest(fmt.Sprintf("读取参考图 %s 失败: %v", path, err))
		}
		data, fileName = d, filepath.Base(path)
	default:
		d, err := imageapi.DecodeBase64(src)
		if err != nil {
			return imageapi.InputImage{}, badRequest(fmt.Sprintf("%s 既不是绝对路径也不是有效的 base64", name))
		}
		data = d
	}
	if len(data) == 0 {
		return imageapi.InputImage{}, badRequest(fmt.Sprintf("%s 内容为空", name))
	}
	mime := storage.DetectMIME(data)
	if fileName == "" {
		fileName = name + storage.ExtFor(mime)
	}
	return imageapi.InputImage{Name: fileName, MIME: mime, Data: data}, nil
}

// badRequest builds a parameter-validation error.
func badRequest(msg string) error {
	return &imageapi.Error{Kind: imageapi.KindBadRequest, Message: msg}
}

// classifyCtx turns a context error into an *imageapi.Error.
func classifyCtx(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &imageapi.Error{Kind: imageapi.KindTimeout, Message: "调用超时"}
	}
	return &imageapi.Error{Kind: imageapi.KindCanceled, Message: "调用已取消", Err: err}
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
