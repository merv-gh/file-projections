package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// JS/TS frontend: event surface + jsonl + the TS/JS unrolled-program adapter.

// JSONL analyzer: generic adapter for external tool outputs.
func AnalyzeJSONL(cfg Config, lens LensConfig) (Projection, error) {
	path := filepath.Join(cfg.Root, lens.Input)
	f, err := os.Open(path)
	if err != nil {
		return Projection{}, err
	}
	defer f.Close()

	var p Projection
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Kind  string   `json:"kind"`
			ID    string   `json:"id"`
			File  string   `json:"file"`
			Mode  string   `json:"mode"`
			Tool  string   `json:"tool"`
			Text  string   `json:"text"`
			Lines []string `json:"lines"`
			Facts []string `json:"facts"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return Projection{}, fmt.Errorf("%s:%d: %w", lens.Input, lineNo, err)
		}
		switch rec.Kind {
		case "block":
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: rec.ID, File: rec.File, Mode: rec.Mode, Tool: coalesce(rec.Tool, "jsonl"), Lines: rec.Lines, Facts: rec.Facts})
		case "fact":
			p.Facts = append(p.Facts, ProjectionFact{ID: rec.ID, Tool: coalesce(rec.Tool, "jsonl"), Text: rec.Text})
		default:
			return Projection{}, fmt.Errorf("%s:%d unknown kind %q", lens.Input, lineNo, rec.Kind)
		}
	}
	return p, sc.Err()
}

func AnalyzeJSEvents(cfg Config, lens LensConfig) (Projection, error) {
	files, err := scanJSFiles(cfg, lens)
	if err != nil {
		return Projection{}, err
	}

	var p Projection
	var summary []string
	totalEmits, totalListeners, totalRegs := 0, 0, 0

	for _, f := range files {
		if len(f.Exports)+len(f.Functions)+len(f.Classes) > 0 {
			var lines []string
			for _, x := range f.Exports {
				lines = append(lines, fmt.Sprintf("export %s %s line=%d :: %s", x.Kind, x.Name, x.Line, x.Sig))
			}
			for _, x := range f.Classes {
				lines = append(lines, fmt.Sprintf("class %s line=%d :: %s", x.Name, x.Line, x.Sig))
			}
			for _, x := range f.Functions {
				lines = append(lines, fmt.Sprintf("function %s line=%d :: %s", x.Name, x.Line, x.Sig))
			}
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "surface", File: f.Rel, Mode: "surface", Tool: "js-events", Lines: dedupe(lines)})
		}

		if len(f.Events) > 0 {
			var lines []string
			var facts []string
			for _, ev := range f.Events {
				lines = append(lines, fmt.Sprintf("%s %s line=%d :: %s", ev.Kind, ev.Name, ev.Line, ev.Code))
				if ev.Kind == "emit" || ev.Kind == "dispatch" {
					totalEmits++
				} else {
					totalListeners++
				}
			}
			facts = append(facts, fmt.Sprintf("event surface: %d events/listeners in %s", len(f.Events), f.Rel))
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "events", File: f.Rel, Mode: "events", Tool: "js-events", Lines: dedupe(lines), Facts: facts})
		}

		if len(f.Regs) > 0 {
			var lines []string
			for _, r := range f.Regs {
				lines = append(lines, fmt.Sprintf("%s %s line=%d :: %s", r.Kind, r.Name, r.Line, r.Code))
				totalRegs++
			}
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "registrations", File: f.Rel, Mode: "registrations", Tool: "js-events", Lines: dedupe(lines)})
		}
	}

	summary = append(summary, fmt.Sprintf("files scanned: %d", len(files)))
	summary = append(summary, fmt.Sprintf("event emits/dispatches: %d", totalEmits))
	summary = append(summary, fmt.Sprintf("event listeners/subscriptions: %d", totalListeners))
	summary = append(summary, fmt.Sprintf("registrations: %d", totalRegs))
	summary = append(summary, "use this lens to see composable event-driven working surface without opening full files")
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "summary", File: "model", Mode: "summary", Tool: "js-events", Lines: summary})

	return p, nil
}

func jsControlWord(s string) bool {
	switch s {
	case "if", "for", "while", "switch", "catch", "function":
		return true
	default:
		return false
	}
}

func scanJSFiles(cfg Config, lens LensConfig) ([]JSFile, error) {
	root := filepath.Join(cfg.Root, lens.SourceRoot)
	include := map[string]bool{}
	for _, inc := range lens.Include {
		include[filepath.ToSlash(inc)] = true
		include[filepath.Base(inc)] = true
	}

	var files []JSFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if shouldSkipDir(cfg, path, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !isJSFile(path) {
			return nil
		}
		rel, _ := filepath.Rel(cfg.Root, path)
		rel = filepath.ToSlash(rel)
		if strings.Contains(rel, "__MACOSX/") || strings.Contains(rel, "/._") || strings.HasPrefix(filepath.Base(rel), "._") {
			return nil
		}
		if len(include) > 0 && !include[rel] && !include[filepath.Base(rel)] {
			return nil
		}
		f, err := parseJSFile(cfg.Root, path)
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	return files, err
}

func isJSFile(path string) bool {
	return languageForPath(path) == "js"
}

func parseJSFile(root, path string) (JSFile, error) {
	lines, err := readLines(path)
	if err != nil {
		return JSFile{}, err
	}
	rel, _ := filepath.Rel(root, path)
	rel = filepath.ToSlash(rel)
	f := JSFile{Rel: rel, Lines: lines}

	for i, line := range lines {
		trim := strings.TrimSpace(line)
		lineNo := i + 1
		if trim == "" || strings.HasPrefix(trim, "//") {
			continue
		}
		if m := jsExportFuncRE.FindStringSubmatch(trim); m != nil {
			f.Exports = append(f.Exports, JSSymbol{Name: m[1], Kind: "function", Line: lineNo, Sig: trimBeforeBrace(trim)})
			continue
		}
		if m := jsExportClassRE.FindStringSubmatch(trim); m != nil {
			f.Exports = append(f.Exports, JSSymbol{Name: m[1], Kind: "class", Line: lineNo, Sig: trimBeforeBrace(trim)})
			continue
		}
		if strings.HasPrefix(trim, "export ") {
			f.Exports = append(f.Exports, JSSymbol{Name: compactJSName(trim), Kind: "value", Line: lineNo, Sig: trimBeforeBrace(trim)})
		}
		if m := jsClassRE.FindStringSubmatch(trim); m != nil {
			f.Classes = append(f.Classes, JSSymbol{Name: m[1], Kind: "class", Line: lineNo, Sig: trimBeforeBrace(trim)})
		}
		// Keep the surface compact: top-level functions/arrow functions only.
		// Class internals and inline callbacks are intentionally left to event/registration facts.
		isTopLevel := len(line) > 0 && line[0] != ' ' && line[0] != '\t'
		if isTopLevel {
			if m := jsFunctionRE.FindStringSubmatch(trim); m != nil {
				f.Functions = append(f.Functions, JSSymbol{Name: m[1], Kind: "function", Line: lineNo, Sig: trimBeforeBrace(trim)})
			}
			if m := jsConstFuncRE.FindStringSubmatch(trim); m != nil {
				f.Functions = append(f.Functions, JSSymbol{Name: m[1], Kind: "function", Line: lineNo, Sig: trimBeforeBrace(trim)})
			}
		}
		for _, m := range jsEmitRE.FindAllStringSubmatch(trim, -1) {
			kind := "emit"
			if m[1] == "dispatchEvent" {
				kind = "dispatch"
			}
			f.Events = append(f.Events, JSEvent{Kind: kind, Name: m[2], Line: lineNo, Code: trim})
		}
		for _, m := range jsOnRE.FindAllStringSubmatch(trim, -1) {
			kind := "listen"
			if m[1] == "on" || m[1] == "once" {
				kind = "subscribe"
			}
			f.Events = append(f.Events, JSEvent{Kind: kind, Name: m[2], Line: lineNo, Code: trim})
		}
		for _, m := range jsRegisterRE.FindAllStringSubmatch(trim, -1) {
			f.Regs = append(f.Regs, JSRegistration{Kind: strings.TrimPrefix(m[1], "core."), Name: m[2], Line: lineNo, Code: trim})
		}
		for _, m := range jsModsRegisterRE.FindAllStringSubmatch(trim, -1) {
			f.Regs = append(f.Regs, JSRegistration{Kind: m[1], Name: m[2], Line: lineNo, Code: trim})
		}
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// unrolled-program (TS/JS): the third-language unroller the architecture called
// out as missing. It flattens a function's branched, cross-file execution into
// one editable straight-line program — the same unrollLine stream the Java/Go
// adapters emit, so the UI's timeline + assumptions + scattered two-way sync all
// work unchanged. Brace-depth (not indentation) drives the guard stack so it is
// robust to the repo's formatting.
// ---------------------------------------------------------------------------

type tsFunc struct {
	rel   string // path relative to the source root
	name  string
	line  int // 1-based line of the signature
	bodyA int // 1-based line of the first body line (after the opening "{")
	bodyB int // 1-based line of the closing "}"
}

// tsLexAdapter adapts TS/JS to the shared lexical unroller (unroll_lexical.go);
// the walk/guard/inline machinery lives there, this only answers TS-specific
// questions. Same shared core as the Go adapter — the two languages no longer
// duplicate the unroll loop.
type tsLexAdapter struct {
	lens  LensConfig
	funcs map[string]tsFunc
}

func (a *tsLexAdapter) lookup(name, file string) (lexFunc, bool) {
	fn, ok := a.funcs[name]
	if !ok {
		return lexFunc{}, false
	}
	if file != "" && fn.rel != file {
		for _, f := range a.funcs {
			if f.rel == file && f.name == name {
				fn = f
				break
			}
		}
	}
	// bodyA/bodyB are 1-based; the shared core wants 0-based inclusive [start,end].
	return lexFunc{Rel: fn.rel, BodyStart: fn.bodyA - 1, BodyEnd: fn.bodyB - 2}, true
}
func (a *tsLexAdapter) guardCond(trim string) (string, bool) { return tsGuardCond(trim) }
func (a *tsLexAdapter) callName(trim string) string          { return simpleTSCall(trim) }
func (a *tsLexAdapter) known(name string) bool               { _, ok := a.funcs[name]; return ok }
func (a *tsLexAdapter) tool() string                         { return "unrolled-program:ts" }
func (a *tsLexAdapter) scope() string {
	return "ts adapter: editable straight-line function path; each line syncs back to its original source line"
}

func AnalyzeTSUnrolledProgram(cfg Config, lens LensConfig) (Projection, error) {
	files, err := scanJSFiles(cfg, lens)
	if err != nil {
		return Projection{}, err
	}
	ad := &tsLexAdapter{lens: lens, funcs: map[string]tsFunc{}}
	prefix := filepath.ToSlash(lens.SourceRoot) + "/"
	for _, f := range files {
		rel := strings.TrimPrefix(strings.TrimPrefix(f.Rel, prefix), "./")
		for _, fn := range parseTSFuncs(rel, f.Lines) {
			ad.funcs[fn.name] = fn
		}
	}
	return runLexicalUnroll(cfg, lens, ad)
}

var (
	tsNamedFuncRE = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*[(<]`)
	tsArrowRE     = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?(?:<[^>]*>\s*)?\([^)]*\)[^=]*=>`)
	tsMethodRE    = regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+|static\s+|async\s+|get\s+|set\s+)*([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*(?::[^={]+)?\{`)
	// tsArrowOpenRE matches the opener of a multi-line arrow function whose params
	// (and the `=>`) spill onto following lines: `const exchange = async (`.
	tsArrowOpenRE = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?(?:<[^>]*>\s*)?\(\s*$`)
)

// parseTSFuncs finds top-levelish functions in a TS/JS file: named declarations,
// arrow functions bound to const/let/var (single- or multi-line signatures), and
// (best-effort) class methods. For each it records the body span using brace
// matching from the line that opens the body.
func parseTSFuncs(rel string, lines []string) []tsFunc {
	var out []tsFunc
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		var name string
		multilineArrow := false
		switch {
		case tsNamedFuncRE.MatchString(line):
			name = tsNamedFuncRE.FindStringSubmatch(line)[1]
		case tsArrowRE.MatchString(line):
			name = tsArrowRE.FindStringSubmatch(line)[1]
		case tsArrowOpenRE.MatchString(line):
			name = tsArrowOpenRE.FindStringSubmatch(line)[1]
			multilineArrow = true
		case tsMethodRE.MatchString(line) && !tsControlHeader(line):
			name = tsMethodRE.FindStringSubmatch(line)[1]
		default:
			continue
		}
		// Find the line that contains the body's opening brace (signatures can span
		// multiple lines), then brace-match to the close.
		open := -1
		for j := i; j < len(lines) && j < i+12; j++ {
			if strings.Contains(stripLineComment(lines[j]), "{") {
				open = j
				break
			}
		}
		if open < 0 {
			continue
		}
		// A multi-line arrow opener is only a function if a `=>` appears before the
		// body brace (else `const x = (` was just a parenthesized expression).
		if multilineArrow {
			sawArrow := false
			for j := i; j <= open; j++ {
				if strings.Contains(lines[j], "=>") {
					sawArrow = true
					break
				}
			}
			if !sawArrow {
				continue
			}
		}
		close, err := findClosingBrace(lines, open)
		if err != nil {
			continue
		}
		out = append(out, tsFunc{rel: rel, name: name, line: i + 1, bodyA: open + 2, bodyB: close + 1})
		i = close
	}
	return out
}

// tsControlHeader reports whether a line that looks like a method is actually a
// control-flow header (if/for/while/switch/catch) so it is not mistaken for a func.
func tsControlHeader(line string) bool {
	t := strings.TrimSpace(line)
	for _, kw := range []string{"if", "for", "while", "switch", "catch", "function", "return"} {
		if strings.HasPrefix(t, kw+" ") || strings.HasPrefix(t, kw+"(") {
			return true
		}
	}
	return false
}

// tsGuardCond extracts the condition a TS `if`/`for`/`while`/`else if` header
// introduces. Returns ok=false for non-guards.
func tsGuardCond(trim string) (string, bool) {
	for _, p := range []struct{ kw, label string }{
		{"} else if", ""}, {"else if", ""}, {"if", ""},
		{"for", "loop: "}, {"while", "loop: "},
	} {
		head := p.kw
		if strings.HasPrefix(trim, head+" (") || strings.HasPrefix(trim, head+"(") {
			i := strings.Index(trim, "(")
			j := matchParen(trim, i)
			if j < 0 {
				return "", false
			}
			cond := strings.TrimSpace(trim[i+1 : j])
			if p.label != "" {
				return p.label + cond, true
			}
			return cond, true
		}
	}
	return "", false
}

// simpleTSCall returns the name of a locally-defined function called on a line of
// the form `foo(...)`, `const x = foo(...)`, `return foo(...)`, or `await foo(...)`.
func simpleTSCall(trim string) string {
	trim = strings.TrimPrefix(trim, "return ")
	trim = strings.TrimPrefix(trim, "await ")
	if i := strings.Index(trim, "="); i >= 0 && (i+1 >= len(trim) || trim[i+1] != '=') && (i == 0 || trim[i-1] != '!' && trim[i-1] != '<' && trim[i-1] != '>') {
		trim = strings.TrimSpace(trim[i+1:])
		trim = strings.TrimPrefix(trim, "await ")
	}
	m := regexp.MustCompile(`^([A-Za-z_$][\w$]*)\s*\(`).FindStringSubmatch(trim)
	if m == nil {
		return ""
	}
	return m[1]
}
