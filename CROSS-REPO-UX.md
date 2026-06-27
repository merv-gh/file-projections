# Cross-repo UX (Phase 4)

Phase 3 proved the cross-repo, dependency-inversion trace works. This phase makes it a
**seamless, config-driven experience**. The engine doesn't change; the surfaces do.

## Goals (from the request)

1. **config.json is the single source of truth** for cross-repo. The workspace
   (projects + their libraries) lives in config, not a hidden `~/.file-projections`
   side file you can't see. Adding/removing a project edits config.
2. **Projects, not source roots.** Choosing what to analyze becomes "pick a known
   project, or add one (folder / clone)". One project = an app repo + its internal
   libraries. This lightens the top bar (no separate Change… + Clone… controls) and
   the trace tab (no repo dropdown).
3. **Trace by symbol, not file:line:repo.** You type a symbol; the selected project
   (and optionally its libraries) is searched automatically. No manual file, line, or
   repo entry. Optionally the answer offers "expand with library" when a path can
   continue into a registered library.
4. **Service-graph works cross-repo** — built from the project's repos, not a single
   hand-written `services` JSON param.
5. **Lighter Ask panel** — underlined inputs (no boxes), drop the big
   lexical/structural badges.

## A. Config as the single source of truth

Extend `Config` with a `workspace` block:

```jsonc
{
  "root": ".",
  "projections_dir": ".projections",
  "workspace": {
    "projects": [
      {
        "name": "shop",
        "repos": [
          { "name": "shop-app",   "path": "fixtures/shop-app",   "role": "app" },
          { "name": "billing-lib","path": "fixtures/billing-lib","role": "library" }
        ]
      }
    ],
    "active": "shop"
  },
  "lenses": [ ... ]
}
```

- `path` is resolved relative to `cfg.Root` if relative, used as-is if absolute (so a
  cloned/linked repo anywhere on disk works). `role` is `app` | `library`, detected
  from gradle (app = has a non-library plugin / is depended-upon-by nobody) but
  overridable.
- The previous `~/.file-projections/workspace.json` becomes a **fallback/import
  source only**: on first load, if config has no `workspace` but the user-level file
  exists, we offer to import it. Going forward config wins. This keeps one visible
  source of truth.
- `Workspace` is built from config: `workspaceFromConfig(cfg) -> *Workspace`. The
  existing `LoadWorkspace()` stays for the CLI `workspace` command but the UI/trace
  read from config. Adding a project via the UI **writes config.json** (the same
  POST `/api/config` path), so it's visible and version-controllable.

### Project = app + libraries
A project groups repos that form one logical service. Gradle group detection
(`gradle.go`) still classifies internal libraries; `role` is persisted so we don't
re-guess. The "active" project drives the whole UI: source root, symbol search,
trace, and service graph all scope to its repos.

## B. Trace by symbol (no file/line/repo)

Today `/api/trace` takes `{repo,file,line}`. New flow:

- Input: **a symbol** (method/type name) + a boolean **include libraries**.
- Resolve: search the active project's repos for a method/type matching the symbol
  (reusing the workspace symbol/type index). If it resolves to exactly one method,
  trace to its declaration line. If several, return the candidates so the UI can
  disambiguate inline (a small picker), still no manual file/line typing.
- `include_libraries`: when off, restrict the trace graph to the **app** repo only;
  when on, include `library` repos so paths can cross the boundary and DI hops
  resolve. The answer reports whether enabling libraries would reveal more
  entrypoints ("3 more paths available if you include billing-lib").
- `/api/trace` new body: `{ project?, symbol, include_libraries }`. `repo/file/line`
  remain accepted for deep-links/back-compat but are not surfaced in the UI.

Engine reuse: `buildTypeIndex` + `buildTraceGraph` already take a `*Workspace`. We
just build that workspace from the active project (optionally filtered to the app
repo) and resolve `symbol -> target method` before calling the existing
`tracePaths`. No change to the path-finding core.

### "Expand with library"
When a trace restricted to the app repo hits a method whose body calls an abstract /
external type that a registered library implements, the answer surfaces an inline
**"expand with <lib>"** affordance. Clicking re-runs with that library included. This
makes the library hop opt-in and visible rather than automatic, matching "optionally
show if it can be expanded with library".

## C. Service-graph cross-repo

`AnalyzeServiceGraph` currently needs a hand-written `services` JSON param + a single
`source_root` containing all services. New path: when a service-graph lens has no
`services` param but the config has an active project, derive the services list from
the project's repos (name, path, detected lang). The graph then spans repos exactly
like the trace does, and the existing TS↔Go / import / route edges plus the new
cross-repo Java call edges render together. The UI graph tab gains the same
"include libraries" scope as trace.

## D. UI changes

### Top bar (lighter)
Replace the `source` chip + `Change…` + clone input with a single **project picker**:
a dropdown of known projects (from config) + an "＋ Add project" affordance that opens
a small panel: name, then add repos by **folder path** or **clone URL**, mark each
app/library. Saving writes config.json and re-detects. The language tag stays.

### Ask panel (lighter)
- Inputs: remove borders/boxes; render blanks as **underlined text inputs** that read
  like fill-in-the-blank prose.
- Remove the per-question `conf` chips and the big answer badge's chip; keep a small,
  muted one-line note (analyzer + short confidence word as plain muted text, not a
  colored pill). The colored `lexical/structural/cpg/exact` pills go away.

### Trace tab (simpler)
- Drop the repo `<select>`, the file input, and the line input.
- One **symbol** input with autocomplete (methods/types from the active project), an
  **include libraries** checkbox, and a **Trace** button.
- The workspace management (link/clone/list) moves into the **project picker** in the
  top bar (single place to manage repos), so the trace tab is purely "ask".
- Answers render as today (per-path cards, DI hops, repo crossings, guards), plus the
  "expand with library" affordance when applicable.

## E. Make config.json complete

The repo's own `config.json` should demonstrate the new shape: a `workspace` with the
`shop` project (shop-app + billing-lib) so the UI boots into a working cross-repo
example, alongside the existing lenses. A service-graph lens that uses the project
(no manual `services`) is added.

## Non-goals (unchanged deferrals)
- Precision ceiling (generics/overloads/non-Spring DI/Kotlin) — still scope-resolved,
  joern is the upgrade.
- Authoring/transactions — still deferred.

## Build order
1. Config `workspace` schema + `workspaceFromConfig` + import-from-user-file fallback.
2. `/api/projects` (list/add/remove → writes config) and trace-by-symbol in
   `/api/trace`.
3. Service-graph: derive services from the active project.
4. UI: project picker (top bar), Ask panel restyle, Trace tab simplification.
5. Update repo `config.json` to the new shape.
6. Tests + demo under gitignored `/demos`.
