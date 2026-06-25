#!/usr/bin/env python3
"""Split the monolithic main.go into concern files under src/ (all package main).

Top-level declarations in gofmt'd Go begin at column 0 with func/type/var/const;
their bodies close with `}` or `)` at column 0, so the *next* col0 decl keyword
marks a clean boundary. We bucket each declaration (with its leading doc///go:embed
comments) into a file by name/receiver, write `package main` + the chunks, then let
goimports regenerate per-file imports. Nothing semantic changes — it's one package.
"""
import re, sys, os

SRC = sys.argv[1] if len(sys.argv) > 1 else "main.go"
OUT = "src"
lines = open(SRC).read().split("\n")

# 1) find top-level decl boundaries (col0 func/type/var/const)
declkw = re.compile(r'^(func|type|var|const)\b')
starts = [i for i, l in enumerate(lines) if declkw.match(l)]

# extend each start back over immediately-preceding comment lines (doc + //go:embed)
def with_leading(idx):
    j = idx
    while j - 1 >= 0 and (lines[j-1].startswith("//") or lines[j-1].startswith("/*") or lines[j-1].startswith(" *") or lines[j-1].endswith("*/")):
        j -= 1
    return j

chunks = []
for n, s in enumerate(starts):
    cs = with_leading(s)
    end = starts[n+1] if n+1 < len(starts) else len(lines)
    cend = with_leading(end) if n+1 < len(starts) else end
    chunks.append((s, lines[cs:cend]))

def declname(line):
    m = re.match(r'^func\s+(?:\(\s*\w+\s+\*?(\w+)\s*\)\s*)?(\w+)', line)
    if m:
        return (m.group(1) or "", m.group(2) or "")
    m = re.match(r'^type\s+(\w+)', line)
    if m: return ("", m.group(1))
    m = re.match(r'^(?:var|const)\s+(\w+)', line)
    if m: return ("", m.group(1))
    return ("", "(block)")

def bucket(recv, name):
    n = name
    def has(*ks): return any(k in n for k in ks)
    # ui
    if recv == "uiServer" or n.startswith("handle") or n in ("RunUI","writeJSON","collectSymbols","uiAnalyzerLanguages","suggestUIDefaults","suggestUIMethod","suggestUIExamples") or n.startswith("suggestUI") or n in ("uiSymbol","uiDefaults","uiServer"):
        return "ui.go"
    # joern
    if ("Joern" in n or "joern" in n) or n in ("RunBuildCPG","prepareJoernLens","ensureJoern","execJoern","runJoernScript","runJoernQuery","RunJoernVarFlow","RunJoernControlFlow","AnalyzeJoernVarFlow","execJoernParse"):
        return "joern.go"
    # service graph
    if n.startswith("sg") or n in ("AnalyzeServiceGraph","resolveTSImport","findGoHandler","serviceOf","indexOfNode","appendCSV","firstNonEmpty","trimServiceLabel") or n.startswith("ts") and "Import" in n:
        return "servicegraph.go"
    # go analyzer / unroller
    if recv == "goUnroller" or n in ("AnalyzeGoSymbols","AnalyzeGoUnrolledProgram","parseGoFile","goCallGraph","findCalls","scanGoFiles","goGuardCond","goCondAfterInit","simpleGoCall","goFuncRef","newGoUnroller","GoFile","GoFunc","GoDecl","goUnroller") or n.startswith("goType") or n.startswith("goFunc") or n.startswith("goPkg"):
        return "analyzers_go.go"
    # java analyzers / unroller / cfg
    if recv in ("javaUnroller",) or n.startswith("AnalyzeControlFlow") or n in ("AnalyzeUnrolledProgram","AnalyzeDataFlow","AnalyzeObjectFlow","AnalyzeCPGMethods","AnalyzeFlow","AnalyzeEntrypoints","AnalyzeExitpoints","AnalyzeEntryToExit","AnalyzeJavaVarFlowFallback","parseJavaFile","parseJavaMethods","javaMethodName","looksLikeJavaMethod","detectElse","extractCond","evalJavaCond","JavaFile","JavaMethod","newJavaUnroller","parseForcedBranches","parseInlineDepth","parseIDSet","parseUnrollInputs","javaParamNames","splitArgs") or n.startswith("cfg") or n.startswith("uiJava") or n.startswith("uiGo") or n.startswith("classRE") or n in ("ifRE","retRE","callRE"):
        return "analyzers_java.go"
    # js
    if n in ("AnalyzeJSEvents","AnalyzeJSONL") or n.startswith("js") or n.startswith("AnalyzeJS"):
        return "analyzers_js.go"
    # misc analyzers
    if n in ("AnalyzeBookmark","AnalyzeAstGrep"):
        return "analyzers_misc.go"
    # assumptions / unroll shared
    if n in ("withGuard","rangeExits","inlineAssignTarget","rewriteInlinedReturns","unrollViewLines","unrollChoices","unrollCalls","unrollDecisionFacts","unrollLineView","branchChoice","inlineCallChoice","unrollLine","uiBranchRE","uiExitStmtRE","inlineLHSRE","inlineRetRE","uiJavaClassRE","uiGoDeclRE","uiJavaLocalRE","uiGoLocalRE"):
        return "assumptions.go"
    # projection
    if n in ("RenderProjection","SyncProjection","projectionBody") or n.startswith("parseProjection") or has("Projection") and recv=="":
        return "projection.go"
    # registry
    if n in ("DefaultRegistry","ExecuteLens","Registry","AnalyzerFunc") or recv=="AnalyzerFunc":
        return "registry.go"
    # config / scan
    if recv == "projectScan" or n in ("LoadConfig","saveConfig","scanProject","commonDir","wizardLang","defaultExcludeDirs","shouldSkipDir","isScannableSource","projectScan","sampleBM") or n.startswith("suggest"):
        return "config.go"
    # menu/wizard/watch
    if n in ("RunMenu","RunWizard","RunWatch","RunWatchUntil") or n.startswith("menu") or n.startswith("wizard"):
        return "menu.go"
    # cli
    if n in ("main","printHelp","subConfigPath","RunPerf","must"):
        return "cli.go"
    # core types
    if recv=="" and re.match(r'^type ', name_line_of(name)) if False else False:
        return "types.go"
    return None  # decide below

# we need the original decl line for type detection; map name->line
name_line = {}
for s, body in chunks:
    name_line[id(body)] = body  # placeholder

def classify(body):
    # find the decl line (first non-comment line)
    dl = next((l for l in body if declkw.match(l)), body[0])
    recv, name = declname(dl)
    b = bucket(recv, name)
    if b: return b
    if dl.startswith("type "):
        return "types.go"
    if dl.startswith("var ") or dl.startswith("const "):
        # globals: embeds + version + small vars -> types.go
        return "types.go"
    return "util.go"

# fix: name_line_of referenced above in a dead branch; ignore
def name_line_of(n): return ""

files = {}
for s, body in chunks:
    f = classify(body)
    files.setdefault(f, []).append("\n".join(body).rstrip())

os.makedirs(OUT, exist_ok=True)
DOC = {
 "types.go":"// Package main: core data types, embedded assets, and small globals.",
 "cli.go":"// CLI entry point: flag parsing and subcommand dispatch.",
 "ui.go":"// The `ui` web studio: HTTP handlers + embedded single-page app.",
 "joern.go":"// Joern/CPG integration: build, parse, and query the code property graph.",
 "registry.go":"// Analyzer registry and the ExecuteLens dispatch.",
 "projection.go":"// Projection rendering and two-way sync with source.",
 "config.go":"// Config loading and project scanning / language detection.",
 "analyzers_go.go":"// Go frontend: symbols, call graph, and the Go unrolled-program adapter.",
 "analyzers_java.go":"// Java frontend: control/data/object flow, CPG methods, unrolled-program.",
 "analyzers_js.go":"// JS/TS frontend: event surface + jsonl. (NOTE: no TS unrolled-program yet.)",
 "analyzers_misc.go":"// Language-agnostic analyzers: bookmark/extract, ast-grep.",
 "assumptions.go":"// Cross-language unroll/assumption helpers (guards, inlining, line views).",
 "servicegraph.go":"// Cross-service graph: TS imports + Go routes + the TS->Go operation seam.",
 "menu.go":"// Interactive commands: menu, setup wizard, watch.",
 "util.go":"// Small shared helpers (fs, hashing, string utils).",
}
for f, parts in files.items():
    doc = DOC.get(f, "")
    open(os.path.join(OUT, f), "w").write("package main\n\n" + (doc+"\n\n" if doc else "") + "\n\n".join(parts) + "\n")
    print(f"{f}: {len(parts)} decls")
print("total chunks", len(chunks), "->", len(files), "files")
