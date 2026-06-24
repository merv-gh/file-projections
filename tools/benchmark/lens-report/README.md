# Lens benchmark report

A reproducible bug-localization benchmark for the **lens** tool surface
(`view_program` + `line_assumptions`) versus plain **base** file reading
(`read_file`), run against a local model on the ollama box.

- **`index.html`** — self-contained, offline viewer. Open it directly in a
  browser. Shows the planted bug, the task, a comparison table, and the full
  turn-by-turn transcript (every message + tool call + tool result) for each
  surface, capped at 8 turns.
- **`report.json`** — the raw run data the viewer embeds.

## What it measures

Both surfaces get the *same* task on a cross-file "loyalty points are doubled"
bug (`fixtures/xfile-sample`, spread across `Loyalty` → `Tier` → `Promo`).

Headline metric = **calls until the tool surfaces the deciding condition**
(`spend > 0`). The lens inlines the whole call chain into one numbered view, so
the deciding branch is visible on the first call without reading any source; the
base surface must open each file in turn. Total call count is reported too, with
the honest caveat that it varies with how much the model chooses to double-check.

## Regenerate

```bash
# full run (hits the box; needs BOX_BASE reachable + BENCH_FPBIN built):
node tools/lens-bench.mjs

# rebuild index.html from an existing report.json (no model calls):
node tools/lens-bench.mjs --render-only
```

Env: `BOX_BASE` (default `http://192.168.1.148:11434`), `BOX_MODEL`
(default `qwen3-coder:latest`). Build the binary first: `go build -o file-projections .`
