package storage

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"

	"golang.org/x/image/draw"
)

// Decode decodes PNG, JPEG, GIF or WebP data.
func Decode(data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("无法解码图片: %w", err)
	}
	return img, nil
}

// DecodeFile reads and decodes an image file.
func DecodeFile(path string) (image.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// Fit scales img down (never up) so its longer side is at most maxPx, using
// Catmull-Rom resampling. The result is an *image.RGBA, which stores
// alpha-premultiplied pixels.
func Fit(img image.Image, maxPx int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if maxPx > 0 && (w > maxPx || h > maxPx) {
		if w >= h {
			h = max(1, h*maxPx/w)
			w = maxPx
		} else {
			w = max(1, w*maxPx/h)
			h = maxPx
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	} else {
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	}
	return dst
}

// Preview makes a small JPEG (longer side ≤ maxPx, quality 80) for the MCP
// ImageContent preview, flattening transparency onto white.
//
// data is the encoded source image. It returns the JPEG bytes.
func Preview(data []byte, maxPx int) ([]byte, error) {
	img, err := Decode(data)
	if err != nil {
		return nil, err
	}
	small := Fit(img, maxPx)
	flat := image.NewRGBA(small.Bounds())
	draw.Draw(flat, flat.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), small, image.Point{}, draw.Over)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
