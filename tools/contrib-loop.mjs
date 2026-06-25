// contrib-loop: a self-hosting contribution gate.
//
//   node tools/contrib-loop.mjs
//
// For each todo in tools/contrib-todos.md:
//   1. make an isolated sandbox copy of the repo (a "worktree")
//   2. inject the todo's failing test
//   3. let qwen on the box (cap 8 turns) read the code THROUGH file-projections'
//      own lens (view_program self-projection) + read_file, then append the change
//   4. gate: `go test ./src -run <test>` must go GREEN
//   5. if green and the diff is non-empty -> "that's good", copy the changed src
//      files back into the main repo. Otherwise reject and leave main untouched.
//
// The point: if a small model can contribute green changes by reading our code
// through our own projection, the projection is clean enough to onboard a junior.
import { spawn, execFile } from "node:child_process";
import { promisify } from "node:util";
import { readFileSync, writeFileSync, mkdtempSync, rmSync, existsSync, appendFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
const run = promisify(execFile);

const REPO = process.cwd();
const BASE = process.env.BOX_BASE || "http://192.168.1.148:11434";
const MODEL = process.env.BOX_MODEL || "qwen3-coder:latest";
const TURN_CAP = 8;

// ---- parse todos ----------------------------------------------------------
function parseTodos(md) {
  const out = [];
  const re = /```todo\n([\s\S]*?)```/g; let m;
  while ((m = re.exec(md))) {
    const body = m[1]; const t = {};
    // simple key: value with `|` block support
    const lines = body.split("\n"); let i = 0;
    while (i < lines.length) {
      const l = lines[i];
      const kv = l.match(/^(\w+):\s?(.*)$/);
      if (kv) {
        if (kv[2].trim() === "|") {
          const buf = []; i++;
          while (i < lines.length && (lines[i].startsWith("  ") || lines[i].trim() === "")) { buf.push(lines[i].replace(/^ {2}/, "")); i++; }
          t[kv[1]] = buf.join("\n").replace(/\n+$/, "");
          continue;
        }
        t[kv[1]] = kv[2].trim();
      }
      i++;
    }
    out.push(t);
  }
  return out;
}

// ---- box (curl; node fetch flakes to this host) ---------------------------
async function box(system, tools, messages) {
  const body = JSON.stringify({ model: MODEL, max_tokens: 1200, system, tools, messages });
  const { stdout } = await run("curl", ["-s", "-m", "180", "-X", "POST", `${BASE}/v1/messages`,
    "-H", "content-type: application/json", "-H", "anthropic-version: 2023-06-01", "-H", "x-api-key: ollama",
    "--data-binary", body], { maxBuffer: 32 * 1024 * 1024 });
  const r = JSON.parse(stdout);
  if (r.type === "error") throw new Error(JSON.stringify(r.error));
  return r;
}

// ---- sandbox helpers ------------------------------------------------------
async function makeSandbox(id) {
  const dir = mkdtempSync(join(tmpdir(), "contrib-" + id + "-"));
  // copy the working tree (uncommitted refactor included), excluding heavy/irrelevant dirs
  await run("rsync", ["-a", "--exclude=.git", "--exclude=node_modules", "--exclude=.projections",
    "--exclude=file-projections", "--exclude=tmp", "--exclude=workspace", "--exclude=spring-petclinic-main",
    "--exclude=recurring-master", REPO + "/", dir + "/"]);
  await run("go", ["build", "-o", "file-projections", "./src"], { cwd: dir, maxBuffer: 32 * 1024 * 1024 });
  return dir;
}
async function goTest(dir, testName) {
  try {
    const { stdout } = await run("go", ["test", "./src", "-run", testName, "-count=1"], { cwd: dir, maxBuffer: 32 * 1024 * 1024 });
    return { ok: true, out: stdout.trim() };
  } catch (e) {
    return { ok: false, out: ((e.stdout || "") + (e.stderr || "")).toString().trim().split("\n").slice(-12).join("\n") };
  }
}

// ---- tools exposed to qwen ------------------------------------------------
function toolDefs() {
  const str = (d) => ({ type: "string", description: d });
  return [
    { name: "read_file", description: "Read a source file under the repo, e.g. src/util.go.", input_schema: { type: "object", properties: { path: str("repo-relative path") }, required: ["path"] } },
    { name: "view_program", description: "Show a Go function as ONE flattened, numbered straight-line program (file-projections' own unrolled-program lens). Args: file (under src/, e.g. util.go), method (function name). Use it to learn the surrounding code's style before you write.", input_schema: { type: "object", properties: { file: str("file under src/, e.g. config.go"), method: str("function name") }, required: ["file", "method"] } },
    { name: "append_code", description: "Append Go code (a new function) to a source file. Provide the complete function text. The file already has `package main` and imports.", input_schema: { type: "object", properties: { path: str("repo-relative path, e.g. src/util.go"), code: str("complete Go function to append") }, required: ["path", "code"] } },
    { name: "run_tests", description: "Build and run the gating test for this task. Returns PASS or the failure output.", input_schema: { type: "object", properties: {}, required: [] } },
  ];
}
async function callTool(dir, todo, name, args) {
  if (name === "read_file") {
    const p = join(dir, args.path);
    return existsSync(p) ? readFileSync(p, "utf8").split("\n").slice(0, 400).map((l, i) => `${i + 1}: ${l}`).join("\n") : `no such file: ${args.path}`;
  }
  if (name === "view_program") {
    try {
      const { stdout } = await run("./file-projections", ["-analyzer", "unrolled-program", "-source-root", "src", "-file", args.file, "-method", args.method, "-inline_depth", "1", "-out", ".cv.projection"], { cwd: dir, maxBuffer: 16 * 1024 * 1024 });
      const proj = readFileSync(join(dir, ".cv.projection"), "utf8");
      const body = proj.split("\n").filter((l) => l && !l.startsWith("#") && !l.startsWith("=>")).join("\n");
      return body.slice(0, 2000) || stdout;
    } catch (e) { return "view_program failed: " + ((e.stderr || e.message) + "").slice(0, 300); }
  }
  if (name === "append_code") {
    appendFileSync(join(dir, args.path), "\n" + args.code + "\n");
    return "appended to " + args.path;
  }
  if (name === "run_tests") {
    const r = await goTest(dir, todo.test_name);
    return r.ok ? "PASS — tests are green." : "FAIL\n" + r.out;
  }
  return "unknown tool";
}

// ---- run one todo ---------------------------------------------------------
async function runTodo(todo) {
  const dir = await makeSandbox(todo.id);
  // inject the failing gating test
  writeFileSync(join(dir, "src", "contrib_" + todo.id.replace(/-/g, "_") + "_test.go"),
    "package main\n\nimport \"testing\"\n\n" + todo.test + "\n");
  const before = await goTest(dir, todo.test_name); // should be red (won't compile)
  if (before.ok) { // already satisfied in main — nothing to do (idempotent re-run)
    rmSync(dir, { recursive: true, force: true });
    return { id: todo.id, title: todo.title, calls: 0, turns: [], green: true, accepted: false, changed: [], skipped: "already satisfied" };
  }
  const SYS = `You are contributing a small change to a Go project (package main, in src/). ` +
    `Use the tools to read the relevant file, optionally view a nearby function with view_program to match its style, then append your new code with append_code, then run_tests. ` +
    `Keep the change minimal and idiomatic. Stop once run_tests returns PASS.`;
  const messages = [{ role: "user", content: todo.instruction + `\n\nThere is a gating test ${todo.test_name}; make it pass. A good function to view first: ${todo.view || "(none)"}.` }];
  const tools = toolDefs();
  let calls = 0, turns = [], green = false;
  for (let t = 0; t < TURN_CAP; t++) {
    let r; try { r = await box(SYS, tools, messages); } catch (e) { turns.push({ error: e.message }); break; }
    const blocks = r.content || [];
    messages.push({ role: "assistant", content: blocks });
    const uses = blocks.filter((b) => b.type === "tool_use");
    const text = blocks.filter((b) => b.type === "text").map((b) => b.text).join(" ").trim();
    if (!uses.length) { turns.push({ text }); break; }
    const results = [];
    for (const u of uses) {
      calls++;
      const out = await callTool(dir, todo, u.name, u.input || {});
      if (u.name === "run_tests" && out.startsWith("PASS")) green = true;
      turns.push({ tool: u.name, input: u.input, out: out.slice(0, 400) });
      results.push({ type: "tool_result", tool_use_id: u.id, content: out });
    }
    messages.push({ role: "user", content: results });
    if (green) break;
  }
  // final authoritative gate
  const after = await goTest(dir, todo.test_name);
  green = after.ok;
  let changed = [];
  if (green) {
    // copy back every changed non-test src file (the model may add code anywhere),
    // never the injected gating test.
    const files = (await run("ls", ["src"], { cwd: dir })).stdout.split("\n")
      .filter((f) => f.endsWith(".go") && !f.endsWith("_test.go"));
    for (const f of files) {
      const rel = join("src", f);
      const sb = readFileSync(join(dir, rel), "utf8");
      if (!existsSync(join(REPO, rel)) || sb !== readFileSync(join(REPO, rel), "utf8")) {
        writeFileSync(join(REPO, rel), sb);
        changed.push(rel);
      }
    }
  }
  rmSync(dir, { recursive: true, force: true });
  return { id: todo.id, title: todo.title, calls, turns, green, accepted: green && changed.length > 0, changed, started_red: !before.ok };
}

// ---- main -----------------------------------------------------------------
const todos = parseTodos(readFileSync(join(REPO, "tools/contrib-todos.md"), "utf8"));
console.log(`contrib-loop: ${todos.length} todos on ${MODEL} (cap ${TURN_CAP} turns each)\n`);
const results = [];
for (const todo of todos) {
  process.stdout.write(`▶ ${todo.id} … `);
  try { const r = await runTodo(todo); results.push(r); console.log(`${r.skipped ? "↺ " + r.skipped : (r.accepted ? "✓ that's good — copied " + r.changed.join(",") : (r.green ? "green (no diff)" : "✗ rejected (red)"))} [${r.calls} calls]`); }
  catch (e) { console.log("ERROR " + e.message); results.push({ id: todo.id, error: e.message, green: false, accepted: false }); }
}
// write a markdown report
const md = ["# contrib-loop report", "", `model: ${MODEL} · cap ${TURN_CAP} turns · ${new Date().toISOString()}`, "",
  "| todo | result | green | tool calls | copied back |", "|---|---|---|---|---|",
  ...results.map((r) => `| ${r.id} | ${r.skipped ? "↺ already satisfied" : (r.accepted ? "✓ accepted" : (r.green ? "green/no-diff" : "✗ rejected"))} | ${r.green ? "✓" : "✗"} | ${r.calls || 0} | ${(r.changed || []).join(", ") || "—"} |`),
  "", "## transcripts", ...results.flatMap((r) => ["", `### ${r.id}`, ...(r.turns || []).map((t) => t.tool ? `- \`${t.tool}(${JSON.stringify(t.input || {})})\` → ${String(t.out || "").replace(/\n/g, " ").slice(0, 120)}` : (t.text ? `- say: ${t.text.slice(0, 160)}` : (t.error ? `- error: ${t.error}` : "")))])];
mkdirSync(join(REPO, "tools/benchmark/contrib-report"), { recursive: true });
writeFileSync(join(REPO, "tools/benchmark/contrib-report/report.md"), md.join("\n"));
console.log(`\nReport: tools/benchmark/contrib-report/report.md`);
