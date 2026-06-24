#!/usr/bin/env node
// Live report for the harness-swap benchmark. Serves a page that tails
// tools/benchmark/harness/results.json and each run's JSONL transcript, so
// conversations + outcomes render as the runs happen.
//
//   node tools/harness-report.mjs            # http://localhost:7800
//   PORT=7801 node tools/harness-report.mjs

import { createServer } from "node:http";
import { readFileSync, existsSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const REPO = dirname(dirname(fileURLToPath(import.meta.url)));
const OUTDIR = join(REPO, "tools", "benchmark", "harness");
const PORT = Number(process.env.PORT || 7800);

const PAGE = `<!doctype html><meta charset=utf8><title>Harness swap benchmark</title>
<style>
:root{--bg:#0d1117;--fg:#e6edf3;--mut:#8b949e;--line:#30363d;--card:#161b22;--ok:#3fb950;--bad:#f85149;--amber:#d29922;--blue:#58a6ff;--code:#1e242c}
*{box-sizing:border-box}body{margin:0;font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;background:var(--bg);color:var(--fg)}
header{padding:14px 20px;border-bottom:1px solid var(--line);display:flex;align-items:center;gap:14px;position:sticky;top:0;background:var(--bg);z-index:5}
h1{font-size:15px;margin:0;font-weight:600}.sub{color:var(--mut);font-size:12px}
.wrap{padding:18px;display:grid;gap:18px;grid-template-columns:repeat(auto-fill,minmax(440px,1fr))}
.card{border:1px solid var(--line);border-radius:8px;background:var(--card);overflow:hidden;display:flex;flex-direction:column}
.chead{padding:10px 12px;border-bottom:1px solid var(--line);display:flex;flex-wrap:wrap;gap:8px;align-items:center}
.tag{font-size:11px;padding:2px 7px;border-radius:10px;border:1px solid var(--line);color:var(--mut)}
.tag.h{color:var(--blue);border-color:var(--blue)}
.badge{font-weight:700;font-size:12px;padding:2px 8px;border-radius:4px;margin-left:auto}
.pass{background:#11371f;color:var(--ok)}.fail{background:#3d1416;color:var(--bad)}.run{background:#3a2d09;color:var(--amber)}
.meta{padding:6px 12px;color:var(--mut);font-size:12px;border-bottom:1px solid var(--line);display:flex;flex-wrap:wrap;gap:10px}
.leak{color:var(--bad);font-weight:700}
.supplied b{color:var(--fg)}
.log{padding:8px 12px;max-height:340px;overflow:auto;display:flex;flex-direction:column;gap:6px}
.turn{font-size:12.5px}
.role{color:var(--mut);font-size:11px;text-transform:uppercase;letter-spacing:.04em}
.txt{white-space:pre-wrap;word-break:break-word}
.tool{border-left:2px solid var(--blue);padding:2px 0 2px 8px;margin:2px 0}
.tool .nm{color:var(--blue);font-weight:700}
.tres{border-left:2px solid var(--line);padding:2px 0 2px 8px;color:var(--mut);white-space:pre-wrap;max-height:120px;overflow:auto}
.args{color:var(--amber)}
.foot{padding:8px 12px;border-top:1px solid var(--line);font-size:12px;color:var(--mut)}
.detail{color:var(--bad)}
</style>
<header><h1>Harness-swap benchmark</h1><span class=sub id=sub>loading…</span><span class=sub style=margin-left:auto id=clock></span></header>
<div class=wrap id=wrap></div>
<script>
const esc=s=>String(s==null?"":s).replace(/[&<>]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;"}[c]));
function clip(s,n){s=String(s||"");return s.length>n?s.slice(0,n)+"…":s}

// parse claude stream-json OR codex --json into a common transcript
function transcript(lines){
  const out=[];
  for(const raw of lines){
    let e;try{e=JSON.parse(raw)}catch{continue}
    // claude
    if(e.type==="assistant"&&e.message?.content){
      for(const b of e.message.content){
        if(b.type==="text"&&b.text.trim())out.push({role:"assistant",kind:"text",text:b.text});
        if(b.type==="tool_use")out.push({role:"assistant",kind:"tool",name:(b.name||"").replace(/^mcp__proj__/,""),args:b.input});
      }
      continue;
    }
    if(e.type==="user"&&e.message?.content){
      for(const b of e.message.content){
        if(b.type==="tool_result"){
          const t=Array.isArray(b.content)?b.content.map(c=>c.text||"").join("\\n"):(b.content||"");
          out.push({role:"tool",kind:"result",text:t});
        }
      }
      continue;
    }
    // codex 0.142 exec --json: {type:"item.completed",item:{type,...}}
    if(e.type==="item.completed"){
      const it=e.item||{};
      if(it.type==="reasoning"&&it.text)out.push({role:"assistant",kind:"text",text:it.text});
      else if(it.type==="agent_message"&&it.text)out.push({role:"assistant",kind:"text",text:it.text});
      else if(it.type==="mcp_tool_call"){
        out.push({role:"assistant",kind:"tool",name:String(it.tool||it.name||"").replace(/^mcp__proj__/,"").replace(/^proj[._]/,""),args:it.arguments||it.invocation?.arguments});
        const r=it.result?.content?.map?.(c=>c.text).join("\\n")??it.result??it.output;
        if(r!=null)out.push({role:"tool",kind:"result",text:typeof r==="string"?r:JSON.stringify(r)});
      }
      else if(it.type==="command_execution"){
        out.push({role:"assistant",kind:"tool",name:"shell",args:{cmd:it.command||it.aggregated_output||""}});
        if(it.aggregated_output)out.push({role:"tool",kind:"result",text:String(it.aggregated_output)});
      }
      else if(it.type==="error"&&it.message)out.push({role:"tool",kind:"result",text:"⚠ "+it.message});
      continue;
    }
  }
  return out;
}

function renderTurn(t){
  if(t.kind==="text")return '<div class=turn><div class=role>'+t.role+'</div><div class=txt>'+esc(clip(t.text,1400))+'</div></div>';
  if(t.kind==="tool"){
    const a=t.args?'<span class=args>'+esc(clip(JSON.stringify(t.args),200))+'</span>':'';
    return '<div class="turn tool"><span class=nm>'+esc(t.name)+'</span> '+a+'</div>';
  }
  if(t.kind==="result")return '<div class=tres>'+esc(clip(t.text,500))+'</div>';
  return '';
}

async function tick(){
  let res;try{res=await (await fetch("results.json?_="+Date.now())).json()}catch{res={runs:[]}}
  const runs=(res.runs||[]).sort((a,b)=>(a.id<b.id?-1:1));
  document.getElementById("sub").textContent=runs.length+" runs";
  document.getElementById("clock").textContent=new Date().toLocaleTimeString();
  const wrap=document.getElementById("wrap");
  const cards=await Promise.all(runs.map(async r=>{
    let lines=[];
    try{const t=await (await fetch(r.jsonl+"?_="+Date.now())).text();lines=t.split("\\n").filter(Boolean)}catch{}
    const tr=transcript(lines);
    const st=r.status==="running"?'<span class="badge run">RUNNING</span>':
      (r.gradle?.pass?'<span class="badge pass">PASS</span>':'<span class="badge fail">FAIL</span>');
    const leak=r.graph_leak?'<span class=leak>⚠ GRAPH LEAK</span>':'';
    const used=(r.tools_used||[]).join(", ")||"—";
    return '<div class=card><div class=chead>'+
      '<span class="tag h">'+esc(r.harness)+'</span>'+
      '<span class=tag>'+esc(r.variant)+'</span>'+
      '<span class=tag>'+esc(r.model)+'</span>'+st+'</div>'+
      '<div class=meta supplied>supplied: <b>'+esc((r.tools_supplied||[]).join(", "))+'</b></div>'+
      '<div class=meta>used: '+esc(used)+' '+leak+'</div>'+
      '<div class=log>'+tr.map(renderTurn).join("")+'</div>'+
      '<div class=foot>calls '+(r.tool_calls?.length||tr.filter(t=>t.kind==="tool").length)+
        ' · tokens '+(r.tokens||0).toLocaleString()+(r.seconds?' · '+r.seconds+'s':'')+
        (r.gradle&&!r.gradle.pass&&r.gradle.detail?'<div class=detail>'+esc(clip(r.gradle.detail,200))+'</div>':'')+'</div></div>';
  }));
  wrap.innerHTML=cards.join("");
}
tick();setInterval(tick,1500);
</script>`;

const server = createServer((req, res) => {
	const url = (req.url || "/").split("?")[0];
	if (url === "/" || url === "/index.html") {
		res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
		return res.end(PAGE);
	}
	if (url === "/results.json") {
		const p = join(OUTDIR, "results.json");
		res.writeHead(200, { "content-type": "application/json", "cache-control": "no-store" });
		return res.end(existsSync(p) ? readFileSync(p) : '{"runs":[]}');
	}
	if (url.startsWith("/harness/") && url.endsWith(".jsonl")) {
		const p = join(OUTDIR, url.replace("/harness/", ""));
		if (!existsSync(p)) { res.writeHead(404); return res.end(""); }
		res.writeHead(200, { "content-type": "text/plain", "cache-control": "no-store" });
		return res.end(readFileSync(p));
	}
	res.writeHead(404); res.end("not found");
});
server.listen(PORT, () => process.stderr.write(`harness report: http://localhost:${PORT}\n`));
