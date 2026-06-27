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
	mux.HandleFunc("/api/workspace", s.handleWorkspace)
	mux.HandleFunc("/api/trace", s.handleTrace)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/trace-symbols", s.handleTraceSymbols)
	mux.HandleFunc("/api/lens-templates", s.handleLensTemplates)
	mux.HandleFunc("/api/tables", s.handleTables)
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
		"projects":      projectsView(s.cfg),
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
	// Absolute paths are allowed so the project folder-chooser can browse anywhere on
	// disk (repos may live outside cfg.Root); relative paths resolve under cfg.Root.
	base := rel
	if !filepath.IsAbs(rel) {
		base = filepath.Join(cfg.Root, rel)
	}
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
	out := filepath.ToSlash(rel)
	if filepath.IsAbs(rel) {
		out = filepath.ToSlash(base)
	}
	writeJSON(w, 200, map[string]any{"path": out, "dirs": dirs, "abs": filepath.IsAbs(rel)})
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

// handleWorkspace is the cross-repo workspace API (CROSS-REPO.md §E). GET lists the
// registered repos with their detected gradle group and internal-dependency edges;
// POST adds a repo by local folder ("link") or git ref ("clone"); DELETE removes one.
func (s *uiServer) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := LoadWorkspace()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Kind string `json:"kind"` // "link" | "clone"
			Path string `json:"path"` // for link
			URL  string `json:"url"`  // for clone (url or owner/repo)
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		var repo *WorkspaceRepo
		switch req.Kind {
		case "clone":
			repo, err = ws.AddClone(coalesce(req.URL, req.Path), nil)
		default:
			repo, err = ws.AddLink(req.Path, req.Name)
		}
		if err != nil {
			writeJSON(w, 200, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "repo": repo, "workspace": workspaceView(ws)})
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if err := ws.Remove(name); err != nil {
			writeJSON(w, 200, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "workspace": workspaceView(ws)})
	default:
		writeJSON(w, 200, workspaceView(ws))
	}
}

// workspaceView decorates the workspace with per-repo internal-dependency edges so
// the UI/CI can highlight which repos form one logical service (same gradle group).
func workspaceView(ws *Workspace) map[string]any {
	type repoView struct {
		WorkspaceRepo
		Internal  []string `json:"internal_deps"`
		HasGradle bool     `json:"has_gradle"`
	}
	// Resolve every repo's gradle info once so the internal-dep comparison (which
	// needs the OTHER repos' groups) works even when repos were loaded from
	// workspace.json without a cached group.
	infos := make([]GradleInfo, len(ws.Repos))
	for i, r := range ws.Repos {
		infos[i] = detectGradle(r.Path)
		if ws.Repos[i].Group == "" {
			ws.Repos[i].Group = infos[i].Group
		}
	}
	var views []repoView
	for i, r := range ws.Repos {
		views = append(views, repoView{
			WorkspaceRepo: r,
			Internal:      internalDepsAmong(infos[i], ws.Repos, r.Name),
			HasGradle:     infos[i].HasGradle,
		})
	}
	return map[string]any{"home": ws.Home, "repos": views}
}

// handleTrace runs the cross-repo, DI-aware trace lens over the active config
// project and returns the multi-answer result. Input is a symbol (preferred) or a
// file:line; include_libraries scopes whether library repos are searched.
func (s *uiServer) handleTrace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project          string `json:"project"`
		Symbol           string `json:"symbol"`
		Repo             string `json:"repo"`
		File             string `json:"file"`
		Line             int    `json:"line"`
		IncludeLibraries *bool  `json:"include_libraries"`
		Max              int    `json:"max_paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	params := map[string]string{
		"project": req.Project, "symbol": req.Symbol,
		"repo": req.Repo, "file": req.File, "line": strconv.Itoa(req.Line),
	}
	if req.IncludeLibraries != nil && !*req.IncludeLibraries {
		params["include_libraries"] = "false"
	}
	if req.Max > 0 {
		params["max_paths"] = strconv.Itoa(req.Max)
	}
	lens := LensConfig{Name: "trace", Analyzer: "trace-to-line", Params: params}
	ws, err := resolveTraceWorkspace(cfg, lens)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	p, err := TraceToLine(cfg, lens, ws)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	// Flatten: summary block + one "answer" per Extra projection.
	var summary []string
	for _, bl := range p.Blocks {
		summary = append(summary, bl.Lines...)
	}
	type answer struct {
		Lines      []string `json:"lines"`
		Confidence string   `json:"confidence"`
		Note       string   `json:"note"`
	}
	var answers []answer
	for _, ex := range p.Extra {
		a := answer{}
		for _, bl := range ex.Proj.Blocks {
			a.Lines = append(a.Lines, bl.Lines...)
		}
		for _, f := range ex.Proj.Facts {
			if f.ID == "confidence" {
				if c, note, ok := strings.Cut(f.Text, ": "); ok {
					a.Confidence, a.Note = c, note
				}
			}
		}
		answers = append(answers, a)
	}
	// Tell the UI whether enabling libraries would widen the search (the
	// "expand with library" affordance), and which libraries exist.
	libs := libraryReposOf(activeProjectFor(cfg, req.Project))
	writeJSON(w, 200, map[string]any{"summary": summary, "answers": answers, "libraries": libs})
}

// activeProjectFor returns the named project, else the active one.
func activeProjectFor(cfg Config, name string) *ProjectConfig {
	if name != "" {
		if p := projectByName(cfg, name); p != nil {
			return p
		}
	}
	return activeProject(cfg)
}

// persistConfig writes the in-memory config back to configPath (single source of
// truth) and resets the detect cache. Caller holds no lock.
func (s *uiServer) persistConfig(cfg Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.configPath, raw, 0644); err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.detectCache = nil
	s.mu.Unlock()
	return nil
}

// handleProjects is the cross-repo project API, backed by config.json (the single
// source of truth). GET lists projects (with detected groups + internal-dep edges);
// POST adds/updates a project or a repo (writing config); DELETE removes one.
func (s *uiServer) handleProjects(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if cfg.Workspace == nil {
		cfg.Workspace = &WorkspaceConfig{}
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Action  string `json:"action"` // "set-active" | "add-repo" | "new-project"
			Project string `json:"project"`
			// add-repo:
			RepoName string `json:"repo_name"`
			Path     string `json:"path"` // local folder
			URL      string `json:"url"`  // git clone ref
			Role     string `json:"role"` // app | library
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		switch req.Action {
		case "set-active":
			cfg.Workspace.Active = req.Project
		case "new-project":
			if req.Project == "" {
				writeJSON(w, 200, map[string]any{"error": "project name required"})
				return
			}
			if projectByName(cfg, req.Project) == nil {
				cfg.Workspace.Projects = append(cfg.Workspace.Projects, ProjectConfig{Name: req.Project})
			}
			cfg.Workspace.Active = req.Project
		case "add-repo":
			proj := findProjectPtr(&cfg, req.Project)
			if proj == nil {
				writeJSON(w, 200, map[string]any{"error": "unknown project " + req.Project})
				return
			}
			path, name, err := s.materializeRepo(cfg, req.Path, req.URL, req.RepoName)
			if err != nil {
				writeJSON(w, 200, map[string]any{"error": err.Error()})
				return
			}
			role := req.Role
			if role == "" {
				role = "app"
			}
			// replace existing repo of same name, else append
			replaced := false
			for i := range proj.Repos {
				if proj.Repos[i].Name == name {
					proj.Repos[i] = RepoConfig{Name: name, Path: path, Role: role}
					replaced = true
				}
			}
			if !replaced {
				proj.Repos = append(proj.Repos, RepoConfig{Name: name, Path: path, Role: role})
			}
		default:
			writeJSON(w, 200, map[string]any{"error": "unknown action " + req.Action})
			return
		}
		if err := s.persistConfig(cfg); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "projects": projectsView(cfg)})
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("project"))
		repo := strings.TrimSpace(r.URL.Query().Get("repo"))
		proj := findProjectPtr(&cfg, name)
		if proj != nil && repo != "" {
			var kept []RepoConfig
			for _, rc := range proj.Repos {
				if rc.Name != repo {
					kept = append(kept, rc)
				}
			}
			proj.Repos = kept
		} else if name != "" {
			var kept []ProjectConfig
			for _, pc := range cfg.Workspace.Projects {
				if pc.Name != name {
					kept = append(kept, pc)
				}
			}
			cfg.Workspace.Projects = kept
			if cfg.Workspace.Active == name {
				cfg.Workspace.Active = ""
			}
		}
		if err := s.persistConfig(cfg); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "projects": projectsView(cfg)})
	default:
		writeJSON(w, 200, projectsView(cfg))
	}
}

// findProjectPtr returns a pointer into cfg's project slice (so edits persist).
func findProjectPtr(cfg *Config, name string) *ProjectConfig {
	if cfg.Workspace == nil {
		return nil
	}
	for i := range cfg.Workspace.Projects {
		if name == "" || cfg.Workspace.Projects[i].Name == name {
			return &cfg.Workspace.Projects[i]
		}
	}
	return nil
}

// materializeRepo turns a folder path or a clone URL into a stored repo path
// (relative to cfg.Root when possible) and a stable name.
func (s *uiServer) materializeRepo(cfg Config, path, url, name string) (storedPath, repoName string, err error) {
	if strings.TrimSpace(url) != "" {
		u, n, e := normalizeGitURL(url)
		if e != nil {
			return "", "", e
		}
		dest := filepath.Join(cfg.Root, "workspace", "clones")
		target, e := cloneRepo(u, n, dest, nil)
		if e != nil {
			return "", "", e
		}
		if rel, e := filepath.Rel(cfg.Root, target); e == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel), coalesce(name, n), nil
		}
		return target, coalesce(name, n), nil
	}
	if strings.TrimSpace(path) == "" {
		return "", "", fmt.Errorf("provide a folder path or a clone URL")
	}
	abs, e := filepath.Abs(path)
	if e != nil {
		return "", "", e
	}
	if st, e := os.Stat(abs); e != nil || !st.IsDir() {
		return "", "", fmt.Errorf("not a directory: %s", abs)
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	if rel, e := filepath.Rel(cfg.Root, abs); e == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel), name, nil
	}
	return abs, name, nil
}

// projectsView decorates each project's repos with detected gradle group + internal
// dependency edges so the UI can show which repos form one logical service.
func projectsView(cfg Config) map[string]any {
	type repoView struct {
		RepoConfig
		Group    string   `json:"group"`
		Internal []string `json:"internal_deps"`
	}
	type projView struct {
		Name  string     `json:"name"`
		Repos []repoView `json:"repos"`
	}
	var out []projView
	if cfg.Workspace != nil {
		for _, p := range cfg.Workspace.Projects {
			ws := workspaceFromProject(cfg, &p, false)
			infos := make([]GradleInfo, len(ws.Repos))
			for i, r := range ws.Repos {
				infos[i] = detectGradle(r.Path)
			}
			pv := projView{Name: p.Name}
			for i, rc := range p.Repos {
				var info GradleInfo
				if i < len(infos) {
					info = infos[i]
				}
				pv.Repos = append(pv.Repos, repoView{
					RepoConfig: rc, Group: info.Group,
					Internal: internalDepsAmong(info, ws.Repos, rc.Name),
				})
			}
			out = append(out, pv)
		}
	}
	active := ""
	if cfg.Workspace != nil {
		active = cfg.Workspace.Active
	}
	if active == "" && len(out) > 0 {
		active = out[0].Name
	}
	return map[string]any{"projects": out, "active": active}
}

// handleTraceSymbols autocompletes method/type names across the active project's
// repos (so the trace symbol input suggests across the whole workspace, not one root).
func (s *uiServer) handleTraceSymbols(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	projName := strings.TrimSpace(r.URL.Query().Get("project"))
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	proj := activeProjectFor(cfg, projName)
	if proj == nil {
		writeJSON(w, 200, map[string]any{"symbols": []any{}})
		return
	}
	ws := workspaceFromProject(cfg, proj, false)
	idx := buildTypeIndex(cfg, ws)
	type sym struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		Repo string `json:"repo"`
		File string `json:"file"`
		Line int    `json:"line"`
	}
	var out []sym
	seen := map[string]bool{}
	for _, t := range idx.all {
		if q == "" || strings.Contains(strings.ToLower(t.Name), q) {
			k := "type:" + t.Repo + ":" + t.Name
			if !seen[k] {
				seen[k] = true
				out = append(out, sym{Name: t.Name, Kind: t.Kind, Repo: t.Repo, File: t.File, Line: t.Line})
			}
		}
		for _, m := range t.Methods {
			full := t.Name + "." + m.Name
			if q == "" || strings.Contains(strings.ToLower(m.Name), q) || strings.Contains(strings.ToLower(full), q) {
				k := "m:" + t.Repo + ":" + full
				if !seen[k] {
					seen[k] = true
					out = append(out, sym{Name: full, Kind: "method", Repo: t.Repo, File: t.File, Line: m.Start})
				}
			}
		}
		if len(out) >= 50 {
			break
		}
	}
	// Tables are trace targets too (TABLES.md §C): suggest discovered physical tables.
	dbm := buildDBModel(cfg, ws, idx)
	for _, tbl := range dbm.sortedTableNames() {
		if q == "" || strings.Contains(strings.ToLower(tbl), q) {
			ti := dbm.Tables[tbl]
			repo := coalesce(ti.MigRepo, ti.EntityRepo)
			out = append(out, sym{Name: tbl, Kind: "table", Repo: repo, File: firstStr(ti.Migrations)})
		}
	}
	writeJSON(w, 200, map[string]any{"symbols": out})
}

func firstStr(ss []string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return ""
}

// handleLensTemplates serves the guided "Add a lens" wizard's catalogue (TABLES.md
// §E): each template is a lens type with a one-line description and example params
// prefilled for the active project, so adding a lens to a new project is click-not-type.
func (s *uiServer) handleLensTemplates(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	proj := activeProject(cfg)

	// Discover the active project's tables so the SQL-watch template can prefill real
	// table names the user can already see on the graph.
	var tables []string
	if proj != nil {
		ws := workspaceFromProject(cfg, proj, false)
		idx := buildTypeIndex(cfg, ws)
		dbm := buildDBModel(cfg, ws, idx)
		tables = dbm.sortedTableNames()
	}

	type tmpl struct {
		ID       string            `json:"id"`
		Analyzer string            `json:"analyzer"`
		Title    string            `json:"title"`
		Intent   string            `json:"intent"`
		Desc     string            `json:"desc"`
		Example  map[string]string `json:"example"`
		Note     string            `json:"note,omitempty"`
	}
	tablesCSV := strings.Join(tables, ",")
	tmpls := []tmpl{
		{ID: "sql-watch", Analyzer: "postgres-watch", Title: "Watch DB tables (live)", Intent: "observe",
			Desc: "Poll Postgres tables and stream new rows into a rolling window — the same tables you see on the graph.",
			Example: map[string]string{
				"connections":    `{"dev":"postgres://user:pass@localhost:5432/app?sslmode=disable"}`,
				"tables":         coalesce(tablesCSV, "ledger_entries,orders"),
				"window_minutes": "10", "bootstrap": "latest", "poll_seconds": "30",
			},
			Note: coalesce(tablesPrefillNote(tables), "")},
		{ID: "table-graph", Analyzer: "service-graph", Title: "Service + table graph (cross-repo)", Intent: "understand",
			Desc:    "Whole-project graph: services, calls, dependency-inversion hops, and DB tables with reads/writes.",
			Example: map[string]string{"project": projName(proj)}},
		{ID: "sql-tables", Analyzer: "sql-tables", Title: "Tables touched by .sql files", Intent: "observe",
			Desc:    "List the tables referenced (FROM/JOIN/INTO/UPDATE) by SQL files under a source root.",
			Example: map[string]string{}},
		{ID: "entrypoints", Analyzer: "entrypoints", Title: "Entrypoints (routes, listeners)", Intent: "change",
			Desc:    "Where control enters the service — Spring mappings, listeners, schedules.",
			Example: map[string]string{"patterns": "http-mapping=@(Get|Post|Put|Delete)Mapping;listener=@KafkaListener"}},
		{ID: "side-effects", Analyzer: "side-effects", Title: "Side effects (IO/net/db)", Intent: "diagnose",
			Desc:    "What a source root actually touches — database, network, files, process.",
			Example: map[string]string{}},
	}
	writeJSON(w, 200, map[string]any{"templates": tmpls, "tables": tables})
}

func tablesPrefillNote(tables []string) string {
	if len(tables) == 0 {
		return "No tables discovered yet — add your app repo (with JPA entities or migrations) to the project first."
	}
	return "Prefilled with tables discovered in this project: " + strings.Join(tables, ", ")
}

func projName(p *ProjectConfig) string {
	if p == nil {
		return ""
	}
	return p.Name
}

// handleTables serves the Tables view: every discovered table in the active project
// with its entity/migration, and the writers/readers (call sites) so the UI can list
// "who writes here / who reads here" and offer a one-click trace per table.
func (s *uiServer) handleTables(w http.ResponseWriter, r *http.Request) {
	projName := strings.TrimSpace(r.URL.Query().Get("project"))
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	proj := activeProjectFor(cfg, projName)
	if proj == nil {
		writeJSON(w, 200, map[string]any{"tables": []any{}})
		return
	}
	ws := workspaceFromProject(cfg, proj, false)
	idx := buildTypeIndex(cfg, ws)
	g := buildTraceGraph(idx)
	dbm := buildDBModel(cfg, ws, idx)

	type site struct {
		Method string `json:"method"`
		Repo   string `json:"repo"`
		File   string `json:"file"`
		Line   int    `json:"line"`
		Code   string `json:"code"`
		Write  bool   `json:"write"`
	}
	type tableView struct {
		Name       string   `json:"name"`
		Entity     string   `json:"entity,omitempty"`
		Mapping    string   `json:"mapping,omitempty"`
		Migrations []string `json:"migrations,omitempty"`
		MigRepo    string   `json:"mig_repo,omitempty"`
		Writers    []site   `json:"writers"`
		Readers    []site   `json:"readers"`
	}
	var out []tableView
	for _, name := range dbm.sortedTableNames() {
		ti := dbm.Tables[name]
		tv := tableView{Name: name, Entity: ti.Entity, Mapping: ti.mapNote, Migrations: ti.Migrations, MigRepo: ti.MigRepo}
		for _, h := range g.tableAccessSites(dbm, name) {
			sv := site{Method: h.owner.label(), Repo: h.owner.typ.Repo, File: h.owner.typ.File, Line: h.line, Code: h.code, Write: h.write}
			if h.write {
				tv.Writers = append(tv.Writers, sv)
			} else {
				tv.Readers = append(tv.Readers, sv)
			}
		}
		out = append(out, tv)
	}
	writeJSON(w, 200, map[string]any{"tables": out})
}
