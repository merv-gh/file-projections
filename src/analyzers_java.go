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
	"strconv"
	"strings"
)

// Java frontend: control/data/object flow, CPG methods, unrolled-program.

// Java PostMapping-to-save analyzer: adapter that emits generic projection blocks.
type JavaFile struct {
	Rel     string
	Lines   []string
	Class   string
	Methods []JavaMethod
}

type JavaMethod struct {
	Name        string
	Annotations []string
	Start       int
	End         int
	Lines       []string
}

// AnalyzeFlow is the generic "annotated source reaches a sink" analyzer (the config-driven
// successor to the Spring-specific java-post-to-save). It is parameterised entirely by
// regexes in the lens, so the program ships no domain knowledge:
//
//	params.entry        regex marking the entry method (annotation or signature), e.g. @PostMapping
//	params.sink         regex marking the sink call, e.g. \.save\s*\(
//	params.file_suffix  optional file filter, e.g. Controller.java
//	params.stop_calls   optional csv of call names to ignore during helper discovery
//	params.mode/tool    optional output labels (default flow/flow)
func AnalyzeFlow(cfg Config, lens LensConfig) (Projection, error) {
	if lens.Params == nil || lens.Params["entry"] == "" || lens.Params["sink"] == "" {
		return Projection{}, errors.New("flow: params.entry and params.sink (regexes) are required")
	}
	entryRe, err := regexp.Compile(lens.Params["entry"])
	if err != nil {
		return Projection{}, fmt.Errorf("flow: bad entry regex: %w", err)
	}
	sinkRe, err := regexp.Compile(lens.Params["sink"])
	if err != nil {
		return Projection{}, fmt.Errorf("flow: bad sink regex: %w", err)
	}
	suffix := lens.Params["file_suffix"]
	mode := coalesce(lens.Params["mode"], "flow")
	tool := coalesce(lens.Params["tool"], "flow")
	stopCalls := mergeStopSet(defaultFlowStopCalls, lens.Params["stop_calls"])

	files, err := scanJavaFiles(cfg, lens)
	if err != nil {
		return Projection{}, err
	}
	methodIndex := map[string]JavaMethod{}
	for _, f := range files {
		for _, m := range f.Methods {
			methodIndex[f.Rel+"#"+m.Name] = m
		}
	}

	var p Projection
	for _, f := range files {
		if suffix != "" && !strings.HasSuffix(f.Rel, suffix) {
			continue
		}
		for _, m := range f.Methods {
			if !methodMatchesEntry(m, entryRe) {
				continue
			}
			block := javaFlowBlock(f, m, methodIndex, entryRe, sinkRe, stopCalls, mode, tool)
			if len(block.Lines) > 0 {
				p.Blocks = append(p.Blocks, block)
			}
		}
	}
	return p, nil
}

func parseJavaFile(root, path string) (JavaFile, error) {
	lines, err := readLines(path)
	if err != nil {
		return JavaFile{}, err
	}
	rel, _ := filepath.Rel(root, path)
	rel = filepath.ToSlash(rel)
	jf := JavaFile{Rel: rel, Lines: lines}
	for _, line := range lines {
		if jf.Class == "" {
			if m := classRE.FindStringSubmatch(line); m != nil {
				jf.Class = m[2]
			}
		}
	}
	ms, err := parseJavaMethods(lines)
	if err != nil {
		return JavaFile{}, err
	}
	jf.Methods = ms
	return jf, nil
}

func parseJavaMethods(lines []string) ([]JavaMethod, error) {
	var methods []JavaMethod
	var anns []string
	for i := 0; i < len(lines); i++ {
		trim := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trim, "@") {
			anns = append(anns, trim)
			continue
		}
		if !looksLikeJavaMethod(trim) {
			if trim != "" && !strings.HasPrefix(trim, "//") && !strings.HasPrefix(trim, "*") {
				anns = nil
			}
			continue
		}

		start := i
		sig := []string{}
		for j := i; j < len(lines); j++ {
			sig = append(sig, strings.TrimSpace(lines[j]))
			if strings.Contains(lines[j], "{") {
				i = j
				break
			}
		}
		name := javaMethodName(strings.Join(sig, " "))
		if name == "" {
			anns = nil
			continue
		}
		close, err := findClosingBrace(lines, i)
		if err != nil {
			return nil, err
		}
		methods = append(methods, JavaMethod{Name: name, Annotations: append([]string{}, anns...), Start: start + 1, End: close + 1, Lines: append([]string{}, lines[start:close+1]...)})
		anns = nil
		i = close
	}
	return methods, nil
}

func looksLikeJavaMethod(trim string) bool {
	if !strings.Contains(trim, "(") {
		return false
	}
	for _, prefix := range []string{"if ", "for ", "while ", "switch ", "catch ", "return ", "@", "new "} {
		if strings.HasPrefix(trim, prefix) {
			return false
		}
	}
	return strings.Contains(trim, "{") || strings.HasSuffix(trim, ",") || strings.HasSuffix(trim, ")")
}

func javaMethodName(sig string) string {
	idx := strings.Index(sig, "(")
	if idx < 0 {
		return ""
	}
	parts := strings.Fields(sig[:idx])
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func AnalyzeJavaVarFlowFallback(cfg Config, lens LensConfig, target VarFlowTarget) (Projection, error) {
	lines, method, target, err := locateJavaMethod(cfg, lens, target)
	if err != nil {
		return Projection{}, err
	}

	res := fallbackVarFlow(lines, method, target)
	var p Projection
	block := ProjectionBlock{
		ID:    fmt.Sprintf("%s.%s:%s@%d", javaClassName(lines), method.Name, target.Variable, target.Line),
		File:  target.File,
		Mode:  "var-flow",
		Tool:  "joern-var-flow:fallback",
		Lines: res.Lines,
		Facts: res.Facts,
	}
	p.Blocks = append(p.Blocks, block)
	p.Facts = append(p.Facts, ProjectionFact{ID: "target", Tool: "joern-var-flow", Text: fmt.Sprintf("%s %s:%d variable %s", method.Name, target.File, target.Line, target.Variable)})
	p.Facts = append(p.Facts, ProjectionFact{ID: "contributors", Tool: "joern-var-flow", Text: strings.Join(res.Contributors, ", ")})
	p.Facts = append(p.Facts, ProjectionFact{ID: "limits", Tool: "joern-var-flow", Text: "fallback is lexical/intraprocedural plus object mutation heuristics; Joern mode should replace this for true interprocedural data-flow"})
	return p, nil
}

func AnalyzeEntrypoints(cfg Config, lens LensConfig) (Projection, error) {
	if lens.Params == nil || lens.Params["patterns"] == "" {
		return Projection{}, errors.New("entrypoints: params.patterns is required, e.g. \"kafka-listener=@KafkaListener;http-mapping=@(Get|Post)Mapping\"")
	}
	patterns := parsePatternParam(lens.Params["patterns"])
	return searchMapProjection(cfg, lens, patterns, "entrypoints", "entrypoints")
}

func AnalyzeExitpoints(cfg Config, lens LensConfig) (Projection, error) {
	if lens.Params == nil || lens.Params["sinks"] == "" {
		return Projection{}, errors.New("exitpoints: params.sinks is required, e.g. \"*repository*.save,*kafka*.send\"")
	}
	sinks := splitCSV(lens.Params["sinks"])
	var patterns []labeledPattern
	for _, s := range sinks {
		// Case-insensitive: real bean names are camelCase (orderRepository, kafkaTemplate)
		// while sink globs are usually written lowercase (*repository*.save).
		patterns = append(patterns, labeledPattern{s, `(?i)` + globToRegex(s) + `\s*\(`})
	}
	return searchMapProjection(cfg, lens, patterns, "exitpoints", "exitpoints")
}

type cfgNode struct {
	kind     string // "stmt" | "if"
	line     int    // 1-based
	text     string // raw source line
	exits    bool   // stmt: return/throw
	cond     string // if: condition text
	thenLo   int    // 1-based content range of then-block
	thenHi   int
	thenBody []cfgNode
	hasElse  bool
	elseLo   int
	elseHi   int
	elseBody []cfgNode
}

type cfgEvent struct {
	kind  string // "guard" | "stmt"
	line  int
	text  string
	truth bool
	cond  string
}

type cfgPath struct {
	events  []cfgEvent
	reached bool
	dead    bool
}

// AnalyzeEntryToExit enumerates control flows from entrypoints (methods with an annotation
// matching params.entry) to exitpoints (calls matching params.exit) over the CPG call graph.
// Default is all-to-all; narrow with params.entry_name / params.exit_file for 1-to-1.
func AnalyzeEntryToExit(cfg Config, lens LensConfig) (Projection, error) {
	if lens.Params == nil || lens.Params["entry"] == "" || lens.Params["exit"] == "" {
		return Projection{}, errors.New("entry-to-exit: params.entry and params.exit (regexes) are required")
	}
	outRel := filepath.ToSlash(filepath.Join(cfg.ProjectionsDir, ".joern-entry-to-exit.jsonl"))
	if err := os.MkdirAll(filepath.Join(cfg.Root, cfg.ProjectionsDir), 0755); err != nil {
		return Projection{}, err
	}
	kv := map[string]string{
		"root":      lens.SourceRoot,
		"entry":     lens.Params["entry"],
		"exit":      lens.Params["exit"],
		"entryName": lens.Params["entry_name"],
		"exitFile":  lens.Params["exit_file"],
		"maxPairs":  coalesce(lens.Params["max_pairs"], "200"),
		"out":       outRel,
	}
	if err := runJoernQuery(cfg, lens, "entry-to-exit.sc", outRel, kv, os.Stderr); err != nil {
		return Projection{}, fmt.Errorf("entry-to-exit: %w", err)
	}
	jsonLens := lens
	jsonLens.Input = outRel
	jsonLens.Analyzer = "jsonl"
	p, err := AnalyzeJSONL(cfg, jsonLens)
	if err != nil {
		return Projection{}, err
	}
	p.Sync = "view-only"
	for i := range p.Blocks {
		p.Blocks[i].Lines = reLocLines(p.Blocks[i].Lines, p.Blocks[i].File)
	}
	return p, nil
}

// AnalyzeObjectFlow runs object-flow.sc over the real CPG (joern, local or farm) to list
// every transformation of a target type's instances across the codebase: the constructor,
// each field's setter call sites (in any file), and the final reads. One block per field
// makes a never-set field (ends null) or a wrong-place mutation obvious. params.type (or
// params.var ad-hoc) is the class name. Joern-only: no lexical fallback.
func AnalyzeObjectFlow(cfg Config, lens LensConfig) (Projection, error) {
	typeName := coalesce(lens.Params["type"], lens.Params["var"])
	if typeName == "" {
		return Projection{}, errors.New("object-flow: params.type (the class name) is required")
	}
	outRel := filepath.ToSlash(filepath.Join(cfg.ProjectionsDir, ".joern-object-flow.jsonl"))
	if err := os.MkdirAll(filepath.Join(cfg.Root, cfg.ProjectionsDir), 0755); err != nil {
		return Projection{}, err
	}
	kv := map[string]string{"root": lens.SourceRoot, "typeName": typeName, "out": outRel,
		"field": coalesce(lens.Params["field"], lens.Params["method"])}
	if err := runJoernQuery(cfg, lens, "object-flow.sc", outRel, kv, os.Stderr); err != nil {
		return Projection{}, fmt.Errorf("object-flow: %w", err)
	}
	jsonLens := lens
	jsonLens.Input = outRel
	jsonLens.Analyzer = "jsonl"
	p, err := AnalyzeJSONL(cfg, jsonLens)
	if err != nil {
		return Projection{}, err
	}
	p.Sync = "view-only"
	return p, nil
}

// AnalyzeCPGMethods is a small language-agnostic CPG adapter: it asks Joern for
// methods and their direct call names under a source root. The root can be Java
// or Go; ensureCPG/buildCPGForRoot chooses javasrc2cpg vs gosrc2cpg from the
// source files, so the lens logic stays language-neutral.
func AnalyzeCPGMethods(cfg Config, lens LensConfig) (Projection, error) {
	outRel := filepath.ToSlash(filepath.Join(cfg.ProjectionsDir, ".joern-cpg-methods.jsonl"))
	if err := os.MkdirAll(filepath.Join(cfg.Root, cfg.ProjectionsDir), 0755); err != nil {
		return Projection{}, err
	}
	kv := map[string]string{
		"root": lens.SourceRoot,
		"out":  outRel,
		"file": lens.Params["file"],
		"name": lens.Params["method"],
	}
	if err := runJoernQuery(cfg, lens, "cpg-methods.sc", outRel, kv, os.Stderr); err != nil {
		return Projection{}, fmt.Errorf("cpg-methods: %w", err)
	}
	jsonLens := lens
	jsonLens.Input = outRel
	jsonLens.Analyzer = "jsonl"
	p, err := AnalyzeJSONL(cfg, jsonLens)
	if err != nil {
		return Projection{}, err
	}
	p.Sync = "view-only"
	return p, nil
}

// AnalyzeUnrolledProgram builds an editable straight-line view of the Java path
// selected by params.inputs. It is intentionally param-driven: callers name the
// entry file/method and may provide concrete inputs for branch selection instead
// of relying on fixture-specific package or class names.
func AnalyzeUnrolledProgram(cfg Config, lens LensConfig) (Projection, error) {
	lang := coalesce(lens.Params["lang"], rootLanguage(cfg, lens.SourceRoot))
	if lang == "go" {
		return AnalyzeGoUnrolledProgram(cfg, lens)
	}
	if lang == "js" {
		return AnalyzeTSUnrolledProgram(cfg, lens)
	}
	file := lens.Params["file"]
	method := lens.Params["method"]
	if file == "" {
		return Projection{}, errors.New("unrolled-program: params.file is required")
	}
	if method == "" {
		return Projection{}, errors.New("unrolled-program: params.method is required")
	}
	u := &javaUnroller{cfg: cfg, lens: lens, env: parseUnrollInputs(lens.Params["inputs"]), seenDecision: map[string]bool{},
		selectMode: lens.Params["branch_select"] == "1", forced: parseForcedBranches(lens.Params["branches"]), choiceSeen: map[string]bool{},
		inlineDepth: parseInlineDepth(lens.Params["inline_depth"]), inlineSkips: parseIDSet(lens.Params["inline_skips"]), callSeen: map[string]bool{}}
	lines, err := u.unrollMethod(file, method, 0, nil)
	if err != nil {
		return Projection{}, err
	}
	var body []string
	var origins []LineOrigin
	var lineGuards [][]string
	for _, line := range lines {
		body = append(body, line.code)
		src := filepath.ToSlash(filepath.Join(lens.SourceRoot, line.file))
		origins = append(origins, LineOrigin{SrcFile: src, Line: line.line, SrcHash: hash(line.code + "\n")})
		lineGuards = append(lineGuards, line.guards)
	}
	if len(body) == 0 {
		body = append(body, "// no executable path found")
		lineGuards = append(lineGuards, nil)
	}
	p := Projection{Sync: "two-way"}
	p.Blocks = append(p.Blocks, ProjectionBlock{
		ID:          method,
		File:        file,
		Mode:        "unrolled",
		Tool:        "unrolled-program",
		Lines:       body,
		LineOrigins: origins,
		LineGuards:  lineGuards,
		Sync:        "two-way",
	})
	p.Facts = append(p.Facts, ProjectionFact{ID: "scope", Tool: "unrolled-program", Text: "editable straight-line Java path; each line syncs back to its original source line"})
	for i, d := range u.decisions {
		p.Facts = append(p.Facts, ProjectionFact{ID: fmt.Sprintf("branch-%d", i+1), Tool: "unrolled-program", Text: d})
	}
	if lens.Params["inputs"] == "" && !u.selectMode {
		p.Facts = append(p.Facts, ProjectionFact{ID: "branching", Tool: "unrolled-program", Text: "no params.inputs supplied; unknown conditions include both branches"})
	}
	for i, c := range u.choices {
		if b, err := json.Marshal(c); err == nil {
			p.Facts = append(p.Facts, ProjectionFact{ID: fmt.Sprintf("choice-%d", i+1), Tool: "unrolled-program", Text: string(b)})
		}
	}
	// Per-line assumptions: the conditions that must hold to reach each line, carried
	// as text facts so the CLI/MCP (not just the web UI) can answer "why does this
	// line run?". One fact per guarded line: `lguard-<n>` = `condA && condB`.
	for n, g := range lineGuards {
		if len(g) > 0 {
			p.Facts = append(p.Facts, ProjectionFact{ID: fmt.Sprintf("lguard-%d", n+1), Tool: "unrolled-program", Text: strings.Join(g, " && ")})
		}
	}
	for i, c := range u.calls {
		if b, err := json.Marshal(c); err == nil {
			p.Facts = append(p.Facts, ProjectionFact{ID: fmt.Sprintf("call-%d", i+1), Tool: "unrolled-program", Text: string(b)})
		}
	}
	return p, nil
}

func parseForcedBranches(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func parseInlineDepth(s string) int {
	if strings.TrimSpace(s) == "" {
		return 8
	}
	n := atoi(s)
	if n < 0 {
		return 0
	}
	if n > 10 {
		return 10
	}
	return n
}

func parseIDSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out[part] = true
		}
	}
	return out
}

func (u *javaUnroller) unrollMethod(file, method string, depth int, guards []string) ([]unrollLine, error) {
	if depth > 10 {
		return nil, fmt.Errorf("unrolled-program: recursion limit while inlining %s.%s", file, method)
	}
	lines, methods, err := u.readJavaMethods(file)
	if err != nil {
		return nil, err
	}
	var m JavaMethod
	found := false
	for _, cand := range methods {
		if cand.Name == method {
			m, found = cand, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("unrolled-program: method %q not found in %s", method, file)
	}
	open := firstBraceLine(lines, m.Start-1)
	close, err := findClosingBrace(lines, open)
	if err != nil {
		return nil, err
	}
	return u.unrollRange(file, lines, open+1, close-1, depth, guards)
}

func (u *javaUnroller) unrollRange(file string, lines []string, lo, hi, depth int, guards []string) ([]unrollLine, error) {
	var out []unrollLine
	// running accumulates implied negations: after a guard clause `if (c) return;`,
	// every later sibling line at this level assumes !(c).
	running := guards
	for i := lo; i <= hi && i < len(lines); i++ {
		raw := lines[i]
		trim := strings.TrimSpace(stripLineComment(raw))
		if trim == "" || trim == "{" || trim == "}" || strings.HasPrefix(trim, "//") {
			continue
		}
		if depth > 0 && regexp.MustCompile(`^return\s+[A-Za-z_][A-Za-z0-9_]*\s*;\s*$`).MatchString(trim) {
			continue
		}
		if isIfHeader(trim) {
			braceLine := firstBraceLine(lines, i)
			closeLine, err := findClosingBrace(lines, braceLine)
			if err != nil {
				return nil, err
			}
			hasElse, elseIf, elseBrace, elseClose := detectElse(lines, closeLine, hi)
			cond := extractCond(lines, i, braceLine)
			decision, known := evalJavaCond(cond, u.env)
			switch {
			case known && decision:
				u.addDecision(file, i+1, cond, "then", "decided from inputs")
				part, err := u.unrollRange(file, lines, braceLine+1, closeLine-1, depth, withGuard(running, cond))
				if err != nil {
					return nil, err
				}
				out = append(out, part...)
			case known && !decision && hasElse && !elseIf:
				u.addDecision(file, i+1, cond, "else", "decided from inputs")
				part, err := u.unrollRange(file, lines, elseBrace+1, elseClose-1, depth, withGuard(running, "!("+cond+")"))
				if err != nil {
					return nil, err
				}
				out = append(out, part...)
			case !known && u.selectMode:
				// Runtime-undecidable: collapse to one side (the user's toggle, else the
				// longest branch) and record it so the UI can offer a per-conditional switch.
				elseSide := "skip"
				if hasElse && !elseIf {
					elseSide = "else"
				}
				sides := []string{"then", elseSide}
				side := u.forced[fmt.Sprintf("%s:%d", file, i+1)]
				if side != "then" && side != elseSide {
					side = "" // ignore stale/invalid forced value
				}
				if side == "" {
					thenSpan := closeLine - braceLine
					elseSpan := 0
					if elseSide == "else" {
						elseSpan = elseClose - elseBrace
					}
					if elseSpan > thenSpan {
						side = "else"
					} else {
						side = "then"
					}
				}
				u.recordChoice(file, i+1, cond, side, sides)
				u.addDecision(file, i+1, cond, side, "branch toggle (runtime-undecidable)")
				switch side {
				case "then":
					part, err := u.unrollRange(file, lines, braceLine+1, closeLine-1, depth, withGuard(running, cond))
					if err != nil {
						return nil, err
					}
					out = append(out, part...)
				case "else":
					part, err := u.unrollRange(file, lines, elseBrace+1, elseClose-1, depth, withGuard(running, "!("+cond+")"))
					if err != nil {
						return nil, err
					}
					out = append(out, part...)
				}
			case !known:
				u.addDecision(file, i+1, cond, "both", "runtime-dependent or missing input")
				out = append(out, unrollLine{code: strings.TrimRight(raw, " \t"), file: file, line: i + 1, guards: running})
				part, err := u.unrollRange(file, lines, braceLine+1, closeLine-1, depth, withGuard(running, cond))
				if err != nil {
					return nil, err
				}
				out = append(out, part...)
				if hasElse && !elseIf {
					part, err = u.unrollRange(file, lines, elseBrace+1, elseClose-1, depth, withGuard(running, "!("+cond+")"))
					if err != nil {
						return nil, err
					}
					out = append(out, part...)
				}
			}
			// Guard-clause fall-through: `if (c) return;` with no else means every
			// later sibling line at this level implicitly assumes !(c).
			if !hasElse && rangeExits(lines, braceLine+1, closeLine-1) {
				running = withGuard(running, "!("+cond+")")
			}
			if hasElse {
				i = elseClose
			} else {
				i = closeLine
			}
			continue
		}
		if inlined, ok, err := u.inlineCall(file, trim, i+1, depth, running); err != nil {
			return nil, err
		} else if ok {
			out = append(out, inlined...)
			continue
		}
		out = append(out, unrollLine{code: strings.TrimRight(raw, " \t"), file: file, line: i + 1, guards: running})
	}
	return out, nil
}

func (u *javaUnroller) recordCall(file string, line int, name string, expanded bool, depth int) {
	id := fmt.Sprintf("%s:%d", file, line)
	if u.callSeen[id] {
		return
	}
	u.callSeen[id] = true
	origin := filepath.ToSlash(filepath.Join(u.lens.SourceRoot, file)) + fmt.Sprintf(":%d", line)
	u.calls = append(u.calls, inlineCallChoice{ID: id, Name: name, Origin: origin, Expanded: expanded, Depth: depth})
}

func (u *javaUnroller) recordChoice(file string, line int, cond, side string, sides []string) {
	id := fmt.Sprintf("%s:%d", file, line)
	if u.choiceSeen[id] {
		return
	}
	u.choiceSeen[id] = true
	origin := filepath.ToSlash(filepath.Join(u.lens.SourceRoot, file)) + fmt.Sprintf(":%d", line)
	u.choices = append(u.choices, branchChoice{ID: id, Cond: cond, Origin: origin, Side: side, Sides: sides})
}

func (u *javaUnroller) addDecision(file string, line int, cond, branch, why string) {
	msg := fmt.Sprintf("%s:%d if (%s) -> %s (%s)", file, line, cond, branch, why)
	if u.seenDecision[msg] {
		return
	}
	u.seenDecision[msg] = true
	u.decisions = append(u.decisions, msg)
}

func (u *javaUnroller) inlineCall(file, trim string, line, depth int, guards []string) ([]unrollLine, bool, error) {
	lhs := inlineAssignTarget(trim)
	if m := regexp.MustCompile(`new\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\)\s*\.\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(([^)]*)\)`).FindStringSubmatch(trim); m != nil {
		calleeFile := filepath.ToSlash(filepath.Join(filepath.Dir(file), m[1]+".java"))
		id := fmt.Sprintf("%s:%d", file, line)
		expanded := depth < u.inlineDepth && !u.inlineSkips[id]
		u.recordCall(file, line, m[2], expanded, depth)
		if !expanded {
			return nil, false, nil
		}
		next := *u
		next.bindArgs(calleeFile, m[2], splitArgs(m[3]))
		lines, err := next.unrollMethod(calleeFile, m[2], depth+1, guards)
		u.decisions = next.decisions
		u.seenDecision = next.seenDecision
		u.choices = next.choices
		u.choiceSeen = next.choiceSeen
		u.calls = next.calls
		u.callSeen = next.callSeen
		rewriteInlinedReturns(lines, lhs)
		return lines, true, err
	}
	if m := regexp.MustCompile(`=\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(([^)]*)\)`).FindStringSubmatch(trim); m != nil {
		if _, methods, err := u.readJavaMethods(file); err == nil {
			for _, method := range methods {
				if method.Name == m[1] {
					id := fmt.Sprintf("%s:%d", file, line)
					expanded := depth < u.inlineDepth && !u.inlineSkips[id]
					u.recordCall(file, line, m[1], expanded, depth)
					if !expanded {
						return nil, false, nil
					}
					next := *u
					next.bindArgs(file, m[1], splitArgs(m[2]))
					lines, err := next.unrollMethod(file, m[1], depth+1, guards)
					u.decisions = next.decisions
					u.seenDecision = next.seenDecision
					u.choices = next.choices
					u.choiceSeen = next.choiceSeen
					u.calls = next.calls
					u.callSeen = next.callSeen
					rewriteInlinedReturns(lines, lhs)
					return lines, true, err
				}
			}
		}
	}
	return nil, false, nil
}

func (u *javaUnroller) bindArgs(file, method string, args []string) {
	_, methods, err := u.readJavaMethods(file)
	if err != nil {
		return
	}
	for _, m := range methods {
		if m.Name != method {
			continue
		}
		params := javaParamNames(m.Lines[0])
		for i, p := range params {
			if i >= len(args) {
				continue
			}
			arg := strings.TrimSpace(args[i])
			if v, ok := u.env[arg]; ok {
				u.env[p] = v
			} else {
				u.env[p] = strings.Trim(arg, `"`)
			}
		}
		return
	}
}

func (u *javaUnroller) readJavaMethods(file string) ([]string, []JavaMethod, error) {
	path := filepath.Join(u.cfg.Root, u.lens.SourceRoot, filepath.FromSlash(file))
	lines, err := readLines(path)
	if err != nil {
		return nil, nil, err
	}
	methods, err := parseJavaMethods(lines)
	if err != nil {
		return nil, nil, err
	}
	return lines, methods, nil
}

func parseUnrollInputs(s string) map[string]string {
	env := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return env
}

func javaParamNames(sig string) []string {
	open := strings.IndexByte(sig, '(')
	if open < 0 {
		return nil
	}
	close := matchParen(sig, open)
	if close < 0 {
		return nil
	}
	var out []string
	for _, p := range splitArgs(sig[open+1 : close]) {
		parts := strings.Fields(strings.TrimSpace(p))
		if len(parts) > 0 {
			out = append(out, strings.Trim(parts[len(parts)-1], "[]..."))
		}
	}
	return out
}

func splitArgs(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if strings.TrimSpace(s[start:]) != "" {
		out = append(out, strings.TrimSpace(s[start:]))
	}
	return out
}

func evalJavaCond(cond string, env map[string]string) (bool, bool) {
	c := strings.TrimSpace(cond)
	re := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*(>=|<=|==|!=|>|<)\s*(-?\d+)$`)
	if m := re.FindStringSubmatch(c); m != nil {
		left, ok := env[m[1]]
		if !ok {
			return false, false
		}
		l, err1 := strconv.Atoi(left)
		r, err2 := strconv.Atoi(m[3])
		if err1 != nil || err2 != nil {
			return false, false
		}
		switch m[2] {
		case ">=":
			return l >= r, true
		case "<=":
			return l <= r, true
		case ">":
			return l > r, true
		case "<":
			return l < r, true
		case "==":
			return l == r, true
		case "!=":
			return l != r, true
		}
	}
	re = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\.equals\("([^"]*)"\)$`)
	if m := re.FindStringSubmatch(c); m != nil {
		v, ok := env[m[1]]
		return ok && v == m[2], ok
	}
	return false, false
}

func AnalyzeControlFlow(cfg Config, lens LensConfig) (Projection, error) {
	file := lens.Params["file"]
	method := lens.Params["method"]
	line := atoi(lens.Params["line"])
	if file == "" {
		return Projection{}, errors.New("params.file is required")
	}
	if line <= 0 && method == "" {
		return Projection{}, errors.New("params.line or params.method is required")
	}
	// Opt-in real-CPG mode (handles else-if chains, switch, loops). The lexical
	// enumerator stays the default since it needs no external engine.
	if lens.Params["mode"] == "joern" {
		return RunJoernControlFlow(cfg, lens, file, line, method)
	}
	lines, m, target, err := locateJavaMethod(cfg, lens, VarFlowTarget{File: file, Line: line, Method: method})
	if err != nil {
		return Projection{}, err
	}
	braceLine := firstBraceLine(lines, m.Start-1)
	closeLine, err := findClosingBrace(lines, braceLine)
	if err != nil {
		return Projection{}, fmt.Errorf("control-flow: %w", err)
	}
	nodes := parseCFG(lines, braceLine+1, closeLine-1)
	paths := enumeratePaths(nodes, target.Line)

	maxBranches := atoiDefault(lens.Params["max_branches"], 16)
	if len(paths) > maxBranches {
		paths = paths[:maxBranches]
	}

	sig := strings.TrimSpace(m.Lines[0])
	exitCode := ""
	if target.Line >= 1 && target.Line <= len(lines) {
		exitCode = strings.TrimSpace(lines[target.Line-1])
	}
	stem := strings.TrimSuffix(LensOut(cfg, lens), ".projection")

	p := Projection{Sync: "view-only"}
	var indexLines []string
	if len(paths) == 0 {
		indexLines = append(indexLines, fmt.Sprintf("// no path found from %s entry to %s:%d", m.Name, file, target.Line))
	}
	for k, path := range paths {
		branchNo := k + 1
		rel := filepath.Base(stem) + fmt.Sprintf(".branch-%d.projection", branchNo)
		indexLines = append(indexLines, fmt.Sprintf("branch %d -> %s", branchNo, rel))

		// Path = entry signature, the active conditions (negated when the false branch is
		// taken), then the exitpoint. Code first, file:line in the padded second column.
		var bl []string
		bl = append(bl, codeLoc(sig, file, m.Start))
		for _, ev := range path.events {
			if ev.kind == "guard" {
				cond := ev.cond
				if !ev.truth {
					cond = "!(" + cond + ")"
				}
				bl = append(bl, codeLoc(cond, file, ev.line))
			}
		}
		bl = append(bl, codeLoc(exitCode, file, target.Line))
		branch := Projection{Sync: "view-only"}
		branch.Blocks = append(branch.Blocks, ProjectionBlock{
			ID: fmt.Sprintf("%s.branch-%d", m.Name, branchNo), File: file, Mode: "cfg-path", Tool: "control-flow", Lines: bl,
		})
		p.Extra = append(p.Extra, ExtraFile{Path: fmt.Sprintf("%s.branch-%d.projection", stem, branchNo), Proj: branch})
	}

	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "control-flow", File: file, Mode: "index", Tool: "control-flow", Lines: indexLines})
	return p, nil
}

// extractCond joins the if-header lines and returns the parenthesized condition.
func extractCond(lines []string, ifLine, braceLine int) string {
	var parts []string
	for i := ifLine; i <= braceLine && i < len(lines); i++ {
		parts = append(parts, strings.TrimSpace(lines[i]))
	}
	s := strings.Join(parts, " ")
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return strings.TrimSpace(s)
	}
	close := matchParen(s, open)
	if close < 0 {
		return strings.TrimSpace(s[open+1:])
	}
	return strings.TrimSpace(s[open+1 : close])
}

// detectElse looks for an else attached to the block closing at closeLine. Returns
// whether an else exists, whether it is an (unmodeled) else-if, and the else block's
// brace/close line indices.
func detectElse(lines []string, closeLine, hi int) (has, elseIf bool, braceLine, closeOfElse int) {
	tail := ""
	if idx := strings.LastIndex(lines[closeLine], "}"); idx >= 0 {
		tail = strings.TrimSpace(lines[closeLine][idx+1:])
	}
	scan := closeLine
	if !strings.HasPrefix(tail, "else") {
		j := closeLine + 1
		for j <= hi && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		if j <= hi && strings.HasPrefix(strings.TrimSpace(lines[j]), "else") {
			scan = j
			tail = strings.TrimSpace(lines[j])
		} else {
			return false, false, 0, 0
		}
	}
	afterElse := strings.TrimSpace(strings.TrimPrefix(tail, "else"))
	if strings.HasPrefix(afterElse, "if") {
		// else-if chain: locate its full extent so the caller can skip it.
		bl := firstBraceLine(lines, scan)
		cl, err := findClosingBrace(lines, bl)
		if err != nil {
			return false, false, 0, 0
		}
		return true, true, bl, cl
	}
	bl := firstBraceLine(lines, scan)
	cl, err := findClosingBrace(lines, bl)
	if err != nil {
		return false, false, 0, 0
	}
	return true, false, bl, cl
}

func AnalyzeDataFlow(cfg Config, lens LensConfig) (Projection, error) {
	target, err := varFlowTarget(lens)
	if err != nil {
		return Projection{}, err
	}
	lines, method, target, err := locateJavaMethod(cfg, lens, target)
	if err != nil {
		return Projection{}, err
	}
	res := fallbackVarFlow(lines, method, target)

	var out []string
	for _, h := range res.Hits {
		out = append(out, padComment(strings.TrimRight(h.Text, "\n"), dataFlowNote(h.Why, target.Variable)))
	}
	p := Projection{Sync: "view-only"}
	p.Blocks = append(p.Blocks, ProjectionBlock{
		ID:    fmt.Sprintf("%s.%s:%s@%d", javaClassName(lines), method.Name, target.Variable, target.Line),
		File:  target.File,
		Mode:  "dataflow-inline",
		Tool:  "data-flow",
		Lines: out,
		Facts: res.Facts,
	})
	p.Facts = append(p.Facts, ProjectionFact{ID: "target", Tool: "data-flow", Text: fmt.Sprintf("%s %s:%d variable %s", method.Name, target.File, target.Line, target.Variable)})
	p.Facts = append(p.Facts, ProjectionFact{ID: "contributors", Tool: "data-flow", Text: strings.Join(res.Contributors, ", ")})
	return p, nil
}

// suggestUIExamples picks a real line/var/type from the entry file so lenses that
// need file+line/var/type are prefilled with something that actually exists.
var uiJavaLocalRE = regexp.MustCompile(`^\s*(?:final\s+)?[A-Z][A-Za-z0-9_<>\[\].]*\s+([a-z][A-Za-z0-9_]*)\s*=`)

var uiGoLocalRE = regexp.MustCompile(`^\s*([a-z][A-Za-z0-9_]*)\s*:?=`)

var uiJavaClassRE = regexp.MustCompile(`^\s*(?:public\s+|final\s+|abstract\s+)*(class|interface|enum|record)\s+([A-Za-z_][A-Za-z0-9_]*)`)

var uiGoDeclRE = regexp.MustCompile(`^func(?:\s+\([^)]*\))?\s+([A-Za-z_][A-Za-z0-9_]*)|^type\s+([A-Za-z_][A-Za-z0-9_]*)`)

func scanJavaFiles(cfg Config, lens LensConfig) ([]JavaFile, error) {
	root := filepath.Join(cfg.Root, lens.SourceRoot)
	var out []JavaFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if shouldSkipDir(cfg, path, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".java") {
			return nil
		}
		jf, err := parseJavaFile(cfg.Root, path)
		if err != nil {
			return err
		}
		out = append(out, jf)
		return nil
	})
	return out, err
}

func javaEntryLabel(m JavaMethod, re *regexp.Regexp) string {
	for _, a := range m.Annotations {
		if re.MatchString(a) {
			return strings.TrimSpace(a)
		}
	}
	return re.String()
}

func javaFlowBlock(f JavaFile, m JavaMethod, methodIndex map[string]JavaMethod, entryRe, sinkRe *regexp.Regexp, stopCalls map[string]bool, mode, tool string) ProjectionBlock {
	var sinks []string
	var facts []string
	var lines []string
	label := javaEntryLabel(m, entryRe)
	lines = append(lines, "// entry "+label)
	lines = append(lines, fmt.Sprintf("// source %s:%d-%d", f.Rel, m.Start, m.End))
	lines = append(lines, m.Lines...)

	for i, line := range m.Lines {
		abs := m.Start + i
		trim := strings.TrimSpace(line)
		if sinkRe.MatchString(trim) {
			sinks = append(sinks, fmt.Sprintf("sink: %s:%d `%s`", f.Rel, abs, trim))
		}
	}

	for _, helper := range javaCalledHelpers(m, stopCalls) {
		h, ok := methodIndex[f.Rel+"#"+helper]
		if !ok {
			continue
		}
		helperHasSink := false
		for i, line := range h.Lines {
			abs := h.Start + i
			trim := strings.TrimSpace(line)
			if sinkRe.MatchString(trim) {
				helperHasSink = true
				sinks = append(sinks, fmt.Sprintf("sink: %s:%d `%s`", f.Rel, abs, trim))
			}
		}
		if helperHasSink {
			lines = append(lines, "")
			lines = append(lines, fmt.Sprintf("// helper reached from %s; source %s:%d-%d", m.Name, f.Rel, h.Start, h.End))
			lines = append(lines, h.Lines...)
			facts = append(facts, "helper: "+m.Name+" calls "+helper+", which reaches the sink")
			facts = append(facts, javaFacts("helper."+helper, h.Lines)...)
		}
	}

	if len(sinks) == 0 {
		return ProjectionBlock{}
	}
	facts = append(facts, "entry: "+f.Class+"."+m.Name+" "+label)
	facts = append(facts, javaFacts("", m.Lines)...)
	facts = append(facts, sinks...)

	return ProjectionBlock{ID: f.Class + "." + m.Name, File: f.Rel, Mode: mode, Tool: tool, Lines: lines, Facts: dedupe(facts)}
}

func javaCalledHelpers(m JavaMethod, stopCalls map[string]bool) []string {
	seen := map[string]bool{}
	var names []string
	for idx, line := range m.Lines {
		if idx == 0 || strings.Contains(line, m.Name+"(") && strings.Contains(line, "public ") {
			continue
		}
		for _, mm := range callRE.FindAllStringSubmatch(strings.TrimSpace(line), -1) {
			name := mm[1]
			if name == m.Name || stopCalls[name] || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

func javaFacts(prefix string, lines []string) []string {
	var facts []string
	add := func(s string) {
		if prefix != "" {
			facts = append(facts, prefix+": "+s)
		} else {
			facts = append(facts, s)
		}
	}
	for idx, line := range lines {
		trim := strings.TrimSpace(line)
		if m := ifRE.FindStringSubmatch(trim); m != nil {
			cond := strings.TrimSpace(m[1])
			add("condition: if " + cond)
			if strings.Contains(cond, "hasErrors()") {
				add("required-before-save: " + cond + " must be false")
			}
		}
		if rejectRE.MatchString(trim) {
			prev := nearestIf(lines, idx)
			if prev != "" {
				add("can-set-error: " + prev + " -> " + trim)
			} else {
				add("can-set-error: " + trim)
			}
		}
		if retRE.MatchString(trim) {
			prev := nearestIf(lines, idx)
			if prev != "" {
				add("early-return: " + prev + " -> " + trim)
			} else {
				add("return: " + trim)
			}
		}
	}
	return facts
}

func varFlowTarget(lens LensConfig) (VarFlowTarget, error) {
	line := 0
	if lens.Params != nil && lens.Params["line"] != "" {
		fmt.Sscanf(lens.Params["line"], "%d", &line)
	}
	t := VarFlowTarget{
		Variable: lens.Params["var"],
		File:     lens.Params["file"],
		Line:     line,
		Method:   lens.Params["method"],
		Mode:     lens.Params["mode"],
	}
	if t.Variable == "" {
		return t, errors.New("params.var is required")
	}
	if t.File == "" {
		return t, errors.New("params.file is required")
	}
	if t.Line <= 0 && t.Method == "" {
		return t, errors.New("params.line or params.method is required")
	}
	return t, nil
}

// locateJavaMethod reads the target file and returns its lines plus the enclosing
// method for the target (by method name or by line). It also fills in target.Line
// when only a method was given. Shared by the data-flow and var-flow lenses.
func locateJavaMethod(cfg Config, lens LensConfig, target VarFlowTarget) ([]string, JavaMethod, VarFlowTarget, error) {
	root := filepath.Join(cfg.Root, lens.SourceRoot)
	path := filepath.Join(root, filepath.FromSlash(target.File))
	lines, err := readLines(path)
	if err != nil {
		return nil, JavaMethod{}, target, err
	}
	methods, err := parseJavaMethods(lines)
	if err != nil {
		return nil, JavaMethod{}, target, err
	}
	var method JavaMethod
	found := false
	for _, m := range methods {
		if target.Method != "" && m.Name == target.Method {
			method, found = m, true
			break
		}
		if target.Line > 0 && target.Line >= m.Start && target.Line <= m.End {
			method, found = m, true
			break
		}
	}
	if !found {
		return nil, JavaMethod{}, target, fmt.Errorf("no enclosing Java method found for %s:%d method=%s", target.File, target.Line, target.Method)
	}
	if target.Line <= 0 {
		target.Line = method.End
	}
	return lines, method, target, nil
}

func fallbackVarFlow(fileLines []string, method JavaMethod, target VarFlowTarget) VarFlowResult {
	targetRelLine := target.Line - method.Start
	if targetRelLine < 0 || targetRelLine >= len(method.Lines) {
		targetRelLine = len(method.Lines) - 1
	}

	contrib := map[string]bool{target.Variable: true}
	var facts []string
	var focus []lineHit

	// Method signature contributes parameters.
	if len(method.Lines) > 0 {
		focus = append(focus, lineHit{Line: method.Start, Text: method.Lines[0], Why: "method signature"})
		for _, p := range javaParams(javaMethodSignatureText(method.Lines)) {
			if p == target.Variable {
				facts = append(facts, "source: target variable is method parameter "+p)
				contrib[p] = true
			}
		}
	}

	changed := true
	for changed {
		changed = false
		for idx := 0; idx <= targetRelLine && idx < len(method.Lines); idx++ {
			abs := method.Start + idx
			trim := strings.TrimSpace(method.Lines[idx])
			if trim == "" {
				continue
			}

			if m := ifRE.FindStringSubmatch(trim); m != nil {
				ids := identifiers(m[1])
				touches := anyContributor(ids, contrib)
				if touches || strings.Contains(trim, "hasErrors()") || strings.Contains(trim, target.Variable) {
					focus = append(focus, lineHit{Line: abs, Text: method.Lines[idx], Why: "reachability condition"})
					for _, id := range ids {
						if isJavaValueIdent(id) && !contrib[id] {
							contrib[id] = true
							changed = true
						}
					}
					facts = append(facts, "condition: "+cleanJavaIf(trim))
				}
				if strings.Contains(trim, "hasErrors()") {
					facts = append(facts, "required-before-target: "+strings.TrimPrefix(cleanJavaIf(trim), "if ")+" must not route to early return before line")
				}
				continue
			}

			if retRE.MatchString(trim) {
				prev := nearestIf(method.Lines, idx)
				if prev != "" {
					focus = append(focus, lineHit{Line: abs, Text: method.Lines[idx], Why: "early return"})
					facts = append(facts, "early-return: "+prev+" -> "+trim)
				}
				continue
			}

			if m := javaAssignRE.FindStringSubmatch(trim); m != nil {
				lhs, rhs := m[1], m[2]
				ids := identifiers(rhs)
				if contrib[lhs] || lhs == target.Variable || anyContributor(ids, contrib) {
					focus = append(focus, lineHit{Line: abs, Text: method.Lines[idx], Why: "assignment"})
					for _, id := range ids {
						if isJavaValueIdent(id) && !contrib[id] {
							contrib[id] = true
							changed = true
						}
					}
					vals := filterValueIdents(ids)
					if len(vals) > 0 {
						facts = append(facts, "assignment: "+lhs+" receives data from "+strings.Join(vals, ", "))
					}
				}
				continue
			}

			if m := javaMutatorRE.FindStringSubmatch(trim); m != nil {
				obj, mut, args := m[1], m[2], m[3]
				ids := identifiers(args)
				if contrib[obj] || obj == target.Variable {
					focus = append(focus, lineHit{Line: abs, Text: method.Lines[idx], Why: "object mutation"})
					for _, id := range ids {
						if isJavaValueIdent(id) && !contrib[id] {
							contrib[id] = true
							changed = true
						}
					}
					vals := filterValueIdents(ids)
					if len(vals) > 0 {
						facts = append(facts, "mutation: "+obj+"."+mut+" receives "+strings.Join(vals, ", "))
					} else {
						facts = append(facts, "mutation: "+obj+"."+mut)
					}
				}
			}
		}
	}

	if target.Line > 0 && target.Line <= len(fileLines) {
		focus = append(focus, lineHit{Line: target.Line, Text: fileLines[target.Line-1], Why: "target line"})
		for _, id := range identifiers(fileLines[target.Line-1]) {
			if isJavaValueIdent(id) {
				contrib[id] = true
			}
		}
	}

	focus = uniqueHits(focus)
	sort.Slice(focus, func(i, j int) bool { return focus[i].Line < focus[j].Line })
	var out []string
	out = append(out, fmt.Sprintf("// target variable %s at %s:%d", target.Variable, target.File, target.Line))
	out = append(out, fmt.Sprintf("// enclosing method %s lines=%d-%d", method.Name, method.Start, method.End))
	for _, h := range focus {
		out = append(out, fmt.Sprintf("// line %d: %s", h.Line, h.Why))
		out = append(out, h.Text)
	}

	contributors := mapKeys(contrib)
	sort.Strings(contributors)
	facts = append(facts, "contributors: "+strings.Join(contributors, ", "))
	return VarFlowResult{Target: target, MethodName: method.Name, File: target.File, MethodStart: method.Start, MethodEnd: method.End, Lines: out, Contributors: contributors, Facts: dedupe(facts), Hits: focus}
}

func javaClassName(lines []string) string {
	for _, l := range lines {
		if m := classRE.FindStringSubmatch(l); m != nil {
			return m[2]
		}
	}
	return "Java"
}

func javaMethodSignatureText(lines []string) string {
	var parts []string
	for _, l := range lines {
		parts = append(parts, strings.TrimSpace(l))
		if strings.Contains(l, "{") {
			break
		}
	}
	return strings.Join(parts, " ")
}

func javaParams(sig string) []string {
	open := strings.Index(sig, "(")
	close := strings.LastIndex(sig, ")")
	if open < 0 || close <= open {
		return nil
	}
	inside := sig[open+1 : close]
	parts := strings.Split(inside, ",")
	var params []string
	for _, part := range parts {
		part = strings.TrimSpace(javaParamStripRE.ReplaceAllString(part, ""))
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		name := strings.Trim(fields[len(fields)-1], "[]...")
		if isJavaValueIdent(name) {
			params = append(params, name)
		}
	}
	return params
}

func cleanJavaIf(trim string) string {
	trim = strings.TrimSpace(trim)
	if strings.HasPrefix(trim, "if") {
		cond := strings.TrimSpace(strings.TrimPrefix(trim, "if"))
		cond = strings.TrimSpace(strings.TrimSuffix(cond, "{"))
		cond = strings.TrimSpace(cond)
		if strings.HasPrefix(cond, "(") && strings.HasSuffix(cond, ")") {
			cond = strings.TrimPrefix(strings.TrimSuffix(cond, ")"), "(")
		}
		return "if " + strings.TrimSpace(cond)
	}
	return trim
}

func stripJavaStrings(s string) string {
	var b strings.Builder
	in := rune(0)
	esc := false
	for _, r := range s {
		if in != 0 {
			if esc {
				esc = false
				b.WriteRune(' ')
				continue
			}
			if r == '\\' {
				esc = true
				b.WriteRune(' ')
				continue
			}
			if r == in {
				in = 0
			}
			b.WriteRune(' ')
			continue
		}
		if r == '"' || r == '\'' || r == '`' {
			in = r
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isJavaValueIdent(id string) bool {
	if id == "" {
		return false
	}
	switch id {
	case "if", "return", "new", "null", "true", "false", "this", "public", "private", "protected", "final",
		"String", "Integer", "int", "boolean", "void", "LocalDate", "Objects", "StringUtils", "RedirectAttributes",
		"BindingResult", "Valid", "PathVariable", "ModelAttribute", "Owner", "Pet", "Visit",
		"equals", "getId", "getName", "getPet", "getBirthDate", "hasErrors", "hasText", "isAfter", "isNew", "now", "save", "owners":
		return false
	default:
		return true
	}
}

// parseCFG builds a shallow control-flow tree for the body lines in [lo,hi]
// (0-indexed, inclusive). Supports if / else and nesting; skips else-if chains.
func parseCFG(lines []string, lo, hi int) []cfgNode {
	var nodes []cfgNode
	i := lo
	for i <= hi && i < len(lines) {
		raw := lines[i]
		trim := strings.TrimSpace(stripLineComment(raw))
		if trim == "" || strings.HasPrefix(strings.TrimSpace(raw), "//") || strings.HasPrefix(trim, "*") || strings.HasPrefix(trim, "/*") {
			i++
			continue
		}
		if isIfHeader(trim) {
			braceLine := firstBraceLine(lines, i)
			if braceLine > hi {
				nodes = append(nodes, cfgNode{kind: "stmt", line: i + 1, text: raw, exits: isExitStmt(trim)})
				i++
				continue
			}
			closeLine, err := findClosingBrace(lines, braceLine)
			if err != nil || closeLine > hi {
				nodes = append(nodes, cfgNode{kind: "stmt", line: i + 1, text: raw, exits: isExitStmt(trim)})
				i++
				continue
			}
			node := cfgNode{
				kind: "if", line: i + 1, text: trim, cond: extractCond(lines, i, braceLine),
				thenLo: braceLine + 2, thenHi: closeLine, thenBody: parseCFG(lines, braceLine+1, closeLine-1),
			}
			end := closeLine
			has, elseIf, ebl, ecl := detectElse(lines, closeLine, hi)
			if has && !elseIf {
				node.hasElse = true
				node.elseLo = ebl + 2
				node.elseHi = ecl
				node.elseBody = parseCFG(lines, ebl+1, ecl-1)
				end = ecl
			} else if has && elseIf {
				end = ecl // skip the unmodeled else-if region
			}
			nodes = append(nodes, node)
			i = end + 1
			continue
		}
		nodes = append(nodes, cfgNode{kind: "stmt", line: i + 1, text: raw, exits: isExitStmt(trim)})
		i++
	}
	return nodes
}

func walkNodes(nodes []cfgNode, i, target int) []cfgPath {
	if i >= len(nodes) {
		return []cfgPath{{}}
	}
	n := nodes[i]
	var out []cfgPath
	if n.kind == "stmt" {
		ev := cfgEvent{kind: "stmt", line: n.line, text: n.text}
		if n.line == target {
			return []cfgPath{{events: []cfgEvent{ev}, reached: true}}
		}
		if n.exits && n.line < target {
			return []cfgPath{{events: []cfgEvent{ev}, dead: true}}
		}
		for _, c := range walkNodes(nodes, i+1, target) {
			out = append(out, cfgPath{events: prependEvent(ev, c.events), reached: c.reached, dead: c.dead})
		}
		return out
	}
	// if node: target inside then-block -> forced true
	if target >= n.thenLo && target <= n.thenHi {
		g := cfgEvent{kind: "guard", line: n.line, cond: n.cond, truth: true}
		for _, ip := range walkNodes(n.thenBody, 0, target) {
			if ip.reached && !ip.dead {
				out = append(out, cfgPath{events: prependEvent(g, ip.events), reached: true})
			}
		}
		return out
	}
	// target inside else-block -> forced false
	if n.hasElse && target >= n.elseLo && target <= n.elseHi {
		g := cfgEvent{kind: "guard", line: n.line, cond: n.cond, truth: false}
		for _, ip := range walkNodes(n.elseBody, 0, target) {
			if ip.reached && !ip.dead {
				out = append(out, cfgPath{events: prependEvent(g, ip.events), reached: true})
			}
		}
		return out
	}
	// fork: target is after this if. Each non-exiting side continues to target.
	cont := walkNodes(nodes, i+1, target)
	gTrue := cfgEvent{kind: "guard", line: n.line, cond: n.cond, truth: true}
	for _, tp := range walkNodes(n.thenBody, 0, target) {
		if tp.dead {
			continue
		}
		for _, c := range cont {
			out = append(out, cfgPath{events: concatEvents(gTrue, tp.events, c.events), reached: c.reached, dead: c.dead})
		}
	}
	gFalse := cfgEvent{kind: "guard", line: n.line, cond: n.cond, truth: false}
	if n.hasElse {
		for _, ep := range walkNodes(n.elseBody, 0, target) {
			if ep.dead {
				continue
			}
			for _, c := range cont {
				out = append(out, cfgPath{events: concatEvents(gFalse, ep.events, c.events), reached: c.reached, dead: c.dead})
			}
		}
	} else {
		for _, c := range cont {
			out = append(out, cfgPath{events: prependEvent(gFalse, c.events), reached: c.reached, dead: c.dead})
		}
	}
	return out
}
