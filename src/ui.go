package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The `ui` web studio: HTTP handlers + embedded single-page app.

type uiServer struct {
	mu          sync.Mutex
	cfg         Config
	configPath  string
	registry    Registry
	detectCache map[string]uiDefaults // keyed by chosen source root; avoids re-walking on every change
}

type uiDefaults struct {
	SourceRoot  string `json:"source_root"`
	EntryFile   string `json:"entry_file"`
	EntryMethod string `json:"entry_method"`
	EntryLine   int    `json:"entry_line"`
	Language    string `json:"language"`
	Analyzer    string `json:"analyzer"`
	// Real-repo examples so each lens is prefilled and runnable on first click.
	ExampleVar  string `json:"example_var"`
	ExampleType string `json:"example_type"`
}

func RunUI(cfg Config, configPath, addr string, out io.Writer) error {
	if configPath == "" {
		configPath = "config.json"
	}
	s := &uiServer{cfg: cfg, configPath: configPath, registry: DefaultRegistry()}
	mux := http.NewServeMux()
	// Serve the composable UI assets (HTML shell + css + js modules) from the
	// embedded ui/ directory. "/" serves the shell; "/ui/*" serves the rest.
	assets, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return err
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		shell, _ := uiFS.ReadFile("ui/index.html")
		w.Write(shell)
	})
	mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/symbols", s.handleSymbols)
	mux.HandleFunc("/api/vars", s.handleVars)
	mux.HandleFunc("/api/lenses", s.handleLenses)
	mux.HandleFunc("/api/graph", s.handleGraph)
	mux.HandleFunc("/api/dirs", s.handleDirs)
	mux.HandleFunc("/api/detect", s.handleDetect)
	mux.HandleFunc("/api/unroll", s.handleUnroll)
	mux.HandleFunc("/api/unroll/edit", s.handleUnrollEdit)
	mux.HandleFunc("/api/clone", s.handleClone)
	mux.HandleFunc("/api/ask", s.handleAsk)
	fmt.Fprintf(out, "file-projections ui on http://localhost%s  (config: %s)\n", addr, configPath)
	return http.ListenAndServe(addr, mux)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *uiServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		var c Config
		if err := json.Unmarshal(body, &c); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid config: " + err.Error()})
			return
		}
		if err := os.WriteFile(s.configPath, body, 0644); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		s.mu.Lock()
		s.cfg = c
		s.detectCache = nil // config changed; re-detect from scratch
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "lenses": len(c.Lenses)})
		return
	}
	raw, err := os.ReadFile(s.configPath)
	if err != nil {
		// no config yet: hand back the in-memory defaults
		s.mu.Lock()
		raw, _ = json.MarshalIndent(s.cfg, "", "  ")
		s.mu.Unlock()
	}
	s.mu.Lock()
	def := suggestUIDefaults(s.cfg)
	s.mu.Unlock()
	analyzers := sortedAnalyzerNames(s.registry)
	writeJSON(w, 200, map[string]any{
		"config":        json.RawMessage(raw),
		"analyzers":     analyzers,
		"applicability": analyzerApplicability(),
		"specs":         analyzerSpecs(),
		"questions":     questionRegistry(),
		"path":          s.configPath,
		"defaults":      def,
	})
}

// handleDetect re-detects the dominant language under a given source root so the UI
// can re-filter analyzers and re-prefill examples when the user changes the root.
func (s *uiServer) handleDetect(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	s.mu.Lock()
	cfg := s.cfg
	if cached, ok := s.detectCache[root]; ok {
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"language": cached.Language, "defaults": cached, "cached": true})
		return
	}
	s.mu.Unlock()
	if root != "" {
		cfg.Root = filepath.Join(cfg.Root, root)
	}
	def := suggestUIDefaults(cfg)
	if root != "" {
		// suggestUIDefaults picked a nested source root (e.g. "sample") under the
		// chosen root and reported entry_file relative to it. Re-base entry_file to
		// the chosen root so it matches the source_root the UI actually uses.
		if def.SourceRoot != "" && def.SourceRoot != "." && def.EntryFile != "" {
			def.EntryFile = filepath.ToSlash(filepath.Join(def.SourceRoot, def.EntryFile))
		}
		def.SourceRoot = root // keep the user's chosen root; defaults re-scanned under it
	}
	s.mu.Lock()
	if s.detectCache == nil {
		s.detectCache = map[string]uiDefaults{}
	}
	s.detectCache[root] = def
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"language": def.Language, "defaults": def})
}

// handleDirs lists immediate subdirectories of a path (relative to cfg.Root) so the
// source_root "Change" button can browse instead of forcing the user to type a path.
func (s *uiServer) handleDirs(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	base := filepath.Join(cfg.Root, rel)
	entries, err := os.ReadDir(base)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "target" {
			continue
		}
		dirs = append(dirs, name)
	}
	sort.Strings(dirs)
	writeJSON(w, 200, map[string]any{"path": filepath.ToSlash(rel), "dirs": dirs})
}

// handleLenses lets the UI bookmark a *configured lens* (analyzer + source root +
// params), not just a code range — GET lists saved lenses; POST upserts one into
// config.json (or removes it with delete:true), so a useful lens setup is one click
// to re-open later. Saved lenses are the same LensConfig the CLI/build use.
func (s *uiServer) handleLenses(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Name       string            `json:"name"`
			Analyzer   string            `json:"analyzer"`
			SourceRoot string            `json:"source_root"`
			Params     map[string]string `json:"params"`
			Out        string            `json:"out"`
			Delete     bool              `json:"delete"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeJSON(w, 400, map[string]string{"error": "name is required"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		lenses := s.cfg.Lenses[:0:0]
		found := false
		for _, l := range s.cfg.Lenses {
			if l.Name == req.Name {
				found = true
				if req.Delete {
					continue // drop it
				}
				lenses = append(lenses, LensConfig{Name: req.Name, Analyzer: req.Analyzer, SourceRoot: req.SourceRoot, Params: req.Params, Out: l.Out})
				continue
			}
			lenses = append(lenses, l)
		}
		if !found && !req.Delete {
			out := req.Out
			if out == "" {
				out = filepath.ToSlash(filepath.Join(s.cfg.ProjectionsDir, req.Name+".projection"))
			}
			lenses = append(lenses, LensConfig{Name: req.Name, Analyzer: req.Analyzer, SourceRoot: req.SourceRoot, Params: req.Params, Out: out})
		}
		s.cfg.Lenses = lenses
		raw, err := json.MarshalIndent(s.cfg, "", "  ")
		if err == nil {
			err = os.WriteFile(s.configPath, raw, 0644)
		}
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "lenses": s.lensSummaries()})
		return
	}
	s.mu.Lock()
	out := s.lensSummaries()
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"lenses": out})
}

// lensSummaries returns the saved lenses as light records for the UI. Caller holds s.mu.
func (s *uiServer) lensSummaries() []map[string]any {
	out := make([]map[string]any, 0, len(s.cfg.Lenses))
	for _, l := range s.cfg.Lenses {
		out = append(out, map[string]any{"name": l.Name, "analyzer": l.Analyzer, "source_root": l.SourceRoot, "params": l.Params})
	}
	return out
}

// handleGraph runs a service-graph lens (named via ?lens=, else the first one in
// config) and returns the parsed {services,nodes,edges} so the UI can render the
// whole-service map and export mermaid. Also lists available graph lenses.
func (s *uiServer) handleGraph(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cfg := s.cfg
	reg := s.registry
	s.mu.Unlock()
	want := r.URL.Query().Get("lens")
	var lenses []string
	var chosen *LensConfig
	for i := range cfg.Lenses {
		if cfg.Lenses[i].Analyzer == "service-graph" {
			lenses = append(lenses, cfg.Lenses[i].Name)
			if chosen == nil || cfg.Lenses[i].Name == want {
				chosen = &cfg.Lenses[i]
			}
		}
	}
	if chosen == nil {
		writeJSON(w, 200, map[string]any{"error": "no service-graph lens in config.json", "lenses": lenses})
		return
	}
	p, err := ExecuteLens(cfg, reg, *chosen)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error(), "lenses": lenses})
		return
	}
	var graph json.RawMessage
	for _, f := range p.Facts {
		if f.ID == "graph" && f.Tool == "service-graph" {
			graph = json.RawMessage(f.Text)
		}
	}
	writeJSON(w, 200, map[string]any{"lens": chosen.Name, "lenses": lenses, "graph": graph, "source_root": chosen.SourceRoot})
}

// handleVars returns identifier names declared in a file (locals, params, fields) so
// the data-flow/joern-var-flow `var` param is autosuggestable instead of copy-pasted.
func (s *uiServer) handleVars(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	file := r.URL.Query().Get("file")
	q := strings.ToLower(r.URL.Query().Get("q"))
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if file == "" {
		writeJSON(w, 200, map[string]any{"vars": []string{}})
		return
	}
	lines, err := readLines(filepath.Join(cfg.Root, root, file))
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	seen := map[string]bool{}
	var vars []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			return
		}
		seen[name] = true
		vars = append(vars, name)
	}
	declRE := []*regexp.Regexp{uiJavaLocalRE, uiGoLocalRE, regexp.MustCompile(`\b([a-z][A-Za-z0-9_]*)\s*=[^=]`)}
	for _, l := range lines {
		for _, re := range declRE {
			if m := re.FindStringSubmatch(l); m != nil {
				add(m[1])
			}
		}
	}
	sort.Strings(vars)
	writeJSON(w, 200, map[string]any{"vars": vars})
}

func suggestUIDefaults(cfg Config) uiDefaults {
	def := uiDefaults{}
	scan := scanProject(cfg)
	lang := scan.dominant()
	if fileExists(filepath.Join(cfg.Root, "go.mod")) || fileExists(filepath.Join(cfg.Root, "main.go")) {
		lang = "go"
	}
	def.Language = lang
	def.SourceRoot = scan.suggestRoot(cfg, lang)
	if def.SourceRoot == "" {
		def.SourceRoot = "."
	}
	def.EntryFile, def.EntryMethod, def.EntryLine = suggestUIMethod(cfg, def.SourceRoot, lang)
	def.ExampleVar, def.ExampleType = suggestUIExamples(cfg, def.SourceRoot, def.EntryFile, lang)
	if def.EntryLine == 0 {
		def.EntryLine = 1
	}
	switch lang {
	case "go":
		def.Analyzer = "go-symbols"
	case "java":
		def.Analyzer = "unrolled-program"
	default:
		def.Analyzer = "entrypoints"
	}
	return def
}

// suggestUIMethod picks a sensible default entry function/method under a source
// root using the language-neutral symbol index, so it works for every registered
// language (including TS) without a per-language walk here.
func suggestUIMethod(cfg Config, sourceRoot, lang string) (file, method string, line int) {
	syms, err := allSymbols(cfg, sourceRoot, "", 0)
	if err != nil {
		return "", "", 0
	}
	preferred := map[string]int{"summary": 100, "main": 90, "handle": 80, "process": 70, "checkout": 60, "build": 50, "run": 40}
	if lang == "go" {
		preferred = map[string]int{"run": 120, "main": 100, "executelens": 90, "analyze": 80, "handle": 70, "build": 60}
	}
	type cand struct {
		file, method string
		score, line  int
	}
	var cands []cand
	for _, s := range syms {
		if s.Kind != "func" && s.Kind != "method" {
			continue
		}
		if strings.Contains(s.File, "/test/") || strings.Contains(strings.ToLower(s.File), "_test.") || strings.Contains(strings.ToLower(s.File), ".test.") {
			continue
		}
		score := preferred[strings.ToLower(s.Name)]
		base := strings.ToLower(filepath.Base(s.File))
		if strings.Contains(base, "controller") {
			score += 10
		}
		if base == "main.go" {
			score += 20
		}
		if strings.HasPrefix(s.Name, "Handle") || strings.HasSuffix(s.Name, "Handler") {
			score += 15
		}
		cands = append(cands, cand{file: s.File, method: s.Name, score: score, line: s.Line})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score == cands[j].score {
			if cands[i].file == cands[j].file {
				return cands[i].line < cands[j].line
			}
			return cands[i].file < cands[j].file
		}
		return cands[i].score > cands[j].score
	})
	if len(cands) == 0 {
		return "", "", 0
	}
	return cands[0].file, cands[0].method, cands[0].line
}

func suggestUIExamples(cfg Config, sourceRoot, entryFile, lang string) (varName, typeName string) {
	if entryFile == "" {
		return "", ""
	}
	lines, err := readLines(filepath.Join(cfg.Root, sourceRoot, entryFile))
	if err != nil {
		return "", ""
	}
	localRE := uiJavaLocalRE
	if lang == "go" {
		localRE = uiGoLocalRE
	}
	for _, l := range lines {
		if varName == "" {
			if m := localRE.FindStringSubmatch(l); m != nil {
				varName = m[1]
			}
		}
		if typeName == "" {
			if m := uiJavaClassRE.FindStringSubmatch(l); m != nil {
				typeName = m[2]
			}
		}
	}
	return varName, typeName
}

// handlePreview runs a single ad-hoc lens and returns the rendered projection
// body — exactly what would be written to disk, minus the volatile header — so a
// user can try a lens/params combo on a real project before committing it to config.
func (s *uiServer) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Analyzer   string            `json:"analyzer"`
		SourceRoot string            `json:"source_root"`
		Include    []string          `json:"include"`
		Params     map[string]any    `json:"params"`
		ParamsStr  map[string]string `json:"-"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.Analyzer == "" {
		writeJSON(w, 400, map[string]string{"error": "analyzer is required"})
		return
	}
	params := map[string]string{}
	for k, v := range req.Params {
		switch t := v.(type) {
		case string:
			params[k] = t
		case float64:
			params[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			params[k] = strconv.FormatBool(t)
		default:
			if b, err := json.Marshal(v); err == nil {
				params[k] = string(b)
			}
		}
	}
	s.mu.Lock()
	cfg := s.cfg
	reg := s.registry
	s.mu.Unlock()
	lens := LensConfig{
		Name:       "ui-preview",
		Analyzer:   req.Analyzer,
		SourceRoot: req.SourceRoot,
		Include:    req.Include,
		Params:     params,
	}
	p, err := ExecuteLens(cfg, reg, lens)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	body := projectionBody(p)
	extra := make([]map[string]any, 0, len(p.Extra))
	for _, ex := range p.Extra {
		extra = append(extra, map[string]any{"path": ex.Path, "body": projectionBody(ex.Proj)})
	}
	writeJSON(w, 200, map[string]any{
		"body":   body,
		"sync":   p.Sync,
		"blocks": len(p.Blocks),
		"facts":  len(p.Facts),
		"extra":  extra,
	})
}

// handleAsk compiles a Question (id + blank values) into a lens and runs it — the
// backend of the Questions panel. Same ExecuteLens path as preview; additionally
// returns the question's confidence badge and any confidence fact the lens emitted,
// so the UI can tell the user how much to trust the answer.
func (s *uiServer) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID         string            `json:"id"`
		SourceRoot string            `json:"source_root"`
		Values     map[string]string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	lens, conf, ok := compileQuestion(req.ID, req.SourceRoot, req.Values)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "unknown question " + req.ID})
		return
	}
	s.mu.Lock()
	cfg := s.cfg
	reg := s.registry
	s.mu.Unlock()
	p, err := ExecuteLens(cfg, reg, lens)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error(), "analyzer": lens.Analyzer})
		return
	}
	// A lens may refine the confidence (e.g. side-effects/query lenses emit a
	// `confidence` fact); prefer it over the question's default badge.
	confNote := ""
	for _, f := range p.Facts {
		if f.ID == "confidence" {
			if c, note, found := strings.Cut(f.Text, ": "); found {
				conf, confNote = c, note
			}
		}
	}
	extra := make([]map[string]any, 0, len(p.Extra))
	for _, ex := range p.Extra {
		extra = append(extra, map[string]any{"path": ex.Path, "body": projectionBody(ex.Proj)})
	}
	writeJSON(w, 200, map[string]any{
		"body":       projectionBody(p),
		"analyzer":   lens.Analyzer,
		"confidence": conf,
		"conf_note":  confNote,
		"blocks":     len(p.Blocks),
		"facts":      len(p.Facts),
		"extra":      extra,
	})
}

// handleSymbols searches source under a root for declarations (functions, methods,
// types/classes) matching q — so a user picking control-flow/data-flow/object-flow
// params has the real file:line/type to fill in, the way an MCP symbol search would.
// Languages are pluggable via the Language registry (language.go), so this handler
// is language-agnostic.
func (s *uiServer) handleSymbols(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	root := r.URL.Query().Get("root")
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	syms, err := allSymbols(cfg, root, q, 200)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"symbols": syms})
}

func (s *uiServer) unrollProjection(srcRoot, file, method, inputs, branches, inlineDepth, inlineSkips string) (Projection, Config, Registry, error) {
	s.mu.Lock()
	cfg := s.cfg
	reg := s.registry
	s.mu.Unlock()
	lens := LensConfig{
		Name:       "ui-unroll",
		Out:        filepath.Join(cfg.ProjectionsDir, "ui-unroll.projection"),
		Analyzer:   "unrolled-program",
		SourceRoot: srcRoot,
		// branch_select makes undecidable conditionals collapse to one (toggleable) side
		// instead of inlining both — the UI's branch-tabs experience.
		Params: map[string]string{"file": file, "method": method, "branch_select": "1"},
	}
	if inputs != "" {
		lens.Params["inputs"] = inputs
	}
	if branches != "" {
		lens.Params["branches"] = branches
	}
	if inlineDepth != "" {
		lens.Params["inline_depth"] = inlineDepth
	}
	if inlineSkips != "" {
		lens.Params["inline_skips"] = inlineSkips
	}
	p, err := ExecuteLens(cfg, reg, lens)
	return p, cfg, reg, err
}

// handleUnroll renders the straight-line program for an entry method. With no inputs the
// analyzer keeps the `if (...)` headers in place (branch discovery); with inputs it collapses
// to the single path those inputs execute.
func (s *uiServer) handleUnroll(w http.ResponseWriter, r *http.Request) {
	var req struct{ SourceRoot, File, Method, Inputs, Branches, InlineDepth, InlineSkips string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.File == "" || req.Method == "" {
		writeJSON(w, 400, map[string]string{"error": "file and method are required"})
		return
	}
	p, _, _, err := s.unrollProjection(req.SourceRoot, req.File, req.Method, req.Inputs, req.Branches, req.InlineDepth, req.InlineSkips)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	lines := unrollViewLines(p)
	unresolved := false
	for _, l := range lines {
		if l.Branch {
			unresolved = true
		}
	}
	writeJSON(w, 200, map[string]any{"lines": lines, "unresolved": unresolved, "inputs": req.Inputs,
		"decisions": unrollDecisionFacts(p), "choices": unrollChoices(p), "calls": unrollCalls(p)})
}

// handleUnrollEdit applies one line edit and writes it back to that line's origin file via the
// real SyncProjection (scattered two-way), then returns the refreshed program. Exactly the CLI
// path: render projection -> edit the block line -> sync -> re-render.
func (s *uiServer) handleUnrollEdit(w http.ResponseWriter, r *http.Request) {
	type uiEdit struct {
		Line    int
		NewCode string
	}
	var req struct {
		SourceRoot, File, Method, Inputs, Branches, InlineDepth, InlineSkips, NewCode string
		Line                                                                          int
		Edits                                                                         []uiEdit
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if len(req.Edits) == 0 && req.Line > 0 {
		req.Edits = []uiEdit{{Line: req.Line, NewCode: req.NewCode}}
	}
	if len(req.Edits) == 0 {
		writeJSON(w, 400, map[string]string{"error": "no edits supplied"})
		return
	}
	p, cfg, reg, err := s.unrollProjection(req.SourceRoot, req.File, req.Method, req.Inputs, req.Branches, req.InlineDepth, req.InlineSkips)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	lines := unrollViewLines(p)
	seen := map[int]bool{}
	for _, edit := range req.Edits {
		if edit.Line < 1 || edit.Line > len(lines) {
			writeJSON(w, 200, map[string]any{"error": fmt.Sprintf("line must be 1..%d", len(lines))})
			return
		}
		if seen[edit.Line] {
			writeJSON(w, 200, map[string]any{"error": fmt.Sprintf("duplicate edit for line %d", edit.Line)})
			return
		}
		seen[edit.Line] = true
	}
	// Render to a temp projection, replace the edited mapped lines exactly, then sync back.
	projPath := filepath.Join(cfg.Root, cfg.ProjectionsDir, "ui-unroll.projection")
	if err := RenderProjection(projPath, p); err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	raw, err := readLines(projPath)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	bi := -1
	for i, l := range raw {
		if strings.HasPrefix(l, "@@ ") {
			bi = i
			break
		}
	}
	if bi < 0 {
		writeJSON(w, 200, map[string]any{"error": "no block in projection"})
		return
	}
	for _, edit := range req.Edits {
		raw[bi+edit.Line] = strings.TrimRight(edit.NewCode, "\r\n")
	}
	if err := writeLines(projPath, raw); err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	res, err := SyncProjection(cfg, projPath)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	// Re-render from current source so the UI shows the synced state.
	p2, err := ExecuteLens(cfg, reg, p.Lens)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	origin := lines[req.Edits[0].Line-1].Origin
	unresolved := false
	for _, l := range unrollViewLines(p2) {
		if l.Branch {
			unresolved = true
		}
	}
	writeJSON(w, 200, map[string]any{
		"lines":      unrollViewLines(p2),
		"unresolved": unresolved,
		"inputs":     req.Inputs,
		"decisions":  unrollDecisionFacts(p2),
		"choices":    unrollChoices(p2),
		"calls":      unrollCalls(p2),
		"synced":     fmt.Sprintf("%d → source, %d conflicts", res.ToSource, len(res.Conflicts)),
		"conflicts":  res.Conflicts,
		"origin":     origin,
	})
}

// handleClone shallow-clones a GitHub repo into a working dir under the repo root and
// returns a source-root-relative path the UI can immediately detect/run lenses against.
// Same clone logic the `clone` CLI command uses.
func (s *uiServer) handleClone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	url, name, err := normalizeGitURL(req.URL)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	// Clone under <root>/workspace/clones so the result is a path relative to cfg.Root,
	// which is exactly what every source-root-based handler (detect/symbols/...) expects.
	destAbs := filepath.Join(cfg.Root, "workspace", "clones")
	target, err := cloneRepo(url, name, destAbs, nil)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	rel, err := filepath.Rel(cfg.Root, target)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	// Invalidate detection cache so the new root is re-scanned.
	s.mu.Lock()
	s.detectCache = nil
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"root": filepath.ToSlash(rel), "url": url})
}
