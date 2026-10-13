package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFlattenMessages(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: json.RawMessage(`"you are terse"`)},
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", Content: json.RawMessage(`"hello"`)},
		{Role: "user", Content: json.RawMessage(`"now summarise"`)},
	}
	got, err := flattenMessages(msgs)
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	for _, want := range []string{"you are terse", "[对话历史]", "user: hi", "assistant: hello", "[对话历史结束]", "now summarise"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "now summarise") {
		t.Errorf("the current turn must be last:\n%s", got)
	}
}

func TestFlattenMessagesPartsAndErrors(t *testing.T) {
	// content-parts form, the shape agent clients send
	msgs := []chatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"from parts"}]`)}}
	got, err := flattenMessages(msgs)
	if err != nil {
		t.Fatalf("flatten parts: %v", err)
	}
	if got != "from parts" {
		t.Errorf("parts flatten = %q", got)
	}

	// a trailing assistant turn has nothing to answer
	if _, err := flattenMessages([]chatMessage{{Role: "user", Content: json.RawMessage(`"x"`)}, {Role: "assistant", Content: json.RawMessage(`"y"`)}}); err == nil {
		t.Error("want an error when the last message is not a user turn")
	}
}

func TestParseTokens(t *testing.T) {
	const text = `
# comment
eyJhbGciOiJIUzI1NiJ9.eyJ1aWQiOjEsImV4cCI6NDEwMjQ0NDgwMH0.sig
acc2=eyJhbGciOiJIUzI1NiJ9.eyJ1aWQiOjIsImV4cCI6NDEwMjQ0NDgwMH0.sig2

labelled: eyJhbGciOiJIUzI1NiJ9.eyJ1aWQiOjMsImV4cCI6NDEwMjQ0NDgwMH0.sig3
`
	toks := parseTokens(text)
	if len(toks) != 3 {
		t.Fatalf("want 3 tokens, got %d", len(toks))
	}
	if toks[1].Label != "acc2" || toks[2].Label != "labelled" {
		t.Errorf("labels: %q, %q", toks[1].Label, toks[2].Label)
	}
	for _, tk := range toks {
		if tk.UID == 0 || tk.Exp.IsZero() {
			t.Errorf("token %s did not decode uid/exp", tk.describe())
		}
	}
}

func TestParseJWTExpiry(t *testing.T) {
	// {"uid":39851,"username":"EMEM","exp":1800488345}
	tok := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ1aWQiOjM5ODUxLCJ1c2VybmFtZSI6IkVNRU0iLCJleHAiOjE4MDA0ODgzNDV9.mG3fYBkfDmbhBqQj4NNjbslNt_3SCroLd9WhAe8nSGY"
	uid, user, exp, ok := parseJWT(tok)
	if !ok || uid != 39851 || user != "EMEM" {
		t.Fatalf("parseJWT = %d/%q/%v, ok=%v", uid, user, exp, ok)
	}
	if exp.Unix() != 1800488345 {
		t.Errorf("exp = %d", exp.Unix())
	}
}

func TestPoolPickSkipsInvalid(t *testing.T) {
	p := &Pool{tokens: []*Token{
		{Value: "a", Exp: time.Now().Add(time.Hour)},
		{Value: "b", Exp: time.Now().Add(time.Hour)},
	}}
	p.tokens[0].invalid = true
	for i := 0; i < 4; i++ {
		if got := p.Pick(); got == nil || got.Value != "b" {
			t.Fatalf("pick %d = %v, want b (a is invalid)", i, got)
		}
	}
	// an expired token is skipped too
	p2 := &Pool{tokens: []*Token{
		{Value: "expired", Exp: time.Now().Add(-time.Hour)},
		{Value: "live", Exp: time.Now().Add(time.Hour)},
	}}
	if got := p2.Pick(); got == nil || got.Value != "live" {
		t.Fatalf("pick = %v, want live", got)
	}
}

func TestExtractTokens(t *testing.T) {
	if got := extractTokens([]byte(`{"tokens":["a","b"]}`)); len(got) != 2 {
		t.Errorf("json list = %v", got)
	}
	if got := extractTokens([]byte(`{"token":"c"}`)); len(got) != 1 || got[0] != "c" {
		t.Errorf("json single = %v", got)
	}
	if got := extractTokens([]byte("x\ny\n")); len(got) != 2 {
		t.Errorf("plain lines = %v", got)
	}
}

func TestStatusesDoNotDeadlock(t *testing.T) {
	p := &Pool{tokens: []*Token{{Value: "a", Label: "l", Exp: time.Now().Add(time.Hour)}}}
	done := make(chan []map[string]any, 1)
	go func() { done <- p.statuses() }()
	select {
	case s := <-done:
		if len(s) != 1 || s[0]["state"] != "ok" {
			t.Fatalf("statuses = %v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("statuses() deadlocked on the token mutex")
	}
}
