package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ── needsMultipartConvert ──

func TestNeedsMultipartConvert_MultipartImageEdits(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
	if !needsMultipartConvert(r) {
		t.Error("expected true for multipart /images/edits")
	}
}

func TestNeedsMultipartConvert_MultipartImageVariations(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/variations", nil)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
	if !needsMultipartConvert(r) {
		t.Error("expected true for multipart /images/variations")
	}
}

func TestNeedsMultipartConvert_JSONBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	r.Header.Set("Content-Type", "application/json")
	if needsMultipartConvert(r) {
		t.Error("expected false for JSON content-type")
	}
}

func TestNeedsMultipartConvert_MultipartNonImageEndpoint(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
	if needsMultipartConvert(r) {
		t.Error("expected false for multipart on non-image endpoint")
	}
}

func TestNeedsMultipartConvert_NilRequest(t *testing.T) {
	if needsMultipartConvert(nil) {
		t.Error("expected false for nil request")
	}
}

func TestNeedsMultipartConvert_MultipartImageGenerations(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
	if needsMultipartConvert(r) {
		t.Error("expected false for multipart /images/generations (not in suffix list)")
	}
}

// ── convertMultipartToJSON ──

// buildMultipartBody 构造 multipart/form-data 测试 body。
func buildMultipartBody(t *testing.T, fields map[string]string, files map[string]struct {
	filename string
	content  []byte
	ct       string
}) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("WriteField %s: %v", k, err)
		}
	}
	for fieldName, file := range files {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{
			fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, file.filename),
		}
		if file.ct != "" {
			h["Content-Type"] = []string{file.ct}
		}
		part, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("CreatePart %s: %v", fieldName, err)
		}
		if _, err := io.Copy(part, bytes.NewReader(file.content)); err != nil {
			t.Fatalf("Copy %s: %v", fieldName, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func TestConvertMultipartToJSON_SingleFile(t *testing.T) {
	imgData := []byte("fake-png-data")
	body, ct := buildMultipartBody(t,
		map[string]string{
			"model":  "gpt-image-1.5",
			"prompt": "make it blue",
			"size":   "1024x1024",
			"n":      "1",
		},
		map[string]struct {
			filename string
			content  []byte
			ct       string
		}{
			"image": {filename: "input.png", content: imgData, ct: "image/png"},
		},
	)

	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	r.Header.Set("Content-Type", ct)

	jsonBody, newCT, err := convertMultipartToJSON(r, body.Bytes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newCT != "application/json" {
		t.Errorf("expected application/json, got %s", newCT)
	}

	var payload map[string]any
	if err := json.Unmarshal(jsonBody, &payload); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if payload["model"] != "gpt-image-1.5" {
		t.Errorf("model = %v, want gpt-image-1.5", payload["model"])
	}
	if payload["prompt"] != "make it blue" {
		t.Errorf("prompt = %v", payload["prompt"])
	}
	if payload["size"] != "1024x1024" {
		t.Errorf("size = %v", payload["size"])
	}
	// "n" should be numeric
	if n, ok := payload["n"].(float64); !ok || n != 1 {
		t.Errorf("n = %v (type %T), want 1 (float64)", payload["n"], payload["n"])
	}

	// Check image field — 单文件应输出为 data URI 字符串
	imageStr, ok := payload["image"].(string)
	if !ok {
		t.Fatalf("image should be string (data URI), got %T %v", payload["image"], payload["image"])
	}
	expectedB64 := base64.StdEncoding.EncodeToString(imgData)
	expectedDataURI := "data:image/png;base64," + expectedB64
	if imageStr != expectedDataURI {
		t.Errorf("image data URI mismatch, got len=%d", len(imageStr))
	}
}

func TestConvertMultipartToJSON_MultipleFiles(t *testing.T) {
	// 构造包含多个同名文件字段的 multipart body
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	_ = w.WriteField("model", "gpt-image-1.5")
	_ = w.WriteField("prompt", "combine")

	for i, data := range [][]byte{[]byte("file1"), []byte("file2"), []byte("file3")} {
		h := map[string][]string{
			"Content-Disposition": {fmt.Sprintf(`form-data; name="image"; filename="img%d.png"`, i)},
			"Content-Type":        {"image/png"},
		}
		part, _ := w.CreatePart(h)
		_, _ = part.Write(data)
	}
	_ = w.Close()

	ct := w.FormDataContentType()
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &buf)
	r.Header.Set("Content-Type", ct)

	jsonBody, _, err := convertMultipartToJSON(r, buf.Bytes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload map[string]any
	_ = json.Unmarshal(jsonBody, &payload)

	imageArr, ok := payload["image"].([]any)
	if !ok {
		t.Fatalf("image should be array for multi-file, got %T", payload["image"])
	}
	if len(imageArr) != 3 {
		t.Errorf("expected 3 image entries, got %d", len(imageArr))
	}
	// 每项应为 data URI 字符串
	for i, item := range imageArr {
		s, ok := item.(string)
		if !ok {
			t.Errorf("image[%d] should be string, got %T", i, item)
			continue
		}
		if len(s) < 22 { // "data:image/png;base64," 最短前缀
			t.Errorf("image[%d] data URI too short: %s", i, s)
		}
	}
}

func TestConvertMultipartToJSON_EmptyBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=abc")

	_, _, err := convertMultipartToJSON(r, nil)
	if err == nil {
		t.Error("expected error for empty body")
	}
}

func TestConvertMultipartToJSON_MissingBoundary(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	r.Header.Set("Content-Type", "multipart/form-data")

	_, _, err := convertMultipartToJSON(r, []byte("some-data"))
	if err == nil {
		t.Error("expected error for missing boundary")
	}
}

func TestConvertMultipartToJSON_FileMimeFromExtension(t *testing.T) {
	imgData := []byte("jpeg-data")
	body, ct := buildMultipartBody(t,
		map[string]string{"model": "gpt-image-1.5", "prompt": "test"},
		map[string]struct {
			filename string
			content  []byte
			ct       string
		}{
			"image": {filename: "photo.jpg", content: imgData, ct: ""}, // no Content-Type header
		},
	)

	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	r.Header.Set("Content-Type", ct)

	jsonBody, _, err := convertMultipartToJSON(r, body.Bytes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload map[string]any
	_ = json.Unmarshal(jsonBody, &payload)

	// 单文件应输出为 data URI 字符串
	imageStr, ok := payload["image"].(string)
	if !ok {
		t.Fatalf("image should be string (data URI), got %T", payload["image"])
	}
	if imageStr[:16] != "data:image/jpeg;" {
		t.Errorf("expected data:image/jpeg; prefix from .jpg extension, got %s", imageStr[:30])
	}
}

// ── detectMimeType ──

func TestDetectMimeType(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		{"input.png", "image/png"},
		{"input.PNG", "image/png"},
		{"photo.jpg", "image/jpeg"},
		{"photo.jpeg", "image/jpeg"},
		{"anim.gif", "image/gif"},
		{"img.webp", "image/webp"},
		{"unknown.bmp", "application/octet-stream"},
	}
	for _, tc := range cases {
		got := detectMimeType(tc.filename)
		if got != tc.want {
			t.Errorf("detectMimeType(%q) = %q, want %q", tc.filename, got, tc.want)
		}
	}
}
