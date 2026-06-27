package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Cross-service graph: TS imports + Go routes + the TS->Go operation seam.

type sgService struct {
	Name string `json:"name"`
	Root string `json:"root"`
	Lang string `json:"lang"`
}

type sgNode struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Service string `json:"service"`
	Lang    string `json:"lang"`
	Kind    string `json:"kind"` // file | entrypoint | router
	File    string `json:"file"` // source_root-relative path
	Line    int    `json:"line,omitempty"`
	Op      string `json:"op,omitempty"`     // operation id for entrypoints
	Method  string `json:"method,omitempty"` // handler/func name for drill-in
	// Effects are the distinct side-effect kinds (io-read/io-write/network/db/process)
	// this file performs, so the graph can highlight what each node actually touches.
	Effects []string `json:"effects,omitempty"`
}

type sgEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"` // import | registers | api-call
	Label string `json:"label,omitempty"`
	Cross bool   `json:"cross,omitempty"` // crosses a service boundary
}

type sgGraph struct {
	Services []sgService `json:"services"`
	Nodes    []sgNode    `json:"nodes"`
	Edges    []sgEdge    `json:"edges"`
}

// AnalyzeServiceGraph builds the whole-service graph from params:
//
//	services : JSON [{name,root,lang}]   (lang = "ts" | "go")
//	packages : JSON {"@scope/pkg":"root/relative/to/source_root"}  (workspace pkgs)
//
// source_root is the repo that contains every service root.
func AnalyzeServiceGraph(cfg Config, lens LensConfig) (Projection, error) {
	var services []sgService
	if err := json.Unmarshal([]byte(coalesce(lens.Params["services"], "[]")), &services); err != nil {
		return Projection{}, fmt.Errorf("service-graph: bad services param: %w", err)
	}
	// Cross-repo (CROSS-REPO-UX.md §C): with no explicit services param but an active
	// config project, derive the graph from the project's repos. A Java project builds
	// a class-level cross-repo graph (types + call/DI edges) so controllers in a
	// library and their concrete overrides in the app appear as one graph.
	if len(services) == 0 {
		var proj *ProjectConfig
		if name := strings.TrimSpace(lens.Params["project"]); name != "" {
			proj = projectByName(cfg, name)
		} else {
			proj = activeProject(cfg)
		}
		if proj != nil {
			appOnly := lens.Params["include_libraries"] == "false" || lens.Params["include_libraries"] == "0"
			return serviceGraphFromProject(cfg, proj, appOnly)
		}
	}
	if len(services) == 0 {
		return Projection{}, errors.New("service-graph: params.services is required (JSON [{name,root,lang}]) — or define a workspace project in config.json")
	}
	pkgMap := map[string]string{}
	if p := lens.Params["packages"]; p != "" {
		if err := json.Unmarshal([]byte(p), &pkgMap); err != nil {
			return Projection{}, fmt.Errorf("service-graph: bad packages param: %w", err)
		}
	}
	base := lens.SourceRoot
	if !filepath.IsAbs(base) {
		base = filepath.Join(cfg.Root, lens.SourceRoot)
	}

	g := sgGraph{Services: services}
	nodeByFile := map[string]string{} // source_root-relative file -> node id
	tsFilesByService := map[string][]string{}
	// Precompile side-effect markers per service language so each file node can be
	// tagged with what it touches (io/network/db/process) — first-class in the graph.
	seByLang := map[string][]compiledMarker{}
	for _, svc := range services {
		lid := sgLangID(svc.Lang)
		if _, done := seByLang[lid]; done {
			continue
		}
		if l := languageByID(lid); l != nil {
			var cms []compiledMarker
			for _, m := range l.SideEffects {
				if re, err := regexp.Compile(m.Regex); err == nil {
					cms = append(cms, compiledMarker{kind: m.Kind, label: m.Label, re: re})
				}
			}
			seByLang[lid] = cms
		}
	}
	addNode := func(n sgNode) {
		if _, ok := nodeByFile[n.File]; ok {
			return
		}
		nodeByFile[n.File] = n.ID
		g.Nodes = append(g.Nodes, n)
	}

	// Pass 1: enumerate file nodes per service.
	for _, svc := range services {
		root := filepath.Join(base, svc.Root)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if shouldSkipDir(cfg, p, d) || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			isTS := svc.Lang == "ts" && (ext == ".ts" || ext == ".tsx")
			isGo := svc.Lang == "go" && ext == ".go"
			if !isTS && !isGo {
				return nil
			}
			if strings.HasSuffix(p, ".d.ts") || strings.HasSuffix(p, "_test.go") || sgIsTestTS(p) {
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			rel = filepath.ToSlash(rel)
			id := svc.Name + "::" + rel
			effects := fileEffectKinds(p, seByLang[sgLangID(svc.Lang)])
			addNode(sgNode{ID: id, Label: trimServiceLabel(svc, rel), Service: svc.Name, Lang: svc.Lang, Kind: "file", File: rel, Effects: effects})
			if isTS {
				tsFilesByService[svc.Name] = append(tsFilesByService[svc.Name], rel)
			}
			return nil
		})
	}

	// Pass 2: edges.
	routeHandlers := map[string]sgNode{} // op -> handler node
	for _, svc := range services {
		root := filepath.Join(base, svc.Root)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if err == nil && d.IsDir() && (shouldSkipDir(cfg, p, d) || d.Name() == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			rel = filepath.ToSlash(rel)
			fromID, known := nodeByFile[rel]
			if !known {
				return nil
			}
			content, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			text := string(content)
			if svc.Lang == "ts" {
				for _, m := range tsImportRE.FindAllStringSubmatch(text, -1) {
					spec := firstNonEmpty(m[1], m[2], m[3])
					if spec == "" {
						continue
					}
					if toID, cross, ok := resolveTSImport(svc, rel, spec, nodeByFile, pkgMap, services); ok {
						g.Edges = append(g.Edges, sgEdge{From: fromID, To: toID, Kind: "import", Cross: cross})
					}
				}
			} else { // go: find route registrations + handler method definitions
				for _, m := range goAddRoutRE.FindAllStringSubmatch(text, -1) {
					op, handler := m[1], m[2]
					if hNode, ok := findGoHandler(cfg, base, svc, handler, nodeByFile); ok {
						for i := range g.Nodes {
							if g.Nodes[i].ID == hNode.ID {
								g.Nodes[i].Kind = "entrypoint"
								g.Nodes[i].Op = appendCSV(g.Nodes[i].Op, op)                  // a route file can host several operations
								g.Nodes[i].Method = firstNonEmpty(g.Nodes[i].Method, handler) // first handler is the drill-in target
								if g.Nodes[i].Line == 0 {
									g.Nodes[i].Line = hNode.Line
								}
							}
						}
						routeHandlers[op] = hNode
						g.Nodes[indexOfNode(g.Nodes, fromID)].Kind = "router"
						g.Edges = append(g.Edges, sgEdge{From: fromID, To: hNode.ID, Kind: "registers", Label: op})
					}
				}
			}
			return nil
		})
	}

	// Pass 3: the TS→Go seam. A TS file that names a Go operation id is an api caller.
	for op, hNode := range routeHandlers {
		opRE := regexp.MustCompile(`\b` + regexp.QuoteMeta(op) + `\b`)
		for svcName, files := range tsFilesByService {
			for _, rel := range files {
				content, err := os.ReadFile(filepath.Join(base, rel))
				if err != nil {
					continue
				}
				if opRE.Match(content) {
					g.Edges = append(g.Edges, sgEdge{From: svcName + "::" + rel, To: hNode.ID, Kind: "api-call", Label: op, Cross: true})
				}
			}
		}
	}

	// Emit: a summary block + the graph as a JSON fact for the UI/mermaid.
	cross := 0
	for _, e := range g.Edges {
		if e.Cross {
			cross++
		}
	}
	body := []string{
		fmt.Sprintf("service-graph: %d services, %d files, %d edges (%d cross-service)", len(services), len(g.Nodes), len(g.Edges), cross),
	}
	for _, svc := range services {
		n := 0
		for _, nd := range g.Nodes {
			if nd.Service == svc.Name {
				n++
			}
		}
		body = append(body, fmt.Sprintf("  %-12s %-4s %s  (%d files)", svc.Name, svc.Lang, svc.Root, n))
	}
	gj, _ := json.Marshal(g)
	p := Projection{Sync: "view-only"}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "service-graph", Tool: "service-graph", Mode: "graph", Lines: body})
	p.Facts = append(p.Facts, ProjectionFact{ID: "graph", Tool: "service-graph", Text: string(gj)})
	return p, nil
}

func sgIsTestTS(path string) bool {
	b := filepath.Base(path)
	return strings.HasSuffix(b, ".test.ts") || strings.HasSuffix(b, ".test.tsx") ||
		strings.HasSuffix(b, ".spec.ts") || strings.HasSuffix(b, ".spec.tsx")
}

// serviceGraphFromProject builds a cross-repo, class-level graph from a config
// project (CROSS-REPO-UX.md §C). Each repo is a "service"; each Java type is a node
// (entrypoint when it declares a Spring-mapped method); resolved call edges (incl.
// dependency-inversion override hops) become edges, cross=true when they span repos.
// This reuses the trace engine's type index + dispatch so the service graph and the
// trace agree on what calls what across the boundary.
func serviceGraphFromProject(cfg Config, proj *ProjectConfig, appOnly bool) (Projection, error) {
	ws := workspaceFromProject(cfg, proj, appOnly)
	if len(ws.Repos) == 0 {
		return Projection{}, fmt.Errorf("service-graph: project %q has no repos", proj.Name)
	}
	idx := buildTypeIndex(cfg, ws)
	tg := buildTraceGraph(idx)

	g := sgGraph{}
	repoLang := map[string]string{}
	for _, r := range ws.Repos {
		g.Services = append(g.Services, sgService{Name: r.Name, Root: r.Path, Lang: "java"})
		repoLang[r.Name] = "java"
	}

	// Node per type. A type is an entrypoint if any method carries a Spring mapping.
	nodeID := func(t *JavaType) string { return t.Repo + "::" + t.Name }
	added := map[string]bool{}
	effOf := func(t *JavaType) []string {
		seen := map[string]bool{}
		var out []string
		for _, m := range t.Methods {
			for _, k := range methodEffectKinds(m) {
				if !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
		}
		return out
	}
	for _, t := range idx.all {
		id := nodeID(t)
		if added[id] {
			continue
		}
		added[id] = true
		kind := "file"
		op, method := "", ""
		for _, m := range t.Methods {
			if ann := entryAnnotation(m); ann != "" {
				kind = "entrypoint"
				op = appendCSV(op, t.Name+"."+m.Name)
				if method == "" {
					method = m.Name
				}
			}
		}
		g.Nodes = append(g.Nodes, sgNode{
			ID: id, Label: t.Name, Service: t.Repo, Lang: "java", Kind: kind,
			File: t.File, Line: t.Line, Op: op, Method: method, Effects: effOf(t),
		})
	}

	// Edges from resolved call sites (deduped at the type level). DI/cross hops are
	// exactly what the trace surfaces; here they become graph edges.
	seen := map[string]bool{}
	for _, caller := range tg.methods {
		for _, e := range tg.callSitesOf(caller) {
			from, to := nodeID(e.caller.typ), nodeID(e.callee.typ)
			if from == to {
				continue
			}
			key := from + ">" + to
			if seen[key] {
				continue
			}
			seen[key] = true
			label := ""
			if e.di {
				label = "DI→" + e.callee.typ.Name
			}
			g.Edges = append(g.Edges, sgEdge{From: from, To: to, Kind: "api-call", Label: label, Cross: e.caller.typ.Repo != e.callee.typ.Repo})
		}
	}

	// Tables as first-class nodes (TABLES.md §B): each discovered table is a node, and
	// each repository type gets writes-to / reads-from edges to the table it manages.
	// Cross=true when the repo and the table's owning migration live in different repos.
	dbm := buildDBModel(cfg, ws, idx)
	tableNodeID := func(tbl string) string { return "table::" + tbl }
	for _, tbl := range dbm.sortedTableNames() {
		ti := dbm.Tables[tbl]
		svc := ti.EntityRepo
		if svc == "" {
			svc = ti.MigRepo
		}
		if svc == "" && len(g.Services) > 0 {
			svc = g.Services[0].Name
		}
		file := ""
		if len(ti.Migrations) > 0 {
			file = ti.Migrations[0]
		}
		g.Nodes = append(g.Nodes, sgNode{
			ID: tableNodeID(tbl), Label: "▱ " + tbl, Service: svc, Lang: "sql", Kind: "table",
			File: file, Effects: []string{"db"}, Method: tbl,
		})
	}
	for _, rt := range dbm.RepoTables {
		from := nodeID(rt.RepoType)
		to := tableNodeID(rt.Table)
		owner := ""
		if ti := dbm.Tables[rt.Table]; ti != nil {
			owner = coalesce(ti.MigRepo, ti.EntityRepo)
		}
		cross := owner != "" && owner != rt.RepoType.Repo
		if rt.Writes {
			g.Edges = append(g.Edges, sgEdge{From: from, To: to, Kind: "writes-to", Label: "writes", Cross: cross})
		}
		if rt.Reads {
			g.Edges = append(g.Edges, sgEdge{From: from, To: to, Kind: "reads-from", Label: "reads", Cross: cross})
		}
	}

	cross := 0
	for _, e := range g.Edges {
		if e.Cross {
			cross++
		}
	}
	body := []string{fmt.Sprintf("service-graph (cross-repo): %d repos, %d types, %d edges (%d cross-repo)", len(g.Services), len(g.Nodes), len(g.Edges), cross)}
	for _, svc := range g.Services {
		n := 0
		for _, nd := range g.Nodes {
			if nd.Service == svc.Name {
				n++
			}
		}
		body = append(body, fmt.Sprintf("  %-16s java  (%d types)", svc.Name, n))
	}
	gj, _ := json.Marshal(g)
	p := Projection{Sync: "view-only"}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "service-graph", Tool: "service-graph", Mode: "graph", Lines: body})
	p.Facts = append(p.Facts, ProjectionFact{ID: "graph", Tool: "service-graph", Text: string(gj)})
	return p, nil
}

// appendCSV appends item to a comma-separated list, de-duplicating.
func appendCSV(csv, item string) string {
	if csv == "" {
		return item
	}
	for _, p := range strings.Split(csv, ", ") {
		if p == item {
			return csv
		}
	}
	return csv + ", " + item
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func indexOfNode(nodes []sgNode, id string) int {
	for i := range nodes {
		if nodes[i].ID == id {
			return i
		}
	}
	return 0
}

func trimServiceLabel(svc sgService, rel string) string {
	lbl := strings.TrimPrefix(rel, svc.Root+"/")
	return lbl
}

// resolveTSImport maps a TS import specifier to a known file node. Returns the
// node id, whether the edge crosses a service boundary, and ok.
func resolveTSImport(from sgService, fromRel, spec string, nodeByFile map[string]string, pkgMap map[string]string, services []sgService) (string, bool, bool) {
	// workspace package (e.g. "@myorg/shared" or a subpath import)
	for pkg, root := range pkgMap {
		if spec == pkg || strings.HasPrefix(spec, pkg+"/") {
			// resolve to that package's index, else any file under its root
			for _, cand := range []string{root + "/index.ts", root + "/index.tsx"} {
				if id, ok := nodeByFile[cand]; ok {
					return id, true, true
				}
			}
			// fall back to the first node under root
			for file, id := range nodeByFile {
				if strings.HasPrefix(file, root+"/") {
					svc := serviceOf(file, services)
					return id, svc != from.Name, true
				}
			}
			return "", false, false
		}
	}
	if !strings.HasPrefix(spec, ".") {
		return "", false, false // external npm dep — skip
	}
	dir := filepath.ToSlash(filepath.Dir(fromRel))
	target := filepath.ToSlash(filepath.Join(dir, spec))
	cands := []string{target, target + ".ts", target + ".tsx", target + "/index.ts", target + "/index.tsx"}
	// the gen client imports often end in .ts already; also try stripping a trailing .js
	if strings.HasSuffix(target, ".js") {
		cands = append(cands, strings.TrimSuffix(target, ".js")+".ts")
	}
	for _, c := range cands {
		if id, ok := nodeByFile[c]; ok {
			cross := serviceOf(c, services) != from.Name
			return id, cross, true
		}
	}
	return "", false, false
}

func serviceOf(file string, services []sgService) string {
	best := ""
	for _, s := range services {
		if strings.HasPrefix(file, s.Root+"/") && len(s.Root) > len(best) {
			best = s.Root
			// keep scanning for the longest matching root
		}
	}
	for _, s := range services {
		if s.Root == best {
			return s.Name
		}
	}
	return ""
}

// findGoHandler locates the file+line defining a Go handler method (e.g. a method
// on HandlerDeps) so the route registration can point at real source.
func findGoHandler(cfg Config, base string, svc sgService, handler string, nodeByFile map[string]string) (sgNode, bool) {
	defRE := regexp.MustCompile(`^\s*func\s*\([^)]*\)\s*` + regexp.QuoteMeta(handler) + `\s*\(`)
	root := filepath.Join(base, svc.Root)
	var found sgNode
	ok := false
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || ok || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		lines, err := readLines(p)
		if err != nil {
			return nil
		}
		for i, l := range lines {
			if defRE.MatchString(l) {
				rel, _ := filepath.Rel(base, p)
				rel = filepath.ToSlash(rel)
				if id, known := nodeByFile[rel]; known {
					found = sgNode{ID: id, File: rel, Line: i + 1, Service: svc.Name, Lang: "go"}
					ok = true
				}
				return nil
			}
		}
		return nil
	})
	return found, ok
}

// sgLangID maps a service-graph lang ("ts"/"go") to a Language registry id.
func sgLangID(lang string) string {
	if lang == "ts" {
		return "js"
	}
	return lang
}

// fileEffectKinds returns the distinct side-effect kinds a file performs, using the
// precompiled language markers — so service-graph nodes know what they touch.
// javaSEMarkers lazily compiles the Java side-effect markers once for method-level
// effect tagging in the cross-repo graph.
var javaSEMarkers = func() []compiledMarker {
	var out []compiledMarker
	if l := languageByID("java"); l != nil {
		for _, m := range l.SideEffects {
			if re, err := regexp.Compile(m.Regex); err == nil {
				out = append(out, compiledMarker{kind: m.Kind, label: m.Label, re: re})
			}
		}
	}
	return out
}()

// methodEffectKinds returns the distinct side-effect kinds a Java method's body
// performs (io/network/db/process), for tagging type nodes in the cross-repo graph.
func methodEffectKinds(m JavaMethod) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range m.Lines {
		code := stripLineComment(raw)
		if strings.TrimSpace(code) == "" {
			continue
		}
		for _, cm := range javaSEMarkers {
			if !seen[cm.kind] && cm.re.MatchString(code) {
				seen[cm.kind] = true
				out = append(out, cm.kind)
			}
		}
	}
	sort.Strings(out)
	return out
}

func fileEffectKinds(path string, markers []compiledMarker) []string {
	if len(markers) == 0 {
		return nil
	}
	lines, err := readLines(path)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range lines {
		code := stripLineComment(raw)
		if strings.TrimSpace(code) == "" {
			continue
		}
		for _, m := range markers {
			if !seen[m.kind] && m.re.MatchString(code) {
				seen[m.kind] = true
				out = append(out, m.kind)
			}
		}
	}
	sort.Strings(out)
	return out
}
