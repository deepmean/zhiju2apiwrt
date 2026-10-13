package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Session upkeep endpoints.
//
// The router cannot mint a session token on its own — this API has no cookie, no
// refresh_token and no sliding expiry, so the only renewal path is a phone + SMS-code
// login. What it CAN do is run that login itself, so renewing means "send one command
// with the 6-digit code" instead of editing a file and restarting the service.

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
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"note": "verification code sent; POST /admin/login with {phone, code}",
	})
}

// handleLogin exchanges phone + SMS code for a fresh token, stores it, and starts using
// it immediately (no restart).
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

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	token, user, err := s.c.PhoneLogin(ctx, req.Phone, req.Code, req.InvitationCode)
	if err != nil {
		s.writeUpstreamErr(w, err)
		return
	}
	added, err := s.pool.Add(token)
	if err != nil {
		logf("WARN", "logged in but could not persist the token: %v", err)
	}

	// The new token may see a different catalog (plan changes), so refresh it now.
	if err := s.cat.Refresh(ctx, true); err == nil {
		go func() {
			warmCtx, warmCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer warmCancel()
			s.warmPrices(warmCtx)
		}()
	}

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
