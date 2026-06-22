# file-projections v0.1.0

Stable, optionally two-way **projection files** that combine code concepts across many
source files into one focused read/edit surface — for humans and agents.

## Lenses
- **entrypoints / exitpoints** — rg-driven maps (annotations in, sink-call globs out),
  fully config-driven, with a built-in stdlib fallback when rg is absent.
- **control-flow** — every path from a method's entry to a target line, **one file per
  branch**. Lexical by default; `mode: joern` uses a real CPG and handles **else-if
  chains, switch, and loops**.
- **data-flow** — only the lines that shape a variable, as trailing padded comments
  (`mode: joern` for interprocedural CPG slices).
- **entry-to-exit** — all control flows from entrypoints to exitpoints over the call
  graph (all-to-all by default, narrowable to 1-to-1).
- **flow** — generic "annotated entry reaches a sink" (config regexes; replaces the old
  Spring-specific lens).
- **bookmark** — a verbatim two-way source span; edits sync back to source. Includes
  **single-line drop-ins**: paste `pkg/Foo.java:17` into a new `.projection` file and it
  expands into a full two-way bookmark.
- **ast-grep**, **go-symbols**, **js-events**, **jsonl**.

## Joern (real CPG)
- Runs from a local binary or a Docker image (`tools.joern.image`); memory tuned via
  `jvm_args`.
- `build` / `refresh` caches a `cpg.bin` per source root with **per-file change detection**
  (skips unchanged roots, reports exactly what changed). Includes an on-the-fly per-node
  CPG removal primitive.
- Joern scripts are **embedded in the binary** — the single executable is self-sufficient.

## Workflow
- `menu` (interactive view builder, with a background watch toggle), `watch`
  (regenerate + two-way sync + drop-in expansion + incremental CPG refresh).
- `tools/ollama-eval.sh` measures the token cost/benefit of the skill (with vs without).

## Install
Download the binary for your platform from the assets below, or `make build` /
`make cross`. Stdlib-only, no Go dependencies.
