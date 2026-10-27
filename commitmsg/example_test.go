package commitmsg_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/pulseaiclub/sev"
	"github.com/pulseaiclub/sev/commitmsg"
)

func ExampleNew() {
	generate := func(ctx context.Context, system, user string) (string, error) {
		// Replace this stub with the host's model call using ctx, system, and
		// user. The host supplies credentials, model selection, and project
		// instructions. The callback must honor ctx and allow concurrent calls.
		return "fix: preserve the typed commit prefix", ctx.Err()
	}
	completer := commitmsg.New(generate)
	engine, err := sev.New(sev.Options{CompletionHook: completer.Complete})
	if err != nil {
		log.Fatal(err)
	}

	// The deadline covers the 300 ms debounce, Git reads, and generation.
	// Cancel this context when the user types another character.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	items, err := engine.Complete(ctx, sev.Request{
		Line: "git commit -m 'fix: ",
		Cwd:  ".", // the working tree with staged changes
	})
	if err != nil {
		log.Print(err)
		return
	}
	for _, item := range items {
		fmt.Println(item.Text, item.Kind, item.Score)
	}
}
