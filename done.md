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
