#!/usr/bin/env python3
"""
java-experiment.py — control-flow lens vs code-review-graph traversal, on a real Java sample.

A tool-calling model must fix TWO wrong conditions (in two files) so a JUnit test passes,
built+run by Gradle inside Docker. Three variants differ only in how the agent locates the
faulty conditions; all edit with the same precise line-range tool, all run the same test:

  base   — search (grep) + read_file + edit_lines + run_tests
  graph  — code-review-graph traversal (semantic_search / query_graph) over the prebuilt
           graph, via the real `code-review-graph serve` stdio MCP + read_file + edit_lines
  proj   — open_control_flow(file): the file-projections control-flow lens returns each
           path's active conditions with their REAL source line numbers; editing those
           lines with edit_lines IS the two-way sync back to source.

Isolated working copies, sequential. Every request/turn logged. Live HTML report.

  python3 tools/java-experiment.py run | report | all
"""
import json, os, re, subprocess, sys, time, threading, shutil, urllib.request
import http.server, socketserver, functools

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MODEL = os.environ.get("MODEL", "qwen3-coder:latest")
HOST = os.environ.get("OLLAMA_HOST", "http://localhost:11434")
OUT = "/tmp/of-exp"
PORT = int(os.environ.get("EXP_PORT", "8773"))
MAX_TURNS = 16
SAMPLE = os.path.join(ROOT, "fixtures", "objflow-sample")   # prebuilt graph lives here
SRC = "src/main/java"
GRADLE_IMG = "gradle:8.5-jdk17"
FPBIN = os.path.join(ROOT, "bin", "file-projections")
CRG = os.path.expanduser("~/.local/bin/code-review-graph")

# the two planted bugs (for reporting only; the sample ships with them)
BUGS = [("sample/TierStage.java", 6, "r.setLabel(t)", "r.setTier(t)"),
        ("sample/LabelStage.java", 6, '"$" + amount', '"$" + net')]
TASK = ("A JUnit test is failing: App.summary(\"save\", 50) should be \"SAVE/$45/SILVER\" but "
        "isn't. A Receipt object is assembled across several stage classes (CodeStage, "
        "LabelStage, TierStage) before App reads its fields. Two of those stages mutate the "
        "Receipt incorrectly — so a field ends up wrong or never set. Trace how the Receipt "
        "object is transformed across the files, find and fix the two faulty mutations, and run "
        "the tests until they pass. Use the tools step by step; reply each step with a tool call.")

LIVE = {"model": MODEL, "status": "starting", "variants": [], "task": "fix 2 faulty Receipt mutations (one field never set, one wrong value) so summary(\"save\",50)==\"SAVE/$45/SILVER\" — Gradle/JUnit"}
def publish():
    LIVE["ts"] = time.strftime("%Y-%m-%d %H:%M:%S")
    try: json.dump(LIVE, open(f"{OUT}/live.json", "w"))
    except Exception: pass

# ---- ollama -----------------------------------------------------------------------------
def chat(messages, tools):
    body = json.dumps({"model": MODEL, "stream": False, "messages": messages, "tools": tools,
                       "options": {"temperature": 0, "num_ctx": 8192}}).encode()
    req = urllib.request.Request(HOST + "/api/chat", data=body, headers={"Content-Type": "application/json"})
    return json.load(urllib.request.urlopen(req, timeout=600))

def _try(s):
    try: return json.loads(s)
    except Exception: return None

def parse_calls(msg):
    out = []
    for c in (msg.get("tool_calls") or []):
        f = c.get("function", {}); a = f.get("arguments", {})
        out.append((f.get("name", ""), a if isinstance(a, dict) else (_try(a) or {})))
    if out: return out
    content = msg.get("content", "") or ""
    for m in re.finditer(r'\{(?:[^{}]|\{[^{}]*\})*\}', content, re.S):
        o = _try(m.group(0))
        if isinstance(o, dict) and "name" in o and ("arguments" in o or "parameters" in o):
            a = o.get("arguments", o.get("parameters", {}))
            out.append((o["name"], a if isinstance(a, dict) else (_try(a) or {})))
    if out: return out
    for fm in re.finditer(r'<function=(\w+)>(.*?)</function>', content, re.S):
        args = {k: v.strip("\n") for k, v in re.findall(r'<parameter=(\w+)>(.*?)</parameter>', fm.group(2), re.S)}
        out.append((fm.group(1), args))
    return out

# ---- code-review-graph stdio MCP client -------------------------------------------------
class GraphMCP:
    def __init__(self, repo_root):
        self.repo_root = repo_root
        self.p = subprocess.Popen([CRG, "serve"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.DEVNULL, text=True, bufsize=1)
        self.id = 0
        self._send({"jsonrpc": "2.0", "id": self._next(), "method": "initialize",
                    "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                               "clientInfo": {"name": "jx", "version": "1"}}})
        self._recv()
        self._send({"jsonrpc": "2.0", "method": "notifications/initialized"})
    def _next(self): self.id += 1; return self.id
    def _send(self, o): self.p.stdin.write(json.dumps(o) + "\n"); self.p.stdin.flush()
    def _recv(self):
        while True:
            ln = self.p.stdout.readline()
            if not ln: return None
            ln = ln.strip()
            if ln.startswith("{"):
                try: return json.loads(ln)
                except Exception: pass
    def call(self, name, args):
        a = dict(args); a["repo_root"] = self.repo_root
        self._send({"jsonrpc": "2.0", "id": self._next(), "method": "tools/call",
                    "params": {"name": name, "arguments": a}})
        r = self._recv()
        try: return r["result"]["content"][0]["text"]
        except Exception: return json.dumps(r)[:500]
    def close(self):
        try: self.p.terminate()
        except Exception: pass

# ---- gradle-in-docker test --------------------------------------------------------------
import glob as _glob, xml.etree.ElementTree as _ET

def gradle_test(workdir):
    """Run the JUnit tests in Docker and return (ok, clean_message). The message is just the
    signal an engineer needs — assertion expected/actual + the test-source stack frame, or the
    javac error — never gradle's "What went wrong / BUILD FAILED / see report" boilerplate."""
    shutil.rmtree(os.path.join(workdir, "build", "test-results"), ignore_errors=True)
    p = subprocess.run(["docker", "run", "--rm", "-v", f"{workdir}:/app", "-w", "/app",
                        "-v", "grade-gradle-cache:/home/gradle/.gradle", GRADLE_IMG,
                        "gradle", "test", "--console=plain"], capture_output=True, text=True)
    out = p.stdout + p.stderr
    if p.returncode == 0:
        return True, "TESTS PASS"
    # 1) assertion failures from the JUnit XML reports (expected/actual + stack)
    fails = []
    for xf in _glob.glob(os.path.join(workdir, "build", "test-results", "test", "*.xml")):
        try:
            root = _ET.parse(xf).getroot()
        except Exception:
            continue
        for tc in root.iter("testcase"):
            fl = tc.find("failure")
            if fl is None:
                continue
            msg = (fl.get("message") or "").strip()
            stack = [s.strip().removeprefix("at ").replace("app//", "")
                     for s in (fl.text or "").splitlines() if ".java:" in s and "sample." in s][:3]
            fails.append(f"{tc.get('name')}: {msg}" + ("".join("\n    at " + s for s in stack)))
    if fails:
        return False, "TESTS FAIL\n" + "\n".join(fails)
    # 2) compile errors (javac: file:line: error: …)
    cerr, grab = [], 0
    for l in out.splitlines():
        if re.search(r'\.java:\d+: (error|warning):', l):
            cerr.append(l.strip().split("/app/")[-1]); grab = 2
        elif grab > 0 and l.strip():
            cerr.append("    " + l.strip()); grab -= 1
    if cerr:
        return False, "COMPILE ERROR\n" + "\n".join(cerr[:15])
    return False, "TESTS FAIL\n" + "\n".join(out.strip().splitlines()[-4:])

# ---- object data-flow projection (proj variant) -----------------------------------------
FARMCFG = os.path.join(ROOT, "farm.config.json")

def open_object_flow(workdir, typ, field=""):
    """Run the REAL joern object-flow lens (mode=joern, via the farm — no fallback). For each
    object the code builds, returns a branch-annotated timeline of how that instance is
    transformed (direct setters and cross-file stage calls resolved to the field + exact
    setter line), and the fields never set. Distinct objects and branches are separated, so a
    naive search can't be fooled. `field` narrows to one field."""
    typ = re.sub(r'[^A-Za-z0-9_]', '', typ) or "Receipt"
    field = re.sub(r'[^A-Za-z0-9_]', '', field or "")
    stem = "/tmp/of-vf"
    if os.path.exists(stem + ".projection"):
        os.remove(stem + ".projection")
    cmd = [FPBIN, "-config", FARMCFG, "-analyzer", "object-flow", "-mode", "joern",
           "-source-root", SRC, "-var", typ, "-out", stem + ".projection"]
    if field:
        cmd += ["-method", field]
    r = subprocess.run(cmd, cwd=workdir, capture_output=True, text=True)
    if not os.path.isfile(stem + ".projection"):
        return f"joern object-flow failed (no fallback): {r.stderr.strip()[-200:]}"
    text = open(stem + ".projection").read(); os.remove(stem + ".projection")
    blocks, facts, cur = [], {}, None
    for l in text.split("\n"):
        if l.startswith("@@ "):
            m = re.search(r'#(\S+) ', l); cur = {"id": m.group(1) if m else "?", "lines": []}; blocks.append(cur)
        elif l == "@@":
            cur = None
        elif l.startswith("=> "):
            m = re.match(r'=> (\S+?): (.*)', l)
            if m: facts.setdefault(m.group(1), []).append(m.group(2))
        elif cur is not None and l.strip():
            cur["lines"].append(l.strip())
    if not blocks:
        return f"object-flow: type {typ} not found (no fallback)"
    out = [f"object-flow of `{typ}` — how each instance is assembled across files.",
           "The `edit <file>:<line>` shown on each step is the REAL source line — change it "
           "directly with edit_lines (start==end==that line); that is the fix. Lines marked "
           "⚠ ANOMALY / ⚠ SUSPECT are where the bug almost certainly is — act on them first; "
           "you usually do NOT need to read other files (only read a stage if you must see a "
           "local variable it computes)."]
    for b in blocks:
        fs = facts.get(b["id"], [])
        scope = next((f for f in fs if f.startswith("object scope")), b["id"])
        anomalies = [f for f in fs if f.startswith("ANOMALY")]
        out.append("")
        out.append(f"# {scope}")
        for a in anomalies:
            out.append("  ⚠ " + a)
        for ln in b["lines"]:
            out.append("  " + ln)
    return "\n".join(out)

# ---- tools ------------------------------------------------------------------------------
def find_file(wd, rel):
    """Resolve a file the agent names (sample/Daypart.java, AppTest.java, …) anywhere in the
    sample copy (src/main or src/test)."""
    rel = (rel or "").lstrip("/")
    for pre in ("", SRC + "/", "src/test/java/"):
        p = os.path.join(wd, pre + rel)
        if os.path.isfile(p): return p
    base = os.path.basename(rel)
    for dp, _, fs in os.walk(wd):
        if base in fs and "/build/" not in dp:
            return os.path.join(dp, base)
    return None

def make_tools(variant, wd, state):
    def read_file(args):
        full = find_file(wd, args.get("path", ""))
        if not full:
            return f"no such file: {args.get('path')} (e.g. sample/Daypart.java, sample/AppTest.java)"
        lines = open(full).read().split("\n")
        s = max(1, int(args.get("start", 1))); e = min(len(lines), int(args["end"]) if args.get("end") else len(lines))
        body = "\n".join(f"{i}: {lines[i-1]}" for i in range(s, e + 1))
        state["inspect"] += len(body); state["icalls"] += 1
        return body

    def search(args):
        pat = args.get("pattern", args.get("query", ""))
        r = subprocess.run(["grep", "-rn", pat, os.path.join(wd, "src")], capture_output=True, text=True).stdout
        r = r.replace(os.path.join(wd, SRC) + "/", "").replace(os.path.join(wd, "src/test/java") + "/", "")[:1200] or "no matches"
        state["inspect"] += len(r); state["icalls"] += 1
        return r

    def g(mcp, args):
        res = state["graph"].call(mcp, args)
        state["inspect"] += len(res); state["icalls"] += 1
        return res[:1400]
    def graph_search(args): return g("semantic_search_nodes_tool", {"query": args.get("query", args.get("name", "")), "detail_level": "minimal"})
    def graph_query(args):  return g("query_graph_tool", {"pattern": args.get("pattern", "children_of"), "target": args.get("target", "")})
    def graph_flows(args):  return g("list_flows_tool", {})
    def graph_traverse(args): return g("traverse_graph_tool", {k: v for k, v in args.items()})

    def object_flow(args):
        typ = args.get("type", args.get("typeName", args.get("class", args.get("name", "Receipt"))))
        res = open_object_flow(wd, typ, args.get("field", ""))
        state["inspect"] += len(res); state["icalls"] += 1
        return res

    def edit_lines(args):
        full = find_file(wd, args.get("path", ""))
        if not full: return f"no such file: {args.get('path')}"
        s = int(args["start"]); e = int(args.get("end", s))
        new = args.get("new_text", args.get("text", args.get("content", "")))
        lines = open(full).read().split("\n")
        if s < 1 or e > len(lines) or e < s: return f"bad range {s}-{e} (file has {len(lines)} lines)"
        lines = lines[:s-1] + new.split("\n") + lines[e:]
        open(full, "w").write("\n".join(lines))
        return f"replaced lines {s}-{e} in {os.path.basename(full)}"

    def run_tests(args):
        ok, msg = gradle_test(wd)
        state["last_ok"] = ok
        return msg

    F = lambda n, d, p, req: {"type": "function", "function": {"name": n, "description": d,
        "parameters": {"type": "object", "properties": p, "required": req}}}
    impls = {"read_file": read_file, "search": search, "object_flow": object_flow,
             "edit_lines": edit_lines, "run_tests": run_tests,
             "graph_search": graph_search, "graph_query": graph_query,
             "graph_flows": graph_flows, "graph_traverse": graph_traverse}
    spec = [
        F("edit_lines", "Replace source lines [start,end] (1-based, inclusive) with new_text. Prefer the SMALLEST edit: to change one line set start==end to that line and put ONLY that corrected line in new_text — do NOT repeat the method signature or surrounding lines (that duplicates them). Use the line numbers from read_file/object_flow.",
          {"path": {"type": "string"}, "start": {"type": "integer"}, "end": {"type": "integer"}, "new_text": {"type": "string"}}, ["path", "start", "new_text"]),
        F("run_tests", "Build and run the JUnit tests with Gradle in Docker. Returns PASS/FAIL.", {}, []),
    ]
    if variant == "base":
        spec[:0] = [F("search", "grep the Java sources for a regex; returns file:line matches.", {"pattern": {"type": "string"}}, ["pattern"]),
                    F("read_file", "Read a Java file line-numbered (path like sample/Daypart.java). Optional start/end.", {"path": {"type": "string"}, "start": {"type": "integer"}, "end": {"type": "integer"}}, ["path"])]
    if variant == "graph":
        spec[:0] = [F("graph_search", "Search the code-review-graph for symbols by name/keyword; returns matches with file:line.", {"query": {"type": "string"}}, ["query"]),
                    F("graph_query", "Query graph relationships. pattern is one of children_of|callers_of|callees_of|file_summary|tests_for; target is a symbol or file.", {"pattern": {"type": "string"}, "target": {"type": "string"}}, ["pattern", "target"]),
                    F("graph_flows", "List execution flows the graph detected.", {}, []),
                    F("read_file", "Read a Java file line-numbered (path like sample/Grader.java). Optional start/end.", {"path": {"type": "string"}, "start": {"type": "integer"}, "end": {"type": "integer"}}, ["path"])]
    if variant == "proj":
        spec[:0] = [F("object_flow", "Trace an object across the codebase with the real joern object-flow lens. Give a class name (e.g. Receipt); for EACH object the code builds it returns a branch-annotated timeline (constructor, then each mutation with the branch it happens under and the exact setter file:line, cross-file stage calls resolved to the field they set) plus the fields never set. Distinct objects/branches are separated. Optional `field` narrows to one field. Fix a faulty mutation with edit_lines at its setter line.",
                      {"type": {"type": "string"}, "field": {"type": "string"}}, ["type"]),
                    F("read_file", "Read a Java file line-numbered (path like sample/LabelStage.java) to inspect a mutation's details. Optional start/end.", {"path": {"type": "string"}, "start": {"type": "integer"}, "end": {"type": "integer"}}, ["path"])]
    # Restrict impls to exactly the advertised tools — no undeclared-tool fallback, so each
    # variant is tested on its own locating mechanism only.
    allowed = {f["function"]["name"] for f in spec}
    impls = {k: v for k, v in impls.items() if k in allowed}
    return spec, impls

def system_prompt(variant):
    s = ("You are a coding agent fixing bugs in a small Java project (package sample: a Receipt "
         "POJO assembled by CodeStage, LabelStage, TierStage, read by App; test AppTest.java). "
         "Two stages mutate the Receipt incorrectly (a field ends wrong or never set). Locate "
         "them, fix with edit_lines, and run_tests until they pass. Make the smallest possible "
         "change — usually a single line (start==end). Reply each step with a single tool call.")
    if variant == "graph":
        s += " Use the graph tools (code-review-graph) to explore symbols/structure, then read_file the region."
    if variant == "proj":
        s += (" Call object_flow(type=\"Receipt\") ONCE. It flags the anomalies (⚠) and gives the "
              "exact `edit <file>:<line>` for each mutation — those are real, editable source "
              "lines. Edit the flagged lines directly with edit_lines (start==end). Do NOT read "
              "every file; only read a stage if you must see a local variable's value it computes. "
              "Then run_tests; its failure message shows expected vs actual to guide the next fix.")
    return s

# ---- run --------------------------------------------------------------------------------
def run_variant(variant, entry):
    wd = f"{OUT}/wc-{variant}"
    shutil.rmtree(wd, ignore_errors=True)
    shutil.copytree(SAMPLE, wd, ignore=shutil.ignore_patterns(".code-review-graph", "build", ".gradle"))
    state = {"inspect": 0, "icalls": 0, "last_ok": False}
    if variant == "graph":
        state["graph"] = GraphMCP(SAMPLE)            # prebuilt graph (source lines match the copy)
    spec, impls = make_tools(variant, wd, state)
    messages = [{"role": "system", "content": system_prompt(variant)}, {"role": "user", "content": TASK}]
    entry["messages"] = messages
    tokens = turns = calls = nudges = 0
    publish()
    for _ in range(MAX_TURNS):
        turns += 1
        r = chat(messages, spec)
        tokens += r.get("prompt_eval_count", 0) + r.get("eval_count", 0)
        msg = r.get("message", {})
        messages.append({"role": "assistant", "content": msg.get("content", ""),
                         **({"tool_calls": msg["tool_calls"]} if msg.get("tool_calls") else {})})
        entry.update(turns=turns, tokens=tokens, tool_calls=calls, inspect_chars=state["inspect"], inspect_calls=state["icalls"]); publish()
        cs = parse_calls(msg)
        if not cs:
            nudges += 1
            if nudges >= 2 or state["last_ok"]: break
            messages.append({"role": "user", "content": "Continue with a tool call (edit_lines then run_tests)."}); publish()
            continue
        nudges = 0; stop = False
        for name, args in cs:
            calls += 1
            res = impls.get(name, lambda a: f"unknown tool {name}")(args)
            messages.append({"role": "tool", "content": f"[{name}] {res}"})
            entry.update(tool_calls=calls, inspect_chars=state["inspect"], inspect_calls=state["icalls"]); publish()
            if name == "run_tests" and state["last_ok"]: stop = True
        if stop: break
    ok, out = gradle_test(wd)
    entry.update(passed=ok, turns=turns, tokens=tokens, tool_calls=calls,
                 inspect_chars=state["inspect"], inspect_calls=state["icalls"], done=True)
    publish()
    if variant == "graph": state["graph"].close()
    shutil.rmtree(wd, ignore_errors=True)

def prepare():
    subprocess.run(["go", "build", "-o", FPBIN, "."], cwd=ROOT, capture_output=True)
    # ensure the code-review-graph build for the sample is fresh
    subprocess.run([CRG, "build", "--repo", SAMPLE], capture_output=True)

# ---- live server + report ---------------------------------------------------------------
class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *a): pass

def start_server():
    socketserver.TCPServer.allow_reuse_address = True
    httpd = socketserver.TCPServer(("127.0.0.1", PORT), functools.partial(QuietHandler, directory=OUT))
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    return httpd

def write_report(initial="null"):
    open(f"{OUT}/report.html", "w").write(REPORT_HTML.replace("__INITIAL__", initial))

def cmd_run():
    os.makedirs(OUT, exist_ok=True)
    write_report("null"); publish(); start_server()
    url = f"http://127.0.0.1:{PORT}/report.html"; subprocess.run(["open", url]); print("live report:", url, flush=True)
    LIVE["status"] = "preparing"; publish(); prepare()
    LIVE["status"] = "running"; publish()
    for v in ("base", "graph", "proj"):
        print(f"=== {v} ===", flush=True)
        e = {"variant": v, "passed": False, "turns": 0, "tool_calls": 0, "tokens": 0,
             "inspect_chars": 0, "inspect_calls": 0, "done": False, "messages": []}
        LIVE["variants"].append(e); publish()
        try: run_variant(v, e)
        except Exception as ex:
            e.update(done=True, error=str(ex)); publish()
        json.dump(e, open(f"{OUT}/{v}.json", "w"), indent=2)
        print(f"  passed={e['passed']} turns={e['turns']} tokens={e['tokens']} inspect={e['inspect_chars']}c/{e['inspect_calls']}", flush=True)
    LIVE["status"] = "done"; publish(); write_report(json.dumps(LIVE)); time.sleep(3)

def cmd_report():
    if os.path.isfile(f"{OUT}/live.json"):
        write_report(open(f"{OUT}/live.json").read()); print(f"wrote {OUT}/report.html")

REPORT_HTML = r"""<!doctype html><html><head><meta charset=utf-8><title>java experiment</title><style>
body{font:14px/1.55 -apple-system,Segoe UI,Roboto,sans-serif;max-width:960px;margin:1.5rem auto;padding:0 1rem;color:#1d1d1f;background:#fafafa}
h1{font-size:1.5rem;margin-bottom:.2rem}.sub{color:#888;margin-top:0}
.live{display:inline-block;font-size:.72rem;padding:.1rem .5rem;border-radius:10px;background:#eee;color:#555}
.live.running,.live.preparing{background:#fff4d6;color:#a86b00}.live.done{background:#daf5e0;color:#127a2b}
table{border-collapse:collapse;width:100%;margin:1rem 0;background:#fff;box-shadow:0 1px 3px #0001;border-radius:8px;overflow:hidden}
th,td{padding:.55rem .8rem;text-align:left;border-bottom:1px solid #eee}th{background:#f4f4f6}
.pass{color:#127a2b;font-weight:600}.fail{color:#b00020;font-weight:600}.run{color:#a86b00;font-weight:600}.best{background:#f1fbf3}
details{background:#fff;margin:.5rem 0;border:1px solid #eee;border-radius:8px;padding:.3rem .8rem}
summary{cursor:pointer;padding:.3rem 0;font-weight:600}
.msg{margin:.4rem 0;border-left:3px solid #ddd;padding-left:.6rem}.msg .role{font-size:.68rem;text-transform:uppercase;letter-spacing:.05em;color:#888}
.msg.user{border-color:#3b82f6}.msg.assistant{border-color:#10b981}.msg.tool{border-color:#f59e0b}.msg.system{border-color:#bbb}
pre{white-space:pre-wrap;word-break:break-word;margin:.2rem 0;font:12px/1.45 ui-monospace,Menlo,monospace;background:#f7f7f9;padding:.5rem;border-radius:6px}
.legend{font-size:.8rem;color:#666}.count{color:#999;font-weight:400;font-size:.85em}
</style></head><body>
<h1>object-flow lens (real joern) vs code-review-graph <span id=status class=live>…</span></h1>
<p class=sub id=task></p>
<p class=legend>model <code id=model></code> · <span id=ts></span> · Java sample, Gradle/JUnit in Docker, isolated copies, sequential.
<b>base</b>=grep+read · <b>graph</b>=code-review-graph traversal (real stdio MCP) · <b>proj</b>=joern object-flow lens via the farm (object transformations across files; no fallback).
<b>inspect chars/calls</b> = code ingested to locate the faulty mutations.</p>
<table><thead><tr><th>variant</th><th>result</th><th>turns</th><th>tool calls</th><th>tokens</th><th>inspect chars</th><th>inspect calls</th></tr></thead><tbody id=rows></tbody></table>
<h2>Conversations <span class=count>(live)</span></h2><div id=convs></div>
<script>
const INITIAL = __INITIAL__;
function esc(s){return (s||"").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;");}
function render(d){ if(!d)return;
  document.getElementById("model").textContent=d.model||"";
  document.getElementById("ts").textContent=d.ts||"";
  document.getElementById("task").textContent=d.task||"";
  const st=document.getElementById("status"); st.textContent=d.status||""; st.className="live "+(d.status||"");
  let best=null; (d.variants||[]).forEach(v=>{if(v.done&&v.passed&&(best===null||v.tokens<best))best=v.tokens;});
  let rows="";
  (d.variants||[]).forEach(v=>{
    let res=!v.done?"<span class=run>… running</span>":(v.passed?"<span class=pass>✅ PASS</span>":"<span class=fail>❌ FAIL</span>");
    const cls=(v.done&&v.passed&&v.tokens===best)?" class=best":"";
    rows+=`<tr${cls}><td><b>${v.variant}</b></td><td>${res}</td><td>${v.turns||0}</td><td>${v.tool_calls||0}</td><td>${v.tokens||0}</td><td>${v.inspect_chars||0}</td><td>${v.inspect_calls||0}</td></tr>`;
  });
  document.getElementById("rows").innerHTML=rows;
  let convs="";
  (d.variants||[]).forEach(v=>{ let items="";
    (v.messages||[]).forEach(m=>{ let c=m.content||""; if(m.tool_calls)c+="\n"+JSON.stringify(m.tool_calls,null,2);
      items+=`<div class="msg ${m.role}"><span class=role>${m.role}</span><pre>${esc(c)}</pre></div>`; });
    const open=(!v.done)?" open":""; const tag=!v.done?"… running":(v.passed?"✅ pass":"❌ fail");
    convs+=`<details${open}><summary>${v.variant} — ${tag}, ${v.turns||0} turns, ${v.tokens||0} tokens, ${v.inspect_chars||0} inspect chars</summary>${items}</details>`; });
  document.getElementById("convs").innerHTML=convs;
}
function poll(){fetch("live.json?_="+Date.now()).then(r=>r.json()).then(render).catch(()=>{});}
render(INITIAL); poll(); setInterval(poll,1000);
</script></body></html>"""

if __name__ == "__main__":
    mode = sys.argv[1] if len(sys.argv) > 1 else "all"
    if mode == "report": cmd_report()
    else:
        cmd_run(); cmd_report()
