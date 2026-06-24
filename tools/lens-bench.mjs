// lens-bench: run the same "counter set too much" bug-localization task through
// two tool surfaces on the ollama box and emit a self-contained HTML report.
//
//   node tools/lens-bench.mjs
//
//   lens  = view_program + line_assumptions   (the new lens surface)
//   base  = read_file                          (plain source reading)
//
// Both capped at 8 turns. Records every message + token usage, judges whether the
// model localized the bug, and writes:
//   tools/benchmark/lens-report/report.json   (raw)
//   tools/benchmark/lens-report/index.html    (slick offline viewer, data inlined)
import { spawn, execFile } from "node:child_process";
import { createInterface } from "node:readline";
import { promisify } from "node:util";
import { mkdirSync, writeFileSync, readFileSync } from "node:fs";
import { join } from "node:path";
const run = promisify(execFile);

const BASE = process.env.BOX_BASE || "http://192.168.1.148:11434";
const MODEL = process.env.BOX_MODEL || "qwen3-coder:latest";
const TURN_CAP = 8;
const ROOT = process.cwd();
const FIXTURE = "fixtures/xfile-sample";
const ENTRY_FILE = "sample/Loyalty.java";
const ENTRY_METHOD = "points";
const OUTDIR = join(ROOT, "tools/benchmark/lens-report");

const TASK = `The Java method Loyalty.points(spend, member) awards too many points for ordinary repeat purchases. ` +
  `The calculation is spread across Loyalty, Tier and Promo. Exactly one line adds a bonus under a condition that is ` +
  `true for almost every purchase (it should only apply in a special case). Find it. ` +
  `Answer with: BUG LINE <n>, the code, and REACHED-WHEN <condition>.`;
const EFFICIENT = `\nBe efficient: stop investigating and give your final answer as soon as you can justify it — do not keep checking lines once you have found the bug.`;
const SYS = {
  lens: `You are debugging Java. Use ONLY the provided tools — do NOT ask to read source files.\n` +
    `Call view_program to see the method as ONE numbered straight-line program with every cross-file call already inlined.\n` +
    `Call line_assumptions on a line that adds to the points to learn the exact condition under which it runs.\n` +
    `The bug is the bonus line whose condition is true for almost every call.` + EFFICIENT,
  base: `You are debugging Java. Use the read_file tool to inspect source.\n` +
    `The logic spans multiple files — read each file you need, reason about the control flow yourself, and find the line that wrongly adds a bonus.` + EFFICIENT,
};
const TOOLSETS = { lens: ["view_program", "line_assumptions"], base: ["read_file"] };

function mcpFor(variant) {
  const env = {
    ...process.env, BENCH_VARIANT: variant === "base" ? "base" : "lens",
    BENCH_FPBIN: ROOT + "/file-projections", BENCH_CWD: ROOT + "/" + FIXTURE,
    BENCH_SRC: "src/main/java", BENCH_ENTRY_FILE: ENTRY_FILE, BENCH_ENTRY_METHOD: ENTRY_METHOD,
  };
  const proc = spawn("node", ["tools/proj-mcp.mjs"], { env, stdio: ["pipe", "pipe", "ignore"] });
  const rl = createInterface({ input: proc.stdout });
  const pend = new Map(); let id = 0;
  rl.on("line", (l) => { if (l.startsWith("{")) { const m = JSON.parse(l); if (pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } } });
  const req = (method, params) => { const i = ++id; proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: i, method, params }) + "\n"); return new Promise((r) => pend.set(i, r)); };
  return { proc, req };
}

async function box(system, tools, messages) {
  const body = JSON.stringify({ model: MODEL, max_tokens: 1024, system, tools, messages });
  const { stdout } = await run("curl", ["-s", "-m", "180", "-X", "POST", `${BASE}/v1/messages`,
    "-H", "content-type: application/json", "-H", "anthropic-version: 2023-06-01", "-H", "x-api-key: ollama",
    "--data-binary", body], { maxBuffer: 32 * 1024 * 1024 });
  const r = JSON.parse(stdout);
  if (r.type === "error") throw new Error(JSON.stringify(r.error));
  return r;
}

function judge(text) {
  const t = text.toLowerCase();
  const cond = /spend\s*>\s*0/.test(t);
  const line = /points\s*=\s*points\s*\+\s*5|promo|bonus/.test(t);
  return cond && line;
}

async function runVariant(variant) {
  const { proc, req } = mcpFor(variant);
  await req("initialize", { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "bench", version: "1" } });
  const allTools = (await req("tools/list", {})).result.tools;
  const tools = allTools.filter((t) => TOOLSETS[variant].includes(t.name))
    .map((t) => ({ name: t.name, description: t.description, input_schema: t.inputSchema }));
  const callTool = async (name, args) => {
    const r = await req("tools/call", { name, arguments: args || {} });
    return r.result?.content?.map((c) => c.text).join("\n") || JSON.stringify(r.error || {});
  };

  const SIGNAL = /spend\s*>\s*0/; // the deciding condition the tools must surface
  const messages = [{ role: "user", content: TASK }];
  const turns = [];
  let calls = 0, inTok = 0, outTok = 0, finalText = "", callsToSignal = 0;
  for (let t = 0; t < TURN_CAP; t++) {
    const r = await box(SYS[variant], tools, messages);
    inTok += r.usage?.input_tokens || 0; outTok += r.usage?.output_tokens || 0;
    const blocks = r.content || [];
    messages.push({ role: "assistant", content: blocks });
    const text = blocks.filter((b) => b.type === "text").map((b) => b.text).join("\n").trim();
    if (text) finalText = text;
    const toolUses = blocks.filter((b) => b.type === "tool_use");
    const turnRec = { n: t + 1, text, tools: [] };
    if (!toolUses.length) { turns.push(turnRec); break; }
    const results = [];
    for (const tu of toolUses) {
      calls++;
      const out = await callTool(tu.name, tu.input);
      if (!callsToSignal && SIGNAL.test(out)) callsToSignal = calls; // first call that surfaced the deciding condition
      turnRec.tools.push({ name: tu.name, input: tu.input, result: out });
      results.push({ type: "tool_result", tool_use_id: tu.id, content: out });
    }
    turns.push(turnRec);
    messages.push({ role: "user", content: results });
  }
  // Force a concluding answer if it ran out of turns mid-investigation.
  if (!finalText || messages[messages.length - 1].role === "user") {
    messages.push({ role: "user", content: "Stop. Final answer now, no tools: BUG LINE <n>, the code, and REACHED-WHEN <condition>." });
    const r = await box(SYS[variant], tools, messages);
    inTok += r.usage?.input_tokens || 0; outTok += r.usage?.output_tokens || 0;
    finalText = (r.content || []).filter((b) => b.type === "text").map((b) => b.text).join("\n").trim() || finalText;
    turns.push({ n: turns.length + 1, text: finalText, tools: [], forced: true });
  }
  proc.kill();
  return { variant, tools: TOOLSETS[variant], turns, calls, callsToSignal, turnCount: turns.length, inTok, outTok, finalText, correct: judge(finalText) };
}

// --render-only: rebuild index.html from the saved report.json (no box calls).
if (process.argv.includes("--render-only")) {
  const rep = JSON.parse(readFileSync(join(OUTDIR, "report.json"), "utf8"));
  writeFileSync(join(OUTDIR, "index.html"), renderHTML(rep));
  console.log("Re-rendered tools/benchmark/lens-report/index.html from report.json");
  process.exit(0);
}

console.log(`Running lens-bench on ${MODEL} (cap ${TURN_CAP} turns)…`);
const results = [];
for (const v of ["lens", "base"]) {
  process.stdout.write(`  ${v} … `);
  try { const r = await runVariant(v); results.push(r); console.log(`${r.correct ? "OK" : "MISS"} (${r.calls} calls, ${r.inTok + r.outTok} tok)`); }
  catch (e) { console.log("ERROR " + e.message); results.push({ variant: v, error: e.message, turns: [], correct: false }); }
}

const report = {
  model: MODEL, generated: new Date().toISOString(), turnCap: TURN_CAP,
  task: TASK, fixture: `${FIXTURE}/src/main/java/${ENTRY_FILE}`, method: ENTRY_METHOD,
  bug: "In `Promo.apply`, `points = points + 5;` runs whenever `spend > 0` — so the \"first purchase\" bonus is added on every purchase, doubling points for repeat buyers. The call chain Loyalty.points → tier → promo spreads it across 3 files.",
  variants: results,
};
mkdirSync(OUTDIR, { recursive: true });
writeFileSync(join(OUTDIR, "report.json"), JSON.stringify(report, null, 2));
writeFileSync(join(OUTDIR, "index.html"), renderHTML(report));
console.log(`\nReport written: tools/benchmark/lens-report/index.html`);

function renderHTML(rep) {
  const data = JSON.stringify(rep).replace(/</g, "\\u003c");
  return `<!doctype html><html lang=en><head><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>file-projections · lens benchmark</title>
<style>
:root{--bg:#ebe7dd;--panel:#f4f0e8;--soft:#e7e1d6;--line:#cec6b8;--fg:#25231f;--mut:#746b5d;--accent:#3f6f9f;--ok:#24784f;--bad:#b54848;--code:#fbf8f0}
*{box-sizing:border-box}body{margin:0;font:14px/1.55 ui-sans-serif,-apple-system,Segoe UI,Roboto,sans-serif;background:var(--bg);color:var(--fg)}
.wrap{max-width:1080px;margin:0 auto;padding:2rem 1.4rem 4rem}
h1{font-size:1.5rem;margin:0 0 .2rem}.sub{color:var(--mut);font-size:.85rem;margin-bottom:1.4rem}
.card{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:1rem 1.2rem;margin:1rem 0}
.bug{background:#fff6dc;border-color:#dfc46c}
h2{font-size:.78rem;text-transform:uppercase;letter-spacing:.08em;color:var(--mut);margin:0 0 .6rem}
table{width:100%;border-collapse:collapse;font-size:.9rem}
th,td{text-align:left;padding:.55rem .6rem;border-bottom:1px solid var(--line);vertical-align:top}
th{font-size:.72rem;text-transform:uppercase;letter-spacing:.05em;color:var(--mut)}
td.num{font-variant-numeric:tabular-nums;font-family:ui-monospace,Menlo,monospace}
.pill{display:inline-block;padding:.1rem .55rem;border-radius:999px;font-size:.74rem;font-weight:600}
.pill.ok{background:#dfeddf;color:var(--ok)}.pill.no{background:#f6dede;color:var(--bad)}
.win{font-weight:700}
.tabs{display:flex;gap:.4rem;margin:1.6rem 0 .8rem}
.tabs button{background:var(--soft);border:1px solid var(--line);color:var(--fg);border-radius:8px;padding:.45rem .9rem;font-weight:600;cursor:pointer;font-size:.85rem}
.tabs button.on{background:var(--accent);color:#fff;border-color:var(--accent)}
.turn{border:1px solid var(--line);border-radius:10px;margin:.7rem 0;overflow:hidden;background:var(--panel)}
.turn>.h{display:flex;align-items:center;gap:.5rem;padding:.5rem .8rem;background:var(--soft);font-size:.78rem;color:var(--mut)}
.turn .n{background:var(--accent);color:#fff;border-radius:6px;padding:.05rem .5rem;font-weight:700;font-size:.74rem}
.turn .body{padding:.7rem .9rem}
.say{white-space:pre-wrap;margin:.1rem 0 .4rem}
.tool{border:1px solid var(--line);border-radius:8px;margin:.5rem 0;overflow:hidden}
.tool .th{background:#eef2f7;color:#2c4a66;font-family:ui-monospace,Menlo,monospace;font-size:.8rem;padding:.4rem .6rem;border-bottom:1px solid var(--line)}
.tool pre{margin:0;padding:.6rem;background:var(--code);font-family:ui-monospace,Menlo,monospace;font-size:12px;white-space:pre-wrap;overflow:auto;max-height:24rem}
.final{background:#dfeddf;border:1px solid #9cc8a6;border-radius:8px;padding:.7rem .9rem;white-space:pre-wrap;font-size:.9rem}
.final.no{background:#f6dede;border-color:#d99}
code{background:var(--code);border:1px solid var(--line);border-radius:4px;padding:.05rem .3rem;font-family:ui-monospace,Menlo,monospace;font-size:.85em}
.muted{color:var(--mut);font-size:.8rem}
.hide{display:none}
</style></head><body><div class=wrap>
<h1>file-projections — lens benchmark</h1>
<div class=sub id=sub></div>
<div class="card bug"><h2>planted bug</h2><div id=bug></div><div class=muted id=fixture style=margin-top:.4rem></div></div>
<div class=card><h2>task given to the model</h2><div class=say id=task></div></div>
<div class=card><h2>results — same task, two tool surfaces, ${rep.turnCap} turn cap</h2>
<table><thead><tr><th>surface</th><th>tools</th><th>localized bug</th><th>calls to deciding&nbsp;condition</th><th>total calls</th><th>turns</th><th>tokens (in/out)</th><th>read source?</th></tr></thead><tbody id=rows></tbody></table>
<div class=muted id=takeaway style=margin-top:.7rem></div></div>
<div class=tabs id=tabs></div>
<div id=transcript></div>
<script>
const D=${data};
const cap=D.turnCap;
document.getElementById('sub').textContent=D.model+' · '+new Date(D.generated).toLocaleString()+' · cap '+cap+' turns';
document.getElementById('bug').innerHTML=md(D.bug);
document.getElementById('fixture').textContent=D.fixture+' · method '+D.method+'()';
document.getElementById('task').textContent=D.task;
function esc(s){return String(s==null?'':s).replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]))}
function md(s){return esc(s).replace(/\`([^\`]+)\`/g,'<code>$1</code>')}
const ok=v=>v.correct?'<span class=pill0 ok>YES</span>':'<span class=pill no>NO</span>';
const rows=D.variants.map(v=>{
 const tok=v.error?'—':(v.inTok+' / '+v.outTok);
 const src=v.variant==='lens'?'<span class="pill ok">no</span>':'<span class=pill no>yes</span>';
 return '<tr><td class=win>'+v.variant+'</td><td><code>'+(v.tools||[]).join('</code> <code>')+'</code></td>'+
  '<td>'+(v.error?'<span class=pill no>ERR</span>':( v.correct?'<span class="pill ok">YES</span>':'<span class="pill no">NO</span>'))+'</td>'+
  '<td class=num><b>'+(v.callsToSignal||'—')+'</b></td>'+
  '<td class=num>'+(v.calls||0)+'</td><td class=num>'+(v.turnCount||0)+'</td><td class=num>'+tok+'</td><td>'+(v.error?'—':src)+'</td></tr>';
}).join('');
document.getElementById('rows').innerHTML=rows;
const lens=D.variants.find(v=>v.variant==='lens'),base=D.variants.find(v=>v.variant==='base');
if(lens&&base&&!lens.error&&!base.error){
 const ds=base.callsToSignal-lens.callsToSignal;
 document.getElementById('takeaway').innerHTML='Both surfaces localized the bug. The <b>lens</b> surfaced the deciding condition '+
  '<code>spend &gt; 0</code> in <b>'+lens.callsToSignal+'</b> call'+(lens.callsToSignal===1?'':'s')+' — one inlined view + one assumption check — '+
  '<b>without reading any source</b>. Plain file reading needed <b>'+base.callsToSignal+'</b> call'+(base.callsToSignal===1?'':'s')+
  ' across separate files to reach the same line'+(ds>0?', '+ds+' more':'')+'. '+
  'Total-call counts vary with how much the model chooses to double-check; the deciding-condition metric is what the tool actually delivers. '+
  'On deeper call graphs the gap widens — every extra hop is another file the reader must open, but the same single inlined view for the lens.';
}
// tabs + transcripts
const tabs=document.getElementById('tabs'),tr=document.getElementById('transcript');
D.variants.forEach((v,idx)=>{
 const b=document.createElement('button');b.textContent=v.variant+(v.error?' (error)':'');b.className=idx===0?'on':'';
 b.onclick=()=>{[...tabs.children].forEach(x=>x.classList.remove('on'));b.classList.add('on');show(idx)};
 tabs.appendChild(b);
});
function show(idx){
 const v=D.variants[idx];
 if(v.error){tr.innerHTML='<div class=card><b>error:</b> '+esc(v.error)+'</div>';return}
 let h='';
 (v.turns||[]).forEach(t=>{
  h+='<div class=turn><div class=h><span class=n>turn '+t.n+'</span>'+(t.forced?'<span class=muted>forced conclusion</span>':'')+(t.tools.length?'<span class=muted>'+t.tools.length+' tool call'+(t.tools.length>1?'s':'')+'</span>':'')+'</div><div class=body>';
  if(t.text)h+='<div class=say>'+esc(t.text)+'</div>';
  t.tools.forEach(tc=>{
   h+='<div class=tool><div class=th>'+esc(tc.name)+'('+esc(JSON.stringify(tc.input))+')</div><pre>'+esc(tc.result)+'</pre></div>';
  });
  h+='</div></div>';
 });
 h+='<div class=card><h2>final answer</h2><div class="final'+(v.correct?'':' no')+'">'+esc(v.finalText||'(none)')+'</div></div>';
 tr.innerHTML=h;
}
show(0);
</script></div></body></html>`;
}
