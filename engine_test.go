package sev

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func newEngineForTest(t *testing.T, opts Options) *Engine {
	t.Helper()
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func complete(t *testing.T, e *Engine, req Request) []Item {
	t.Helper()
	items, err := e.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete(%q): %v", req.Line, err)
	}
	return items
}

// scoreOf finds one row's score, ignoring the trailing-space variant.
func scoreOf(items []Item, text string) float64 {
	for _, it := range items {
		if strings.TrimRight(it.Text, " ") == text {
			return it.Score
		}
	}
	return 0
}

func textsOf(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}

// has compares the accepted line, ignoring whether the row offers a trailing
// space: a command that takes subcommands or arguments appends one, and that is
// a property of the row, not of the command it names.
func has(items []Item, text string) bool {
	want := strings.TrimRight(text, " ")
	for _, it := range items {
		if strings.TrimRight(it.Text, " ") == want {
			return true
		}
	}
	return false
}

func TestEngineLoadsEmbeddedSpecs(t *testing.T) {
	e := newEngineForTest(t, Options{})
	if e.Size() < 500 {
		t.Fatalf("size = %d, want the full command tree", e.Size())
	}
	top := e.TopLevel(5)
	if len(top) != 5 {
		t.Fatalf("TopLevel = %v", top)
	}
	all := e.TopLevel(1000)
	if !slices.Contains(all, "git") || !slices.Contains(all, "docker") {
		t.Fatalf("TopLevel(1000) is missing core commands: %d entries", len(all))
	}
}

func TestCompletePrefixAndExact(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 10})

	items := complete(t, e, Request{Line: "git ch"})
	if !has(items, "git checkout") {
		t.Fatalf("git ch -> %q, want git checkout", textsOf(items))
	}
	for _, it := range items {
		if !strings.HasPrefix(it.Text, "git ch") {
			t.Fatalf("item %q does not continue the typed prefix", it.Text)
		}
	}

	// A command with subcommands offers the space that leads into them.
	items = complete(t, e, Request{Line: "git branch"})
	if !has(items, "git branch ") {
		t.Fatalf("git branch -> %q, want the open-ended variant", textsOf(items))
	}
}

func TestCompleteOptions(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 10})
	items := complete(t, e, Request{Line: "git log --"})
	if len(items) == 0 {
		t.Fatal("no flag completions for git log --")
	}
	for _, it := range items {
		if !strings.HasPrefix(it.Text, "git log --") {
			t.Fatalf("flag completion %q does not keep the line", it.Text)
		}
	}
}

func TestCompleteFuzzyFallback(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 10})
	items := complete(t, e, Request{Line: "git chekout"})
	if !has(items, "git checkout") {
		t.Fatalf("typo fallback -> %q, want git checkout", textsOf(items))
	}
	for _, it := range items {
		if it.Score > 0.5 {
			t.Fatalf("fuzzy row %q scored %.2f, must stay below prefix matches", it.Text, it.Score)
		}
	}
}

func TestCompleteUnknownCommand(t *testing.T) {
	e := newEngineForTest(t, Options{})
	items := complete(t, e, Request{Line: "zzz-not-a-command --x"})
	for _, it := range items {
		if it.Kind == KindSpec {
			t.Fatalf("unknown command produced a spec row %q", it.Text)
		}
	}
}

func TestCompleteKeepsWholeLineThroughSeparators(t *testing.T) {
	e := newEngineForTest(t, Options{})
	items := complete(t, e, Request{Line: "cd /tmp && git ch"})
	if !has(items, "cd /tmp && git checkout") {
		t.Fatalf("-> %q, want the separator kept in the accepted line", textsOf(items))
	}
}

func TestCompleteRespectsLimit(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 3})
	if items := complete(t, e, Request{Line: "git "}); len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	if items := complete(t, e, Request{Line: "git ", Limit: 7}); len(items) != 7 {
		t.Fatalf("got %d items, want 7", len(items))
	}
}

func TestCompleteHistoryRanksFirst(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 10})
	cwd := "/repo/one"
	for i := 0; i < 5; i++ {
		e.Record(Event{Cmd: "git commit", Cwd: cwd})
	}
	items := complete(t, e, Request{Line: "git c", Cwd: cwd})
	if len(items) == 0 || strings.TrimRight(items[0].Text, " ") != "git commit" {
		t.Fatalf("-> %q, want git commit first", textsOf(items))
	}
	// Global usage still counts elsewhere, but the directory that actually ran
	// the command must score it higher.
	other := complete(t, e, Request{Line: "git c", Cwd: "/elsewhere"})
	hereScore, thereScore := scoreOf(items, "git commit"), scoreOf(other, "git commit")
	if thereScore <= 0 {
		t.Fatalf("global frecency did not survive the cwd change: %v", thereScore)
	}
	if hereScore <= thereScore {
		t.Fatalf("cwd-scoped score %.3f must beat global-only %.3f", hereScore, thereScore)
	}
}

func TestCompleteHistoryAnswersUnknownCommands(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 10})
	cwd := "/repo"
	e.Record(Event{Cmd: "kubectl get pods -A", Cwd: cwd})
	items := complete(t, e, Request{Line: "kubectl get", Cwd: cwd})
	if !has(items, "kubectl get pods -A") {
		t.Fatalf("-> %q, want the history line", textsOf(items))
	}
}

func TestCompleteTransitionBoost(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 50})
	cwd := "/repo"
	for i := 0; i < 4; i++ {
		e.Record(Event{Cmd: "git add .", Cwd: cwd})
		e.Record(Event{Cmd: "git stash pop", Cwd: cwd, Prev: "git add ."})
		e.Record(Event{Cmd: "git status", Cwd: cwd, Prev: "git add ."})
	}
	// Same frecency, but stash follows "git add" three times out of four.
	withPrev := complete(t, e, Request{Line: "git st", Cwd: cwd, Prev: "git add ."})
	withoutPrev := complete(t, e, Request{Line: "git st", Cwd: cwd})
	if len(withPrev) == 0 || len(withoutPrev) == 0 {
		t.Fatalf("no rows: %q / %q", textsOf(withPrev), textsOf(withoutPrev))
	}
	if withPrev[0].Score <= withoutPrev[0].Score {
		t.Fatalf("transition did not raise the score: %v vs %v", withPrev[0].Score, withoutPrev[0].Score)
	}
}

func TestGeneratorFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha.txt", "beta.txt", ".hidden", "gamma"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEngineForTest(t, Options{})

	items := complete(t, e, Request{Line: "cat ", Cwd: dir})
	if !has(items, "cat alpha.txt") || !has(items, "cat beta.txt") {
		t.Fatalf("-> %q, want the files in %s", textsOf(items), dir)
	}
	if has(items, "cat .hidden") {
		t.Fatalf("hidden file offered without a dot: %q", textsOf(items))
	}
	if !has(items, "cat subdir/") {
		t.Fatalf("directory offered without a trailing slash: %q", textsOf(items))
	}

	// cd only takes directories.
	items = complete(t, e, Request{Line: "cd ", Cwd: dir})
	if !has(items, "cd subdir/") || has(items, "cd alpha.txt") {
		t.Fatalf("cd -> %q, want only directories", textsOf(items))
	}

	// The dot is what reveals hidden entries.
	items = complete(t, e, Request{Line: "cat .", Cwd: dir})
	if !has(items, "cat .hidden") {
		t.Fatalf("cat . -> %q, want the hidden file", textsOf(items))
	}
}

func TestGeneratorDoesNotRunForCommandNames(t *testing.T) {
	e := newEngineForTest(t, Options{})
	// "git " is a command position: git has children, so no branch generator.
	items := complete(t, e, Request{Line: "git ", Cwd: t.TempDir()})
	for _, it := range items {
		if it.Kind == KindGenerator {
			t.Fatalf("generator ran in a command position: %q", it.Text)
		}
	}
}

func TestGeneratorBudget(t *testing.T) {
	slow := slowGenerator{delay: 80 * time.Millisecond}
	e := newEngineForTest(t, Options{
		Generators:      []Generator{slow},
		GeneratorBudget: 5 * time.Millisecond,
	})
	start := time.Now()
	items := complete(t, e, Request{Line: "cat ", Cwd: t.TempDir()})
	elapsed := time.Since(start)
	if elapsed > 60*time.Millisecond {
		t.Fatalf("budget not enforced: %v", elapsed)
	}
	for _, it := range items {
		if it.Kind == KindGenerator {
			t.Fatalf("slow generator leaked a row after the budget: %q", it.Text)
		}
	}
}

type slowGenerator struct{ delay time.Duration }

func (s slowGenerator) Name() string { return "files" }
func (s slowGenerator) Complete(ctx context.Context, _ GenRequest) ([]string, error) {
	select {
	case <-time.After(s.delay):
		return []string{"too-late"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConcurrentComplete(t *testing.T) {
	e := newEngineForTest(t, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if _, err := e.Complete(context.Background(), Request{Line: "git ch", Cwd: "/repo"}); err != nil {
					t.Errorf("concurrent Complete: %v", err)
					return
				}
				if j%50 == 0 {
					e.Record(Event{Cmd: "git commit", Cwd: "/repo"})
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestHistoryKeepsTheLineBeforeTheSeparator(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 10})
	cwd := "/repo"
	e.Record(Event{Cmd: "git status", Cwd: cwd})
	items := complete(t, e, Request{Line: "cd /tmp && git st", Cwd: cwd})
	if !has(items, "cd /tmp && git status") {
		t.Fatalf("-> %q, want the separator kept in the history row", textsOf(items))
	}
	for _, it := range items {
		if it.Text == "git status" {
			t.Fatalf("history row dropped the line before the separator: %q", it.Text)
		}
	}
}

// A flag ends the command path but not the command: everything after it still
// belongs to the node the user reached.
func TestFlagsAfterFlags(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 10, DisableGenerators: true})
	for _, line := range []string{"git log --", "git log --oneline --", "git commit --no-verify --"} {
		if items := complete(t, e, Request{Line: line}); len(items) == 0 {
			t.Errorf("Complete(%q) = no rows", line)
		}
	}
}

// A bare prompt has nothing to match, so the used commands are the answer.
// History never leaks across commands: a remembered "ls -la" must not answer
// "git ".
func TestHistoryStaysScopedToTheTypedCommand(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 10, DisableGenerators: true})
	e.Record(Event{Cmd: "ls -la", Cwd: "/repo"})
	items := complete(t, e, Request{Line: "git ", Cwd: "/repo"})
	for _, it := range items {
		if it.Kind == KindHistory && !strings.HasPrefix(it.Text, "git ") {
			t.Fatalf("history row %q does not belong to the typed command", it.Text)
		}
	}
}

func TestBarePromptPrefersHistory(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 5, DisableGenerators: true})
	for i := 0; i < 20; i++ {
		e.Record(Event{Cmd: "docker compose up", Cwd: "/repo"})
	}
	items := complete(t, e, Request{Line: "", Cwd: "/repo"})
	if len(items) == 0 || strings.TrimRight(items[0].Text, " ") != "docker compose up" {
		t.Fatalf("first row = %q, want the used command", firstText(items))
	}
}

// The completion is a whole line, so a value with a space in it must not turn
// one argument into two.
func TestQuotedPathCompletionKeepsQuotes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "my file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEngineForTest(t, Options{Limit: 10, Generators: []Generator{filesGenerator{}}})
	for _, line := range []string{`cat "my fi`, `cat 'my fi`, `cat my\ fi`} {
		items := complete(t, e, Request{Line: line, Cwd: dir})
		if len(items) == 0 {
			t.Fatalf("Complete(%q) = no rows", line)
		}
		got := items[0].Text
		if strings.Count(got, "my file.txt") != 1 || !strings.Contains(got, `'`) && !strings.Contains(got, `"`) && !strings.Contains(got, `\`) {
			t.Errorf("Complete(%q) = %q, want a single quoted argument", line, got)
		}
	}
}

// "FOO=1 git st" is a git command; the assignment must not enter the query.
func TestAssignmentPrefixStillCompletes(t *testing.T) {
	e := newEngineForTest(t, Options{Limit: 10, DisableGenerators: true})
	e.Record(Event{Cmd: "git status", Cwd: "/repo"})
	items := complete(t, e, Request{Line: "FOO=1 git st", Cwd: "/repo"})
	if !has(items, "FOO=1 git status") {
		t.Fatalf("-> %q, want the assignment kept and the command completed", textsOf(items))
	}
}

func firstText(items []Item) string {
	if len(items) == 0 {
		return ""
	}
	return items[0].Text
}
