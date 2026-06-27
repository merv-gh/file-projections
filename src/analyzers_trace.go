package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Cross-repo, DI-aware trace-to-line lens (CROSS-REPO.md §D) — the headline of the
// cross-repo phase. It answers "how do we end up at this line of code?" with
// MULTIPLE answers: every distinct control path from a (possibly cross-repo)
// entrypoint to the target line, each rendered separately with its own guard
// assumptions, loop markers, dependency-inversion hops, and repo-boundary crossings.
//
// This is the piece that makes the project's original purpose work: a Spring
// controller in an internal library calls an abstract service; the concrete override
// lives in the app repo. Resolving that hop across the repo boundary (via the
// workspace type index, javatypes.go) is what produces a seamless path no single-repo
// view can show.
//
// Pure-Go and scope-resolved (type-name + method-name): generics/overloads/proxies
// are out of scope and reported as ambiguity. joern stays the precise upgrade.

// springEntryAnnRE matches the Spring annotations that mark an entrypoint method.
var springEntryAnnRE = regexp.MustCompile(`@(RestController|Controller|(?:Get|Post|Put|Delete|Patch|Request)Mapping|KafkaListener|Scheduled|EventListener|MessageMapping|RabbitListener|SqsListener)\b`)

// tMethod is a workspace method node: its declaring type + the parsed method.
type tMethod struct {
	typ    *JavaType
	method JavaMethod
	entry  string // entrypoint annotation if this method is an entrypoint, else ""
}

func (m *tMethod) key() string {
	return m.typ.Repo + "::" + m.typ.Name + "." + m.method.Name + "#" + strconv.Itoa(m.method.Start)
}
func (m *tMethod) label() string { return m.typ.Name + "." + m.method.Name }

// tEdge is a resolved call edge: caller calls callee at callLine. di marks a hop that
// crossed a dependency-inversion boundary (abstract/interface -> concrete override).
type tEdge struct {
	caller     *tMethod
	callee     *tMethod
	callLine   int
	callCode   string
	di         bool
	calleeType string // statically-declared receiver type at the call site
}

// traceGraph is the workspace-wide method call graph with dispatch resolution.
type traceGraph struct {
	idx      *TypeIndex
	methods  []*tMethod
	byType   map[string][]*tMethod // simple type name -> its methods
	incoming map[string][]tEdge    // callee key -> edges into it
}

// buildTraceGraph assembles every Java method across the workspace into a call graph,
// resolving each call site's receiver type and DI dispatch via the type index.
func buildTraceGraph(idx *TypeIndex) *traceGraph {
	g := &traceGraph{idx: idx, byType: map[string][]*tMethod{}, incoming: map[string][]tEdge{}}
	for _, t := range idx.all {
		for _, m := range t.Methods {
			tm := &tMethod{typ: t, method: m, entry: entryAnnotation(m)}
			g.methods = append(g.methods, tm)
			g.byType[t.Name] = append(g.byType[t.Name], tm)
		}
	}
	for _, caller := range g.methods {
		for _, e := range g.callSitesOf(caller) {
			g.incoming[e.callee.key()] = append(g.incoming[e.callee.key()], e)
		}
	}
	return g
}

// entryAnnotation returns the Spring entrypoint annotation on a method, or "".
func entryAnnotation(m JavaMethod) string {
	for _, a := range m.Annotations {
		if sm := springEntryAnnRE.FindStringSubmatch(a); sm != nil {
			return "@" + sm[1]
		}
	}
	return ""
}

var (
	tQualCallRE = regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*\.\s*([A-Za-z_$][\w$]*)\s*\(`)
	tBareCallRE = regexp.MustCompile(`(?:^|[^.\w])([a-z_$][\w$]*)\s*\(`)
	tLocalVarRE = regexp.MustCompile(`\b([A-Z][A-Za-z0-9_]*)\s+([a-z_$][\w$]*)\s*=`)
)

// callSitesOf resolves every call edge originating in a method body.
func (g *traceGraph) callSitesOf(caller *tMethod) []tEdge {
	body := caller.method.Lines
	// Map local variable names to their declared types within this method.
	locals := map[string]string{}
	for _, l := range body {
		for _, m := range tLocalVarRE.FindAllStringSubmatch(stripCallNoise(l), -1) {
			locals[m[2]] = m[1]
		}
	}
	var edges []tEdge
	seen := map[string]bool{}
	for li, raw := range body {
		line := stripCallNoise(raw)
		callLine := caller.method.Start + li
		// Qualified calls: receiver.method(...)
		for _, m := range tQualCallRE.FindAllStringSubmatch(line, -1) {
			recv, meth := m[1], m[2]
			recvType := g.resolveReceiver(caller, recv, locals)
			edges = appendDispatch(g, edges, seen, caller, recvType, meth, callLine, raw)
		}
		// Bare calls: method(...)  -> implicit this (the enclosing type).
		for _, m := range tBareCallRE.FindAllStringSubmatch(line, -1) {
			meth := m[1]
			if callKeywords[meth] {
				continue
			}
			edges = appendDispatch(g, edges, seen, caller, caller.typ.Name, meth, callLine, raw)
		}
	}
	return edges
}

// resolveReceiver maps a receiver expression to a static type name: a local var, a
// field of the enclosing type, "this"/enclosing, else "".
func (g *traceGraph) resolveReceiver(caller *tMethod, recv string, locals map[string]string) string {
	if recv == "this" {
		return caller.typ.Name
	}
	if t, ok := locals[recv]; ok {
		return t
	}
	if ft := caller.typ.fieldType(recv); ft != "" {
		return ft
	}
	// constructor-injected fields sometimes resolve only by name == lowercased type.
	if t := g.idx.findType(strings.Title(recv)); t != nil {
		return t.Name
	}
	return ""
}

// appendDispatch resolves a (receiverType, method) call to its concrete target
// method(s) and appends an edge per target. A target reached through an abstract /
// interface receiver is flagged di (dependency inversion).
func appendDispatch(g *traceGraph, edges []tEdge, seen map[string]bool, caller *tMethod, recvType, meth string, callLine int, code string) []tEdge {
	if recvType == "" {
		return edges
	}
	targets, di := g.dispatch(recvType, meth)
	for _, t := range targets {
		if t == caller {
			continue
		}
		k := caller.key() + ">" + t.key() + "#" + strconv.Itoa(callLine)
		if seen[k] {
			continue
		}
		seen[k] = true
		edges = append(edges, tEdge{caller: caller, callee: t, callLine: callLine, callCode: strings.TrimSpace(code), di: di, calleeType: recvType})
	}
	return edges
}

// dispatch resolves a static (type, method) to runtime target method nodes. Returns
// di=true when resolution crossed an abstraction (the declared receiver type is
// abstract/interface but the body lives on a concrete subtype override).
func (g *traceGraph) dispatch(typeName, method string) ([]*tMethod, bool) {
	declared := g.idx.findType(typeName)
	overrides := g.idx.ConcreteOverrides(typeName, method)
	if len(overrides) > 0 {
		// di if the declared receiver type does not itself provide the concrete body.
		di := declared == nil || declared.Abstract || !declared.declares(method)
		return g.methodsFor(overrides, method), di
	}
	// No concrete override below: the body lives on the declared type (template
	// method on an abstract base, or a plain concrete method).
	if declared != nil && declared.declares(method) {
		return g.methodsFor([]*JavaType{declared}, method), false
	}
	return nil, false
}

// methodsFor returns the tMethod nodes for a method name on the given types.
func (g *traceGraph) methodsFor(types []*JavaType, method string) []*tMethod {
	var out []*tMethod
	for _, t := range types {
		for _, m := range g.byType[t.Name] {
			if m.method.Name == method {
				out = append(out, m)
			}
		}
	}
	return out
}

// interestingLine picks the most useful line to aim a symbol trace at: the first
// side-effect (io/network/db/process) line in the body, else the first call line,
// else the method signature.
func interestingLine(m *tMethod) int {
	for li, raw := range m.method.Lines {
		code := stripCallNoise(raw)
		if strings.TrimSpace(code) == "" {
			continue
		}
		for _, cm := range javaSEMarkers {
			if cm.re.MatchString(code) {
				return m.method.Start + li
			}
		}
	}
	for li, raw := range m.method.Lines {
		code := stripCallNoise(raw)
		if tQualCallRE.MatchString(code) && li > 0 {
			return m.method.Start + li
		}
	}
	return m.method.Start
}

// tracePath is one answer: an ordered chain of edges from an entrypoint to the target
// method, plus whether it actually starts at an entrypoint.
type tracePath struct {
	edges    []tEdge // entrypoint-first order
	entry    *tMethod
	hasEntry bool
}

// TraceToLine is the analyzer: build the workspace graph, locate the target method,
// reverse-search to entrypoints, and emit one projection per distinct path. The
// target is located either by a symbol name (preferred — params.symbol) or by an
// explicit file:line (deep-link / back-compat).
func TraceToLine(cfg Config, lens LensConfig, ws *Workspace) (Projection, error) {
	repo := strings.TrimSpace(lens.Params["repo"])
	file := strings.TrimSpace(lens.Params["file"])
	symbol := strings.TrimSpace(lens.Params["symbol"])
	line := atoi(lens.Params["line"])
	if symbol == "" && (file == "" || line <= 0) {
		return Projection{}, fmt.Errorf("trace-to-line: params.symbol (or file+line) is required")
	}
	maxPaths := atoi(lens.Params["max_paths"])
	if maxPaths <= 0 {
		maxPaths = 8
	}
	if len(ws.Repos) == 0 {
		return Projection{}, fmt.Errorf("trace-to-line: no repos in scope — add a project (folder or clone) first")
	}

	idx := buildTypeIndex(cfg, ws)
	g := buildTraceGraph(idx)

	// Table-as-target (TABLES.md §C): if the symbol is a known physical table, the
	// targets are the call sites that write/read it via a Spring Data repository.
	dbm := buildDBModel(cfg, ws, idx)
	if symbol != "" {
		if _, isTable := dbm.Tables[normalizeTableName(symbol)]; isTable {
			return traceToTable(cfg, lens, g, dbm, normalizeTableName(symbol), maxPaths)
		}
	}

	var target *tMethod
	if symbol != "" {
		cands := g.methodsBySymbol(symbol)
		if len(cands) == 0 {
			return Projection{}, fmt.Errorf("trace-to-line: no method/type named %q in the selected project", symbol)
		}
		target = cands[0]
		// Symbol mode: aim at the most interesting line in the target body — the
		// first side-effect/call line — else the signature.
		line = interestingLine(target)
	} else {
		target = g.methodAt(repo, file, line)
	}
	if target == nil {
		return Projection{}, fmt.Errorf("trace-to-line: could not locate %q", coalesce(symbol, fmt.Sprintf("%s:%d", file, line)))
	}

	paths := g.tracePaths(target, maxPaths)
	p := Projection{Sync: "view-only"}

	// Summary block: the target + how many answers.
	var summary []string
	summary = append(summary, fmt.Sprintf("// target: %s  (%s)", codeAtLine(target, line), repoRelPath(target.typ.Repo, target.typ.File)))
	summary = append(summary, fmt.Sprintf("// %d path(s) from an entrypoint reach this line", len(paths)))
	for i := range paths {
		summary = append(summary, fmt.Sprintf("//   answer %d: %s", i+1, pathHeadline(paths[i])))
	}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "trace", File: "model", Mode: "trace-to-line", Tool: "trace", Lines: summary})

	// One Extra projection per answer (multi-answer: separate control-flows).
	anyDI, anyCross := false, false
	for i, path := range paths {
		lines, di, cross := renderTracePath(g, path, target, line)
		anyDI = anyDI || di
		anyCross = anyCross || cross
		ep := Projection{Sync: "view-only"}
		ep.Blocks = append(ep.Blocks, ProjectionBlock{
			ID: fmt.Sprintf("answer-%d", i+1), File: "model", Mode: "trace-path", Tool: "trace", Lines: lines,
		})
		conf := "structural"
		note := pathHeadline(path)
		if di {
			conf = "structural (di)"
		}
		ep.Facts = append(ep.Facts, confidenceFact(conf, note))
		out := LensOut(cfg, lens)
		stem := strings.TrimSuffix(out, ".projection")
		p.Extra = append(p.Extra, ExtraFile{Path: fmt.Sprintf("%s.answer-%d.projection", stem, i+1), Proj: ep})
	}

	conf := "structural"
	noteBits := []string{fmt.Sprintf("%d path(s); scope-resolved by type+method name", len(paths))}
	if anyDI {
		conf = "structural (di)"
		noteBits = append(noteBits, "includes dependency-inversion hops (abstract→concrete override)")
	}
	if anyCross {
		noteBits = append(noteBits, "crosses repo boundaries")
	}
	p.Facts = append(p.Facts, confidenceFact(conf, strings.Join(noteBits, "; ")))
	if len(paths) == 0 {
		p.Facts = append(p.Facts, ProjectionFact{ID: "hint", Tool: "trace", Text: "no entrypoint path found — the method may be an entrypoint itself, dead, or only reached via reflection/proxy"})
	}
	return p, nil
}

// tableAccessSite is one place a repository call reads/writes a table, with the
// containing method. Shared by the table trace and the /api/tables view.
type tableAccessSite struct {
	owner *tMethod
	line  int
	code  string
	write bool
}

// tableAccessSites finds every call site that reads/writes the given table through a
// Spring Data repository field of one of the table's managing repository types.
func (g *traceGraph) tableAccessSites(dbm *DBModel, table string) []tableAccessSite {
	repoTypes := map[string]bool{}
	for _, rt := range dbm.RepoTables {
		if rt.Table == table {
			repoTypes[rt.RepoType.Name] = true
		}
	}
	var hits []tableAccessSite
	for _, m := range g.methods {
		locals := map[string]string{}
		for _, l := range m.method.Lines {
			for _, mm := range tLocalVarRE.FindAllStringSubmatch(stripCallNoise(l), -1) {
				locals[mm[2]] = mm[1]
			}
		}
		for li, raw := range m.method.Lines {
			code := stripCallNoise(raw)
			for _, cm := range tQualCallRE.FindAllStringSubmatch(code, -1) {
				recv, meth := cm[1], cm[2]
				rtype := g.resolveReceiver(m, recv, locals)
				if !repoTypes[rtype] {
					continue
				}
				w, r := repoAccessForCall(meth)
				if !w && !r {
					continue
				}
				hits = append(hits, tableAccessSite{owner: m, line: m.method.Start + li, code: strings.TrimSpace(raw), write: w})
			}
		}
	}
	return hits
}

// traceToTable answers "how do we end up writing to / reading from <table>?". It
// finds every method whose body calls a Spring Data repository for the table, traces
// each to its entrypoints (reusing the path engine), and appends a table terminal
// marker to each answer.
func traceToTable(cfg Config, lens LensConfig, g *traceGraph, dbm *DBModel, table string, maxPaths int) (Projection, error) {
	hits := g.tableAccessSites(dbm, table)

	p := Projection{Sync: "view-only"}
	var summary []string
	summary = append(summary, fmt.Sprintf("// target table: %s", table))
	if ti := dbm.Tables[table]; ti != nil {
		if ti.Entity != "" {
			summary = append(summary, fmt.Sprintf("// entity %s · mapping: %s", ti.Entity, ti.mapNote))
		}
		if len(ti.Migrations) > 0 {
			summary = append(summary, fmt.Sprintf("// migration: %s (%s)", ti.Migrations[0], ti.MigRepo))
		}
	}
	if len(hits) == 0 {
		summary = append(summary, "// no repository call sites write/read this table in scope")
		p.Blocks = append(p.Blocks, ProjectionBlock{ID: "trace", File: "model", Mode: "trace-to-table", Tool: "trace", Lines: summary})
		p.Facts = append(p.Facts, confidenceFact("structural", "table known from "+coalesce(dbm.Tables[table].mapNote, "schema")+", but no code accesses it in scope"))
		return p, nil
	}

	type answer struct {
		lines []string
		di    bool
		cross bool
		head  string
	}
	var answers []answer
	for _, h := range hits {
		// Trace from the call-site's owning method back to entrypoints.
		paths := g.tracePaths(h.owner, maxPaths)
		verb := "reads"
		if h.write {
			verb = "writes"
		}
		if len(paths) == 0 {
			// the owner itself is the only context (entrypoint or unreached)
			ls := []string{fmt.Sprintf("  %s   %s", h.owner.label(), repoRelPath(h.owner.typ.Repo, h.owner.typ.File))}
			ls = append(ls, codeLoc("  → "+strings.TrimSuffix(h.code, ";"), h.owner.typ.File, h.line))
			ls = append(ls, "★ "+verb+" "+table)
			answers = append(answers, answer{lines: ls, head: h.owner.label() + " → " + verb + " " + table})
			continue
		}
		for _, path := range paths {
			ls, di, cross := renderTracePath(g, path, h.owner, h.line)
			// renderTracePath already ends with the call-site line (the ★ target);
			// append only the table terminal so the path reads …save() → table.
			ls = append(ls, "      ↳ "+verb+" table "+table)
			answers = append(answers, answer{lines: ls, di: di, cross: cross, head: pathHeadline(path) + " → " + verb + " " + table})
		}
	}
	if len(answers) > maxPaths {
		answers = answers[:maxPaths]
	}

	summary = append(summary, fmt.Sprintf("// %d path(s) reach this table", len(answers)))
	for i, a := range answers {
		summary = append(summary, fmt.Sprintf("//   answer %d: %s", i+1, a.head))
	}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "trace", File: "model", Mode: "trace-to-table", Tool: "trace", Lines: summary})

	anyDI, anyCross := false, false
	for i, a := range answers {
		anyDI = anyDI || a.di
		anyCross = anyCross || a.cross
		ep := Projection{Sync: "view-only"}
		ep.Blocks = append(ep.Blocks, ProjectionBlock{ID: fmt.Sprintf("answer-%d", i+1), File: "model", Mode: "trace-path", Tool: "trace", Lines: a.lines})
		conf := "structural"
		if a.di {
			conf = "structural (di)"
		}
		ep.Facts = append(ep.Facts, confidenceFact(conf, a.head))
		stem := strings.TrimSuffix(LensOut(cfg, lens), ".projection")
		p.Extra = append(p.Extra, ExtraFile{Path: fmt.Sprintf("%s.answer-%d.projection", stem, i+1), Proj: ep})
	}
	conf := "structural"
	bits := []string{fmt.Sprintf("%d path(s) to table %s", len(answers), table)}
	if anyDI {
		conf = "structural (di)"
		bits = append(bits, "via dependency-inversion hops")
	}
	if anyCross {
		bits = append(bits, "crosses repo boundaries")
	}
	p.Facts = append(p.Facts, confidenceFact(conf, strings.Join(bits, "; ")))
	return p, nil
}

// methodAt finds the method whose body span contains file:line in the given repo.
func (g *traceGraph) methodAt(repo, file string, line int) *tMethod {
	file = normRel(file)
	var best *tMethod
	for _, m := range g.methods {
		if repo != "" && m.typ.Repo != repo {
			continue
		}
		if normRel(m.typ.File) != file && !strings.HasSuffix(normRel(m.typ.File), file) {
			continue
		}
		if line >= m.method.Start && line <= m.method.End {
			if best == nil || m.method.Start > best.method.Start {
				best = m // innermost
			}
		}
	}
	return best
}

// methodsBySymbol resolves a user-typed symbol (a method name "pay", a qualified
// "RealPaymentService.pay", or a type name "RealPaymentService") to candidate target
// methods, ordered with non-entrypoint methods first (you usually trace TO a worker,
// not an entrypoint). For a bare type name, its declared methods are candidates.
func (g *traceGraph) methodsBySymbol(symbol string) []*tMethod {
	symbol = strings.TrimSpace(symbol)
	typeName, methName := "", symbol
	if i := strings.LastIndex(symbol, "."); i >= 0 {
		typeName, methName = symbol[:i], symbol[i+1:]
	}
	typeName = simpleTypeName(typeName)
	var out []*tMethod
	for _, m := range g.methods {
		if typeName != "" && m.typ.Name != typeName {
			continue
		}
		if m.method.Name == methName {
			out = append(out, m)
		}
	}
	// Bare type name: offer the type's own methods (e.g. trace "Ledger" -> write()).
	if len(out) == 0 {
		if t := g.idx.findType(simpleTypeName(symbol)); t != nil {
			for _, m := range g.byType[t.Name] {
				out = append(out, m)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ei, ej := out[i].entry != "", out[j].entry != ""
		if ei != ej {
			return ej // non-entry first
		}
		return out[i].typ.Name < out[j].typ.Name
	})
	return out
}

// tracePaths reverse-BFS from target to entrypoints, returning distinct paths
// (entrypoint-first). Paths that reach an entrypoint are preferred; if none exist,
// the longest partial chains are returned so the user still sees the call context.
func (g *traceGraph) tracePaths(target *tMethod, maxPaths int) []tracePath {
	type state struct {
		node  *tMethod
		chain []tEdge // callee-first; reversed at emit
		seen  map[string]bool
	}
	var complete, partial []tracePath
	start := state{node: target, seen: map[string]bool{target.key(): true}}
	queue := []state{start}
	guard := 0
	for len(queue) > 0 && len(complete) < maxPaths && guard < 5000 {
		guard++
		st := queue[0]
		queue = queue[1:]
		if st.node.entry != "" && len(st.chain) > 0 {
			complete = append(complete, makePath(st.chain, st.node))
			continue
		}
		ins := g.incoming[st.node.key()]
		if len(ins) == 0 {
			if len(st.chain) > 0 {
				partial = append(partial, makePath(st.chain, st.node))
			}
			continue
		}
		for _, e := range ins {
			if st.seen[e.caller.key()] {
				continue // cycle guard
			}
			ns := state{node: e.caller, chain: append(append([]tEdge{}, st.chain...), e), seen: cloneSet(st.seen)}
			ns.seen[e.caller.key()] = true
			queue = append(queue, ns)
		}
	}
	if len(complete) > 0 {
		sort.SliceStable(complete, func(i, j int) bool { return len(complete[i].edges) < len(complete[j].edges) })
		return complete
	}
	// fall back to the most informative partial chains
	sort.SliceStable(partial, func(i, j int) bool { return len(partial[i].edges) > len(partial[j].edges) })
	if len(partial) > maxPaths {
		partial = partial[:maxPaths]
	}
	return partial
}

// makePath reverses a callee-first chain into entrypoint-first order.
func makePath(chain []tEdge, head *tMethod) tracePath {
	rev := make([]tEdge, len(chain))
	for i := range chain {
		rev[len(chain)-1-i] = chain[i]
	}
	tp := tracePath{edges: rev, entry: head}
	tp.hasEntry = head.entry != ""
	return tp
}

// renderTracePath produces the readable lines for one answer and reports whether it
// contained a DI hop / crossed repos.
func renderTracePath(g *traceGraph, path tracePath, target *tMethod, targetLine int) (lines []string, di, cross bool) {
	if path.entry != nil {
		ann := path.entry.entry
		if ann == "" {
			ann = "(no entrypoint annotation — call context only)"
		}
		lines = append(lines, fmt.Sprintf("[entry] %s   %s   @%s", path.entry.label(), repoRelPath(path.entry.typ.Repo, path.entry.typ.File), strings.TrimPrefix(ann, "@")))
	}
	prevRepo := ""
	if path.entry != nil {
		prevRepo = path.entry.typ.Repo
	}
	for _, e := range path.edges {
		// guards active at the call site within the caller method
		for _, gd := range guardsAt(e.caller.method, e.callLine) {
			lines = append(lines, "    assume: "+gd)
		}
		arrow := "  → " + strings.TrimSuffix(e.callCode, ";")
		lines = append(lines, codeLoc(arrow, e.caller.typ.File, e.callLine))
		if e.di {
			di = true
			lines = append(lines, fmt.Sprintf("      ↳ DI: %s.%s is abstract → dispatched to %s (%s)",
				e.calleeType, e.callee.method.Name, e.callee.label(), repoRelPath(e.callee.typ.Repo, e.callee.typ.File)))
		}
		if e.callee.typ.Repo != prevRepo && prevRepo != "" {
			cross = true
			lines = append(lines, fmt.Sprintf("    ═══ crosses repo boundary: %s → %s ═══", prevRepo, e.callee.typ.Repo))
		}
		prevRepo = e.callee.typ.Repo
		lines = append(lines, fmt.Sprintf("  %s   %s", e.callee.label(), repoRelPath(e.callee.typ.Repo, e.callee.typ.File)))
	}
	// the target line itself, with its in-method guards and loop markers
	for _, gd := range guardsAt(target.method, targetLine) {
		lines = append(lines, "    assume: "+gd)
	}
	if lp := loopAt(target.method, targetLine); lp != "" {
		lines = append(lines, "    loop: "+lp+" (reached 0..N times)")
	}
	lines = append(lines, codeLoc("★ "+codeAtLine(target, targetLine), target.typ.File, targetLine))
	return lines, di, cross
}

// guardsAt returns the branch/loop conditions active at targetLine within a method,
// using the same brace-depth guard-stack discipline as the lexical unroller.
func guardsAt(m JavaMethod, targetLine int) []string {
	type gframe struct {
		depth int
		cond  string
	}
	var stack []gframe
	braceDepth := 0
	for li, raw := range m.Lines {
		ln := m.Start + li
		trim := strings.TrimSpace(stripLineComment(raw))
		if trim == "" {
			continue
		}
		if ln == targetLine {
			var out []string
			for _, f := range stack {
				out = append(out, f.cond)
			}
			return out
		}
		opens := strings.Count(trim, "{")
		closes := strings.Count(trim, "}")
		if strings.HasPrefix(trim, "}") {
			for len(stack) > 0 && stack[len(stack)-1].depth >= braceDepth {
				stack = stack[:len(stack)-1]
			}
		}
		if cond, ok := javaGuardCond(trim); ok {
			stack = append(stack, gframe{depth: braceDepth + 1, cond: cond})
		}
		braceDepth += opens - closes
		if braceDepth < 0 {
			braceDepth = 0
		}
	}
	return nil
}

var (
	jIfRE    = regexp.MustCompile(`^\}?\s*(?:else\s+)?if\s*\((.*)\)\s*\{?\s*$`)
	jForRE   = regexp.MustCompile(`^\s*for\s*\((.*)\)\s*\{?\s*$`)
	jWhileRE = regexp.MustCompile(`^\s*while\s*\((.*)\)\s*\{?\s*$`)
)

// javaGuardCond reports the condition an if/else-if/for/while header introduces.
func javaGuardCond(trim string) (string, bool) {
	if m := jIfRE.FindStringSubmatch(trim); m != nil {
		return strings.TrimSpace(m[1]), true
	}
	if m := jForRE.FindStringSubmatch(trim); m != nil {
		return "for(" + strings.TrimSpace(m[1]) + ")", true
	}
	if m := jWhileRE.FindStringSubmatch(trim); m != nil {
		return "while(" + strings.TrimSpace(m[1]) + ")", true
	}
	return "", false
}

// loopAt returns a loop header description if targetLine is inside a for/while loop.
func loopAt(m JavaMethod, targetLine int) string {
	for _, g := range guardsAt(m, targetLine) {
		if strings.HasPrefix(g, "for(") || strings.HasPrefix(g, "while(") {
			return g
		}
	}
	return ""
}

func codeAtLine(m *tMethod, line int) string {
	i := line - m.method.Start
	if i >= 0 && i < len(m.method.Lines) {
		return strings.TrimSpace(m.method.Lines[i])
	}
	return m.label()
}

func pathHeadline(p tracePath) string {
	parts := []string{}
	if p.entry != nil {
		parts = append(parts, p.entry.label())
	}
	for _, e := range p.edges {
		seg := e.callee.label()
		if e.di {
			seg += "*"
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, " → ")
}

// AnalyzeTraceToLine is the registered analyzer entry: it builds the workspace from
// config (the active project, the single source of truth) or a param-injected one
// (tests / deep-links) and runs the trace.
func AnalyzeTraceToLine(cfg Config, lens LensConfig) (Projection, error) {
	ws, err := resolveTraceWorkspace(cfg, lens)
	if err != nil {
		return Projection{}, err
	}
	return TraceToLine(cfg, lens, ws)
}

// resolveTraceWorkspace builds the workspace for a trace, in priority order:
//  1. the lens `repos` param (JSON [{name,path}]) — tests / explicit selection;
//  2. the active config project (the single source of truth), honoring
//     params.include_libraries / params.project;
//  3. the legacy user-level workspace (~/.file-projections) as a last resort.
func resolveTraceWorkspace(cfg Config, lens LensConfig) (*Workspace, error) {
	if raw := strings.TrimSpace(lens.Params["repos"]); raw != "" {
		var repos []WorkspaceRepo
		if err := json.Unmarshal([]byte(raw), &repos); err != nil {
			return nil, fmt.Errorf("trace-to-line: bad repos param: %w", err)
		}
		ws := &Workspace{Repos: repos}
		for i := range ws.Repos {
			if ws.Repos[i].Group == "" {
				ws.Repos[i].Group = detectGradle(ws.Repos[i].Path).Group
			}
		}
		return ws, nil
	}
	var proj *ProjectConfig
	if name := strings.TrimSpace(lens.Params["project"]); name != "" {
		proj = projectByName(cfg, name)
	} else {
		proj = activeProject(cfg)
	}
	if proj != nil {
		appOnly := lens.Params["include_libraries"] == "false" || lens.Params["include_libraries"] == "0"
		return workspaceFromProject(cfg, proj, appOnly), nil
	}
	return LoadWorkspace()
}

func cloneSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func normRel(p string) string { return strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./") }
