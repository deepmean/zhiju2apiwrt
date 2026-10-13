package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Session upkeep endpoints.
//
// The router cannot mint a session token on its own — this API has no cookie, no
// refresh_token and no sliding expiry, so the only renewal path is a phone + SMS-code
// login. What it CAN do is run that login itself, and accept the code from wherever it
// arrives, so "renew" is one command (or none at all) instead of editing a file.

// pendingLogin remembers the phone a code was requested for, so the code can be handed
// back without repeating the number.
type pendingLogin struct {
	phone string
	at    time.Time
}

// loginPendingTTL is how long a requested code stays usable. SMS codes expire quickly
// upstream; this only bounds how long we keep the phone number around.
const loginPendingTTL = 10 * time.Minute

func (s *Server) setPending(phone string) {
	s.pendMu.Lock()
	s.pend = &pendingLogin{phone: phone, at: time.Now()}
	s.pendMu.Unlock()
}

func (s *Server) takePending() (string, bool) {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	if s.pend == nil || time.Since(s.pend.at) > loginPendingTTL {
		s.pend = nil
		return "", false
	}
	phone := s.pend.phone
	return phone, phone != ""
}

// handleLoginSMS asks the upstream to text a verification code.
//
//	POST /admin/login/sms   {"phone":"136xxxxxxxx"}
func (s *Server) handleLoginSMS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
		Type  int    `json:"type"`
	}
	if err := decodeJSON(w, r, &req, 64<<10); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req.Phone = strings.TrimSpace(req.Phone)
	if req.Phone == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "phone is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.c.SendLoginSMS(ctx, req.Phone, req.Type); err != nil {
		s.writeUpstreamErr(w, err)
		return
	}
	s.setPending(req.Phone)
	logf("INFO", "verification code requested for %s; waiting for the code via /admin/login/code or %s",
		maskPhone(req.Phone), s.cfg.SMSCodeFile)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"note":  "code sent — hand it back with POST /admin/login/code {\"code\":\"...\"}, or write it to " + s.cfg.SMSCodeFile,
		"phone": maskPhone(req.Phone),
	})
}

// handleLoginCode completes a login using the code alone, for the phone the last
// /admin/login/sms was called with. This is the endpoint an SMS-forwarding app on the
// phone can hit: it only ever has the code, never the number.
//
//	POST /admin/login/code   {"code":"123456"}
func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(w, r, &req, 64<<10); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "code is required")
		return
	}
	phone, ok := s.takePending()
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"no pending login: call POST /admin/login/sms first (a code stays usable for "+loginPendingTTL.String()+")")
		return
	}
	s.finishLogin(w, r, phone, req.Code, "")
}

// handleLogin exchanges phone + SMS code for a fresh token, stores it, and starts using
// it immediately (no restart). Works standalone, without a prior /admin/login/sms.
//
//	POST /admin/login   {"phone":"136xxxxxxxx","code":"123456"}
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone          string `json:"phone"`
		Code           string `json:"code"`
		InvitationCode string `json:"invitation_code"`
	}
	if err := decodeJSON(w, r, &req, 64<<10); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if strings.TrimSpace(req.Phone) == "" || strings.TrimSpace(req.Code) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "phone and code are required")
		return
	}
	s.finishLogin(w, r, req.Phone, req.Code, req.InvitationCode)
}

// finishLogin is the shared tail of every login route.
func (s *Server) finishLogin(w http.ResponseWriter, r *http.Request, phone, code, invitation string) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	token, user, err := s.c.PhoneLogin(ctx, phone, code, invitation)
	if err != nil {
		s.writeUpstreamErr(w, err)
		return
	}
	added, err := s.pool.Add(token)
	if err != nil {
		logf("WARN", "logged in but could not persist the token: %v", err)
	}
	s.clearPending()

	// The new token may see a different catalog (plan changes), so refresh it now.
	if err := s.cat.Refresh(ctx, true); err == nil {
		go func() {
			warmCtx, warmCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer warmCancel()
			s.warmPrices(warmCtx)
		}()
	}
	logf("INFO", "login succeeded for %s: %d token(s) added, %d total", maskPhone(phone), added, len(s.pool.All()))

	out := map[string]any{
		"ok":          true,
		"added":       added,
		"token_count": len(s.pool.All()),
		"tokens":      s.pool.statuses(),
	}
	if user != nil {
		out["user"] = summariseUser(user)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) clearPending() {
	s.pendMu.Lock()
	s.pend = nil
	s.pendMu.Unlock()
}

// watchSMSCodeFile lets the code arrive as a plain file, so renewing needs no JSON:
//
//	ssh router 'echo 123456 > /tmp/zhiju-sms-code'
//
// The file is read only while a login is pending and removed as soon as it is consumed.
func (s *Server) watchSMSCodeFile(stop <-chan struct{}) {
	path := s.cfg.SMSCodeFile
	if path == "" {
		return
	}
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if _, ok := s.peekPending(); !ok {
				continue
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			code := strings.TrimSpace(string(raw))
			code = strings.Trim(code, `"'`)
			if code == "" {
				continue
			}
			_ = os.Remove(path)
			phone, ok := s.takePending()
			if !ok {
				continue
			}
			logf("INFO", "verification code picked up from %s; completing login for %s", path, maskPhone(phone))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			token, _, err := s.c.PhoneLogin(ctx, phone, code, "")
			cancel()
			if err != nil {
				logf("WARN", "login with the file-provided code failed: %v", err)
				// Put the pending state back so a retry is possible.
				s.setPending(phone)
				continue
			}
			if added, err := s.pool.Add(token); err != nil {
				logf("WARN", "logged in but could not persist the token: %v", err)
			} else {
				logf("INFO", "login from file succeeded: %d token(s) added, %d total", added, len(s.pool.All()))
			}
			s.clearPending()
			rctx, rcancel := context.WithTimeout(context.Background(), 60*time.Second)
			if err := s.cat.Refresh(rctx, true); err == nil {
				go func() {
					warmCtx, warmCancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer warmCancel()
					s.warmPrices(warmCtx)
				}()
			}
			rcancel()
		}
	}
}

func (s *Server) peekPending() (string, bool) {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	if s.pend == nil || time.Since(s.pend.at) > loginPendingTTL {
		return "", false
	}
	return s.pend.phone, true
}

// handlePush is the endpoint the panel's bookmarklet targets: the token arrives as the
// raw request body, and the API key may come from ?key= because a cross-origin POST from
// the site's page cannot set an Authorization header without a CORS preflight.
//
//	POST /admin/push?key=sk-...      body: <jwt>
//
// It is deliberately tolerant (GET with ?token= works too) and answers with permissive
// CORS headers, so the caller can read the result when the browser allows it.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("access-control-allow-origin", "*")
	w.Header().Set("access-control-allow-headers", "authorization,content-type")
	w.Header().Set("access-control-allow-methods", "GET,POST,OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Key: header first (normal callers), then ?key= (bookmarklet from another origin).
	if len(s.cfg.APIKeys) > 0 {
		key := bearer(r)
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		ok := false
		for _, k := range s.cfg.APIKeys {
			if key == k {
				ok = true
				break
			}
		}
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", "invalid API key")
			return
		}
	}

	var raw []byte
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		raw = b
	}
	tokens := extractTokens(raw)
	if v := strings.TrimSpace(r.URL.Query().Get("token")); v != "" {
		tokens = append(tokens, v)
	}
	if len(tokens) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "send the JWT as the body, or as ?token=")
		return
	}

	added, err := s.pool.Add(tokens...)
	if err != nil {
		logf("WARN", "pushed token could not be persisted: %v", err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_ = s.cat.Refresh(ctx, true)

	logf("INFO", "token pushed via /admin/push: %d added, %d total", added, len(s.pool.All()))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"added":       added,
		"token_count": len(s.pool.All()),
		"tokens":      s.pool.statuses(),
	})
}

// handleAccount reports the account behind the current token: identity, plan, balance and
// the per-model quota the account catalog carries. This is what the panel shows, so the
// operator can see which account is actually being spent.
//
//	GET /admin/account
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	var acct struct {
		Nick      string `json:"nick"`
		MID       int64  `json:"mid"`
		Phone     string `json:"phone"`
		Platform  string `json:"platform"`
		Channel   string `json:"channel"`
		UserType  int    `json:"user_type"`
		IsNewVIP  bool   `json:"is_new_vip"`
		Integral  struct {
			Balance int `json:"balance"`
		} `json:"integral"`
		VipItem struct {
			VIPName       string `json:"vip_name"`
			VIPLevelName  string `json:"vip_level_name"`
			VIPExpireName string `json:"vip_expire_name"`
			VIPKey        string `json:"vip_key"`
		} `json:"vip_item"`
		Subscriptions struct {
			InSubscription     bool `json:"in_subscription"`
			SubscriptionStatus int  `json:"subscription_status"`
			ActivePlans        int  `json:"active_plans"`
			ModelsRemain       []struct {
				ModelName   string `json:"model_name"`
				ModelID     int    `json:"model_id"`
				RemainCount int    `json:"remain_count"`
			} `json:"models_remain"`
		} `json:"subscriptions"`
	}
	if err := s.c.call(ctx, kindV1, "/v1/account/get", map[string]any{}, &acct); err != nil {
		s.writeUpstreamErr(w, err)
		return
	}

	unlimited, exhausted := 0, 0
	type quota struct {
		Name   string `json:"name"`
		ID     int    `json:"id"`
		Remain int    `json:"remain"`
	}
	quotas := make([]quota, 0, len(acct.Subscriptions.ModelsRemain))
	for _, m := range acct.Subscriptions.ModelsRemain {
		switch {
		case m.RemainCount < 0:
			unlimited++
		case m.RemainCount == 0:
			exhausted++
		}
		quotas = append(quotas, quota{Name: m.ModelName, ID: m.ModelID, Remain: m.RemainCount})
	}

	out := map[string]any{
		"nick":                acct.Nick,
		"mid":                 acct.MID,
		"phone":               maskPhone(acct.Phone),
		"platform":            acct.Platform,
		"channel":             acct.Channel,
		"user_type":           acct.UserType,
		"is_new_vip":          acct.IsNewVIP,
		"vip_name":            acct.VipItem.VIPName,
		"vip_level":           acct.VipItem.VIPLevelName,
		"vip_expire":          acct.VipItem.VIPExpireName,
		"vip_key":             acct.VipItem.VIPKey,
		"in_subscription":     acct.Subscriptions.InSubscription,
		"subscription_status": acct.Subscriptions.SubscriptionStatus,
		"active_plans":        acct.Subscriptions.ActivePlans,
		"balance":             acct.Integral.Balance,
		"models_total":        len(quotas),
		"models_unlimited":    unlimited,
		"models_exhausted":    exhausted,
		"quotas":              quotas,
	}
	if bal, expire, err := s.c.Balance(ctx); err == nil {
		out["balance"] = bal
		out["balance_expire"] = expire
	}
	writeJSON(w, http.StatusOK, out)
}

// maskPhone keeps the number out of logs and responses beyond its last four digits.
func maskPhone(phone string) string {
	if len(phone) < 7 {
		return "***"
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

// handleSession reports what the account currently is: balance, token states, version.
//
//	GET /admin/session
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	out := map[string]any{
		"tokens":   s.pool.statuses(),
		"chat_ver": s.c.version(),
		"catalog":  s.cat.Status(),
	}
	if balance, expire, err := s.c.Balance(ctx); err == nil {
		out["balance"] = balance
		out["balance_expire"] = expire
	} else {
		out["balance_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// summariseUser keeps the useful identity fields and drops everything else, so the
// response never echoes a wall of account data (and no phone number in full).
func summariseUser(user map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"mid", "nick", "user_type", "platform", "channel", "vip_item"} {
		if v, ok := user[k]; ok {
			out[k] = v
		}
	}
	if phone, ok := user["phone"].(string); ok && len(phone) >= 7 {
		out["phone"] = phone[:3] + "****" + phone[len(phone)-4:]
	}
	if subs, ok := user["subscriptions"].(map[string]any); ok {
		out["subscriptions"] = map[string]any{
			"in_subscription":   subs["in_subscription"],
			"subscription_status": subs["subscription_status"],
			"vip_type":          subs["vip_type"],
			"vip_key":           subs["vip_key"],
		}
	}
	return out
}

var _ = json.Marshal
