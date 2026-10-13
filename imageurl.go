package main

import (
	"encoding/json"
	"fmt"
)

// ImageURL is one picture reference from an upstream message.
//
// The two image families disagree about this shape and both shapes appear in the wild:
// the repair family puts plain URL strings in `urls`, while Midjourney's message page
// puts objects (`{"url":...,"thumb_url":...,"width":...,"height":...}`) in the very same
// field. Decoding that into []string fails the whole page — and because the poll loop
// treats a decode failure as "not ready yet", the symptom is an MJ request that never
// returns instead of an error. Accepting both shapes here is what keeps that from
// happening.
type ImageURL struct {
	URL      string `json:"url"`
	ThumbURL string `json:"thumb_url,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

func (u *ImageURL) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &u.URL)
	}
	var obj struct {
		URL      string `json:"url"`
		ThumbURL string `json:"thumb_url"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("image url: %w", err)
	}
	u.URL, u.ThumbURL, u.Width, u.Height = obj.URL, obj.ThumbURL, obj.Width, obj.Height
	return nil
}

func (u ImageURL) MarshalJSON() ([]byte, error) {
	return json.Marshal(u.URL)
}

// urlsOf pulls the non-empty URLs out of a decoded list.
func urlsOf(list []ImageURL) []string {
	out := make([]string, 0, len(list))
	for _, u := range list {
		if u.URL != "" {
			out = append(out, u.URL)
		}
	}
	return out
}
