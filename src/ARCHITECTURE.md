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

## Three decoupled registries (lenses ⟂ languages ⟂ engines)

The design keeps three concerns orthogonal so they can grow independently — you can
add a lens without touching a language, add a language without touching a lens, and
swap the engine that answers a language without touching either:

1. **Analyzers / lenses** (`registry.go`, `analyzer*.go`) — *what question* a
   projection answers. Language-agnostic spine; an analyzer asks the language
   registry, it never `switch`es on a language id itself.
2. **Languages** (`language.go`) — *what a source language is*: extensions, the
   Joern frontend binary, wizard/menu default lens params, and a `SymbolScanner`.
   This is the single home for every former scattered `switch lang { java|go|js }`
   (the wizard, menu, joern frontend, entry/exit defaults, dominant-language
   detection all route through it). Adding a language is one `Language` entry.
3. **Engines / scanners** (`symbols.go`, `joern.go`) — *how* a fact is extracted.
   The default `SymbolScanner`s are regex-based and dependency-free; a tree-sitter
   (or LSP) backend would implement `SymbolScanner` per language and nothing else
   changes — `symbolIndexFor`, the UI, and every analyzer consume the neutral
   `Symbol` type regardless of who produced it.

The **symbol index** (`symbols.go`) is the one place source is walked for symbols.
It caches per source root with a per-file content hash and only re-scans changed
files — the same incremental unit as the CPG manifest, so a large repo isn't
re-walked on every UI keystroke, and a tree-sitter backend inherits the caching
for free.

The **analyzer spec registry** (`analyzerspec.go`) is the single source of truth
for each lens's params, applicable languages and hint. It is served at
`/api/config` and the UI builds its forms from it — there is no hand-maintained JS
mirror to drift (that drift previously hid `ast-grep`'s required `lang` param).

## File map

| File | Responsibility |
|------|----------------|
| `cli.go` | flag parsing + subcommand dispatch (`ui`, `build`, `sync`, `menu`, `clone`, …) |
| `registry.go` | core pipeline + analyzer registry |
| `language.go` | **Language registry**: ext→lang, joern frontend, wizard defaults, symbol scanner — the one home for language specifics |
| `symbols.go` | cached, incremental, language-neutral **symbol index** + the default regex scanners (tree-sitter would slot in here) |
| `analyzerspec.go` | **analyzer param specs** (params/langs/hint) served to the UI — single source of truth, no JS mirror |
| `projection.go` | projection render + two-way sync |
| `types.go` | core types, embedded `ui/`/`VERSION`/joern scripts |
| `config.go` | config load, project scan, language detection (delegates to `language.go`) |
| `analyzers_go.go` | **Go** frontend: symbols, call graph, unrolled-program adapter (shared core) |
| `analyzers_java.go` | **Java** frontend: control/data/object flow, cpg-methods, unroll (own engine), flow, entry/exitpoints |
| `analyzers_web.go` | **JS/TS** frontend: event surface, jsonl, unrolled-program adapter (shared core) |
| `unroll_lexical.go` | shared lexical unroll core (Go + TS adapters plug in via `lexAdapter`) |
| `analyzers_sideeffects.go` | `side-effects` lens + the shared effect scanner reused by graph/unroll |
| `analyzers_misc.go` | language-agnostic: bookmark/extract, ast-grep |
| `analyzers_postgres.go` | stateful Postgres table polling lens (`postgres-watch`) |
| `assumptions.go` | shared unroll/assumption helpers (guards, inlining, line views, per-line effects) |
| `servicegraph.go` | cross-service graph: TS imports + Go routes + TS→Go seam + per-node side-effect tags |
| `report.go` | `report` command: bake a graph + side-effects + findings markdown into one self-contained HTML |
| `clone.go` | shallow-clone a GitHub repo (shared by `clone` CLI + `/api/clone`) |
| `joern.go` | CPG build/parse/query (Joern, local + farm) |
| `ui.go` | the `ui` web studio (HTTP API + embedded SPA served from `ui/`) |
| `ui/` | composable web studio assets: `index.html`, `app.css`, `core.js`, `unroll.js`, `graph.js`, `studio.js` (embedded via `embed.FS`) |
| `menu.go` | interactive menu, setup wizard, watch |

## Where the abstraction is uneven (honest gaps)

The spine is language-agnostic; the **frontends are not symmetric**. Coverage
today, by analyzer × language:

| capability | Java | Go | JS/TS |
|------------|:----:|:--:|:-----:|
| symbols / structure | cpg-methods | go-symbols | js-events (events only) |
| control / data / object flow | ✅ (joern) | — | — |
| **unrolled-program** (flatten cross-file) | ✅ | ✅ | ✅ (lexical) |
| **per-line assumptions** (guard sets) | ✅ | ✅ (indentation-based) | ✅ (brace-depth) |
| **object timeline / loop bands** | ✅ (UI-lexical) | ✅ (UI-lexical) | ✅ (UI-lexical) |
| **branch input-collapse / toggle rail** | ✅ | — | — |
| service-graph node | imports n/a | routes + handlers | imports + seam source |

The TS unroller (`analyzers_web.go`) parses TS/JS into the same `unrollLine`
stream the UI already renders, so timeline + assumptions are now symmetric across
all three languages, and clicking a TS node in the cross-service graph drills into
a full assumptions+timeline view (not just opening the file). The remaining
unevenness is **branch resolution**: the Java unroller can collapse to one path
from `params.inputs` and expose a per-conditional toggle rail; the Go and TS
unrollers show both branches with their guard sets but do not yet evaluate inputs.

Secondary notes:
- `analyzers_java.go` is the heaviest file because Java has the most frontends
  (it predates the others); it's not doing anything the spine couldn't host for
  other languages.
- `unroll_lexical.go` is the shared lexical unroll core: Go and TS now plug into it
  via a tiny `lexAdapter` (find a function, recognize a guard header, recognize a
  local call) instead of each re-implementing the body walk. Java keeps its own
  recursive engine because it additionally resolves `inputs` and toggles branches —
  folding that into the lexical core would regress it; that remains the one place
  the three unrollers diverge.

## Adding a lens (the "small bro" path)

1. Write `AnalyzeXxx(cfg Config, lens LensConfig) (Projection, error)` in the
   matching `analyzers_*.go`.
2. Register it in `DefaultRegistry` (`registry.go`) and add an `AnalyzerSpec`
   (params/langs/hint) in `analyzerSpecs()` (`analyzerspec.go`) — that one entry
   drives the UI form, the analyzer filter and the consistency test; there is no
   separate JS schema to update.
3. Emit `ProjectionBlock`s (+ optional `LineGuards` for assumptions). Everything
   downstream — render, sync, UI, mermaid — is already generic.

## Adding a language

1. Add one `Language` entry in `language.go` (id, extensions, joern frontend,
   wizard defaults, `SuggestRoot`, and a `SymbolScanner`).
2. That's it for detection/wizard/menu/joern/symbol-search/entry-suggestion — they
   all read the registry. If the language should support `unrolled-program`, add an
   adapter (see the Java/Go/TS unrollers) and dispatch to it in
   `AnalyzeUnrolledProgram`.

## Swapping the symbol engine (e.g. tree-sitter)

`SymbolScanner` (`language.go`) is `func(rel string, lines []string) []Symbol`. The
default scanners are regex-based (`symbols.go`). To use tree-sitter/LSP instead,
implement the scanner per language and point the `Language.Scan` field at it — the
cached `symbolIndexFor`, the UI, and every analyzer keep working unchanged because
they only see the neutral `Symbol` type.

Stateful analyzers can keep private files under `.projections/<tool>/...`; the
`postgres-watch` lens uses `.projections/.postgres-watch/<lens>.json` for
environment/table high-water marks and a rolling row buffer. `watch` has a small
polling hook for analyzers that need time-based refresh instead of source-file
mtime changes.
