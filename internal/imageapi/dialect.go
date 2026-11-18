package imageapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
)

// openaiGenerateBody builds the JSON body for the openai dialect's
// /images/generations.
//
// Merge order (later wins): provider extra_body, model extra, the explicit
// options that were set, then the call's own extra. response_format is only
// sent when the caller set it, because gpt-image models reject it.
func openaiGenerateBody(t Target, p GenerateParams) map[string]any {
	body := map[string]any{}
	merge(body, t.ExtraBody)
	merge(body, t.ModelExtra)
	body["model"] = t.Model
	body["prompt"] = p.Prompt
	setInt(body, "n", p.N)
	setStr(body, "size", p.Size)
	setStr(body, "quality", p.Quality)
	setStr(body, "style", p.Style)
	setStr(body, "response_format", p.ResponseFormat)
	setStr(body, "output_format", p.OutputFormat)
	setStr(body, "background", p.Background)
	setStr(body, "negative_prompt", p.NegativePrompt)
	if p.Seed != nil {
		body["seed"] = *p.Seed
	}
	merge(body, p.Extra)
	return body
}

// siliconflowGenerateBody builds SiliconFlow's /images/generations body:
// size→image_size, n→batch_size. quality/style have no equivalent and are
// dropped; anything else can still be passed through extra.
func siliconflowGenerateBody(t Target, p GenerateParams) map[string]any {
	body := map[string]any{}
	merge(body, t.ExtraBody)
	merge(body, t.ModelExtra)
	body["model"] = t.Model
	body["prompt"] = p.Prompt
	if p.Size != "" && p.Size != "auto" {
		body["image_size"] = p.Size
	}
	setInt(body, "batch_size", p.N)
	setStr(body, "negative_prompt", p.NegativePrompt)
	if p.Seed != nil {
		body["seed"] = *p.Seed
	}
	merge(body, p.Extra)
	return body
}

// siliconflowEditBody is the generation body plus reference images as data
// URLs: image, image2, image3 (the multi-image Qwen edit models read all three).
func siliconflowEditBody(t Target, p EditParams) map[string]any {
	body := siliconflowGenerateBody(t, p.GenerateParams)
	for i, img := range p.Images {
		if i >= 3 {
			break
		}
		key := "image"
		if i > 0 {
			key = fmt.Sprintf("image%d", i+1)
		}
		if _, set := p.Extra[key]; !set {
			body[key] = DataURL(img)
		}
	}
	if p.Mask != nil {
		body["mask"] = DataURL(*p.Mask)
	}
	return body
}

// openaiEditMultipart builds the multipart body for /images/edits. One
// reference image goes in the "image" field; several go in repeated "image[]"
// fields as gpt-image models expect. Each file part carries its real MIME
// type, since OpenAI rejects application/octet-stream.
//
// It returns the body, its Content-Type, and a short text summary of the
// fields for the debug view (file contents replaced by sizes).
func openaiEditMultipart(t Target, p EditParams) ([]byte, string, string, error) {
	fields := openaiGenerateBody(t, p.GenerateParams)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	var summary []string

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := formValue(fields[k])
		if err := w.WriteField(k, v); err != nil {
			return nil, "", "", err
		}
		summary = append(summary, fmt.Sprintf("%s=%s", k, truncate(v, 200)))
	}

	field := "image"
	if len(p.Images) > 1 {
		field = "image[]"
	}
	for i, img := range p.Images {
		name := img.Name
		if name == "" {
			name = fmt.Sprintf("image-%d%s", i+1, extFor(img.MIME))
		}
		if err := writeFile(w, field, name, img); err != nil {
			return nil, "", "", err
		}
		summary = append(summary, fmt.Sprintf("%s=<%s %s %dKB>", field, name, img.MIME, len(img.Data)/1024))
	}
	if p.Mask != nil {
		name := p.Mask.Name
		if name == "" {
			name = "mask" + extFor(p.Mask.MIME)
		}
		if err := writeFile(w, "mask", name, *p.Mask); err != nil {
			return nil, "", "", err
		}
		summary = append(summary, fmt.Sprintf("mask=<%s %dKB>", name, len(p.Mask.Data)/1024))
	}
	if err := w.Close(); err != nil {
		return nil, "", "", err
	}
	return buf.Bytes(), w.FormDataContentType(), "multipart/form-data\n" + strings.Join(summary, "\n"), nil
}

// writeFile adds one file part with an explicit Content-Type header.
func writeFile(w *multipart.Writer, field, name string, img InputImage) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escapeQuotes(field), escapeQuotes(name)))
	mime := img.MIME
	if mime == "" {
		mime = "image/png"
	}
	h.Set("Content-Type", mime)
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = part.Write(img.Data)
	return err
}

// formValue renders a JSON-like value as a multipart text field: strings as-is,
// numbers and booleans in their literal form, everything else as JSON.
func formValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// DataURL encodes an image as data:<mime>;base64,<data>.
func DataURL(img InputImage) string {
	mime := img.MIME
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

// extFor maps a MIME type to a file extension.
func extFor(mime string) string {
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

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

// escapeQuotes escapes a multipart header parameter value.
func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }

// merge copies src's entries into dst, overwriting existing keys.
func merge(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

// setStr sets key when v is not empty.
func setStr(m map[string]any, key, v string) {
	if v = strings.TrimSpace(v); v != "" {
		m[key] = v
	}
}

// setInt sets key when v is positive.
func setInt(m map[string]any, key string, v int) {
	if v > 0 {
		m[key] = v
	}
}
