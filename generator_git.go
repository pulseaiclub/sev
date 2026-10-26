package sev

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// gitGenerator answers argument positions that only git knows: branches, tags,
// remotes, stashes. It is the slow tier (an exec, 2–6 ms), so it is written
// around the latency budget rather than against it:
//
//   - a hit in the cache answers instantly, stale or not (stale-while-revalidate);
//   - a miss under budget returns no rows and refreshes in the background, so
//     the next keystroke has the answer instead of paying the exec again;
//   - only one exec per directory and mode is ever in flight.
type gitGenerator struct{ mode string }

func (g gitGenerator) Name() string { return "git." + g.mode }

func (g gitGenerator) args() []string {
	switch g.mode {
	case "branch":
		return []string{"for-each-ref", "--format=%(refname:short)", "refs/heads"}
	case "tag":
		return []string{"for-each-ref", "--format=%(refname:short)", "refs/tags"}
	case "remote":
		return []string{"remote"}
	case "stash":
		return []string{"stash", "list", "--format=%gd"}
	default:
		return nil
	}
}

const (
	gitCacheTTL = 2 * time.Second
	// gitRefreshTimeout bounds the detached refresh. It is generous because
	// nobody is waiting on it: the result only has to be there next keystroke.
	gitRefreshTimeout = 2 * time.Second
)

var gitCache = struct {
	sync.Mutex
	entries  map[string]gitCacheEntry
	inflight map[string]bool
}{entries: map[string]gitCacheEntry{}, inflight: map[string]bool{}}

type gitCacheEntry struct {
	at    time.Time
	names []string
}

func (g gitGenerator) Complete(ctx context.Context, req GenRequest) ([]string, error) {
	args := g.args()
	if args == nil {
		return nil, nil
	}
	key := g.mode + "\x00" + req.Cwd

	gitCache.Lock()
	cached, ok := gitCache.entries[key]
	busy := gitCache.inflight[key]
	gitCache.Unlock()
	if ok {
		// Stale-while-revalidate: a known answer is always served immediately,
		// even after checkout created a branch. Refreshing on the request path
		// would put a 3 ms exec on a keystroke for nothing.
		if time.Since(cached.at) >= gitCacheTTL && !busy {
			go g.refresh(key, req.Cwd, args)
		}
		return sortedPrefix(cached.names, req.Partial, req.Limit), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// First miss with nobody warming the cache: one synchronous attempt.
	// Budgeted callers time out here, and the detached refresh below takes over.
	names, err := g.run(ctx, req.Cwd, args)
	if err != nil {
		// Cache the failure. Most directories are not repositories, and without
		// this every keystroke in ~/Downloads would pay a 5 ms failed exec.
		gitCache.Lock()
		gitCache.entries[key] = gitCacheEntry{at: time.Now()}
		gitCache.Unlock()
		if !busy {
			go g.refresh(key, req.Cwd, args)
		}
		return nil, nil
	}
	gitCache.Lock()
	gitCache.entries[key] = gitCacheEntry{at: time.Now(), names: names}
	gitCache.Unlock()
	return sortedPrefix(names, req.Partial, req.Limit), nil
}

func (g gitGenerator) run(ctx context.Context, cwd string, args []string) ([]string, error) {
	argv := append([]string{"-C", cwd, "--no-pager"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	// Without a WaitDelay, a git that spawned something holding the pipe keeps
	// Output() blocked long after the context expired.
	cmd.WaitDelay = 200 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, 32)
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// refresh fills the cache off the request path. The context is deliberately
// detached from the caller's: the caller already gave up, and the whole point is
// that the answer exists before the next keystroke.
func (g gitGenerator) refresh(key, cwd string, args []string) {
	gitCache.Lock()
	if gitCache.inflight[key] {
		gitCache.Unlock()
		return
	}
	gitCache.inflight[key] = true
	gitCache.Unlock()

	defer func() {
		gitCache.Lock()
		delete(gitCache.inflight, key)
		gitCache.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), gitRefreshTimeout)
	defer cancel()
	names, err := g.run(ctx, cwd, args)
	if err != nil {
		return
	}
	gitCache.Lock()
	gitCache.entries[key] = gitCacheEntry{at: time.Now(), names: names}
	gitCache.Unlock()
}
