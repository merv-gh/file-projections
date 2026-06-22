# file-projections
![tests](https://github.com/merv-gh/file-projections/actions/workflows/test.yml/badge.svg)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Stable, (optionally two-way) **projection files** that combine code concepts across
many source files into a single read/edit surface.

A folder tree only expresses **one** nesting of a codebase. Real questions cut across
files: *"what are the entrypoints of this service?"*, *"what are all the ways execution
reaches this line?"*, *"which lines actually shape this object before it's saved?"*. A
projection is a generated file that answers exactly one such question, pulling the
relevant slices out of however many source files are involved — so you (or an agent)
read one focused file instead of scanning ten, at a fraction of the tokens.

Single binary (`main.go`, stdlib-only), single config (`config.json`). External engines
(`rg`, `ast-grep`, `joern`) are used when present and fall back to a Docker image or a
built-in scanner when not.

## Quick start

```sh
make build                 # -> bin/file-projections
make run                   # generate every lens in config.json into .projections/
make menu                  # interactively add a view (control-flow, data-flow, ...)
make watch                 # regenerate on change + sync two-way edits back to source
make test                  # full test suite
make cross                 # mac (amd64/arm64) + linux + windows binaries
```

## Lenses (analyzers)

Each lens is one entry in `config.json` with an `analyzer` and `params`. Lenses are
**view-only** (regenerated, never written back) unless noted.

| analyzer | answers | engine |
|----------|---------|--------|
| `entrypoints` | where does control enter? (`@KafkaListener`, `@Scheduled`, `@*Mapping`, `@EventListener`) | rg → stdlib |
| `exitpoints` | where does control leave? (`*kafka*.send`, `*repository*.save`, any glob) | rg → stdlib |
| `control-flow` | all ways from a method entry to a target line — **one file per branch**; `mode: joern` handles else-if/switch/loops | stdlib CFG / joern |
| `data-flow` | only the lines that shape a variable, annotated as **trailing comments** | fallback slicer / joern |
| `entry-to-exit` | all control flows from entrypoints to exitpoints over the call graph (all-to-all or 1-to-1) | joern |
| `bookmark` | a verbatim source span — **two-way**: edits sync back to source; supports single-line drop-ins | — |
| `flow` | generic "annotated entry reaches a sink" (config regexes); `java-post-to-save` is an alias | stdlib Java |
| `joern-var-flow` | interprocedural var data-flow (CPG) with Java fallback | joern → stdlib |
| `ast-grep` | structural pattern matches | ast-grep → docker |
| `go-symbols` / `js-events` / `jsonl` | Go symbol map / JS event surface / generic tool adapter | stdlib |

### entrypoints / exitpoints

```json
{ "name": "svc-entrypoints", "analyzer": "entrypoints", "source_root": "src/main/java" }
{ "name": "svc-exitpoints", "analyzer": "exitpoints", "source_root": "src/main/java",
  "params": { "sinks": "*kafka*.send,*repository*.save" } }
```

The tool ships with **no** built-in patterns — they are project-specific and live entirely
in config:

- `entrypoints` requires `params.patterns` as `label=regex;label=regex`
  (e.g. `kafka-listener=@KafkaListener;http-mapping=@(Get|Post)Mapping`).
- `exitpoints` requires `params.sinks`, a comma list of glob-ish patterns (`*` = identifier/dot
  run, `.` literal). Matching is **case-insensitive** (real beans are camelCase).

Each emits one sorted map block (`file:line :: label :: code`) plus per-label counts.

### control-flow — "ways from entry to a line", branch per file

```json
{ "name": "checkout-paths", "analyzer": "control-flow",
  "source_root": "src/main/java",
  "params": { "file": "com/example/shop/OrderController.java", "line": "35", "max_branches": "16" } }
```

The lens finds the enclosing method and enumerates every distinct path from its entry to
the target line. Each `if`/`else` fork that both sides can pass through doubles the
branch count; an early-return guard is forced to its non-exiting side. It writes:

- the main file: a **branch index** (`branch k: guardA=true & guardB=false -> …branch-k.projection`)
- one `…branch-k.projection` per path: the straight-line slice with `// guard:` decisions
  and the target line marked `// <== target`.

Two engines:

- **default (lexical)** — stdlib, no setup; models `if`/`else` + nesting + early-return
  guards. No `else-if` chains, `switch`, or loops.
- **`params.mode: "joern"`** — real CPG via Joern. Handles **else-if chains, switch/case,
  and loops** (acyclic CFG path enumeration), with `entered`/`true`/`false` branch labels
  per guard. Needs the Joern binary or `tools.joern.image` + Docker. See *Joern* below.

### data-flow — contributing lines with trailing comments

```json
{ "name": "order-data-flow", "analyzer": "data-flow",
  "source_root": "src/main/java",
  "params": { "file": "…/OrderController.java", "line": "35", "var": "order", "mode": "fallback" } }
```

Emits only the lines that shape the variable, each with a right-padded trailing comment
(`order.setShipping("express");          // <- mutates order`) so the code stays scannable.
Set `mode: joern` to use a Joern CPG slice instead of the static fallback.

### bookmark — two-way sync

```json
{ "name": "ctor", "analyzer": "bookmark", "source_root": "src/main/java",
  "params": { "file": "…/OrderController.java", "lines": "16-18" } }
```

Pulls the span verbatim into a block anchored with `sync=two-way src=…:a-b srchash=…`.
(`extract` is kept as a back-compat alias.) Under `make watch`, the menu's watch toggle, or
a programmatic `SyncProjection`:

- edit the **source** → projection refreshes,
- edit the **projection block** → the span is written back to source,
- edit **both** → reported as a conflict, neither side clobbered.

A single round-trip is idempotent: no extra lines or leftover markers.

**Single-line drop-ins.** Create a new `.projection` file whose only content is a source
reference and let the tool expand it into a full two-way bookmark:

```sh
echo 'com/example/demo/UserEventConsumer.java:17' > .projections/consumer.projection
./bin/file-projections bookmarks       # or it happens automatically under `watch`
```

The path may be repo-relative or package-relative (resolved across source roots); use
`:a-b` for a range. Expansion is idempotent — an already-rendered file is left alone.

## Projection format

```
# generated by file-projections
# lens: <name>
# analyzer: <analyzer>
# sync: view-only | two-way
# source-hash: <hash of body>           # stable across regen if content unchanged
# generated-at: <RFC3339>               # volatile; ignored for idempotency

@@ <file>#<id> [<tool>.<mode> hash=<h>]                 # view-only block
@@ <file>#<id> [<tool>.<mode> hash=<h> sync=two-way src=<f>:a-b srchash=<h>]  # bookmark
<lines…>
@@
=> <id>: <fact>
```

## config.json reference

```jsonc
{
  "root": ".",                      // repo root
  "projections_dir": ".projections",
  "exclude_dirs": ["target", "build", "node_modules", ...],
  "tools": {                        // optional external-engine config
    "joern": { "image": "ghcr.io/joernio/joern:latest", "jvm_args": "-Xmx6g" },
    "rg":    { "image": "" }         // image used only when the binary is absent
  },
  "lenses": [ { "name", "out", "analyzer", "source_root", "include", "input", "params" } ]
}
```

## Joern (real CPG)

The `joern-var-flow` lens and `control-flow`/`data-flow` `mode: "joern"` run against a real
Code Property Graph — the only way to handle interprocedural data-flow and
else-if/switch/loop control-flow faithfully.

```sh
make cpg CONFIG=joern.config.json     # build/refresh cached cpg.bin per source root (fast)
make run CONFIG=joern.config.json     # run the CPG lenses (uses the cache if present)
```

- Joern runs from a local binary if present, else the `tools.joern.image` Docker image
  (`ghcr.io/joernio/joern:nightly` in `joern.config.json`); `jvm_args` (e.g. `-Xmx6g`) is
  forwarded via `_JAVA_OPTIONS`.
- `build`/`refresh` parses each source root once into `<projections_dir>/.cpg/<hash>.bin` via
  `joern-parse`. Lenses then `importCpg` that cache instead of re-importing source — the basis
  for incremental refresh (re-run `build` for a root after its files change).
- Scripts live in `tools/joern/` (`java-var-flow.sc`, `control-flow.sc`) and emit the same
  JSONL the renderer consumes — JSON is hand-built so they run on stock Joern images.
- `control-flow mode=joern` writes the same branch-per-file output as the lexical lens.

### Incremental CPG model

`build`/`refresh` keeps a per-file content **manifest** beside each `cpg.bin`. A refresh
skips roots with no changes and, when something did change, reports the exact added /
modified / removed files before rebuilding that root. `watch` triggers this automatically.

The correct incremental **unit is the source root**, not the single file — and that is a
property of code-property graphs, not a tooling shortcut. A CPG's value is its *cross-file*
edges (call graph, type hierarchy, data-flow), which are resolved whole-program. Re-parsing
one file in isolation can refresh that file's own nodes (the embedded `cpg-remove-file.sc`
demonstrates on-the-fly node removal via `DiffGraphBuilder`), but it cannot re-resolve edges
that *other* files form into the changed one without a whole-program relink pass. So for
correctness, a changed root is reparsed (fast: ~seconds for a typical service). To get
finer granularity on a large codebase, split it into multiple source roots/lenses — only the
changed root rebuilds. True surgical node-level re-add (frontend AST pass into the live CPG +
re-run of the linker overlays + persistence back to `cpg.bin`) is scoped as a Phase-3 item.

## External tools & Docker fallback

`runTool` prefers a local binary; if absent and `tools.<name>.image` is set (and Docker is
available), it runs `docker run --rm -v <root>:/src -w /src <image> <tool> …`, forwarding
`jvm_args` via `_JAVA_OPTIONS` for memory-hungry engines like Joern. `rg`/`exitpoints`
additionally fall back to a built-in regex scanner so lenses still work with no tools at
all.

## Verification

`make test` covers: entrypoints find Kafka/Scheduled/mappings; exitpoints find
`*repository*.save` + `*kafka*.send`; control-flow emits one file per path with correct
guards; data-flow renders trailing padded comments; bookmark round-trip is idempotent and
conflicts are detected; the menu persists lenses. Fixtures live under `fixtures/`; the real
`spring-petclinic-main` tree is used for non-synthetic entrypoint/exitpoint sanity.

## Releases

CI (`.github/workflows/test.yml`) runs `gofmt`/`vet`/`test`/`build` on every push and PR;
engine-backed tests self-skip on a stock runner. Pushing a `vX.Y.Z` tag triggers
`release.yml`, which cross-compiles the four binaries (+ `SHA256SUMS.txt`) and publishes a
GitHub Release using `RELEASE_NOTES.md`. The binary embeds its Joern scripts, so a single
downloaded executable is self-sufficient.

## Roadmap

**Done:** real Joern CPG via Docker (interprocedural data-flow + else-if/switch/loop
control-flow, entry→exit call-graph flows); cached + per-file-incremental `cpg.bin` with an
on-the-fly node-removal primitive; config-driven `flow`/entry/exit analyzers + formatter;
single-line drop-in bookmarks; embedded scripts; `ast-grep` analyzer; Ollama token eval.

**Next (Phase 3):** two-way sync for analytical views; analyzer merging across dependency
sources (libraries that bring their own entry/exitpoints); finer node-level CPG re-add.

## License

[MIT](LICENSE).
