package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Daily credit tasks.
//
// The web frontend never touches these routes — they belong to the app side (the config
// carries psychic_sign, and the icon font has a check-in glyph) — but they answer the web
// headers fine:
//
//	POST /v1/aimoss/task/sign    -> ret 1 on a fresh sign-in, ret 2016 "已经签到" otherwise
//	POST /v1/aimoss/task/list    -> {task_list:[...], all_task_max_integral, all_task_finish_status}
//	POST /v1/aimoss/task/finish  -> completes one task (payload assumed {task_id})
//
// The sign-in is verified against a real account. The finish payload is NOT: the task list
// was empty on the day this was written, so there was no item to observe. The sweep passes
// the id under every plausible key and logs whatever the server answers, so the first day a
// task appears will show exactly what it expects.

// SignInResult reports what the sign-in call did.
type SignInResult struct {
	Already bool
	Message string
}

// SignIn performs the daily check-in. ret=2016 means it was already done today, which is a
// normal outcome, not a failure.
func (c *Client) SignIn(ctx context.Context) (*SignInResult, error) {
	var data map[string]any
	err := c.call(ctx, kindV1, "/v1/aimoss/task/sign", map[string]any{}, &data)
	if err != nil {
		if ue, ok := err.(*errUpstream); ok && ue.Ret == 2016 {
			return &SignInResult{Already: true, Message: ue.Msg}, nil
		}
		return nil, err
	}
	return &SignInResult{Message: "signed in"}, nil
}

// DailyTask is one credit task from the list. Only the fields that matter are read, and
// the raw item is kept so an unfamiliar shape can still be logged.
type DailyTask struct {
	ID       int
	Title    string
	Integral int
	Status   int
	Raw      map[string]any
}

// TaskList reads the account's credit tasks.
func (c *Client) TaskList(ctx context.Context) ([]DailyTask, int, error) {
	var data struct {
		TaskList           []map[string]any `json:"task_list"`
		AllTaskMaxIntegral int              `json:"all_task_max_integral"`
		FinishStatus       int              `json:"all_task_finish_status"`
	}
	if err := c.call(ctx, kindV1, "/v1/aimoss/task/list", map[string]any{}, &data); err != nil {
		return nil, 0, err
	}
	out := make([]DailyTask, 0, len(data.TaskList))
	for _, raw := range data.TaskList {
		t := DailyTask{Raw: raw}
		t.ID = firstInt(raw, "task_id", "id", "task_code", "code")
		t.Title = firstStr(raw, "title", "task_name", "name", "desc")
		t.Integral = firstInt(raw, "integral", "integral_num", "change_num", "reward", "max_integral")
		t.Status = firstInt(raw, "status", "finish_status", "state")
		out = append(out, t)
	}
	return out, data.FinishStatus, nil
}

// FinishTask completes one task. The payload carries the id under several names because the
// item's shape has not been observed; the upstream ignores unknown fields.
func (c *Client) FinishTask(ctx context.Context, task DailyTask) (int, string, error) {
	body := map[string]any{}
	if task.ID != 0 {
		body["task_id"] = task.ID
		body["id"] = task.ID
	}
	for _, k := range []string{"task_id", "id", "task_code", "code"} {
		if v, ok := task.Raw[k]; ok {
			body[k] = v
		}
	}
	var data map[string]any
	err := c.call(ctx, kindV1, "/v1/aimoss/task/finish", body, &data)
	if err != nil {
		if ue, ok := err.(*errUpstream); ok {
			// 2016 is the family's "nothing to do / already done" code.
			return ue.Ret, ue.Msg, nil
		}
		return 0, "", err
	}
	return 1, "ok", nil
}

// ---- scheduling --------------------------------------------------------------------

// dailyState is what the panel and /health report about the daily job.
type dailyState struct {
	mu         sync.Mutex
	lastRun    time.Time
	lastDay    string
	lastResult string
	nextRun    time.Time
	enabled    bool
	at         string
}

func (s *Server) dailySnapshot() map[string]any {
	d := &s.daily
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]any{
		"enabled":     d.enabled,
		"at":          d.at,
		"last_run":    d.lastRun.Format(time.RFC3339),
		"last_day":    d.lastDay,
		"last_result": d.lastResult,
		"next_run":    d.nextRun.Format(time.RFC3339),
	}
}

// startDailyTasks runs the sign-in once shortly after boot (idempotent, so a restart cannot
// miss the day) and then every day at the configured time.
func (s *Server) startDailyTasks(stop <-chan struct{}) {
	at, err := parseClock(s.cfg.SignInAt)
	if err != nil {
		logf("WARN", "SIGNIN_AT=%q is not HH:MM; daily sign-in disabled", s.cfg.SignInAt)
		return
	}
	s.daily.mu.Lock()
	s.daily.enabled, s.daily.at = true, s.cfg.SignInAt
	s.daily.mu.Unlock()
	logf("INFO", "daily credit tasks enabled, running at %s local time", s.cfg.SignInAt)

	// Catch up on today, since the process may have been restarted after the scheduled time.
	select {
	case <-stop:
		return
	case <-time.After(20 * time.Second):
	}
	s.runDailyTasks("startup")

	for {
		next := nextClock(at)
		s.daily.mu.Lock()
		s.daily.nextRun = next
		s.daily.mu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			s.runDailyTasks("scheduled")
		}
	}
}

// runDailyTasks signs in and completes whatever credit tasks the account lists.
func (s *Server) runDailyTasks(reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var summary []string
	if s.cfg.SignInEnabled {
		res, err := s.c.SignIn(ctx)
		switch {
		case err != nil:
			summary = append(summary, "签到失败: "+err.Error())
			logf("WARN", "daily sign-in failed (%s): %v", reason, err)
		case res.Already:
			summary = append(summary, "签到: 今天已签过")
			logf("INFO", "daily sign-in: already done today")
		default:
			summary = append(summary, "签到: 成功")
			logf("INFO", "daily sign-in: success")
		}
	}

	tasks, finishStatus, err := s.c.TaskList(ctx)
	if err != nil {
		summary = append(summary, "任务列表读取失败: "+err.Error())
		logf("WARN", "task list failed: %v", err)
	} else {
		logf("INFO", "task list: %d item(s), all_task_finish_status=%d", len(tasks), finishStatus)
		done := 0
		for _, t := range tasks {
			if t.Status == 1 {
				continue
			}
			code, msg, ferr := s.c.FinishTask(ctx, t)
			if ferr != nil {
				logf("WARN", "finish task %d (%s) failed: %v", t.ID, t.Title, ferr)
				summary = append(summary, fmt.Sprintf("任务 %d 失败", t.ID))
				continue
			}
			logf("INFO", "finish task %d (%s, %d 灵力) -> ret=%d %s", t.ID, t.Title, t.Integral, code, msg)
			// Log the item shape once, so an unobserved payload can be corrected from the
			// log rather than by guessing again.
			if raw, jerr := json.Marshal(t.Raw); jerr == nil {
				logf("DEBUG", "task item shape: %s", raw)
			}
			if code == 1 {
				done++
			}
		}
		summary = append(summary, fmt.Sprintf("任务: %d 项, 完成 %d", len(tasks), done))
	}

	s.daily.mu.Lock()
	s.daily.lastRun = time.Now().In(localLoc)
	s.daily.lastDay = time.Now().In(localLoc).Format("2006-01-02")
	s.daily.lastResult = strings.Join(summary, "；")
	s.daily.mu.Unlock()
}

// parseClock reads an "HH:MM" local time.
func parseClock(s string) (time.Duration, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM")
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("expected HH:MM")
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

// nextClock returns the next occurrence of a wall-clock offset from local midnight, in the
// resolved timezone (not whatever the TZ environment variable happens to mean).
func nextClock(at time.Duration) time.Time {
	now := time.Now().In(localLoc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, localLoc)
	next := midnight.Add(at)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// handleTasks reports the daily job's state and what the account currently lists.
//
//	GET /admin/tasks
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"daily": s.dailySnapshot(),
		"note": "签到 = POST /v1/aimoss/task/sign（一天一次，实测 +50 灵力）；" +
			"任务 = /v1/aimoss/task/list + /v1/aimoss/task/finish（本账号当前列表为空）",
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	tasks, finishStatus, err := s.c.TaskList(ctx)
	if err != nil {
		out["task_error"] = err.Error()
	} else {
		out["tasks"] = tasks
		out["all_task_finish_status"] = finishStatus
	}
	if bal, _, err := s.c.Balance(ctx); err == nil {
		out["balance"] = bal
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTasksRun runs the daily job now (the panel's button, and how the first run was
// verified without waiting for the clock).
//
//	POST /admin/tasks/run
func (s *Server) handleTasksRun(w http.ResponseWriter, r *http.Request) {
	s.runDailyTasks("manual")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "daily": s.dailySnapshot()})
}

// ---- permissive field readers ------------------------------------------------------

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n := intOf(v); n != 0 {
				return n
			}
		}
	}
	return 0
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
