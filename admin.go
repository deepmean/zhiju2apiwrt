package main

import (
	"context"
	"encoding/json"
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
