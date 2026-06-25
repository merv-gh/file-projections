# Self-hosting contribution loop

Can a small model contribute *good* changes to file-projections by reading the
code through **file-projections' own lens**? If yes, the projection is clean
enough to onboard a junior ("small bro"). This loop is that test.

- **`tools/contrib-todos.md`** — a queue of small, objective todos. Each pins its
  outcome with a failing Go test (the green gate).
- **`tools/contrib-loop.mjs`** — the runner. Per todo:
  1. make an isolated sandbox copy of the repo (a worktree),
  2. inject the failing gating test,
  3. let **qwen on the box** (cap **8 turns**) use `read_file`, **`view_program`**
     (the self-projection — flatten one of our own Go functions), `append_code`,
     and `run_tests`,
  4. **gate**: `go test ./src -run <test>` must go green,
  5. if green, "that's good" → copy the changed non-test source back into the
     main repo. Otherwise reject and leave main untouched.
  - **Idempotent**: a todo whose test already passes in a fresh sandbox is skipped.

`report.md` is the last run. Result of the demo queue (qwen3-coder, cap 8):

| todo | result | by |
|------|--------|----|
| `clampInt` (util.go) | ✓ accepted | qwen |
| `Config.LensByName` (config.go) | ✓ accepted | qwen |
| `Projection.FactByID` (projection.go) | ✓ accepted | qwen |

All three were written by the model, passed the gate, and were copied into `src/`
— each is small, idiomatic, and green. (For trivial additive functions the model
mostly uses `read_file`; `view_program` earns its keep when the target function
is large and cross-file — that's the projection doing the explaining.)

## Run it

```bash
go build -o file-projections ./src      # the lens binary the sandbox uses
node tools/contrib-loop.mjs             # needs BOX_BASE reachable
```

Env: `BOX_BASE` (default `http://192.168.1.148:11434`), `BOX_MODEL`
(default `qwen3-coder:latest`). Add todos to `tools/contrib-todos.md` in the
fenced ` ```todo ` format (id/title/file/test_name/view/test/instruction).
