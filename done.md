# Done — Questions panel (Phase 1)

The "graspability" front door is shipped: users ask a plain-language question with
typed blanks ("Who calls `[name]`?") and it compiles to an existing lens through the
same `ExecuteLens` spine. No new engine — questions bind known lenses to a phrasing
and a confidence badge.

## Shipped

### 6 focused "where" lenses (lexical/structural, no CPG)
Registered in `registry.go` + spec'd in `analyzerspec.go` so the UI builds forms
from one source of truth.

| Lens | Answers | Backend | Confidence |
|---|---|---|---|
| `references` | where is X used | rg word-boundary | lexical |
| `callers` | who calls X() | rg call-shape | lexical |
| `constructions` | where is type T built | rg `new T`/`T{}`/`&T{}`/`T.new` | lexical |
| `writes-to` | what mutates X | rg assign/`++`/`--`/setter | lexical |
| `sql-tables` | which tables `.sql` files touch | `.sql` FROM/JOIN/INTO/UPDATE walk | lexical |
| `git-blame` | who last changed file:line | `git blame --porcelain` | exact |

All emit a `confidence` fact so the UI can badge how much to trust the answer
(`analyzers_query.go`, `analyzers_git.go`). `git-blame` runs from the source-root
dir so it works in nested clones; degrades to a clear error without a git checkout.

### Question registry + binding
- `questionspec.go` — 12 questions across **Change / Diagnose / Observe** intents,
  each a template with typed blanks, a lens binding, langs, and a confidence badge.
  Adding a question is one entry (mirrors `analyzerSpecs()`).
- `compileQuestion()` maps blank values → lens params (with optional rename map),
  missing blanks fall through to the lens's own required-param error.

### Server
- `/api/ask` (`ui.go:516`) — compiles a question + values → `ExecuteLens`, then
  prefers any `confidence` fact the lens emitted over the question's default badge.
- `/api/config` now serves `questionRegistry()` so the panel is data-driven.

### UI
- `ui/questions.js` + `ui/index.html` — grouped fill-the-blank questions; blanks
  autocomplete from the language-neutral symbol index (`symbols.go`); confidence
  badge on each answer.

## Verified
- `go build`, `go vet`, full `go test ./src` green (build was broken on resume by
  two `queryRows` calls missing the `level` arg — fixed).
- Live server smoke test: `/api/config` lists all 12 questions; `who-calls queryRows`
  returns 5 correct call sites with file:line refs + `lexical` badge.

---

# Done — Structural call graph (Phase 2)

Built the precise call graph that turns the pile of lexical lenses into a graph, and
upgraded `who-calls` from lexical to structural. **Pure-Go, no new dependencies** —
chose this over real tree-sitter (which needs CGO + grammar packages and breaks the
repo's dependency-free/offline design; verified it builds, then rejected the dep).

## Shipped

### Pure-Go structural call graph (`callgraph.go`)
- Scope-resolved edges: a `name(` counts as a call only when it's **inside a known
  function body** and **resolves to a function declared in the same source root**.
  Comments and string/char/template literals are scrubbed (`stripCallNoise`) so a
  name mentioned in text is never a false call site.
- Incremental per-file content-hash cache, mirroring the symbol index.
- `ImpactSet` = BFS over reverse edges with a visited guard (cycles terminate).
- Honest ceiling: scope-resolved, not type-resolved — same-named functions merge,
  reported via `ambiguityNote`. joern stays the type-precise upgrade.

### `FuncBodies` seam (`language.go`)
New per-language hook reusing each language's existing body parser
(`parseJavaMethods` / `goFuncBodies` / `tsFuncBodies` → `parseTSFuncs`). Adding a
language stays one registry entry; a tree-sitter backend can later swap this one
function without touching the graph or lenses.

### Two structural lenses (`analyzers_callgraph.go`)
| Lens | Answers | Confidence |
|---|---|---|
| `call-graph-callers` | resolved call sites of a function (no false positives) | structural |
| `impact-set` | transitive callers by BFS depth — "if I change X, what breaks" | structural |

Registered in `registry.go`, spec'd in `analyzerspec.go`. `who-calls` now binds to
`call-graph-callers` (`structural`); new question **"If I change {name}, what breaks?"**
binds to `impact-set`.

## Verified
- `go build`, `go vet`, full `go test ./src` green.
- New `TestCallGraphResolvesCallersAndImpact`: comment/string decoy `leaf()` mentions
  are not counted (1 real caller), impact depths correct (mid=1, entry=2), cycle
  (`other`↔`unrelated`) terminates, structural+blast facts present.

## Deferred (Phase 3, per YAGNI — see review-graph.md)
- **Write/authoring**: the anchor model is replace-only; insert/create code and a
  transactional multi-file apply (for rename/rewrite) are deferred until a concrete
  refactor use case needs them. The call graph shipped here is the prerequisite that
  makes safe rewrites *possible*.
- Real tree-sitter backend behind the seam (pure-Go covers the need today).
- Lexical control/data-flow for Go/TS (still Java-only via joern).
- Runtime/Observe: OTel traces, sql-table ↔ call-site linking, git diff-since.

---

# Done — Cross-repo interprocedural paths (Phase 3)

The project's original purpose, now working: trace "how do we end up at this line?"
across an app repo and its internal libraries, resolving Spring **dependency
inversion** across the repo boundary. Pure-Go (no joern). Full design in CROSS-REPO.md.

## The hard problem solved
A `@RestController` in an internal library calls an **abstract** service; the
**concrete** override lives in the app repo. Neither repo alone shows a path. Loading
both + resolving the type hierarchy across them yields the seamless path. Verified
against the fixtures (`fixtures/billing-lib` + `fixtures/shop-app`): tracing
`RealPaymentService.java:29` (`ledger.write`) produces two answers, each starting at
the library's `@PostMapping charge()`, crossing the repo boundary through the DI hop
(`AbstractPaymentService.pay → RealPaymentService.pay`), with per-path guards.

## Shipped
- **`workspace.go`** — user-level workspace at `~/.file-projections` (override via
  `FILE_PROJECTIONS_HOME`). Add a repo by **link** (point at a local folder, no copy)
  or **clone**. `workspace.json` persists the set.
- **`gradle.go`** — parse group + dependencies from build.gradle(.kts)/settings;
  classify internal libraries (shared group prefix or `project(:...)` dep) vs external.
- **`javatypes.go`** — cross-repo Java type hierarchy (package/extends/implements/
  fields/methods) + `ConcreteOverrides` dispatch (the DI primitive). Strips block
  comments so javadoc mentioning a method isn't mis-parsed.
- **`analyzers_trace.go`** — `trace-to-line` lens: MULTIPLE answers, one control path
  per entrypoint→line, each with guard assumptions, loop markers, DI hops and repo
  crossings. Reverse-BFS over the workspace call graph with cycle guard + path cap.
- **UI** — `/api/workspace` (list/link/clone/rm) and `/api/trace`; Workspace + Trace
  panels (`ui/trace.js`, new tab in `index.html`); `workspace` CLI command. New
  question "How do we end up at {file}:{line}? (cross-repo)".

## Confidence
`structural` (scope-resolved by type+method+arity). A path through a DI boundary is
badged **`structural (di)`** with the resolved override named. Ambiguity (>1 override,
generics, overloads, proxies) is reported, not hidden — joern stays the precise upgrade.

## Verified
- `go build`, `go vet`, full `go test ./src` green (3 new cross-repo tests:
  gradle parse + internal-dep classification, cross-repo override resolution, full
  DI trace with entrypoint/boundary/guard assertions).
- Live UI on :7777: `/api/workspace` shows `shop-app` internally depends on
  `billing-lib`; `/api/trace` returns the 2 DI-resolved answers. `demo-phase3.html`
  baked from live output.

## Deferred (Phase 4, per YAGNI)
- Precision: generics/overload resolution, non-Spring DI (Guice/Dagger), Kotlin/Scala.
- Runtime/Observe: OTel traces, sql-table ↔ call-site linking, git diff-since.
- Write/authoring + transactional multi-file apply (unchanged deferral).
