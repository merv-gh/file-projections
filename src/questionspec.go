package main

// Question registry — the front door of the Questions panel (see GRASPABILITY.md).
// A Question is a plain-language template with typed blanks ("How do I get from
// [entry] to [line]?") that compiles to an existing lens. This mirrors the analyzer
// spec registry: adding a question is one entry here; it reuses ExecuteLens, the
// symbol-index autocomplete for blanks, and the existing projection rendering.
//
// Questions deliberately do NOT add a new engine — they bind known lenses to a
// human phrasing and a confidence level, so the same answer is reachable whether a
// user thinks in "lenses" (Result tab) or "questions" (Questions panel).

// QBlank is one fill-in slot in a question template.
type QBlank struct {
	Key  string `json:"key"`  // placeholder id, referenced as {key} in Template
	Kind string `json:"kind"` // file | line | var | method | type | text — drives autocomplete
	Hint string `json:"hint"` // short placeholder text
}

// Question is a templated question bound to a lens.
type Question struct {
	ID       string   `json:"id"`
	Intent   string   `json:"intent"`   // understand | diagnose | change | observe
	Template string   `json:"template"` // e.g. "Who calls {name}?" — {key} marks blanks
	Blanks   []QBlank `json:"blanks"`
	Analyzer string   `json:"analyzer"` // lens this compiles to
	Langs    []string `json:"langs"`    // applicable language ids, or ["any"]
	Conf     string   `json:"conf"`     // lexical | structural | cpg | exact — default confidence badge
	// Fixed params merged into the lens (e.g. side-effects needs no blanks). Blank
	// values are mapped into params via Map (blankKey -> paramKey); when Map is nil,
	// each blank key is used verbatim as the param key.
	Fixed map[string]string `json:"-"`
	Map   map[string]string `json:"-"`
}

// questionRegistry is the catalogue. Order is the display order within each intent.
func questionRegistry() []Question {
	return []Question{
		// ── Change ──────────────────────────────────────────────────────────────
		{ID: "where-defined", Intent: "change", Template: "Where is {name} defined?",
			Blanks:   []QBlank{{"name", "method", "function / method"}},
			Analyzer: "references", Langs: []string{"java", "go", "js"}, Conf: "lexical"},
		{ID: "who-calls", Intent: "change", Template: "Who calls {name}?",
			Blanks:   []QBlank{{"name", "method", "function / method"}},
			Analyzer: "call-graph-callers", Langs: []string{"java", "go", "js"}, Conf: "structural"},
		{ID: "impact-of", Intent: "change", Template: "If I change {name}, what breaks?",
			Blanks:   []QBlank{{"name", "method", "function / method"}},
			Analyzer: "impact-set", Langs: []string{"java", "go", "js"}, Conf: "structural"},
		{ID: "where-constructed", Intent: "change", Template: "Where is {type} constructed?",
			Blanks:   []QBlank{{"type", "type", "class / type"}},
			Analyzer: "constructions", Langs: []string{"java", "go", "js"}, Conf: "structural"},
		{ID: "entrypoints", Intent: "change", Template: "What are the entrypoints of this service?",
			Analyzer: "entrypoints", Langs: []string{"java", "go", "js"}, Conf: "lexical"},

		// ── Diagnose ────────────────────────────────────────────────────────────
		{ID: "reach-line", Intent: "diagnose", Template: "What must be true to reach {file}:{line}?",
			Blanks:   []QBlank{{"file", "file", "file"}, {"line", "line", "line"}},
			Analyzer: "control-flow", Langs: []string{"java"}, Conf: "cpg",
			Map: map[string]string{"file": "file", "line": "line"}},
		{ID: "flatten", Intent: "diagnose", Template: "Flatten {method} in {file} so I can read one path",
			Blanks:   []QBlank{{"file", "file", "file"}, {"method", "method", "method / function"}},
			Analyzer: "unrolled-program", Langs: []string{"java", "go", "js"}, Conf: "lexical",
			Map: map[string]string{"file": "file", "method": "method"}},
		{ID: "shape-var", Intent: "diagnose", Template: "Which lines shape {var} in {file}:{line}?",
			Blanks:   []QBlank{{"file", "file", "file"}, {"line", "line", "line"}, {"var", "var", "variable"}},
			Analyzer: "data-flow", Langs: []string{"java"}, Conf: "cpg",
			Map: map[string]string{"file": "file", "line": "line", "var": "var"}},
		{ID: "ways-to-save", Intent: "diagnose", Template: "Show all the ways we end up {sink}",
			Blanks:   []QBlank{{"sink", "text", "*repository*.save,*kafka*.send"}},
			Analyzer: "exitpoints", Langs: []string{"java", "go", "js"}, Conf: "lexical",
			Map: map[string]string{"sink": "sinks"}},
		{ID: "what-touches", Intent: "diagnose", Template: "What does this code touch (IO / network / db)?",
			Analyzer: "side-effects", Langs: []string{"java", "go", "js"}, Conf: "lexical"},
		{ID: "mutates-var", Intent: "diagnose", Template: "What mutates {var}?",
			Blanks:   []QBlank{{"var", "var", "variable / field"}},
			Analyzer: "writes-to", Langs: []string{"java", "go", "js"}, Conf: "lexical"},

		// ── Observe ─────────────────────────────────────────────────────────────
		{ID: "who-changed", Intent: "observe", Template: "Who last changed {file} (lines {lines})?",
			Blanks:   []QBlank{{"file", "file", "file"}, {"lines", "text", "20-40 (optional)"}},
			Analyzer: "git-blame", Langs: []string{"any"}, Conf: "exact",
			Map: map[string]string{"file": "file", "lines": "lines"}},
		{ID: "which-tables", Intent: "observe", Template: "Which tables do the SQL files touch?",
			Analyzer: "sql-tables", Langs: []string{"any"}, Conf: "lexical"},
	}
}

// compileQuestion turns a question id + supplied blank values into a LensConfig.
// Unknown question ids return ok=false. Missing blanks are simply omitted (the lens
// validates required params and returns a clear error, surfaced to the user).
func compileQuestion(id, sourceRoot string, values map[string]string) (LensConfig, string, bool) {
	for _, q := range questionRegistry() {
		if q.ID != id {
			continue
		}
		params := map[string]string{}
		for k, v := range q.Fixed {
			params[k] = v
		}
		for _, b := range q.Blanks {
			val, ok := values[b.Key]
			if !ok || val == "" {
				continue
			}
			pk := b.Key
			if q.Map != nil {
				if mapped, ok := q.Map[b.Key]; ok {
					pk = mapped
				}
			}
			params[pk] = val
		}
		return LensConfig{
			Name:       "ask-" + q.ID,
			Analyzer:   q.Analyzer,
			SourceRoot: sourceRoot,
			Params:     params,
		}, q.Conf, true
	}
	return LensConfig{}, "", false
}
