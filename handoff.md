# Lens-vs-Graph Benchmark — Handoff

## Purpose
Measure whether a **file-projections lens** lets a small tool-calling LLM fix a real bug with
fewer tokens/tool-calls than (a) plain grep+read+edit (`base`) and (b) the `code-review-graph`
traversal MCP (`graph`). Three variants, same task, same test, isolated copies, run
sequentially; every request/turn logged; live HTML report.

Latest experiment (`unroll-experiment.py`) — the **unrolled-program lens**: flatten the
cross-file/branched execution that a failing test triggers into ONE numbered straight-line
program (stages inlined, only the taken branch, decoy objects excluded). The agent reads/edits
ONLY that program; edits sync back two-way to the real scattered source. Result: proj 4 calls /
3.5k tok vs base 13 / 21.7k vs graph 16 / 43.2k — all PASS.

## Environment
- **Binary**: `make build` → `bin/file-projections` (Go, stdlib-only). Lens analyzers in `main.go`,
  registered in `DefaultRegistry()`: entrypoints, exitpoints, control-flow, data-flow,
  entry-to-exit, joern-var-flow, **object-flow** (new), bookmark/extract (two-way), go-symbols,
  ast-grep, flow, js-events, jsonl.
- **Ollama** (tool-calling): `qwen3-coder:latest` (default; reliable tool use), `qwen2.5-coder:3b`
  (emits tool calls as JSON-in-content — harness `parse_calls` handles native + JSON + XML).
- **Docker**: tests run as `gradle:8.5-jdk17` with a warm cache volume `grade-gradle-cache`
  (~12s/run warm, ~40s cold). `javac` is also local.
- **code-review-graph**: CLI at `~/.local/bin/code-review-graph`. `build --repo <dir>` writes
  `<dir>/.code-review-graph/graph.db`; `serve` = stdio MCP (harness `GraphMCP` drives it,
  passing `repo_root` per call — otherwise it auto-detects the OUTER git repo and queries the
  wrong db). Pre-build the sample's graph so the graph variant "jumps right in".
- **Samples** (`fixtures/<name>/`, each a Gradle+JUnit project with a planted bug):
  `grade-sample` (control-flow / wrong conditions), `dataflow-sample` (var-flow / wrong source),
  `objflow-sample` (object-flow & unroll / Receipt assembled across stages; bugs: `TierStage:6`
  `setLabel`→`setTier`, `LabelStage:6` `amount`→`net`; has 2 ctors, an `if/else`, a decoy
  `PreviewService` object to defeat naive search).

## Joern
No local joern binary. Real-CPG lenses (`mode: joern`) run embedded scripts in `tools/joern/*.sc`
(`//go:embed`): `control-flow.sc`, `entry-to-exit.sc`, `java-var-flow.sc`, **`object-flow.sc`**.
`object-flow.sc` walks every method that allocates a target type and emits a per-object,
branch-annotated mutation timeline (constructor → cross-file setter calls resolved to the field
→ never-set fields → name/field-mismatch anomalies). Scripts are uploaded+run by the farm; the
binary re-embeds on `go build` (verify with `strings bin/file-projections | grep "object built in"`).

## Farm (joern offload, port 9090)
`tools.joern.farm` in `farm.config.json` points lenses at `http://localhost:9090` so no local
joern is needed. The farm parses source (content-manifest cached per source-root; edits → diff →
re-parse) and runs scripts against the kept CPG (`POST /jobs`, `POST /jobs/{id}/script`).
- Start: `make farm-up` (docker compose in `joern-farm/`). **It is currently DOWN** (stopped by a
  `docker kill` cleanup) — restart before any joern/unroll run.
- Health: `curl -s localhost:9090/health` → `{"done":N,...}`.
- Stale CPG during manual testing: `rm -f .projections/.cpg/*.farmjob*` to force re-parse.

## How to run
```sh
make build && make farm-up                       # binary + joern farm (needed for joern lenses)
ollama list | grep qwen3-coder                   # tool-calling model present
MODEL=qwen3-coder:latest python3 tools/unroll-experiment.py run   # run + open live report
python3 tools/unroll-experiment.py report        # rebuild report from /tmp/unroll-exp/*.json
```
Harness map (all same shape: ollama tool-loop, isolated `/tmp/<exp>/wc-<variant>` copies,
gradle-in-docker `run_tests` → clean expected/actual from JUnit XML, live report + conversation
replay): `java-experiment.py` (control-flow, :8771), `dataflow-experiment.py` (var-flow, :8772),
`objflow-experiment.py` (object-flow, :8773), **`unroll-experiment.py`** (unrolled-program, :8774,
the current best), `clean-experiment.py` (projection-as-source on Go, :8770). Outputs in
`/tmp/<exp>-exp/{live.json,<variant>.json,report.html}`. Don't background a "waiter" loop — it
gets killed and leaks `code-review-graph serve`/docker procs (clean: `pkill -f "code-review-graph
serve"; docker ps -q | xargs docker kill`).

unroll proj tools (lens-only, no file access): `view_program(inputs={...})` (inputs choose the
branch — no magic; no inputs → returns the branch condition + entry sig), `edit_program(line,
new_code)` (line# → real origin, offset-safe two-way), `run_tests`.

## TODO
1. **Fix base & graph on the latest sample/prompt.** With the terser task prompt + harder
   `objflow-sample`, base regressed to 13 turns/21.7k tok and graph to 16/43.2k (graph often
   loops). Check: is the graph variant given non-applicable tools (the `objflow-sample` graph is
   tiny — semantic_search/query_graph add little), and did the prompt change make base over-read?
   Goal: base/graph runs that are sane, not strawmen.
2. **Review fairness & overfit.** The unroll lens is tuned to this sample: `build_unrolled`
   hardcodes the `App`/`Receipt`/`sample.` package + `XxxStage().apply(r,...)` orchestrator
   pattern and inlines `apply`/`build` bodies via regex (`_path_stages`, `_method_body`,
   `_test_inputs`). It is NOT general. Decide what's the honest claim; ideally drive the unroll
   from the real `object-flow.sc` CPG output (call order + branches it already produces) instead
   of Python regex, so it generalizes beyond this fixture.
3. **Re-run with reports** once 1–2 land; capture the table + transcripts; keep the JSON/HTML.
4. **Integrate into CLI + watch.** Promote object-flow (done as analyzer) and the unrolled-program
   view into real lenses/config, and make their `line→origin` edits a first-class **two-way sync**
   under `file-projections watch` (today the sync lives only in the Python harness;
   `SyncProjection` handles contiguous bookmark spans, not the scattered per-line origins the
   unroll lens needs — extend it).

## Gotchas
- `filepath.Join(".", "/abs", …)` drops the leading slash → pass joern lenses a RELATIVE
  source-root with `cwd=workdir`, not an absolute one.
- Java tree-sitter in the graph labels methods by return type (`of`→`String`); query by class.
- `gradle -q` prints nothing on success; failures aren't suppressed.
- Each variant copy gets a unique path → unique farm job → fresh parse (no cross-variant staleness).
