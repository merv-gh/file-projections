package main

import (
	"path/filepath"
	"sort"
	"strings"
)

// Language registry — the single place that knows about source languages. Every
// scattered `switch lang { java | go | js }` in analyzers, the wizard, the menu,
// the UI and the Joern plumbing routes through here instead, so:
//
//   - adding a language is one Language entry, not edits across ten files;
//   - lenses/analyzers stay language-agnostic (they ask the registry, not switch);
//   - engines (Joern today, tree-sitter later) attach per-language without the
//     analyzers caring which engine answered.
//
// A Language is identified by a short id ("java", "go", "js"). The id "js" covers
// the whole JS/TS family (ts/tsx/js/jsx/mjs/cjs) because they share one frontend.

// Symbol is a language-neutral declaration found in source: a file, function/method,
// type/class, or local variable. The UI symbol search, entry suggestion and the
// var autosuggest all consume this one shape regardless of language.
type Symbol struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // file | func | method | type | class | interface | enum | record | var
	File string `json:"file"` // source-root-relative, slash path
	Line int    `json:"line"`
}

// SymbolScanner extracts symbols from one source file's lines. This is the seam a
// future tree-sitter (or LSP) backend implements: swap the scanner, keep every
// caller. The regex-based scanners below are the default, no-dependency backend.
type SymbolScanner func(rel string, lines []string) []Symbol

// Language bundles everything language-specific behind one value.
type Language struct {
	ID   string   // "java" | "go" | "js"
	Name string   // human label for summaries, e.g. ".java"
	Exts []string // file extensions (lowercase, with dot) that map to this language

	// JoernFrontend is the language frontend binary for CPG builds, or "" to let
	// joern-parse autodetect (the JS/TS case today).
	JoernFrontend string

	// Scan extracts symbols from a file. Defaults to a regex scanner per language.
	Scan SymbolScanner

	// Wizard/menu defaults — the language-appropriate starting lens params. Kept
	// here (not in util.go switches) so a new language ships its own conventions.
	EntrypointPatterns string // entrypoints lens `patterns`
	ExitSinks          string // exitpoints lens `sinks`
	EntryRegex         string // flow/entry-to-exit `entry`
	ExitRegex          string // flow/entry-to-exit `exit`

	// SideEffects are the language's default side-effect markers (IO read/write,
	// network, db). They make side-effects a first-class, language-aware concept:
	// the `side-effects` lens, the unrolled-program views and the service graph all
	// read these so "what does this code actually touch" is answerable without the
	// user hand-writing regexes. A project can still override via lens params.
	SideEffects []SideEffectMarker

	// SuggestRoot picks a sensible source root for this language given a project scan.
	SuggestRoot func(cfg Config, s projectScan) string
}

// SideEffectMarker classifies a line of code as an externally-observable effect.
// Kind is one of the SE* constants; Regex matches the call/syntax that performs it.
type SideEffectMarker struct {
	Kind  string // SEFileRead | SEFileWrite | SENetwork | SEDatabase | SEProcess
	Label string // short human label, e.g. "fs.read", "fetch", "db.query"
	Regex string // matched against a source line (already-compiled lazily by callers)
}

// Side-effect kind constants — a small, language-neutral taxonomy. Anything
// externally observable that makes a function impure falls into one of these.
const (
	SEFileRead  = "io-read"
	SEFileWrite = "io-write"
	SENetwork   = "network"
	SEDatabase  = "db"
	SEProcess   = "process"
)

// languageRegistry is the ordered set of known languages. Order matters only for
// deterministic "dominant language" tie-breaking (java, go, then js).
var languageRegistry = buildLanguageRegistry()

func buildLanguageRegistry() []*Language {
	java := &Language{
		ID: "java", Name: ".java", Exts: []string{".java"},
		JoernFrontend:      "javasrc2cpg",
		Scan:               scanJavaSymbols,
		EntrypointPatterns: "kafka-listener=@KafkaListener;scheduled=@Scheduled;event-listener=@EventListener;http-mapping=@(Get|Post|Put|Delete|Patch|Request)Mapping",
		ExitSinks:          "*repository*.save,*kafka*.send,*.publish",
		EntryRegex:         "@(KafkaListener|Scheduled|EventListener|PostMapping|GetMapping)",
		ExitRegex:          `\.(save|send|publish|Save|Exec)\s*\(`,
		SideEffects:        javaSideEffects(),
		SuggestRoot: func(cfg Config, s projectScan) string {
			if len(s.srcMainJava) > 0 {
				return s.srcMainJava[0]
			}
			return commonDir(s.files["java"])
		},
	}
	golang := &Language{
		ID: "go", Name: ".go", Exts: []string{".go"},
		JoernFrontend:      "gosrc2cpg",
		Scan:               scanGoSymbols,
		EntrypointPatterns: `http-handler=func .*http\.ResponseWriter;route=\.(GET|POST|PUT|DELETE|HandleFunc)\(`,
		ExitSinks:          "*repo*.Save,*.Exec,*.Publish",
		EntryRegex:         "@(KafkaListener|Scheduled|PostMapping|GetMapping)",
		ExitRegex:          `\.(save|send|publish|Save|Exec)\s*\(`,
		SideEffects:        goSideEffects(),
		SuggestRoot: func(cfg Config, s projectScan) string {
			if fileExists(filepath.Join(cfg.Root, "go.mod")) {
				return "."
			}
			return commonDir(s.files["go"])
		},
	}
	js := &Language{
		ID: "js", Name: ".js/.ts", Exts: []string{".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx"},
		JoernFrontend:      "", // joern-parse autodetect
		Scan:               scanTSSymbols,
		EntrypointPatterns: `route=\.(get|post|put|delete)\(;listener=addEventListener\(;handler=\.on\(`,
		ExitSinks:          "*repository*.save,*kafka*.send,*.publish",
		EntryRegex:         "@(KafkaListener|Scheduled|PostMapping|GetMapping)",
		ExitRegex:          `\.(save|send|publish|Save|Exec)\s*\(`,
		SideEffects:        tsSideEffects(),
		SuggestRoot: func(cfg Config, s projectScan) string {
			if fileExists(filepath.Join(cfg.Root, "package.json")) {
				return "."
			}
			return commonDir(s.files["js"])
		},
	}
	return []*Language{java, golang, js}
}

// Default side-effect markers per language. These are deliberately conservative,
// dependency-free regexes over common stdlib/framework idioms — enough to flag the
// usual IO/network/db reach of a function. Projects can extend via the
// `side-effects` lens `markers` param without editing the registry.

func javaSideEffects() []SideEffectMarker {
	return []SideEffectMarker{
		{SEDatabase, "jdbc/jpa", `\b(\.save|\.saveAll|\.delete|\.persist|\.merge|createQuery|prepareStatement|executeQuery|executeUpdate|\.find(One|All|ById)?)\s*\(`},
		{SENetwork, "http-client", `\b(RestTemplate|WebClient|HttpClient|\.getForObject|\.postForObject|\.exchange|URLConnection|new\s+URL)\b`},
		{SEFileWrite, "file-write", `\b(Files\.write|FileOutputStream|FileWriter|BufferedWriter|\.write|PrintWriter)\b`},
		{SEFileRead, "file-read", `\b(Files\.read|FileInputStream|FileReader|BufferedReader|\.readAllBytes|Files\.lines)\b`},
		{SENetwork, "messaging", `\b(kafkaTemplate|\.send|\.publish|\.convertAndSend)\s*\(`},
		{SEProcess, "process/env", `\b(ProcessBuilder|Runtime\.getRuntime|System\.getenv|System\.exit)\b`},
	}
}

func goSideEffects() []SideEffectMarker {
	return []SideEffectMarker{
		{SEDatabase, "database/sql", `\b(db|tx|conn|pool|q|querier)\.(Query|QueryRow|Exec|QueryContext|ExecContext|QueryRowContext|Prepare|Begin|SendBatch|CopyFrom)\s*\(|\.(Save|Create|Updates?|Delete|First|Find|Where)\s*\(`},
		{SEDatabase, "codegen-query", `\b(pggen|sqlc|queries|db)\.(New)?Quer(y|ier)\b|NewQuerier\s*\(|\.(Insert|Select|Update|Delete|Upsert)[A-Z]\w*\s*\(`},
		{SENetwork, "http-client", `\b(http\.(Get|Post|Head|PostForm|Do)|\.RoundTrip|net\.Dial|grpc\.Dial|\.Invoke|serviceFetch|client\.Do)\s*\(`},
		{SEFileWrite, "file-write", `\b(os\.(Create|WriteFile|OpenFile|Remove(All)?|Mkdir(All)?)|ioutil\.WriteFile|\.Write(String)?)\s*\(`},
		{SEFileRead, "file-read", `\b(os\.(Open|ReadFile|Stat|ReadDir)|ioutil\.ReadFile|os\.ReadDir)\s*\(`},
		{SEProcess, "process/env", `\b(exec\.Command|os\.(Getenv|Setenv|Exit|Environ))\b`},
	}
}

func tsSideEffects() []SideEffectMarker {
	return []SideEffectMarker{
		{SENetwork, "fetch/http", `\b(fetch|axios|got|ky|http\.request|https\.request|XMLHttpRequest|\.\$fetch)\s*\(`},
		{SEDatabase, "db/orm", `\b(\.query|\.execute|prisma\.|\.findMany|\.findFirst|\.findUnique|\.create|\.update|\.delete|\.upsert|knex\(|\.raw)\s*\(|\b(pool|client|db)\.(query|connect)\s*\(`},
		{SEFileWrite, "fs-write", `\b(fs\.(writeFile|writeFileSync|appendFile|rm|rmSync|unlink|mkdir|mkdirSync)|writeFile|createWriteStream)\s*\(`},
		{SEFileRead, "fs-read", `\b(fs\.(readFile|readFileSync|readdir|stat|existsSync)|createReadStream)\s*\(`},
		{SEProcess, "process/env", `\b(process\.env|process\.exit|child_process|execSync|spawn|Bun\.spawn)\b`},
	}
}

// languageByID returns the registered language, or nil.
func languageByID(id string) *Language {
	for _, l := range languageRegistry {
		if l.ID == id {
			return l
		}
	}
	return nil
}

// languageForExt maps a file extension (with dot, any case) to its language id, or "".
func languageForExt(ext string) string {
	ext = strings.ToLower(ext)
	for _, l := range languageRegistry {
		for _, e := range l.Exts {
			if e == ext {
				return l.ID
			}
		}
	}
	return ""
}

// languageForPath maps a file path to its language id, or "".
func languageForPath(path string) string {
	return languageForExt(filepath.Ext(path))
}

// isScannableSourcePath reports whether a file belongs to any known language.
// (Also accepts a few extra source extensions for wizard project-scan breadth.)
func isScannableSourcePath(path string) bool {
	if languageForPath(path) != "" {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".kt", ".scala", ".py":
		return true
	default:
		return false
	}
}

// langOf maps a path to a language id, defaulting unknown source files to "js"
// (the wizard's historical behavior so .kt/.scala/.py still count as a web-ish bucket).
func langOf(path string) string {
	if id := languageForPath(path); id != "" {
		return id
	}
	return "js"
}

// allSymbols scans every file under a source root using the per-language scanner,
// matching name/q. limit caps results (<=0 = unlimited). This is the one symbol
// walk; collectSymbols/var/entry suggestions all build on it.
func allSymbols(cfg Config, root, q string, limit int) ([]Symbol, error) {
	idx, err := symbolIndexFor(cfg, root)
	if err != nil {
		return nil, err
	}
	q = strings.ToLower(q)
	match := func(name string) bool { return q == "" || strings.Contains(strings.ToLower(name), q) }
	var out []Symbol
	for _, s := range idx.Symbols {
		if match(s.Name) || (s.Kind == "file" && match(filepath.Base(s.File))) {
			out = append(out, s)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File == out[j].File {
			return out[i].Line < out[j].Line
		}
		return out[i].File < out[j].File
	})
	return out, nil
}
