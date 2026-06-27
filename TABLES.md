# Tables as first-class, traceable nodes (Phase 5)

Make the database a first-class citizen of the cross-repo graph: tables become nodes,
JPA repositories get **writes-to / reads-from** edges, SQL migrations are attributed,
and a table is a **trace target** ("how do we end up writing to `orders`?") across
services. Plus: a folder chooser for adding repos, and a hand-holding lens wizard
(SQL watches + custom lenses with examples). Finish by running it against a real DB.

## The gap (from exploration)

There is **zero** entity/repository/`@Table`/migration parsing in the tool today.
`parseJavaTypes` discards class-level annotations and generic type args; the Java db
side-effect regex flags a `.save(` but captures no table; `sql-tables` only reads
`.sql` files; `postgres-watch` polls real tables but isn't connected to code. The
graph renderer (`renderGraph`) and `serviceGraphFromProject` are kind-agnostic, so
table nodes + read/write edges are mostly additive once we can resolve the mapping.

## A. The join key: code → entity → table → physical table

The whole feature hinges on one mapping chain, built in a new `javadb.go`:

```
orderRepository.save(o)         (call site, analyzers_trace.go already lands here)
   └─ orderRepository : OrderRepository           (field type, javatypes already resolves)
        └─ OrderRepository extends JpaRepository<Order, Long>   (entity = generic arg 0)
             └─ Order is @Entity @Table(name="orders")          (entity → table)
                  └─ "orders"  ── same string ──> Flyway V1__*.sql, postgres-watch
```

New parsing (extends `parseJavaTypes`, mirroring the method-annotation pattern
`springEntryAnnRE` already uses):
- **Class annotations**: capture `@Entity`, `@Table(name="...")` preceding a class.
  Add `Annotations []string` + an `EntityTable string` to `JavaType`.
- **Repository interfaces**: detect `extends JpaRepository<E, Id>` /
  `CrudRepository` / `PagingAndSortingRepository` / `Repository`. Capture the first
  generic arg `E` **before** `simpleTypeName` strips generics. So
  `OrderRepository → entity Order`.
- **Entity → table**: `@Table(name=X)` if present, else the snake/lowercase of the
  entity name (Spring's default `Order → order`, but real apps usually annotate;
  we report which rule was used).
- **Migrations**: scan `**/db/migration/V*__*.sql` (Flyway) and
  `**/db/changelog/**` (Liquibase) for `CREATE TABLE` / `ALTER TABLE` targets,
  attributing each table to the migration file that creates/changes it.

Result: two maps usable everywhere — `repoType → table` and `table → {entity,
migrations, repos}`. Pure-Go, regex, dependency-free, same ethos as Phases 2–4.

## B. Tables on the service graph

In `serviceGraphFromProject` add, after the type nodes:
- One **table node** per discovered table: `Kind:"table"`, ID `table::orders`,
  Label `orders`, `Effects:["db"]`, and (when known) the migration file as `File`.
- For each repository type, an edge **repo → table**:
  - `Kind:"writes-to"` when the type's methods include write ops
    (`save/saveAll/delete/persist/merge/insert/update/upsert`),
  - `Kind:"reads-from"` when they include read ops (`find*/get*/exists/count/query`).
  - A repo usually does both → two edges (or one `Kind:"reads-writes"`; we emit the
    distinct ones so the graph reads cleanly).
- `Cross:true` when the repo and the migration that owns the table live in different
  repos (a table defined by a library migration but written from the app — exactly
  the cross-service traceability you want).

The UI (`graph.js`) is kind-agnostic, so a `.gnode.ktable` color rule + `.gedge.writes-to`
/ `.gedge.reads-from` styles + a small node-kind legend make tables visible. Tables
get a distinct shape/color (cylinder-ish via color, since it's `<rect>`).

## C. Tables as trace targets

`interestingLine` already lands a trace on the first side-effect line — which for a
repository is the `.save(`/`.find(` call. To make a **table** a trace target:
- `methodsBySymbol` gains a table case: typing `orders` (a known table) resolves to
  every repository method that writes/reads it; the trace then runs to each of those
  call sites. So "how do we end up at `orders`?" yields the full entrypoint→…→
  `orderRepository.save` paths, cross-repo and DI-aware, terminating with a
  synthesized `★ writes orders` marker.
- A new question: **"How do we end up writing to {table}?"** bound to `trace-to-line`
  with the table symbol. Table names autocomplete in `/api/trace-symbols`.

This is the headline: tables traceable cross-service, reusing the entire Phase 3/4
path engine — no new path-finding code, just symbol resolution + a terminal marker.

## D. Folder chooser for "Add a repo"

The project modal's repo path input becomes a **browse** button reusing the existing
`/api/dirs` directory browser (already powering the old source picker). Clicking
Browse opens the same crumb/▸ navigation; "Use this folder" fills the path. Clone URL
stays as the alternative. No new backend (the dir API exists).

## E. Hand-holding lens wizard

A guided **"＋ Add a lens"** flow in the UI (and a parallel `menu` improvement) that:
- Lists lens types grouped by intent with a one-line "what it answers" + a **filled-in
  example** for the active project (prefilled params), so adding to a new project is
  click-not-type.
- First-class **SQL watch** path: pick env/DSN (with a localhost example), pick tables
  (autocompleted from the discovered table set — so you watch real tables you can
  already see on the graph), set window/poll. Writes a `postgres-watch` lens to
  config.
- **Custom lens** path: any analyzer, with its `analyzerSpecs` form and example values.
- Everything writes config.json (single source of truth) via the existing
  `/api/lenses` upsert.

Served by a new `/api/lens-templates` (examples derived from the active project) so
the wizard is data-driven, not hardcoded in JS.

## F. Run it against a real DB

Bring up Postgres (docker if available), apply the fixture migration, insert a row via
the traced path's shape, configure a `postgres-watch` lens through the wizard, and show
the live rolling window — closing the loop: a table you can **see** on the graph,
**trace** to from an entrypoint, and **watch** change at runtime. Demo saved under the
gitignored `/demos`.

## Honest non-goals (unchanged)
- Full SQL/ORM coverage (named queries, `@Query`, criteria, QueryDSL, JOOQ) — we cover
  Spring Data repository conventions + `@Table` + Flyway/Liquibase `CREATE/ALTER`.
- Column-level lineage. Table granularity only.
- Precision ceiling and authoring/transactions — still deferred.

## Build order
1. Fixtures (entity + repository + migration) so everything has something real.
2. `javadb.go` — entity/repo/`@Table`/migration parsing + the maps.
3. Service-graph table nodes + read/write edges.
4. Trace: table-as-target resolution + terminal marker + question.
5. UI: folder chooser; table node/edge styling + legend; lens wizard + `/api/lens-templates`.
6. Tests.
7. Run against a real DB; demo under `/demos`.
