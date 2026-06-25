package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// JS/TS frontend: event surface + jsonl. (NOTE: no TS unrolled-program yet.)

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
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx":
		return true
	default:
		return false
	}
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
