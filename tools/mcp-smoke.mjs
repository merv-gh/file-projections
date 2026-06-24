// Quick smoke test for proj-mcp.mjs lens-variant tools (view_program + line_assumptions).
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";

const env = {
  ...process.env,
  BENCH_VARIANT: "lens",
  BENCH_FPBIN: process.cwd() + "/file-projections",
  BENCH_CWD: process.cwd() + "/fixtures/grade-sample",
  BENCH_SRC: "src/main/java",
  BENCH_ENTRY_FILE: "sample/Grader.java",
  BENCH_ENTRY_METHOD: "of",
};
const p = spawn("node", ["tools/proj-mcp.mjs"], { env, stdio: ["pipe", "pipe", "inherit"] });
const rl = createInterface({ input: p.stdout });
const pend = new Map();
let id = 0;
rl.on("line", (l) => {
  if (!l.startsWith("{")) return;
  const m = JSON.parse(l);
  if (pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); }
});
function req(method, params) {
  const i = ++id;
  p.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: i, method, params }) + "\n");
  return new Promise((r) => pend.set(i, r));
}
const txt = (r) => r.result?.content?.map((c) => c.text).join("\n") || JSON.stringify(r.error || r.result);
await req("initialize", { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "smoke", version: "1" } });
const tl = await req("tools/list", {});
console.log("TOOLS:", tl.result.tools.map((t) => t.name).join(", "));
console.log("\n--- view_program {} ---\n" + txt(await req("tools/call", { name: "view_program", arguments: {} })));
console.log("\n--- line_assumptions {line:4} ---\n" + txt(await req("tools/call", { name: "line_assumptions", arguments: { line: 4 } })));
p.kill();
process.exit(0);
