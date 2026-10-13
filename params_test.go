package main

import "testing"

func TestModelParamsValidation(t *testing.T) {
	// 升智-1: the only model that offers two tiers.
	seedream4 := &ModelParams{
		ModelKey:      "seedream4",
		Family:        "repair",
		AspectRatios:  []string{"", "2:3", "3:2", "1:1", "9:16", "16:9", "21:9"},
		DefaultAspect: "",
		ImageSizes:    []string{"1K", "2K"},
		DefaultSize:   "2K",
	}
	// 香蕉 Pro: one fixed tier, and "auto" instead of "" as the default ratio.
	nanobanana2 := &ModelParams{
		ModelKey:      "nanobanana2",
		AspectRatios:  []string{"", "2:3", "1:1"},
		DefaultAspect: "",
		ImageSizes:    []string{"2K"},
		DefaultSize:   "2K",
	}

	if !seedream4.AllowsSize("1K") || !seedream4.AllowsSize("2k") {
		t.Error("seedream4 should accept 1K and 2K (case-insensitively)")
	}
	if seedream4.AllowsSize("4K") {
		t.Error("seedream4 must reject 4K")
	}
	if nanobanana2.AllowsSize("1K") {
		t.Error("香蕉 Pro is 2K only; 1K must be rejected rather than silently ignored")
	}
	if !nanobanana2.AllowsAspect("1:1") || nanobanana2.AllowsAspect("21:9") {
		t.Error("aspect validation is wrong for nanobanana2")
	}
	// An empty request means "model default", which is always allowed.
	if !nanobanana2.AllowsAspect("") || !nanobanana2.AllowsSize("") {
		t.Error("empty values must be accepted")
	}

	mj := &ModelParams{
		ModelKey:      "mj2",
		Family:        "mj",
		AspectRatios:  []string{"4:3", "1:1", "3:4"},
		DefaultAspect: "1:1",
	}
	if !mj.AllowsAspect("4:3") || mj.AllowsAspect("16:9") {
		t.Error("mj2 offers only 4:3, 1:1 and 3:4")
	}
	// Midjourney declares no resolution tiers, so any explicit one must be rejected.
	if mj.AllowsSize("2K") {
		t.Error("mj2 declares no image sizes")
	}
}

func TestRatioFromSizeDerivation(t *testing.T) {
	// The OpenAI `size` field is the only ratio signal most clients send.
	cases := map[string]string{"1024x1024": "1:1", "1024x1536": "2:3", "1536x1024": "3:2"}
	for in, want := range cases {
		if got := ratioFromSize(in); got != want {
			t.Errorf("ratioFromSize(%q) = %q, want %q", in, got, want)
		}
	}
}

// The regression this locks in: a client that picks "2K" in its UI can only express that
// as pixels, and ignoring the implied tier made every request fall back to the configured
// IMAGE_SIZE — "I selected 2K and got 1K".
func TestTierFromPixels(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1K",
		"2048x2048": "2K",
		"2048x1365": "2K", // 3:2 at 2K, from the plugin's size table
		"1365x2048": "2K",
		"3072x3072": "3K",
		"4096x1715": "4K", // 21:9 at 4K
		"512x512":   "1K", // rounds up to the smallest tier
		"":          "",
		"garbage":   "",
		"auto":      "",
		"0x0":       "",
	}
	for in, want := range cases {
		if got := tierFromPixels(in); got != want {
			t.Errorf("tierFromPixels(%q) = %q, want %q", in, got, want)
		}
	}
}

// The pixel-derived tier must beat the configured preference, but only when the model
// actually offers it.
func TestSizePrecedence(t *testing.T) {
	seedream4 := &ModelParams{ImageSizes: []string{"1K", "2K"}, DefaultSize: "2K"}
	nanobanana2 := &ModelParams{ImageSizes: []string{"2K"}, DefaultSize: "2K"}

	// 2048x2048 implies 2K, which both models offer.
	for _, p := range []*ModelParams{seedream4, nanobanana2} {
		if got := tierFromPixels("2048x2048"); got != "2K" || !p.AllowsSize(got) {
			t.Errorf("2048x2048 should be usable as 2K on %v", p.ImageSizes)
		}
	}
	// 1024x1024 implies 1K, which 香蕉 Pro does not offer — the caller must not end up
	// silently asking for a tier the model lacks.
	if got := tierFromPixels("1024x1024"); got != "1K" || nanobanana2.AllowsSize(got) {
		t.Error("1K must be rejected for a 2K-only model rather than passed through")
	}
}
func TestSnapAspect(t *testing.T) {
	p := &ModelParams{
		AspectRatios: []string{"", "1:1", "2:3", "3:2", "3:4", "4:3", "16:9", "9:16", "21:9"},
	}
	cases := map[string]string{
		"2048:1365": "3:2",   // 2048x1365 from a plugin size table
		"1365:2048": "2:3",   // and its portrait twin
		"2048:1152": "16:9",  // 16:9 at 2K
		"1024:1024": "1:1",
		"1024:1536": "2:3",
		"1536:1024": "3:2",
		"4096:1715": "21:9",  // 21:9 rounded to whole pixels
	}
	for in, want := range cases {
		got, ok := p.SnapAspect(in)
		if !ok || got != want {
			t.Errorf("SnapAspect(%q) = %q/%v, want %q", in, got, ok, want)
		}
	}
	// A genuinely different ratio must not be forced onto something close-ish.
	if got, ok := p.SnapAspect("5:4"); ok && got == "1:1" {
		t.Errorf("5:4 snapped to %q, which is too far away", got)
	}
	if _, ok := p.SnapAspect("not-a-ratio"); ok {
		t.Error("garbage must not snap")
	}
}
