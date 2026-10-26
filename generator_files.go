package sev

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filesGenerator completes paths inside the token being typed. It is pure
// syscalls — no process — which keeps argument completion in the tens of
// microseconds instead of the milliseconds an exec costs.
type filesGenerator struct {
	// dirsOnly serves commands whose arguments must be directories (cd, pushd),
	// where offering a file is offering a suggestion that cannot run.
	dirsOnly bool
}

// maxDirEntries bounds how much of one directory a keystroke may look at. A
// directory larger than this is a place where a picker is useless anyway.
const maxDirEntries = 2000

// dirBatch is how many entries are read between context checks.
const dirBatch = 256

func (g filesGenerator) Name() string {
	if g.dirsOnly {
		return "dirs"
	}
	return "files"
}

func (g filesGenerator) Complete(ctx context.Context, req GenRequest) ([]string, error) {
	partial := req.Partial
	dir, base := filepath.Split(partial)

	lookup := dir
	if lookup == "" {
		lookup = "."
	}
	if strings.HasPrefix(lookup, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil
		}
		lookup = filepath.Join(home, strings.TrimPrefix(lookup, "~"))
	}
	if !filepath.IsAbs(lookup) {
		lookup = filepath.Join(req.Cwd, lookup)
	}

	f, err := os.Open(lookup)
	if err != nil {
		return nil, nil // a directory we cannot read is a directory with no matches
	}
	defer f.Close()

	showHidden := strings.HasPrefix(base, ".")
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	out := make([]string, 0, limit)
	// Read in batches and check the context between them: os.ReadDir stats and
	// sorts the whole directory before it returns, which on a 100k-entry
	// directory is tens of milliseconds the caller never agreed to wait for.
	buf := make([]os.DirEntry, 0, dirBatch)
	seen := 0
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		buf, err = f.ReadDir(dirBatch)
		if err != nil && len(buf) == 0 {
			break
		}
		for _, e := range buf {
			seen++
			name := e.Name()
			if !showHidden && strings.HasPrefix(name, ".") {
				continue
			}
			if base != "" && !strings.HasPrefix(name, base) {
				continue
			}
			if g.dirsOnly && !e.IsDir() {
				continue
			}
			text := dir + name
			if e.IsDir() {
				text += "/"
			}
			out = append(out, text)
			if len(out) == limit || seen >= maxDirEntries {
				break
			}
		}
		if len(out) == limit || seen >= maxDirEntries || err != nil {
			break
		}
	}
	// Directory order is whatever the filesystem returns; a picker that reorders
	// itself between keystrokes reads as a bug.
	sort.Strings(out)
	return out, nil
}
