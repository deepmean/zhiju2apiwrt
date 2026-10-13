package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---- OpenAI request/response shapes ------------------------------------------------

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	// Accepted for compatibility; the upstream route does not expose them.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	FastModel   *bool    `json:"fast_model,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatOutMsg  `json:"message"`
	FinishReason string      `json:"finish_reason"`
	Delta        *chatOutMsg `json:"delta,omitempty"`
}

type chatOutMsg struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *usage       `json:"usage,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---- upstream turn protocol -------------------------------------------------------

type turnRequest struct {
	ClientRequestID  string    `json:"client_request_id"`
	Operation        string    `json:"operation"`
	FastModel        bool      `json:"fast_model"`
	Capacities       []string  `json:"capacities"`
	Ty               int       `json:"ty"`
	Data             turnData  `json:"data"`
	MaxContextRounds int       `json:"max_context_rounds"`
	ModelCode        string    `json:"model_code"`
	SessionID        string    `json:"session_id,omitempty"`
}

type turnData struct {
	Prompt      string `json:"prompt"`
	ContentType string `json:"content_type"`
	URLs        []any  `json:"urls"`
	Seq         int    `json:"seq"`
}

type turnEvent struct {
	TurnID    string `json:"turn_id"`
	SessionID string `json:"session_id"`
	Sequence  int    `json:"sequence"`
	Type      string `json:"type"`
	State     string `json:"state"`
	Delta     string `json:"delta"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	Usage     *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := decodeJSON(w, r, &req, s.cfg.MaxBodyBytes); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}
	res, ok := s.cat.Resolve(req.Model)
	if !ok || res.Kind != "chat" {
		// A name we do not know yet may simply be newer than the cache.
		if err := s.cat.Refresh(r.Context(), true); err == nil {
			res, ok = s.cat.Resolve(req.Model)
		}
	}
	if !ok || res.Kind != "chat" {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("model %q is not a chat model; call GET /v1/models (image models use the -image suffix)", req.Model))
		return
	}

	prompt, err := flattenMessages(req.Messages)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Pre-flight: ask the upstream billing module what this turn costs, log it, expose it
	// on the response headers, and refuse it when a ceiling is configured.
	if price := s.annotate(r.Context(), w, res.Key, "text", s.cat.IDOf(res.Key)); !s.guard(w, price) {
		return
	}

	fast := s.cfg.FastModel
	if req.FastModel != nil {
		fast = *req.FastModel
	}
	body := turnRequest{
		ClientRequestID:  newUUID(),
		Operation:        "create",
		FastModel:        fast,
		Capacities:       []string{},
		Ty:               0,
		Data:             turnData{Prompt: prompt, ContentType: "text", URLs: []any{}, Seq: 0},
		MaxContextRounds: s.cfg.MaxContextRounds,
		ModelCode:        res.Key,
	}

	if !s.acquire(r.Context(), s.c.chatGate) {
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "too many concurrent chat requests")
		return
	}
	defer func() { <-s.c.chatGate }()

	if req.Stream {
		s.streamChat(w, r, req, body)
		return
	}
	s.bufferedChat(w, r, req, body)
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req chatRequest, body turnRequest) {
	flusher, _ := w.(http.Flusher)
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ChatTimeout)
	defer cancel()

	resp, err := s.c.openTurn(ctx, body)
	if err != nil {
		s.writeUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := "chatcmpl-" + newUUID()
	created := time.Now().Unix()
	sentRole := false
	failed := false

	send := func(delta chatOutMsg, finish string) {
		chunk := chatResponse{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []chatChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	err = readSSE(resp.Body, func(ev sseEvent) error {
		if len(ev.Data) == 0 {
			return nil
		}
		var te turnEvent
		if err := json.Unmarshal(ev.Data, &te); err != nil {
			return nil // ignore frames we do not understand
		}
		if te.Type == "" {
			te.Type = ev.Event
		}
		switch te.Type {
		case "turn.started", "model.started":
			if !sentRole {
				sentRole = true
				send(chatOutMsg{Role: "assistant", Content: ""}, "")
			}
		case "message.delta":
			if te.Delta != "" {
				if !sentRole {
					sentRole = true
				}
				send(chatOutMsg{Content: te.Delta}, "")
			}
		case "thinking.delta":
			if te.Delta != "" {
				send(chatOutMsg{ReasoningContent: te.Delta}, "")
			}
		case "turn.failed":
			failed = true
			code := te.ErrorCode
			if code == "" {
				code = "internal_error"
			}
			b, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": turnErrorText(code),
					"type":    "upstream_error",
					"code":    code,
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
			return nil
		case "turn.completed":
			send(chatOutMsg{}, "stop")
		case "turn.canceled":
			send(chatOutMsg{}, "stop")
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		logf("WARN", "turn stream ended: %v", err)
	}
	if !failed {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) bufferedChat(w http.ResponseWriter, r *http.Request, req chatRequest, body turnRequest) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ChatTimeout)
	defer cancel()

	resp, err := s.c.openTurn(ctx, body)
	if err != nil {
		s.writeUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	var content, reasoning strings.Builder
	var failCode, failMsg string
	_ = readSSE(resp.Body, func(ev sseEvent) error {
		if len(ev.Data) == 0 {
			return nil
		}
		var te turnEvent
		if err := json.Unmarshal(ev.Data, &te); err != nil {
			return nil
		}
		if te.Type == "" {
			te.Type = ev.Event
		}
		switch te.Type {
		case "message.delta":
			content.WriteString(te.Delta)
		case "thinking.delta":
			reasoning.WriteString(te.Delta)
		case "turn.failed":
			failCode = te.ErrorCode
			failMsg = te.Message
		}
		return nil
	})

	if failCode != "" && content.Len() == 0 {
		writeOpenAIError(w, upstreamStatus(failCode), "upstream_error", turnErrorText(failCode)+" ("+failCode+")")
		return
	}
	_ = failMsg

	out := chatResponse{
		ID: "chatcmpl-" + newUUID(), Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatOutMsg{Role: "assistant", Content: content.String(), ReasoningContent: reasoning.String()},
			FinishReason: "stop",
		}},
	}
	writeJSON(w, http.StatusOK, out)
}

// openTurn starts one upstream turn and returns the raw event stream.
func (c *Client) openTurn(ctx context.Context, body turnRequest) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		tok := c.pool.Pick()
		if tok == nil {
			return nil, fmt.Errorf("no session token configured (%s)", c.cfg.TokensF)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/v2/ai/chat/turns"), strings.NewReader(string(raw)))
		if err != nil {
			return nil, err
		}
		req.Header = c.headers(kindChat, tok, "text/event-stream")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		ct := strings.ToLower(resp.Header.Get("content-type"))
		if strings.Contains(ct, "text/event-stream") {
			c.pool.MarkOK(tok)
			return resp, nil
		}
		// A JSON body here means an envelope error (typically an invalid session).
		buf, _ := readMaybeGzip(resp)
		resp.Body.Close()
		var env apiResp
		if err := json.Unmarshal(buf, &env); err == nil && env.Ret == 20001 {
			c.pool.MarkInvalid(tok, env.Msg)
			lastErr = &errAuth{token: tok}
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("turns: HTTP %d (%.200s)", resp.StatusCode, buf)
		}
		return nil, fmt.Errorf("turns: unexpected content-type %q (%.200s)", ct, buf)
	}
	return nil, lastErr
}

// flattenMessages turns an OpenAI conversation into the single prompt the web route
// accepts. The route only takes one current message, so earlier turns ride along as a
// compact transcript block.
func flattenMessages(msgs []chatMessage) (string, error) {
	var system []string
	var turns []string
	var last string

	for i, m := range msgs {
		text, err := messageText(m.Content)
		if err != nil {
			return "", err
		}
		switch m.Role {
		case "system", "developer":
			if strings.TrimSpace(text) != "" {
				system = append(system, strings.TrimSpace(text))
			}
		case "user":
			if i == len(msgs)-1 {
				last = text
				continue
			}
			turns = append(turns, "user: "+text)
		case "assistant":
			turns = append(turns, "assistant: "+text)
		case "tool", "function":
			turns = append(turns, "tool: "+text)
		default:
			if strings.TrimSpace(text) != "" {
				turns = append(turns, m.Role+": "+text)
			}
		}
	}
	if strings.TrimSpace(last) == "" {
		return "", errors.New("the last message must be a non-empty user message")
	}

	var sb strings.Builder
	if len(system) > 0 {
		sb.WriteString(strings.Join(system, "\n"))
		sb.WriteString("\n\n")
	}
	if len(turns) > 0 {
		sb.WriteString("[对话历史]\n")
		sb.WriteString(strings.Join(turns, "\n"))
		sb.WriteString("\n[对话历史结束]\n\n")
	}
	sb.WriteString(last)
	return sb.String(), nil
}

// messageText accepts both the plain string form and the content-parts array.
func messageText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "input_text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String(), nil
	}
	return "", errors.New("unsupported message content shape")
}

// turnErrorText mirrors the wording the web client shows for each upstream code.
func turnErrorText(code string) string {
	switch code {
	case "price_unavailable":
		return "upstream could not price this request — usually a stale version header (CHAT_VER) or a temporarily unavailable route"
	case "insufficient_balance":
		return "upstream account has insufficient balance"
	case "rate_limited":
		return "upstream is rate limiting this account"
	case "content_rejected":
		return "upstream rejected the content"
	case "context_too_large":
		return "context too large; shorten the conversation"
	case "authentication_failed", "account_unavailable":
		return "upstream session is no longer valid; supply a fresh token"
	case "provider_unavailable", "no_available_route", "invalid_configuration":
		return "upstream route is temporarily unavailable"
	case "timeout":
		return "upstream timed out"
	case "cancelled":
		return "cancelled"
	default:
		return "upstream turn failed"
	}
}

func upstreamStatus(code string) int {
	switch code {
	case "authentication_failed", "account_unavailable":
		return http.StatusUnauthorized
	case "rate_limited":
		return http.StatusTooManyRequests
	case "insufficient_balance":
		return http.StatusPaymentRequired
	case "content_rejected":
		return http.StatusBadRequest
	case "timeout":
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}
