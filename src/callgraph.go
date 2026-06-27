package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Structural call graph — the Phase 2 precision lever, built in pure Go (no CGO,
// no tree-sitter dependency; the SymbolScanner/FuncBodies seam in language.go is
// what a tree-sitter backend would later swap in). It answers "who *actually*
// calls X" and "if I change X, what breaks" with far fewer false positives than
// the lexical `callers` lens, because a call only counts when:
//
//   - it occurs inside a known function body (not a comment, string, or the def), and
//   - the called name resolves to a function declared in the same source root.
//
// This is scope-aware, not type-aware: like the rest of the non-joern path it does
// not resolve receivers/overloads, so two same-named methods on different types
// still merge into one node. That residual ambiguity is reported as a fact and is
// the honest ceiling of a dependency-free backend (joern remains the type-precise
// upgrade). Within those limits it turns the pile of lexical lenses into a graph:
// edges compose, so transitive impact sets are just a BFS over it.

// FuncSpan is a function/method body located in a file: name + 1-based inclusive
// line range. Each language's existing body parser produces these (language.go).
type FuncSpan struct {
	Name  string
	File  string // source-root-relative, slash path
	Start int    // 1-based signature line
	End   int    // 1-based closing line
}

// CallEdge is a resolved call site: function Caller (at File:Line) calls Callee.
type CallEdge struct {
	Caller string
	Callee string
	File   string
	Line   int
}

// CallGraph is the resolved call graph for one source root.
type CallGraph struct {
	Funcs     []FuncSpan                 // every declared function/method
	Defs      map[string][]FuncSpan      // name -> declarations (>1 = ambiguous name)
	Edges     []CallEdge                 // resolved call edges
	outByName map[string]map[string]bool // caller name -> set of callee names
	inByName  map[string]map[string]bool // callee name -> set of caller names
	ambiguous map[string]bool            // names with >1 declaration
}

// stripCallNoise removes line comments and the contents of string/char/template
// literals from a source line before call extraction, so a function name mentioned
// inside a comment or string ("call leaf()") is not mistaken for a call site. It is
// a lexical scrubber (not a full lexer): it handles "..", '..', `..` with backslash
// escapes, which covers Java/Go/JS. Block comments are not tracked across lines —
// acceptable since calls inside block comments are rare and the cost is a possible
// false positive, never a false negative.
func stripCallNoise(s string) string {
	s = stripLineComment(s)
	var b strings.Builder
	var quote rune
	escaped := false
	for _, ch := range s {
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
				b.WriteRune(' ') // keep column-ish spacing, drop content
			}
			continue
		}
		switch ch {
		case '"', '\'', '`':
			quote = ch
		default:
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// callIdentRE finds candidate call identifiers: `name(`. Receiver/qualifier (the
// `foo.` in `foo.bar(`) is intentionally dropped — we resolve by bare callee name,
// matching the scope-not-type precision level documented above.
var callIdentRE = regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*\(`)

// callKeywords are identifiers that look like calls but are language control-flow.
var callKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "synchronized": true, "func": true, "function": true,
	"super": true, "this": true, "new": true, "typeof": true, "await": true,
	"yield": true, "throw": true, "go": true, "defer": true, "range": true,
}

var (
	callGraphMu    sync.Mutex
	callGraphCache = map[string]*cgEntry{} // abs source root -> cached graph
)

type cgEntry struct {
	graph  *CallGraph
	hashes map[string]string // rel -> content hash, for incremental rebuild
}

// buildCallGraphFor returns the (cached, incrementally rebuilt) call graph for a
// source root. It reuses the symbol index's walk discipline and the per-language
// FuncBodies parser, so it stays consistent with symbol search and the unroller.
func buildCallGraphFor(cfg Config, root string) (*CallGraph, error) {
	base := filepath.Join(cfg.Root, root)
	abs, err := filepath.Abs(base)
	if err != nil {
		abs = base
	}

	callGraphMu.Lock()
	defer callGraphMu.Unlock()
	prev := callGraphCache[abs]

	// Collect function spans per file, reusing unchanged files from the cache.
	type fileFns struct {
		lines []string
		fns   []FuncSpan
	}
	curHashes := map[string]string{}
	perFile := map[string]fileFns{}
	prevSpans := map[string][]FuncSpan{}
	if prev != nil {
		for _, f := range prev.graph.Funcs {
			prevSpans[f.File] = append(prevSpans[f.File], f)
		}
	}

	err = walkSourceFiles(cfg, base, func(rel string, lines []string) {
		h := hash(strings.Join(lines, "\n"))
		curHashes[rel] = h
		if prev != nil && prev.hashes[rel] == h {
			perFile[rel] = fileFns{lines: lines, fns: prevSpans[rel]}
			return
		}
		lang := languageByID(languageForPath(rel))
		var fns []FuncSpan
		if lang != nil && lang.FuncBodies != nil {
			fns = lang.FuncBodies(rel, lines)
		}
		perFile[rel] = fileFns{lines: lines, fns: fns}
	})
	if err != nil {
		return nil, err
	}

	g := &CallGraph{
		Defs:      map[string][]FuncSpan{},
		outByName: map[string]map[string]bool{},
		inByName:  map[string]map[string]bool{},
		ambiguous: map[string]bool{},
	}
	// Index every declaration first so call resolution can check "is this a known
	// function in the root?" in one pass.
	files := make([]string, 0, len(perFile))
	for rel := range perFile {
		files = append(files, rel)
	}
	sort.Strings(files)
	for _, rel := range files {
		for _, fn := range perFile[rel].fns {
			g.Funcs = append(g.Funcs, fn)
			g.Defs[fn.Name] = append(g.Defs[fn.Name], fn)
		}
	}
	for name, defs := range g.Defs {
		if len(defs) > 1 {
			g.ambiguous[name] = true
		}
	}

	// Resolve call edges: scan each function body for `name(` where name is a known
	// declared function (and not the function itself / a keyword).
	for _, rel := range files {
		ff := perFile[rel]
		for _, fn := range ff.fns {
			seen := map[int]map[string]bool{} // line -> callee set (dedupe per line)
			for ln := fn.Start; ln <= fn.End && ln-1 < len(ff.lines); ln++ {
				code := stripCallNoise(ff.lines[ln-1])
				for _, m := range callIdentRE.FindAllStringSubmatch(code, -1) {
					callee := m[1]
					if callee == fn.Name || callKeywords[callee] {
						continue
					}
					if _, known := g.Defs[callee]; !known {
						continue
					}
					if seen[ln] == nil {
						seen[ln] = map[string]bool{}
					}
					if seen[ln][callee] {
						continue
					}
					seen[ln][callee] = true
					g.Edges = append(g.Edges, CallEdge{Caller: fn.Name, Callee: callee, File: rel, Line: ln})
					if g.outByName[fn.Name] == nil {
						g.outByName[fn.Name] = map[string]bool{}
					}
					g.outByName[fn.Name][callee] = true
					if g.inByName[callee] == nil {
						g.inByName[callee] = map[string]bool{}
					}
					g.inByName[callee][fn.Name] = true
				}
			}
		}
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].File != g.Edges[j].File {
			return g.Edges[i].File < g.Edges[j].File
		}
		return g.Edges[i].Line < g.Edges[j].Line
	})

	callGraphCache[abs] = &cgEntry{graph: g, hashes: curHashes}
	return g, nil
}

// CallersOf returns the resolved call sites of name (direct callers), sorted.
func (g *CallGraph) CallersOf(name string) []CallEdge {
	var out []CallEdge
	for _, e := range g.Edges {
		if e.Callee == name {
			out = append(out, e)
		}
	}
	return out
}

// ImpactSet returns the transitive set of caller function names that (in)directly
// reach name, together with the BFS depth at which each was found. The seed
// function itself is depth 0 and excluded from the returned map's "callers" sense
// but reported separately by the lens. Cycles terminate (visited guard).
func (g *CallGraph) ImpactSet(name string) map[string]int {
	depth := map[string]int{name: 0}
	frontier := []string{name}
	for len(frontier) > 0 {
		var next []string
		for _, cur := range frontier {
			for caller := range g.inByName[cur] {
				if _, ok := depth[caller]; ok {
					continue
				}
				depth[caller] = depth[cur] + 1
				next = append(next, caller)
			}
		}
		frontier = next
	}
	return depth
}

// IsAmbiguous reports whether name has more than one declaration in the root, which
// caps the precision of any edge touching it (the scope-not-type ceiling).
func (g *CallGraph) IsAmbiguous(name string) bool { return g.ambiguous[name] }

// ---------------------------------------------------------------------------
// Per-language FuncBodies adapters — thin wrappers over the existing body parsers
// so the graph reuses one definition of "what is a function" per language.
// ---------------------------------------------------------------------------

func javaFuncBodies(rel string, lines []string) []FuncSpan {
	ms, err := parseJavaMethods(lines)
	if err != nil {
		return nil
	}
	out := make([]FuncSpan, 0, len(ms))
	for _, m := range ms {
		out = append(out, FuncSpan{Name: m.Name, File: rel, Start: m.Start, End: m.End})
	}
	return out
}

func goFuncBodies(rel string, lines []string) []FuncSpan {
	var out []FuncSpan
	for i := 0; i < len(lines); i++ {
		m := goFuncRE.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		close, err := findClosingBrace(lines, i)
		if err != nil {
			continue
		}
		out = append(out, FuncSpan{Name: m[2], File: rel, Start: i + 1, End: close + 1})
		i = close
	}
	return out
}

func tsFuncBodies(rel string, lines []string) []FuncSpan {
	var out []FuncSpan
	for _, fn := range parseTSFuncs(rel, lines) {
		// bodyB is the closing-brace line (1-based); fn.line is the signature.
		out = append(out, FuncSpan{Name: fn.name, File: rel, Start: fn.line, End: fn.bodyB})
	}
	return out
}

// ambiguityNote builds the standard confidence note for a graph answer about name.
func ambiguityNote(g *CallGraph, name string) string {
	if g.IsAmbiguous(name) {
		return fmt.Sprintf("%d declarations named %s in scope; same-named functions are merged (scope-resolved, not type-resolved)", len(g.Defs[name]), name)
	}
	return "resolved against declared functions in this source root (scope-resolved, not type-resolved)"
}
