package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Token is one upstream session (a JWT sent as `x-token-id: Bearer <jwt>`).
//
// The upstream has no refresh_token and no sliding renewal: the JWT carries a fixed
// `exp`. "Keeping the login alive" therefore means (a) never losing the token, and
// (b) noticing the moment the server stops accepting it, so the operator can drop a
// fresh one in without restarting the gateway.
type Token struct {
	Label    string
	Value    string
	UID      int64
	Username string
	Exp      time.Time

	mu       sync.Mutex
	invalid  bool
	lastErr  string
	lastSeen time.Time // last time the upstream accepted it
}

func (t *Token) masked() string {
	v := t.Value
	if len(v) > 24 {
		return v[:12] + "..." + v[len(v)-6:]
	}
	return "***"
}

// state reports the token's health. Callers must hold t.mu.
func (t *Token) stateLocked() string {
	switch {
	case t.invalid:
		return "invalid"
	case t.Exp.IsZero():
		return "unknown"
	case time.Now().After(t.Exp):
		return "expired"
	default:
		return "ok"
	}
}

func (t *Token) state() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stateLocked()
}

// Pool holds the tokens and hands them out round-robin.
type Pool struct {
	cfg  *Config
	path string

	mu     sync.RWMutex
	tokens []*Token
	next   int
	mtime  time.Time
	size   int64
}

func NewPool(cfg *Config) *Pool {
	p := &Pool{cfg: cfg, path: cfg.TokensF}
	p.Reload()
	return p
}

// parseTokens accepts, per line:
//
//	<jwt>
//	label=<jwt>
//	label: <jwt>        (only the first ':' splits, so labels may contain ':')
//	# comment
func parseTokens(text string) []*Token {
	var out []*Token
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		label, val := "", line
		if i := strings.IndexByte(line, '='); i > 0 {
			label, val = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		} else if i := strings.IndexByte(line, ':'); i > 0 && !strings.HasPrefix(line, "ey") {
			label, val = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		}
		val = strings.TrimPrefix(strings.TrimSpace(val), "Bearer ")
		if val == "" {
			continue
		}
		t := &Token{Label: label, Value: val, lastSeen: time.Now()}
		if uid, user, exp, ok := parseJWT(val); ok {
			t.UID, t.Username, t.Exp = uid, user, exp
		}
		out = append(out, t)
	}
	return out
}

// parseJWT reads the (unverified) payload of the upstream token: we only use it for
// logging and expiry display, never for authorisation decisions.
func parseJWT(tok string) (uid int64, user string, exp time.Time, ok bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, "", time.Time{}, false
	}
	raw, err := b64urlDecode(parts[1])
	if err != nil {
		return 0, "", time.Time{}, false
	}
	var payload struct {
		UID      json.Number `json:"uid"`
		Username string      `json:"username"`
		Exp      json.Number `json:"exp"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, "", time.Time{}, false
	}
	if n, err := payload.UID.Int64(); err == nil {
		uid = n
	}
	if n, err := payload.Exp.Int64(); err == nil && n > 0 {
		exp = time.Unix(n, 0)
	}
	return uid, payload.Username, exp, true
}

// Reload re-reads the token file when it changed on disk. Cheap enough to poll.
func (p *Pool) Reload() (changed bool) {
	st, err := os.Stat(p.path)
	if err != nil {
		return false
	}
	p.mu.RLock()
	same := st.ModTime().Equal(p.mtime) && st.Size() == p.size && len(p.tokens) > 0
	p.mu.RUnlock()
	if same {
		return false
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return false
	}
	toks := parseTokens(string(raw))
	if len(toks) == 0 {
		return false
	}

	// Keep the runtime state (invalid flags, last error) for tokens that survived.
	p.mu.Lock()
	prev := map[string]*Token{}
	for _, t := range p.tokens {
		prev[t.Value] = t
	}
	for _, t := range toks {
		if old, ok := prev[t.Value]; ok {
			t.invalid = old.invalid
			t.lastErr = old.lastErr
			t.lastSeen = old.lastSeen
		}
	}
	p.tokens = toks
	p.mtime, p.size = st.ModTime(), st.Size()
	p.mu.Unlock()
	logf("INFO", "token pool reloaded: %d token(s) from %s", len(toks), p.path)
	return true
}

// Replace swaps the pool contents (admin push) and writes them back to disk so a
// restart keeps the fresh token.
func (p *Pool) Replace(values []string) error {
	var sb strings.Builder
	var toks []*Token
	for _, v := range values {
		v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Bearer "))
		if v == "" {
			continue
		}
		t := &Token{Value: v, lastSeen: time.Now()}
		if uid, user, exp, ok := parseJWT(v); ok {
			t.UID, t.Username, t.Exp = uid, user, exp
		}
		toks = append(toks, t)
		sb.WriteString(v)
		sb.WriteString("\n")
	}
	if len(toks) == 0 {
		return fmt.Errorf("no usable token in payload")
	}
	if err := os.WriteFile(p.path, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	p.mu.Lock()
	p.tokens = toks
	if st, err := os.Stat(p.path); err == nil {
		p.mtime, p.size = st.ModTime(), st.Size()
	}
	p.mu.Unlock()
	logf("INFO", "token pool replaced via admin API: %d token(s)", len(toks))
	return nil
}

func (p *Pool) All() []*Token {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]*Token(nil), p.tokens...)
}

func (p *Pool) Empty() bool { return len(p.All()) == 0 }

// Pick returns the next usable token, skipping ones the upstream rejected or whose
// exp has already passed.
func (p *Pool) Pick() *Token {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.tokens)
	if n == 0 {
		return nil
	}
	now := time.Now()
	for i := 0; i < n; i++ {
		t := p.tokens[(p.next+i)%n]
		t.mu.Lock()
		bad := t.invalid || (!t.Exp.IsZero() && now.After(t.Exp))
		t.mu.Unlock()
		if bad {
			continue
		}
		p.next = (p.next + i + 1) % n
		return t
	}
	// Everything looks bad — hand out the first anyway so the caller can surface a
	// real upstream error instead of a local "no token" guess.
	return p.tokens[p.next%n]
}

func (p *Pool) MarkOK(t *Token) {
	t.mu.Lock()
	t.invalid, t.lastErr, t.lastSeen = false, "", time.Now()
	t.mu.Unlock()
}

func (p *Pool) MarkInvalid(t *Token, reason string) {
	t.mu.Lock()
	t.invalid, t.lastErr = true, reason
	t.mu.Unlock()
	logf("WARN", "token %s rejected by upstream: %s", t.describe(), reason)
}

func (t *Token) describe() string {
	name := t.Label
	if name == "" && t.Username != "" {
		name = fmt.Sprintf("%s#%d", t.Username, t.UID)
	}
	if name == "" {
		name = t.masked()
	}
	return name
}

// statuses is the /health payload.
func (p *Pool) statuses() []map[string]any {
	var out []map[string]any
	for _, t := range p.All() {
		t.mu.Lock()
		m := map[string]any{
			"label":     t.describe(),
			"state":     t.stateLocked(),
			"exp":       t.Exp.Format(time.RFC3339),
			"invalid":   t.invalid,
			"last_seen": t.lastSeen.Format(time.RFC3339),
		}
		if t.lastErr != "" {
			m["last_error"] = t.lastErr
		}
		t.mu.Unlock()
		out = append(out, m)
	}
	return out
}

// StartMaintenance polls the token file for changes and pings each token so the
// operator sees a dead session in /health before a client request fails on it.
func (p *Pool) StartMaintenance(c *Client, stop <-chan struct{}) {
	reload := time.NewTicker(p.cfg.TokenReloadEvery)
	defer reload.Stop()
	beat := time.NewTicker(p.cfg.HeartbeatInterval)
	defer beat.Stop()
	for {
		select {
		case <-stop:
			return
		case <-reload.C:
			p.Reload()
		case <-beat.C:
			for _, t := range p.All() {
				go func(t *Token) {
					if err := c.Ping(t); err != nil {
						if isAuthError(err) {
							p.MarkInvalid(t, err.Error())
						} else {
							logf("WARN", "heartbeat %s: %v", t.describe(), err)
						}
					} else {
						p.MarkOK(t)
					}
				}(t)
			}
		}
	}
}
