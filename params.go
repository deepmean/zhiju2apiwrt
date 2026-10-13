package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ModelParams is what one image model accepts, taken from the upstream rather than
// guessed: /v1/aimoss/repair/params declares each repair model's aspect ratios, its
// resolution tier and its prompt limit, and /v1/aimoss/draw/config/params/new does the
// same for the Midjourney family (including its extra knobs).
//
// This matters because the values are per model and NOT interchangeable: most repair
// models offer exactly one resolution tier (banana Pro is 2K only, 升智-2.5创作版 is 2K
// only), and only 升智-1 (seedream4) offers both 1K and 2K. Sending "1K" to a model that
// does not list it is a silently ignored field, not an error.
type ModelParams struct {
	ModelKey      string       `json:"model_key"`
	Family        string       `json:"family"` // repair | mj
	AspectRatios  []string     `json:"aspect_ratios"`
	DefaultAspect string       `json:"default_aspect_ratio"`
	ImageSizes    []string     `json:"image_sizes"`
	DefaultSize   string       `json:"default_image_size"`
	PromptLimit   int          `json:"prompt_limit,omitempty"`
	Extras        []ExtraParam `json:"extra_params,omitempty"`
	// SizeTable is ratio -> tier -> nominal pixels. The upstream declares ratios and
	// tiers but never pixels, so these are derived (long edge = tier x 1024). They are
	// guidance for clients that speak pixels; the gateway snaps whatever it receives onto
	// the nearest declared ratio anyway, so an approximation is safe here.
	SizeTable map[string]map[string]string `json:"size_table,omitempty"`
}

// tierPixels turns a resolution tier name into its long edge in pixels.
func tierPixels(tier string) int {
	t := strings.ToUpper(strings.TrimSpace(tier))
	t = strings.TrimSuffix(t, "K")
	if t == "" {
		return 0
	}
	n, err := strconv.Atoi(t)
	if err != nil || n <= 0 || n > 16 {
		return 0
	}
	return n * 1024
}

// buildSizeTable derives the pixel dimensions for every declared ratio at every declared
// tier. Long edge is held at the tier size, the other edge follows the ratio.
func buildSizeTable(p *ModelParams) map[string]map[string]string {
	if len(p.ImageSizes) == 0 {
		return nil
	}
	out := map[string]map[string]string{}
	for _, ratio := range p.AspectRatios {
		rv, ok := ratioValue(ratio)
		if !ok {
			continue // "" / "auto": no fixed shape
		}
		row := map[string]string{}
		for _, tier := range p.ImageSizes {
			long := tierPixels(tier)
			if long == 0 {
				continue
			}
			w, h := long, long
			if rv >= 1 {
				h = evenInt(math.Round(float64(long) / rv))
			} else {
				w = evenInt(math.Round(float64(long) * rv))
			}
			row[tier] = fmt.Sprintf("%dx%d", w, h)
		}
		if len(row) > 0 {
			out[ratio] = row
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// evenInt keeps derived dimensions even, which is what image models expect.
func evenInt(n int) int {
	if n%2 != 0 {
		return n + 1
	}
	return n
}

// ExtraParam is a Midjourney knob (stylize, chaos, quality, seed, ...) with its range
// or allowed values, so a client can pass them through knowingly.
type ExtraParam struct {
	Key     string   `json:"key"`
	Name    string   `json:"name,omitempty"`
	Style   string   `json:"style,omitempty"`
	Default any      `json:"default,omitempty"`
	Values  []string `json:"values,omitempty"`
	Min     int      `json:"min,omitempty"`
	Max     int      `json:"max,omitempty"`
}

// AllowsAspect reports whether the model lists this ratio. "" and "auto" both mean
// "let the model choose" and are accepted whenever the model offers an auto default.
func (p *ModelParams) AllowsAspect(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	if v == "auto" && (p.DefaultAspect == "auto" || contains(p.AspectRatios, "auto")) {
		return true
	}
	return contains(p.AspectRatios, v)
}

// AllowsSize reports whether the model offers this resolution tier.
func (p *ModelParams) AllowsSize(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	return contains(p.ImageSizes, strings.ToUpper(v))
}

// SnapAspect maps a pixel-derived ratio onto the closest ratio the model supports.
//
// This exists because OpenAI-style clients do not send ratios, they send pixels: the
// dsh image-gen plugin turns its `openaiCompatSizes` table into `size: "2048x1365"`, and
// a greatest-common-divisor reduction of that is "2048:1365" — a value no model lists.
// Comparing the ratios numerically (within a few percent) recovers "3:2" and keeps the
// request valid without the client having to know anything about the upstream.
func (p *ModelParams) SnapAspect(requested string) (string, bool) {
	rv, ok := ratioValue(requested)
	if !ok {
		return "", false
	}
	best, bestDiff := "", 0.0
	for _, cand := range p.AspectRatios {
		cv, ok := ratioValue(cand)
		if !ok {
			continue // "" and "auto" are not numeric ratios
		}
		d := math.Abs(math.Log(rv / cv))
		if best == "" || d < bestDiff {
			best, bestDiff = cand, d
		}
	}
	if best == "" || bestDiff > 0.05 { // > ~5% off is a different ratio, not a rounding
		return "", false
	}
	return best, true
}

func ratioValue(s string) (float64, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, false
	}
	w, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	h, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, false
	}
	return w / h, true
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

type paramsCache struct {
	mu      sync.Mutex
	m       map[string]*ModelParams
	fetched time.Time
	ttl     time.Duration
}

func (pc *paramsCache) get(key string) (*ModelParams, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if time.Since(pc.fetched) > pc.ttl {
		return nil, false
	}
	p, ok := pc.m[key]
	return p, ok
}

// ParamsOf returns the upstream-declared parameters for a model key.
func (c *Client) ParamsOf(ctx context.Context, key string) (*ModelParams, bool) {
	if p, ok := c.params.get(key); ok {
		return p, true
	}
	if err := c.refreshParams(ctx); err != nil {
		logf("WARN", "params refresh failed: %v", err)
		return nil, false
	}
	return c.params.get(key)
}

// refreshParams loads both param tables into one map.
func (c *Client) refreshParams(ctx context.Context) error {
	out := map[string]*ModelParams{}

	// --- repair family ---------------------------------------------------------------
	var repair struct {
		RepairParams []struct {
			ModelKey    string `json:"model_key"`
			PromptLimit int    `json:"prompt_limit"`
			ModelParams map[string][]struct {
				ParamKey string `json:"param_key"`
				Items    []struct {
					ParamValue string `json:"param_value"`
					Default    bool   `json:"default"`
				} `json:"items"`
			} `json:"model_params"`
		} `json:"repair_params"`
	}
	if err := c.call(ctx, kindRepair, "/v1/aimoss/repair/params", map[string]any{}, &repair); err != nil {
		return fmt.Errorf("repair/params: %w", err)
	}
	for _, m := range repair.RepairParams {
		if m.ModelKey == "" {
			continue
		}
		p := &ModelParams{ModelKey: m.ModelKey, Family: "repair", PromptLimit: m.PromptLimit}
		for _, params := range m.ModelParams {
			for _, def := range params {
				switch def.ParamKey {
				case "aspect_ratio":
					for _, it := range def.Items {
						p.AspectRatios = append(p.AspectRatios, it.ParamValue)
						if it.Default {
							p.DefaultAspect = it.ParamValue
						}
					}
				case "image_size":
					for _, it := range def.Items {
						p.ImageSizes = append(p.ImageSizes, it.ParamValue)
						if it.Default {
							p.DefaultSize = it.ParamValue
						}
					}
				}
			}
		}
		p.SizeTable = buildSizeTable(p)
		out[m.ModelKey] = p
	}

	// --- Midjourney family -----------------------------------------------------------
	var draw struct {
		MJ []struct {
			GroupName string `json:"group_name"`
			Params    []struct {
				ParamKey          string `json:"param_key"`
				ParamName         string `json:"param_name"`
				ShowStyle         string `json:"show_style"`
				DefaultParamValue any    `json:"default_param_value"`
				MinParamValue     int    `json:"min_param_value"`
				MaxParamValue     int    `json:"max_param_value"`
				Buttons           []struct {
					ParamValue string `json:"param_value"`
					Default    bool   `json:"default"`
				} `json:"buttons"`
				Items []struct {
					ParamValue string `json:"param_value"`
					Default    bool   `json:"default"`
				} `json:"items"`
			} `json:"params"`
		} `json:"MJ"`
	}
	if err := c.call(ctx, kindV1, "/v1/aimoss/draw/config/params/new",
		map[string]any{"draw_type": "TextToImg"}, &draw); err == nil {
		mj := &ModelParams{ModelKey: "mj2", Family: "mj", Extras: []ExtraParam{}}
		for _, g := range draw.MJ {
			for _, def := range g.Params {
				// The aspect buttons arrive with an empty param_key; the draw page sends
				// them as "aspect" (its own config supplies the key).
				if def.ParamKey == "" && len(def.Buttons) > 0 {
					for _, b := range def.Buttons {
						mj.AspectRatios = append(mj.AspectRatios, b.ParamValue)
						if b.Default {
							mj.DefaultAspect = b.ParamValue
						}
					}
					continue
				}
				if def.ParamKey == "" {
					continue
				}
				extra := ExtraParam{
					Key: def.ParamKey, Name: def.ParamName, Style: def.ShowStyle,
					Default: def.DefaultParamValue, Min: def.MinParamValue, Max: def.MaxParamValue,
				}
				for _, b := range def.Buttons {
					extra.Values = append(extra.Values, b.ParamValue)
				}
				for _, it := range def.Items {
					if it.ParamValue != "" {
						extra.Values = append(extra.Values, it.ParamValue)
					}
				}
				mj.Extras = append(mj.Extras, extra)
			}
		}
		out["mj2"] = mj
	}

	c.params.mu.Lock()
	c.params.m = out
	c.params.fetched = time.Now()
	c.params.mu.Unlock()
	logf("INFO", "model params loaded: %d models", len(out))
	return nil
}

// ParamsTable returns every model's parameters, sorted by key (for GET /v1/models/params).
func (c *Client) ParamsTable(ctx context.Context) []*ModelParams {
	if err := c.refreshParams(ctx); err != nil {
		logf("WARN", "params table refresh failed: %v", err)
	}
	c.params.mu.Lock()
	defer c.params.mu.Unlock()
	out := make([]*ModelParams, 0, len(c.params.m))
	for _, p := range c.params.m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelKey < out[j].ModelKey })
	return out
}
