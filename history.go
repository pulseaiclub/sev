package sev

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one executed command. Hosts append these as their shell runs; the
// engine derives frecency and transitions from them and never touches the disk.
type Event struct {
	Cmd  string
	Cwd  string
	Prev string // previous command in the same shell, if known
	At   int64  // unix seconds; 0 means now
	Exit *int   // exit status when available
}

// History is the in-memory half of the engine's signals: whole command lines for
// prefix completion, per-skeleton counters for frecency, and a markov table for
// "what usually follows this".
//
// The data is kept query-shaped rather than tabular, so a completion is a binary
// search plus a bounded number of arithmetic operations:
//
//   - ordered is the command list kept sorted by name on insert, which is what
//     makes prefix completion a range search instead of a scan;
//   - top is the shortlist that a bare prompt ranks, because "the most used
//     commands" must not cost a pass over the whole history;
//   - each transition row carries its own total, so P(next|prev) is one division
//     instead of a sum over the row.
//
// Reads take a shared lock only; the exclusive lock is taken by Record, and the
// work under it is bounded (an insert, a 128-entry shortlist update, and — rarely
// — an eviction pass).
type History struct {
	mu      sync.RWMutex
	entries map[string]*entry
	ordered []*entry
	skels   map[string]*skelStat
	trans   map[string]*transRow
	top     []*entry
	topMin  uint32
	max     int
	now     func() int64

	runs atomic.Int64 // lock-free hint that history is non-empty
}

type entry struct {
	cmd    string
	all    uint32
	lastAt int64
	cwds   map[string]*cwdStat
}

type cwdStat struct {
	count  uint32
	lastAt int64
}

type skelStat struct {
	all    uint32
	lastAt int64
	cwds   map[string]uint32
}

// transRow is the markov row for one skeleton. total is maintained so a
// probability lookup is a single division.
type transRow struct {
	total uint32
	next  map[string]uint32
}

const (
	// DefaultHistoryMax bounds the in-memory history. 20k distinct commands is
	// more than a heavy user types in a year.
	DefaultHistoryMax = 20000
	// topN is how many entries a bare prompt ranks. It is larger than any
	// sensible picker so that directory scoping still has room to reorder.
	topN = 128
)

// NewHistory returns an empty history bounded at max entries (0 = default).
func NewHistory(max int) *History {
	if max <= 0 {
		max = DefaultHistoryMax
	}
	return &History{
		entries: make(map[string]*entry, 64),
		skels:   make(map[string]*skelStat, 64),
		trans:   make(map[string]*transRow, 64),
		max:     max,
		now:     func() int64 { return time.Now().Unix() },
	}
}

// Record folds one executed command into the counters.
func (h *History) Record(ev Event) {
	cmd := strings.TrimSpace(ev.Cmd)
	if cmd == "" {
		return
	}
	at := ev.At
	if at == 0 {
		at = h.now()
	}
	skel := skeleton(cmd)

	h.mu.Lock()
	defer h.mu.Unlock()

	h.runs.Add(1)

	e := h.entries[cmd]
	if e == nil {
		e = &entry{cmd: cmd, all: 1, lastAt: at, cwds: map[string]*cwdStat{}}
		h.entries[cmd] = e
		h.insertOrdered(e)
		h.touchTop(e)
		// Evict after the insert, and after lastAt is set: the command that was
		// just run is the least stale thing in the history, never the victim.
		h.evictLocked()
	} else {
		e.all++
		if at > e.lastAt {
			e.lastAt = at
		}
		h.touchTop(e)
	}

	if ev.Cwd != "" {
		cs := e.cwds[ev.Cwd]
		if cs == nil {
			cs = &cwdStat{}
			e.cwds[ev.Cwd] = cs
		}
		cs.count++
		if at > cs.lastAt {
			cs.lastAt = at
		}
	}

	s := h.skels[skel]
	if s == nil {
		s = &skelStat{cwds: map[string]uint32{}}
		h.skels[skel] = s
	}
	s.all++
	if at > s.lastAt {
		s.lastAt = at
	}
	if ev.Cwd != "" {
		s.cwds[ev.Cwd]++
	}

	if prev := strings.TrimSpace(ev.Prev); prev != "" {
		ps := skeleton(prev)
		row := h.trans[ps]
		if row == nil {
			row = &transRow{next: make(map[string]uint32, 8)}
			h.trans[ps] = row
		}
		row.next[skel]++
		row.total++
	}
}

// insertOrdered keeps the name-sorted key slice current. A new command is the
// only case that pays the copy, and a new command is rare.
func (h *History) insertOrdered(e *entry) {
	i := sort.Search(len(h.ordered), func(i int) bool { return h.ordered[i].cmd >= e.cmd })
	h.ordered = append(h.ordered, nil)
	copy(h.ordered[i+1:], h.ordered[i:])
	h.ordered[i] = e
}

// touchTop maintains the shortlist a bare prompt ranks, so "most used up front"
// is a 128-entry sort instead of a full-history pass on every keystroke.
func (h *History) touchTop(e *entry) {
	if len(h.top) < topN {
		for _, t := range h.top {
			if t == e {
				return
			}
		}
		h.top = append(h.top, e)
		if len(h.top) == topN {
			h.recomputeTopMin()
		}
		return
	}
	if e.all < h.topMin {
		return
	}
	weakest := 0
	for i := range h.top {
		if h.top[i].all < h.top[weakest].all {
			weakest = i
		}
	}
	if e.all > h.top[weakest].all {
		h.top[weakest] = e
		h.recomputeTopMin()
	}
}

func (h *History) recomputeTopMin() {
	if len(h.top) == 0 {
		h.topMin = 0
		return
	}
	h.topMin = h.top[0].all
	for _, t := range h.top[1:] {
		if t.all < h.topMin {
			h.topMin = t.all
		}
	}
}

// evictLocked keeps the maps bounded. Eviction is by staleness, in one pass that
// drops a quarter of the history at a time: at steady state this runs once per
// thousand commands, never per command.
func (h *History) evictLocked() {
	if len(h.entries) <= h.max {
		return
	}
	target := h.max - h.max/4
	all := make([]*entry, 0, len(h.entries))
	for _, e := range h.entries {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].lastAt > all[j].lastAt })
	for _, e := range all[target:] {
		delete(h.entries, e.cmd)
	}

	// Rebuild the derived views from the survivors rather than repairing them
	// entry by entry: this path is already the rare one.
	h.ordered = h.ordered[:0]
	h.top = h.top[:0]
	for _, e := range h.entries {
		h.ordered = append(h.ordered, e)
		h.top = append(h.top, e)
	}
	sort.Slice(h.ordered, func(i, j int) bool { return h.ordered[i].cmd < h.ordered[j].cmd })
	if len(h.top) > topN {
		h.top = h.top[:topN]
	}
	h.recomputeTopMin()

	// Skeleton and transition tables follow the entries: past a generous
	// multiple they are rebuilt from what survived, which drops rows whose
	// commands are gone.
	if len(h.skels) > 3*h.max {
		h.skels = make(map[string]*skelStat, len(h.entries))
		for _, e := range h.entries {
			sk := skeleton(e.cmd)
			s := h.skels[sk]
			if s == nil {
				s = &skelStat{cwds: map[string]uint32{}}
				h.skels[sk] = s
			}
			s.all += e.all
			if e.lastAt > s.lastAt {
				s.lastAt = e.lastAt
			}
			for cwd, cs := range e.cwds {
				s.cwds[cwd] += cs.count
			}
		}
	}
	if len(h.trans) > 3*h.max {
		for prev, row := range h.trans {
			if _, ok := h.skels[prev]; !ok {
				delete(h.trans, prev)
			}
			_ = row
		}
	}
}

// Frecency is the 0..1 weight of a command skeleton in a directory: how often it
// runs (per-directory weighted over global) times how recently it ran. A 14-day
// half-life matches how quickly a project's command mix turns over.
func (h *History) Frecency(skel, cwd string) float64 {
	if skel == "" {
		return 0
	}
	h.mu.RLock()
	s := h.skels[skel]
	if s == nil {
		h.mu.RUnlock()
		return 0
	}
	var local uint32
	if cwd != "" {
		local = s.cwds[cwd]
	}
	all, lastAt := s.all, s.lastAt
	h.mu.RUnlock()
	return frecency(local, all, lastAt, h.now())
}

// frecency is the formula in one place: frequency, local-first, times recency.
func frecency(local, all uint32, lastAt, now int64) float64 {
	if all == 0 {
		return 0
	}
	weight := 0.6*math.Log1p(float64(local)) + 0.4*math.Log1p(float64(all))
	if weight == 0 {
		return 0
	}
	return (weight / (1 + weight)) * recencyDecay(lastAt, now)
}

// Transition is P(next | prev) from observed command sequences, in 0..1.
func (h *History) Transition(prevSkel, nextSkel string) float64 {
	if prevSkel == "" || nextSkel == "" {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	row := h.trans[prevSkel]
	if row == nil || row.total == 0 {
		return 0
	}
	return float64(row.next[nextSkel]) / float64(row.total)
}

// Hit is one memory matching a typed prefix.
type Hit struct {
	Cmd  string
	Frec float64
}

// shortlist is the Prefix working set. It lives in a pool because Prefix runs
// concurrently under the read lock and must not share mutable state.
type shortlist struct {
	buf []*entry
	all []uint32
}

var shortPool = sync.Pool{New: func() any {
	sl := &shortlist{buf: make([]*entry, 0, 64), all: make([]uint32, 0, 64)}
	return sl
}}

// Prefix returns the most valuable commands that start with query, capped at
// limit. The scan walks the sorted key slice, so it touches only the prefix
// range, and the arithmetic — two log1p and a decay exponential per candidate —
// runs only on a bounded shortlist. A three-letter prefix can match every entry
// in a 20k history; keeping that off the keystroke path is the difference
// between 6 ms and 0.3 ms.
func (h *History) Prefix(query, cwd string, limit int, dst []Hit) []Hit {
	dst = dst[:0]
	if limit <= 0 {
		return dst
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.ordered) == 0 {
		return dst
	}

	lo := 0
	if query != "" {
		lo = sort.Search(len(h.ordered), func(i int) bool { return h.ordered[i].cmd >= query })
	}

	// Shortlist by raw count first: an integer compare per candidate, no math.
	k := min(max(2*limit, 16), 512)
	sl := shortPool.Get().(*shortlist)
	defer shortPool.Put(sl)
	short, all := sl.buf[:0], sl.all[:0]
	for i := lo; i < len(h.ordered); i++ {
		e := h.ordered[i]
		if query != "" && !hasPrefix(e.cmd, query) {
			break
		}
		if len(short) < k {
			short = append(short, e)
			all = append(all, e.all)
			continue
		}
		if e.all <= all[0] {
			continue
		}
		// Replace the weakest survivor; k is small enough that a linear scan for
		// the minimum is cheaper than a heap and needs no allocation.
		weak := 0
		for j := 1; j < len(all); j++ {
			if all[j] < all[weak] {
				weak = j
			}
		}
		short[weak], all[weak] = e, e.all
	}
	sl.buf, sl.all = short, all

	now := h.now()
	best := make([]Hit, 0, len(short))
	for _, e := range short {
		var local uint32
		if cs := e.cwds[cwd]; cs != nil {
			local = cs.count
		}
		best = append(best, Hit{Cmd: e.cmd, Frec: frecency(local, e.all, e.lastAt, now)})
	}
	sort.Slice(best, func(i, j int) bool {
		if best[i].Frec != best[j].Frec {
			return best[i].Frec > best[j].Frec
		}
		return best[i].Cmd < best[j].Cmd
	})
	if len(best) > limit {
		best = best[:limit]
	}
	return append(dst, best...)
}

// Top returns the most valuable commands overall, which is what a bare prompt
// ranks. It reads the maintained shortlist, so its cost does not grow with the
// history.
func (h *History) Top(cwd string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.top) == 0 {
		return nil
	}
	now := h.now()
	rows := make([]Hit, 0, len(h.top))
	for _, e := range h.top {
		var local uint32
		if cs := e.cwds[cwd]; cs != nil {
			local = cs.count
		}
		rows = append(rows, Hit{Cmd: e.cmd, Frec: frecency(local, e.all, e.lastAt, now)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Frec != rows[j].Frec {
			return rows[i].Frec > rows[j].Frec
		}
		return rows[i].Cmd < rows[j].Cmd
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Cmd
	}
	return out
}

// Count reports how many commands the history has recorded. It is lock-free on
// purpose: the completion path calls it on every keystroke, and an empty history
// lets ranking skip both map lookups and skeleton extraction entirely.
func (h *History) Count() int { return int(h.runs.Load()) }

// Len reports how many distinct commands the history knows.
func (h *History) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.entries)
}

// LoadJSONL reads the project history format phi's shellhist writes:
// {"v":1,"at":<unix milli>,"cwd":"/repo","cmd":"git status","exit":0}
// Lines it cannot parse are skipped rather than failing the load, because a
// history file is user data and one corrupt line must not cost all of it.
func (h *History) LoadJSONL(r io.Reader) (int, error) {
	type record struct {
		Version int    `json:"v"`
		At      int64  `json:"at"`
		Cwd     string `json:"cwd"`
		Cmd     string `json:"cmd"`
		Exit    *int   `json:"exit"`
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var prev, prevCwd string
	loaded := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil || strings.TrimSpace(rec.Cmd) == "" {
			continue
		}
		at := rec.At
		if at > 1_000_000_000_000 { // milliseconds → seconds
			at /= 1000
		}
		ev := Event{Cmd: rec.Cmd, Cwd: rec.Cwd, At: at, Exit: rec.Exit}
		if rec.Cwd == prevCwd {
			ev.Prev = prev
		}
		h.Record(ev)
		prev, prevCwd = rec.Cmd, rec.Cwd
		loaded++
	}
	return loaded, sc.Err()
}

// recencyDecay is exp(-age/14d) on the unix-seconds clock.
func recencyDecay(lastAt, now int64) float64 {
	if lastAt <= 0 {
		return 0
	}
	age := float64(now-lastAt) / 86400
	if age < 0 {
		return 1
	}
	return math.Exp(-age / 14)
}

// skeleton reduces a command line to the part that identifies the operation:
// "git checkout -b feature" -> "git checkout". Generators and transitions are
// keyed by skeleton, so flags and arguments never split the statistics.
func skeleton(cmd string) string {
	first, rest := nextWord(cmd)
	if first == "" {
		return ""
	}
	second, _ := nextWord(rest)
	if second == "" {
		return first
	}
	return first + " " + second
}

func nextWord(s string) (word, rest string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start := i
	for i < len(s) && s[i] != ' ' && s[i] != '\t' {
		i++
	}
	return s[start:i], s[i:]
}
