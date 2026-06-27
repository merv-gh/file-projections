# Graspability — making the codebase answer questions

A plan for turning file-projections from "pick a lens + fill params" into "ask a
question in plain language." The unit of UX is a **question with typed blanks**
("How do I get from `[___]` to `[___]`?") that compiles to an existing lens. The
blanks autocomplete from the language-neutral symbol index (`symbols.go`), which is
what makes the whole panel cheap.

## Principles

- A **question is a saved lens recipe with typed blanks**. The question registry
  (`questionspec.go`) mirrors the analyzer spec registry (`analyzerspec.go`): adding
  a question is one entry; it compiles to an `ExecuteLens` call.
- **Answer instantly and approximately, offer to upgrade to precise.** rg/ast-grep
  give a lexical answer now; joern (when present) upgrades it. Every answer carries a
  **confidence badge** (`lexical` | `structural` | `cpg`).
- **Many tiny single-file lenses** beat one general engine (YAGNI). Each answers one
  question over one file/scope and plugs into the existing analyzer+spec registries.
- **Honesty over silence.** If a question can't answer for the detected language, say
  so and point at the lexical alternative; never return an empty pane.
- This lets us **defer the joern incremental problem**: most "where" questions are
  answerable lexically/structurally and never touch a CPG.

## Intents

- **Understand** — "what is this / what's the surface"
- **Diagnose** — "where is the bug" (we narrow the search; we never claim "here's the bug")
- **Change** — "where do I add/modify, and what breaks"
- **Observe** — "what happened / what changed" (needs external signals: git, db, traces)

## Status legend

✅ have · 🔧 glue (compose + small work) · 🏗️ invest (new lens/engine) · 🧗 defer · ⚡ fast approximate tool

---

## Question catalogue

### Change
- [✅] Where is `[symbol]` defined? — symbol index
- [✅] Public surface of `[file/module]` — go-symbols / js-events / cpg-methods
- [✅] Entrypoints of `[service]` — entrypoints
- [🔧] Who calls / references `[symbol]`? — rg ⚡ / ast-grep → tree-sitter precise
- [🔧] Where is `[type]` constructed? — ast-grep `new X()` / `X{}`
- [✅] Where do we read config `[ENV]`? — side-effects(process) + rg
- [🏗️] If I change `[function]`, what breaks? (impact set) — transitive callers
- [🔧] Where should I add a handler like the others? — entrypoints + service-graph routers

### Diagnose
- [✅ Java / 🏗️ Go+TS] How do I get from `[entry]` to `[line/sink]`? — control-flow / entry-to-exit
- [✅ Java / 🏗️ multi] Show all the ways we end up `[saving/sending]` `[object]` — exitpoints + entry-to-exit
- [✅ Java / 🏗️ multi] Which lines shape `[var]`? — data-flow
- [✅ Java / 🏗️ multi] How is `[type]` assembled / which field is never set? — object-flow
- [✅ all] What must be true to reach `[file:line]`? — unrolled-program guards
- [✅ all] Flatten `[function]` so I read one path — unrolled-program
- [✅ all] What does `[function/dir]` touch (IO/net/db)? — side-effects (+ unroll se-tags)
- [✅] What runs on a schedule / async? — entrypoints
- [✅] Which TS calls which Go op? — service-graph api-call edges
- [🧗] Where can `[var]` be null/unchecked? — nullability dataflow

### Observe
- [✅] What rows changed recently in `[table]`? — postgres-watch
- [🔧] History of `[file:line]` / who last touched it? — git blame/log
- [🔧] What changed since `[commit]`? — git diff + symbol index
- [✅ partial / 🏗️ precise] What DB queries does `[endpoint]` run? — unroll+side-effects(db); SQL lens
- [🏗️] What did request `[trace-id]` do? — OTel/Jaeger fetch
- [🧗] Recent errors/logs for `[service]`? — log source

---

## Tooling ladder (fastest answer first)

| Tool | In repo | Gives faster | Verdict |
|---|---|---|---|
| rg | ✅ | name references, strings | instant first answer |
| ast-grep | ✅ (underused) | structural matches, no CPG | 🔧 wire behind "where" questions |
| tree-sitter | seam ready (`Language.Scan`) | precise multi-lang symbols/calls/scopes | 🏗️ **biggest lever**; closes parity, defers joern |
| git | no | blame/log/diff = "what happened/changed" | 🔧 cheap, high value |
| SQL parse (.pggen/sqlc) | no | "which queries touch table X" + call sites | 🔧 cheap regex |
| OTel/Jaeger | no | runtime "what did this request do" | 🏗️ poll-adapter like postgres-watch |
| LSP | no | precise cross-lang find-refs | 🧗 heavy per-language |
| joern | ✅ | interprocedural precise | keep for deep Qs; incremental deferred |

---

## Roadmap

### Phase 1 — front door, no new engines (SHIPPED)
Focused single-file lenses + git + the Questions panel. All lexical/structural.

- [x] `references` lens — every mention of a symbol (rg-backed), confidence `lexical`
- [x] `callers` lens — call sites of a function (rg call-shape `name(`), `lexical`
- [x] `constructions` lens — `new X` / `X{}` / `&X{}` / `X.new` for a type, `lexical`
- [x] `writes-to` lens — assignments/`++`/`--`/setter mutations of a var, `lexical`
- [x] `sql-tables` lens — tables a `.sql` file touches (FROM/JOIN/INTO/UPDATE), `lexical`
- [x] `git-blame` lens — annotate a file/span with commit/author/date, `exact`
- [x] Register all (`registry.go`) + `analyzerSpecs()` entries (`analyzerspec.go`)
- [x] `questionspec.go` — 12-question registry (template, blanks, lens binding, conf, langs)
- [x] `/api/ask` — compile question + blank values → `ExecuteLens`; list in `/api/config`
- [x] UI Questions panel (`index.html` + `questions.js`): grouped fill-blank questions, blanks
      autocomplete from symbol index, confidence badge on each answer
- [x] Tests: query lenses, git-blame, question registry + ask (in `main_test.go`)
- [ ] Docs: README question-panel section (deferred — `done.md` covers it for now)

### Phase 2 — close parity with a structural call graph (SHIPPED, pure-Go)
Chose a **pure-Go** structural scanner over real tree-sitter: tree-sitter needs CGO +
grammar packages, which breaks this repo's dependency-free/offline design (verified it
builds, but rejected the dependency). The `SymbolScanner`/`FuncBodies` seam
(`language.go`) is exactly where a tree-sitter backend would later slot in with no
change to the graph or lenses — so this is the same architecture, cheaper backend.

- [x] `FuncBodies` seam per language (`language.go`) — reuses each language's existing
      body parser (`parseJavaMethods`/`goFuncBodies`/`parseTSFuncs`) so the graph,
      unroller and symbol search agree on "what is a function"
- [x] Structural call graph (`callgraph.go`): scope-resolved edges (a call counts only
      inside a known body and resolving to a declared function), incremental per-file
      cache, string/comment scrubbing so mentions in text don't count
- [x] `call-graph-callers` lens — precise upgrade of lexical `callers`; `who-calls`
      question now binds to it (`structural`)
- [x] `impact-set` lens + "If I change {name}, what breaks?" question — transitive
      callers by BFS depth (the review-graph blast radius)
- [x] Confidence drops `lexical` → `structural`; honest `ambiguityNote` when a name
      has >1 declaration (the scope-not-type ceiling, where joern stays the upgrade)
- [x] Tests: resolution precision (comment/string decoys), impact depths, cycle safety
- [ ] Lexical control-flow + data-flow for Go/TS (still Java-only via joern — deferred)
- [ ] Real tree-sitter backend behind the seam (deferred; pure-Go covers the need)

### Phase 3 — cross-repo interprocedural paths (SHIPPED — see CROSS-REPO.md)
The original purpose: "how do we end up at this line?" across an app repo + its
internal libraries, resolving Spring dependency-inversion (library calls an abstract
service; the app provides the concrete override) across the repo boundary. Pure-Go,
no joern.

- [x] Multi-root **workspace** at `~/.file-projections` (`workspace.go`): register a
      repo by **link** (point at a local folder) or **clone**; `workspace.json`.
- [x] **Gradle group/dep detection** (`gradle.go`): parse group + dependencies from
      build.gradle(.kts)/settings; classify internal libs (shared group / project dep).
- [x] **Java type hierarchy + override resolution** (`javatypes.go`): cross-repo
      extends/implements + concrete-override dispatch (the DI primitive). Block-comment
      aware so javadoc isn't mis-parsed.
- [x] **trace-to-line** lens (`analyzers_trace.go`): MULTIPLE answers — one control
      path per entrypoint→line, each with guards, loops, DI hops (abstract→concrete)
      and repo-boundary crossings. Reverse-BFS over the cross-repo call graph.
- [x] `/api/workspace` + `/api/trace`; **Workspace + Trace UI panels**
      (`ui/trace.js`); `workspace` CLI command. Question "How do we end up at
      {file}:{line}? (cross-repo)".
- [x] Sample fixtures (`fixtures/billing-lib` + `fixtures/shop-app`) demonstrating the
      DI inversion across repos; tests cover gradle parse, override resolution, and the
      full cross-repo DI trace.

### Phase 4 — "what happened" / runtime &nbsp;·&nbsp; deferred authoring/transactions
- [ ] OTel/Jaeger trace lens (poll-adapter; target repo already runs Jaeger)
- [ ] SQL query → call-site linking (sql-tables ↔ db side-effects)
- [ ] git diff-since-commit question
- [ ] joern incremental (the long-deferred CPG node-level re-add)
- [ ] Cross-repo precision upgrades: generics/overload resolution, non-Spring DI
      (Guice/Dagger), Kotlin/Scala libraries — currently scope-resolved by
      type+method+arity and badged ambiguous; joern stays the precise upgrade.
- [ ] **Write/authoring (deferred, see review-graph.md):** insert/create code (anchor
      model is replace-only today) + transactional multi-file apply for rename/rewrite.
      Deferred per YAGNI until a concrete refactor use case needs it; the precise call
      graph shipped here is the prerequisite that makes safe rewrites *possible*.

---

## Done (this pass — Phase 1)

Questions panel shipped end-to-end. See `done.md` for the full table; in short:

- 6 lexical/structural lenses: `references`, `callers`, `constructions`, `writes-to`,
  `sql-tables`, `git-blame` (`analyzers_query.go`, `analyzers_git.go`).
- 12 questions across Change/Diagnose/Observe (`questionspec.go`), bound to existing
  lenses, served via `/api/config`, run via `/api/ask` (`ui.go:516`).
- UI panel (`ui/questions.js`) with symbol-index autocomplete + confidence badges.
- Every answer carries a `confidence` fact (`lexical` | `structural` | `cpg` | `exact`).
- Build/vet/tests green; verified live (`who-calls queryRows` → 5 call sites).

Honest note: `constructions` shipped at `lexical` confidence (not `structural` as
first planned) — it's still rg-backed, not AST-backed. Tree-sitter (Phase 2) is what
earns the `structural` badge.

## Done (this pass — Phase 2)

Structural call graph shipped end-to-end, **pure-Go** (no CGO/tree-sitter dependency):

- `callgraph.go` — scope-resolved call graph per source root: a call counts only
  inside a known function body and resolving to a declared function; comments and
  string literals are scrubbed first. Incremental per-file cache like the symbol index.
- `FuncBodies` seam (`language.go`) reuses each language's existing body parser, so
  adding a language stays a one-entry change and a tree-sitter backend can later swap
  in without touching the graph.
- `call-graph-callers` lens (precise upgrade of lexical `callers`; `who-calls` now
  binds to it) and `impact-set` lens ("If I change X, what breaks?" — transitive
  callers by BFS depth).
- Confidence `lexical` → `structural`; `ambiguityNote` is honest about the
  scope-not-type ceiling (joern remains the type-precise upgrade).
- Tests: comment/string decoy resolution, impact depths, cycle termination. Full
  suite + vet green.
- Deferred per YAGNI: write/authoring (insert/create + transactional multi-file
  apply) and a real tree-sitter backend. See `review-graph.md` for why the call graph
  was the right next step for both the review graph and write-safety.

## Risks / honest notes

- The panel will **expose the Java-only parity gap** loudly (control/data/object-flow).
  Confidence badges are the mitigation; tree-sitter (Phase 2) is the fix.
- "Where is the bug" must read "where to look" in the UI — we narrow, we don't diagnose.
- Lexical `callers`/`references` will have false positives (same-named methods on
  different types). Badge them `lexical` and offer the structural/CPG upgrade.
