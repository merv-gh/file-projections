// #4 validation: drive qwen3-coder on the ollama box through the minimal lens
// surface (view_program + line_assumptions) on a planted "counter set too much"
// bug, and check it localizes the bug from assumptions alone — no source reads.
//
//   node tools/qwen-lens-check.mjs
//
// Box exposes the Anthropic Messages API (ollama). Read-only: edit/test tools are
// withheld so this measures localization, not patching.
import { spawn, execFile } from "node:child_process";
import { createInterface } from "node:readline";
import { promisify } from "node:util";
const run = promisify(execFile);

const BASE = process.env.BOX_BASE || "http://192.168.1.148:11434";
const MODEL = process.env.BOX_MODEL || "qwen3-coder:latest";

// ---- spawn the lens MCP for the loyalty fixture --------------------------
const env = {
  ...process.env,
  BENCH_VARIANT: "lens",
  BENCH_FPBIN: process.cwd() + "/file-projections",
  BENCH_CWD: process.cwd() + "/fixtures/loyalty-sample",
  BENCH_SRC: "src/main/java",
  BENCH_ENTRY_FILE: "sample/Loyalty.java",
  BENCH_ENTRY_METHOD: "points",
};
const mcp = spawn("node", ["tools/proj-mcp.mjs"], { env, stdio: ["pipe", "pipe", "inherit"] });
const rl = createInterface({ input: mcp.stdout });
const pend = new Map();
let mid = 0;
rl.on("line", (l) => { if (l.startsWith("{")) { const m = JSON.parse(l); if (pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } } });
function mreq(method, params) { const i = ++mid; mcp.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: i, method, params }) + "\n"); return new Promise((r) => pend.set(i, r)); }
await mreq("initialize", { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "qwen-check", version: "1" } });
const toolList = (await mreq("tools/list", {})).result.tools
  .filter((t) => t.name === "view_program" || t.name === "line_assumptions")
  .map((t) => ({ name: t.name, description: t.description, input_schema: t.inputSchema }));
async function callTool(name, args) {
  const r = await mreq("tools/call", { name, arguments: args || {} });
  return r.result?.content?.map((c) => c.text).join("\n") || JSON.stringify(r.error || {});
}

// ---- Anthropic Messages tool-use loop against the box --------------------
const SYSTEM = `You are debugging a Java method. You may ONLY use the two tools provided — do not ask to read source files.
Workflow: call view_program (optionally with inline=2 to expand helper calls) to see the method as one numbered straight-line program.
Then call line_assumptions on any line that changes the result, to learn the exact condition under which it runs.
A line that mutates the accumulator under a condition that is true for almost every normal call is the bug.`;
const TASK = `The method Loyalty.points(spend, member) awards too many points for ordinary repeat purchases.
Find the single line that over-awards points and state the exact condition under which it (wrongly) fires.
Answer with: BUG LINE <n>, the code, and REACHED-WHEN <condition>.`;

const messages = [{ role: "user", content: TASK }];
async function box(messages, withTools = true) {
  // curl reaches the box reliably where node fetch hits intermittent EHOSTUNREACH.
  const payload = { model: MODEL, max_tokens: 1024, system: SYSTEM, messages };
  if (withTools) payload.tools = toolList;
  const body = JSON.stringify(payload);
  const { stdout } = await run("curl", ["-s", "-m", "120", "-X", "POST", `${BASE}/v1/messages`,
    "-H", "content-type: application/json", "-H", "anthropic-version: 2023-06-01", "-H", "x-api-key: ollama",
    "--data-binary", body], { maxBuffer: 16 * 1024 * 1024 });
  const r = JSON.parse(stdout);
  if (r.type === "error") throw new Error(JSON.stringify(r.error));
  return r;
}

let calls = 0, finalText = "";
for (let turn = 0; turn < 14; turn++) {
  const r = await box(messages);
  const blocks = r.content || [];
  messages.push({ role: "assistant", content: blocks });
  const texts = blocks.filter((b) => b.type === "text").map((b) => b.text).join(" ").trim();
  if (texts) finalText = texts;
  const toolUses = blocks.filter((b) => b.type === "tool_use");
  if (!toolUses.length) break;
  const results = [];
  for (const tu of toolUses) {
    calls++;
    const out = await callTool(tu.name, tu.input);
    console.log(`\n[tool ${calls}] ${tu.name}(${JSON.stringify(tu.input)}) ->\n${out}`);
    results.push({ type: "tool_result", tool_use_id: tu.id, content: out });
  }
  messages.push({ role: "user", content: results });
}
// Force a final conclusion if the model ended on a tool call (no tools this turn).
if (!finalText || messages[messages.length - 1].role === "user") {
  messages.push({ role: "user", content: "Stop investigating. Give your final answer now and DO NOT call any tools: BUG LINE <n>, the code, and REACHED-WHEN <condition>." });
  const r = await box(messages, false);
  finalText = (r.content || []).filter((b) => b.type === "text").map((b) => b.text).join(" ").trim() || finalText;
}

console.log("\n================ FINAL ANSWER ================\n" + finalText);
const hit = /\bspend\s*>\s*0\b/.test(finalText) && /\b(12|13)\b/.test(finalText.replace(/spend\s*>\s*0/g, ""));
const lineHit = /(line\s*5\b|points\s*=\s*points\s*\+\s*5)/i.test(finalText) && /spend\s*>\s*0/.test(finalText);
console.log("\n================ JUDGE ================");
console.log(`tool calls: ${calls}`);
console.log(`localized bug (cites 'spend > 0' as the wrong guard): ${/spend\s*>\s*0/.test(finalText) ? "YES" : "NO"}`);
mcp.kill();
process.exit(0);
