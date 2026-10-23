package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
)

// The bug this guards: on an image-edit turn the user message carries the input picture
// in `urls`, and the first implementation returned it as the result — a "successful"
// edit whose output was byte-identical to the input.
func TestPickRepairURLsIgnoresInputImage(t *testing.T) {
	const page = `{
      "items": [
        {"messages": {"": [
          {"msg_id":"u1","ref_id":"","fuid":"me","content":"make it snowy","status":"complete",
           "content_type":"image_url","urls":["https://cdn.example/INPUT.png"],
           "draw_content":{"url":"","thumb_url":""}}
        ]}},
        {"messages": {"u1": [
          {"msg_id":"a1","ref_id":"u1","fuid":"ai","content":"[{\"url\":\"https://cdn.example/OUT.png\"}]",
           "status":"complete","content_type":"image_url","urls":[],
           "draw_content":{"url":"https://static.zhijuai.com/ai/OUT.png","thumb_url":""}}
        ]}}
      ]
    }`
	var p repairPage
	if err := json.Unmarshal([]byte(page), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := pickRepairURLs(p)
	if len(got) != 1 || got[0] != "https://static.zhijuai.com/ai/OUT.png" {
		t.Fatalf("want the result url only, got %v", got)
	}
}

// A plain-text turn (no picture yet) must not report the prompt as an image.
func TestPickRepairURLsTextOnlyTurn(t *testing.T) {
	const page = `{"items":[{"messages":{"":[{"msg_id":"u1","status":"complete","content":"a cat","urls":[]}]}}]}`
	var p repairPage
	if err := json.Unmarshal([]byte(page), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := pickRepairURLs(p); len(got) != 0 {
		t.Fatalf("want no urls, got %v", got)
	}
}

// Fallback: a reply message with urls but no draw_content still counts.
func TestPickRepairURLsFallbackOnReply(t *testing.T) {
	const page = `{"items":[{"messages":{"u1":[{"msg_id":"a1","ref_id":"u1","urls":["https://cdn.example/R.png"]}]}}]}`
	var p repairPage
	if err := json.Unmarshal([]byte(page), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := pickRepairURLs(p)
	if len(got) != 1 || got[0] != "https://cdn.example/R.png" {
		t.Fatalf("want the reply url, got %v", got)
	}
}

// The canvas app attaches three reference images to one edit and numbers them in the prompt
// ("Uploaded Image 1/2/3"). The handler used to read only r.FormFile("image"), so the other
// two were dropped and the model got references to pictures it never received.
func TestEditFilesCollectsEveryImage(t *testing.T) {
	// Same picture twice under different names, plus a distinct second picture: the first
	// must be de-duplicated, the second must survive.
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	png := []byte("\x89PNG\r\n\x1a\n") // enough for sniffing; content is not decoded here
	jpg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF")
	writePart := func(name, filename string, data []byte) {
		fw, _ := w.CreateFormFile(name, filename)
		fw.Write(data)
	}
	writePart("image", "a.png", png)
	writePart("image[]", "b.png", png) // same bytes: a duplicate, not a second image
	writePart("image[]", "c.jpg", jpg)
	w.Close()

	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := editFiles(r)
	if len(got) != 2 {
		t.Fatalf("want 2 unique images, got %d", len(got))
	}
	// CreateFormFile always sets a Content-Type, so the sniffing fallback is exercised by
	// writing the part header directly instead — otherwise the header wins and the sniff
	// branch never runs.
	if len(got) != 2 {
		t.Fatalf("want 2 unique images, got %d", len(got))
	}
	if got[0].contentType != "application/octet-stream" {
		t.Errorf("content type = %q", got[0].contentType)
	}

	// Same body, no Content-Type: sniffImageType must identify it from the bytes.
	raw := &bytes.Buffer{}
	w2 := multipart.NewWriter(raw)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="image[]"; filename="c.jpg"`)
	part, err := w2.CreatePart(hdr)
	if err != nil {
		t.Fatalf("part: %v", err)
	}
	part.Write(jpg)
	w2.Close()
	r2 := httptest.NewRequest(http.MethodPost, "/v1/images/edits", raw)
	r2.Header.Set("Content-Type", w2.FormDataContentType())
	if err := r2.ParseMultipartForm(8 << 20); err != nil {
		t.Fatalf("parse2: %v", err)
	}
	got2 := editFiles(r2)
	if len(got2) != 1 || got2[0].contentType != "image/jpeg" {
		t.Errorf("sniffed content type = %q (n=%d)", got2[0].contentType, len(got2))
	}
}

// A request with no image at all must be rejected with a clear message rather than failing
// later inside the upload.
func TestEditFilesRejectsEmpty(t *testing.T) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	w.WriteField("model", "seedream4-image-edit")
	w.WriteField("prompt", "x")
	w.Close()
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := editFiles(r); len(got) != 0 {
		t.Fatalf("want no files, got %d", len(got))
	}
}

func TestTerminalRepairState(t *testing.T) {
	const page = `{"items":[{"messages":{"":[{"msg_id":"u1","status":"complete","content":"a dog"}],
      "u1":[{"msg_id":"a1","ref_id":"u1","status":"failure","content":"图片生成失败"}]}}]}`
	var p repairPage
	if err := json.Unmarshal([]byte(page), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	status, text := terminalRepairState(p)
	if status != "failure" || text != "图片生成失败" {
		t.Fatalf("want failure/图片生成失败, got %q/%q", status, text)
	}
}

func TestPickDrawURLsSkipsUserTurn(t *testing.T) {
	// Midjourney's urls are objects with dimensions; the repair family uses plain
	// strings. Both must decode, and the user's own turn must be skipped.
	const page = `{"items":[
      {"message_id":"u1","is_user_message":true,"waiting":false,"status":1,"content":{"urls":["https://cdn.example/ECHO.png"]}},
      {"message_id":"a1","is_user_message":false,"waiting":false,"status":3,
       "content":{"urls":[{"url":"https://cdn.example/MJ.png","thumb_url":"https://cdn.example/MJ.thumb.png","width":1024,"height":772},
                          "https://cdn.example/MJ2.png"]}}
    ]}`
	var p drawPage
	if err := json.Unmarshal([]byte(page), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := pickDrawURLs(p)
	if len(got) != 2 || got[0] != "https://cdn.example/MJ.png" || got[1] != "https://cdn.example/MJ2.png" {
		t.Fatalf("want the two assistant urls, got %v", got)
	}
}

// The object shape carries dimensions; decoding must not lose them, because the poll
// loop relies on the whole page decoding successfully.
func TestImageURLShapes(t *testing.T) {
	var list []ImageURL
	if err := json.Unmarshal([]byte(`["https://a/1.png",{"url":"https://a/2.png","width":1024,"height":772}]`), &list); err != nil {
		t.Fatalf("unmarshal mixed shapes: %v", err)
	}
	if len(list) != 2 || list[0].URL != "https://a/1.png" {
		t.Fatalf("string form lost: %+v", list)
	}
	if list[1].Width != 1024 || list[1].Height != 772 {
		t.Fatalf("object form lost its dimensions: %+v", list[1])
	}
	if got := urlsOf(list); len(got) != 2 {
		t.Fatalf("urlsOf = %v", got)
	}
}

func TestRatioFromSize(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1:1",
		"1024x1536": "2:3",
		"1536x1024": "3:2",
		"1792x1024": "7:4",
		"":          "",
		"auto":      "",
		"4:3":       "4:3",
		"garbage":   "",
	}
	for in, want := range cases {
		if got := ratioFromSize(in); got != want {
			t.Errorf("ratioFromSize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSniffImageType(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	jpg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 16)...)
	if got := sniffImageType(png, "x.bin"); got != "image/png" {
		t.Errorf("png sniff = %q", got)
	}
	if got := sniffImageType(jpg, "x.bin"); got != "image/jpeg" {
		t.Errorf("jpeg sniff = %q", got)
	}
	if got := sniffImageType([]byte("nonsense"), "photo.WEBP"); got != "image/webp" {
		t.Errorf("extension fallback = %q", got)
	}
}
