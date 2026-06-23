#!/usr/bin/env python3
"""
clean-experiment.py — an honest, agentic A/B/C dogfooding experiment.

A local tool-calling model must fix one planted bug in main.go so the test suite passes.
Three variants differ ONLY in how the agent locates + opens the code; nothing is pre-fed,
so identification cost is paid by every variant and counted:

  base   — search (grep) + read_file + edit_file + run_tests on the real source
  graph  — + graph_lookup (pre-built code-review-graph index) to locate a symbol
  proj   — open_projection(symbol): the file-projections tool locates the function and
           returns a focused, line-numbered, EDITABLE projection of just its code; edits
           are synced back to the real source (offset-aware two-way sync). Tests run on the
           real tree. The agent must call the tool itself — no slice is pre-supplied.

Each variant runs in its own git worktree (isolated), sequentially. Every request and turn
is logged. An HTML report at /tmp/clean-exp/report.html updates live as the run proceeds.

  python3 tools/clean-experiment.py run | report | selftest | all
"""
import json, os, re, subprocess, sys, urllib.request, html, time, threading
import http.server, socketserver, functools

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MODEL = os.environ.get("MODEL", "qwen3-coder:latest")
HOST = os.environ.get("OLLAMA_HOST", "http://localhost:11434")
OUT = "/tmp/clean-exp"
PORT = int(os.environ.get("EXP_PORT", "8770"))
MAX_TURNS = 10

TARGET_FILE = "main.go"
TARGET_FN = "firstLine"
BUG_FROM = "\t\treturn s[:i]"
BUG_TO   = "\t\treturn s[i:]"
GUARD_TEST = (
    "\nfunc TestFirstLineExp(t *testing.T) {\n"
    "\tif got := firstLine(\"a\\nb\"); got != \"a\" {\n"
    "\t\tt.Fatalf(\"firstLine(\\\"a\\\\nb\\\") = %q, want \\\"a\\\"\", got)\n"
    "\t}\n"
    "\tif got := firstLine(\"solo\"); got != \"solo\" {\n"
    "\t\tt.Fatalf(\"firstLine(\\\"solo\\\") = %q, want \\\"solo\\\"\", got)\n"
    "\t}\n"
    "}\n"
)
TASK = ("The Go test suite is failing: TestFirstLineExp expects firstLine(\"a\\nb\") == \"a\" "
        "but it returns something else. Locate the firstLine function, fix the one-line bug, "
        "and run the tests until they pass. Use the tools step by step; reply each step with a "
        "tool call.")

LIVE = {"model": MODEL, "status": "starting", "variants": []}

def publish():
    LIVE["ts"] = time.strftime("%Y-%m-%d %H:%M:%S")
    try:
        json.dump(LIVE, open(f"{OUT}/live.json", "w"))
    except Exception:
        pass

# ---- ollama chat -----------------------------------------------------------------------
def chat(messages, tools):
    body = json.dumps({"model": MODEL, "stream": False, "messages": messages,
                       "tools": tools, "options": {"temperature": 0, "num_ctx": 8192}}).encode()
    req = urllib.request.Request(HOST + "/api/chat", data=body,
                                 headers={"Content-Type": "application/json"})
    return json.load(urllib.request.urlopen(req, timeout=600))

def _try(s):
    try:
        return json.loads(s)
    except Exception:
        return None

def parse_calls(msg):
    out = []
    for c in (msg.get("tool_calls") or []):
        f = c.get("function", {}); a = f.get("arguments", {})
        out.append((f.get("name", ""), a if isinstance(a, dict) else (_try(a) or {})))
    if out:
        return out
    content = msg.get("content", "") or ""
    for m in re.finditer(r'\{(?:[^{}]|\{[^{}]*\})*\}', content, re.S):
        o = _try(m.group(0))
        if isinstance(o, dict) and "name" in o and ("arguments" in o or "parameters" in o):
            a = o.get("arguments", o.get("parameters", {}))
            out.append((o["name"], a if isinstance(a, dict) else (_try(a) or {})))
    if out:
        return out
    for fm in re.finditer(r'<function=(\w+)>(.*?)</function>', content, re.S):  # qwen3-coder
        args = {k: v.strip("\n") for k, v in
                re.findall(r'<parameter=(\w+)>(.*?)</parameter>', fm.group(2), re.S)}
        out.append((fm.group(1), args))
    return out

# ---- projection: rendering + offset-aware two-way sync ---------------------------------
def strip_prefixes(text):
    return "\n".join(re.sub(r'^\s*\d+:\s?', '', ln) for ln in text.split("\n"))

def _renumber(state):
    a = state["a"]
    state["proj_text"] = "\n".join(f"{a+k}: {ln}" for k, ln in enumerate(state["raw_span"]))

def load_span(wt, state):
    lines = open(os.path.join(wt, TARGET_FILE)).read().split("\n")
    state["raw_span"] = lines[state["a"]-1:state["b"]]
    _renumber(state)

def sync_projection(wt, state):
    """Write the raw span back over source lines [a,b]; inserted lines land in order and b
    re-offsets by the line-count delta so re-renders renumber correctly."""
    full = os.path.join(wt, TARGET_FILE)
    src = open(full).read().split("\n")
    a, b = state["a"], state["b"]
    src = src[:a-1] + state["raw_span"] + src[b:]
    open(full, "w").write("\n".join(src))
    state["b"] = a + len(state["raw_span"]) - 1
    _renumber(state)

def locate(graph_index, sym):
    m = re.search(rf"func (?:\([^)]*\) )?{re.escape(sym)}\(.*?lines=(\d+)-(\d+)", graph_index)
    return (int(m.group(1)), int(m.group(2))) if m else (None, None)

# ---- tools -----------------------------------------------------------------------------
def go_test(wt):
    p = subprocess.run(["go", "test", "-run", "TestFirstLineExp", "."],
                       cwd=wt, capture_output=True, text=True)
    return p.returncode == 0, (p.stdout + p.stderr)

def sh(cmd, wt):
    return subprocess.run(cmd, cwd=wt, capture_output=True, text=True)

def make_tools(variant, wt, state):
    def read_file(args):
        path = args.get("path", "") or TARGET_FILE
        full = os.path.join(wt, path)
        if not os.path.isfile(full):
            return f"no such file: {path}"
        lines = open(full).read().split("\n")
        s = int(args.get("start", 1)); e = int(args.get("end", min(len(lines), s + 60)))
        s = max(1, s); e = min(len(lines), e)
        hdr = f"// {path} lines {s}-{e} of {len(lines)}\n"
        more = "" if e >= len(lines) else f"\n… ({len(lines)-e} more lines; use search then read_file start/end)"
        return hdr + "\n".join(lines[s-1:e]) + more

    def search(args):
        pat = args.get("pattern", args.get("query", ""))
        return sh(["grep", "-rn", "--include=*.go", pat, "."], wt).stdout[:1500] or "no matches"

    def graph_lookup(args):
        name = args.get("name", args.get("symbol", ""))
        for ln in state["graph_index"].splitlines():
            if re.search(rf"\b{re.escape(name)}\b", ln):
                return ln
        return f"no symbol {name!r} in graph"

    def open_function(args):
        # NOTE: agent-facing wording deliberately presents this as plain source. It is in fact
        # a file-projections view kept in sync; the agent is never told (clean-experiment spec).
        sym = args.get("symbol", args.get("name", args.get("function", "")))
        a, b = locate(state["graph_index"], sym)
        if a is None:
            return f"no such function: {sym!r}"
        state["a"], state["b"] = a, b
        load_span(wt, state)
        state["opened"] = True
        return (f"source of {sym} (line-numbered — edit the code, ignore the leading 'N: '):\n"
                + state["proj_text"])

    def edit_file(args):
        old = args.get("old_string", args.get("old", ""))
        new = args.get("new_string", args.get("new", ""))
        if variant == "proj":
            if not state.get("opened"):
                return "open the function first with open_function(name)"
            old, new = strip_prefixes(old), strip_prefixes(new)
            raw = "\n".join(state["raw_span"])
            if old not in raw:
                return "old_string not found in the open function"
            state["raw_span"] = raw.replace(old, new, 1).split("\n")
            sync_projection(wt, state)   # quiet watcher: offset-aware write back to real source
            return "edit applied"
        path = args.get("path", "") or TARGET_FILE
        full = os.path.join(wt, path)
        if not os.path.isfile(full):
            return f"no such file: {path}"
        txt = open(full).read()
        if old not in txt:
            return "old_string not found in file"
        open(full, "w").write(txt.replace(old, new, 1))
        return "edit applied"

    def run_tests(args):
        ok, out = go_test(wt)
        state["last_test_ok"] = ok
        tail = "\n".join(out.strip().split("\n")[-12:])
        return ("TESTS PASS\n" + tail) if ok else ("TESTS FAIL\n" + tail)

    impls = {"read_file": read_file, "edit_file": edit_file, "run_tests": run_tests,
             "search": search, "graph_lookup": graph_lookup, "open_function": open_function}
    F = lambda n, d, p, req: {"type": "function", "function":
        {"name": n, "description": d, "parameters": {"type": "object", "properties": p, "required": req}}}
    spec = [
        F("edit_file", "Replace old_string with new_string (first match) in the open file/projection.",
          {"path": {"type": "string"}, "old_string": {"type": "string"}, "new_string": {"type": "string"}},
          ["old_string", "new_string"]),
        F("run_tests", "Run the Go test suite. Returns PASS or FAIL with output.", {}, []),
    ]
    if variant == "proj":
        spec.insert(0, F("open_function",
            "Open a function's source by name (line-numbered, editable). Edit it with "
            "edit_file and run_tests.",
            {"symbol": {"type": "string"}}, ["symbol"]))
    else:
        spec.insert(0, F("read_file", "Read a source file. Optional start/end line range.",
            {"path": {"type": "string"}, "start": {"type": "integer"}, "end": {"type": "integer"}}, ["path"]))
        spec.insert(1, F("search", "grep the repo for a regex; returns file:line matches.",
            {"pattern": {"type": "string"}}, ["pattern"]))
    if variant == "graph":
        spec.insert(2, F("graph_lookup", "Look up a symbol in the code-review-graph; returns its signature and line range.",
            {"name": {"type": "string"}}, ["name"]))
    return spec, impls

def system_prompt(variant):
    base = ("You are a coding agent fixing a bug in a Go project. Use the tools to locate, "
            "inspect and edit code, then run_tests until they pass. Make the smallest change. "
            "Reply each step with a single tool call.")
    if variant == "graph":
        base += " Use graph_lookup to locate a symbol fast, then read_file the range."
    if variant == "proj":
        base += (" To work on a function, call open_function(name) — it opens that function's "
                 "source, line-numbered and editable. Edit it with edit_file and run_tests. You "
                 "do not need to search the file.")
    return base

# ---- per-variant run -------------------------------------------------------------------
def setup_worktree(variant):
    wt = f"{OUT}/wt-{variant}"
    sh(["git", "worktree", "remove", "--force", wt], ROOT)
    subprocess.run(["rm", "-rf", wt]); sh(["git", "worktree", "prune"], ROOT)
    sh(["git", "worktree", "add", "--detach", wt, "HEAD"], ROOT)
    with open(os.path.join(wt, "main_test.go"), "a") as f:
        f.write(GUARD_TEST)
    mg = os.path.join(wt, TARGET_FILE)
    txt = open(mg).read()
    assert BUG_FROM in txt, "bug anchor not found"
    open(mg, "w").write(txt.replace(BUG_FROM, BUG_TO, 1))
    return wt

def run_variant(variant, graph_index, live_entry):
    wt = setup_worktree(variant)
    state = {"a": 0, "b": 0, "graph_index": graph_index, "opened": False, "last_test_ok": False}
    spec, impls = make_tools(variant, wt, state)
    messages = [{"role": "system", "content": system_prompt(variant)},
                {"role": "user", "content": TASK}]
    live_entry["messages"] = messages
    tokens = turns = calls_made = nudges = 0
    publish()
    for _ in range(MAX_TURNS):
        turns += 1
        r = chat(messages, spec)
        tokens += r.get("prompt_eval_count", 0) + r.get("eval_count", 0)
        msg = r.get("message", {})
        messages.append({"role": "assistant", "content": msg.get("content", ""),
                         **({"tool_calls": msg["tool_calls"]} if msg.get("tool_calls") else {})})
        live_entry.update(turns=turns, tokens=tokens, tool_calls=calls_made); publish()
        calls = parse_calls(msg)
        if not calls:
            # The model emitted no tool call (often prose). Nudge it to keep acting; only
            # give up after two consecutive empty turns.
            nudges += 1
            if nudges >= 2 or state.get("last_test_ok"):
                break
            messages.append({"role": "user", "content":
                "Keep going with a tool call: edit_file to apply the fix, then run_tests."})
            live_entry["messages"] = messages; publish()
            continue
        nudges = 0
        stop = False
        for name, args in calls:
            calls_made += 1
            res = impls.get(name, lambda a: f"unknown tool {name}")(args)
            messages.append({"role": "tool", "content": f"[{name}] {res}"})
            live_entry.update(tool_calls=calls_made); publish()
            if name == "run_tests" and state["last_test_ok"]:
                stop = True
        if stop:
            break
    ok, out = go_test(wt)
    live_entry.update(passed=ok, turns=turns, tokens=tokens, tool_calls=calls_made,
                      final_test_tail="\n".join(out.strip().split("\n")[-6:]), done=True)
    publish()
    sh(["git", "worktree", "remove", "--force", wt], ROOT)

def build_graph_index():
    # Build the symbol index from a HEAD checkout so line ranges match the experiment
    # worktrees (the live tree has uncommitted edits that would shift line numbers).
    bin = os.path.join(ROOT, "bin", "file-projections")
    sh(["go", "build", "-o", bin, "."], ROOT)
    wt = f"{OUT}/wt-index"
    sh(["git", "worktree", "remove", "--force", wt], ROOT)
    subprocess.run(["rm", "-rf", wt]); sh(["git", "worktree", "prune"], ROOT)
    sh(["git", "worktree", "add", "--detach", wt, "HEAD"], ROOT)
    subprocess.run([bin, "-analyzer", "go-symbols", "-source-root", ".",
                    "-out", "/tmp/self-model.projection"], cwd=wt, capture_output=True, text=True)
    txt = open("/tmp/self-model.projection").read() if os.path.isfile("/tmp/self-model.projection") else ""
    sh(["git", "worktree", "remove", "--force", wt], ROOT)
    return "\n".join(ln for ln in txt.splitlines() if ln.startswith("func "))

# ---- live HTTP server + report ---------------------------------------------------------
class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *a):
        pass

def start_server():
    handler = functools.partial(QuietHandler, directory=OUT)
    socketserver.TCPServer.allow_reuse_address = True
    httpd = socketserver.TCPServer(("127.0.0.1", PORT), handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    return httpd

def write_report(initial="null"):
    open(f"{OUT}/report.html", "w").write(REPORT_HTML.replace("__INITIAL__", initial))

def cmd_run():
    os.makedirs(OUT, exist_ok=True)
    write_report("null"); publish()
    httpd = start_server()
    url = f"http://127.0.0.1:{PORT}/report.html"
    subprocess.run(["open", url])
    print(f"live report: {url}", flush=True)
    LIVE["status"] = "running"
    LIVE["sync_demo"] = run_sync_demo(); publish()   # prove the offset-aware two-way sync up front
    graph_index = build_graph_index(); publish()
    for v in ("base", "graph", "proj"):
        print(f"=== {v} ===", flush=True)
        entry = {"variant": v, "passed": False, "turns": 0, "tool_calls": 0, "tokens": 0,
                 "done": False, "messages": []}
        LIVE["variants"].append(entry); publish()
        try:
            run_variant(v, graph_index, entry)
        except Exception as e:
            entry.update(done=True, final_test_tail=f"harness error: {e}"); publish()
        json.dump(entry, open(f"{OUT}/{v}.json", "w"), indent=2)
        print(f"  passed={entry['passed']} turns={entry['turns']} tokens={entry['tokens']}", flush=True)
    LIVE["status"] = "done"; publish()
    write_report(json.dumps(LIVE))    # embed final data for offline file:// viewing
    time.sleep(3)                      # let the open tab catch the final poll

def cmd_report():
    if os.path.isfile(f"{OUT}/live.json"):
        write_report(open(f"{OUT}/live.json").read())
        print(f"wrote {OUT}/report.html")

REPORT_HTML = r"""<!doctype html><html><head><meta charset=utf-8><title>clean experiment</title><style>
body{font:14px/1.55 -apple-system,Segoe UI,Roboto,sans-serif;max-width:940px;margin:1.5rem auto;padding:0 1rem;color:#1d1d1f;background:#fafafa}
h1{font-size:1.5rem;margin-bottom:.2rem}.sub{color:#888;margin-top:0}
.live{display:inline-block;font-size:.72rem;padding:.1rem .5rem;border-radius:10px;background:#eee;color:#555}
.live.running{background:#fff4d6;color:#a86b00}.live.done{background:#daf5e0;color:#127a2b}
table{border-collapse:collapse;width:100%;margin:1rem 0;background:#fff;box-shadow:0 1px 3px #0001;border-radius:8px;overflow:hidden}
th,td{padding:.55rem .8rem;text-align:left;border-bottom:1px solid #eee}th{background:#f4f4f6}
.pass{color:#127a2b;font-weight:600}.fail{color:#b00020;font-weight:600}.run{color:#a86b00;font-weight:600}
.best{background:#f1fbf3}
details{background:#fff;margin:.5rem 0;border:1px solid #eee;border-radius:8px;padding:.3rem .8rem}
summary{cursor:pointer;padding:.3rem 0;font-weight:600}
.msg{margin:.4rem 0;border-left:3px solid #ddd;padding-left:.6rem}
.msg .role{font-size:.68rem;text-transform:uppercase;letter-spacing:.05em;color:#888}
.msg.user{border-color:#3b82f6}.msg.assistant{border-color:#10b981}.msg.tool{border-color:#f59e0b}.msg.system{border-color:#bbb}
pre{white-space:pre-wrap;word-break:break-word;margin:.2rem 0;font:12px/1.45 ui-monospace,Menlo,monospace;background:#f7f7f9;padding:.5rem;border-radius:6px}
.legend{font-size:.8rem;color:#666}.count{color:#999;font-weight:400;font-size:.85em}
</style></head><body>
<h1>Clean dogfooding experiment <span id=status class=live>…</span></h1>
<p class=sub>Fix one planted bug in main.go so tests pass · model <code id=model></code> · <span id=ts></span></p>
<p class=legend>Agentic tool-calling, isolated git worktrees, sequential. Nothing pre-fed — every variant
pays for identification. <b>base</b>=search+read · <b>graph</b>=code-review-graph lookup ·
<b>proj</b>=<code>open_function(name)</code> hands the model a file-projections view it edits as if it
were source (it is never told); a quiet watcher syncs edits back to the real tree.</p>
<table><thead><tr><th>variant</th><th>result</th><th>turns</th><th>tool calls</th><th>tokens</th><th title="bytes of code the model had to ingest to locate+inspect">inspect chars</th><th>inspect calls</th></tr></thead><tbody id=rows></tbody></table>
<p class=legend><b>inspect chars/calls</b> = total content the model ingested from locate/read tools
(search, read_file, graph_lookup, open_function) — the honest measure of focused-view economy.</p>
<div id=syncdemo></div>
<h2>Conversations <span class=count>(live)</span></h2><div id=convs></div>
<script>
const INITIAL = __INITIAL__;
function esc(s){return (s||"").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;");}
function render(d){
  if(!d){return;}
  document.getElementById("model").textContent=d.model||"";
  document.getElementById("ts").textContent=d.ts||"";
  const st=document.getElementById("status"); st.textContent=d.status||""; st.className="live "+(d.status||"");
  let rows="";
  let best=null;
  (d.variants||[]).forEach(v=>{ if(v.done&&v.passed&&(best===null||v.tokens<best))best=v.tokens; });
  const INSPECT=new Set(["read_file","search","graph_lookup","open_function"]);
  const sd=d.sync_demo;
  if(sd){document.getElementById("syncdemo").innerHTML=
    `<h2>Two-way sync check <span class=count>${sd.ok?"✅ verified":"❌ failed"}</span></h2>`+
    `<p class=legend>${esc(sd.scenario)} — ${esc(sd.pointer)}.</p>`+
    `<table><tr><th>source before</th><th>edited view (proj)</th><th>source after (synced)</th></tr>`+
    `<tr><td><pre>${esc(sd.source_before)}</pre></td><td><pre>${esc(sd.edited_projection)}</pre></td><td><pre>${esc(sd.source_after)}</pre></td></tr></table>`;}
  (d.variants||[]).forEach(v=>{
    let res = !v.done ? "<span class=run>… running</span>" : (v.passed?"<span class=pass>✅ PASS</span>":"<span class=fail>❌ FAIL</span>");
    const cls = (v.done&&v.passed&&v.tokens===best)?" class=best":"";
    let ichars=0, icalls=0;
    (v.messages||[]).forEach(m=>{ if(m.role==="tool"){const n=(m.content||"").split("]")[0].replace("[","");
      if(INSPECT.has(n)){ichars+=(m.content||"").length;icalls++;}}});
    rows+=`<tr${cls}><td><b>${v.variant}</b></td><td>${res}</td><td>${v.turns||0}</td><td>${v.tool_calls||0}</td><td>${v.tokens||0}</td><td>${ichars}</td><td>${icalls}</td></tr>`;
  });
  document.getElementById("rows").innerHTML=rows;
  let convs="";
  (d.variants||[]).forEach(v=>{
    let items="";
    (v.messages||[]).forEach(m=>{
      let c=m.content||"";
      if(m.tool_calls)c+="\n"+JSON.stringify(m.tool_calls,null,2);
      items+=`<div class="msg ${m.role}"><span class=role>${m.role}</span><pre>${esc(c)}</pre></div>`;
    });
    const open=(!v.done)?" open":"";
    const tag=!v.done?"… running":(v.passed?"✅ pass":"❌ fail");
    convs+=`<details${open}><summary>${v.variant} — ${tag}, ${v.turns||0} turns, ${v.tokens||0} tokens</summary>${items}</details>`;
  });
  document.getElementById("convs").innerHTML=convs;
}
function poll(){fetch("live.json?_="+Date.now()).then(r=>r.json()).then(render).catch(()=>{});}
render(INITIAL);
poll(); setInterval(poll,1000);
</script></body></html>"""

# ---- offset sync demonstration ---------------------------------------------------------
def run_sync_demo():
    """The clean-experiment sync scenario: a projection line is commented out AND a new line
    is inserted right after it; the watcher writes both back to source, re-offsets the span
    pointer, and renumbers. Mirrors the spec's `42: foo` -> `42: // foo` + `43: foo` case."""
    import tempfile
    d = tempfile.mkdtemp()
    src = ["package p", "", "func f() {", "\tbad()", "\tafter()", "}"]
    open(os.path.join(d, TARGET_FILE), "w").write("\n".join(src))
    st = {"a": 4, "b": 4}
    load_span(d, st)
    proj_before = st["proj_text"]
    st["raw_span"] = ["\t// bad()", "\tfixed()"]          # comment out + insert below
    sync_projection(d, st)
    after = open(os.path.join(d, TARGET_FILE)).read()
    # invariants the spec asks for
    ok = (after.split("\n")[3:6] == ["\t// bad()", "\tfixed()", "\tafter()"]
          and st["b"] == 5 and st["proj_text"] == "4: \t// bad()\n5: \tfixed()")
    return {"ok": ok,
            "scenario": "projection line 4 `bad()` is commented out and a fixed line inserted after it",
            "projection_before": proj_before,
            "edited_projection": st["proj_text"],
            "source_before": "\n".join(src),
            "source_after": after.rstrip("\n"),
            "pointer": "span end b: 4 → 5 (offset +1 for the inserted line); lines renumbered"}

def cmd_selftest():
    demo = run_sync_demo()
    print(json.dumps(demo, indent=2))
    assert demo["ok"], "offset sync invariants failed"
    print("selftest OK: comment+insert synced, pointer offset 4->5, renumbered")

if __name__ == "__main__":
    mode = sys.argv[1] if len(sys.argv) > 1 else "all"
    if mode == "selftest":
        cmd_selftest()
    elif mode == "report":
        cmd_report()
    else:
        cmd_run()
        cmd_report()
