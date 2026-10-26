package sev

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func newTestHistory(t *testing.T) *History {
	t.Helper()
	h := NewHistory(0)
	h.now = func() int64 { return 1_700_000_000 }
	return h
}

func TestHistoryPrefixAndFrecency(t *testing.T) {
	h := newTestHistory(t)
	cwd := "/repo"
	for i := 0; i < 5; i++ {
		h.Record(Event{Cmd: "git status", Cwd: cwd, At: 1_700_000_000 - int64(i)})
	}
	h.Record(Event{Cmd: "git stash pop", Cwd: cwd, At: 1_700_000_000})
	h.Record(Event{Cmd: "make build", Cwd: "/other", At: 1_700_000_000})

	hits := h.Prefix("git ", cwd, 5, nil)
	if len(hits) != 2 {
		t.Fatalf("hits = %d (%v), want 2", len(hits), hits)
	}
	if hits[0].Cmd != "git status" {
		t.Fatalf("first hit = %q, want git status", hits[0].Cmd)
	}
	if local := h.Frecency("git status", cwd); local <= h.Frecency("git status", "/elsewhere") {
		t.Fatalf("cwd-scoped frecency %v must beat the global-only score", local)
	}
	if h.Frecency("git status", cwd) > 1 || h.Frecency("git status", cwd) <= 0 {
		t.Fatalf("frecency must be in (0,1]")
	}
	if h.Count() != 7 || h.Len() != 3 {
		t.Fatalf("count=%d len=%d, want 7 and 3", h.Count(), h.Len())
	}
}

func TestHistoryDecay(t *testing.T) {
	h := newTestHistory(t)
	fresh := h.Frecency("git pull", "/repo")
	if fresh != 0 {
		t.Fatalf("unknown command must score 0, got %v", fresh)
	}
	h.Record(Event{Cmd: "git pull", Cwd: "/repo", At: 1_700_000_000})
	nowScore := h.Frecency("git pull", "/repo")
	h.now = func() int64 { return 1_700_000_000 + 90*86400 }
	oldScore := h.Frecency("git pull", "/repo")
	if oldScore >= nowScore {
		t.Fatalf("90 days later the same command must score lower: %v vs %v", oldScore, nowScore)
	}
	if oldScore <= 0 {
		t.Fatalf("decay must not reach zero at 90 days: %v", oldScore)
	}
}

func TestHistoryTransition(t *testing.T) {
	h := newTestHistory(t)
	for i := 0; i < 3; i++ {
		h.Record(Event{Cmd: "git add .", Cwd: "/r"})
		h.Record(Event{Cmd: "git commit -m x", Cwd: "/r", Prev: "git add ."})
	}
	h.Record(Event{Cmd: "git push", Cwd: "/r", Prev: "git add ."})
	if got := h.Transition("git add", "git commit"); got < 0.7 || got > 0.76 {
		t.Fatalf("transition = %v, want 0.75", got)
	}
	if got := h.Transition("git status", "git commit"); got != 0 {
		t.Fatalf("unseen transition = %v, want 0", got)
	}
}

func TestHistoryLoadJSONL(t *testing.T) {
	h := newTestHistory(t)
	jsonl := strings.Join([]string{
		`{"v":1,"at":1700000000000,"cwd":"/repo","cmd":"git status","exit":0}`,
		`{"v":1,"at":1700000001000,"cwd":"/repo","cmd":"git add .","exit":0}`,
		`not json`,
		`{"v":1,"at":1700000002000,"cwd":"/repo","cmd":"git commit","exit":1}`,
		`{"v":1,"at":1700000003000,"cwd":"/elsewhere","cmd":"ls","exit":0}`,
	}, "\n")
	n, err := h.LoadJSONL(strings.NewReader(jsonl))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n != 4 {
		t.Fatalf("loaded %d, want 4 (the garbage line must be skipped)", n)
	}
	// Consecutive commands in the same directory imply a transition.
	if got := h.Transition("git status", "git add"); got == 0 {
		t.Fatal("consecutive history lines must produce a transition")
	}
	if hits := h.Prefix("git", "/repo", 10, nil); len(hits) != 3 {
		t.Fatalf("prefix hits = %d, want 3", len(hits))
	}
}

func TestHistoryEviction(t *testing.T) {
	h := NewHistory(100)
	h.now = func() int64 { return 1_700_000_000 }
	for i := 0; i < 500; i++ {
		h.Record(Event{Cmd: "cmd" + string(rune('a'+i%26)) + string(rune('a'+i/26)), At: int64(1_699_000_000 + i)})
	}
	if h.Len() > 100 {
		t.Fatalf("history grew past its bound: %d", h.Len())
	}
	// The newest command must survive eviction.
	h.Record(Event{Cmd: "keep me", At: 1_700_000_000})
	if hits := h.Prefix("keep", "", 1, nil); len(hits) != 1 {
		t.Fatal("newest command was evicted")
	}
}

func TestHistoryPrefixBounds(t *testing.T) {
	h := newTestHistory(t)
	for _, cmd := range []string{"a", "ab", "abc", "b", "ba"} {
		h.Record(Event{Cmd: cmd, Cwd: "/r"})
	}
	for query, want := range map[string]int{"a": 3, "ab": 2, "abc": 1, "b": 2, "": 5} {
		if got := len(h.Prefix(query, "/r", 10, nil)); got != want {
			t.Errorf("prefix %q: %d hits, want %d", query, got, want)
		}
	}
}

func TestSkeleton(t *testing.T) {
	cases := map[string]string{
		"":                         "",
		"ls":                       "ls",
		"git checkout -b feat":     "git checkout",
		"  docker   run  -it  x  ": "docker run",
		"kubectl":                  "kubectl",
	}
	for in, want := range cases {
		if got := skeleton(in); got != want {
			t.Errorf("skeleton(%q) = %q, want %q", in, got, want)
		}
	}
}

func BenchmarkHistoryRecord(b *testing.B) {
	h := NewHistory(0)
	b.ReportAllocs()
	for b.Loop() {
		h.Record(Event{Cmd: "git status", Cwd: "/repo", At: 1_700_000_000})
	}
}

func BenchmarkHistoryPrefix(b *testing.B) {
	h := NewHistory(0)
	for i := 0; i < 5000; i++ {
		h.Record(Event{Cmd: "cmd" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)), Cwd: "/repo"})
	}
	b.ReportAllocs()
	for b.Loop() {
		h.Prefix("cmdxx", "/repo", 10, nil)
	}
}

// At capacity the command that was just run is the least stale thing in the
// history; evicting it would make every new command invisible until its second
// run.
func TestRecordAtCapacityKeepsTheNewCommand(t *testing.T) {
	h := NewHistory(100)
	for i := 0; i < 100; i++ {
		h.Record(Event{Cmd: fmt.Sprintf("old-%03d", i), Cwd: "/repo", At: 1_700_000_000 + int64(i)})
	}
	h.Record(Event{Cmd: "brand-new", Cwd: "/repo", At: 1_700_000_500})
	if h.Len() > 100 {
		t.Fatalf("Len() = %d, want <= 100", h.Len())
	}
	hits := h.Prefix("brand", "/repo", 10, nil)
	if len(hits) != 1 || hits[0].Cmd != "brand-new" {
		t.Fatalf("Prefix(brand) = %v, want the just-recorded command", hits)
	}
}

// The cost of a query must follow the typed prefix, not the history size or the
// caller's Limit.
func TestBigHistoryBigLimitStaysFast(t *testing.T) {
	h := NewHistory(DefaultHistoryMax)
	for i := 0; i < 20000; i++ {
		h.Record(Event{Cmd: fmt.Sprintf("cmd-%05d --flag", i), Cwd: "/repo", At: 1_700_000_000 + int64(i)})
	}
	start := time.Now()
	for i := 0; i < 20; i++ {
		h.Prefix("cmd", "/repo", 600, nil)
		h.Top("/repo", 600)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("40 queries over 20k entries took %v", d)
	}
}
