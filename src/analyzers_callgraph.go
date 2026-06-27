package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Structural lenses backed by the pure-Go call graph (callgraph.go). These are the
// precise (scope-resolved) upgrades of the lexical `callers`/`references` lenses and
// the new transitive `impact-set` — the Phase 2 payoff: edges that compose into a
// graph instead of one-shot text matches. Confidence is `structural` (the call only
// counts inside a known body and resolves to a declared function), dropping to a
// caveat when the name is ambiguous (same name declared more than once in scope).

// AnalyzeCallGraphCallers lists the resolved call sites of a function — only calls
// that occur inside a known function body and resolve to a declared function. This
// is the false-positive-free counterpart of the lexical `callers` lens.
func AnalyzeCallGraphCallers(cfg Config, lens LensConfig) (Projection, error) {
	name := strings.TrimSpace(lens.Params["name"])
	if name == "" {
		return Projection{}, errors.New("call-graph-callers: params.name is required")
	}
	g, err := buildCallGraphFor(cfg, sourceRootOrDot(lens))
	if err != nil {
		return Projection{}, err
	}
	if _, known := g.Defs[name]; !known {
		p := Projection{Sync: "view-only"}
		p.Blocks = append(p.Blocks, ProjectionBlock{ID: "callers", File: "model", Mode: "callers", Tool: "call-graph",
			Lines: []string{"// no function named " + name + " is declared in this source root"}})
		p.Facts = append(p.Facts, confidenceFact("structural", "no declaration found; lexical `callers` may still match text"))
		return p, nil
	}

	edges := g.CallersOf(name)
	lines := make([]string, 0, len(edges))
	callers := map[string]bool{}
	for _, e := range edges {
		lines = append(lines, codeLoc(e.Caller+" → "+name+"()", e.File, e.Line))
		callers[e.Caller] = true
	}
	if len(lines) == 0 {
		lines = append(lines, "// "+name+" is declared but has no resolved callers (entrypoint, dead, or called via interface/reflection)")
	}
	p := Projection{Sync: "view-only"}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "callers", File: "model", Mode: "callers", Tool: "call-graph", Lines: lines})
	p.Facts = append(p.Facts, confidenceFact("structural", ambiguityNote(g, name)))
	p.Facts = append(p.Facts, ProjectionFact{ID: "count", Tool: "call-graph",
		Text: fmt.Sprintf("%d call sites from %d distinct functions", len(edges), len(callers))})
	return p, nil
}

// AnalyzeImpactSet computes the transitive set of functions that (in)directly call
// the target — the "if I change X, what breaks" review primitive. Output groups
// callers by BFS depth so the blast radius is legible: depth 1 = direct callers,
// depth 2 = their callers, and so on.
func AnalyzeImpactSet(cfg Config, lens LensConfig) (Projection, error) {
	name := strings.TrimSpace(lens.Params["name"])
	if name == "" {
		return Projection{}, errors.New("impact-set: params.name is required")
	}
	g, err := buildCallGraphFor(cfg, sourceRootOrDot(lens))
	if err != nil {
		return Projection{}, err
	}
	if _, known := g.Defs[name]; !known {
		return Projection{}, fmt.Errorf("impact-set: no function named %s declared in this source root", name)
	}

	depth := g.ImpactSet(name)
	// Group names by depth (skip the seed at depth 0).
	byDepth := map[int][]string{}
	maxDepth := 0
	for fn, d := range depth {
		if d == 0 {
			continue
		}
		byDepth[d] = append(byDepth[d], fn)
		if d > maxDepth {
			maxDepth = d
		}
	}

	var lines []string
	for d := 1; d <= maxDepth; d++ {
		names := byDepth[d]
		sort.Strings(names)
		label := "direct callers"
		if d > 1 {
			label = fmt.Sprintf("depth %d", d)
		}
		lines = append(lines, fmt.Sprintf("// ── %s (%d) ──", label, len(names)))
		for _, fn := range names {
			loc := firstDeclLoc(g, fn)
			lines = append(lines, codeLoc(fn, loc.File, loc.Start))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "// nothing calls "+name+" — changing it affects only itself (entrypoint or dead code)")
	}
	p := Projection{Sync: "view-only"}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "impact", File: "model", Mode: "impact-set", Tool: "call-graph", Lines: lines})
	total := len(depth) - 1
	p.Facts = append(p.Facts, confidenceFact("structural", ambiguityNote(g, name)))
	p.Facts = append(p.Facts, ProjectionFact{ID: "blast", Tool: "call-graph",
		Text: fmt.Sprintf("%d functions transitively reach %s (max depth %d)", total, name, maxDepth)})
	return p, nil
}

// firstDeclLoc returns the first declaration span for a name (deterministic).
func firstDeclLoc(g *CallGraph, name string) FuncSpan {
	defs := g.Defs[name]
	if len(defs) == 0 {
		return FuncSpan{File: "model", Start: 1}
	}
	best := defs[0]
	for _, d := range defs[1:] {
		if d.File < best.File || (d.File == best.File && d.Start < best.Start) {
			best = d
		}
	}
	return best
}

func sourceRootOrDot(lens LensConfig) string {
	if lens.SourceRoot == "" {
		return "."
	}
	return lens.SourceRoot
}
