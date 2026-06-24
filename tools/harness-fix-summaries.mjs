#!/usr/bin/env node
// Recompute per-run summary fields (tool_calls, tools_used, tokens, final,
// graph_leak) directly from the captured JSONL transcripts. Ground-truth gradle
// status is left untouched (the orchestrator computes it independently). Use when
// a run was produced by an orchestrator build with an older event parser.
//
//   node tools/harness-fix-summaries.mjs

import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const REPO = dirname(dirname(fileURLToPath(import.meta.url)));
const OUTDIR = join(REPO, "tools", "benchmark", "harness");
const GRAPH_TOOLS = ["semantic_search_nodes", "query_graph", "traverse_graph", "get_architecture_overview", "list_flows"];

function parseCodex(events) {
	const calls = []; let tokens = 0, final = "";
	for (const e of events) {
		if (e.type === "item.completed") {
			const it = e.item || {};
			if (it.type === "mcp_tool_call") calls.push(String(it.tool || it.name || "").replace(/^mcp__proj__/, "").replace(/^proj[._]/, ""));
			else if (it.type === "command_execution") calls.push("shell");
			else if (it.type === "agent_message" && it.text) final = it.text;
		}
		if (e.type === "turn.completed" && e.usage) tokens += (e.usage.input_tokens || 0) + (e.usage.output_tokens || 0);
	}
	return { calls, tokens, final };
}
function parseClaude(events) {
	const calls = []; let inT = 0, outT = 0, final = "";
	for (const e of events) {
		const blocks = e?.message?.content;
		if (Array.isArray(blocks)) for (const b of blocks) if (b.type === "tool_use") calls.push((b.name || "").replace(/^mcp__proj__/, ""));
		const u = e?.message?.usage;
		if (u) { inT += (u.input_tokens || 0) + (u.cache_read_input_tokens || 0) + (u.cache_creation_input_tokens || 0); outT += u.output_tokens || 0; }
		if (e.type === "result") { if (e.usage) { inT = e.usage.input_tokens || inT; outT = e.usage.output_tokens || outT; } final = e.result || final; }
	}
	return { calls, tokens: inT + outT, final };
}

const RESULTS = join(OUTDIR, "results.json");
const data = JSON.parse(readFileSync(RESULTS, "utf8"));
for (const r of data.runs) {
	const p = join(OUTDIR, (r.jsonl || `harness/${r.id}.jsonl`).replace(/^harness\//, ""));
	if (!existsSync(p)) continue;
	const events = readFileSync(p, "utf8").split("\n").filter((l) => l.trim().startsWith("{")).map((l) => { try { return JSON.parse(l); } catch { return null; } }).filter(Boolean);
	const parsed = r.harness === "codex" ? parseCodex(events) : parseClaude(events);
	const used = [...new Set(parsed.calls)];
	r.tool_calls = parsed.calls;
	r.tools_used = used;
	r.tokens = parsed.tokens;
	r.final = (parsed.final || "").slice(0, 400);
	r.graph_leak = used.some((t) => GRAPH_TOOLS.includes(t)) && r.variant !== "graph";
	process.stderr.write(`${r.id}: calls=${parsed.calls.length} used=[${used.join(",")}] tokens=${parsed.tokens} leak=${r.graph_leak} gradle=${r.gradle?.pass ? "PASS" : "FAIL"}\n`);
}
writeFileSync(RESULTS, JSON.stringify(data, null, 2));
process.stderr.write(`fixed ${data.runs.length} runs in ${RESULTS}\n`);
