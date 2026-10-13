package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Model is one entry of the upstream catalog (/v1/models/snapshot/all).
type Model struct {
	Key         string         `json:"model_key"`
	Name        string         `json:"model_name"`
	Type        string         `json:"model_type"` // text | draw | repair | video | music | voice | tool
	Desc        string         `json:"model_desc"`
	DrawUI      string         `json:"draw_ui_type"`
	IsDefault   bool           `json:"is_default"`
	IsUnlimited bool           `json:"is_unlimited"`
	Caps        map[string]any `json:"capacities"`
}

type modelSnapshot struct {
	Models map[string][]Model `json:"models"`
}

// Resolved tells the handlers how to drive one OpenAI-facing model name.
type Resolved struct {
	OpenAI     string // the name the client asked for, e.g. gptimage2-image
	Kind       string // chat | mj | repair
	Key        string // upstream model_key, e.g. gptimage2
	ID         int    // numeric model_id the image routes require
	Name       string // upstream display name, e.g. 升智-2
	Desc       string // upstream one-line description
	Family     string // upstream model_type: text | draw | repair
	Capability string // chat | image | image-edit (what the -suffix selects)
}

// Catalog caches the upstream model list plus the model_key -> numeric id map.
type Catalog struct {
	cfg *Config
	c   *Client

	mu          sync.RWMutex
	text        []Model
	draw        []Model
	repair      []Model
	idByKey     map[string]int
	remainByKey map[string]int // model_key -> remaining quota (-1 = unlimited, 0 = none left)
	byName      map[string]Resolved
	fetched     time.Time
	lastErr     string
	overrides   map[string]int
}

func NewCatalog(cfg *Config, c *Client) *Catalog {
	return &Catalog{
		cfg:       cfg,
		c:         c,
		idByKey:   map[string]int{},
		byName:    map[string]Resolved{},
		overrides: loadModelMap(cfg.ModelMapF),
	}
}

// loadModelMap reads `model_key=model_id` overrides, the escape hatch for any model the
// automatic resolution cannot place.
func loadModelMap(path string) map[string]int {
	out := map[string]int{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexAny(line, "=:")
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		if id, err := strconv.Atoi(strings.TrimSpace(line[i+1:])); err == nil {
			out[key] = id
		}
	}
	return out
}

func (cat *Catalog) Refresh(ctx context.Context, force bool) error {
	cat.mu.RLock()
	fresh := time.Since(cat.fetched) < cat.cfg.ModelCacheTTL && len(cat.text) > 0
	cat.mu.RUnlock()
	if fresh && !force {
		return nil
	}

	var snap modelSnapshot
	if err := cat.c.call(ctx, kindChat, "/v1/models/snapshot/all", map[string]any{"check_params": nil}, &snap); err != nil {
		cat.mu.Lock()
		cat.lastErr = err.Error()
		cat.mu.Unlock()
		return err
	}

	ids := map[string]int{}
	names := map[string]string{}
	drawCapable := map[string]bool{}
	// 1) The web client's own config carries the full model table (model_key + model_id)
	// for every family, which is the freshest and most complete source there is.
	if cfgMap, err := cat.c.CfgList(ctx); err == nil {
		collectModelIDs(cfgMap, ids)
		collectModelTable(cfgMap, names, drawCapable)
	} else {
		logf("WARN", "cfg/list unavailable for model ids: %v", err)
	}

	// 2) The account's own repair history records the group key and id each past task
	// actually ran with; it fills gaps cfg/list does not know about yet.
	var history struct {
		Items []struct {
			ModelGroupKey string `json:"model_group_key"`
			ModelGroupID  int    `json:"model_group_id"`
		} `json:"items"`
	}
	if err := cat.c.call(ctx, kindRepair, "/v1/aimoss/repair/history",
		map[string]any{"keyword": "", "limit": 1000, "cursor": "0"}, &history); err == nil {
		for _, it := range history.Items {
			if it.ModelGroupKey != "" && it.ModelGroupID > 0 {
				if _, seen := ids[it.ModelGroupKey]; !seen {
					ids[it.ModelGroupKey] = it.ModelGroupID
				}
			}
		}
	}

	// 3) The repair home groups carry ids for the models it showcases.
	var home struct {
		Items []struct {
			ModelGroupKey string `json:"model_group_key"`
			ModelGroupID  int    `json:"model_group_id"`
		} `json:"items"`
	}
	if err := cat.c.call(ctx, kindRepair, "/v1/aimoss/repair/home_v2", map[string]any{}, &home); err == nil {
		for _, it := range home.Items {
			if it.ModelGroupKey != "" && it.ModelGroupID > 0 {
				if _, seen := ids[it.ModelGroupKey]; !seen {
					ids[it.ModelGroupKey] = it.ModelGroupID
				}
			}
		}
	}

	// 4) The account catalog pairs display names with ids, and carries the per-model
	// remaining quota — the only price-like signal some models have at all: Midjourney
	// is remain_count=-1, i.e. billed by task count rather than 灵力.
	var acct struct {
		Subscriptions struct {
			ModelsRemain []struct {
				ModelName   string `json:"model_name"`
				ModelID     int    `json:"model_id"`
				RemainCount int    `json:"remain_count"`
			} `json:"models_remain"`
		} `json:"subscriptions"`
	}
	remain := map[string]int{}
	if err := cat.c.call(ctx, kindV1, "/v1/account/get", map[string]any{}, &acct); err == nil {
		nameToID := map[string]int{}
		remainByID := map[int]int{}
		for _, m := range acct.Subscriptions.ModelsRemain {
			nameToID[normName(m.ModelName)] = m.ModelID
			remainByID[m.ModelID] = m.RemainCount
		}
		for _, group := range [][]Model{snap.Models["draw"], snap.Models["repair"]} {
			for _, m := range group {
				if _, ok := ids[m.Key]; !ok {
					if id := matchName(nameToID, m.Name); id > 0 {
						ids[m.Key] = id
					}
				}
			}
		}
		for key, id := range ids {
			if n, ok := remainByID[id]; ok {
				remain[key] = n
				continue
			}
			// The account catalog can key a model by a different id than the one the
			// draw routes use (Midjourney is id 8 there, 139 in cfg/list), so fall back
			// to matching by display name before giving up on the quota.
			for _, group := range [][]Model{snap.Models["draw"], snap.Models["repair"], snap.Models["text"]} {
				for _, m := range group {
					if m.Key != key || m.Name == "" {
						continue
					}
					if rid := matchName(nameToID, m.Name); rid > 0 {
						if n, ok := remainByID[rid]; ok {
							remain[key] = n
						}
					}
				}
			}
		}
	}

	// 4b) cfg/list also knows image-capable models the snapshot's draw/repair lists omit —
	//     every 4K tier, for instance: the snapshot carries only the 2K variants of 香蕉 Pro,
	//     香蕉 2 and 升智-2.5, so nothing downstream could ever select 4K even though the
	//     web client offers it. cfg/list flags them with draw:true.
	//
	//     They join the repair family, which is the chain the web client uses for them (a
	//     real 香蕉 Pro 2K session sends model_code=nanobanana2 with the id cfg/list lists).
	for key := range drawCapable {
		if key == "" {
			continue
		}
		if _, ok := ids[key]; !ok {
			continue // no id means nothing to call
		}
		already := false
		for _, group := range [][]Model{snap.Models["text"], snap.Models["draw"], snap.Models["repair"]} {
			for _, m := range group {
				if m.Key == key {
					already = true
				}
			}
		}
		if already {
			continue
		}
		snap.Models["repair"] = append(snap.Models["repair"], Model{
			Key: key, Name: names[key], Type: "repair",
			Desc: "image-capable model from cfg/list (absent from the snapshot's draw/repair lists)",
		})
		logf("INFO", "cfg/list adds an image model the snapshot omits: %s (id=%d, %s)", key, ids[key], names[key])
	}

	// 5) explicit overrides win, and an override for a key the upstream never declares also
	//    *creates* that model.
	//
	//    banana21 (香蕉2.1) is why this exists. The snapshot does list it — in both the draw
	//    and repair families, named 香蕉2.1 — but with no model_id at all, and the web
	//    client sends it as model_code with model_id 0. So there is no numeric id anywhere
	//    to resolve, and the old "id must be > 0" rule dropped it silently: the model was
	//    in the catalog and still absent from /v1/models and the panel. It is a real model
	//    (the operator generated an image with it, billed 268 灵力 as "使用香蕉2.1修图"),
	//    reached through the same repair chain as everything else.
	//
	//    A models.map line therefore does two jobs: it pins ids for known models, and for a
	//    model with no id at all it both supplies the id and, if no list carries the key,
	//    creates the entry.
	known := map[string]bool{}
	for _, group := range [][]Model{snap.Models["text"], snap.Models["draw"], snap.Models["repair"]} {
		for _, m := range group {
			known[m.Key] = true
		}
	}
	for k, v := range cat.overrides {
		ids[k] = v
		if known[k] {
			continue
		}
		snap.Models["repair"] = append(snap.Models["repair"], Model{
			Key: k, Name: k, Type: "repair",
			Desc: "mapped in models.map; the upstream declares this model nowhere",
		})
		logf("INFO", "models.map adds a model the upstream never declares: %s (id=%d)", k, v)
	}

	cat.mu.Lock()
	cat.text = snap.Models["text"]
	cat.draw = snap.Models["draw"]
	cat.repair = snap.Models["repair"]
	cat.idByKey = ids
	cat.remainByKey = remain
	cat.byName = buildIndex(snap.Models["text"], snap.Models["draw"], snap.Models["repair"], ids)
	cat.fetched = time.Now()
	cat.lastErr = ""
	cat.mu.Unlock()

	logf("INFO", "catalog: %d text, %d draw, %d repair, %d ids resolved, %d quotas",
		len(snap.Models["text"]), len(snap.Models["draw"]), len(snap.Models["repair"]), len(ids), len(remain))
	return nil
}

// buildIndex maps the OpenAI-facing names onto upstream routes.
//
//	Naming follows the qwen2api convention: the bare key is the chat model, and the
//	-image / -image-edit suffixes select the image capabilities of the same model.
func buildIndex(text, draw, repair []Model, ids map[string]int) map[string]Resolved {
	idx := map[string]Resolved{}
	for _, m := range text {
		idx[m.Key] = Resolved{
			OpenAI: m.Key, Kind: "chat", Key: m.Key,
			Name: m.Name, Desc: m.Desc, Family: m.Type, Capability: "chat",
		}
	}
	addImage := func(m Model, kind string) {
		id, ok := ids[m.Key]
		if !ok {
			return
		}
		// Presence means resolved: every declaration source only records an id it actually
		// found (> 0), so an explicit models.map entry is the only way to reach here with 0
		// — which is a real value, not a failure (banana21 is sent as model_id 0).
		family := m.Type
		if family == "" {
			family = "repair"
		}
		idx[m.Key+"-image"] = Resolved{
			OpenAI: m.Key + "-image", Kind: kind, Key: m.Key, ID: id,
			Name: m.Name, Desc: m.Desc, Family: family, Capability: "image",
		}
		idx[m.Key+"-image-edit"] = Resolved{
			OpenAI: m.Key + "-image-edit", Kind: kind, Key: m.Key, ID: id,
			Name: m.Name, Desc: m.Desc, Family: family, Capability: "image-edit",
		}
	}
	for _, m := range draw {
		addImage(m, "mj")
	}
	for _, m := range repair {
		addImage(m, "repair")
	}
	return idx
}

var nonAlnum = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// collectModelIDs walks an arbitrary config document and records every
// {model_key, model_id} pair it finds — the cfg/list payload nests them a few levels deep
// and the shape is not documented, so a generic walk beats a brittle struct.
func collectModelIDs(v any, out map[string]int) {
	switch t := v.(type) {
	case map[string]any:
		if id, ok := toInt(t["model_id"]); ok && id > 0 {
			if key, ok := t["model_key"].(string); ok && key != "" {
				if _, seen := out[key]; !seen {
					out[key] = id
				}
			}
		}
		for _, sub := range t {
			collectModelIDs(sub, out)
		}
	case []any:
		for _, sub := range t {
			collectModelIDs(sub, out)
		}
	}
}

// collectModelTable walks the same config document as collectModelIDs but records what the
// index needs: the display name for each model_key, and whether the entry is image-capable
// (`draw: true` marks the base model the web client uses for 图片创作; the `-draw` sibling
// carries draw:false and is the picker's own entry).
func collectModelTable(v any, names map[string]string, drawCapable map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		if key, ok := t["model_key"].(string); ok && key != "" {
			if n, ok := t["model_name"].(string); ok && n != "" {
				if _, seen := names[key]; !seen {
					names[key] = n
				}
			}
			if b, ok := t["draw"].(bool); ok && b {
				drawCapable[key] = true
			}
		}
		for _, sub := range t {
			collectModelTable(sub, names, drawCapable)
		}
	case []any:
		for _, sub := range t {
			collectModelTable(sub, names, drawCapable)
		}
	}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func normName(s string) string {
	return strings.ToLower(nonAlnum.ReplaceAllString(s, ""))
}

// matchName looks a catalog display name up in the account name->id map: exact first,
// then containment in either direction (the two lists word the same model differently).
func matchName(nameToID map[string]int, name string) int {
	n := normName(name)
	if n == "" {
		return 0
	}
	if id, ok := nameToID[n]; ok {
		return id
	}
	for k, id := range nameToID {
		if k == "" {
			continue
		}
		if strings.Contains(k, n) || strings.Contains(n, k) {
			return id
		}
	}
	return 0
}

// Resolve maps a requested model name (or a raw `image-<id>` form) to a route.
func (cat *Catalog) Resolve(name string) (Resolved, bool) {
	cat.mu.RLock()
	defer cat.mu.RUnlock()

	if r, ok := cat.byName[name]; ok {
		return r, true
	}
	// Allow `image-78` to pin a numeric id directly, which is handy while probing.
	if strings.HasPrefix(name, "image-") {
		if id, err := strconv.Atoi(strings.TrimPrefix(name, "image-")); err == nil {
			return Resolved{OpenAI: name, Kind: "repair", Key: "", ID: id}, true
		}
	}
	// Unknown suffix handling: `foo` may still be a valid image model key.
	if strings.HasSuffix(name, "-image") || strings.HasSuffix(name, "-image-edit") {
		base := strings.TrimSuffix(strings.TrimSuffix(name, "-image-edit"), "-image")
		if id, ok := cat.idByKey[base]; ok {
			kind := "repair"
			for _, m := range cat.draw {
				if m.Key == base {
					kind = "mj"
				}
			}
			return Resolved{OpenAI: name, Kind: kind, Key: base, ID: id}, true
		}
	}
	return Resolved{}, false
}

// OpenAIList is the /v1/models payload. Besides the required fields it carries the real
// upstream model name and the numeric id, so a caller can see that `gptimage2-image` is
// 升智-2 without having to consult anything else.
func (cat *Catalog) OpenAIList() []map[string]any {
	cat.mu.RLock()
	defer cat.mu.RUnlock()

	keys := make([]string, 0, len(cat.byName))
	for k := range cat.byName {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]map[string]any, 0, len(keys))
	now := time.Now().Unix()
	for _, k := range keys {
		r := cat.byName[k]
		entry := map[string]any{
			"id":       k,
			"object":   "model",
			"created":  now,
			"owned_by": "zhijuai",
		}
		if r.Name != "" {
			entry["name"] = r.Name
		}
		if r.Key != "" {
			entry["upstream_key"] = r.Key
		}
		if r.ID > 0 {
			entry["upstream_id"] = r.ID
		}
		if r.Family != "" {
			entry["family"] = r.Family
		}
		if r.Capability != "" {
			entry["capability"] = r.Capability
		}
		entry["unlimited"] = cat.isUnlimitedLocked(r.Key)
		out = append(out, entry)
	}
	return out
}

// IndexEntry is one OpenAI-facing model name with everything needed to display it:
// the real upstream name, the numeric id, what the id selects, and — for text models —
// the context window the upstream advertises.
type IndexEntry struct {
	OpenAI     string
	Key        string
	Name       string
	Desc       string
	Family     string
	Capability string
	ID         int
	Unlimited  bool

	ContextWindow int
	MaxTokens     int
	Think         bool
	HasThink      bool
}

// capsInt reads a numeric capability, which the upstream may send as a number or string.
func capsInt(caps map[string]any, key string) int {
	if caps == nil {
		return 0
	}
	switch v := caps[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		var n int
		_, _ = fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
		return n
	}
	return 0
}

func capsBool(caps map[string]any, key string) (bool, bool) {
	if caps == nil {
		return false, false
	}
	if v, ok := caps[key].(bool); ok {
		return v, true
	}
	return false, false
}

// Index returns every OpenAI-facing name, sorted, with its upstream identity.
func (cat *Catalog) Index() []IndexEntry {
	cat.mu.RLock()
	defer cat.mu.RUnlock()

	// Text models carry their context window in capacities.
	caps := map[string]map[string]any{}
	for _, m := range cat.text {
		caps[m.Key] = m.Caps
	}

	out := make([]IndexEntry, 0, len(cat.byName))
	for openai, r := range cat.byName {
		e := IndexEntry{
			OpenAI:     openai,
			Key:        r.Key,
			Name:       r.Name,
			Desc:       r.Desc,
			Family:     r.Family,
			Capability: r.Capability,
			ID:         r.ID,
			Unlimited:  cat.isUnlimitedLocked(r.Key),
		}
		if c, ok := caps[r.Key]; ok {
			e.ContextWindow = capsInt(c, "max_context")
			e.MaxTokens = capsInt(c, "max_tokens")
			if b, ok := capsBool(c, "think"); ok {
				e.Think, e.HasThink = b, true
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenAI < out[j].OpenAI })
	return out
}

// isUnlimitedLocked reports the catalog's `is_unlimited` flag. Callers must hold the
// read lock.
func (cat *Catalog) isUnlimitedLocked(key string) bool {
	for _, group := range [][]Model{cat.text, cat.draw, cat.repair} {
		for _, m := range group {
			if m.Key == key {
				return m.IsUnlimited
			}
		}
	}
	return false
}

func (cat *Catalog) TextModels() []Model {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	return append([]Model(nil), cat.text...)
}

// ImageModels lists the image-capable models (both families) that resolved to an id.
func (cat *Catalog) ImageModels() []Model {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	var out []Model
	for _, group := range [][]Model{cat.draw, cat.repair} {
		for _, m := range group {
			if cat.idByKey[m.Key] > 0 {
				out = append(out, m)
			}
		}
	}
	return out
}

// ModelsByContentType maps the upstream content_type ("text" / "draw") onto models.
func (cat *Catalog) ModelsByContentType(contentType string) []Model {
	if contentType == "text" {
		return cat.TextModels()
	}
	return cat.ImageModels()
}

// IDOf returns the numeric model id for a key, or 0 when it is not known.
func (cat *Catalog) IDOf(key string) int {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	return cat.idByKey[key]
}

// RemainOf reports the account's remaining quota for a model. -1 means unlimited (the
// upstream bills those by task count, and has no 灵力 price row for them at all — which
// is why Midjourney's price lookup answers with a generic error).
func (cat *Catalog) RemainOf(key string) (int, bool) {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	n, ok := cat.remainByKey[key]
	return n, ok
}

// IsUnlimited reports the catalog's `is_unlimited` flag: those models do not consume
// credits on this plan (升智-1 is one).
func (cat *Catalog) IsUnlimited(key string) bool {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	return cat.isUnlimitedLocked(key)
}

func (cat *Catalog) Status() map[string]any {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	ids := map[string]int{}
	for k, v := range cat.idByKey {
		ids[k] = v
	}
	return map[string]any{
		"text":       len(cat.text),
		"draw":       len(cat.draw),
		"repair":     len(cat.repair),
		"resolved":   len(ids),
		"openai_ids": len(cat.byName),
		"fetched":    cat.fetched.Format(time.RFC3339),
		"last_error": cat.lastErr,
	}
}
