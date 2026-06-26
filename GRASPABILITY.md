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

### Phase 1 — front door, no new engines (THIS PASS)
Focused single-file lenses + git + the Questions panel. All lexical/structural.

- [ ] `references` lens — every mention of a symbol (rg-backed), confidence `lexical`
- [ ] `callers` lens — call sites of a function (ast-grep `name($$$)` → rg fallback), `structural`/`lexical`
- [ ] `constructions` lens — `new X` / `X{}` / factory calls for a type, `structural`
- [ ] `writes-to` lens — assignments/setters/mutations of a var (reuse UI `lineWrites` logic), `lexical`
- [ ] `sql-tables` lens — tables/columns a `.sql`/`.pggen.sql` file touches, `lexical`
- [ ] `git-blame` lens — annotate a span with commit/author/date, `exact`
- [ ] Register all + `analyzerSpecs()` entries
- [ ] `questionspec.go` — question registry (template, blanks, lens binding, confidence, langs)
- [ ] `/api/ask` — compile question + blank values → `ExecuteLens`; served list in `/api/config`
- [ ] UI Questions panel (`index.html` + `questions.js`): grouped fill-blank questions, blanks
      autocomplete from symbol index, confidence badge on each answer, deep-linkable
- [ ] Tests: query lenses, git-blame, question registry + ask
- [ ] Docs: README question-panel section

### Phase 2 — close parity with tree-sitter
- [ ] Tree-sitter `SymbolScanner` backend (per language) behind the existing seam
- [ ] Language-neutral call graph → precise `callers`/`references`/impact-set
- [ ] Lexical control-flow + data-flow for Go/TS (parity with Java's non-joern path)
- [ ] Drop "lexical" → "structural" confidence where tree-sitter backs the answer

### Phase 3 — "what happened" / runtime
- [ ] OTel/Jaeger trace lens (poll-adapter; target repo already runs Jaeger)
- [ ] SQL query → call-site linking (sql-tables ↔ db side-effects)
- [ ] git diff-since-commit question
- [ ] joern incremental (the long-deferred CPG node-level re-add)

---

## Done (this pass — update as we go)

- (filled in during Phase 1 implementation)

## Risks / honest notes

- The panel will **expose the Java-only parity gap** loudly (control/data/object-flow).
  Confidence badges are the mitigation; tree-sitter (Phase 2) is the fix.
- "Where is the bug" must read "where to look" in the UI — we narrow, we don't diagnose.
- Lexical `callers`/`references` will have false positives (same-named methods on
  different types). Badge them `lexical` and offer the structural/CPG upgrade.
