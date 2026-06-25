# Architecture (`src/`)

`main.go` was one 7k-line file; it's now split by concern into `src/` (still one
`package main`). Build the binary with:

```bash
go build -o file-projections ./src
go test ./src        # 30 tests; TestMain hops to the repo root for fixtures
```

## The spine (language-agnostic)

Every projection flows through one generic pipeline — no language knowledge lives
here:

```
Run ─▶ ExecuteLens ─▶ Analyzer.Analyze ─▶ RenderProjection ─▶ (SyncProjection)
 registry.go          registry.go          per-frontend         projection.go
```

- **`registry.go`** — `Run`, `ExecuteLens`, `DefaultRegistry`. An **Analyzer** is
  just `Analyze(Config, LensConfig) (Projection, error)`. Adding a capability =
  one entry in `DefaultRegistry`. This is the whole extension surface.
- **`projection.go`** — render a `Projection` to the on-disk `.projection` text
  and sync edits back to scattered source (two-way blocks).
- **`types.go`** — `Config`, `LensConfig`, `Projection`, embeds, globals.
- **`config.go`** — load config, scan a project, detect the dominant language.
- **`util.go`** — small shared helpers (fs, hashing, identifiers, ripgrep).

## File map

| File | Responsibility |
|------|----------------|
| `cli.go` | flag parsing + subcommand dispatch (`ui`, `build`, `sync`, `menu`, …) |
| `registry.go` | core pipeline + analyzer registry |
| `projection.go` | projection render + two-way sync |
| `types.go` | core types, embedded `ui.html`/`VERSION`/joern scripts |
| `config.go` | config load, project scan, language detection |
| `analyzers_go.go` | **Go** frontend: symbols, call graph, unrolled-program (+guards) |
| `analyzers_java.go` | **Java** frontend: control/data/object flow, cpg-methods, unroll, flow, entry/exitpoints |
| `analyzers_web.go` | **JS/TS** frontend: event surface, jsonl |
| `analyzers_misc.go` | language-agnostic: bookmark/extract, ast-grep |
| `assumptions.go` | shared unroll/assumption helpers (guards, inlining, line views) |
| `servicegraph.go` | cross-service graph: TS imports + Go routes + TS→Go seam |
| `joern.go` | CPG build/parse/query (Joern, local + farm) |
| `ui.go` | the `ui` web studio (HTTP API + embedded SPA) |
| `menu.go` | interactive menu, setup wizard, watch |

## Where the abstraction is uneven (honest gaps)

The spine is language-agnostic; the **frontends are not symmetric**. Coverage
today, by analyzer × language:

| capability | Java | Go | JS/TS |
|------------|:----:|:--:|:-----:|
| symbols / structure | cpg-methods | go-symbols | js-events (events only) |
| control / data / object flow | ✅ (joern) | — | — |
| **unrolled-program** (flatten cross-file) | ✅ | ✅ | ❌ **none yet** |
| **per-line assumptions** (guard sets) | ✅ | ✅ (indentation-based) | ❌ (needs unroll) |
| **object timeline / loop bands** | ✅ (UI-lexical) | ✅ (UI-lexical) | ❌ (needs unroll) |
| service-graph node | imports n/a | routes + handlers | imports + seam source |

**The biggest unevenness: there is no TS `unrolled-program` frontend.** So in the
cross-service graph, clicking a Go entrypoint drills into a full
assumptions+timeline view, but clicking a TS node only opens the file. A
`analyzers_web.go` TS unroller (parsing TS into the same `unrollLine` stream the
UI already renders) would make timeline + assumptions symmetric across all three
languages. The UI tagging (`tagFlow`) and the assumption/guard plumbing are
already language-neutral — only the per-language *unroller* is missing for TS.

Secondary notes:
- `analyzers_java.go` is the heaviest file because Java has the most frontends
  (it predates the others); it's not doing anything the spine couldn't host for
  other languages.
- `assumptions.go` holds the cross-language guard/inline helpers; the Java and Go
  unrollers each re-implement the walk (Java recursive, Go indentation-based) —
  a shared unroll core would let a third language reuse it.

## Adding a frontend (the "small bro" path)

1. Write `AnalyzeXxx(cfg Config, lens LensConfig) (Projection, error)` in the
   matching `analyzers_*.go`.
2. Register it in `DefaultRegistry` (`registry.go`) and, if UI-facing, add it to
   `uiAnalyzerLanguages()` (`ui.go`).
3. Emit `ProjectionBlock`s (+ optional `LineGuards` for assumptions). Everything
   downstream — render, sync, UI, mermaid — is already generic.
