package imageapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/972579/open-image-mcp/internal/imageapi"
	"github.com/972579/open-image-mcp/internal/testutil"
)

// target returns an imageapi.Target for the fake upstream.
func target(u *testutil.Upstream, dialect string) imageapi.Target {
	return imageapi.Target{Provider: "fake", Dialect: dialect, BaseURL: u.BaseURL(), APIKey: "sk-test-1234567890", Model: "m1",
		ExtraBody: map[string]any{"from_provider": 1}}
}

// TestGenerateOpenAI checks the openai body and b64 parsing.
func TestGenerateOpenAI(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	c := imageapi.New(nil)
	seed := int64(42)
	resp, dbg, err := c.Generate(context.Background(), target(u, "openai"), imageapi.GenerateParams{
		Prompt: "cat", Size: "1024x1024", N: 2, Quality: "high", Seed: &seed, Extra: map[string]any{"moderation": "low"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 1 || len(resp.Images[0].Data) == 0 || resp.Images[0].RevisedPrompt != "revised" {
		t.Fatalf("unexpected images: %+v", resp.Images)
	}
	r := u.Requests()[0]
	if r.Path != "/v1/images/generations" || r.Auth != "Bearer sk-test-1234567890" {
		t.Errorf("wrong path/auth: %s %s", r.Path, r.Auth)
	}
	for k, want := range map[string]any{"model": "m1", "prompt": "cat", "size": "1024x1024", "n": 2.0, "quality": "high", "seed": 42.0, "moderation": "low", "from_provider": 1.0} {
		if r.JSON[k] != want {
			t.Errorf("body[%s] = %v, want %v", k, r.JSON[k], want)
		}
	}
	if _, ok := r.JSON["response_format"]; ok {
		t.Error("response_format must not be sent unless set")
	}
	if strings.Contains(dbg.ResponseBody, "iVBOR") {
		t.Error("debug response should have base64 redacted")
	}
}

// TestGenerateSiliconFlow checks field renames and the images[] URL shape.
func TestGenerateSiliconFlow(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = func(n int, r testutil.Request) (int, any) {
		return 200, map[string]any{"images": []any{map[string]any{"url": u.URL + "/files/a.png"}}, "seed": 7}
	}
	resp, _, err := imageapi.New(nil).Generate(context.Background(), target(u, "siliconflow"), imageapi.GenerateParams{Prompt: "dog", Size: "1328x1328", N: 1, Quality: "hd"})
	if err != nil {
		t.Fatal(err)
	}
	r := u.Requests()[0]
	if r.JSON["image_size"] != "1328x1328" || r.JSON["batch_size"] != 1.0 || r.JSON["quality"] != nil || r.JSON["size"] != nil {
		t.Errorf("siliconflow body wrong: %v", r.JSON)
	}
	if resp.Images[0].URL == "" || resp.Seed == nil || *resp.Seed != 7 {
		t.Errorf("url/seed not parsed: %+v", resp)
	}
}

// TestEditMultipart checks image[] fields carry real MIME types.
func TestEditMultipart(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	png := testutil.PNG(8, 8)
	_, dbg, err := imageapi.New(nil).Edit(context.Background(), target(u, "openai"), imageapi.EditParams{
		GenerateParams: imageapi.GenerateParams{Prompt: "make it blue", N: 1},
		Images:         []imageapi.InputImage{{Name: "a.png", MIME: "image/png", Data: png}, {MIME: "image/png", Data: png}},
		Mask:           &imageapi.InputImage{MIME: "image/png", Data: png},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := u.Requests()[0]
	if r.Path != "/v1/images/edits" || len(r.Files["image[]"]) != 2 || len(r.Files["mask"]) != 1 {
		t.Fatalf("multipart wrong: path=%s files=%v", r.Path, r.Files)
	}
	if r.Files["image[]"][0].MIME != "image/png" || r.Form["prompt"][0] != "make it blue" || r.Form["model"][0] != "m1" {
		t.Errorf("parts wrong: %+v %+v", r.Files, r.Form)
	}
	if !strings.Contains(dbg.RequestBody, "image[]=<a.png") {
		t.Errorf("debug summary missing file: %s", dbg.RequestBody)
	}
}

// TestErrorClassification checks status/body to Kind mapping.
func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   any
		kind   imageapi.Kind
		retry  bool
	}{
		{401, map[string]any{"error": map[string]any{"message": "Incorrect API key provided: sk-abcd****", "code": "invalid_api_key"}}, imageapi.KindAuth, false},
		{404, map[string]any{"error": map[string]any{"message": "The model `x` does not exist", "code": "model_not_found"}}, imageapi.KindModel, false},
		{503, map[string]any{"error": map[string]any{"message": "当前分组 default 下对于模型 x 无可用渠道"}}, imageapi.KindModel, false},
		{429, map[string]any{"error": "rate limited"}, imageapi.KindRateLimit, true},
		{500, map[string]any{"message": "internal", "code": 50000}, imageapi.KindUpstream, true},
		{400, map[string]any{"error": map[string]any{"message": "Invalid size"}}, imageapi.KindBadRequest, false},
	}
	for _, tc := range cases {
		u := testutil.NewUpstream()
		u.Handler = func(int, testutil.Request) (int, any) { return tc.status, tc.body }
		_, _, err := imageapi.New(nil).Generate(context.Background(), target(u, "openai"), imageapi.GenerateParams{Prompt: "x"})
		u.Close()
		e, ok := imageapi.AsError(err)
		if !ok {
			t.Fatalf("status %d: not an *Error: %v", tc.status, err)
		}
		if e.Kind != tc.kind || e.Retryable() != tc.retry {
			t.Errorf("status %d: kind=%s retry=%v, want %s %v (%v)", tc.status, e.Kind, e.Retryable(), tc.kind, tc.retry, err)
		}
		if strings.Contains(err.Error(), "sk-test-1234567890") {
			t.Errorf("API key leaked in error: %v", err)
		}
	}
}

// TestNoImages checks an empty success body is a KindBadResponse.
func TestNoImages(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	u.Handler = func(int, testutil.Request) (int, any) { return 200, map[string]any{"data": []any{}} }
	_, _, err := imageapi.New(nil).Generate(context.Background(), target(u, "openai"), imageapi.GenerateParams{Prompt: "x"})
	if e, ok := imageapi.AsError(err); !ok || e.Kind != imageapi.KindBadResponse {
		t.Fatalf("want KindBadResponse, got %v", err)
	}
}

// TestRedactText checks key masking in free text.
func TestRedactText(t *testing.T) {
	in := "auth Bearer abcdefghijklmnop key=sk-proj-ABCDEFGHIJKLMNOP1234"
	out := imageapi.RedactText(in)
	if strings.Contains(out, "abcdefghijklmnop") || strings.Contains(out, "EFGHIJKLMNOP") {
		t.Errorf("not redacted: %s", out)
	}
	if !strings.Contains(out, "sk-****1234") {
		t.Errorf("expected masked key with suffix: %s", out)
	}
}

// TestListModels checks GET /models parsing.
func TestListModels(t *testing.T) {
	u := testutil.NewUpstream()
	defer u.Close()
	ids, _, err := imageapi.New(nil).ListModels(context.Background(), target(u, "openai"))
	if err != nil || len(ids) != 2 || ids[0] != "gpt-image-2" {
		t.Fatalf("ListModels = %v, %v", ids, err)
	}
}
