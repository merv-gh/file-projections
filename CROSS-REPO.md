# Cross-repo interprocedural paths (Phase 3)

## The problem, stated plainly

A real Java service is not one repo. It's an app repo plus several internal libraries
(same Gradle group, e.g. `com.acme.*`), often with Spring controllers and beans living
*inside the libraries*. Asking "how do we end up at this line of code?" therefore can't
be answered from a single repo: the entrypoint (a `@RestController` in a library) and
the line you're staring at (in your app) are in different checkouts, wired together by
Spring dependency injection.

The hard part is **dependency inversion across the repo boundary**:

```
billing-lib (library, com.acme.billing)
  PaymentController.charge()            ← Spring @RestController entrypoint
     └─ calls  abstractPaymentService.pay(order)   // field typed as AbstractPaymentService

shop-app (your repo, com.acme.shop)
  RealPaymentService extends AbstractPaymentService
     └─ pay(order) { ... ledger.write(order) }      ← the line you care about
```

The library only knows the **abstract** type. Your repo provides the **concrete**
override. A single-repo view of the library dead-ends at `AbstractPaymentService.pay`
(abstract, no body). A single-repo view of your app never sees the controller. The
*seamless path* — controller → abstract call → resolved concrete override → your line —
only exists when both repos are loaded **and** the type hierarchy is resolved across
them. This is the original purpose of this project and the thing no off-the-shelf
single-repo tool answers.

## Why not just joern

Joern *can* build one CPG over both roots and resolve this. But: (1) it's the deferred
incremental-CPG cost we keep dodging, (2) it needs both repos materialised and a multi-
minute parse, and (3) the whole Phase 1/2 thesis is that a dependency-free, scope-
resolved backend answers most questions instantly with an honest confidence badge.
Cross-repo DI resolution is *structural* (type hierarchy + override dispatch), not
data-flow, so it's exactly the kind of thing the pure-Go backend can do. Joern stays the
type-precise upgrade for the cases the lexical hierarchy can't disambiguate (generics,
overloads, runtime proxies).

## Decomposition

Five independent pieces, each shippable and testable on its own:

### A. Multi-root workspace (`workspace.go`)
Today: one `cfg.Root` + per-lens `SourceRoot` (a path *inside* root). Clones land in
`<root>/workspace/clones`. That's repo-local and can't represent "my app + three
libraries I cloned from elsewhere."

Add a **user-level workspace** at `~/.file-projections/` (override with
`FILE_PROJECTIONS_HOME`). It holds:
- `repos/<name>/` — cloned or symlinked repo checkouts
- `workspace.json` — the set of registered repos: `{name, path, kind: clone|link, group?}`

A repo is added either by **clone** (existing git flow, retargeted to the workspace) or
by **link** (user points at an existing local folder; we record the absolute path, no
copy). This is the "user points to a folder OR we clone" requirement.

A `Workspace` is a set of roots. The symbol index and call graph are already keyed by
**absolute source root** (`symbols.go`, `callgraph.go`), so per-repo indexing already
works unchanged — the new work is a layer that *queries across* the per-repo indexes
and merges results. No change to the single-root caches.

### B. Gradle/group detection (`gradle.go`)
For each registered repo, parse `build.gradle` / `build.gradle.kts` / `settings.gradle`
for:
- the project **group** (`group = "com.acme.billing"` / `group 'com.acme'`)
- declared **dependencies** (`implementation "com.acme.shared:..."`, project deps
  `project(":lib")`)

Then classify dependencies as **internal** (same group prefix as another registered
repo, or a `project(:...)` reference) vs external (Maven Central etc.). This is what
lets the UI/CI say "this edge crosses into an internal library" and what tells the
workspace which repos belong to one logical service.

If `build.gradle` has no group, fall back to: the git remote org, else the repo dir
name. Honest fact when guessed.

### C. Java type hierarchy + override resolution (`javatypes.go`)
The missing primitive (the explore confirmed zero `extends`/`implements` parsing today).
Across all registered repos, build a language-neutral-ish **type table**:
- `package`, `class/interface/abstract`, `extends` supertype, `implements` interfaces
- methods per type (reuse `parseJavaMethods`)

From it, resolve **override dispatch**: given a call `x.pay(...)` where `x` is declared
of type `AbstractPaymentService` (abstract/interface), the concrete targets are all
types that `extends`/`implements` it (transitively) **and** declare `pay`. This is the
DI resolution. It is *scope-resolved by name+arity*, not full generic/overload
resolution — the honest ceiling, badged accordingly.

Bean/field type inference is deliberately simple and covers the common Spring shapes:
- field declarations `private AbstractPaymentService paymentService;`
- constructor params `RealService(AbstractPaymentService s)`
- `@Autowired` / constructor-injected fields
We map a *call on a field* to the field's **declared** type, then dispatch to overrides.

### D. Trace-to-line lens with multiple answers (`analyzers_trace.go`)
The headline feature. "How do we end up at `file:line`?" Answer: **every control path
from an entrypoint to that line**, each rendered as a separate, readable answer with:
- the entrypoint that starts it (Spring annotation, cross-repo aware)
- the call chain, **including DI hops** ("via override: Real… extends Abstract…")
- per-line **guards/assumptions** (reuse the unroller's brace-depth guard stack)
- **loop** markers (the line is reached 0..N times) and where data the line depends on
  was last changed (reuse `writes-to`)
- **cross-repo hop** markers so it's obvious when a path leaves the library into the app

Crucially this is **multi-answer**: not a flat list of callers but N distinct
control-flows, each with its own assumption set, emitted as separate projection blocks
(the `Projection.Extra` mechanism already supports one-file-per-branch, used by
control-flow). This directly answers your "questions can have multiple answers" point.

Algorithm (pure-Go, bounded):
1. Locate the target function (the one whose body contains `file:line`).
2. Reverse-BFS over the **cross-repo call graph** (Phase 2 graph extended with DI edges
   from C) to find entrypoints that reach it. Each distinct path = one answer.
3. For each path, forward-render the straight-line guards along the chain using the
   existing lexical unroller per hop, annotating DI hops and repo boundaries.
4. Cap paths (`max_paths`, default 8) and depth; report truncation honestly.

### E. UI + CI surface
- `/api/workspace` (list/add/remove repos: clone or link), `/api/trace` (run the lens).
- A **Workspace panel** (`ui/workspace.js`): see registered repos, their detected
  group, internal-vs-external deps, add by folder path or git URL.
- A **Trace panel** (`ui/trace.js`): pick file:line (or a symbol), see the N answers,
  each collapsible, cross-repo hops and guards highlighted.
- Keep the server on :7777 so it's directly checkable.

## Confidence model (unchanged philosophy)
- `structural` when the path resolves through declared types/overrides in scope.
- A path that crosses a DI boundary is badged **`structural (di)`** with the resolved
  override named, so the user sees *why* we think the abstract call lands on the
  concrete method.
- If an abstract call has >1 concrete override in scope, **all** are emitted as separate
  answers (that's the multi-answer point again) and flagged ambiguous.
- joern remains the upgrade for cases we can't resolve lexically.

## Honest non-goals (YAGNI / deferred)
- Full generic/overload resolution, runtime proxy/AOP, reflection, `@Conditional` bean
  selection. We resolve by type+name+arity and report ambiguity.
- Writing/refactoring across repos (the deferred authoring/transaction work stays
  deferred).
- Non-Spring DI containers (Guice, Dagger) — the field-type→override model is generic,
  but the entrypoint detection ships Spring patterns first.
- Kotlin/Scala libraries — Java first; the hierarchy parser is structured so a second
  language scanner can be added behind the same seam.

## Build order
1. Sample fixtures (so every later piece has something real to run against).
2. `workspace.go` (multi-root + link/clone).
3. `gradle.go` (group/dep detection, internal-lib classification).
4. `javatypes.go` (hierarchy + override resolution) — the core.
5. `analyzers_trace.go` (multi-answer trace, cross-repo, DI-aware).
6. UI panels + endpoints; keep :7777 live.
7. Tests + `demo-phase3.html`.
