# sev

A shell-command completion engine in Go. Give it the line a user is typing, get
back ranked completions — in under a microsecond for everything that is memory,
and inside a 20 ms budget for everything that is not.

Built to be embedded in a TUI host (it is the completion core for
[phi](../phi)), not to wrap a shell. There is no PTY, no raw mode, no overlay, no
daemon, no config file, and no new dependency: `go.mod` requires nothing.

```
$ go run ./cmd/sev -query "git ch"
  2 items in 31µs
  1. git checkout                             spec      0.85 switch branches
  2. git cherry-pick                          spec      0.85 apply commit
```

## Numbers

Measured on an M-series laptop, warm engine, `go test -bench .` (see
`bench_test.go`; the assertions behind the last row are in `TestLatencyBudget`).
Nanosecond figures move ±20% between runs; the allocation counts do not.

| Path | Cost | Allocs |
|---|---|---|
| interned name view (`ix.name`) | **2.9 ns** | 0 |
| binary search over one node's children | 31 ns | 0 |
| `Complete("git log --")` — flags | 440 ns | 8 |
| `Complete("git ch")` — subcommand + fuzzy fallback | 410 ns | 9 |
| `Complete("gi")` — top-level prefix | 720 ns | 12 |
| `Complete("git ")` — 100 children, ranked | 1.9 µs | 18 |
| `Complete("")` — bare prompt | 2.1 µs | 14 |
| `Complete("git st")` with history + transitions | 1.0 µs | 14 |
| `Complete("cat ")` — path generator, 200-entry directory | 15 µs | 80 |
| git branch generator, cached | 200 ns | 2 |
| git branch generator, cold exec | 2–8 ms | — |
| `Prefix` over a 20k history, three-letter query | 140 µs | 5 |
| engine load (embedded tree decoded) | 4.3 ms | 582 |

End-to-end over 500 mixed keystrokes: **p50 1.7 µs, p99 62 µs, max 233 µs.**

The floor and the ceiling are both real: 2.9 ns is the cost of reading a command
name out of the embedded blob, and 20 ms is the budget a generator gets before
its rows are dropped. In between, every cost is bounded by something other than
the size of the user's data: spec rows by the arity of one node, history rows by
a 512-entry shortlist, directory reads by 256-entry batches with a context check
between them, and git answers by a process with a context and a `WaitDelay`.
Nothing on the memory path allocates a snapshot, and nothing on the dynamic path
runs without a deadline.

## Use

Requires Go 1.25 or newer. Once the repository is public, install the latest
released version with:

```sh
go get github.com/pulseaiclub/sev@latest
```

```go
e, err := sev.New(sev.Options{Limit: 10})
if err != nil {
	return err
}

// Feed it what the shell runs. Persist it however you like; the engine keeps no
// files of its own.
e.Record(sev.Event{Cmd: "git checkout main", Cwd: "/repo", Prev: "git status"})

items, err := e.Complete(ctx, sev.Request{
	Line: "git ch",
	Cwd:  "/repo",
	Prev: "git status", // previous command, for transition scoring
})
for _, it := range items {
	fmt.Println(it.Text, it.Kind, it.Score) // "git checkout ", spec, 0.85
}
```

`Item.Text` is the **whole line** to put back in the prompt, so a host can
replace the typed command with it without knowing where the completion starts.
That is also how a `!`-style bash mode integrates: map `Text` to the command to
insert, `Score` to the row weight.

Loading an existing history (phi's `history.jsonl` format, one JSON object per
line) is one call:

```go
n, err := e.History().LoadJSONL(f)
```

## How it works

Three sources, merged under one score.

**spec** — a compiled command tree: 573 commands, 6,515 nodes (names,
subcommands, flags, descriptions), shipped gzipped in the binary at 358 KiB and
decoded once into flat tables. Names are length-prefixed in one read-only blob,
so reading a command name is an `unsafe.String` slice — no copy, no allocation.
Children are sorted and contiguous, so both the exact lookup and the prefix range
are binary searches. Nothing here allocates until the ten surviving rows are
materialised.

**history** — in-memory only, fed by `Record`. Three signals: prefix matching over
whole lines (sorted key slice, range search), frecency
(`(0.6·ln(1+cwd count) + 0.4·ln(1+total)) · e^(-age/14d)`), and a markov table of
what followed the previous command. Reads take a shared lock and allocate no
snapshot.

**generator** — dynamic arguments: paths, directories, git branches/tags/remotes/
stashes. These are the only sources allowed to block, so they run under
`Options.GeneratorBudget` (default 20 ms). A generator is called synchronously,
so it owns its own preemption: the bundled ones check the context between
batches and pass it to the process. Then:
- a cached answer is always served immediately (stale-while-revalidate);
- a failure is cached too, so a directory that is not a git repo costs one exec,
  not one per keystroke;
- a cold miss that times out returns nothing and refreshes in the background, so
  the next keystroke has it.

Ranking is a weighted sum, match quality first:

```
score = 1.00·match + 0.45·frecency + 0.30·transition
match = exact 1.0 | prefix 0.85 | word-prefix 0.65 | subsequence ≤ 0.5
```

Fuzzy matching only runs when prefix matching finds nothing, and is capped below
0.5 so a typo can never outrank a real prefix. With nothing typed there is no
match quality to weigh, so ranking reduces to "commands you actually run, then
the spec's own order" — the history shortlist takes the top rows and the tree
fills in behind it.

A flag or an argument ends the command path but not the command: `git log
--oneline --` still completes `git log`'s flags, because the node the user
reached owns the rest of the line. A token the user quoted is completed back
into a quote, so a value with a space in it stays one argument.

## Regenerating the command data

`specdb.bin.gz` is a build artifact compiled from a corpus that lives in this
repository: `tools/genspecs/corpus/commands.json.gz`, a 268 KiB JSON tree of
commands, flags, descriptions, and generator keys. `tools/genspecs` turns that
into the binary tree:

```
cd tools/genspecs && go run . -out ../../specdb.bin.gz
```

The tool is standard library only and reads nothing outside the repository, so a
checkout with no network and no module cache can rebuild the data. Its output is
deterministic: compiling the committed corpus reproduces the committed
`specdb.bin.gz` byte for byte, which is also the test that the corpus is complete.

Editing the corpus is the supported way to add commands: change the JSON, rebuild,
commit both files. The format the tool emits is documented at the top of
`format.go` and validated on load (`TestLoadIndexTruncated`,
`TestLoadIndexCorrupt`), so a different producer can target it too.

The corpus was compiled once, from a 0BSD-licensed command-spec corpus; it is data,
and the engine has no runtime or build-time relationship with that project.

## What it does not do

Deliberately out of scope, because a TUI host already owns them: PTY wrapping and
inline overlays, keybinding parsing, themes, config files, self-update, shell
startup scripts, AI suggestions (a host with an LLM routes that itself), and
persistence (history lives in the host's store, which is one interface away).

## Releasing

Git tags publish this Go module; GitHub Releases provide release notes. CI tests
Go 1.25 and the latest stable Go version, runs the race detector and vet, checks
formatting, builds the example CLI, and verifies that Go 1.25 reproduces the
committed command data. Timing assertions are skipped on shared CI runners.

To release a tested commit on `main`, push it and a version tag:

```sh
git push origin main
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

The release workflow runs CI against the tagged commit before creating a GitHub
Release with generated notes. Tags such as `v0.1.0-rc.1` create prereleases.
It uses the built-in `GITHUB_TOKEN`; no publishing secret is needed. Re-running
it leaves an existing release intact. Release tags should never be moved or
reused: Go module proxies cache published versions.

Consumers pin a release with `go get github.com/pulseaiclub/sev@v0.1.0`. The
example CLI can be installed with `go install github.com/pulseaiclub/sev/cmd/sev@v0.1.0`.
Go downloads the module source, including the embedded spec database, so this
library does not need binary release assets or a separate package registry.
While the repository is private, consumers still need GitHub access; public
module proxy access becomes available after the repository is made public.
A future v2 release requires changing the module path to end in `/v2` first.

## Test

```
go test ./...            # unit + budget assertions
go test -bench . ./      # the numbers above
go vet ./... && gofmt -l .
```

## License

[MIT](LICENSE). Copyright (c) 2026 pulseaiclub.
