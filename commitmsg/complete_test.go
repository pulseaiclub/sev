package commitmsg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pulseaiclub/sev"
)

func TestComplete(t *testing.T) {
	t.Parallel()
	dir := repository(t, true)
	put(t, dir, "tracked", "staged evidence\n")
	gitRun(t, dir, "add", ".")
	put(t, dir, "tracked", "working evidence\n")
	calls := 0
	c := New(func(ctx context.Context, system, user string) (string, error) {
		calls++
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		checkContains(t, system, "Repository rules", "untrusted data")
		var data map[string]any
		if err := json.Unmarshal([]byte(user), &data); err != nil {
			t.Fatal(err)
		}
		if data["prefix"] != "fix: " {
			t.Fatalf("prefix=%v", data["prefix"])
		}
		checkContains(t, data["recent_subjects_style_only"].(string), "test: initial subject")
		if calls == 1 {
			checkContains(t, data["diff"].(string), "staged evidence")
			checkExcludes(t, data["diff"].(string), "working evidence")
		} else {
			checkContains(t, data["diff"].(string), "working evidence")
		}
		return "fix: retain user's $value", nil
	})
	for _, typed := range []string{"git commit -m 'fix: ", "git commit -am 'fix: ", "git commit -a -m 'fix: "} {
		start := time.Now()
		items, handled, err := c.Complete(t.Context(), sev.Request{Line: typed, Cwd: dir})
		if err != nil || !handled || len(items) != 1 {
			t.Fatalf("Complete=%v, %v, %v", items, handled, err)
		}
		want := sev.Item{Text: typed + "retain user'\\''s $value'", Desc: "AI commit message", Kind: sev.KindHook, Score: 100}
		if items[0] != want || time.Since(start) < 300*time.Millisecond {
			t.Fatalf("item=%+v, want %+v; elapsed=%v", items[0], want, time.Since(start))
		}
	}
	if calls != 3 {
		t.Fatalf("generator calls=%d, want 3", calls)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-qm", "fix: clear changes")
	items, handled, err := c.Complete(t.Context(), sev.Request{Line: "git commit -m '", Cwd: dir})
	if !errors.Is(err, ErrNoChanges) || !handled || len(items) != 0 || calls != 3 {
		t.Fatalf("repository state must not be cached: items=%v handled=%v err=%v calls=%d", items, handled, err, calls)
	}
}

func TestCompleteNoChanges(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"nonrepo", "clean", "unstaged", "unborn"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if kind != "nonrepo" {
				dir = repository(t, kind != "unborn")
				put(t, dir, "untracked", "not included\n")
				if kind == "unstaged" {
					put(t, dir, "tracked", "not staged\n")
				}
			}
			c := New(func(context.Context, string, string) (string, error) {
				t.Error("generator called without changes")
				return "", nil
			})
			items, handled, err := c.Complete(t.Context(), sev.Request{Line: "git commit -m '", Cwd: dir})
			want := ErrNoChanges
			if kind == "nonrepo" {
				want = ErrNotRepository
			}
			if !errors.Is(err, want) || !handled || len(items) != 0 {
				t.Fatalf("items=%v handled=%v err=%v; want %v", items, handled, err, want)
			}
			var h interface{ CompletionHint() string }
			if !errors.As(err, &h) || h.CompletionHint() != want.Error() {
				t.Fatalf("missing safe completion hint: %v", err)
			}
		})
	}
	t.Run("no tracked changes", func(t *testing.T) {
		t.Parallel()
		dir := repository(t, true)
		put(t, dir, "untracked", "not included\n")
		items, handled, err := New(nil).Complete(t.Context(), sev.Request{Line: "git commit -am '", Cwd: dir})
		if !errors.Is(err, ErrNoTrackedChanges) || !handled || len(items) != 0 {
			t.Fatalf("items=%v handled=%v err=%v", items, handled, err)
		}
		var h interface{ CompletionHint() string }
		if !errors.As(err, &h) || h.CompletionHint() != ErrNoTrackedChanges.Error() {
			t.Fatalf("missing safe completion hint: %v", err)
		}
	})
}

func TestCompleteCancellationAndOrdinaryInput(t *testing.T) {
	// An invalid cwd makes reaching Git observable without global executable mocks.
	dir := filepath.Join(t.TempDir(), "missing")
	c := New(nil)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	items, handled, err := c.Complete(ctx, sev.Request{Line: "git commit -m '", Cwd: dir})
	if !errors.Is(err, context.DeadlineExceeded) || !handled || len(items) != 0 ||
		(!testing.Short() && time.Since(start) >= 250*time.Millisecond) {
		t.Fatalf("debounce cancellation: items=%v handled=%v err=%v elapsed=%v", items, handled, err, time.Since(start))
	}
	start = time.Now()
	items, handled, err = c.Complete(ctx, sev.Request{Line: "git status", Cwd: dir})
	if err != nil || handled || len(items) != 0 || (!testing.Short() && time.Since(start) >= 100*time.Millisecond) {
		t.Fatalf("ordinary input: items=%v handled=%v err=%v elapsed=%v", items, handled, err, time.Since(start))
	}
	items, handled, err = c.Complete(t.Context(), sev.Request{Line: "git commit -m '" + strings.Repeat("x", 241), Cwd: dir})
	if err == nil || !strings.Contains(err.Error(), "exceeds 240 bytes") || !handled || len(items) != 0 {
		t.Fatalf("oversize prefix: items=%v handled=%v err=%v", items, handled, err)
	}
}

func TestCompleteErrors(t *testing.T) {
	t.Parallel()
	dir := repository(t, false)
	put(t, dir, "file", "change\n")
	gitRun(t, dir, "add", ".")
	failure := errors.New("model offline")
	for _, tc := range []struct {
		name     string
		generate Generate
		want     string
	}{
		{"nil", nil, "not configured"},
		{"generator", func(context.Context, string, string) (string, error) { return "", failure }, "model offline"},
		{"prefix", func(context.Context, string, string) (string, error) { return "feat: wrong", nil }, "matching"},
		{"multiline", func(context.Context, string, string) (string, error) { return "fix: first\nsecond", nil }, "plain line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			items, handled, err := New(tc.generate).Complete(t.Context(), sev.Request{Line: "git commit -m 'fix: ", Cwd: dir})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !handled || len(items) != 0 {
				t.Fatalf("items=%v handled=%v err=%v; want %q", items, handled, err, tc.want)
			}
			if tc.name == "generator" && !errors.Is(err, failure) {
				t.Fatalf("lost generator error: %v", err)
			}
		})
	}
	t.Run("git", func(t *testing.T) {
		t.Parallel()
		items, handled, err := New(nil).Complete(t.Context(), sev.Request{Line: "git commit -m", Cwd: filepath.Join(dir, "missing")})
		if err == nil || !strings.Contains(err.Error(), "git rev-parse") || !handled || len(items) != 0 {
			t.Fatalf("items=%v handled=%v err=%v", items, handled, err)
		}
	})
	t.Run("cancel generator", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c := New(func(context.Context, string, string) (string, error) {
			cancel()
			return "fix: otherwise valid", nil
		})
		items, handled, err := c.Complete(ctx, sev.Request{Line: "git commit -m 'fix: ", Cwd: dir})
		if !errors.Is(err, context.Canceled) || !handled || len(items) != 0 {
			t.Fatalf("items=%v handled=%v err=%v", items, handled, err)
		}
	})
}

func TestGitDisablesExternalDiffAndTextconv(t *testing.T) {
	t.Parallel()
	dir := repository(t, true)
	marker := filepath.Join(dir, "executed")
	script := filepath.Join(dir, "external")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "config", "diff.external", script)
	gitRun(t, dir, "config", "diff.unsafe.textconv", script)
	put(t, dir, ".gitattributes", "tracked diff=unsafe\n")
	put(t, dir, "tracked", "updated\n")
	gitRun(t, dir, "add", ".")
	for _, all := range []bool{false, true} {
		diff, _, _, err := collect(t.Context(), dir, all)
		if err != nil {
			t.Fatal(err)
		}
		checkContains(t, diff, "+updated")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external diff or textconv executed: %v", err)
	}
}

func TestCompleteConcurrentRepositories(t *testing.T) {
	t.Parallel()
	dirs := []string{repository(t, false), repository(t, false)}
	for i, dir := range dirs {
		put(t, dir, "file", []string{"first evidence\n", "second evidence\n"}[i])
		gitRun(t, dir, "add", ".")
	}
	c := New(func(_ context.Context, _, user string) (string, error) {
		var data struct{ Prefix, Diff string }
		if err := json.Unmarshal([]byte(user), &data); err != nil {
			return "", err
		}
		if !strings.Contains(data.Diff, data.Prefix+"evidence") {
			return "", errors.New("request used another repository's diff")
		}
		return data.Prefix + "subject", nil
	})
	e, err := sev.New(sev.Options{CompletionHook: c.Complete, GeneratorBudget: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			prefix := []string{"first ", "second "}[i%2]
			typed := "git commit -m '" + prefix
			items, err := e.Complete(t.Context(), sev.Request{Line: typed, Cwd: dirs[i%2]})
			if err != nil || len(items) != 1 || items[0].Text != typed+"subject'" || items[0].Kind != sev.KindHook {
				t.Errorf("concurrent completion: items=%v err=%v", items, err)
			}
		})
	}
	wg.Wait()
}
