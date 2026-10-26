package sev_test

import (
	"context"
	"fmt"
	"log"

	"github.com/pulseaiclub/sev"
)

// ExampleEngine_Complete is the whole integration: build an engine, tell it what
// the shell ran, ask it what to offer. The history line wins the tie against the
// bare command because the command it starts with was just used in this
// directory — that is frecency, and it is why the two rows score the same.
//
// Argument positions ("cat ", "cd ", "git checkout ") work the same way; their
// answers come from a generator instead of the command tree, and the engine
// splices them into the line the caller typed.
func ExampleEngine_Complete() {
	e, err := sev.New(sev.Options{Limit: 3})
	if err != nil {
		log.Fatal(err)
	}

	// History is in-memory; a host records as its shell executes and persists
	// the events wherever it already keeps state.
	e.Record(sev.Event{Cmd: "git commit -m wip", Cwd: "/repo"})

	items, err := e.Complete(context.Background(), sev.Request{
		Line: "git c",
		Cwd:  "/repo",
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, it := range items {
		fmt.Printf("%-20s %-9s %.2f\n", it.Text, it.Kind, it.Score)
	}
	// Output:
	// git commit           spec      1.03
	// git commit -m wip    history   1.03
	// git checkout         spec      0.85
}
