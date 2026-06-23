---
name: file-projections
description: Generate cross-file "projection" views (entrypoints, exitpoints, control-flow paths, data-flow, two-way bookmarks) to read a codebase in one focused file instead of scanning many. Use for "where does this service enter/exit?", "all ways execution reaches this line?", "which lines shape this object?" on Java/Spring, Go, JS/TS.
---

# file-projections

One Go binary renders **projection files**: one file = one cross-file question, only the
relevant slices. Cheaper to read than raw files. Build: `make build` -> `bin/file-projections`.

## Lenses (set in config.json, or ad-hoc flags)

- `entrypoints` — `params.patterns` `label=regex;...` (e.g. `@KafkaListener`, `@*Mapping`).
- `exitpoints` — `params.sinks` glob csv (`*repository*.save,*kafka*.send`; case-insensitive).
- `control-flow` — `params.file,line` -> all paths entry→line, **one file per branch** + index.
  Add `params.mode=joern` for real CPG (else-if/switch/loops); `build` caches the cpg.bin first.
- `data-flow` — `params.file,line,var` -> contributing lines w/ trailing `// <-` comments.
- `unrolled-program` — `params.file,method,inputs` -> editable straight-line Java path; each
  projection line syncs back to its scattered source origin under `watch`.
- `entry-to-exit` — `params.entry,exit` regexes -> all call-graph flows entrypoints→exitpoints
  (all-to-all; narrow with `entry_name`/`exit_file`). joern.
- `bookmark` — `params.file,lines=a-b` -> verbatim span, **two-way** (edits sync to source).
  Drop-in: write `pkg/Foo.java:17` into a new .projection file -> `bookmarks` expands it.
- `flow` — `params.entry,sink` regexes -> annotated entry reaching a sink. `ast-grep` -> structural.

No domain patterns are built in — all project specifics live in config.

## Commands

```sh
./bin/file-projections -config config.json          # generate all lenses
./bin/file-projections menu  -config config.json    # add views interactively; option 7 = watch
./bin/file-projections watch -config config.json     # regenerate + sync on change

# ad-hoc (no config):
./bin/file-projections -analyzer control-flow -source-root SRC -file F.java -line N -out o.projection
./bin/file-projections -analyzer data-flow -mode fallback -source-root SRC -file F.java -line N -var V -out o.projection
./bin/file-projections -analyzer unrolled-program -source-root SRC -file F.java -method M -inputs a=1,b=x -out o.projection
./bin/file-projections -analyzer entrypoints -source-root SRC -out o.projection   # needs config patterns
```

## Reading

```
# sync: view-only | two-way
@@ <file>#<id> [<tool>.<mode> hash=<h>]      # two-way bookmarks add: sync=two-way src=f:a-b srchash=h
<code>                              <file>:<line>   # code first, file:line padded 2nd column
@@
=> <id>: <fact>                               # only where it adds signal (e.g. bookmark sync)
=> <id>: origin N src=f:line srchash=h        # scattered two-way analytical line
```

- entrypoints/exitpoints: matched code first, `file:line` second; no regexp label or counts.
- control-flow: read the index, then the `…branch-k.projection` you care about — each is
  entry signature → active conditions (negated `!(…)` on the not-taken branch) → exitpoint.
- entry-to-exit: entrypoint signature → exitpoint side-effect, code first.
- data-flow: only contributing lines, `// <-` notes; non-contributing omitted.
- unrolled-program: read/edit the straight-line path; origin metadata maps each line back.
- bookmark: edit the block; `watch`/SyncProjection writes it back (conflicts detected, not clobbered).

## Writing tests (two-way spike)

Two-way bookmarks author code back into any file, including `*_test.go` — use them to write
tests through a projection instead of editing the test file directly:

1. Put a sentinel line at the tail of the test file, e.g. `// add tests below`.
2. `bookmark` that one line: `params.file=foo_test.go, lines=N-N` (or drop in `foo_test.go:N`).
3. Edit the projection block — keep the sentinel, append `func TestX(t *testing.T){…}` after it.
4. `watch` / SyncProjection writes the grown block back; the test file gains the test
   (existing tests preserved; both-sides-edited is reported as a conflict, not clobbered).

The block may grow to any length, so one bookmark drafts many tests. Token-cheap context for
the model: pair this with a `go-symbols` index + a `control-flow`/bookmark slice of the
function under test (see the dogfood lenses in config.json).

## Notes

- Engines `rg`/`ast-grep`/`joern` used if installed, else Docker image (`tools.<name>.image`) or
  built-in scanner. Nothing else required.
- Slow/low-RAM machine? Set `tools.joern.farm` to a joern-farm URL to offload CPG build + queries
  entirely (no local Joern); `build` downloads the cpg.bin back.
- control-flow is lexical (if/else+nesting+early-return guards); no else-if/switch/loops yet.
- Token tip: pick the smallest lens that answers the question.
