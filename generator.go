package sev

import (
	"context"
	"sort"
	"strings"
)

// Kind says where an item came from, so a UI can label a guess differently from
// a fact.
type Kind uint8

const (
	KindSpec      Kind = iota + 1 // a command or flag from the compiled spec tree
	KindHistory                   // a line the user actually ran
	KindGenerator                 // a dynamic argument (path, branch, …)
	KindHook                      // a completion supplied by the optional hook
)

func (k Kind) String() string {
	switch k {
	case KindSpec:
		return "spec"
	case KindHistory:
		return "history"
	case KindGenerator:
		return "generator"
	case KindHook:
		return "hook"
	default:
		return "unknown"
	}
}

// Item is one completion. Text is the whole line to put in the prompt: hosts can
// replace the typed command with it without knowing where the suggestion starts.
type Item struct {
	Text  string
	Desc  string
	Kind  Kind
	Score float64
}

// GenRequest is what a generator gets: the token being typed (not the whole
// line), the tokens before it, and the working directory. A generator returns
// token-level texts; the engine prepends the rest of the line itself, so a
// generator never has to know how its answer will be spliced back in.
type GenRequest struct {
	Tokens  []string
	Partial string
	Cwd     string
	Limit   int
}

// Generator supplies dynamic arguments for one spec key ("files", "git.branch").
// Complete runs on the caller's goroutine under a context bounded by
// Options.GeneratorBudget; a generator that cannot answer in time should return
// the context error and let the engine drop its rows.
type Generator interface {
	Name() string
	Complete(ctx context.Context, req GenRequest) ([]string, error)
}

// generatorSet is the immutable name → generator table.
type generatorSet map[string]Generator

func newGeneratorSet(gens []Generator) generatorSet {
	if len(gens) == 0 {
		return nil
	}
	set := make(generatorSet, len(gens))
	for _, g := range gens {
		if g == nil || g.Name() == "" {
			continue
		}
		set[g.Name()] = g
	}
	return set
}

// defaultGenerators is what Options.Generators gets when the caller names none:
// the ones that pay for themselves without configuration.
func defaultGenerators() []Generator {
	return []Generator{
		filesGenerator{},
		filesGenerator{dirsOnly: true},
		gitGenerator{mode: "branch"},
		gitGenerator{mode: "tag"},
		gitGenerator{mode: "remote"},
		gitGenerator{mode: "stash"},
	}
}

// sortedPrefix filters candidate strings by prefix, keeping at most limit and
// sorting the survivors so the picker order is stable between identical calls.
func sortedPrefix(cands []string, prefix string, limit int) []string {
	if limit <= 0 || limit > len(cands) {
		limit = len(cands)
	}
	out := make([]string, 0, limit)
	for _, c := range cands {
		if prefix == "" || strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
