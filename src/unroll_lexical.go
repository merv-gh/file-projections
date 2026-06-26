package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Shared lexical unroller — the common core behind the Go and TS/JS
// unrolled-program adapters. Both used to re-implement the same walk (iterate a
// function body, maintain a guard stack, detect a local call, inline or record
// it, emit unrollLine). They now share this core and differ only in a tiny
// lexAdapter: how to find a function, what a guard header is, and what a local
// call is. (Java keeps its own recursive branch-evaluating unroller in
// analyzers_java.go because it additionally resolves inputs and toggles branches;
// folding that in here would regress it.)
//
// Guard tracking is brace-depth based, which is robust to formatting and works for
// both C-like Go and TS.

// lexFunc locates a function's body within its file as 0-based inclusive line
// indices [BodyStart, BodyEnd] (the first body line through the line before the
// closing brace).
type lexFunc struct {
	Rel       string
	BodyStart int
	BodyEnd   int
}

// lexAdapter is the per-language seam the shared core calls into.
type lexAdapter interface {
	// lookup resolves a function by name, preferring one declared in file when the
	// same name exists in several files.
	lookup(name, file string) (lexFunc, bool)
	// guardCond reports the condition an if/else-if/for/while header introduces, or
	// ok=false for a non-guard line.
	guardCond(trim string) (cond string, ok bool)
	// callName returns the name of a locally-defined function called on a line, or "".
	callName(trim string) string
	// known reports whether name is a locally-defined function (inline candidate).
	known(name string) bool
	tool() string  // projection block tool tag, e.g. "unrolled-program:go"
	scope() string // the scope fact text
}

type lexicalUnroller struct {
	cfg         Config
	lens        LensConfig
	ad          lexAdapter
	inlineDepth int
	inlineSkips map[string]bool
	calls       []inlineCallChoice
	callSeen    map[string]bool
}

// runLexicalUnroll is the entry point both adapters call after building their
// function table. It produces the same Projection shape the Java unroller does
// (two-way block + scope/lguard/call facts) so every downstream consumer — render,
// sync, UI timeline/assumptions, side-effects — is identical across languages.
func runLexicalUnroll(cfg Config, lens LensConfig, ad lexAdapter) (Projection, error) {
	file := lens.Params["file"]
	method := lens.Params["method"]
	if file == "" {
		return Projection{}, fmt.Errorf("%s: params.file is required", ad.tool())
	}
	if method == "" {
		return Projection{}, fmt.Errorf("%s: params.method is required", ad.tool())
	}
	u := &lexicalUnroller{
		cfg: cfg, lens: lens, ad: ad,
		inlineDepth: parseInlineDepth(lens.Params["inline_depth"]),
		inlineSkips: parseIDSet(lens.Params["inline_skips"]),
		callSeen:    map[string]bool{},
	}
	lines, err := u.unroll(file, method, 0, nil)
	if err != nil {
		return Projection{}, err
	}
	var body []string
	var origins []LineOrigin
	var lineGuards [][]string
	for _, line := range lines {
		body = append(body, line.code)
		src := filepath.ToSlash(filepath.Join(lens.SourceRoot, line.file))
		origins = append(origins, LineOrigin{SrcFile: src, Line: line.line, SrcHash: hash(line.code + "\n")})
		lineGuards = append(lineGuards, line.guards)
	}
	if len(body) == 0 {
		body = append(body, "// no executable path found")
		lineGuards = append(lineGuards, nil)
	}
	p := Projection{Sync: "two-way"}
	p.Blocks = append(p.Blocks, ProjectionBlock{
		ID: method, File: file, Mode: "unrolled", Tool: ad.tool(),
		Lines: body, LineOrigins: origins, LineGuards: lineGuards, Sync: "two-way",
	})
	p.Facts = append(p.Facts, ProjectionFact{ID: "scope", Tool: "unrolled-program", Text: ad.scope()})
	for n, gd := range lineGuards {
		if len(gd) > 0 {
			p.Facts = append(p.Facts, ProjectionFact{ID: fmt.Sprintf("lguard-%d", n+1), Tool: "unrolled-program", Text: strings.Join(gd, " && ")})
		}
	}
	for i, c := range u.calls {
		if b, err := json.Marshal(c); err == nil {
			p.Facts = append(p.Facts, ProjectionFact{ID: fmt.Sprintf("call-%d", i+1), Tool: "unrolled-program", Text: string(b)})
		}
	}
	// Tag the lines that perform side effects (IO/network/db/process) using the
	// language's first-class markers — so the unrolled view answers "what does this
	// path touch" without a separate scan.
	appendUnrollSideEffectFacts(&p, langForTool(ad.tool()), body, origins)
	return p, nil
}

// langForTool maps an unroller tool tag ("unrolled-program:go") to a language id.
func langForTool(tool string) string {
	if i := strings.LastIndex(tool, ":"); i >= 0 {
		switch tool[i+1:] {
		case "go":
			return "go"
		case "ts":
			return "js"
		case "java":
			return "java"
		}
	}
	return ""
}

func (u *lexicalUnroller) unroll(file, name string, depth int, callerGuards []string) ([]unrollLine, error) {
	if depth > 10 {
		return nil, fmt.Errorf("%s: recursion limit while inlining %s", u.ad.tool(), name)
	}
	fn, ok := u.ad.lookup(name, file)
	if !ok {
		return nil, fmt.Errorf("%s: function %q not found", u.ad.tool(), name)
	}
	path := filepath.Join(u.cfg.Root, u.lens.SourceRoot, filepath.FromSlash(fn.Rel))
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	var out []unrollLine
	// Guard stack keyed by brace depth: each header pushes its condition at the depth
	// its body occupies, so a line names every condition guarding it.
	type gframe struct {
		depth int
		cond  string
	}
	var stack []gframe
	curGuards := func() []string {
		g := append([]string{}, callerGuards...)
		for _, f := range stack {
			g = append(g, f.cond)
		}
		return g
	}
	braceDepth := 0
	for i := fn.BodyStart; i <= fn.BodyEnd && i < len(lines); i++ {
		raw := lines[i]
		trim := strings.TrimSpace(stripLineComment(raw))
		if trim == "" {
			continue
		}
		opens := strings.Count(trim, "{")
		closes := strings.Count(trim, "}")
		if strings.HasPrefix(trim, "}") {
			for len(stack) > 0 && stack[len(stack)-1].depth >= braceDepth {
				stack = stack[:len(stack)-1]
			}
		}
		if trim == "{" || trim == "}" || trim == "})" || trim == "});" {
			braceDepth += opens - closes
			continue
		}
		guards := curGuards()
		if called := u.ad.callName(trim); called != "" && called != name && u.ad.known(called) {
			expanded := depth < u.inlineDepth && !u.inlineSkips[fmt.Sprintf("%s:%d", fn.Rel, i+1)]
			u.recordCall(fn.Rel, i+1, called, expanded, depth)
			if expanded {
				part, err := u.unroll("", called, depth+1, guards)
				if err != nil {
					return nil, err
				}
				out = append(out, part...)
				braceDepth += opens - closes
				continue
			}
		}
		out = append(out, unrollLine{code: strings.TrimRight(raw, " \t"), file: fn.Rel, line: i + 1, guards: guards})
		if cond, ok := u.ad.guardCond(trim); ok {
			stack = append(stack, gframe{depth: braceDepth + 1, cond: cond})
		}
		braceDepth += opens - closes
		if braceDepth < 0 {
			braceDepth = 0
		}
	}
	return out, nil
}

func (u *lexicalUnroller) recordCall(file string, line int, name string, expanded bool, depth int) {
	id := fmt.Sprintf("%s:%d", file, line)
	if u.callSeen[id] {
		return
	}
	u.callSeen[id] = true
	origin := filepath.ToSlash(filepath.Join(u.lens.SourceRoot, file)) + fmt.Sprintf(":%d", line)
	u.calls = append(u.calls, inlineCallChoice{ID: id, Name: name, Origin: origin, Expanded: expanded, Depth: depth})
}
