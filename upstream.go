package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// errAuth means the upstream refused the session ("登录状态无效，请重新登录").
type errAuth struct{ token *Token }

func (e *errAuth) Error() string {
	if e.token != nil {
		return "upstream rejected the session token (" + e.token.describe() + "); re-login required"
	}
	return "upstream rejected the session token; re-login required"
}

func isAuthError(err error) bool {
	_, ok := err.(*errAuth)
	return ok
}

// errUpstream is a business error carried in the {ret,msg} envelope.
type errUpstream struct {
	Ret int
	Msg string
}

func (e *errUpstream) Error() string { return fmt.Sprintf("upstream ret=%d: %s", e.Ret, e.Msg) }

// reqKind selects the header profile the web client uses for that route family.
type reqKind int

const (
	kindChat reqKind = iota // v2/ai/* — needs wholeApiVersion (3.2.0)
	kindV1                  // aimoss v1 web routes — 2.2.11
	kindRepair              // repair routes — 1.1.10
	kindIOS                 // draw/repair create — plat:iOS
	kindRepairIOS           // repair routes that only answer the iOS platform — 1.1.10
)

type Client struct {
	cfg    *Config
	http   *http.Client
	pool   *Pool
	chatVer atomic.Value // string, may be discovered at boot
	prices *priceCache   // upstream-quoted cost per model, for pre-flight annotation
	params *paramsCache  // per-model aspect ratios / resolution tiers / prompt limits

	chatGate  chan struct{}
	imageGate chan struct{}
}

func NewClient(cfg *Config, pool *Pool) *Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// The upstream answers with gzip; Go's transparent decompression is disabled
		// here only for streaming routes, which we handle explicitly.
		DisableCompression: false,
	}
	c := &Client{
		cfg:       cfg,
		http:      &http.Client{Transport: tr, Timeout: 0},
		pool:      pool,
		prices:    newPriceCache(cfg.PriceCacheTTL),
		params:    &paramsCache{m: map[string]*ModelParams{}, ttl: cfg.ParamsCacheTTL},
		chatGate:  make(chan struct{}, maxInt(1, cfg.ChatMaxConcurrent)),
		imageGate: make(chan struct{}, maxInt(1, cfg.ImageMaxConcurrent)),
	}
	c.chatVer.Store(cfg.ChatVer)
	return c
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (c *Client) version() string {
	if v, ok := c.chatVer.Load().(string); ok && v != "" {
		return v
	}
	return c.cfg.ChatVer
}

func (c *Client) headers(kind reqKind, token *Token, accept string) http.Header {
	h := http.Header{}
	h.Set("content-type", "application/json")
	if accept == "" {
		accept = "application/json"
	}
	h.Set("accept", accept)
	h.Set("accept-language", "zh-CN")
	h.Set("x-locale", "zh-CN")
	h.Set("origin", c.cfg.Upstream)
	h.Set("referer", c.cfg.Upstream+"/")
	h.Set("user-agent", webUA)
	h.Set("x-common-params", "channel=molica_web_browser")
	if token != nil {
		h.Set("x-token-id", "Bearer "+token.Value)
	}

	switch kind {
	case kindChat:
		h.Set("plat", "web_browser")
		h.Set("x-platform", "web_browser")
		h.Set("ver", c.version())
	case kindV1:
		h.Set("plat", "web_browser")
		h.Set("x-platform", "web_browser")
		h.Set("ver", c.cfg.AimossVer)
	case kindRepair:
		h.Set("plat", "web_browser")
		h.Set("x-platform", "web_browser")
		h.Set("ver", c.cfg.RepairVer)
	case kindIOS:
		h.Set("plat", "iOS")
		h.Set("x-platform", "iOS")
		h.Set("ver", c.cfg.AimossVer)
	case kindRepairIOS:
		// /v1/aimoss/cfg/list answers "不支持的平台" to plat:web_browser; the web client
		// itself sends the iOS platform with the repair version.
		h.Set("plat", "iOS")
		h.Set("x-platform", "iOS")
		h.Set("ver", c.cfg.RepairVer)
	}
	return h
}

// apiResp is the shared {ret,msg,data} envelope.
type apiResp struct {
	Ret  int             `json:"ret"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (c *Client) url(path string) string {
	if strings.HasPrefix(path, "http") {
		return path
	}
	return c.cfg.Upstream + "/quanling" + path
}

// call performs a JSON POST and decodes the envelope. It rotates to another token once
// when the upstream reports an invalid session.
func (c *Client) call(ctx context.Context, kind reqKind, path string, body any, out any) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		tok := c.pool.Pick()
		if tok == nil {
			return fmt.Errorf("no session token configured (%s)", c.cfg.TokensF)
		}
		raw, err := c.postJSON(ctx, kind, tok, path, body)
		if err != nil {
			return err
		}
		var env apiResp
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("decode %s: %w (body: %.200s)", path, err, raw)
		}
		switch env.Ret {
		case 1:
			c.pool.MarkOK(tok)
			if out != nil && len(env.Data) > 0 {
				if err := json.Unmarshal(env.Data, out); err != nil {
					return fmt.Errorf("decode data of %s: %w", path, err)
				}
			}
			return nil
		case 20001:
			c.pool.MarkInvalid(tok, env.Msg)
			lastErr = &errAuth{token: tok}
			continue // try the next token
		default:
			return &errUpstream{Ret: env.Ret, Msg: env.Msg}
		}
	}
	return lastErr
}

func (c *Client) postJSON(ctx context.Context, kind reqKind, tok *Token, path string, body any) ([]byte, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), &buf)
	if err != nil {
		return nil, err
	}
	req.Header = c.headers(kind, tok, "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readMaybeGzip(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s: HTTP %d (%.200s)", path, resp.StatusCode, raw)
	}
	return raw, nil
}

func readMaybeGzip(resp *http.Response) ([]byte, error) {
	var r io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("content-encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	return io.ReadAll(io.LimitReader(r, 32<<20))
}

// Ping is the heartbeat: a cheap authenticated call that also confirms the token.
func (c *Client) Ping(tok *Token) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := c.postJSON(ctx, kindV1, tok, "/v1/account/get", map[string]any{})
	if err != nil {
		return err
	}
	var env apiResp
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if env.Ret == 20001 {
		return &errAuth{token: tok}
	}
	if env.Ret != 1 {
		return &errUpstream{Ret: env.Ret, Msg: env.Msg}
	}
	return nil
}

// callAnon is call() without a session token: the login routes are the only ones that
// must work while every stored token is dead.
func (c *Client) callAnon(ctx context.Context, kind reqKind, path string, body any, out any) error {
	raw, err := c.postJSON(ctx, kind, nil, path, body)
	if err != nil {
		return err
	}
	var env apiResp
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode %s: %w (body: %.200s)", path, err, raw)
	}
	if env.Ret != 1 {
		return &errUpstream{Ret: env.Ret, Msg: env.Msg}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode data of %s: %w", path, err)
		}
	}
	return nil
}

// SendLoginSMS asks the upstream to text a verification code to the account's phone.
// The upstream expects type=3 for the web login flow.
func (c *Client) SendLoginSMS(ctx context.Context, phone string, typ int) error {
	if typ == 0 {
		typ = 3
	}
	var data map[string]any
	return c.callAnon(ctx, kindV1, "/v1/account/binding/send_sms_v2", map[string]any{
		"phone_number": phone,
		"type":         typ,
	}, &data)
}

// PhoneLogin exchanges phone + SMS code for a fresh session token. This is the only way
// to renew a session: there is no refresh_token and no cookie anywhere in this API.
func (c *Client) PhoneLogin(ctx context.Context, phone, code, invitation string) (string, map[string]any, error) {
	var data struct {
		Token string         `json:"token"`
		User  map[string]any `json:"user"`
	}
	err := c.callAnon(ctx, kindV1, "/v1/account/phone_login", map[string]any{
		"platform":          6,
		"phone":             phone,
		"verification_code": code,
		"invitation_code":   invitation,
	}, &data)
	if err != nil {
		return "", nil, err
	}
	if data.Token == "" {
		return "", nil, fmt.Errorf("phone_login returned no token")
	}
	return data.Token, data.User, nil
}

// Balance reads the credit balance (灵力) the account has left.
func (c *Client) Balance(ctx context.Context) (int, string, error) {
	var data struct {
		IntegralBalance int    `json:"integral_balance"`
		ExpireTime      string `json:"expire_time"`
	}
	if err := c.call(ctx, kindV1, "/v1/integral/balance/query", map[string]any{}, &data); err != nil {
		return 0, "", err
	}
	return data.IntegralBalance, data.ExpireTime, nil
}

// UpstreamSession is one conversation the account holds upstream.
type UpstreamSession struct {
	Kind      string    `json:"kind"` // chat | image | mj
	SessionID string    `json:"session_id"`
	Title     string    `json:"title"`
	ModelID   int       `json:"model_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ListSessions reads the account's conversations from all three families, so the panel can
// offer cleanup for records created before the ledger existed.
func (c *Client) ListSessions(ctx context.Context) ([]UpstreamSession, error) {
	var out []UpstreamSession

	var repair struct {
		Items []struct {
			SessionID string `json:"session_id"`
			Title     string `json:"title"`
			ModelID   int    `json:"model_group_id"`
			CreatedAt string `json:"created_at"`
		} `json:"items"`
	}
	if err := c.call(ctx, kindRepair, "/v1/aimoss/repair/history",
		map[string]any{"keyword": "", "limit": 200, "cursor": "0"}, &repair); err == nil {
		for _, it := range repair.Items {
			out = append(out, UpstreamSession{
				Kind: "image", SessionID: it.SessionID, Title: it.Title,
				ModelID: it.ModelID, CreatedAt: parseUpstreamTime(it.CreatedAt),
			})
		}
	}

	var draw struct {
		Items []struct {
			SessionID string `json:"session_id"`
			Title     string `json:"title"`
			ModelID   int    `json:"model_id"`
			CreatedAt string `json:"created_at"`
		} `json:"items"`
	}
	if err := c.call(ctx, kindV1, "/v1/aimoss/draw/session/page",
		map[string]any{"keyword": "", "cursor": "0", "limit": 200}, &draw); err == nil {
		for _, it := range draw.Items {
			out = append(out, UpstreamSession{
				Kind: "mj", SessionID: it.SessionID, Title: it.Title,
				ModelID: it.ModelID, CreatedAt: parseUpstreamTime(it.CreatedAt),
			})
		}
	}

	var chat struct {
		Items []struct {
			SessionID string `json:"session_id"`
			Title     string `json:"title"`
			ModelID   int    `json:"model_id"`
			ChatTime  int64  `json:"chat_time"`
			CreatedAt string `json:"created_at"`
		} `json:"items"`
	}
	if err := c.call(ctx, kindChat, "/v2/ai/chat/sessions/query",
		map[string]any{"cursor": "", "keyword": "", "limit": 100}, &chat); err == nil {
		for _, it := range chat.Items {
			created := parseUpstreamTime(it.CreatedAt)
			if created.IsZero() && it.ChatTime > 0 {
				created = time.Unix(it.ChatTime, 0)
			}
			out = append(out, UpstreamSession{
				Kind: "chat", SessionID: it.SessionID, Title: it.Title,
				ModelID: it.ModelID, CreatedAt: created,
			})
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no sessions listed (every family call failed)")
	}
	return out, nil
}

// parseUpstreamTime accepts the RFC3339 timestamps the account uses.
func parseUpstreamTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// DeleteSession removes one upstream session. Each family has its own route, and all
// three take the same single field, which is why the kind only selects the path.
func (c *Client) DeleteSession(ctx context.Context, kind, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("empty session id")
	}
	var path string
	var rk reqKind
	switch kind {
	case "chat":
		path, rk = "/v2/ai/chat/sessions/delete", kindChat
	case "mj":
		path, rk = "/v1/aimoss/draw/session/delete", kindV1
	default: // image (repair family)
		path, rk = "/v1/aimoss/repair/delete", kindRepair
	}
	var data map[string]any
	return c.call(ctx, rk, path, map[string]any{"session_id": sessionID}, &data)
}

// CfgList fetches the web client's channel config. It answers only the iOS platform
// (plat:web_browser gets ret=10002 不支持的平台) and carries two things this gateway
// needs: channelConfig.wholeApiVersion for the chat route, and the full model table with
// model_key -> model_id.
func (c *Client) CfgList(ctx context.Context) (map[string]any, error) {
	var data map[string]any
	if err := c.call(ctx, kindRepairIOS, "/v1/aimoss/cfg/list", map[string]any{"check_params": ""}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// DiscoverVersion reads channelConfig.wholeApiVersion, the value the v2 chat route
// requires. Without it the route answers `price_unavailable` for every model, which
// looks like a model problem but is really a header problem.
func (c *Client) DiscoverVersion(ctx context.Context) string {
	data, err := c.CfgList(ctx)
	if err != nil {
		logf("WARN", "version discovery failed, keeping %s: %v", c.version(), err)
		return ""
	}
	if v := findString(data, "wholeApiVersion"); v != "" {
		c.chatVer.Store(v)
		logf("INFO", "discovered wholeApiVersion=%s", v)
		return v
	}
	logf("WARN", "wholeApiVersion not present in cfg/list, keeping %s", c.version())
	return ""
}

func findString(v any, key string) string {
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t[key].(string); ok && s != "" {
			return s
		}
		for _, sub := range t {
			if s := findString(sub, key); s != "" {
				return s
			}
		}
	case []any:
		for _, sub := range t {
			if s := findString(sub, key); s != "" {
				return s
			}
		}
	}
	return ""
}

func b64urlDecode(s string) ([]byte, error) {
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.URLEncoding.DecodeString(s)
}
