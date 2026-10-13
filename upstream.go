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
)

type Client struct {
	cfg    *Config
	http   *http.Client
	pool   *Pool
	chatVer atomic.Value // string, may be discovered at boot

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
		chatGate:  make(chan struct{}, max(1, cfg.ChatMaxConcurrent)),
		imageGate: make(chan struct{}, max(1, cfg.ImageMaxConcurrent)),
	}
	c.chatVer.Store(cfg.ChatVer)
	return c
}

func max(a, b int) int {
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

// DiscoverVersion reads channelConfig.wholeApiVersion, the value the v2 chat route
// requires. Without it the route answers `price_unavailable` for every model, which
// looks like a model problem but is really a header problem.
func (c *Client) DiscoverVersion(ctx context.Context) string {
	var data map[string]any
	if err := c.call(ctx, kindRepair, "/v1/aimoss/cfg/list", map[string]any{"check_params": ""}, &data); err != nil {
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
