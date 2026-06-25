package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Language-agnostic analyzers: bookmark/extract, ast-grep.

// AnalyzeAstGrep runs an ast-grep structural pattern and emits a map block of matches.
// Uses a local ast-grep/sg binary if present, else the configured Docker image
// (tools.ast-grep.image). params.pattern and params.lang are required.
func AnalyzeAstGrep(cfg Config, lens LensConfig) (Projection, error) {
	if lens.Params == nil || lens.Params["pattern"] == "" || lens.Params["lang"] == "" {
		return Projection{}, errors.New("ast-grep: params.pattern and params.lang are required")
	}
	root := lens.SourceRoot
	if root == "" {
		root = "."
	}
	out, err := astGrepRun(cfg, lens.Params["pattern"], lens.Params["lang"], root)
	if err != nil {
		return Projection{}, err
	}
	var matches []struct {
		File  string `json:"file"`
		Text  string `json:"text"`
		Range struct {
			Start struct {
				Line int `json:"line"`
			} `json:"start"`
		} `json:"range"`
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		if err := json.Unmarshal(out, &matches); err != nil {
			return Projection{}, fmt.Errorf("ast-grep: parsing JSON output: %w\n%s", err, truncate(string(out), 300))
		}
	}
	label := coalesce(lens.Params["label"], "match")
	tmpl := lens.Params["line_format"]
	var lines []string
	for _, m := range matches {
		first := m.Text
		if i := strings.IndexByte(first, '\n'); i >= 0 {
			first = first[:i]
		}
		file := filepath.ToSlash(m.File)
		ln := m.Range.Start.Line + 1
		if tmpl == "" {
			lines = append(lines, codeLoc(strings.TrimSpace(first), file, ln))
		} else {
			lines = append(lines, formatRow(tmpl, file, ln, label, strings.TrimSpace(first)))
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		lines = append(lines, "// no ast-grep matches under "+root)
	}
	p := Projection{Sync: "view-only"}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "ast-grep", File: "model", Mode: "ast-grep", Tool: "ast-grep", Lines: lines})
	return p, nil
}

// AnalyzeBookmark pins a verbatim source span as a two-way "bookmark" block: edits
// inside the block sync back to the source span (see SyncProjection). Registered as
// both "bookmark" and the legacy "extract" alias.
func AnalyzeBookmark(cfg Config, lens LensConfig) (Projection, error) {
	file := lens.Params["file"]
	if file == "" {
		return Projection{}, errors.New("params.file is required")
	}
	root := lens.SourceRoot
	path := filepath.Join(cfg.Root, root, filepath.FromSlash(file))
	lines, err := readLines(path)
	if err != nil {
		return Projection{}, err
	}
	var a, b int
	if lens.Params["method"] != "" {
		methods, err := parseJavaMethods(lines)
		if err != nil {
			return Projection{}, err
		}
		found := false
		for _, m := range methods {
			if m.Name == lens.Params["method"] {
				a, b, found = m.Start, m.End, true
				break
			}
		}
		if !found {
			return Projection{}, fmt.Errorf("bookmark: method %q not found in %s", lens.Params["method"], file)
		}
	} else {
		a, b = parseLineRange(lens.Params["lines"])
	}
	return makeBookmarkProjection(cfg, root, file, a, b)
}

func scanRegex(cfg Config, pattern, root string) ([]grepHit, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(cfg.Root, root)
	var hits []grepHit
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if shouldSkipDir(cfg, path, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !isScannableSource(path) {
			return nil
		}
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(cfg.Root, path)
		rel = filepath.ToSlash(rel)
		for i, l := range lines {
			if re.MatchString(l) {
				hits = append(hits, grepHit{File: rel, Line: i + 1, Text: strings.TrimSpace(l)})
			}
		}
		return nil
	})
	return hits, err
}

// astGrepRun invokes ast-grep (binary `ast-grep` or `sg`, else the configured Docker
// image) with JSON output. ast-grep exits 0 even with no matches.
func astGrepRun(cfg Config, pattern, lang, root string) ([]byte, error) {
	args := []string{"run", "-p", pattern, "-l", lang, "--json", root}
	for _, bin := range []string{"ast-grep", "sg"} {
		if path, err := exec.LookPath(bin); err == nil {
			cmd := exec.Command(path, args...)
			cmd.Dir = cfg.Root
			out, err := cmd.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("ast-grep failed: %w\n%s", err, truncate(string(out), 300))
			}
			return out, nil
		}
	}
	tc, ok := cfg.Tools["ast-grep"]
	if !ok || tc.Image == "" {
		return nil, errors.New("ast-grep: not in PATH and no tools.ast-grep.image configured")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, errors.New("ast-grep: not in PATH and docker unavailable for fallback")
	}
	absRoot, _ := filepath.Abs(cfg.Root)
	dargs := []string{"run", "--rm", "-v", absRoot + ":/src", "-w", "/src", tc.Image, "ast-grep"}
	dargs = append(dargs, args...)
	cmd := exec.Command("docker", dargs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ast-grep (docker) failed: %w\n%s", err, truncate(string(out), 300))
	}
	return out, nil
}
