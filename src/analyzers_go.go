package main

import (
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

type goFuncRef struct {
	file string
	fn   GoFunc
}

// goLexAdapter adapts Go to the shared lexical unroller (unroll_lexical.go). All
// the walk/guard/inline machinery lives there; this only answers Go-specific
// questions (find a func, recognize a guard header, recognize a local call).
type goLexAdapter struct {
	lens      LensConfig
	functions map[string]goFuncRef
}

func (a *goLexAdapter) lookup(name, file string) (lexFunc, bool) {
	ref, ok := a.functions[name]
	if !ok {
		return lexFunc{}, false
	}
	if file != "" && ref.file != file {
		for _, r := range a.functions {
			if r.file == file && r.fn.Name == name {
				ref = r
				break
			}
		}
	}
	// GoFunc.Line is the 1-based signature line; body runs from that index (the line
	// after the signature, 0-based == Line) to End-2 (the line before the close brace).
	return lexFunc{Rel: ref.file, BodyStart: ref.fn.Line, BodyEnd: ref.fn.End - 2}, true
}
func (a *goLexAdapter) guardCond(trim string) (string, bool) { return goGuardCond(trim) }
func (a *goLexAdapter) callName(trim string) string          { return simpleGoCall(trim) }
func (a *goLexAdapter) known(name string) bool               { _, ok := a.functions[name]; return ok }
func (a *goLexAdapter) tool() string                         { return "unrolled-program:go" }
func (a *goLexAdapter) scope() string {
	return "go adapter: editable straight-line function path; each line syncs back to its original source line"
}

func AnalyzeGoUnrolledProgram(cfg Config, lens LensConfig) (Projection, error) {
	files, err := scanGoFiles(cfg, lens)
	if err != nil {
		return Projection{}, err
	}
	ad := &goLexAdapter{lens: lens, functions: map[string]goFuncRef{}}
	for _, f := range files {
		rel := strings.TrimPrefix(strings.TrimPrefix(f.Rel, filepath.ToSlash(lens.SourceRoot)+"/"), "./")
		for _, fn := range f.Funcs {
			ad.functions[fn.Name] = goFuncRef{file: rel, fn: fn}
		}
	}
	return runLexicalUnroll(cfg, lens, ad)
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
