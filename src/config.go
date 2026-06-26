package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Config loading and project scanning / language detection.

func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Root == "" {
		cfg.Root = "."
	}
	if cfg.ProjectionsDir == "" {
		cfg.ProjectionsDir = ".projections"
	}
	if len(cfg.ExcludeDirs) == 0 {
		cfg.ExcludeDirs = defaultExcludeDirs()
	}
	return cfg, nil
}

func (l *LensConfig) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name       string         `json:"name"`
		Out        string         `json:"out,omitempty"`
		Analyzer   string         `json:"analyzer"`
		SourceRoot string         `json:"source_root,omitempty"`
		Include    []string       `json:"include,omitempty"`
		Input      string         `json:"input,omitempty"`
		Params     map[string]any `json:"params,omitempty"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	l.Name = raw.Name
	l.Out = raw.Out
	l.Analyzer = raw.Analyzer
	l.SourceRoot = raw.SourceRoot
	l.Include = raw.Include
	l.Input = raw.Input
	if raw.Params != nil {
		l.Params = map[string]string{}
		for k, v := range raw.Params {
			switch t := v.(type) {
			case string:
				l.Params[k] = t
			default:
				j, err := json.Marshal(t)
				if err != nil {
					return fmt.Errorf("lens %q param %q: %w", raw.Name, k, err)
				}
				l.Params[k] = string(j)
			}
		}
	}
	return nil
}

func defaultExcludeDirs() []string {
	return []string{".git", ".gocache", ".gomodcache", ".projections", "node_modules", "target", "build", "dist", "vendor", "__MACOSX"}
}

func shouldSkipDir(cfg Config, path string, d fs.DirEntry) bool {
	if !d.IsDir() {
		return false
	}
	name := d.Name()
	if path == cfg.Root || name == "." {
		return false
	}
	for _, ex := range cfg.ExcludeDirs {
		if ex == name || strings.HasSuffix(filepath.ToSlash(path), "/"+ex) || strings.Contains(filepath.ToSlash(path), "/"+ex+"/") {
			return true
		}
	}
	return false
}

func isScannableSource(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".java", ".go", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".kt", ".scala", ".py":
		return true
	default:
		return false
	}
}

// projectScan summarizes the source files found during the wizard's auto-detection.
type projectScan struct {
	total       int
	lang        map[string]int
	files       map[string][]string // lang -> rel paths
	srcMainJava []string
}

func scanProject(cfg Config) projectScan {
	s := projectScan{lang: map[string]int{}, files: map[string][]string{}}
	srcMain := map[string]bool{}
	filepath.WalkDir(cfg.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if shouldSkipDir(cfg, p, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !isScannableSource(p) {
			return nil
		}
		rel, _ := filepath.Rel(cfg.Root, p)
		rel = filepath.ToSlash(rel)
		lang := wizardLang(p)
		s.lang[lang]++
		s.total++
		s.files[lang] = append(s.files[lang], rel)
		if lang == "java" {
			if i := strings.Index(rel, "src/main/java"); i >= 0 {
				srcMain[rel[:i+len("src/main/java")]] = true
			}
		}
		return nil
	})
	for k := range srcMain {
		s.srcMainJava = append(s.srcMainJava, k)
	}
	sort.Strings(s.srcMainJava)
	return s
}

func wizardLang(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".java":
		return "java"
	case ".go":
		return "go"
	default:
		return "js" // .js/.ts/.jsx/.tsx/.mjs/.cjs
	}
}

func (s projectScan) summary() string {
	var parts []string
	for _, l := range []string{"java", "go", "js"} {
		if s.lang[l] > 0 {
			name := map[string]string{"java": ".java", "go": ".go", "js": ".js/.ts"}[l]
			parts = append(parts, fmt.Sprintf("%d %s", s.lang[l], name))
		}
	}
	return strings.Join(parts, ", ")
}

func (s projectScan) dominant() string {
	best, n := "js", -1
	for _, l := range []string{"java", "go", "js"} {
		if s.lang[l] > n {
			best, n = l, s.lang[l]
		}
	}
	return best
}

func (s projectScan) suggestRoot(cfg Config, lang string) string {
	switch lang {
	case "java":
		if len(s.srcMainJava) > 0 {
			return s.srcMainJava[0]
		}
		return commonDir(s.files["java"])
	case "go":
		if fileExists(filepath.Join(cfg.Root, "go.mod")) {
			return "."
		}
		return commonDir(s.files["go"])
	default:
		if fileExists(filepath.Join(cfg.Root, "package.json")) {
			return "."
		}
		return commonDir(s.files["js"])
	}
}

// commonDir returns the longest common directory of a set of rel file paths.
func commonDir(files []string) string {
	if len(files) == 0 {
		return "."
	}
	parts := strings.Split(filepath.ToSlash(filepath.Dir(files[0])), "/")
	for _, f := range files[1:] {
		fp := strings.Split(filepath.ToSlash(filepath.Dir(f)), "/")
		i := 0
		for i < len(parts) && i < len(fp) && parts[i] == fp[i] {
			i++
		}
		parts = parts[:i]
	}
	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		return "."
	}
	return strings.Join(parts, "/")
}

type sampleBM struct {
	file  string
	a, b  int
	label string
}

// sampleBookmark finds a real method (Java) or function (Go/JS) under the source root to
// offer as the user's first reference bookmark.
func (s projectScan) sampleBookmark(cfg Config, sourceRoot string) (sampleBM, bool) {
	base := filepath.Join(cfg.Root, sourceRoot)
	var got sampleBM
	found := false
	funcRE := regexp.MustCompile(`^\s*(?:export\s+)?(?:public\s+|private\s+|func\s+|function\s+|async\s+)`)
	filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if shouldSkipDir(cfg, p, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !isScannableSource(p) {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(p, ".java") {
			lines, err := readLines(p)
			if err != nil {
				return nil
			}
			methods, _ := parseJavaMethods(lines)
			for _, m := range methods {
				if m.End > m.Start {
					got = sampleBM{file: rel, a: m.Start, b: m.End, label: javaClassName(lines) + "." + m.Name}
					found = true
					return filepath.SkipDir
				}
			}
			return nil
		}
		// Go/JS: bookmark the first function-ish block (a small window).
		lines, err := readLines(p)
		if err != nil {
			return nil
		}
		for i, l := range lines {
			if funcRE.MatchString(l) && strings.Contains(l, "(") {
				end := i + 8
				if end > len(lines) {
					end = len(lines)
				}
				got = sampleBM{file: rel, a: i + 1, b: end, label: "first function in " + filepath.Base(rel)}
				found = true
				return filepath.SkipDir
			}
		}
		return nil
	})
	return got, found
}

// rootLanguage returns the dominant source language (java/go/js) under a source root.
func rootLanguage(cfg Config, root string) string {
	base := filepath.Join(cfg.Root, root)
	count := map[string]int{}
	filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if shouldSkipDir(cfg, p, d) {
			return filepath.SkipDir
		}
		if !d.IsDir() && isScannableSource(p) {
			count[wizardLang(p)]++
		}
		return nil
	})
	best, n := "", 0
	for _, l := range []string{"java", "go", "js"} {
		if count[l] > n {
			best, n = l, count[l]
		}
	}
	return best
}

// resolveSourceFile locates a referenced source file, returning its source root and the
// path relative to that root. Tries configured source roots, then the repo root, then a
// suffix search across the tree. References must stay inside the repo (no `..`/absolute).
func resolveSourceFile(cfg Config, ref string) (sourceRoot, rel string, err error) {
	ref = filepath.ToSlash(ref)
	if filepath.IsAbs(ref) || ref == ".." || strings.HasPrefix(ref, "../") || strings.Contains(ref, "/../") || strings.HasSuffix(ref, "/..") {
		return "", "", fmt.Errorf("unsafe source reference %q (must be inside the repo)", ref)
	}
	for _, lens := range cfg.Lenses {
		if lens.SourceRoot == "" {
			continue
		}
		if fileExists(filepath.Join(cfg.Root, lens.SourceRoot, filepath.FromSlash(ref))) {
			return lens.SourceRoot, ref, nil
		}
	}
	if fileExists(filepath.Join(cfg.Root, filepath.FromSlash(ref))) {
		return ".", ref, nil
	}
	// Suffix search: find a file whose path ends with the reference.
	var foundRoot, foundRel string
	_ = filepath.WalkDir(cfg.Root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if shouldSkipDir(cfg, p, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || foundRel != "" {
			return nil
		}
		relPath, _ := filepath.Rel(cfg.Root, p)
		relPath = filepath.ToSlash(relPath)
		if relPath == ref || strings.HasSuffix(relPath, "/"+ref) {
			foundRoot = strings.TrimSuffix(strings.TrimSuffix(relPath, ref), "/")
			if foundRoot == "" {
				foundRoot = "."
			}
			foundRel = ref
		}
		return nil
	})
	if foundRel != "" {
		return foundRoot, foundRel, nil
	}
	return "", "", fmt.Errorf("could not resolve source file %q under any source root", ref)
}

// LensByName returns the lens with the given Name, or the zero LensConfig and false if none matches.
func (c Config) LensByName(name string) (LensConfig, bool) {
	for _, lens := range c.Lenses {
		if lens.Name == name {
			return lens, true
		}
	}
	return LensConfig{}, false
}
