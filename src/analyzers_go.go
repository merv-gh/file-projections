package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Go frontend: symbols, call graph, and the Go unrolled-program adapter.

// Go symbols analyzer: language adapter, not renderer/core.
type GoFile struct {
	Rel   string
	Lines []string
	Types []GoDecl
	Funcs []GoFunc
}

type GoDecl struct {
	Name string
	Kind string
	Line int
	End  int
	Sig  string
}

type GoFunc struct {
	Name  string
	Line  int
	End   int
	Sig   string
	Calls []string
}

func AnalyzeGoSymbols(cfg Config, lens LensConfig) (Projection, error) {
	files, err := scanGoFiles(cfg, lens)
	if err != nil {
		return Projection{}, err
	}

	funcNames := map[string]bool{}
	for _, f := range files {
		for _, fn := range f.Funcs {
			funcNames[fn.Name] = true
		}
	}
	for fi := range files {
		for i := range files[fi].Funcs {
			fn := &files[fi].Funcs[i]
			fn.Calls = findCalls(fn.Name, files[fi].Lines[fn.Line-1:fn.End], funcNames)
		}
	}

	var p Projection
	for _, f := range files {
		var typeLines []string
		for _, t := range f.Types {
			typeLines = append(typeLines, fmt.Sprintf("%s %s lines=%d-%d :: %s", t.Kind, t.Name, t.Line, t.End, t.Sig))
		}
		if len(typeLines) > 0 {
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "types", File: f.Rel, Mode: "types", Tool: "go-symbols", Lines: typeLines})
		}

		var funcLines []string
		for _, fn := range f.Funcs {
			funcLines = append(funcLines, fmt.Sprintf("%s lines=%d-%d calls=%s", fn.Sig, fn.Line, fn.End, strings.Join(fn.Calls, ",")))
		}
		if len(funcLines) > 0 {
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "functions", File: f.Rel, Mode: "functions", Tool: "go-symbols", Lines: funcLines})
		}
	}

	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "main-callgraph", File: "model", Mode: "callgraph", Tool: "go-symbols", Lines: goCallGraph(files)})
	p.Facts = append(p.Facts, ProjectionFact{ID: "core", Tool: "go-symbols", Text: "Run -> ExecuteLens -> Analyzer -> RenderProjection is the generic path."})
	p.Facts = append(p.Facts, ProjectionFact{ID: "adapters", Tool: "go-symbols", Text: "Language/tool-specific behavior lives behind analyzer adapters registered in DefaultRegistry."})
	return p, nil
}

func scanGoFiles(cfg Config, lens LensConfig) ([]GoFile, error) {
	root := filepath.Join(cfg.Root, lens.SourceRoot)
	allowed := map[string]bool{}
	for _, inc := range lens.Include {
		allowed[filepath.ToSlash(inc)] = true
		allowed[filepath.Base(inc)] = true
	}

	var out []GoFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if shouldSkipDir(cfg, path, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(cfg.Root, path)
		rel = filepath.ToSlash(rel)
		if len(allowed) > 0 && !allowed[rel] && !allowed[filepath.Base(rel)] {
			return nil
		}
		gf, err := parseGoFile(cfg.Root, path)
		if err != nil {
			return err
		}
		out = append(out, gf)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, err
}

func parseGoFile(root, path string) (GoFile, error) {
	lines, err := readLines(path)
	if err != nil {
		return GoFile{}, err
	}
	rel, _ := filepath.Rel(root, path)
	rel = filepath.ToSlash(rel)
	gf := GoFile{Rel: rel, Lines: lines}

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		_ = goPkgRE.FindStringSubmatch(line)
		if m := goTypeRE.FindStringSubmatch(line); m != nil {
			end := i + 1
			if strings.Contains(line, "{") {
				if close, err := findClosingBrace(lines, i); err == nil {
					end = close + 1
				}
			}
			gf.Types = append(gf.Types, GoDecl{Name: m[1], Kind: m[2], Line: i + 1, End: end, Sig: trimBeforeBrace(line)})
		}
		if m := goFuncRE.FindStringSubmatch(line); m != nil {
			close, err := findClosingBrace(lines, i)
			if err != nil {
				continue
			}
			gf.Funcs = append(gf.Funcs, GoFunc{Name: m[2], Line: i + 1, End: close + 1, Sig: trimBeforeBrace(line)})
			i = close
		}
	}
	return gf, nil
}

func findCalls(current string, lines []string, funcNames map[string]bool) []string {
	rx := regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	seen := map[string]bool{}
	var calls []string
	for _, line := range lines {
		for _, m := range rx.FindAllStringSubmatch(stripLineComment(line), -1) {
			name := m[1]
			if name == current || !funcNames[name] || seen[name] {
				continue
			}
			seen[name] = true
			calls = append(calls, name)
		}
	}
	sort.Strings(calls)
	return calls
}

func goCallGraph(files []GoFile) []string {
	graph := map[string][]string{}
	for _, f := range files {
		for _, fn := range f.Funcs {
			graph[fn.Name] = fn.Calls
		}
	}
	var lines []string
	var walk func(string, int, map[string]bool)
	walk = func(name string, depth int, seen map[string]bool) {
		prefix := strings.Repeat("  ", depth)
		if seen[name] {
			lines = append(lines, prefix+"-> "+name+" (seen)")
			return
		}
		lines = append(lines, prefix+"-> "+name)
		seen[name] = true
		calls := append([]string{}, graph[name]...)
		sort.Strings(calls)
		for _, c := range calls {
			walk(c, depth+1, seen)
		}
	}
	walk("main", 0, map[string]bool{})
	return lines
}

// ---------------------------------------------------------------------------
// service-graph: a cross-service, cross-language map of a multi-folder repo.
// Nodes are source files (one per .ts/.tsx/.go), grouped by service. Edges are:
//   - import   : TS `import ... from "spec"` (relative + workspace-package), Go
//                internal imports — the intra/cross-service module wiring.
//   - registers: Go router `rb.AddRoute("op", deps.Handler)` → the handler file.
//   - api-call : the custom TS→Go SEAM — a TS file that references a Go operation
//                id (the same name Go registered) is wired to that Go handler.
// The graph is emitted as one JSON fact the UI renders (and exports to mermaid),
// and every node carries file/line so a click drills into the unrolled-program
// lens (assumptions + object timeline) for that file — cross-service.
// ---------------------------------------------------------------------------

type goUnroller struct {
	cfg         Config
	lens        LensConfig
	functions   map[string]goFuncRef
	inlineDepth int
	inlineSkips map[string]bool
	calls       []inlineCallChoice
	callSeen    map[string]bool
}

type goFuncRef struct {
	file string
	fn   GoFunc
}

func AnalyzeGoUnrolledProgram(cfg Config, lens LensConfig) (Projection, error) {
	file := lens.Params["file"]
	method := lens.Params["method"]
	if file == "" {
		return Projection{}, errors.New("unrolled-program go: params.file is required")
	}
	if method == "" {
		return Projection{}, errors.New("unrolled-program go: params.method is required")
	}
	u, err := newGoUnroller(cfg, lens)
	if err != nil {
		return Projection{}, err
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
	p := Projection{Sync: "two-way"}
	p.Blocks = append(p.Blocks, ProjectionBlock{
		ID: method, File: file, Mode: "unrolled", Tool: "unrolled-program:go",
		Lines: body, LineOrigins: origins, LineGuards: lineGuards, Sync: "two-way",
	})
	p.Facts = append(p.Facts, ProjectionFact{ID: "scope", Tool: "unrolled-program", Text: "go adapter: editable straight-line function path; each line syncs back to its original source line"})
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
	return p, nil
}

func newGoUnroller(cfg Config, lens LensConfig) (*goUnroller, error) {
	files, err := scanGoFiles(cfg, lens)
	if err != nil {
		return nil, err
	}
	u := &goUnroller{
		cfg: cfg, lens: lens, functions: map[string]goFuncRef{},
		inlineDepth: parseInlineDepth(lens.Params["inline_depth"]),
		inlineSkips: parseIDSet(lens.Params["inline_skips"]),
		callSeen:    map[string]bool{},
	}
	for _, f := range files {
		rel := strings.TrimPrefix(strings.TrimPrefix(f.Rel, filepath.ToSlash(lens.SourceRoot)+"/"), "./")
		for _, fn := range f.Funcs {
			u.functions[fn.Name] = goFuncRef{file: rel, fn: fn}
		}
	}
	return u, nil
}

func (u *goUnroller) unroll(file, name string, depth int, callerGuards []string) ([]unrollLine, error) {
	if depth > 10 {
		return nil, fmt.Errorf("unrolled-program go: recursion limit while inlining %s", name)
	}
	ref, ok := u.functions[name]
	if !ok {
		return nil, fmt.Errorf("unrolled-program go: function %q not found", name)
	}
	if file != "" && ref.file != file {
		if byFile, ok := u.findGoFunc(file, name); ok {
			ref = byFile
		}
	}
	path := filepath.Join(u.cfg.Root, u.lens.SourceRoot, filepath.FromSlash(ref.file))
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	var out []unrollLine
	// Per-line assumptions: track enclosing if/for conditions by indentation so each
	// line carries the guard set that must hold to reach it (cross-service drill-in).
	type gframe struct {
		indent int
		cond   string
	}
	var stack []gframe
	curGuards := func() []string {
		g := append([]string{}, callerGuards...)
		for _, f := range stack {
			g = append(g, f.cond)
		}
		return g
	}
	for i := ref.fn.Line; i <= ref.fn.End-2 && i < len(lines); i++ {
		raw := lines[i]
		trim := strings.TrimSpace(stripLineComment(raw))
		if trim == "" || trim == "{" || trim == "}" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		guards := curGuards()
		if called := simpleGoCall(trim); called != "" && called != name {
			if _, ok := u.functions[called]; ok {
				expanded := depth < u.inlineDepth && !u.inlineSkips[fmt.Sprintf("%s:%d", ref.file, i+1)]
				u.recordCall(ref.file, i+1, called, expanded, depth)
				if !expanded {
					out = append(out, unrollLine{code: strings.TrimRight(raw, " \t"), file: ref.file, line: i + 1, guards: guards})
					continue
				}
				part, err := u.unroll("", called, depth+1, guards)
				if err != nil {
					return nil, err
				}
				out = append(out, part...)
				continue
			}
		}
		out = append(out, unrollLine{code: strings.TrimRight(raw, " \t"), file: ref.file, line: i + 1, guards: guards})
		if cond, ok := goGuardCond(trim); ok {
			stack = append(stack, gframe{indent: indent, cond: cond})
		}
	}
	return out, nil
}

// goGuardCond extracts the condition a Go `if`/`for`/`else if` header introduces so
// the lines it governs can name their assumptions. Returns ok=false for non-guards.
func goGuardCond(trim string) (string, bool) {
	if !strings.HasSuffix(trim, "{") {
		return "", false
	}
	body := strings.TrimSpace(strings.TrimSuffix(trim, "{"))
	switch {
	case strings.HasPrefix(body, "if "):
		return goCondAfterInit(strings.TrimPrefix(body, "if ")), true
	case strings.HasPrefix(body, "} else if "):
		return goCondAfterInit(strings.TrimPrefix(body, "} else if ")), true
	case strings.HasPrefix(body, "else if "):
		return goCondAfterInit(strings.TrimPrefix(body, "else if ")), true
	case strings.HasPrefix(body, "for ") && body != "for":
		return "loop: " + strings.TrimSpace(strings.TrimPrefix(body, "for ")), true
	}
	return "", false
}

// goCondAfterInit drops a Go `if init; cond` init statement, keeping the condition.
func goCondAfterInit(s string) string {
	if i := strings.LastIndex(s, ";"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return strings.TrimSpace(s)
}

func (u *goUnroller) recordCall(file string, line int, name string, expanded bool, depth int) {
	id := fmt.Sprintf("%s:%d", file, line)
	if u.callSeen[id] {
		return
	}
	u.callSeen[id] = true
	origin := filepath.ToSlash(filepath.Join(u.lens.SourceRoot, file)) + fmt.Sprintf(":%d", line)
	u.calls = append(u.calls, inlineCallChoice{ID: id, Name: name, Origin: origin, Expanded: expanded, Depth: depth})
}

func (u *goUnroller) findGoFunc(file, name string) (goFuncRef, bool) {
	for _, ref := range u.functions {
		if ref.file == file && ref.fn.Name == name {
			return ref, true
		}
	}
	return goFuncRef{}, false
}

func simpleGoCall(trim string) string {
	if strings.HasPrefix(trim, "return ") {
		trim = strings.TrimSpace(strings.TrimPrefix(trim, "return "))
	}
	if strings.Contains(trim, ":=") {
		_, rhs, _ := strings.Cut(trim, ":=")
		trim = strings.TrimSpace(rhs)
	} else if strings.Contains(trim, "=") {
		_, rhs, _ := strings.Cut(trim, "=")
		trim = strings.TrimSpace(rhs)
	}
	m := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*\(`).FindStringSubmatch(trim)
	if m == nil {
		return ""
	}
	return m[1]
}
