package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// APIRecord is one upstream session this gateway created, so the cloud records it leaves
// behind can be cleaned up without touching the operator's own conversations.
//
// The distinction matters: deleting "the last N sessions" from the account would take out
// whatever the human was doing in the web UI. A ledger of what we made lets the delete be
// precise.
type APIRecord struct {
	Kind      string    `json:"kind"` // chat | image | mj
	SessionID string    `json:"session_id"`
	Model     string    `json:"model"`
	Prompt    string    `json:"prompt,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Deleted   bool      `json:"deleted,omitempty"`
	DeletedAt time.Time `json:"deleted_at,omitempty"`
}

// RecordLedger keeps the ledger, bounded, on disk so it survives a restart.
type RecordLedger struct {
	mu    sync.Mutex
	path  string
	items []APIRecord
	max   int
}

const ledgerMax = 1000

func NewRecordLedger(path string) *RecordLedger {
	l := &RecordLedger{path: path, max: ledgerMax}
	l.load()
	return l
}

func (l *RecordLedger) load() {
	if l.path == "" {
		return
	}
	f, err := os.Open(l.path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var items []APIRecord
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r APIRecord
		if err := json.Unmarshal([]byte(line), &r); err == nil && r.SessionID != "" {
			items = append(items, r)
		}
	}
	l.mu.Lock()
	l.items = items
	l.mu.Unlock()
	if len(items) > 0 {
		logf("INFO", "record ledger: %d session(s) loaded from %s", len(items), l.path)
	}
}

// Add records a session once. The same session can be seen again on a follow-up turn, so
// an existing entry is left alone rather than duplicated.
func (l *RecordLedger) Add(kind, sessionID, model, prompt string) {
	if sessionID == "" {
		return
	}
	l.mu.Lock()
	for i := range l.items {
		if l.items[i].SessionID == sessionID {
			l.mu.Unlock()
			return
		}
	}
	l.items = append(l.items, APIRecord{
		Kind: kind, SessionID: sessionID, Model: model,
		Prompt: truncateRunes(prompt, 120), CreatedAt: time.Now(),
	})
	if len(l.items) > l.max {
		l.items = l.items[len(l.items)-l.max:]
	}
	snapshot := append([]APIRecord(nil), l.items...)
	path := l.path
	l.mu.Unlock()
	writeLedger(path, snapshot)
}

// MarkDeleted flags a session as removed upstream.
func (l *RecordLedger) MarkDeleted(sessionIDs ...string) {
	want := map[string]bool{}
	for _, id := range sessionIDs {
		want[id] = true
	}
	l.mu.Lock()
	for i := range l.items {
		if want[l.items[i].SessionID] {
			l.items[i].Deleted = true
			l.items[i].DeletedAt = time.Now()
		}
	}
	snapshot := append([]APIRecord(nil), l.items...)
	path := l.path
	l.mu.Unlock()
	writeLedger(path, snapshot)
}

// List returns the ledger, newest first.
func (l *RecordLedger) List(includeDeleted bool) []APIRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]APIRecord, 0, len(l.items))
	for _, r := range l.items {
		if r.Deleted && !includeDeleted {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Pending returns the session ids still believed to exist upstream.
func (l *RecordLedger) Pending() []APIRecord {
	return l.List(false)
}

func writeLedger(path string, items []APIRecord) {
	if path == "" {
		return
	}
	var sb strings.Builder
	for _, r := range items {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		logf("WARN", "record ledger write failed: %v", err)
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
