package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
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
	// Non-standard extras this gateway understands:
	//   aspect_ratio  an explicit upstream ratio ("4:3"), overriding whatever `size` implies
	//   image_size    an explicit resolution tier ("1K"/"2K"/"4K"), validated per model
	//   mj_params     Midjourney knobs, passed through (stylize, chaos, quality, seed, ...)
	//   image         an input picture as a URL or data URI
	AspectRatio string         `json:"aspect_ratio,omitempty"`
	ImageSize   string         `json:"image_size,omitempty"`
	MJParams    map[string]any `json:"mj_params,omitempty"`
	Image       string         `json:"image,omitempty"`
}

// mjDisabledText explains why Midjourney is off by default, in the error itself, so the
// reason travels with the refusal instead of living only in the docs.
const mjDisabledText = "Midjourney is disabled (MJ_ENABLED=false). It is not metered in 灵力 — the " +
	"billing module has no price row for it and the account shows it as unlimited — but it is the " +
	"most expensive model on the account, so it must be enabled deliberately. Set MJ_ENABLED=true " +
	"in the config to allow it."

// tierFromPixels maps a requested pixel size onto a resolution tier name, e.g.
// "2048x2048" -> "2K".
//
// This exists because OpenAI-style clients express resolution as pixels, not tiers: the
// dsh image-gen plugin turns its "2K" dropdown choice into `size: "2048x2048"`. Deriving
// only the aspect ratio from that throws the tier away, and the request then falls back to
// the configured IMAGE_SIZE — which is how "I selected 2K and got 1K" happened.
func tierFromPixels(size string) string {
	w, h, ok := parseSizePixels(size)
	if !ok {
		return ""
	}
	long := w
	if h > w {
		long = h
	}
	if long < 512 {
		return ""
	}
	tier := int(math.Round(float64(long) / 1024.0))
	if tier < 1 {
		tier = 1
	}
	if tier > 8 {
		return ""
	}
	return fmt.Sprintf("%dK", tier)
}

// parseSizePixels reads "WIDTHxHEIGHT".
func parseSizePixels(size string) (int, int, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// genOptions is a validated, model-aware image request.
type genOptions struct {
	Prompt    string
	Aspect    string
	Size      string
	InputURLs []string
	MJParams  map[string]any
	// Where each value came from, so a log line can answer "why did I get 1K".
	AspectSource string
	SizeSource   string
}

// prepareImageOptions validates the requested ratio and resolution against the model's
// own parameter table and fills in its defaults.
//
// Validation is not pedantry here: the values are per model, most models offer exactly
// one resolution tier, and the upstream ignores an unsupported value silently — so a
// caller asking for 1K on a 2K-only model would otherwise get 2K and never know.
func (s *Server) prepareImageOptions(ctx context.Context, w http.ResponseWriter, res Resolved, req imageRequest) (*genOptions, bool) {
	p, _ := s.c.ParamsOf(ctx, res.Key)
	opts := &genOptions{Prompt: req.Prompt, MJParams: req.MJParams}

	aspect := strings.TrimSpace(req.AspectRatio)
	aspectSource := "request aspect_ratio"
	if aspect == "" {
		if aspect = ratioFromSize(req.Size); aspect != "" {
			aspectSource = "requested pixels " + req.Size
		}
	}
	if p != nil {
		if aspect != "" && !p.AllowsAspect(aspect) {
			// A pixel-derived ratio ("2048x1365" -> "2048:1365") is not what any model
			// lists; snap it to the nearest supported one first.
			if snapped, ok := p.SnapAspect(aspect); ok {
				logf("INFO", "aspect %s snapped to %s for model %s", aspect, snapped, res.Key)
				aspect = snapped
				aspectSource += " (snapped)"
			} else if s.cfg.StrictParams {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(
					"model %s does not accept aspect_ratio %q; supported: %s",
					res.Key, aspect, strings.Join(append(append([]string{}, p.AspectRatios...), `"" (auto)`), ", ")))
				return nil, false
			} else {
				// Pass it through and let the upstream answer: nothing has shown that it
				// refuses an unlisted ratio, and inventing a rejection here would hide the
				// provider's real behaviour. (STRICT_PARAMS=1 turns this into a 400.)
				logf("INFO", "aspect %s is not in model %s's list (%s); passing it through (STRICT_PARAMS=0)",
					aspect, res.Key, strings.Join(p.AspectRatios, " "))
			}
		}
		if aspect == "" {
			aspect, aspectSource = p.DefaultAspect, "model default"
		}
	}
	opts.Aspect = aspect
	opts.AspectSource = aspectSource

	// Resolution: an explicit tier field wins, then the tier implied by the requested
	// pixels, and only then the configured preference. A client that says 2048x2048 has
	// already chosen 2K — letting IMAGE_SIZE override that is what produced "I selected 2K
	// and got 1K".
	size := strings.TrimSpace(req.ImageSize)
	sizeSource := "request image_size"
	if size == "" {
		if derived := tierFromPixels(req.Size); derived != "" {
			size, sizeSource = derived, "requested pixels "+req.Size
		}
	}
	if size != "" && p != nil && !p.AllowsSize(size) {
		if s.cfg.StrictParams {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(
				"model %s does not accept image_size %q; supported: %s (default %s)",
				res.Key, size, strings.Join(p.ImageSizes, ", "), p.DefaultSize))
			return nil, false
		}
		logf("INFO", "image_size %s (%s) is not in model %s's list (%s); passing it through (STRICT_PARAMS=0)",
			size, sizeSource, res.Key, strings.Join(p.ImageSizes, " "))
	}
	if size == "" && s.cfg.ImageSize != "" && (p == nil || p.AllowsSize(s.cfg.ImageSize)) {
		size, sizeSource = s.cfg.ImageSize, "configured IMAGE_SIZE"
	}
	if p != nil && size == "" {
		size, sizeSource = p.DefaultSize, "model default"
	}
	opts.Size = size
	opts.SizeSource = sizeSource
	logf("INFO", "image request %s: aspect=%s (%s), image_size=%s (%s)",
		res.Key, aspect, aspectSource, size, sizeSource)

	if p != nil && p.PromptLimit > 0 && len([]rune(req.Prompt)) > p.PromptLimit {
		msg := fmt.Sprintf("prompt is %d characters; model %s declares prompt_limit=%d upstream (which it does not enforce)",
			len([]rune(req.Prompt)), res.Key, p.PromptLimit)
		switch s.cfg.PromptLimitMode {
		case "enforce":
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", msg)
			return nil, false
		case "warn":
			logf("WARN", "%s — sending anyway (PROMPT_LIMIT_MODE=warn)", msg)
		}
	}
	return opts, true
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
			Text  string     `json:"text"`
			URLs  []ImageURL `json:"urls"`
			Error string     `json:"error"`
		} `json:"content"`
	} `json:"items"`
}

type repairMessage struct {
	MsgID       string     `json:"msg_id"`
	RefID       string     `json:"ref_id"`
	FUID        string     `json:"fuid"`
	Content     string     `json:"content"`
	Status      string     `json:"status"`
	ContentType string     `json:"content_type"`
	URLs        []ImageURL `json:"urls"`
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
					fallback = append(fallback, urlsOf(m.URLs)...)
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
// terminalRepairState reports the task's terminal status and the text that goes with it.
//
// The page groups messages in a map keyed by the parent message id, and Go's map iteration
// order is random — so "the last message wins" made this return complete or failure at
// random on a page that holds both. The assistant's own turn is the one carrying ref_id (the
// operator's turn has none), and within it the highest msg_id is the newest, so ordering by
// ref_id + msg_id makes the answer deterministic and semantically right.
func terminalRepairState(page repairPage) (status, text string) {
	type cand struct{ id, status, content string }
	var withRef, anyMsg []cand
	for _, item := range page.Items {
		for _, msgs := range item.Messages {
			for _, m := range msgs {
				c := cand{id: m.MsgID, status: m.Status, content: m.Content}
				anyMsg = append(anyMsg, c)
				if m.RefID != "" {
					withRef = append(withRef, c)
				}
			}
		}
	}
	list := withRef
	if len(list) == 0 {
		list = anyMsg // a page with no reply grouping: fall back rather than report nothing
	}
	sort.Slice(list, func(i, j int) bool { return list[i].id < list[j].id })
	for _, c := range list {
		if c.status != "" {
			status = c.status
		}
		if c.content != "" && !strings.HasPrefix(strings.TrimSpace(c.content), "[{") {
			text = c.content
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
		out = append(out, urlsOf(it.Content.URLs)...)
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
	if res.Kind == "mj" && !s.cfg.MJEnabled {
		writeOpenAIError(w, http.StatusBadRequest, "model_disabled", mjDisabledText)
		return
	}

	// Pre-flight price: the upstream billing module answers before anything is spent.
	if price := s.annotate(r.Context(), w, res.Key, "draw", res.ID); !s.guard(w, price) {
		return
	}

	// Validate the ratio/resolution against the model's own table before spending anything.
	opts, ok := s.prepareImageOptions(r.Context(), w, res, req)
	if !ok {
		return
	}

	if !s.acquire(r.Context(), s.c.imageGate) {
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "too many concurrent image requests")
		return
	}
	defer func() { <-s.c.imageGate }()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ImageTimeout)
	defer cancel()

	// Meter the real cost: read the balance before and after and record the difference.
	// The upstream quote is what the billing module predicts, not what it takes — a chat
	// model quoting 0 灵力 was measured deducting 48 per turn.
	balanceBefore := s.balanceOf(ctx)
	urls, sessionID, err := s.generate(ctx, res, opts)
	balanceAfter := s.balanceOf(ctx)
	if s.records != nil && sessionID != "" && balanceBefore >= 0 && balanceAfter >= 0 {
		actual := balanceBefore - balanceAfter
		quoted := 0
		if price, perr := s.c.PriceOf(ctx, res.Key, "draw", res.ID, "draw"); perr == nil && price != nil {
			quoted = price.Effective
		}
		logf("INFO", "measured cost for %s: quoted=%d 灵力, actual=%d 灵力 (balance %d -> %d)",
			res.Key, quoted, actual, balanceBefore, balanceAfter)
		s.records.SetCost(sessionID, quoted, actual)
	}
	if err != nil {
		s.writeUpstreamErr(w, err)
		return
	}
	s.writeImages(w, req, urls)
}

// balanceOf reads the credit balance, or -1 when it cannot be read.
func (s *Server) balanceOf(ctx context.Context) int {
	if s.c == nil {
		return -1
	}
	if bal, _, err := s.c.Balance(ctx); err == nil {
		return bal
	}
	return -1
}

// generate starts one image task and polls it to completion.
func (s *Server) generate(ctx context.Context, res Resolved, opts *genOptions) ([]string, string, error) {
	if res.Kind == "mj" {
		return s.generateMJ(ctx, res, opts)
	}
	return s.generateRepair(ctx, res, opts)
}

func (s *Server) generateMJ(ctx context.Context, res Resolved, opts *genOptions) ([]string, string, error) {
	// The draw page sends the ratio as check_params_v2[{param_key:"aspect"}]; verified
	// against a real 4:3 request that came back 1024x772.
	params := []map[string]string{}
	if opts.Aspect != "" {
		params = append(params, map[string]string{"param_key": "aspect", "param_value": opts.Aspect})
	}
	// Extra Midjourney knobs (stylize, chaos, quality, seed, no, tile, ...) pass through.
	for k, v := range opts.MJParams {
		if k == "" {
			continue
		}
		params = append(params, map[string]string{"param_key": k, "param_value": fmt.Sprint(v)})
	}
	body := map[string]any{
		"session_id":       "",
		"input_text":       opts.Prompt,
		"check_params":     nil,
		"model_id":         res.ID,
		"draw_type":        "TextToImg",
		"check_prompt_ids": nil,
		"fast_model":       s.cfg.FastModel,
		"check_params_v2":  params,
	}
	var created drawCreateResp
	if err := s.c.call(ctx, kindIOS, "/v1/aimoss/draw/create", body, &created); err != nil {
		return nil, "", err
	}
	if created.SessionID == "" {
		return nil, "", fmt.Errorf("draw/create returned no session_id")
	}
	if s.records != nil {
		s.records.Add("mj", created.SessionID, res.Key, opts.Prompt)
	}
	logf("INFO", "mj task started session=%s model=%d", created.SessionID, res.ID)

	deadline := time.Now().Add(s.cfg.ImageTimeout)
	for time.Now().Before(deadline) {
		if err := sleepCtx(ctx, s.cfg.PollInterval); err != nil {
			return nil, "", err
		}
		var page drawPage
		if err := s.c.call(ctx, kindV1, "/v1/aimoss/draw/message/page",
			map[string]any{"session_id": created.SessionID, "cursor": "0", "limit": 100}, &page); err != nil {
			logf("WARN", "mj poll: %v", err)
			continue
		}
		if urls := pickDrawURLs(page); len(urls) > 0 {
			return urls, created.SessionID, nil
		}
	}
	return nil, "", fmt.Errorf("midjourney task did not finish within %s", s.cfg.ImageTimeout)
}

func (s *Server) generateRepair(ctx context.Context, res Resolved, opts *genOptions) ([]string, string, error) {
	inputURLs := opts.InputURLs
	if inputURLs == nil {
		inputURLs = []string{}
	}
	body := map[string]any{
		"fast_model":   s.cfg.FastModel,
		"urls":         inputURLs,
		"prompt":       opts.Prompt,
		"model_id":     res.ID,
		"model_code":   res.Key,
		"session_id":   "",
		"tag":          "",
		"aspect_ratio": opts.Aspect,
		"image_size":   opts.Size,
	}
	var created repairCreateResp
	err := s.c.call(ctx, kindIOS, "/v1/aimoss/repair/async/create", body, &created)
	if err != nil {
		// 10033 means the shared task slot is busy; retry briefly.
		if ue, ok := err.(*errUpstream); ok && (ue.Ret == 10033 || ue.Ret == 500) {
			for i := 0; i < 6; i++ {
				if err := sleepCtx(ctx, 8*time.Second); err != nil {
					return nil, "", err
				}
				if err = s.c.call(ctx, kindIOS, "/v1/aimoss/repair/async/create", body, &created); err == nil {
					break
				}
				if ue2, ok := err.(*errUpstream); !ok || (ue2.Ret != 10033 && ue2.Ret != 500) {
					return nil, "", err
				}
			}
			if err != nil {
				return nil, "", err
			}
		} else {
			return nil, "", err
		}
	}
	if created.SessionID == "" {
		return nil, "", fmt.Errorf("repair/async/create returned no session_id")
	}
	if s.records != nil {
		s.records.Add("image", created.SessionID, res.Key, opts.Prompt)
	}
	logf("INFO", "image task started session=%s model=%d code=%s aspect=%s image_size=%s",
		created.SessionID, res.ID, res.Key, opts.Aspect, opts.Size)

	deadline := time.Now().Add(s.cfg.ImageTimeout)
	for time.Now().Before(deadline) {
		if err := sleepCtx(ctx, s.cfg.PollInterval); err != nil {
			return nil, "", err
		}
		var page repairPage
		if err := s.c.call(ctx, kindV1, "/v1/aimoss/repair/web/list_msg",
			map[string]any{"session_id": created.SessionID, "limit": 100, "cursor": "0"}, &page); err != nil {
			logf("WARN", "image poll: %v", err)
			continue
		}
		if urls := pickRepairURLs(page); len(urls) > 0 {
			return urls, created.SessionID, nil
		}
		if status, text := terminalRepairState(page); status == "failure" {
			if text == "" {
				text = "图片生成失败"
			}
			return nil, "", fmt.Errorf("upstream image task failed: %s", text)
		}
	}
	return nil, "", fmt.Errorf("image task did not finish within %s", s.cfg.ImageTimeout)
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

	// Same validation as generation, with the ratio taken from the form.
	editReq := imageRequest{
		Model:       model,
		Prompt:      prompt,
		Size:        size,
		ImageSize:   r.FormValue("image_size"),
		AspectRatio: r.FormValue("aspect_ratio"),
	}
	opts, ok := s.prepareImageOptions(r.Context(), w, res, editReq)
	if !ok {
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
	opts.InputURLs = []string{objectURL}

	balanceBefore := s.balanceOf(ctx)
	urls, sessionID, err := s.generate(ctx, res, opts)
	balanceAfter := s.balanceOf(ctx)
	if s.records != nil && sessionID != "" && balanceBefore >= 0 && balanceAfter >= 0 {
		actual := balanceBefore - balanceAfter
		quoted := 0
		if price, perr := s.c.PriceOf(ctx, res.Key, "draw", res.ID, "draw"); perr == nil && price != nil {
			quoted = price.Effective
		}
		logf("INFO", "measured cost for %s (edit): quoted=%d 灵力, actual=%d 灵力 (balance %d -> %d)",
			res.Key, quoted, actual, balanceBefore, balanceAfter)
		s.records.SetCost(sessionID, quoted, actual)
	}
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
