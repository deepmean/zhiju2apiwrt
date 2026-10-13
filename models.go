package main

import (
	"bufio"
	"context"
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
	Key       string         `json:"model_key"`
	Name      string         `json:"model_name"`
	Type      string         `json:"model_type"` // text | draw | repair | video | music | voice | tool
	Desc      string         `json:"model_desc"`
	DrawUI    string         `json:"draw_ui_type"`
	IsDefault bool           `json:"is_default"`
	Caps      map[string]any `json:"capacities"`
}

type modelSnapshot struct {
	Models map[string][]Model `json:"models"`
}

// Resolved tells the handlers how to drive one OpenAI-facing model name.
type Resolved struct {
	OpenAI string // the name the client asked for
	Kind   string // chat | mj | repair
	Key    string // upstream model_key
	ID     int    // numeric model_id the image routes require
}

// Catalog caches the upstream model list plus the model_key -> numeric id map.
type Catalog struct {
	cfg *Config
	c   *Client

	mu       sync.RWMutex
	text     []Model
	draw     []Model
	repair   []Model
	idByKey  map[string]int
	byName   map[string]Resolved // OpenAI-facing name -> resolution
	fetched  time.Time
	lastErr  string
	overrides map[string]int
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
	// 1) authoritative for the repair family: the repair home groups carry their id.
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

	// 2) the account catalog pairs display names with ids; match those names back to keys.
	var acct struct {
		Subscriptions struct {
			ModelsRemain []struct {
				ModelName string `json:"model_name"`
				ModelID   int    `json:"model_id"`
			} `json:"models_remain"`
		} `json:"subscriptions"`
	}
	if err := cat.c.call(ctx, kindV1, "/v1/account/get", map[string]any{}, &acct); err == nil {
		nameToID := map[string]int{}
		for _, m := range acct.Subscriptions.ModelsRemain {
			nameToID[normName(m.ModelName)] = m.ModelID
		}
		for _, group := range [][]Model{snap.Models["draw"], snap.Models["repair"]} {
			for _, m := range group {
				if _, ok := ids[m.Key]; ok {
					continue
				}
				if id := matchName(nameToID, m.Name); id > 0 {
					ids[m.Key] = id
				}
			}
		}
	}

	// 3) explicit overrides win.
	for k, v := range cat.overrides {
		ids[k] = v
	}

	cat.mu.Lock()
	cat.text = snap.Models["text"]
	cat.draw = snap.Models["draw"]
	cat.repair = snap.Models["repair"]
	cat.idByKey = ids
	cat.byName = buildIndex(snap.Models["text"], snap.Models["draw"], snap.Models["repair"], ids)
	cat.fetched = time.Now()
	cat.lastErr = ""
	cat.mu.Unlock()

	logf("INFO", "catalog: %d text, %d draw, %d repair, %d ids resolved",
		len(snap.Models["text"]), len(snap.Models["draw"]), len(snap.Models["repair"]), len(ids))
	return nil
}

// buildIndex maps the OpenAI-facing names onto upstream routes.
//
//	Naming follows the qwen2api convention: the bare key is the chat model, and the
//	-image / -image-edit suffixes select the image capabilities of the same model.
func buildIndex(text, draw, repair []Model, ids map[string]int) map[string]Resolved {
	idx := map[string]Resolved{}
	for _, m := range text {
		idx[m.Key] = Resolved{OpenAI: m.Key, Kind: "chat", Key: m.Key}
	}
	addImage := func(m Model, kind string) {
		id, ok := ids[m.Key]
		if !ok || id <= 0 {
			return
		}
		idx[m.Key+"-image"] = Resolved{OpenAI: m.Key + "-image", Kind: kind, Key: m.Key, ID: id}
		idx[m.Key+"-image-edit"] = Resolved{OpenAI: m.Key + "-image-edit", Kind: kind, Key: m.Key, ID: id}
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

// OpenAIList is the /v1/models payload.
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
		out = append(out, map[string]any{
			"id":       k,
			"object":   "model",
			"created":  now,
			"owned_by": "zhijuai",
		})
	}
	return out
}

func (cat *Catalog) TextModels() []Model {
	cat.mu.RLock()
	defer cat.mu.RUnlock()
	return append([]Model(nil), cat.text...)
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
