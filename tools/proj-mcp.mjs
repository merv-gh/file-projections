#!/usr/bin/env node
// Standalone stdio MCP server exposing the benchmark's variant-separated tools.
// One source of truth shared by BOTH harnesses (codex + claude-code) so the
// prompt and tools are supplied identically; only the exposed toolset differs.
//
//   BENCH_VARIANT=base  -> read_file, edit_lines, run_tests
//   BENCH_VARIANT=graph -> code-review-graph tools + read_file, edit_lines, run_tests
//   BENCH_VARIANT=proj  -> view_program, edit_program, run_tests
//
// Protocol: newline-delimited JSON-RPC 2.0 (MCP 2024-11-05 stdio transport).
// Methods handled: initialize, notifications/initialized, tools/list, tools/call.
//
// Env: BENCH_CWD/PROJ_CWD (fixture root), BENCH_FPBIN, BENCH_SRC, BENCH_ENTRY_FILE,
// BENCH_ENTRY_METHOD, BENCH_GRADLE_IMG, BENCH_CRG/CRG.

import { spawn, execFile } from "node:child_process";
import { createInterface } from "node:readline";
import { promisify } from "node:util";
import {
	existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync, statSync,
} from "node:fs";
import { basename, join } from "node:path";

const run = promisify(execFile);

const VARIANT = process.env.BENCH_VARIANT || "proj";
const FPBIN = process.env.BENCH_FPBIN || process.env.PROJ_FPBIN || "file-projections";
const SRC = process.env.BENCH_SRC || process.env.PROJ_SRC || "src/main/java";
const ENTRY_FILE = process.env.BENCH_ENTRY_FILE || process.env.PROJ_ENTRY_FILE || "sample/App.java";
const ENTRY_METHOD = process.env.BENCH_ENTRY_METHOD || process.env.PROJ_ENTRY_METHOD || "summary";
const GRADLE_IMG = process.env.BENCH_GRADLE_IMG || process.env.PROJ_GRADLE_IMG || "gradle:8.5-jdk17";
const CRG = process.env.BENCH_CRG || process.env.CRG || `${process.env.HOME}/.local/bin/code-review-graph`;
const ROOT = process.env.BENCH_CWD || process.env.PROJ_CWD || process.cwd();
const CFG = "fp.config.json";
const PROJ = ".projections/unrolled.projection";

let last = null;   // latest concrete (inputs-decided) listing
let shown = null;  // latest listing shown (may be branch view)

// ---- helpers ------------------------------------------------------------

function safeRel(p) {
	const s = String(p || "").replace(/\\/g, "/").replace(/^\/+/, "");
	if (!s || s.includes("..")) return "";
	return s;
}
function skipDir(name) {
	return [".git", ".gradle", ".code-review-graph", ".projections", "build", "node_modules"].includes(name);
}
function findFile(root, requested) {
	const rel = safeRel(requested);
	for (const t of [rel, join(SRC, rel), join("src/test/java", rel)]) {
		const p = join(root, t);
		if (existsSync(p) && statSync(p).isFile()) return p;
	}
	const base = basename(rel);
	if (!base) return "";
	const stack = [root];
	while (stack.length) {
		const dir = stack.pop();
		for (const ent of readdirSync(dir, { withFileTypes: true })) {
			const p = join(dir, ent.name);
			if (ent.isDirectory()) { if (!skipDir(ent.name)) stack.push(p); }
			else if (ent.name === base) return p;
		}
	}
	return "";
}
const relSourcePath = (root, full) => full.replace(root + "/", "");
const readLines = (p) => readFileSync(p, "utf8").split("\n");
function xmlUnescape(s) {
	return String(s || "").replace(/&lt;/g, "<").replace(/&gt;/g, ">")
		.replace(/&quot;/g, '"').replace(/&apos;/g, "'").replace(/&amp;/g, "&");
}
function junitFailure(root) {
	const dir = join(root, "build", "test-results", "test");
	if (!existsSync(dir)) return "";
	for (const f of readdirSync(dir)) {
		if (!f.endsWith(".xml")) continue;
		const xml = readFileSync(join(dir, f), "utf8");
		const name = (xml.match(/<testcase[^>]*name="([^"]+)"/) || [])[1] || "test";
		const msg = (xml.match(/<failure[^>]*message="([^"]+)"/) || [])[1];
		if (msg) return `${name}: ${xmlUnescape(msg)}`;
	}
	return "";
}
function ensureConfig(root) {
	const p = join(root, CFG);
	if (!existsSync(p)) writeFileSync(p, JSON.stringify({ root: ".", projections_dir: ".projections" }));
}
function parseInputs(s) {
	const env = {}, invalid = [];
	for (const part of String(s || "").split(",")) {
		const raw = part.trim();
		if (!raw) continue;
		const [k, v] = raw.split("=");
		if (k && k.trim() && v !== undefined) {
			const key = k.trim(), val = v.trim();
			env[key] = /^-?\d+$/.test(val) ? Number(val) : val;
		} else invalid.push(raw);
	}
	return { env, invalid };
}
function entryParamNames(root) {
	const src = readFileSync(join(root, SRC, ENTRY_FILE), "utf8");
	const m = src.match(new RegExp(`\\b${ENTRY_METHOD}\\s*\\(([^)]*)\\)`));
	if (!m) return [];
	return m[1].split(",").map((p) => p.trim().split(/\s+/).pop()).filter(Boolean);
}
function parseProjection(text) {
	const body = [], origins = {}, facts = [], guards = {};
	let inBlock = false;
	for (const l of text.split("\n")) {
		if (l.startsWith("@@ ")) { inBlock = true; continue; }
		if (l === "@@") { inBlock = false; continue; }
		if (inBlock) body.push(l);
		const m = l.match(/^=> \S+: origin (\d+) src=(.+):(\d+) srchash=/);
		if (m) origins[Number(m[1])] = [m[2], Number(m[3])];
		const f = l.match(/^=> unrolled-program\.branch-\d+: (.+)$/);
		if (f) facts.push(f[1]);
		const g = l.match(/^=> unrolled-program\.lguard-(\d+): (.+)$/);
		if (g) guards[Number(g[1])] = g[2];
	}
	return { body, origins, facts, guards };
}
async function genUnrolled(root, inputs, inline) {
	ensureConfig(root);
	mkdirSync(join(root, ".projections"), { recursive: true });
	const args = ["-config", CFG, "-analyzer", "unrolled-program", "-source-root", SRC,
		"-file", ENTRY_FILE, "-method", ENTRY_METHOD, "-out", PROJ];
	if (inputs && Object.keys(inputs).length) {
		args.push("-inputs", Object.entries(inputs).map(([k, v]) => `${k}=${v}`).join(","));
	}
	if (inline != null && inline !== "") args.push("-inline_depth", String(inline));
	await run(FPBIN, args, { cwd: root, maxBuffer: 16 * 1024 * 1024 });
	const r = parseProjection(readFileSync(join(root, PROJ), "utf8"));
	r._inputs = inputs || {};
	return r;
}
function listing(r) {
	const rows = r.body.map((c, i) => {
		const o = r.origins[i + 1];
		const where = o ? `${o[0].split("/").pop()}:${o[1]}` : "?";
		return `${i + 1} ${where} | ${c.trim()}`;
	});
	const branches = rows.filter((l) => /\|\s*(if|else if|switch|while|for|case)\b/.test(l));
	if (branches.length && !Object.keys(r._inputs || {}).length) {
		return "BRANCHES undecided; choose inputs from the failing call, then call view_program again.\n" + rows.join("\n");
	}
	if (branches.length) {
		return "BRANCHES runtime-dependent; both sides remain visible.\n" + rows.join("\n");
	}
	const inputText = Object.keys(r._inputs || {}).length
		? "PATH " + Object.entries(r._inputs).map(([k, v]) => `${k}=${v}`).join(",") + "\n" : "";
	const decisions = (r.facts || []).length ? "DECISIONS " + r.facts.join("; ") + "\n" : "";
	return inputText + decisions + rows.join("\n");
}
function mutationChoices(r) {
	return r.body.map((c, i) => ({ n: i + 1, code: c.trim(), origin: r.origins[i + 1] }))
		.filter((x) => /\.[A-Za-z_]*set[A-Za-z0-9_]*\s*\(|\bset[A-Za-z0-9_]*\s*\(/.test(x.code))
		.map((x) => `${x.n} ${x.origin ? x.origin[0].split("/").pop() + ":" + x.origin[1] : "?"} | ${x.code}`)
		.join("\n");
}
function cleanNewCode(s) {
	let code = String(s || "").trim();
	const pipe = code.indexOf("|");
	if (pipe >= 0 && /^[^|]+\.(java|go|js|ts):\d+\s*$/.test(code.slice(0, pipe).trim())) {
		code = code.slice(pipe + 1).trim();
	}
	return code;
}

// ---- code-review-graph stdio client (graph variant) ---------------------

class CRGMCP {
	constructor(repoRoot) {
		this.id = 0; this.pending = new Map();
		this.proc = spawn(CRG, ["serve", "--repo", repoRoot], { stdio: ["pipe", "pipe", "ignore"] });
		const rl = createInterface({ input: this.proc.stdout });
		rl.on("line", (line) => {
			if (!line.startsWith("{")) return;
			let msg; try { msg = JSON.parse(line); } catch { return; }
			if (msg.id && this.pending.has(msg.id)) {
				const { resolve } = this.pending.get(msg.id);
				this.pending.delete(msg.id); resolve(msg);
			}
		});
	}
	send(msg) { this.proc.stdin.write(JSON.stringify(msg) + "\n"); }
	request(method, params) {
		const id = ++this.id;
		this.send({ jsonrpc: "2.0", id, method, params });
		return new Promise((resolve, reject) => {
			const t = setTimeout(() => { this.pending.delete(id); reject(new Error(`${method} timed out`)); }, 120000);
			this.pending.set(id, { resolve: (v) => { clearTimeout(t); resolve(v); }, reject });
		});
	}
	async init() {
		await this.request("initialize", { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "fp-bench", version: "1" } });
		this.send({ jsonrpc: "2.0", method: "notifications/initialized" });
	}
	async call(name, args) {
		const r = await this.request("tools/call", { name, arguments: args || {} });
		if (r.error) return JSON.stringify(r.error);
		const content = r.result?.content || [];
		return content.map((c) => c.text || JSON.stringify(c)).join("\n") || JSON.stringify(r.result || {});
	}
	close() { try { this.proc.kill(); } catch {} }
}
async function graphCall(root, tool, args) {
	const c = new CRGMCP(root);
	try {
		await c.init();
		const text = await c.call(tool, { ...(args || {}), repo_root: root });
		return text.length > 6000 ? text.slice(0, 6000) + "\n...[truncated]" : text;
	} finally { c.close(); }
}

// ---- tool implementations (return string) -------------------------------

async function tRead(p) {
	const full = findFile(ROOT, p.path);
	if (!full) return `no such file: ${p.path}`;
	const lines = readLines(full);
	const start = Math.max(1, Number(p.start || 1));
	const end = Math.min(lines.length, Number(p.end || lines.length));
	return `${relSourcePath(ROOT, full)}\n` + lines.slice(start - 1, end).map((l, i) => `${start + i}: ${l}`).join("\n");
}
async function tEditLines(p) {
	const full = findFile(ROOT, p.path);
	if (!full) return `no such file: ${p.path}`;
	const lines = readLines(full);
	const start = Number(p.start), end = Number(p.end || p.start);
	if (start < 1 || end < start || end > lines.length) return `bad range ${start}-${end}; file has ${lines.length} lines`;
	writeFileSync(full, lines.slice(0, start - 1).concat(String(p.new_text).split("\n"), lines.slice(end)).join("\n"));
	return `OK replaced ${relSourcePath(ROOT, full)}:${start}-${end}`;
}
async function tRunTests() {
	try {
		rmSync(join(ROOT, "build", "test-results"), { recursive: true, force: true });
		await run("docker", ["run", "--rm", "-v", `${ROOT}:/app`, "-w", "/app",
			"-v", "grade-gradle-cache:/home/gradle/.gradle", GRADLE_IMG, "gradle", "test", "--console=plain"],
			{ maxBuffer: 16 * 1024 * 1024 });
		return "TESTS PASS\nThe task is complete; stop now.";
	} catch (e) {
		const out = (e.stdout?.toString() || "") + (e.stderr?.toString() || "");
		const junit = junitFailure(ROOT);
		if (junit) return "TESTS FAIL\n" + junit;
		const m = out.match(/expected:.*$/m);
		return "TESTS FAIL" + (m ? "\n" + m[0] : "\n" + out.trim().split("\n").slice(-4).join("\n"));
	}
}
// lineWritesJS: which variable a line writes (assign / += / ++ / setter) — mirrors
// the UI's mutation detection so line_assumptions can report a write timeline.
function lineWritesJS(t) {
	let m;
	if ((m = t.match(/^([A-Za-z_]\w*)\s*(?:\+\+|--)\s*;?\s*$/)) || (m = t.match(/^(?:\+\+|--)\s*([A-Za-z_]\w*)/))) return [m[1]];
	if ((m = t.match(/\b([A-Za-z_]\w*)\.(set[A-Z]\w*)\s*\(/))) return [m[1]];
	if ((m = t.match(/^(?:final\s+)?(?:[A-Za-z_][\w<>\[\].]*\s+)?([A-Za-z_]\w*)\s*(?:[-+*/|&^]?=)[^=]/))) {
		if (!/^(if|for|while|return|else|switch|case|do)$/.test(m[1])) return [m[1]];
	}
	return [];
}
async function tViewProgram(p) {
	const parsed = parseInputs(p.inputs);
	const allowed = entryParamNames(ROOT);
	const badKeys = Object.keys(parsed.env).filter((k) => allowed.length && !allowed.includes(k));
	if (parsed.invalid.length || badKeys.length) {
		const hint = `Invalid inputs: ${[...parsed.invalid, ...badKeys].join(", ")}. Use only ${allowed.join(", ") || "method argument"} as k=v, or pass empty inputs to discover branches.`;
		const r = shown || last || await genUnrolled(ROOT, {});
		return hint + "\n" + listing(r);
	}
	const r = await genUnrolled(ROOT, parsed.env, p.inline);
	shown = r;
	if (Object.keys(parsed.env).length) last = r;
	return listing(r);
}
// line_assumptions: for one numbered line of the latest view_program output, return
// the conditions that must be true to reach it (its guard set) plus the variable
// names that feed it — so the model can ask "why does this line run, with what?".
async function tLineAssumptions(p) {
	const r = shown || last || await genUnrolled(ROOT, {});
	const n = Number(p.line);
	if (!(n >= 1 && n <= r.body.length)) return `line must be 1..${r.body.length}; call view_program first.`;
	const code = r.body[n - 1].trim();
	const o = r.origins[n];
	const where = o ? `${o[0].split("/").pop()}:${o[1]}` : "?";
	const guard = r.guards[n];
	const bare = code.replace(/"[^"]*"/g, "").replace(/'[^']*'/g, "");
	const vars = [...new Set((bare.match(/[A-Za-z_][A-Za-z0-9_]*/g) || [])
		.filter((w) => !/^(if|else|return|new|int|long|double|float|boolean|String|void|true|false|null|this|for|while|switch|case|break|continue|throw|try|catch|finally)$/.test(w)))];
	const written = lineWritesJS(code.trim());
	let writeTimeline = "";
	if (written.length) {
		const v = written[0];
		const sites = r.body.map((c, i) => ({ i: i + 1, c: c.trim() }))
			.filter((x) => lineWritesJS(x.c).includes(v));
		writeTimeline = `\nWRITES ${v} (this is write ${sites.findIndex((s) => s.i === n) + 1} of ${sites.length} on this path: ` +
			sites.map((s) => `L${s.i}`).join(", ") + ")";
	}
	return `LINE ${n} ${where} | ${code}\n` +
		`REACHED-WHEN ${guard || "(always — no branch guards on this line)"}\n` +
		`VALUES-IN ${vars.join(", ") || "(none)"} — trace any of these with view_program at higher inline depth.` +
		writeTimeline;
}
async function tEditProgram(p) {
	if (!last) return 'Call view_program with concrete inputs first, e.g. inputs="coupon=save,amount=50".';
	const n = p.line;
	if (!(n >= 1 && n <= last.body.length)) return `line must be 1..${last.body.length}. Editable mutation lines:\n${mutationChoices(last)}`;
	const orig = last.body[n - 1];
	const cleaned = cleanNewCode(p.new_code);
	if (/\breturn\b|^\s*(?:String|int|long|boolean|double|float|Receipt)\s+\w+/.test(orig) && /\bset[A-Za-z0-9_]*\s*\(/.test(cleaned)) {
		return `Refusing to replace non-mutation line ${n}: ${orig.trim()}\nEditable mutation lines:\n${mutationChoices(last)}`;
	}
	if (/\bset[A-Za-z0-9_]*\s*\(/.test(orig) && !/\bset[A-Za-z0-9_]*\s*\(/.test(cleaned)) {
		return `Refusing to replace mutation line ${n} with non-mutation code: ${cleaned}\nEditable mutation lines:\n${mutationChoices(last)}`;
	}
	const indent = orig.slice(0, orig.length - orig.trimStart().length);
	last.body[n - 1] = indent + cleaned;
	const path = join(ROOT, PROJ);
	const lines = readFileSync(path, "utf8").split("\n");
	const bi = lines.findIndex((l) => l.startsWith("@@ "));
	last.body.forEach((c, off) => { lines[bi + 1 + off] = c; });
	writeFileSync(path, lines.join("\n"));
	await run(FPBIN, ["sync", "-config", CFG, PROJ], { cwd: ROOT });
	const dst = last.origins[n] || ["?", 0];
	last = await genUnrolled(ROOT, last._inputs || {});
	shown = last;
	return `OK line ${n} -> ${dst[0].split("/").pop()}:${dst[1]} | ${last.body[n - 1].trim()}`;
}

// ---- tool registry per variant ------------------------------------------

const S = (props, required) => ({ type: "object", properties: props, required: required || [], additionalProperties: false });
const num = (d) => ({ type: "number", description: d });
const str = (d) => ({ type: "string", description: d });

const READ = {
	name: "read_file",
	description: "Read a source file by relative path or basename. Optional start/end line range.",
	inputSchema: S({ path: str("Relative path or basename, e.g. App.java"), start: num("1-based start line"), end: num("1-based end line") }, ["path"]),
	run: tRead,
};
const EDIT_LINES = {
	name: "edit_lines",
	description: "Replace a 1-based inclusive line range in a source file. For one line, start=end.",
	inputSchema: S({ path: str("Relative path or basename"), start: num("1-based start line"), end: num("1-based end line; defaults to start"), new_text: str("Replacement text") }, ["path", "start", "new_text"]),
	run: tEditLines,
};
const RUN_TESTS = {
	name: "run_tests",
	description: "Compile and run the project's JUnit tests; returns PASS, or FAIL with the failing assertion's expected vs actual.",
	inputSchema: S({}, []),
	run: tRunTests,
};
const VIEW = {
	name: "view_program",
	description: "Render the failing method as ONE numbered straight-line program with every cross-file call inlined. Optional inputs is comma-separated method args only, e.g. \"coupon=save,amount=50\"; with no inputs, branch conditions stay visible.",
	inputSchema: S({ inputs: str("Comma-separated arg=value pairs, or omit to reveal branches.") }, []),
	run: tViewProgram,
};
const VIEW_INLINE = {
	name: "view_program",
	description: "Show the method as ONE numbered straight-line program with cross-file calls inlined. Args: inputs = comma-separated method args (e.g. \"amount=50,coupon=save\") to pick a concrete path, or omit to reveal branch conditions; inline = how many levels of called methods to expand inline (0 = none, 2 = expand calls and their calls). Each row is: <n> <file:line> | <code>.",
	inputSchema: S({ inputs: str("Comma-separated arg=value pairs, or omit to reveal branches."), inline: num("Levels of called methods to expand inline, e.g. 2. Omit for default.") }, []),
	run: tViewProgram,
};
const LINE_ASSUME = {
	name: "line_assumptions",
	description: "Explain ONE line of the latest view_program output: returns the exact conditions that must be true for execution to reach that line (its branch/guard assumptions), the file:line it came from, and the variable names feeding it. Use this on a suspicious line (a return, or a setter like counter=...) to learn WHY it runs and WHAT values drive it, instead of re-reading source. Arg: line = the row number from view_program.",
	inputSchema: S({ line: num("Row number from the latest view_program output.") }, ["line"]),
	run: tLineAssumptions,
};
const EDIT_PROGRAM = {
	name: "edit_program",
	description: "Replace line N of the latest concrete view_program listing; sync writes it to the real source origin.",
	inputSchema: S({ line: num("Line number from latest view_program output."), new_code: str("Corrected code for that line.") }, ["line", "new_code"]),
	run: tEditProgram,
};

function graphTool(name, mcpName, description, schema, mapArgs = (p) => p) {
	return { name, description, inputSchema: schema, run: async (p) => graphCall(ROOT, mcpName, mapArgs(p)) };
}
const GRAPH_TOOLS = [
	graphTool("semantic_search_nodes", "semantic_search_nodes_tool", "Search code-review-graph nodes by symbol/name/keyword.",
		S({ query: str("symbol or keyword"), kind: str("optional node kind"), limit: num("max results") }, ["query"]),
		(p) => ({ query: p.query, kind: p.kind || null, limit: p.limit || 20, detail_level: "minimal" })),
	graphTool("query_graph", "query_graph_tool", "Run a graph relationship query such as callers_of, callees_of, children_of, file_summary, or tests_for.",
		S({ pattern: str("e.g. callers_of"), target: str("symbol or file") }, ["pattern", "target"]),
		(p) => ({ pattern: p.pattern, target: p.target, detail_level: "minimal" })),
	graphTool("traverse_graph", "traverse_graph_tool", "Traverse outward from a matching code node with a bounded token budget.",
		S({ query: str("start symbol"), depth: num("hops"), mode: str("traversal mode"), token_budget: num("budget") }, ["query"])),
	graphTool("get_architecture_overview", "get_architecture_overview_tool", "Summarize code-review-graph communities and coupling.", S({}, [])),
	graphTool("list_flows", "list_flows_tool", "List execution flows sorted by criticality.", S({ limit: num("max"), sort_by: str("sort key") }, [])),
];

function toolsForVariant() {
	if (VARIANT === "base") return [READ, EDIT_LINES, RUN_TESTS];
	if (VARIANT === "graph") return [...GRAPH_TOOLS, READ, EDIT_LINES, RUN_TESTS];
	if (VARIANT === "proj") return [VIEW, EDIT_PROGRAM, RUN_TESTS];
	// "lens" = the minimal future-editing surface: expandable straight-line view +
	// per-line assumptions + edit + tests. The two-tool core for the qwen validation.
	if (VARIANT === "lens") return [VIEW_INLINE, LINE_ASSUME, EDIT_PROGRAM, RUN_TESTS];
	throw new Error(`unknown BENCH_VARIANT=${VARIANT}`);
}
const TOOLS = toolsForVariant();
const BY_NAME = new Map(TOOLS.map((t) => [t.name, t]));

// ---- JSON-RPC stdio loop ------------------------------------------------

function send(msg) { process.stdout.write(JSON.stringify(msg) + "\n"); }

async function handle(msg) {
	const { id, method, params } = msg;
	if (method === "initialize") {
		send({ jsonrpc: "2.0", id, result: {
			protocolVersion: "2024-11-05",
			capabilities: { tools: {} },
			serverInfo: { name: `proj-bench-${VARIANT}`, version: "1.0.0" },
		} });
		return;
	}
	if (method === "notifications/initialized" || (method && method.startsWith("notifications/"))) return;
	if (method === "tools/list") {
		send({ jsonrpc: "2.0", id, result: { tools: TOOLS.map((t) => ({ name: t.name, description: t.description, inputSchema: t.inputSchema })) } });
		return;
	}
	if (method === "tools/call") {
		const t = BY_NAME.get(params?.name);
		if (!t) { send({ jsonrpc: "2.0", id, error: { code: -32601, message: `unknown tool ${params?.name}` } }); return; }
		try {
			const text = await t.run(params.arguments || {});
			send({ jsonrpc: "2.0", id, result: { content: [{ type: "text", text: String(text) }] } });
		} catch (e) {
			send({ jsonrpc: "2.0", id, result: { content: [{ type: "text", text: `${t.name} failed: ${e.stderr?.toString() || e.message}` }], isError: true } });
		}
		return;
	}
	if (id !== undefined) send({ jsonrpc: "2.0", id, error: { code: -32601, message: `method not found: ${method}` } });
}

const rl = createInterface({ input: process.stdin });
rl.on("line", (line) => {
	const s = line.trim();
	if (!s.startsWith("{")) return;
	let msg; try { msg = JSON.parse(s); } catch { return; }
	handle(msg).catch((e) => { if (msg.id !== undefined) send({ jsonrpc: "2.0", id: msg.id, error: { code: -32603, message: e.message } }); });
});
process.stderr.write(`proj-mcp ready: variant=${VARIANT} tools=${TOOLS.map((t) => t.name).join(",")} root=${ROOT}\n`);
