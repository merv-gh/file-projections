package main

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// side-effects lens — first-class, language-aware detection of externally-observable
// effects (IO read/write, network, db, process). It is the read-only sibling of
// entrypoints/exitpoints: where those answer "where does control enter/leave", this
// answers "what does this code actually touch". Default markers per language live on
// the Language registry (language.go); projects can add their own with the `markers`
// param ("kind=regex;kind=regex"). Used by the CLI, the service graph and the UI.

// SideEffectHit is one detected effect occurrence.
type SideEffectHit struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	File  string `json:"file"`
	Line  int    `json:"line"`
	Code  string `json:"code"`
}

// compiledMarker is a SideEffectMarker with its regex compiled.
type compiledMarker struct {
	kind, label string
	re          *regexp.Regexp
}

// sideEffectMarkersFor returns the compiled markers for a source root: the dominant
// language's defaults, plus any project overrides from params.markers, plus an
// optional language override (params.lang).
func sideEffectMarkersFor(cfg Config, lens LensConfig) ([]compiledMarker, string, error) {
	lang := coalesce(lens.Params["lang"], rootLanguage(cfg, lens.SourceRoot))
	var markers []SideEffectMarker
	if l := languageByID(lang); l != nil {
		markers = append(markers, l.SideEffects...)
	}
	// Project overrides: "kind=regex;kind=regex".
	for _, part := range strings.Split(lens.Params["markers"], ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, re, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		markers = append(markers, SideEffectMarker{Kind: strings.TrimSpace(kind), Label: strings.TrimSpace(kind), Regex: strings.TrimSpace(re)})
	}
	if len(markers) == 0 {
		return nil, lang, fmt.Errorf("side-effects: no markers for language %q (pass params.markers=\"db=...;network=...\")", lang)
	}
	var out []compiledMarker
	for _, m := range markers {
		re, err := regexp.Compile(m.Regex)
		if err != nil {
			return nil, lang, fmt.Errorf("side-effects: bad marker regex %q: %w", m.Regex, err)
		}
		out = append(out, compiledMarker{kind: m.Kind, label: m.Label, re: re})
	}
	return out, lang, nil
}

// scanSideEffects walks the source root and returns every line that matches a marker.
// It is the shared engine the analyzer, the service graph and per-function summaries
// reuse, so "side effect" means the same thing everywhere.
func scanSideEffects(cfg Config, lens LensConfig) ([]SideEffectHit, error) {
	markers, _, err := sideEffectMarkersFor(cfg, lens)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(cfg.Root, lens.SourceRoot)
	var hits []SideEffectHit
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipDir(cfg, path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if languageForPath(path) == "" {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		rel = filepath.ToSlash(rel)
		if isTestPath(rel) {
			return nil
		}
		lines, rerr := readLines(path)
		if rerr != nil {
			return nil
		}
		for i, raw := range lines {
			code := stripLineComment(raw)
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, m := range markers {
				if m.re.MatchString(code) {
					hits = append(hits, SideEffectHit{Kind: m.kind, Label: m.label, File: rel, Line: i + 1, Code: strings.TrimSpace(raw)})
					break // one effect kind per line is enough signal
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	return hits, nil
}

// isTestPath reports whether a rel path looks like a test file, so side-effect scans
// focus on production code.
func isTestPath(rel string) bool {
	l := strings.ToLower(rel)
	return strings.Contains(l, "/test/") || strings.HasPrefix(l, "test/") ||
		strings.Contains(l, "_test.") || strings.Contains(l, ".test.") || strings.Contains(l, ".spec.")
}

// appendUnrollSideEffectFacts tags an unrolled program with the side effects its
// lines perform, as `se-line-<n>` facts (kind + label) plus a `se-summary` fact.
// This makes side-effects first-class in the unrolled-program view too: a drilled-in
// function shows exactly which lines hit IO/network/db, in the same projection the
// UI/CLI already render. lang is the source language; body/origins come from the
// unroller. Best-effort: unknown language → no-op.
func appendUnrollSideEffectFacts(p *Projection, lang string, body []string, origins []LineOrigin) {
	l := languageByID(lang)
	if l == nil || len(l.SideEffects) == 0 {
		return
	}
	var markers []compiledMarker
	for _, m := range l.SideEffects {
		if re, err := regexp.Compile(m.Regex); err == nil {
			markers = append(markers, compiledMarker{kind: m.Kind, label: m.Label, re: re})
		}
	}
	counts := map[string]int{}
	var kinds []string
	for i, code := range body {
		c := stripLineComment(code)
		for _, m := range markers {
			if m.re.MatchString(c) {
				if counts[m.kind] == 0 {
					kinds = append(kinds, m.kind)
				}
				counts[m.kind]++
				origin := ""
				if i < len(origins) {
					origin = fmt.Sprintf("%s:%d", origins[i].SrcFile, origins[i].Line)
				}
				p.Facts = append(p.Facts, ProjectionFact{
					ID:   fmt.Sprintf("se-line-%d", i+1),
					Tool: "unrolled-program",
					Text: fmt.Sprintf("%s (%s) %s", m.kind, m.label, origin),
				})
				break
			}
		}
	}
	if len(kinds) > 0 {
		sort.Strings(kinds)
		var parts []string
		for _, k := range kinds {
			parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
		}
		p.Facts = append(p.Facts, ProjectionFact{ID: "se-summary", Tool: "unrolled-program", Text: strings.Join(parts, " ")})
	}
}

func AnalyzeSideEffects(cfg Config, lens LensConfig) (Projection, error) {
	hits, err := scanSideEffects(cfg, lens)
	if err != nil {
		return Projection{}, err
	}
	// Group by kind for a scannable, code-first projection.
	byKind := map[string][]SideEffectHit{}
	var kinds []string
	for _, h := range hits {
		if _, ok := byKind[h.Kind]; !ok {
			kinds = append(kinds, h.Kind)
		}
		byKind[h.Kind] = append(byKind[h.Kind], h)
	}
	sort.Strings(kinds)
	p := Projection{Sync: "view-only"}
	for _, kind := range kinds {
		var lines []string
		for _, h := range byKind[kind] {
			lines = append(lines, codeLoc(h.Code, h.File, h.Line))
		}
		p.Blocks = append(p.Blocks, ProjectionBlock{ID: kind, File: "model", Mode: "side-effects", Tool: "side-effects", Lines: lines})
	}
	if len(p.Blocks) == 0 {
		p.Blocks = append(p.Blocks, ProjectionBlock{ID: "none", File: "model", Mode: "side-effects", Tool: "side-effects", Lines: []string{"// no side effects detected under " + coalesce(lens.SourceRoot, ".")}})
	}
	// A summary fact per kind so CLI/MCP get counts without parsing the blocks.
	for _, kind := range kinds {
		p.Facts = append(p.Facts, ProjectionFact{ID: "count-" + kind, Tool: "side-effects", Text: fmt.Sprintf("%s: %d", kind, len(byKind[kind]))})
	}
	return p, nil
}
