package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// The web client hands an input picture to the image models by uploading it first:
// a one-time QR token is exchanged for a mobile token, that token authorises a direct
// OSS form upload, and the resulting object URL is what the model receives. Doing the
// same server-side is what makes OpenAI-shaped /v1/images/edits possible.
//
//	POST /v2/ai/uploads/qr/create                       -> {qr_payload, upload_session_id}
//	POST /v2/ai/uploads/qr/authorize {qr_token}         -> {mobile_token}
//	POST /v2/ai/uploads/qr/authorize {mobile_token,...} -> per-item {upload_url, fields}
//	POST <upload_url> (multipart form, file last)       -> 2xx
//	POST /v2/ai/uploads/qr/complete  {..., finalize}    -> ok

type qrCreateResp struct {
	QRPayload       string `json:"qr_payload"`
	UploadSessionID string `json:"upload_session_id"`
	ExpiresAt       string `json:"expires_at"`
}

type qrAuthorizeResp struct {
	MobileToken     string `json:"mobile_token"`
	UploadSessionID string `json:"upload_session_id"`
	Items           []struct {
		ClientAttachmentID string            `json:"client_attachment_id"`
		UploadItemID       string            `json:"upload_item_id"`
		UploadURL          string            `json:"upload_url"`
		ObjectURL          string            `json:"object_url"`
		Fields             map[string]string `json:"fields"`
	} `json:"items"`
}

// uploadImage pushes one picture through the QR upload flow and returns its object URL.
func (s *Server) uploadImage(ctx context.Context, data []byte, name, contentType string) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("empty image")
	}
	if name == "" {
		name = "image.png"
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = sniffImageType(data, name)
	}

	var created qrCreateResp
	if err := s.c.call(ctx, kindChat, "/v2/ai/uploads/qr/create", map[string]any{}, &created); err != nil {
		return "", fmt.Errorf("qr/create: %w", err)
	}
	qrToken := ""
	if created.QRPayload != "" {
		if u, err := url.Parse(created.QRPayload); err == nil {
			qrToken = u.Query().Get("qr_token")
		}
	}
	if qrToken == "" {
		return "", fmt.Errorf("qr/create returned no qr_token")
	}

	var exchanged qrAuthorizeResp
	if err := s.c.call(ctx, kindChat, "/v2/ai/uploads/qr/authorize",
		map[string]any{"qr_token": qrToken}, &exchanged); err != nil {
		return "", fmt.Errorf("qr/authorize(qr_token): %w", err)
	}
	if exchanged.MobileToken == "" {
		return "", fmt.Errorf("qr/authorize returned no mobile_token")
	}

	attachmentID := newUUID()
	var authorized qrAuthorizeResp
	if err := s.c.call(ctx, kindChat, "/v2/ai/uploads/qr/authorize", map[string]any{
		"mobile_token": exchanged.MobileToken,
		"items": []map[string]any{{
			"client_attachment_id": attachmentID,
			"name":                 name,
			"mime_type":            contentType,
			"size":                 len(data),
		}},
	}, &authorized); err != nil {
		return "", fmt.Errorf("qr/authorize(items): %w", err)
	}

	var item *struct {
		ClientAttachmentID string            `json:"client_attachment_id"`
		UploadItemID       string            `json:"upload_item_id"`
		UploadURL          string            `json:"upload_url"`
		ObjectURL          string            `json:"object_url"`
		Fields             map[string]string `json:"fields"`
	}
	for i := range authorized.Items {
		if authorized.Items[i].ClientAttachmentID == attachmentID || item == nil {
			item = &authorized.Items[i]
		}
	}
	if item == nil || item.UploadURL == "" || len(item.Fields) == 0 {
		return "", fmt.Errorf("qr/authorize(items) returned no upload target")
	}

	if err := uploadToOSS(ctx, s.c.http, item.UploadURL, item.Fields, data, name, contentType); err != nil {
		return "", fmt.Errorf("oss upload: %w", err)
	}

	if err := s.c.call(ctx, kindChat, "/v2/ai/uploads/qr/complete", map[string]any{
		"mobile_token":    exchanged.MobileToken,
		"upload_item_ids": []string{item.UploadItemID},
		"finalize":        true,
	}, &map[string]any{}); err != nil {
		return "", fmt.Errorf("qr/complete: %w", err)
	}

	if item.ObjectURL == "" {
		// Some responses omit it; the OSS key is the last resort.
		if key := item.Fields["key"]; key != "" {
			if host, err := url.Parse(item.UploadURL); err == nil {
				return fmt.Sprintf("%s://%s/%s", host.Scheme, host.Host, strings.TrimPrefix(key, "/")), nil
			}
		}
		return "", fmt.Errorf("upload finished but the upstream returned no object url")
	}
	return item.ObjectURL, nil
}

// uploadToOSS performs the OSS PostObject form upload the web page does: every signed
// field verbatim, the binary last.
func uploadToOSS(ctx context.Context, client *http.Client, uploadURL string, fields map[string]string, data []byte, name, contentType string) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	// x-oss-security-token must come after the credential fields, so keep the map order
	// stable by writing all ordinary fields first and the security token right after.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k != "x-oss-security-token" && k != "file" {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if err := w.WriteField(k, fields[k]); err != nil {
			return err
		}
	}
	if v, ok := fields["x-oss-security-token"]; ok {
		if err := w.WriteField("x-oss-security-token", v); err != nil {
			return err
		}
	}

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(name)))
	h.Set("Content-Type", contentType)
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", w.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := readMaybeGzip(resp)
		return fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, buf)
	}
	return nil
}

func escapeQuotes(s string) string {
	return strings.NewReplacer(`"`, `\"`, "\r", "", "\n", "").Replace(s)
}

func sniffImageType(data []byte, name string) string {
	switch {
	case len(data) > 8 && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(data) > 3 && data[0] == 0xFF && data[1] == 0xD8:
		return "image/jpeg"
	case len(data) > 12 && bytes.HasPrefix(data, []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case len(data) > 6 && bytes.HasPrefix(data, []byte("GIF8")):
		return "image/gif"
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		switch strings.ToLower(name[i+1:]) {
		case "png":
			return "image/png"
		case "jpg", "jpeg":
			return "image/jpeg"
		case "webp":
			return "image/webp"
		}
	}
	return "image/png"
}

var _ = json.Marshal
