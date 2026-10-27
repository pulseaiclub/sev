package sev

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestCompletionHookFallback(t *testing.T) {
	opts := Options{DisableGenerators: true}
	baseline := newEngineForTest(t, opts)
	opts.CompletionHook = func(_ context.Context, req Request) ([]Item, bool, error) {
		return []Item{{Text: "ignored"}}, false, nil
	}
	e := newEngineForTest(t, opts)
	for _, req := range []Request{{Line: "git ch"}, {Line: "git log --"}, {Line: "", Limit: 2}} {
		if got, want := complete(t, e, req), complete(t, baseline, req); !slices.Equal(got, want) {
			t.Fatalf("fallback for %q = %v, want %v", req.Line, got, want)
		}
	}
}

func TestCompletionHookOwnershipAndLimit(t *testing.T) {
	for _, tc := range []struct{ configured, requested, want int }{{0, 0, 10}, {2, 0, 2}, {2, -1, 2}, {2, 1, 1}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			req := Request{Line: "git ch", Cwd: "/repo", Prev: "git status", Limit: tc.requested}
			rows := make([]Item, 12)
			for i := range rows {
				rows[i] = Item{Text: fmt.Sprint(i), Kind: KindHook, Score: float64(i)}
			}
			e := newEngineForTest(t, Options{Limit: tc.configured, CompletionHook: func(_ context.Context, got Request) ([]Item, bool, error) {
				want := req
				want.Limit = tc.want
				if got != want {
					t.Fatalf("request = %+v, want %+v", got, want)
				}
				return rows, true, nil
			}})
			// Handling must not touch the built-in tree or scratch pool.
			e.ix = nil
			e.pool.New = func() any { panic("hook allocated scratch") }
			if got := complete(t, e, req); !slices.Equal(got, rows[:tc.want]) {
				t.Fatalf("hook rows changed order, scores or limit: %v", got)
			}
		})
	}
	for _, rows := range [][]Item{nil, {}} {
		e := newEngineForTest(t, Options{CompletionHook: func(context.Context, Request) ([]Item, bool, error) {
			return rows, true, nil
		}})
		if got := complete(t, e, Request{Line: "git ch"}); len(got) != 0 {
			t.Fatalf("handled empty result fell back: %v", got)
		}
	}
	if KindHook.String() != "hook" || KindGenerator != 3 {
		t.Fatal("kind names or existing values changed")
	}
}

func TestCompletionHookErrorAndCancellation(t *testing.T) {
	failure := errors.New("hook failed")
	for _, handled := range []bool{false, true} {
		e := newEngineForTest(t, Options{CompletionHook: func(context.Context, Request) ([]Item, bool, error) {
			return []Item{{Text: "discard"}}, handled, failure
		}})
		if items, err := e.Complete(t.Context(), Request{Line: "git ch"}); !errors.Is(err, failure) || items != nil {
			t.Fatalf("error = %v, items = %v", err, items)
		}
	}
	for _, before := range []bool{false, true} {
		for _, handled := range []bool{false, true} {
			ctx, cancel := context.WithCancel(t.Context())
			calls := 0
			e := newEngineForTest(t, Options{CompletionHook: func(context.Context, Request) ([]Item, bool, error) {
				calls++
				cancel()
				return []Item{{Text: "discard"}}, handled, nil
			}})
			if before {
				cancel()
			}
			items, err := e.Complete(ctx, Request{Line: "git ch"})
			cancel()
			if !errors.Is(err, context.Canceled) || items != nil || (before && calls != 0) || (!before && calls != 1) {
				t.Fatalf("before=%v handled=%v: items=%v err=%v calls=%d", before, handled, items, err, calls)
			}
		}
	}
}

func TestCompletionHookCallerContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	e := newEngineForTest(t, Options{GeneratorBudget: time.Nanosecond, CompletionHook: func(got context.Context, _ Request) ([]Item, bool, error) {
		if got != ctx {
			t.Fatal("hook context was replaced by generator budget")
		}
		return nil, true, nil
	}})
	if _, err := e.Complete(ctx, Request{}); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionHookConcurrent(t *testing.T) {
	e := newEngineForTest(t, Options{CompletionHook: func(_ context.Context, req Request) ([]Item, bool, error) {
		return []Item{{Text: req.Cwd + req.Line, Kind: KindHook}}, true, nil
	}})
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			req := Request{Line: fmt.Sprint(i), Cwd: fmt.Sprintf("/repo%d/", i)}
			got, err := e.Complete(t.Context(), req)
			if err != nil || len(got) != 1 || got[0].Text != req.Cwd+req.Line {
				t.Errorf("concurrent request %+v: %v, %v", req, got, err)
			}
		})
	}
	wg.Wait()
}
