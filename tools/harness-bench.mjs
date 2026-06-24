#!/usr/bin/env node
// Harness-swap benchmark: run the SAME task + SAME variant-separated MCP tools
// under two real coding harnesses (codex, claude-code) and report honestly.
//
//   HARNESS=claude|codex  VARIANTS=base,graph,proj  [MODEL=...] [BOX=http://host:11434/v1]
//   node tools/harness-bench.mjs
//
// Each run:
//  - copies the fixture to a clean per-run workdir (no cross-run contamination),
//  - exposes ONLY the variant's tools via tools/proj-mcp.mjs (graph tools never
//    reach base/proj),
//  - streams the harness's own JSONL events to tools/benchmark/harness/<run>.jsonl
//    (live; the report server tails it),
//  - after the agent stops, runs Gradle/JUnit independently for ground-truth PASS/FAIL,
//  - records a summary (tools_supplied, tools_used, unexpected/graph-leak, turns, status)
//    into tools/benchmark/harness/results.json.

import { spawn, execFile } from "node:child_process";
import { promisify } from "node:util";
import { cpSync, rmSync, mkdirSync, writeFileSync, readFileSync, existsSync, appendFileSync, createWriteStream } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { createInterface } from "node:readline";

const run = promisify(execFile);
const REPO = dirname(dirname(fileURLToPath(import.meta.url)));
const BIN = process.env.BENCH_FPBIN || join(REPO, "bin", "file-projections");
const FIXTURE = process.env.BENCH_FIXTURE || join(REPO, "fixtures", "objflow-sample");
const TASK = readFileSync(join(REPO, "tools", "proj-task.txt"), "utf8").trim();
const OUTDIR = join(REPO, "tools", "benchmark", "harness");
const GRADLE_IMG = process.env.BENCH_GRADLE_IMG || "gradle:8.5-jdk17";
const BOX = process.env.BOX || "http://192.168.1.148:11434/v1";

const HARNESS = process.env.HARNESS || "claude";
const VARIANTS = (process.env.VARIANTS || "base,graph,proj").split(",").map((s) => s.trim()).filter(Boolean);
const MODEL = process.env.MODEL || (HARNESS === "codex" ? "qwen3-coder:latest" : "");
// Set CLAUDE_BASE_URL to an ollama host to drive claude-code against a local model
// (e.g. http://localhost:11434); leave unset to use the default Anthropic endpoint.
const CLAUDE_BASE_URL = process.env.CLAUDE_BASE_URL || "";

const GRAPH_TOOLS = ["semantic_search_nodes", "query_graph", "traverse_graph", "get_architecture_overview", "list_flows"];
const VARIANT_TOOLS = {
	base: ["read_file", "edit_lines", "run_tests"],
	graph: [...GRAPH_TOOLS, "read_file", "edit_lines", "run_tests"],
	proj: ["view_program", "edit_program", "run_tests"],
};

mkdirSync(OUTDIR, { recursive: true });
const RESULTS = join(OUTDIR, "results.json");
function loadResults() { try { return JSON.parse(readFileSync(RESULTS, "utf8")); } catch { return { runs: [] }; } }
function saveResults(r) { writeFileSync(RESULTS, JSON.stringify(r, null, 2)); }

function freshWorkdir(tag) {
	const wd = join("/tmp", "fp-harness", tag);
	rmSync(wd, { recursive: true, force: true });
	mkdirSync(dirname(wd), { recursive: true });
	cpSync(FIXTURE, wd, { recursive: true });
	for (const junk of [".projections", "build", ".gradle", ".code-review-graph"]) {
		rmSync(join(wd, junk), { recursive: true, force: true });
	}
	return wd;
}

async function gradleStatus(wd) {
	try {
		rmSync(join(wd, "build", "test-results"), { recursive: true, force: true });
		await run("docker", ["run", "--rm", "-v", `${wd}:/app`, "-w", "/app",
			"-v", "grade-gradle-cache:/home/gradle/.gradle", GRADLE_IMG, "gradle", "test", "--console=plain"],
			{ maxBuffer: 32 * 1024 * 1024 });
		return { pass: true, detail: "" };
	} catch (e) {
		const out = (e.stdout?.toString() || "") + (e.stderr?.toString() || "");
		const m = out.match(/expected:.*$/m);
		return { pass: false, detail: m ? m[0] : out.trim().split("\n").slice(-3).join(" ") };
	}
}

function mcpConfigFile(wd, variant, runTag) {
	const cfg = { mcpServers: { proj: {
		command: "node",
		args: [join(REPO, "tools", "proj-mcp.mjs")],
		env: { BENCH_VARIANT: variant, BENCH_CWD: wd, BENCH_FPBIN: BIN, BENCH_GRADLE_IMG: GRADLE_IMG },
	} } };
	const p = join(OUTDIR, `mcp-${runTag}.json`);
	writeFileSync(p, JSON.stringify(cfg));
	return p;
}

// ---- claude-code ---------------------------------------------------------

function runClaude(wd, variant, runTag, jsonlPath) {
	const mcp = mcpConfigFile(wd, variant, runTag);
	const allowed = VARIANT_TOOLS[variant].map((t) => `mcp__proj__${t}`);
	// Hard-deny every builtin so only the variant's mcp__proj__* tools remain (disallowedTools
	// is an absolute deny, even under bypassPermissions). Keeps the run on the supplied tools.
	const builtins = [
		"Bash", "BashOutput", "KillBash", "Read", "Edit", "MultiEdit", "Write", "Glob", "Grep",
		"WebFetch", "WebSearch", "NotebookEdit", "Task", "TodoWrite", "SlashCommand",
		"AskUserQuestion", "CronCreate", "CronDelete", "CronList", "DesignSync",
		"EnterPlanMode", "ExitPlanMode", "EnterWorktree", "ExitWorktree", "Monitor",
		"PushNotification", "ScheduleWakeup", "Skill", "Workflow",
		"TaskCreate", "TaskGet", "TaskList", "TaskOutput", "TaskStop", "TaskUpdate",
		"ListMcpResourcesTool", "ReadMcpResourceTool",
	];
	const args = [
		"-p", TASK,
		"--output-format", "stream-json", "--verbose",
		"--mcp-config", mcp,
		"--strict-mcp-config",
		"--permission-mode", "bypassPermissions",
		"--allowedTools", ...allowed,
		"--disallowedTools", ...builtins,
		"--add-dir", wd,
	];
	if (MODEL) args.push("--model", MODEL);
	// ollama 0.20.4 serves the Anthropic Messages API at /v1/messages, so claude-code
	// can drive an ollama model directly (no shim) by pointing ANTHROPIC_BASE_URL at it.
	const env = { ...process.env };
	if (CLAUDE_BASE_URL) {
		env.ANTHROPIC_BASE_URL = CLAUDE_BASE_URL;
		env.ANTHROPIC_AUTH_TOKEN = process.env.ANTHROPIC_AUTH_TOKEN || "ollama";
		env.ANTHROPIC_API_KEY = "";
	}
	return spawnHarness("claude", args, { cwd: wd, env }, jsonlPath);
}

// ---- codex ---------------------------------------------------------------

// Isolated CODEX_HOME so the user's ~/.codex MCP servers (e.g. code-review-graph)
// never leak into base/proj — codex has no --strict-mcp-config. The box's ollama
// (0.20.4) serves the Responses API codex 0.142 requires, via a non-reserved
// custom provider id.
function codexHome() {
	const home = join("/tmp", "codex-clean");
	mkdirSync(home, { recursive: true });
	writeFileSync(join(home, "config.toml"), [
		`model_provider = "boxoss"`,
		``,
		`[model_providers.boxoss]`,
		`name = "boxoss"`,
		`base_url = "${BOX}"`,
		`wire_api = "responses"`,
		`requires_openai_auth = false`,
		``,
	].join("\n"));
	return home;
}

function runCodex(wd, variant, runTag, jsonlPath) {
	const mcp = join(REPO, "tools", "proj-mcp.mjs");
	const c = (k, v) => ["-c", `${k}=${v}`];
	// proj is read-only so any edit is forced through the lens (edit_program);
	// base/graph get workspace-write so codex's native edit works as its baseline.
	const sandbox = variant === "proj" ? "read-only" : "workspace-write";
	const args = [
		"exec", "--json", "--skip-git-repo-check",
		"-C", wd,
		"-s", sandbox,
		...c("approval_policy", '"never"'),
		...c("mcp_servers.proj.command", '"node"'),
		...c("mcp_servers.proj.args", JSON.stringify([mcp])),
		...c("mcp_servers.proj.env.BENCH_VARIANT", `"${variant}"`),
		...c("mcp_servers.proj.env.BENCH_CWD", `"${wd}"`),
		...c("mcp_servers.proj.env.BENCH_FPBIN", `"${BIN}"`),
		...c("mcp_servers.proj.env.BENCH_GRADLE_IMG", `"${GRADLE_IMG}"`),
	];
	if (MODEL) args.push("-m", MODEL);
	args.push(TASK);
	return spawnHarness("codex", args, { cwd: wd, env: { ...process.env, CODEX_HOME: codexHome(), OPENAI_API_KEY: "ollama" } }, jsonlPath);
}

const MAX_MS = Number(process.env.RUN_TIMEOUT_MS || 900000); // 15 min hard cap per run

function spawnHarness(cmd, args, opts, jsonlPath) {
	return new Promise((resolve) => {
		const out = createWriteStream(jsonlPath);
		const events = [];
		const proc = spawn(cmd, args, { ...opts, stdio: ["ignore", "pipe", "pipe"] });
		const killer = setTimeout(() => { try { proc.kill("SIGKILL"); } catch {} }, MAX_MS);
		const rl = createInterface({ input: proc.stdout });
		rl.on("line", (line) => {
			const s = line.trim();
			if (!s) return;
			out.write(s + "\n");
			if (s.startsWith("{")) { try { events.push(JSON.parse(s)); } catch {} }
		});
		let stderr = "";
		proc.stderr.on("data", (d) => { stderr += d.toString(); });
		proc.on("close", (code) => { clearTimeout(killer); out.end(); resolve({ code, events, stderr }); });
	});
}

// ---- event interpretation (per harness) ----------------------------------

function toolCallsClaude(events) {
	const calls = [];
	for (const e of events) {
		const blocks = e?.message?.content;
		if (Array.isArray(blocks)) {
			for (const b of blocks) if (b.type === "tool_use") calls.push((b.name || "").replace(/^mcp__proj__/, ""));
		}
	}
	const usage = events.map((e) => e?.message?.usage).filter(Boolean).at(-1) ||
		events.map((e) => e?.usage).filter(Boolean).at(-1) || null;
	const result = events.find((e) => e.type === "result");
	return { calls, tokens: tokensFromClaude(events), final: result?.result || "" };
}
function tokensFromClaude(events) {
	let inT = 0, outT = 0;
	for (const e of events) {
		const u = e?.message?.usage;
		if (u) { inT += (u.input_tokens || 0) + (u.cache_read_input_tokens || 0) + (u.cache_creation_input_tokens || 0); outT += u.output_tokens || 0; }
	}
	const res = events.find((e) => e.type === "result");
	if (res?.usage) { inT = (res.usage.input_tokens || inT); outT = (res.usage.output_tokens || outT); }
	return inT + outT;
}
function toolCallsCodex(events) {
	// codex 0.142 `exec --json` emits {type:"item.completed",item:{type,...}} and
	// {type:"turn.completed",usage:{...}}. MCP/shell calls arrive as item.type
	// mcp_tool_call / command_execution; final text as agent_message.
	const calls = [];
	let tokens = 0, final = "";
	for (const e of events) {
		const t = e.type || "";
		if (t === "item.completed") {
			const it = e.item || {};
			if (it.type === "mcp_tool_call") {
				const nm = it.tool || it.name || it.invocation?.tool || "";
				calls.push(String(nm).replace(/^proj[._]/, "").replace(/^mcp__proj__/, ""));
			} else if (it.type === "command_execution") {
				calls.push("shell");
			} else if (it.type === "agent_message" && it.text) {
				final = it.text;
			}
		}
		if (t === "turn.completed" && e.usage) {
			tokens += (e.usage.input_tokens || 0) + (e.usage.output_tokens || 0);
		}
	}
	return { calls, tokens, final };
}

// ---- main ----------------------------------------------------------------

async function main() {
	const all = loadResults();
	for (const variant of VARIANTS) {
		const runTag = `${HARNESS}-${variant}`;
		const jsonlPath = join(OUTDIR, `${runTag}.jsonl`);
		const wd = freshWorkdir(runTag);
		const supplied = VARIANT_TOOLS[variant];
		const meta = {
			id: runTag, harness: HARNESS, variant, model: MODEL || "(harness default)",
			tools_supplied: supplied, status: "running", started: new Date().toISOString(),
			tool_calls: [], tokens: 0, gradle: null, graph_leak: false, workdir: wd, jsonl: `harness/${runTag}.jsonl`,
		};
		const others = all.runs.filter((r) => r.id !== runTag);
		all.runs = [...others, meta]; saveResults(all);

		process.stderr.write(`\n=== ${runTag} (model=${meta.model}) tools=${supplied.join(",")} ===\n`);
		const t0 = Date.now();
		const res = HARNESS === "codex"
			? await runCodex(wd, variant, runTag, jsonlPath)
			: await runClaude(wd, variant, runTag, jsonlPath);
		const parsed = HARNESS === "codex" ? toolCallsCodex(res.events) : toolCallsClaude(res.events);

		const gradle = await gradleStatus(wd);
		const usedSet = [...new Set(parsed.calls)];
		const leak = usedSet.some((t) => GRAPH_TOOLS.includes(t)) && variant !== "graph";

		Object.assign(meta, {
			status: "done", finished: new Date().toISOString(), seconds: Math.round((Date.now() - t0) / 1000),
			tool_calls: parsed.calls, tools_used: usedSet, tokens: parsed.tokens,
			gradle: { pass: gradle.pass, detail: gradle.detail }, graph_leak: leak,
			exit_code: res.code, final: (parsed.final || "").slice(0, 400),
			stderr_tail: (res.stderr || "").split("\n").filter(Boolean).slice(-4).join(" | ").slice(0, 400),
		});
		all.runs = [...all.runs.filter((r) => r.id !== runTag), meta]; saveResults(all);
		process.stderr.write(`--- ${runTag}: ${gradle.pass ? "PASS" : "FAIL"} | calls=${parsed.calls.length} [${usedSet.join(",")}] | tokens=${parsed.tokens} | leak=${leak} | ${meta.seconds}s\n`);
	}
	process.stderr.write(`\nDone. Results: ${RESULTS}\n`);
}

main().catch((e) => { process.stderr.write("FATAL " + e.stack + "\n"); process.exit(1); });
