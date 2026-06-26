package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Focused single-file / single-symbol lenses — the cheap, lexical/structural
// backends behind the Questions panel (see GRASPABILITY.md). Each answers exactly
// one "where" question and carries a confidence fact ("lexical" | "structural")
// so the UI can badge how trustworthy the answer is. None of these need a CPG, so
// they work across every language today and keep joern off the critical path.

// confidenceFact records how the answer was derived, for UI badging.
func confidenceFact(level, note string) ProjectionFact {
	return ProjectionFact{ID: "confidence", Tool: "query", Text: level + ": " + note}
}

// queryRows runs a regex over the source root and returns matches as a sorted,
// code-first projection block. Shared by the symbol-oriented lenses below. level is
// the confidence to report ("lexical" | "structural").
func queryRows(cfg Config, lens LensConfig, pattern, mode, level, note string) (Projection, error) {
	root := lens.SourceRoot
	if root == "" {
		root = "."
	}
	hits, err := ripgrep(cfg, pattern, root)
	if err != nil {
		return Projection{}, err
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	var lines []string
	for _, h := range hits {
		lines = append(lines, codeLoc(h.Text, h.File, h.Line))
	}
	if len(lines) == 0 {
		lines = append(lines, "// no matches under "+root)
	}
	p := Projection{Sync: "view-only"}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: mode, File: "model", Mode: mode, Tool: mode, Lines: lines})
	p.Facts = append(p.Facts, confidenceFact(level, note))
	p.Facts = append(p.Facts, ProjectionFact{ID: "count", Tool: mode, Text: fmt.Sprintf("%d matches", len(hits))})
	return p, nil
}

// AnalyzeReferences finds every textual mention of a symbol (word-boundary match).
// Answers "where is X used?". Lexical: a same-named symbol on a different type will
// also match — badged accordingly.
func AnalyzeReferences(cfg Config, lens LensConfig) (Projection, error) {
	name := strings.TrimSpace(lens.Params["name"])
	if name == "" {
		return Projection{}, errors.New("references: params.name is required")
	}
	pat := `\b` + regexp.QuoteMeta(name) + `\b`
	return queryRows(cfg, lens, pat, "references", "lexical",
		"textual references to "+name+" (same-named symbols on other types also match)")
}

// AnalyzeCallers finds call sites of a function/method: `name(` possibly preceded by
// a receiver. Answers "who calls X?". Structural-ish via the call shape, but still
// lexical confidence (no type resolution).
func AnalyzeCallers(cfg Config, lens LensConfig) (Projection, error) {
	name := strings.TrimSpace(lens.Params["name"])
	if name == "" {
		return Projection{}, errors.New("callers: params.name is required")
	}
	// `name (` as a call, not a definition. Exclude common definition keywords on the
	// same leading position so the function's own declaration is less noisy.
	pat := `\b` + regexp.QuoteMeta(name) + `\s*\(`
	p, err := queryRows(cfg, lens, pat, "callers", "lexical",
		"call sites of "+name+"() by name (no type resolution; def line may appear)")
	return p, err
}

// AnalyzeConstructions finds where instances of a type are created: `new Type(`,
// Go composite literals `Type{`, and `Type(` factory-ish calls. Answers
// "where is Type constructed?".
func AnalyzeConstructions(cfg Config, lens LensConfig) (Projection, error) {
	typ := strings.TrimSpace(lens.Params["type"])
	if typ == "" {
		return Projection{}, errors.New("constructions: params.type is required")
	}
	q := regexp.QuoteMeta(typ)
	// new Type( | Type{ (composite literal / &Type{) | Type.new / Type::new style
	pat := `(\bnew\s+` + q + `\b|\b` + q + `\s*\{|&` + q + `\s*\{|\b` + q + `\.new\b)`
	return queryRows(cfg, lens, pat, "constructions", "lexical",
		"construction sites of "+typ+" (new/composite-literal/factory)")
}

// AnalyzeWritesTo finds lines that write a variable/field: assignment, compound
// assignment, ++/--, or a setter call x.setFoo(. Answers "what mutates X?". Reuses
// the same write detection the unrolled-program object timeline uses.
func AnalyzeWritesTo(cfg Config, lens LensConfig) (Projection, error) {
	name := strings.TrimSpace(lens.Params["var"])
	if name == "" {
		return Projection{}, errors.New("writes-to: params.var is required")
	}
	q := regexp.QuoteMeta(name)
	cap := strings.Title(name) // best-effort setter name: setFoo for foo
	// assignment / compound-assign / inc-dec / setter
	pats := []string{
		`\b` + q + `\s*(=|\+=|-=|\*=|/=|\|=|&=|\^=)[^=]`, // x = / x += (not ==)
		`\b` + q + `\s*(\+\+|--)`,                        // x++ / x--
		`\.\s*set` + cap + `\s*\(`,                       // obj.setFoo(
		`\b` + q + `\.\s*(set|add|put|remove)\w*\s*\(`,   // x.setX(/x.add(
	}
	return queryRows(cfg, lens, `(`+strings.Join(pats, "|")+`)`, "writes-to", "lexical",
		"writes/mutations of "+name+" (assign, ++/--, setter)")
}

// AnalyzeSQLTables lists the tables (and the statements) a SQL or codegen-SQL file
// references. Answers "which tables does this query file touch?". Scans .sql files
// under the source root for FROM/JOIN/INTO/UPDATE targets.
func AnalyzeSQLTables(cfg Config, lens LensConfig) (Projection, error) {
	root := lens.SourceRoot
	if root == "" {
		root = "."
	}
	base := filepath.Join(cfg.Root, root)
	tableRE := regexp.MustCompile(`(?i)\b(?:from|join|into|update)\s+([a-z_][a-z0-9_."]*)`)
	type hit struct {
		table, file, stmt string
		line              int
	}
	var hits []hit
	tables := map[string]int{}
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipDir(cfg, path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.ToLower(filepath.Ext(path)) != ".sql" {
			return nil
		}
		lines, rerr := readLines(path)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		rel = filepath.ToSlash(rel)
		want := strings.TrimSpace(lens.Params["file"])
		if want != "" && rel != want && filepath.Base(rel) != want {
			return nil
		}
		for i, l := range lines {
			for _, m := range tableRE.FindAllStringSubmatch(l, -1) {
				t := strings.Trim(strings.ToLower(m[1]), `"`)
				hits = append(hits, hit{table: t, file: rel, stmt: strings.TrimSpace(l), line: i + 1})
				tables[t]++
			}
		}
		return nil
	})
	if err != nil {
		return Projection{}, err
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].line < hits[j].line
	})
	var lines []string
	for _, h := range hits {
		lines = append(lines, codeLoc(h.stmt, h.file, h.line))
	}
	if len(lines) == 0 {
		lines = append(lines, "// no SQL table references under "+root)
	}
	p := Projection{Sync: "view-only"}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "sql-tables", File: "model", Mode: "sql-tables", Tool: "sql-tables", Lines: lines})
	var names []string
	for t := range tables {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		p.Facts = append(p.Facts, ProjectionFact{ID: "table-" + t, Tool: "sql-tables", Text: fmt.Sprintf("%s: %d refs", t, tables[t])})
	}
	p.Facts = append(p.Facts, confidenceFact("lexical", "FROM/JOIN/INTO/UPDATE targets in .sql files"))
	return p, nil
}
