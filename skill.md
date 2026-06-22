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
./bin/file-projections -analyzer entrypoints -source-root SRC -out o.projection   # needs config patterns
```

## Reading

```
# sync: view-only | two-way
@@ <file>#<id> [<tool>.<mode> hash=<h>]      # two-way bookmarks add: sync=two-way src=f:a-b srchash=h
<slice>
@@
=> <id>: <fact>                               # guards, sinks, contributors, counts
```

- control-flow: read the index, then the `…branch-k.projection` you care about.
- data-flow: only contributing lines, `// <-` notes; non-contributing omitted.
- bookmark: edit the block; `watch`/SyncProjection writes it back (conflicts detected, not clobbered).

## Notes

- Engines `rg`/`ast-grep`/`joern` used if installed, else Docker image (`tools.<name>.image`) or
  built-in scanner. Nothing else required.
- control-flow is lexical (if/else+nesting+early-return guards); no else-if/switch/loops yet.
- Token tip: pick the smallest lens that answers the question.
