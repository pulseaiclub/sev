package sev

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func benchEngine(b *testing.B) *Engine {
	b.Helper()
	e, err := New(Options{Limit: 10})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	return e
}

// BenchmarkNew is the one-time cost: decode the embedded tree. It is paid at
// process start, not per keystroke.
func BenchmarkNew(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := New(Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStrView is the floor of the whole design: a command name costs a
// length read and an unsafe.String over the read-only blob.
func BenchmarkStrView(b *testing.B) {
	ix, err := loadGzip(specDB)
	if err != nil {
		b.Fatal(err)
	}
	git, _ := ix.child(ix.root, "git")
	b.ReportAllocs()
	for b.Loop() {
		_ = ix.name(git)
	}
}

// BenchmarkChildLookup is the per-token cost: one binary search over a node's
// sorted children.
func BenchmarkChildLookup(b *testing.B) {
	ix, err := loadGzip(specDB)
	if err != nil {
		b.Fatal(err)
	}
	git, _ := ix.child(ix.root, "git")
	b.ReportAllocs()
	for b.Loop() {
		ix.child(git, "checkout")
	}
}

func BenchmarkComplete(b *testing.B) {
	cases := []struct {
		name string
		line string
	}{
		{"TopLevel", ""},
		{"Prefix", "gi"},
		{"Subcommand", "git ch"},
		{"Many-Child", "git "},
		{"Options", "git log --"},
		{"Fuzzy", "git chekout"},
		{"Argument-Files", "cat "},
	}
	e := benchEngine(b)
	cwd, _ := os.Getwd()
	ctx := context.Background()
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := e.Complete(ctx, Request{Line: tc.line, Cwd: cwd}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCompleteWithHistory(b *testing.B) {
	e := benchEngine(b)
	cwd := "/repo"
	for i := 0; i < 200; i++ {
		e.Record(Event{Cmd: "git commit -m wip" + string(rune('a'+i%26)), Cwd: cwd})
		e.Record(Event{Cmd: "git stash pop", Cwd: cwd, Prev: "git add ."})
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := e.Complete(ctx, Request{Line: "git st", Cwd: cwd, Prev: "git add ."}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFilesGenerator(b *testing.B) {
	dir := b.TempDir()
	for i := 0; i < 200; i++ {
		if err := os.WriteFile(filepath.Join(dir, "file"+string(rune('a'+i%26))+string(rune('a'+i/26))), nil, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	g := filesGenerator{}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := g.Complete(ctx, GenRequest{Partial: "file", Cwd: dir, Limit: 10}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGitGeneratorCached(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git not installed")
	}
	dir := b.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		b.Skipf("git init: %v: %s", err, out)
	}
	g := gitGenerator{mode: "branch"}
	ctx := context.Background()
	// Prime the cache; the benchmark measures the cached path, which is what a
	// keystroke actually hits.
	_, _ = g.Complete(ctx, GenRequest{Cwd: dir, Limit: 10})
	time.Sleep(5 * time.Millisecond)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := g.Complete(ctx, GenRequest{Cwd: dir, Limit: 10}); err != nil {
			b.Fatal(err)
		}
	}
}

// TestLatencyBudget asserts the contract the README advertises: a keystroke is
// answered in microseconds, and even the slowest path stays inside 20 ms.
func TestLatencyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	e := newEngineForTest(t, Options{})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{"", "g", "gi", "git", "git ", "git ch", "git checkout ", "git log --", "cd ", "cat "}
	ctx := context.Background()

	// Warm: load, first generator calls, map growth.
	for _, line := range lines {
		if _, err := e.Complete(ctx, Request{Line: line, Cwd: cwd}); err != nil {
			t.Fatal(err)
		}
	}

	var samples []time.Duration
	for i := 0; i < 50; i++ {
		for _, line := range lines {
			start := time.Now()
			if _, err := e.Complete(ctx, Request{Line: line, Cwd: cwd}); err != nil {
				t.Fatal(err)
			}
			samples = append(samples, time.Since(start))
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)/2]
	p99 := samples[len(samples)*99/100]
	worst := samples[len(samples)-1]
	t.Logf("p50=%v p99=%v max=%v over %d calls", p50, p99, worst, len(samples))

	if p99 > 2*time.Millisecond {
		t.Fatalf("p99 = %v, want well under the 20 ms budget", p99)
	}
	if worst > 20*time.Millisecond {
		t.Fatalf("max = %v, want at most the 20 ms generator budget", worst)
	}
}
