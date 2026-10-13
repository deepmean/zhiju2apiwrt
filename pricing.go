package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Price is what the upstream would charge for one request, asked BEFORE running it.
//
// Two numbers matter and they are not the same:
//   - ChangeNum  is the list price in 灵力 (what a non-VIP pays).
//   - Effective  is what this account actually pays (a VIP plan makes most of it 0).
//
// Annotating both is the point: it lets a caller see "香蕉 Pro costs 420, 升智-1 costs 80
// and is unlimited for this account" before a single credit is spent.
type Price struct {
	GroupKey    string `json:"model_key"`
	Name        string `json:"name,omitempty"`
	ContentType string `json:"content_type"`
	ModelID     int    `json:"model_id"`
	PolicyName  string `json:"policy_name,omitempty"`

	ChangeNum int  `json:"change_num"`
	Effective int  `json:"effective_change_num"`
	Original  int  `json:"original_change_num,omitempty"`
	IsVIP     bool `json:"is_vip"`

	Unlimited   bool      `json:"unlimited,omitempty"`
	RemainCount int       `json:"quota_remain,omitempty"`
	Fetched     time.Time `json:"fetched"`
	Err         string    `json:"error,omitempty"`
}

type priceCache struct {
	mu  sync.Mutex
	m   map[string]*Price
	ttl time.Duration
}

func newPriceCache(ttl time.Duration) *priceCache {
	return &priceCache{m: map[string]*Price{}, ttl: ttl}
}

func priceKey(groupKey, contentType string, modelID int) string {
	return fmt.Sprintf("%s|%s|%d", groupKey, contentType, modelID)
}

func (pc *priceCache) get(key string) (*Price, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	p, ok := pc.m[key]
	if !ok || time.Since(p.Fetched) > pc.ttl {
		return nil, false
	}
	return p, true
}

func (pc *priceCache) put(key string, p *Price) {
	pc.mu.Lock()
	pc.m[key] = p
	pc.mu.Unlock()
}

// PriceOf asks the upstream what one request of this model costs. The answer is cached
// because it changes rarely and every annotation would otherwise cost a round trip.
func (c *Client) PriceOf(ctx context.Context, groupKey, contentType string, modelID int, capacities string) (*Price, error) {
	if groupKey == "" {
		return nil, fmt.Errorf("empty model key")
	}
	if capacities == "" {
		capacities = contentType
	}
	key := priceKey(groupKey, contentType, modelID)
	if p, ok := c.prices.get(key); ok {
		return p, nil
	}

	var raw map[string]any
	err := c.call(ctx, kindV1, "/v1/integral/getpolicy", map[string]any{
		"group_key":    groupKey,
		"content_type": contentType,
		"voice_key":    "",
		"model_id":     modelID,
		"capacities":   capacities,
	}, &raw)
	if err != nil {
		return nil, err
	}

	p := &Price{
		GroupKey:    groupKey,
		ContentType: contentType,
		ModelID:     modelID,
		Fetched:     time.Now(),
		ChangeNum:   intOf(raw["change_num"]),
		Effective:   intOf(raw["change_num"]),
		PolicyName:  strOf(raw["policy_name"]),
	}
	if np, ok := raw["new_policy"].(map[string]any); ok {
		if n := strOf(np["name"]); n != "" {
			p.PolicyName = n
		}
		if res, ok := np["change_num_result"].(map[string]any); ok {
			// The VIP column is what this account pays; when the upstream omits it, the
			// list price is the honest answer.
			if v, ok := res["vip_change_num_sum"]; ok && v != nil {
				p.Effective = intOf(v)
			}
			p.Original = intOf(res["original_change_num_sum"])
			if b, ok := res["is_vip"].(bool); ok {
				p.IsVIP = b
			}
		}
	}
	c.prices.put(key, p)
	return p, nil
}

// annotate looks the estimate up, logs it, and exposes it on the response headers so a
// caller can see the price of the request it is about to get. A pricing failure never
// blocks the request — the cost is simply reported as unknown.
func (s *Server) annotate(ctx context.Context, w http.ResponseWriter, modelKey, contentType string, modelID int) *Price {
	p, err := s.c.PriceOf(ctx, modelKey, contentType, modelID, contentType)
	if err != nil {
		// Quota-billed models (Midjourney) have no 灵力 price row, so the billing module
		// answers with a generic error. Report the quota rather than a scary warning.
		if n, ok := s.cat.RemainOf(modelKey); ok {
			logf("INFO", "cost estimate: %s %s -> no 灵力 price upstream (quota-billed, remain=%d)", modelKey, contentType, n)
		} else {
			logf("WARN", "price lookup for %s failed: %v", modelKey, err)
		}
		return nil
	}
	logf("INFO", "cost estimate: %s %s -> list=%d 灵力, this account pays=%d 灵力 (%s)",
		modelKey, contentType, p.ChangeNum, p.Effective, p.PolicyName)
	if w != nil {
		w.Header().Set("x-zhiju-list-cost", fmt.Sprintf("%d", p.ChangeNum))
		w.Header().Set("x-zhiju-estimated-cost", fmt.Sprintf("%d", p.Effective))
	}
	return p
}

// guard refuses a request whose estimate exceeds the configured ceiling, naming a
// cheaper alternative so the caller does not have to go looking. MAX_COST_PER_REQUEST=0
// (the default) means annotate-only.
func (s *Server) guard(w http.ResponseWriter, p *Price) bool {
	limit := s.cfg.MaxCostPerRequest
	if limit <= 0 || p == nil || p.Effective <= limit {
		return true
	}
	hint := ""
	if key, cost := s.cheapest(p.ContentType); key != "" {
		hint = fmt.Sprintf("; cheapest %s model here is %s at %d 灵力", p.ContentType, key, cost)
	}
	writeOpenAIError(w, http.StatusPaymentRequired, "insufficient_quota",
		fmt.Sprintf("%s would cost %d 灵力, above MAX_COST_PER_REQUEST=%d%s", p.GroupKey, p.Effective, limit, hint))
	return false
}

// cheapest returns the lowest-priced model of a content type (cached prices only after
// the first call, so a rejection does not trigger a burst of upstream lookups).
func (s *Server) cheapest(contentType string) (string, int) {
	best, bestCost := "", -1
	for _, m := range s.cat.ModelsByContentType(contentType) {
		p, err := s.c.PriceOf(context.Background(), m.Key, contentType, s.cat.IDOf(m.Key), contentType)
		if err != nil || p == nil {
			continue
		}
		if bestCost < 0 || p.Effective < bestCost {
			best, bestCost = m.Key, p.Effective
		}
	}
	return best, bestCost
}

// modelList builds the /v1/models payload. Every entry carries the real upstream model
// name and, when prices are requested, the server-quoted cost — so one call answers
// "what can I call, what is it really, and what will it cost me".
//
//	GET /v1/models            full list with names and prices (prices are cached)
//	GET /v1/models?prices=0   names only, no upstream price lookups
func (s *Server) modelList(ctx context.Context, withPrices bool) []map[string]any {
	idx := s.cat.Index()
	now := time.Now().Unix()
	out := make([]map[string]any, 0, len(idx))
	for _, e := range idx {
		entry := map[string]any{
			"id":        e.OpenAI,
			"object":    "model",
			"created":   now,
			"owned_by":  "zhijuai",
			"unlimited": e.Unlimited,
		}
		if e.Name != "" {
			entry["name"] = e.Name
		}
		if e.Key != "" {
			entry["upstream_key"] = e.Key
		}
		if e.ID > 0 {
			entry["upstream_id"] = e.ID
		}
		if e.Family != "" {
			entry["family"] = e.Family
		}
		if e.Capability != "" {
			entry["capability"] = e.Capability
		}
		if withPrices && e.Key != "" {
			contentType := "text"
			if e.Capability != "chat" {
				contentType = "draw"
			}
			if p, err := s.c.PriceOf(ctx, e.Key, contentType, e.ID, contentType); err == nil && p != nil {
				entry["change_num"] = p.ChangeNum
				entry["effective_change_num"] = p.Effective
				entry["price_available"] = true
			} else {
				// Not every model has a 灵力 price row: quota-billed ones (Midjourney is
				// remain_count=-1) make the billing module answer with a generic error.
				// Say so instead of leaving the fields blank.
				entry["price_available"] = false
				entry["price_note"] = "upstream returned no price policy for this model (quota-billed)"
			}
		}
		if e.Key != "" {
			if n, ok := s.cat.RemainOf(e.Key); ok {
				entry["quota_remain"] = n
			}
		}
		// Image models advertise what they accept, straight from the upstream table, so a
		// client can pick a valid ratio/resolution instead of guessing one.
		if e.Capability != "chat" && e.Key != "" {
			if p, ok := s.c.ParamsOf(ctx, e.Key); ok && p != nil {
				entry["aspect_ratios"] = p.AspectRatios
				entry["default_aspect_ratio"] = p.DefaultAspect
				entry["image_sizes"] = p.ImageSizes
				entry["default_image_size"] = p.DefaultSize
				if p.PromptLimit > 0 {
					entry["prompt_limit"] = p.PromptLimit
				}
			}
		}
		out = append(out, entry)
	}
	return out
}

// warmPrices fills the price cache in the background so the first client call is fast.
func (s *Server) warmPrices(ctx context.Context) {
	for _, m := range s.cat.TextModels() {
		if ctx.Err() != nil {
			return
		}
		_, _ = s.c.PriceOf(ctx, m.Key, "text", s.cat.IDOf(m.Key), "text")
	}
	for _, m := range s.cat.ImageModels() {
		if ctx.Err() != nil {
			return
		}
		_, _ = s.c.PriceOf(ctx, m.Key, "draw", s.cat.IDOf(m.Key), "draw")
	}
	logf("INFO", "price cache warmed")
}

// handleModelParams serves the raw per-model parameter table: the aspect ratios and
// resolution tiers each model accepts, its defaults, its prompt limit, and (for
// Midjourney) the extra knobs with their ranges.
//
//	GET /v1/models/params
func (s *Server) handleModelParams(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"note":   "values are declared per model upstream; image_size is usually a single fixed tier, so send it only if the model lists it",
		"data":   s.c.ParamsTable(r.Context()),
	})
}

// handleModelPrices serves the annotated table: every model with its list price, this
// account's effective price, and whether it is unlimited.
func (s *Server) handleModelPrices(w http.ResponseWriter, r *http.Request) {
	if err := s.cat.Refresh(r.Context(), false); err != nil && len(s.cat.OpenAIList()) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	type target struct {
		key, contentType, name string
		id                     int
	}
	var targets []target
	for _, m := range s.cat.TextModels() {
		targets = append(targets, target{m.Key, "text", m.Name, s.cat.IDOf(m.Key)})
	}
	for _, m := range s.cat.ImageModels() {
		targets = append(targets, target{m.Key, "draw", m.Name, s.cat.IDOf(m.Key)})
	}

	out := make([]*Price, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p, err := s.c.PriceOf(r.Context(), t.key, t.contentType, t.id, t.contentType)
			if err != nil {
				// No price row upstream (quota-billed models). Report the quota instead,
				// which is the only signal the account gives for them.
				entry := &Price{GroupKey: t.key, Name: t.name, ContentType: t.contentType, ModelID: t.id, Err: err.Error()}
				if n, ok := s.cat.RemainOf(t.key); ok {
					entry.Unlimited = n < 0
					entry.RemainCount = n
				}
				out[i] = entry
				return
			}
			p.Name = t.name
			p.Unlimited = s.cat.IsUnlimited(t.key)
			if n, ok := s.cat.RemainOf(t.key); ok {
				p.RemainCount = n
				if n < 0 {
					p.Unlimited = true
				}
			}
			out[i] = p
		}(i, t)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool {
		if out[i].ContentType != out[j].ContentType {
			return out[i].ContentType < out[j].ContentType
		}
		return out[i].Effective < out[j].Effective
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"note":   "effective_change_num is what THIS account pays; change_num is the list price. 灵力 = upstream credits.",
		"data":   out,
	})
}

// ---- small JSON helpers ------------------------------------------------------------

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		var out int
		_, _ = fmt.Sscanf(strings.TrimSpace(n), "%d", &out)
		return out
	}
	return 0
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
