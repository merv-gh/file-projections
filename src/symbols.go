package main

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Symbol index — a cached, per-source-root map of language-neutral Symbols, built
// by the per-language SymbolScanners. This is the decoupling between *what* a
// symbol is (Symbol, language-agnostic) and *how* it's found (regex today,
// tree-sitter/LSP later: swap the scanner, the index and every caller are unchanged).
//
// Caching mirrors the CPG's incremental model: the index keeps a per-file content
// hash and only re-scans files whose hash changed, so repeated symbol queries (the
// UI fires one per keystroke) don't re-walk a large tree. The unit is the source
// root, like the CPG manifest, so the two stay consistent.

type symbolIndex struct {
	Symbols []Symbol
	hashes  map[string]string // rel file -> content hash, for incremental refresh
	byFile  map[string][]Symbol
}

var (
	symIndexMu    sync.Mutex
	symIndexCache = map[string]*symbolIndex{} // keyed by absolute source root
)

// symbolIndexFor returns the (cached, incrementally refreshed) symbol index for a
// source root. Files unchanged since the last call are reused; only changed/added
// files are re-scanned and removed files dropped.
func symbolIndexFor(cfg Config, root string) (*symbolIndex, error) {
	base := filepath.Join(cfg.Root, root)
	abs, err := filepath.Abs(base)
	if err != nil {
		abs = base
	}
	symIndexMu.Lock()
	defer symIndexMu.Unlock()

	prev := symIndexCache[abs]
	cur := &symbolIndex{hashes: map[string]string{}, byFile: map[string][]Symbol{}}

	seen := map[string]bool{}
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // skip unreadable entries, don't abort
		}
		if d.IsDir() {
			if shouldSkipDir(cfg, path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		lang := languageForPath(path)
		if lang == "" {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		rel = filepath.ToSlash(rel)
		if strings.Contains(rel, "__MACOSX/") || strings.HasPrefix(filepath.Base(rel), "._") {
			return nil
		}
		seen[rel] = true
		lines, rerr := readLines(path)
		if rerr != nil {
			return nil
		}
		h := hash(strings.Join(lines, "\n"))
		// Reuse the previous scan for unchanged files.
		if prev != nil && prev.hashes[rel] == h {
			if syms, ok := prev.byFile[rel]; ok {
				cur.hashes[rel] = h
				cur.byFile[rel] = syms
				return nil
			}
		}
		l := languageByID(lang)
		var syms []Symbol
		// Always offer the file itself as a symbol, then the language's declarations.
		syms = append(syms, Symbol{Name: rel, Kind: "file", File: rel, Line: 1})
		if l != nil && l.Scan != nil {
			syms = append(syms, l.Scan(rel, lines)...)
		}
		cur.hashes[rel] = h
		cur.byFile[rel] = syms
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Flatten in a stable file order.
	files := make([]string, 0, len(cur.byFile))
	for f := range cur.byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		cur.Symbols = append(cur.Symbols, cur.byFile[f]...)
	}
	symIndexCache[abs] = cur
	return cur, nil
}

// ---------------------------------------------------------------------------
// Default (regex) symbol scanners. These are the no-dependency backend; a
// tree-sitter backend would replace these functions and nothing else.
// ---------------------------------------------------------------------------

var symJavaClassRE = regexp.MustCompile(`^\s*(?:public\s+|final\s+|abstract\s+)*(class|interface|enum|record)\s+([A-Za-z_][A-Za-z0-9_]*)`)

func scanJavaSymbols(rel string, lines []string) []Symbol {
	var out []Symbol
	for i, l := range lines {
		if m := symJavaClassRE.FindStringSubmatch(l); m != nil {
			out = append(out, Symbol{Name: m[2], Kind: m[1], File: rel, Line: i + 1})
		}
	}
	if methods, err := parseJavaMethods(lines); err == nil {
		for _, m := range methods {
			out = append(out, Symbol{Name: m.Name, Kind: "method", File: rel, Line: m.Start})
		}
	}
	return out
}

var symGoDeclRE = regexp.MustCompile(`^func(?:\s+\([^)]*\))?\s+([A-Za-z_][A-Za-z0-9_]*)|^type\s+([A-Za-z_][A-Za-z0-9_]*)`)

func scanGoSymbols(rel string, lines []string) []Symbol {
	var out []Symbol
	for i, l := range lines {
		if m := symGoDeclRE.FindStringSubmatch(l); m != nil {
			name, kind := m[1], "func"
			if name == "" {
				name, kind = m[2], "type"
			}
			out = append(out, Symbol{Name: name, Kind: kind, File: rel, Line: i + 1})
		}
	}
	return out
}

// scanTSSymbols reuses the TS unroller's function parser so symbol search and
// unrolling agree on what a function is.
func scanTSSymbols(rel string, lines []string) []Symbol {
	var out []Symbol
	for _, fn := range parseTSFuncs(rel, lines) {
		out = append(out, Symbol{Name: fn.name, Kind: "func", File: rel, Line: fn.line})
	}
	return out
}
