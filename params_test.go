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

// The dsh image-gen plugin sends pixels from its own size table, so the gateway has to
// recover the ratio the model actually lists — gcd reduction gives values nobody has.
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
