package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---- OpenAI image shapes -----------------------------------------------------------

type imageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	// Non-standard extras this gateway understands: an explicit upstream resolution
	// tier (1K/2K/4K) and an input picture as a URL or data URI.
	ImageSize string `json:"image_size,omitempty"`
	Image     string `json:"image,omitempty"`
}

type imageResponse struct {
	Created int64            `json:"created"`
	Data    []map[string]any `json:"data"`
}

// ---- upstream payloads -------------------------------------------------------------

type drawCreateResp struct {
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	Status    int    `json:"status"`
}

type repairCreateResp struct {
	SessionID string `json:"session_id"`
	MessageQ  struct {
		MsgID string `json:"msg_id"`
	} `json:"message_q"`
}

type drawPage struct {
	Items []struct {
		MessageID     string `json:"message_id"`
		IsUserMessage bool   `json:"is_user_message"`
		Waiting       bool   `json:"waiting"`
		Status        int    `json:"status"`
		Content       struct {
			Text  string   `json:"text"`
			URLs  []string `json:"urls"`
			Error string   `json:"error"`
		} `json:"content"`
	} `json:"items"`
}

type repairMessage struct {
	MsgID       string   `json:"msg_id"`
	RefID       string   `json:"ref_id"`
	FUID        string   `json:"fuid"`
	Content     string   `json:"content"`
	Status      string   `json:"status"`
	ContentType string   `json:"content_type"`
	URLs        []string `json:"urls"`
	DrawContent struct {
		URL      string `json:"url"`
		ThumbURL string `json:"thumb_url"`
	} `json:"draw_content"`
}

type repairPage struct {
	Items []struct {
		Messages map[string][]repairMessage `json:"messages"`
	} `json:"items"`
}

// pickRepairURLs extracts the finished picture from a repair message page.
//
// `draw_content.url` is the authoritative result field. The plain `urls` array must NOT
// be trusted blindly: on an image-edit turn the *user* message carries the input picture
// there, so reading it early returns the input unchanged — which looks like a successful
// no-op edit. Only reply messages (those with a ref_id) may contribute fallback urls.
func pickRepairURLs(page repairPage) []string {
	var results, fallback []string
	for _, item := range page.Items {
		for _, msgs := range item.Messages {
			for _, m := range msgs {
				if m.DrawContent.URL != "" {
					results = append(results, m.DrawContent.URL)
					continue
				}
				if m.RefID != "" && len(m.URLs) > 0 {
					fallback = append(fallback, m.URLs...)
				}
			}
		}
	}
	if len(results) > 0 {
		return results
	}
	return fallback
}

// terminalRepairState reports the last status/content seen, for error reporting.
func terminalRepairState(page repairPage) (status, text string) {
	for _, item := range page.Items {
		for _, msgs := range item.Messages {
			for _, m := range msgs {
				if m.Status != "" {
					status = m.Status
				}
				if m.Content != "" && !strings.HasPrefix(strings.TrimSpace(m.Content), "[{") {
					text = m.Content
				}
			}
		}
	}
	return status, text
}

// pickDrawURLs extracts the finished Midjourney pictures, skipping the user's own turn.
func pickDrawURLs(page drawPage) []string {
	var out []string
	for _, it := range page.Items {
		if it.IsUserMessage {
			continue
		}
		out = append(out, it.Content.URLs...)
	}
	return out
}

func (s *Server) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	var req imageRequest
	if err := decodeJSON(w, r, &req, s.cfg.MaxBodyBytes); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	res, ok := s.cat.Resolve(req.Model)
	if !ok || res.Kind == "chat" {
		if err := s.cat.Refresh(r.Context(), true); err == nil {
			res, ok = s.cat.Resolve(req.Model)
		}
	}
	if !ok || res.Kind == "chat" || res.ID <= 0 {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("model %q is not an image model; use GET /v1/models and pick a *-image id", req.Model))
		return
	}

	// Pre-flight price: the upstream billing module answers before anything is spent.
	if price := s.annotate(r.Context(), w, res.Key, "draw", res.ID); !s.guard(w, price) {
		return
	}

	if !s.acquire(r.Context(), s.c.imageGate) {
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "too many concurrent image requests")
		return
	}
	defer func() { <-s.c.imageGate }()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ImageTimeout)
	defer cancel()

	urls, err := s.generate(ctx, res, req.Prompt, ratioFromSize(req.Size), imageSize(req, s.cfg.ImageSize), nil)
	if err != nil {
		s.writeUpstreamErr(w, err)
		return
	}
	s.writeImages(w, req, urls)
}

// imageSize picks the resolution tier: the request wins, then the configured default.
func imageSize(req imageRequest, def string) string {
	if v := strings.TrimSpace(req.ImageSize); v != "" {
		return v
	}
	return def
}

// generate starts one image task and polls it to completion.
func (s *Server) generate(ctx context.Context, res Resolved, prompt, ratio, size string, inputURLs []string) ([]string, error) {
	if res.Kind == "mj" {
		return s.generateMJ(ctx, res, prompt, ratio)
	}
	return s.generateRepair(ctx, res, prompt, ratio, size, inputURLs)
}

func (s *Server) generateMJ(ctx context.Context, res Resolved, prompt, ratio string) ([]string, error) {
	params := []map[string]string{}
	if ratio != "" {
		params = append(params, map[string]string{"param_key": "aspect", "param_value": ratio})
	}
	body := map[string]any{
		"session_id":       "",
		"input_text":       prompt,
		"check_params":     nil,
		"model_id":         res.ID,
		"draw_type":        "TextToImg",
		"check_prompt_ids": nil,
		"fast_model":       s.cfg.FastModel,
		"check_params_v2":  params,
	}
	var created drawCreateResp
	if err := s.c.call(ctx, kindIOS, "/v1/aimoss/draw/create", body, &created); err != nil {
		return nil, err
	}
	if created.SessionID == "" {
		return nil, fmt.Errorf("draw/create returned no session_id")
	}
	logf("INFO", "mj task started session=%s model=%d", created.SessionID, res.ID)

	deadline := time.Now().Add(s.cfg.ImageTimeout)
	for time.Now().Before(deadline) {
		if err := sleepCtx(ctx, s.cfg.PollInterval); err != nil {
			return nil, err
		}
		var page drawPage
		if err := s.c.call(ctx, kindV1, "/v1/aimoss/draw/message/page",
			map[string]any{"session_id": created.SessionID, "cursor": "0", "limit": 100}, &page); err != nil {
			logf("WARN", "mj poll: %v", err)
			continue
		}
		if urls := pickDrawURLs(page); len(urls) > 0 {
			return urls, nil
		}
	}
	return nil, fmt.Errorf("midjourney task did not finish within %s", s.cfg.ImageTimeout)
}

func (s *Server) generateRepair(ctx context.Context, res Resolved, prompt, ratio, size string, inputURLs []string) ([]string, error) {
	if inputURLs == nil {
		inputURLs = []string{}
	}
	if size == "" {
		size = "2K"
	}
	body := map[string]any{
		"fast_model":   s.cfg.FastModel,
		"urls":         inputURLs,
		"prompt":       prompt,
		"model_id":     res.ID,
		"model_code":   res.Key,
		"session_id":   "",
		"tag":          "",
		"aspect_ratio": ratio,
		"image_size":   size,
	}
	var created repairCreateResp
	err := s.c.call(ctx, kindIOS, "/v1/aimoss/repair/async/create", body, &created)
	if err != nil {
		// 10033 means the shared task slot is busy; retry briefly.
		if ue, ok := err.(*errUpstream); ok && (ue.Ret == 10033 || ue.Ret == 500) {
			for i := 0; i < 6; i++ {
				if err := sleepCtx(ctx, 8*time.Second); err != nil {
					return nil, err
				}
				if err = s.c.call(ctx, kindIOS, "/v1/aimoss/repair/async/create", body, &created); err == nil {
					break
				}
				if ue2, ok := err.(*errUpstream); !ok || (ue2.Ret != 10033 && ue2.Ret != 500) {
					return nil, err
				}
			}
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	if created.SessionID == "" {
		return nil, fmt.Errorf("repair/async/create returned no session_id")
	}
	logf("INFO", "image task started session=%s model=%d code=%s", created.SessionID, res.ID, res.Key)

	deadline := time.Now().Add(s.cfg.ImageTimeout)
	for time.Now().Before(deadline) {
		if err := sleepCtx(ctx, s.cfg.PollInterval); err != nil {
			return nil, err
		}
		var page repairPage
		if err := s.c.call(ctx, kindV1, "/v1/aimoss/repair/web/list_msg",
			map[string]any{"session_id": created.SessionID, "limit": 100, "cursor": "0"}, &page); err != nil {
			logf("WARN", "image poll: %v", err)
			continue
		}
		if urls := pickRepairURLs(page); len(urls) > 0 {
			return urls, nil
		}
		if status, text := terminalRepairState(page); status == "failure" {
			if text == "" {
				text = "图片生成失败"
			}
			return nil, fmt.Errorf("upstream image task failed: %s", text)
		}
	}
	return nil, fmt.Errorf("image task did not finish within %s", s.cfg.ImageTimeout)
}

func (s *Server) writeImages(w http.ResponseWriter, req imageRequest, urls []string) {
	data := make([]map[string]any, 0, len(urls))
	for _, u := range urls {
		if strings.EqualFold(req.ResponseFormat, "b64_json") {
			b64, err := s.fetchB64(u)
			if err != nil {
				logf("WARN", "download image: %v", err)
				data = append(data, map[string]any{"url": u})
				continue
			}
			data = append(data, map[string]any{"b64_json": b64})
			continue
		}
		data = append(data, map[string]any{"url": u})
	}
	writeJSON(w, http.StatusOK, imageResponse{Created: time.Now().Unix(), Data: data})
}

func (s *Server) fetchB64(u string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: HTTP %d", u, resp.StatusCode)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxImageBytes))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// handleImageEdits accepts the OpenAI multipart shape (image + prompt) and routes it to
// an image model with the input picture attached.
func (s *Server) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(s.cfg.MaxUploadBytes); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "expected multipart/form-data: "+err.Error())
		return
	}
	model := r.FormValue("model")
	prompt := r.FormValue("prompt")
	size := r.FormValue("size")
	responseFormat := r.FormValue("response_format")
	if strings.TrimSpace(prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	res, ok := s.cat.Resolve(model)
	if !ok || res.Kind == "chat" || res.ID <= 0 {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("model %q cannot edit images; pick a repair-family *-image-edit id from GET /v1/models", model))
		return
	}
	if res.Kind == "mj" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"the Midjourney model does not accept input images on this route; use a repair-family model")
		return
	}
	if price := s.annotate(r.Context(), w, res.Key, "draw", res.ID); !s.guard(w, price) {
		return
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "image file is required")
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, s.cfg.MaxUploadBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if !s.acquire(r.Context(), s.c.imageGate) {
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "too many concurrent image requests")
		return
	}
	defer func() { <-s.c.imageGate }()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ImageTimeout)
	defer cancel()

	objectURL, err := s.uploadImage(ctx, raw, header.Filename, header.Header.Get("Content-Type"))
	if err != nil {
		s.writeUpstreamErr(w, err)
		return
	}
	logf("INFO", "input image uploaded: %s", objectURL)

	urls, err := s.generate(ctx, res, prompt, ratioFromSize(size), responseFormatOr(r.FormValue("image_size"), s.cfg.ImageSize), []string{objectURL})
	if err != nil {
		s.writeUpstreamErr(w, err)
		return
	}
	s.writeImages(w, imageRequest{ResponseFormat: responseFormat}, urls)
}

func responseFormatOr(v, def string) string {
	if s := strings.TrimSpace(v); s != "" {
		return s
	}
	return def
}

// ratioFromSize converts an OpenAI size such as 1024x1536 into the aspect ratio the
// upstream image routes take.
func ratioFromSize(size string) string {
	size = strings.TrimSpace(strings.ToLower(size))
	if size == "" || size == "auto" {
		return ""
	}
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		if strings.Contains(size, ":") {
			return size
		}
		return ""
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return ""
	}
	g := gcd(w, h)
	return fmt.Sprintf("%d:%d", w/g, h/g)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

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

var _ = json.Marshal
