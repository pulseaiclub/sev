package sev

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// Options tunes an Engine. The zero value is usable: default limit, default
// generators, generator budget capped at 20 ms.
type Options struct {
	// Limit is how many items Complete returns when Request.Limit is zero.
	Limit int
	// GeneratorBudget bounds the built-in slow sources for one call. The spec
	// tree and history are memory-only and finish in microseconds. A
	// generator that has not answered when the budget expires contributes no
	// rows; one that ignores its context cannot be preempted, so implementations
	// are expected to check ctx (the bundled ones do).
	GeneratorBudget time.Duration
	// Generators replaces the default set (paths, directories, git branches).
	// Pass an empty non-nil slice to ship an engine with no dynamic sources.
	Generators []Generator
	// DisableGenerators keeps the configured generators but skips them, which is
	// what a host uses while the user is still typing a command name.
	DisableGenerators bool
	// DisableHistory skips history ranking even when a History is attached.
	DisableHistory bool
	// History is the in-memory signal store. When nil the engine allocates one,
	// so a host can start completing before it has any history.
	History *History
	// CompletionHook optionally handles a request before the built-in sources.
	// It runs outside GeneratorBudget; the caller owns its deadline.
	CompletionHook CompletionHook
}

const (
	defaultGeneratorBudget = 20 * time.Millisecond
	// historyWidth bounds how many history rows one query considers, so a host
	// asking for a tall picker cannot make the memory path scale with the Limit.
	historyWidth = 64
)

// Engine answers completion queries. Apart from the history it owns it is
// immutable, so one Engine serves concurrent callers.
type Engine struct {
	ix   *index
	hist *History
	gens generatorSet
	opts Options
	pool sync.Pool
}

// candidate is one row during a call. Spec rows keep their name and prefix
// instead of a joined string, so a query that gathers hundreds of children only
// builds the ten strings it returns.
type candidate struct {
	text   string // full line, for history and generator rows
	name   string // token text, for spec rows
	prefix string // the line up to the token, shared by every spec row
	desc   string
	kind   Kind
	match  float64
	score  float64
	space  bool // a space can follow: the command takes subcommands or arguments
}

// full materialises the line to put in the prompt. Only the rows that survive
// ranking reach it.
func (c *candidate) full() string {
	if c.text != "" {
		return c.text
	}
	if c.space {
		return c.prefix + c.name + " "
	}
	return c.prefix + c.name
}

// sameKey reports whether another row is the same command, without joining the
// prefix and name: merging rows is on the keystroke path for a history-heavy
// prefix, and a concatenation per comparison would make it allocate per row.
func (c *candidate) sameKey(other *candidate) bool {
	switch {
	case c.text != "" && other.text != "":
		return strings.TrimRight(c.text, " ") == strings.TrimRight(other.text, " ")
	case c.text != "":
		return other.matchesKey(strings.TrimRight(c.text, " "))
	case other.text != "":
		return c.matchesKey(strings.TrimRight(other.text, " "))
	}
	return c.name == other.name && c.prefix == other.prefix
}

func (c *candidate) matchesKey(key string) bool {
	if c.text != "" {
		return strings.TrimRight(c.text, " ") == key
	}
	if c.prefix == "" {
		return c.name == key
	}
	return len(key) == len(c.prefix)+len(c.name) &&
		hasPrefix(key, c.prefix) &&
		key[len(c.prefix):] == c.name
}

// sortKey is the row identity for tie-breaking, without the join.
func (c *candidate) sortKey() string {
	if c.text != "" {
		return c.text
	}
	return c.name
}

// scratch is the per-call working set. Reusing it through a pool keeps Complete
// at a couple of allocations for ten items instead of hundreds.
type scratch struct {
	toks  []Token
	cands []candidate
	hits  []Hit
	used  []string
}

// New loads the embedded command tree and returns a ready engine.
func New(opts Options) (*Engine, error) {
	ix, err := loadGzip(specDB)
	if err != nil {
		return nil, err
	}
	return newEngine(ix, opts), nil
}

// NewWithData builds an engine over a caller-supplied specdb blob, which is how
// tests and hosts with their own command set avoid the embedded one.
func NewWithData(raw []byte, opts Options) (*Engine, error) {
	ix, err := loadIndex(raw)
	if err != nil {
		return nil, err
	}
	return newEngine(ix, opts), nil
}

func newEngine(ix *index, opts Options) *Engine {
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	if opts.GeneratorBudget <= 0 {
		opts.GeneratorBudget = defaultGeneratorBudget
	}
	gens := opts.Generators
	if gens == nil {
		gens = defaultGenerators()
	}
	hist := opts.History
	if hist == nil {
		hist = NewHistory(0)
	}
	e := &Engine{ix: ix, hist: hist, gens: newGeneratorSet(gens), opts: opts}
	e.pool.New = func() any { return &scratch{} }
	return e
}

// History exposes the signal store so a host can Record what its shell runs and
// persist it between sessions.
func (e *Engine) History() *History { return e.hist }

// Record is a convenience for History().Record.
func (e *Engine) Record(ev Event) { e.hist.Record(ev) }

// Request is one completion query: the line as typed plus the context that
// ranking depends on.
type Request struct {
	Line  string
	Cwd   string
	Prev  string // the previous command in this shell, for transition scoring
	Limit int
}

// CompletionHook optionally takes ownership of a completion request. Request.Limit
// is normalized before the call. Returning handled=false and a nil error falls
// back to the built-in sources; handled=true suppresses them even with no items.
// Any error propagates. Handled items retain their order and scores and are
// truncated to the limit. The returned slice must be safe for the caller to own.
//
// Hooks run synchronously, outside GeneratorBudget, and must honor the caller's
// context: the engine checks cancellation before and after, but cannot preempt a
// callback. An Engine can invoke its hook concurrently; callbacks must be safe
// for concurrent use.
type CompletionHook func(context.Context, Request) ([]Item, bool, error)

// Complete returns ranked completions for the line in req, or the hook's items
// when it handles the request. Hook errors and cancellation propagate. A built-in
// generator that fails or runs out of budget contributes no rows.
func (e *Engine) Complete(ctx context.Context, req Request) ([]Item, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = e.opts.Limit
	}

	if hook := e.opts.CompletionHook; hook != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req.Limit = limit
		items, handled, err := hook(ctx, req)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if handled {
			if len(items) > limit {
				items = items[:limit]
			}
			return items, nil
		}
	}

	sc := e.pool.Get().(*scratch)
	defer func() {
		sc.cands = sc.cands[:0]
		sc.hits = sc.hits[:0]
		e.pool.Put(sc)
	}()

	toks, _, partial := scan(req.Line, sc.toks)
	cands := sc.cands[:0]

	cur := e.ix.root
	known := true
	walk := len(toks)
	if partial {
		walk--
	}
	for i := 0; i < walk; i++ {
		kid, ok := e.ix.child(cur, toks[i].Text)
		if ok {
			cur = kid
			continue
		}
		// A flag or an argument ends the command path but not the command: the
		// node we are on still owns the rest of the line, which is why "git log
		// --oneline --" completes flags while "git log --" always did.
		if i == 0 {
			known = false
		}
		break
	}

	partialTok, prefix, rawPartial := "", req.Line, ""
	if partial {
		last := toks[len(toks)-1]
		partialTok, prefix = last.Text, req.Line[:last.Start]
		rawPartial = req.Line[last.Start:]
	}

	// The command text starts at the first token, not at the pipeline separator:
	// "FOO=1 git st" completes from "git st", not from "FOO=1 git st".
	cmdStart := 0
	if len(toks) > 0 {
		cmdStart = toks[0].Start
	}

	var kids []uint32
	if known {
		// A flag already on the line is not a suggestion: "git log --oneline --"
		// must not offer --oneline again.
		used := sc.used[:0]
		for _, t := range toks[:walk] {
			if strings.HasPrefix(t.Text, "-") {
				used = append(used, t.Text)
			}
		}
		sc.used = used
		cands = e.specCandidates(cur, partialTok, prefix, used, scope(partialTok, limit), cands, 0)
		kids = e.ix.childRange(cur, partialTok)
	}

	if !e.opts.DisableHistory {
		width := min(max(limit*3, 8), historyWidth)
		cmdPrefix := req.Line[:cmdStart]
		switch query := strings.TrimRight(req.Line[cmdStart:], " \t"); {
		case query != "":
			sc.hits = e.hist.Prefix(query, req.Cwd, width, sc.hits)
			for _, h := range sc.hits {
				match := matchTier(query, h.Cmd)
				if partialTok == "" {
					match = 0.95
				}
				cands = appendUnique(cands, candidate{
					text:  cmdPrefix + h.Cmd,
					desc:  "history",
					kind:  KindHistory,
					match: match,
				})
			}
		case partialTok == "" && strings.TrimSpace(req.Line) == "":
			// Only a genuinely empty prompt ranks "what the user runs" with no
			// prefix to match. With a command already typed ("git ") the case
			// above answers, because that list has to stay scoped to the command:
			// offering a remembered "ls -la" under "git " would be wrong.
			// The shortlist is maintained by the history, so this costs a
			// 128-entry sort, not a history pass.
			top := e.hist.Top(req.Cwd, width)
			for i, cmd := range top {
				cands = appendUnique(cands, candidate{
					text:  cmdPrefix + cmd,
					desc:  "history",
					kind:  KindHistory,
					match: 0.99 - 0.002*float64(i),
				})
			}
		}
	}

	// A generator answers an argument position, so it only runs when the token
	// being typed is not already a command name.
	if known && !e.opts.DisableGenerators && len(e.gens) > 0 &&
		!strings.HasPrefix(partialTok, "-") && len(kids) == 0 {
		if g := e.gens[e.ix.genKey(cur)]; g != nil {
			args := make([]string, 0, walk)
			for _, t := range toks[:walk] {
				args = append(args, t.Text)
			}
			gctx, cancel := context.WithTimeout(ctx, e.opts.GeneratorBudget)
			names, err := g.Complete(gctx, GenRequest{
				Tokens:  args,
				Partial: partialTok,
				Cwd:     req.Cwd,
				Limit:   limit,
			})
			cancel()
			if err == nil {
				for _, name := range names {
					if name == "" {
						continue
					}
					cands = appendUnique(cands, candidate{
						text:  prefix + spliceValue(rawPartial, name),
						desc:  g.Name(),
						kind:  KindGenerator,
						match: 0.9,
					})
				}
			}
		}
	}

	// Rank. Match quality is already known; frecency and transitions are only
	// worth a map lookup when there is history to look up.
	if partialTok != "" && e.hist.Count() > 0 {
		var shared string
		if walk >= 2 {
			// Every row continues the same operation ("git checkout …"), so the
			// skeleton is the same string for the whole call.
			shared = toks[0].Text + " " + toks[1].Text
		}
		prevSkel := skeleton(req.Prev)
		for i := range cands {
			c := &cands[i]
			skel := shared
			if skel == "" {
				// One word typed: the row's own name completes the operation
				// skeleton ("git" + "commit"). Spec rows keep their name field
				// without the line prefix, so the join cannot double it.
				switch {
				case walk == 0 && c.text != "":
					skel = skeleton(c.text)
				case walk == 0:
					skel = c.name
				case c.text != "":
					skel = skeleton(c.text)
				default:
					skel = toks[0].Text + " " + c.name
				}
			}
			frec := e.hist.Frecency(skel, req.Cwd)
			trans := e.hist.Transition(prevSkel, skel)
			c.score = weightMatch*c.match + weightFrecency*frec + weightTransition*trans
		}
	} else {
		for i := range cands {
			cands[i].score = cands[i].match
		}
	}

	slices.SortFunc(cands, func(a, b candidate) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return strings.Compare(a.sortKey(), b.sortKey())
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}

	items := make([]Item, len(cands))
	for i := range cands {
		c := &cands[i]
		items[i] = Item{Text: c.full(), Desc: c.desc, Kind: c.kind, Score: c.score}
	}
	return items, nil
}

// spliceValue puts a completed value back into the quoting the user opened. A
// value with a space in it cannot be returned bare: "cat my file.txt" is two
// arguments, so the token keeps (or gains) quotes.
func spliceValue(rawPartial, value string) string {
	if value == "" {
		return value
	}
	switch {
	case strings.HasPrefix(rawPartial, `"`):
		return `"` + escapeDouble(value) + `"`
	case strings.HasPrefix(rawPartial, `'`):
		return `'` + value + `'`
	case strings.ContainsAny(value, " \t"):
		if strings.ContainsRune(value, '\'') {
			return `"` + escapeDouble(value) + `"`
		}
		return `'` + value + `'`
	}
	return value
}

func escapeDouble(v string) string {
	if !strings.ContainsAny(v, `"\$`+"`") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 4)
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '"', '\\', '$', '`':
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// maxCandidates bounds how many spec children one query considers when a prefix
// is being typed. scope() lowers it for a bare prompt, where match quality is
// uniform and the extra rows cannot outrank history anyway.
const maxCandidates = 256

// scope is how many children one query may gather. With nothing typed there is
// nothing to match, so the rows that can win are the used ones; gathering
// hundreds of alphabetical neighbours to discard them would cost microseconds
// per keystroke for no visible difference.
func scope(partial string, limit int) int {
	if partial != "" {
		return maxCandidates
	}
	return max(limit*3, 32)
}

// specCandidates adds the spec rows for one node: flags when the token starts
// with a dash, otherwise child commands. Rows are appended directly because the
// tree has no duplicates among one node's children.
func (e *Engine) specCandidates(cur uint32, partial, prefix string, used []string, cap int, out []candidate, depth int) []candidate {
	if strings.HasPrefix(partial, "-") {
		opts := e.ix.optSlice(cur)
		for i := range opts {
			name := e.ix.str(opts[i].name)
			if partial != "" && !hasPrefix(name, partial) {
				continue
			}
			if slices.Contains(used, name) {
				continue
			}
			out = append(out, candidate{
				name:   name,
				prefix: prefix,
				desc:   e.ix.str(opts[i].desc),
				kind:   KindSpec,
				match:  matchTier(partial, name),
			})
			if len(out) >= maxCandidates {
				return out
			}
		}
		return out
	}

	kids := e.ix.childRange(cur, partial)
	if len(kids) > cap {
		kids = kids[:cap]
	}
	exact := noStr
	for _, kid := range kids {
		nd := &e.ix.nodes[kid]
		name := e.ix.name(kid)
		if strings.HasPrefix(name, "-") && slices.Contains(used, name) {
			continue
		}
		out = append(out, candidate{
			name:   name,
			prefix: prefix,
			desc:   e.ix.desc(kid),
			kind:   KindSpec,
			match:  matchTier(partial, name),
			space:  nd.nChild > 0 || nd.gen != noStr,
		})
		if name == partial {
			exact = kid
		}
	}

	// A word the user finished typing but has not separated yet: offer what goes
	// after it, so tab-tab does not dead-end on "git branch".
	if exact != noStr && depth == 0 {
		out = e.specCandidates(exact, "", prefix+e.ix.name(exact)+" ", used, cap, out, depth+1)
	}

	// Prefix matching is the fast path; fuzzy is the fallback that rescues a
	// typo, and it only runs when prefix matching found nothing.
	if len(kids) == 0 && len(partial) >= 2 {
		out = e.fuzzyCandidates(cur, partial, prefix, out)
	}
	return out
}

// fuzzyCandidates scans one node's children for a subsequence match. Bounded by
// the node's own arity, so the worst case is a few hundred string walks.
func (e *Engine) fuzzyCandidates(cur uint32, partial, prefix string, out []candidate) []candidate {
	kids := e.ix.childSlice(cur)
	if len(kids) > maxCandidates {
		kids = kids[:maxCandidates]
	}
	for _, kid := range kids {
		name := e.ix.name(kid)
		score, ok := subsequenceScore(partial, name)
		if !ok {
			continue
		}
		nd := &e.ix.nodes[kid]
		out = append(out, candidate{
			name:   name,
			prefix: prefix,
			desc:   e.ix.desc(kid),
			kind:   KindSpec,
			match:  score,
			space:  nd.nChild > 0 || nd.gen != noStr,
		})
	}
	return out
}

// TopLevel lists the command tree's roots. Hosts use it for a bare prompt where
// there is nothing to match but history.
func (e *Engine) TopLevel(limit int) []string {
	if limit <= 0 {
		limit = e.opts.Limit
	}
	kids := e.ix.childRange(e.ix.root, "")
	if len(kids) > limit {
		kids = kids[:limit]
	}
	out := make([]string, len(kids))
	for i, kid := range kids {
		out[i] = e.ix.name(kid)
	}
	return out
}

// Size reports the compiled command count, useful for logs and health checks.
func (e *Engine) Size() int { return len(e.ix.nodes) - 1 }
