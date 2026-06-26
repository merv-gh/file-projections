package main

import "sort"

// Analyzer param specs — the single source of truth for "what params does this
// analyzer take, what languages does it apply to, what does it do". Previously
// this lived twice: once implicitly in each analyzer's runtime validation, and
// once hand-mirrored in the UI's JS `SCHEMA`/`HINTS`/applicability maps. They
// drifted (e.g. ast-grep's required `lang` was missing from the UI). Now the UI
// fetches this from /api/config and builds its forms from it, so adding a param
// is one edit here.

// ParamSpec describes one lens param for form rendering and validation.
type ParamSpec struct {
	Key      string   `json:"k"`
	Kind     string   `json:"t"`              // file | line | var | method | type | text | select
	Options  []string `json:"opts,omitempty"` // for kind=select
	Example  string   `json:"ex,omitempty"`
	Required bool     `json:"required,omitempty"`
}

// AnalyzerSpec is the full UI-facing description of an analyzer.
type AnalyzerSpec struct {
	Name   string      `json:"name"`
	Langs  []string    `json:"langs"` // applicable language ids, or ["any"]
	Hint   string      `json:"hint"`
	Params []ParamSpec `json:"params"`
}

// analyzerSpecs returns every analyzer's spec, keyed by name. Languages reference
// the Language registry ids ("java"/"go"/"js") or "any" for language-agnostic lenses.
func analyzerSpecs() map[string]AnalyzerSpec {
	f := func(k, t string) ParamSpec { return ParamSpec{Key: k, Kind: t, Required: true} }
	opt := func(k, t string) ParamSpec { return ParamSpec{Key: k, Kind: t} }
	text := func(k, ex string, req bool) ParamSpec {
		return ParamSpec{Key: k, Kind: "text", Example: ex, Required: req}
	}
	sel := func(k string, opts ...string) ParamSpec { return ParamSpec{Key: k, Kind: "select", Options: opts} }

	specs := []AnalyzerSpec{
		{"control-flow", []string{"java"}, "Control-flow graph of a method at file:line.",
			[]ParamSpec{f("file", "file"), f("line", "line")}},
		{"data-flow", []string{"java"}, "How a variable flows through a method.",
			[]ParamSpec{f("file", "file"), f("line", "line"), f("var", "var")}},
		{"object-flow", []string{"java"}, "How instances of a type move through the program.",
			[]ParamSpec{f("type", "type"), sel("mode", "joern", "cpg")}},
		{"cpg-methods", []string{"java"}, "Methods reachable in the CPG from file:method.",
			[]ParamSpec{f("file", "file"), f("method", "method")}},
		{"joern-var-flow", []string{"java"}, "Joern-backed variable flow.",
			[]ParamSpec{f("file", "file"), f("var", "var"), sel("mode", "joern", "cpg")}},
		{"entrypoints", []string{"java", "go", "js"}, "Detected app entrypoints (routes, listeners).",
			[]ParamSpec{text("patterns", "http-mapping=@(Get|Post)Mapping", true)}},
		{"entry-to-exit", []string{"java"}, "All call-graph flows from entrypoints to exitpoints (joern).",
			[]ParamSpec{text("entry", "@(KafkaListener|PostMapping)", false), text("exit", "\\.(save|send)\\(", false)}},
		{"exitpoints", []string{"java", "go", "js"}, "Sinks/exits (saves, sends).",
			[]ParamSpec{text("sinks", "*repository*.save,*kafka*.send", true)}},
		{"side-effects", []string{"java", "go", "js"}, "Externally-observable effects (IO read/write, network, db, process) — language-aware default markers, override with markers=\"kind=regex;…\".",
			[]ParamSpec{text("markers", "db=\\.query\\(;network=fetch\\(", false)}},
		{"flow", []string{"java"}, "Paths from an entry pattern to a sink pattern.",
			[]ParamSpec{text("entry", "@PostMapping", true), text("sink", "\\.save\\(", true)}},
		{"java-post-to-save", []string{"java"}, "Paths from an entry pattern to a sink pattern.",
			[]ParamSpec{text("entry", "@PostMapping", true), text("sink", "\\.save\\(", true)}},
		{"unrolled-program", []string{"java", "go", "js"}, "Flatten a method's branched, cross-file execution into one editable straight-line program. Edits sync back to real source.",
			nil}, // unroll has its own dedicated UI panel, not generic params
		{"bookmark", []string{"java", "go", "js"}, "Pin a line range of a file.",
			[]ParamSpec{f("file", "file"), text("lines", "7-12", true)}},
		{"extract", []string{"java", "go", "js"}, "Pin a line range of a file.",
			[]ParamSpec{f("file", "file"), text("lines", "7-12", true)}},
		{"ast-grep", []string{"java", "go", "js"}, "Structural search by pattern.",
			[]ParamSpec{text("pattern", "$A.save($B)", true), sel("lang", "ts", "tsx", "js", "java", "go", "python")}},
		{"go-symbols", []string{"go"}, "All Go symbols under the source root — no params.", nil},
		{"js-events", []string{"js"}, "JS event surface.", nil},
		{"jsonl", []string{"any"}, "Project the .jsonl data files.", nil},
		{"service-graph", []string{"any"}, "Cross-service map: imports + routes + TS→Go seam.",
			[]ParamSpec{text("services", `[{"name":"api","root":"api","lang":"go"}]`, true), text("packages", `{"@org/pkg":"shared"}`, false)}},
		{"postgres-watch", []string{"any"}, "Poll Postgres tables by id high-water marks into a rolling CSV window.",
			[]ParamSpec{
				text("connections", `{"dev":"postgres://user:pass@localhost:5432/app?sslmode=disable"}`, true),
				text("tables", "orders,audit_events", true),
				text("window_minutes", "10", false),
				sel("bootstrap", "latest", "all"),
				text("poll_seconds", "30", false),
			}},
	}
	_ = opt
	out := map[string]AnalyzerSpec{}
	for _, s := range specs {
		out[s.Name] = s
	}
	return out
}

// analyzerApplicability derives the language applicability map from the specs, so
// the UI's analyzer filter and form builder read from one source.
func analyzerApplicability() map[string][]string {
	out := map[string][]string{}
	for name, spec := range analyzerSpecs() {
		out[name] = spec.Langs
	}
	return out
}

// sortedAnalyzerNames returns registered analyzer names in stable order.
func sortedAnalyzerNames(reg Registry) []string {
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
