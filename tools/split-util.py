#!/usr/bin/env python3
"""Drain the catch-all util.go into the right concern files (better classifier).

Same col0-boundary chunker as split-main.py, applied only to src/util.go. Truly
generic helpers stay; everything else is appended to its concern file.
"""
import re, os

SRCDIR = "src"
F = os.path.join(SRCDIR, "util.go")
lines = open(F).read().split("\n")
declkw = re.compile(r'^(func|type|var|const)\b')
starts = [i for i, l in enumerate(lines) if declkw.match(l)]

def with_leading(idx):
    j = idx
    while j-1 >= 0 and (lines[j-1].startswith("//") or lines[j-1].startswith("/*") or lines[j-1].startswith(" *") or lines[j-1].endswith("*/")):
        j -= 1
    return j

# header = lines before first decl (package + import + file doc)
first = with_leading(starts[0])
chunks = []
for n, s in enumerate(starts):
    cs = with_leading(s)
    end = starts[n+1] if n+1 < len(starts) else len(lines)
    cend = with_leading(end) if n+1 < len(starts) else end
    chunks.append(lines[cs:cend])

def declname(line):
    m = re.match(r'^func\s+(?:\(\s*\w+\s+\*?(\w+)\s*\)\s*)?(\w+)', line)
    if m: return (m.group(1) or "", m.group(2) or "")
    m = re.match(r'^type\s+(\w+)', line)
    if m: return ("", m.group(1))
    m = re.match(r'^(?:var|const)\s+(\w+)', line)
    if m: return ("", m.group(1))
    return ("", "")

KEEP = {"util.go"}  # generic helpers we leave in place
def classify(body):
    dl = next((l for l in body if declkw.match(l)), body[0])
    recv, name = declname(dl)
    n = name; ln = (recv+" "+name).lower()
    def hasw(*ks): return any(k.lower() in ln for k in ks)
    # joern / farm / cpg build
    if hasw("joern","farm","zipDir","sourceManifest","ensureJoern","execJoern","runTool","cpg"):
        return "joern.go"
    # java frontend + cfg + var-flow fallback
    if recv in ("javaUnroller",) or hasw("java","cfg","varflow","fallback","branch","walkNodes","parseCFG","detectElse","extractCond","stripJavaStrings","locateJavaMethod"):
        return "analyzers_java.go"
    # js/ts frontend
    if hasw("jsfile","scanjs","parsejs","jsevent") or name in ("parseJSFile","scanJSFiles","JSFile"):
        return "analyzers_web.go"
    # ast-grep / regex scanning (misc analyzers)
    if hasw("astgrep","scanregex","grephit","astgrep"):
        return "analyzers_misc.go"
    # projection / sync / drop-ins
    if hasw("sync","projection","dropin","scattered","refreshtwoway","manifest") or name in ("expandDropIns","parseDropIn"):
        return "projection.go"
    # config / scanning / language detection
    if hasw("resolvesourcefile","rootlanguage","isscannable","sourceroot","language"):
        return "config.go"
    return "util.go"

groups = {}
for c in chunks:
    groups.setdefault(classify(c), []).append("\n".join(c).rstrip())

# rewrite util.go with header + only the kept chunks
header = "\n".join(lines[:first]).rstrip()
kept = groups.get("util.go", [])
open(F, "w").write(header + "\n\n" + "\n\n".join(kept) + "\n")
print("util.go keeps", len(kept), "decls")

# append moved chunks to their files (just the decls; goimports fixes imports)
for f, parts in groups.items():
    if f == "util.go": continue
    path = os.path.join(SRCDIR, f)
    with open(path, "a") as fh:
        fh.write("\n\n" + "\n\n".join(parts) + "\n")
    print(f"-> {f}: +{len(parts)} decls")
