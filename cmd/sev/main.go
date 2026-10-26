// Command sev is a terminal front end for the engine: type a line, see the
// ranked completions. It exists so the library can be exercised without wiring a
// real shell.
//
//	go run ./cmd/sev                    # interactive
//	go run ./cmd/sev -query "git ch"    # one shot
//	go run ./cmd/sev -history ~/h.jsonl # with history
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pulseaiclub/sev"
)

func main() {
	query := flag.String("query", "", "answer one line and exit")
	historyPath := flag.String("history", "", "JSONL history to load (phi shellhist format)")
	cwd := flag.String("cwd", "", "working directory for ranking and path completion")
	limit := flag.Int("limit", 10, "how many completions to show")
	flag.Parse()

	dir := *cwd
	if dir == "" {
		dir, _ = os.Getwd()
	}

	start := time.Now()
	e, err := sev.New(sev.Options{Limit: *limit})
	if err != nil {
		fail("load: %v", err)
	}
	loadTime := time.Since(start)

	if *historyPath != "" {
		f, err := os.Open(*historyPath)
		if err != nil {
			fail("history: %v", err)
		}
		n, err := e.History().LoadJSONL(f)
		_ = f.Close()
		if err != nil {
			fail("history: %v", err)
		}
		fmt.Fprintf(os.Stderr, "loaded %d history entries\n", n)
	}
	fmt.Fprintf(os.Stderr, "spec tree: %d commands, loaded in %v\n", e.Size(), loadTime)

	if *query != "" {
		show(e, *query, dir)
		return
	}

	fmt.Fprintln(os.Stderr, "type a command line, empty line to quit")
	sc := bufio.NewScanner(os.Stdin)
	var prev string
	for {
		fmt.Fprint(os.Stderr, "> ")
		if !sc.Scan() {
			return
		}
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			return
		}
		showWithPrev(e, line, dir, prev)
		prev = line
	}
}

func show(e *sev.Engine, line, dir string) {
	showWithPrev(e, line, dir, "")
}

func showWithPrev(e *sev.Engine, line, dir, prev string) {
	start := time.Now()
	items, err := e.Complete(context.Background(), sev.Request{Line: line, Cwd: dir, Prev: prev})
	took := time.Since(start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "complete: %v\n", err)
		return
	}
	if len(items) == 0 {
		fmt.Printf("  (nothing)  %v\n", took)
		return
	}
	fmt.Printf("  %d items in %v\n", len(items), took)
	for i, it := range items {
		fmt.Printf("  %d. %-40s %-9s %.2f %s\n", i+1, it.Text, it.Kind, it.Score, it.Desc)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sev: "+format+"\n", args...)
	os.Exit(1)
}
