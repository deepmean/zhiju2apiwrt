package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	// Embeds the IANA timezone database in the binary (~450KB): a router has no tzdata,
	// and Go refuses to interpret the POSIX TZ=CST-8 form OpenWrt sets, so without this
	// the process runs in UTC and the daily task fires at the wrong hour.
	_ "time/tzdata"
)

var (
	startedAt = time.Now()
	logLevel  atomic.Int32
	version   = "dev" // set with -ldflags "-X main.version=..."
	// localLoc is the timezone the operator means. Resolved explicitly rather than trusting
	// the TZ environment variable: Go does not understand the POSIX form (TZ=CST-8) that
	// OpenWrt uses, so on a router without tzdata Go silently runs in UTC.
	localLoc = time.UTC
)

// resolveLocation picks the process timezone. time/tzdata is embedded (see the import in
// main), so the IANA name resolves even with no zoneinfo on the device; a fixed offset is
// the last resort for a name the database does not know.
func resolveLocation(name string, offsetHours int) *time.Location {
	if name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
		logf("WARN", "timezone %q not found in the embedded database; falling back to UTC%+d", name, offsetHours)
	}
	return time.FixedZone(fmt.Sprintf("UTC%+d", offsetHours), offsetHours*3600)
}

const (
	lvlDebug = iota
	lvlInfo
	lvlWarn
	lvlError
)

func logf(level, format string, args ...any) {
	var l int
	switch level {
	case "DEBUG":
		l = lvlDebug
	case "INFO":
		l = lvlInfo
	case "WARN":
		l = lvlWarn
	case "ERROR":
		l = lvlError
	default:
		l = lvlInfo
	}
	if l < int(logLevel.Load()) {
		return
	}
	fmt.Fprintf(os.Stderr, "%s [%s] %s\n", time.Now().In(localLoc).Format("2006-01-02 15:04:05"), level, fmt.Sprintf(format, args...))
}

type Server struct {
	cfg     *Config
	c       *Client
	pool    *Pool
	cat     *Catalog
	records *RecordLedger

	// A pending SMS login: which phone a code was requested for, so the code can be
	// handed back on its own (from a forwarder, or a file) without repeating the number.
	pendMu sync.Mutex
	pend   *pendingLogin

	// Daily credit tasks (check-in + task sweep) and what the last run did.
	daily dailyState
}

func main() {
	confPath := flag.String("conf", "", "path to a KEY=VALUE config file")
	showVer := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *confPath != "" {
		_ = os.Setenv("CONF_FILE", *confPath)
	}
	if *showVer {
		fmt.Printf("zhiju2api %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	// Memory is the scarce resource on the target router, so trade GC cycles for a
	// smaller heap unless the operator overrode it.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(30)
	}

	cfg := LoadConfig()
	localLoc = resolveLocation(cfg.TZName, cfg.TZOffsetHours)
	switch cfg.LogLevel {
	case "DEBUG":
		logLevel.Store(lvlDebug)
	case "WARN":
		logLevel.Store(lvlWarn)
	case "ERROR":
		logLevel.Store(lvlError)
	default:
		logLevel.Store(lvlInfo)
	}

	logf("INFO", "zhiju2api %s (%s/%s), upstream=%s", version, runtime.GOOS, runtime.GOARCH, cfg.Upstream)

	pool := NewPool(cfg)
	if pool.Empty() {
		logf("WARN", "no session token loaded from %s — every request will fail until one is supplied", cfg.TokensF)
	} else {
		for _, t := range pool.All() {
			logf("INFO", "token %s state=%s exp=%s", t.describe(), t.state(), t.Exp.Format(time.RFC3339))
		}
	}

	client := NewClient(cfg, pool)
	cat := NewCatalog(cfg, client)
	srv := &Server{cfg: cfg, c: client, pool: pool, cat: cat, records: NewRecordLedger(cfg.RecordsFile)}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if cfg.Discover {
		client.DiscoverVersion(ctx)
	}
	if err := cat.Refresh(ctx, true); err != nil {
		logf("WARN", "initial catalog fetch failed (will retry on demand): %v", err)
	}
	cancel()

	// Fill the price cache in the background so the first /v1/models call is fast.
	go func() {
		warmCtx, warmCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer warmCancel()
		srv.warmPrices(warmCtx)
	}()

	stop := make(chan struct{})
	go pool.StartMaintenance(client, stop)
	// Accept a verification code dropped into a file, so renewing needs no JSON at all.
	go srv.watchSMSCodeFile(stop)
	// The daily check-in pays credits; run it once shortly after boot (idempotent, so a
	// restart cannot miss the day) and then every day at SIGNIN_AT.
	go srv.startDailyTasks(stop)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", srv.auth(srv.handleModels))
	mux.HandleFunc("/v1/models/prices", srv.auth(srv.handleModelPrices))
	mux.HandleFunc("/v1/models/params", srv.auth(srv.handleModelParams))
	mux.HandleFunc("/v1/models/refresh", srv.auth(srv.handleRefresh))
	mux.HandleFunc("/v1/chat/completions", srv.auth(srv.handleChat))
	mux.HandleFunc("/v1/images/generations", srv.auth(srv.handleImageGenerations))
	mux.HandleFunc("/v1/images/edits", srv.auth(srv.handleImageEdits))
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/panel", srv.handlePanel)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Anything that is not one of the API routes gets the panel, so opening the
		// router's IP in a browser lands somewhere useful.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/panel", http.StatusFound)
	})
	mux.HandleFunc("/admin/tokens", srv.auth(srv.handleAdminTokens))
	mux.HandleFunc("/admin/account", srv.auth(srv.handleAccount))
	mux.HandleFunc("/admin/records", srv.auth(srv.handleRecords))
	mux.HandleFunc("/admin/records/delete", srv.auth(srv.handleRecordsDelete))
	mux.HandleFunc("/admin/tasks", srv.auth(srv.handleTasks))
	mux.HandleFunc("/admin/tasks/run", srv.auth(srv.handleTasksRun))
	mux.HandleFunc("/admin/push", srv.handlePush) // key may arrive in ?key= (cross-origin)
	mux.HandleFunc("/admin/session", srv.auth(srv.handleSession))
	mux.HandleFunc("/admin/login/sms", srv.auth(srv.handleLoginSMS))
	mux.HandleFunc("/admin/login/code", srv.auth(srv.handleLoginCode))
	mux.HandleFunc("/admin/login", srv.auth(srv.handleLogin))

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logf("INFO", "listening on %s", cfg.Listen)
		if len(cfg.APIKeys) == 0 {
			logf("WARN", "API_KEY is not set: the endpoint is open to anyone who can reach the port")
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("ERROR", "listen: %v", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logf("INFO", "shutting down")
	close(stop)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// ---- middleware --------------------------------------------------------------------

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.APIKeys) == 0 {
			next(w, r)
			return
		}
		token := bearer(r)
		for _, k := range s.cfg.APIKeys {
			if token == k {
				next(w, r)
				return
			}
		}
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid API key")
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("authorization")
	if h == "" {
		h = r.Header.Get("x-api-key")
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(h, "Bearer"), "bearer"))
}

// ---- endpoints ---------------------------------------------------------------------

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if err := s.cat.Refresh(r.Context(), false); err != nil && len(s.cat.Index()) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "cannot load the model catalog: "+err.Error())
		return
	}
	// Prices are included by default (they are cached, and the real name alone does not
	// tell you what a model costs). ?prices=0 skips the upstream lookups entirely.
	withPrices := r.URL.Query().Get("prices") != "0"
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": s.modelList(r.Context(), withPrices)})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	err := s.cat.Refresh(r.Context(), true)
	out := map[string]any{"ok": err == nil, "catalog": s.cat.Status()}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"version":      version,
		"uptime":       time.Since(startedAt).Round(time.Second).String(),
		"upstream":     s.cfg.Upstream,
		"chat_ver":     s.c.version(),
		"tokens":       s.pool.statuses(),
		"token_count":  len(s.pool.All()),
		"catalog":      s.cat.Status(),
		"daily":        s.dailySnapshot(),
		"goroutines":   runtime.NumGoroutine(),
		"mem_alloc_mb": memAllocMB(),
	})
}

// handleAdminTokens is the refresh path: POST a fresh JWT (plain text or JSON
// {"tokens":["..."]}) and the gateway starts using it immediately, no restart.
func (s *Server) handleAdminTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"tokens": s.pool.statuses()})
	case http.MethodPost, http.MethodPut:
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		tokens := extractTokens(raw)
		if len(tokens) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				"send a raw JWT, a newline separated list, or {\"tokens\":[...]}")
			return
		}
		if err := s.pool.Replace(tokens); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		_ = s.cat.Refresh(ctx, true)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token_count": len(s.pool.All()), "tokens": s.pool.statuses()})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "GET or POST")
	}
}

func extractTokens(raw []byte) []string {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "{") {
		var payload struct {
			Tokens []string `json:"tokens"`
			Token  string   `json:"token"`
		}
		if err := json.Unmarshal(raw, &payload); err == nil {
			if len(payload.Tokens) > 0 {
				return payload.Tokens
			}
			if payload.Token != "" {
				return []string{payload.Token}
			}
		}
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ---- helpers -----------------------------------------------------------------------

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	ct := r.Header.Get("content-type")
	if ct != "" && !strings.Contains(ct, "application/json") {
		return fmt.Errorf("expected application/json, got %q", ct)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("empty request body")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": typ},
	})
}

// writeUpstreamErr maps an upstream failure onto an OpenAI-shaped error.
func (s *Server) writeUpstreamErr(w http.ResponseWriter, err error) {
	var ue *errUpstream
	switch {
	case errors.As(err, &ue):
		status := http.StatusBadGateway
		switch ue.Ret {
		case 10033:
			status = http.StatusTooManyRequests
		case 10027, 10002:
			status = http.StatusNotFound
		case 2017:
			status = http.StatusPaymentRequired
		}
		writeOpenAIError(w, status, "upstream_error", ue.Error())
	case isAuthError(err):
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", err.Error())
	default:
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
	}
}

// acquire takes a slot from a concurrency gate, honouring the request context.
func (s *Server) acquire(ctx context.Context, gate chan struct{}) bool {
	select {
	case gate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-time.After(30 * time.Second):
		return false
	}
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

func memAllocMB() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.Alloc) / (1 << 20)
}
