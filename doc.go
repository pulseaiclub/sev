// Package sev is a fast shell-command completion engine: given the line a
// user is typing, it returns ranked completions. Its built-in memory sources
// finish in microseconds, so a caller can render a picker on every keystroke.
//
// The engine has three independent sources and merges them under one score:
//
//   - spec: a compiled command tree (names, descriptions, flags) shipped in the
//     binary. Pure memory: no process, no IO, tens of nanoseconds per token.
//   - history: commands the user actually ran, with frecency (frequency ×
//     recency, scoped to the working directory) and markov transitions
//     (what usually follows the previous command). Callers feed it with
//     Record; it never touches the disk itself.
//   - generator: dynamic arguments (paths, git branches, …). These built-in
//     sources may block and run under Options.GeneratorBudget.
//
// Options.CompletionHook optionally handles a request before the built-in
// sources. It receives a normalized Request.Limit. Returning handled=false with
// no error falls back to normal completion; handled=true returns only the hook's
// items, even when empty, truncated without reranking. Hook errors propagate.
// Hooks run synchronously outside GeneratorBudget under the caller's context.
// Cancellation is checked before and after the callback; the callback must honor
// the context and support concurrent calls. The host owns its timeout.
//
// No AI or network requests are made by default. The optional commitmsg package
// implements CompletionHook for Git commit messages with a cancellable 300 ms
// debounce. It uses Request.Cwd per call and delegates generation to a host
// callback; model clients, credentials, project rules, and network policy belong
// to the host. A caller deadline includes debounce, Git reads, and generation.
//
// Typical cost on a warm engine (see bench_test.go for the measured numbers):
//
//	spec lookup, single token          ~ 350 ns
//	spec + history, whole line         ~ 1-2 µs
//	history shortlist, three letters   ~ 140 µs (20k entries)
//	path generator (200 entries)       ~ 15 µs
//	git branch generator (exec)        ~ 2-8 ms
//
// The zero of that range is the point: on the memory path nothing spawns a
// process, nothing waits on IO, every cost is bounded by the arity of one node
// or a fixed shortlist rather than by the size of the user's data, and the only
// lock is a reader lock held across arithmetic. A generator is expected to honour
// its context; the engine cannot preempt one that does not.
package sev
