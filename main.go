package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// joernContext bounds the running time of Joern subprocesses. It defaults to no limit; the
// `perf` benchmark sets a deadline so a runaway parse is killed instead of hanging.
var joernContext = context.Background()

// Joern scripts are embedded so the binary is self-sufficient — no tools/ dir needs to
// ship alongside it. They are materialized under <projections_dir>/.joern-scripts/ at run
// time (that path is inside the Docker bind mount, so containerized Joern can read them).
//
//go:embed tools/joern/*.sc
var embeddedJoernScripts embed.FS

//go:embed VERSION
var versionRaw string

// version is the released semver, sourced from the VERSION file (bumped by `make release-*`).
var version = strings.TrimSpace(versionRaw)

// nowFunc is indirected so tests can pin timestamps deterministically.
var nowFunc = time.Now

type Config struct {
	Root           string                `json:"root"`
	ProjectionsDir string                `json:"projections_dir"`
	ExcludeDirs    []string              `json:"exclude_dirs"`
	Tools          map[string]ToolConfig `json:"tools,omitempty"`
	Lenses         []LensConfig          `json:"lenses"`
}

// ToolConfig describes how to invoke an external tool that may not be installed
// locally. When the binary is missing from PATH, runTool falls back to a Docker
// image. JVMArgs is forwarded to memory-hungry tools like Joern via _JAVA_OPTIONS.
type ToolConfig struct {
	Image   string `json:"image,omitempty"`
	JVMArgs string `json:"jvm_args,omitempty"`
	// Farm is a joern-farm base URL (e.g. http://farmhost:9090). When set for "joern",
	// CPG building AND queries are offloaded to the farm — the local machine runs no Joern.
	Farm string `json:"farm,omitempty"`
}

type LensConfig struct {
	Name       string            `json:"name"`
	Out        string            `json:"out,omitempty"`
	Analyzer   string            `json:"analyzer"`
	SourceRoot string            `json:"source_root,omitempty"`
	Include    []string          `json:"include,omitempty"`
	Input      string            `json:"input,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
}

type Projection struct {
	Lens   LensConfig
	Blocks []ProjectionBlock
	Facts  []ProjectionFact
	// Sync is the projection-level sync policy: "view-only" (analytical lenses
	// that are regenerated, never written back) or "two-way" (extract lenses).
	Sync string
	// Extra holds additional projection files a single lens emits, e.g. the
	// control-flow lens emitting one file per branch. Each is rendered to its Path.
	Extra []ExtraFile
}

type ExtraFile struct {
	Path string
	Proj Projection
}

type ProjectionBlock struct {
	ID    string
	File  string
	Mode  string
	Tool  string
	Lines []string
	Facts []string
	Hash  string
	// LineOrigins maps individual projection lines back to scattered source
	// lines. Unlike SrcStart/SrcEnd, this supports analytical projections whose
	// editable surface is assembled from many files.
	LineOrigins []LineOrigin
	// LineGuards holds, per line, the branch conditions that must hold to reach it
	// (the unrolled-program "per-line assumptions"). In-memory only — never written
	// to the projection text or serialized.
	LineGuards [][]string `json:"-"`
	// Sync/Src fields support two-way "extract" blocks. SrcHash is the hash of
	// the source span at generation time; it lets SyncProjection detect whether
	// the source changed independently of the projection (conflict detection).
	Sync     string
	SrcFile  string
	SrcStart int
	SrcEnd   int
	SrcHash  string
}

type LineOrigin struct {
	SrcFile string
	Line    int
	SrcHash string
}

type ProjectionFact struct {
	ID   string
	Text string
	Tool string
}

type Analyzer interface {
	Name() string
	Analyze(cfg Config, lens LensConfig) (Projection, error)
}

type AnalyzerFunc struct {
	name string
	fn   func(Config, LensConfig) (Projection, error)
}

func (a AnalyzerFunc) Name() string { return a.name }

func (a AnalyzerFunc) Analyze(cfg Config, lens LensConfig) (Projection, error) {
	return a.fn(cfg, lens)
}

type Registry map[string]Analyzer

func DefaultRegistry() Registry {
	return Registry{
		"jsonl":             AnalyzerFunc{"jsonl", AnalyzeJSONL},
		"go-symbols":        AnalyzerFunc{"go-symbols", AnalyzeGoSymbols},
		"flow":              AnalyzerFunc{"flow", AnalyzeFlow},
		"java-post-to-save": AnalyzerFunc{"java-post-to-save", AnalyzeFlow}, // back-compat alias for flow
		"js-events":         AnalyzerFunc{"js-events", AnalyzeJSEvents},
		"joern-var-flow":    AnalyzerFunc{"joern-var-flow", AnalyzeJoernVarFlow},
		"entrypoints":       AnalyzerFunc{"entrypoints", AnalyzeEntrypoints},
		"exitpoints":        AnalyzerFunc{"exitpoints", AnalyzeExitpoints},
		"ast-grep":          AnalyzerFunc{"ast-grep", AnalyzeAstGrep},
		"control-flow":      AnalyzerFunc{"control-flow", AnalyzeControlFlow},
		"entry-to-exit":     AnalyzerFunc{"entry-to-exit", AnalyzeEntryToExit},
		"data-flow":         AnalyzerFunc{"data-flow", AnalyzeDataFlow},
		"object-flow":       AnalyzerFunc{"object-flow", AnalyzeObjectFlow},
		"cpg-methods":       AnalyzerFunc{"cpg-methods", AnalyzeCPGMethods},
		"unrolled-program":  AnalyzerFunc{"unrolled-program", AnalyzeUnrolledProgram},
		"bookmark":          AnalyzerFunc{"bookmark", AnalyzeBookmark},
		"extract":           AnalyzerFunc{"extract", AnalyzeBookmark}, // back-compat alias
	}
}

func main() {
	// Subcommand dispatch: `menu` (interactive) and `watch` (regenerate + back-sync
	// on change) are handled before flag parsing so they keep a clean arg surface.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-version", "--version", "-v":
			fmt.Println("file-projections", version)
			return
		case "help", "-h", "-help", "--help":
			printHelp(os.Stdout)
			return
		case "menu":
			cfg, err := LoadConfig(subConfigPath(os.Args[2:]))
			must(err)
			must(RunMenu(cfg, subConfigPath(os.Args[2:]), os.Stdin, os.Stdout))
			return
		case "watch":
			cfg, err := LoadConfig(subConfigPath(os.Args[2:]))
			must(err)
			must(RunWatch(cfg))
			return
		case "build", "refresh":
			cfg, err := LoadConfig(subConfigPath(os.Args[2:]))
			must(err)
			must(RunBuildCPG(cfg))
			return
		case "perf":
			must(RunPerf(os.Args[2:], os.Stdout))
			return
		case "sync":
			// Reconcile a two-way projection with its source files (the same engine
			// `watch` uses, exposed as a one-shot command so external tools can drive
			// the scattered per-line sync of an unrolled-program edit explicitly).
			args := os.Args[2:]
			cfg, err := LoadConfig(subConfigPath(args))
			must(err)
			var projArgs []string
			for i := 0; i < len(args); i++ {
				a := args[i]
				if a == "-config" {
					i++ // skip the flag's value too
					continue
				}
				if strings.HasPrefix(a, "-config=") {
					continue
				}
				projArgs = append(projArgs, a)
			}
			if len(projArgs) == 0 {
				must(errors.New("usage: file-projections sync <projection-file>"))
			}
			res, err := SyncProjection(cfg, projArgs[0])
			must(err)
			fmt.Printf("synced %s: %d -> source, %d -> projection, %d conflicts\n",
				projArgs[0], res.ToSource, res.ToProjection, len(res.Conflicts))
			for _, c := range res.Conflicts {
				fmt.Println("  conflict:", c)
			}
			return
		case "ui":
			args := os.Args[2:]
			cfg, err := LoadConfig(subConfigPath(args))
			if err != nil && os.IsNotExist(err) {
				cfg = Config{Root: ".", ProjectionsDir: ".projections", ExcludeDirs: defaultExcludeDirs()}
			} else {
				must(err)
			}
			addr := ":7777"
			for i, a := range args {
				if a == "-addr" && i+1 < len(args) {
					addr = args[i+1]
				} else if strings.HasPrefix(a, "-addr=") {
					addr = strings.TrimPrefix(a, "-addr=")
				}
			}
			must(RunUI(cfg, subConfigPath(args), addr, os.Stdout))
			return
		case "bookmarks":
			cfg, err := LoadConfig(subConfigPath(os.Args[2:]))
			must(err)
			expanded, err := expandDropIns(cfg)
			must(err)
			if len(expanded) == 0 {
				fmt.Println("no drop-in bookmarks found (paste e.g. `pkg/Foo.java:17` into a new .projection file)")
			}
			for _, p := range expanded {
				fmt.Println("expanded drop-in bookmark:", p)
			}
			return
		}
	}

	flag.Usage = func() { printHelp(os.Stderr) }
	configPath := flag.String("config", "config.json", "config file")
	analyzer := flag.String("analyzer", "", "run a single ad-hoc lens with this analyzer, e.g. joern-var-flow")
	sourceRoot := flag.String("source-root", "", "source root for ad-hoc lens")
	out := flag.String("out", "", "projection output for ad-hoc lens")
	targetVar := flag.String("var", "", "target variable for joern-var-flow")
	targetFile := flag.String("file", "", "target source file for joern-var-flow")
	targetLine := flag.String("line", "", "target line number for joern-var-flow")
	targetMethod := flag.String("method", "", "target method name for joern-var-flow")
	targetType := flag.String("type", "", "target type name for object-flow")
	inputs := flag.String("inputs", "", "concrete branch inputs for unrolled-program, e.g. amount=50,coupon=save")
	inlineDepth := flag.String("inline_depth", "", "how many levels of called methods to inline for unrolled-program")
	branches := flag.String("branches", "", "forced branch sides for unrolled-program, e.g. App.java:12=then")
	mode := flag.String("mode", "", "adapter mode, e.g. auto, joern, fallback")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil && os.IsNotExist(err) {
		// First run: no config yet. Ad-hoc lens calls get sane defaults; a bare run
		// launches the interactive setup wizard instead of erroring.
		if *analyzer != "" || *targetVar != "" {
			cfg = Config{Root: ".", ProjectionsDir: ".projections", ExcludeDirs: defaultExcludeDirs()}
		} else {
			must(RunWizard(".", *configPath, os.Stdin, os.Stdout))
			return
		}
	} else {
		must(err)
	}

	if *analyzer != "" || *targetVar != "" {
		params := map[string]string{}
		if *targetVar != "" {
			params["var"] = *targetVar
		}
		if *targetFile != "" {
			params["file"] = *targetFile
		}
		if *targetLine != "" {
			params["line"] = *targetLine
		}
		if *targetMethod != "" {
			params["method"] = *targetMethod
		}
		if *targetType != "" {
			params["type"] = *targetType
		}
		if *inputs != "" {
			params["inputs"] = *inputs
		}
		if *inlineDepth != "" {
			params["inline_depth"] = *inlineDepth
		}
		if *branches != "" {
			params["branches"] = *branches
		}
		if *mode != "" {
			params["mode"] = *mode
		}
		lensOut := *out
		if lensOut == "" {
			lensOut = filepath.Join(cfg.ProjectionsDir, "adhoc-"+coalesce(*analyzer, "joern-var-flow")+".projection")
		}
		cfg.Lenses = []LensConfig{{
			Name:       "adhoc-" + coalesce(*analyzer, "joern-var-flow"),
			Out:        lensOut,
			Analyzer:   coalesce(*analyzer, "joern-var-flow"),
			SourceRoot: *sourceRoot,
			Params:     params,
		}}
	}

	results, err := Run(cfg, DefaultRegistry())
	must(err)

	for _, p := range results {
		out := LensOut(cfg, p.Lens)
		fmt.Printf("wrote %s (%d blocks, %d facts)\n", out, len(p.Blocks), len(p.Facts))
	}
}

func printHelp(w io.Writer) {
	fmt.Fprintf(w, `file-projections %s — cross-file projection views for code

USAGE
  file-projections [flags]            generate all lenses in config.json
                                      (with no config.json, launches the setup wizard)
  file-projections <command> [flags]

COMMANDS
  menu          interactively add views (control-flow, data-flow, bookmark, …)
  watch         regenerate on change + sync two-way projection edits back to source
  build         build/refresh the cached Joern CPG (alias: refresh)
  bookmarks     expand single-line drop-in .projection files into two-way bookmarks
  sync          reconcile a two-way projection with its source (one-shot, like watch)
  ui            serve a local web UI to edit config, preview lenses, search symbols (-addr :7777)
  perf          benchmark all-to-all entry→exit on a repo, with a wall-clock cap
  version       print the version
  help          show this help

FLAGS (run one ad-hoc lens without a config)
  -config <path>       config file (default config.json)
  -analyzer <name>     entrypoints|exitpoints|control-flow|data-flow|entry-to-exit|bookmark|flow|ast-grep|joern-var-flow|object-flow|cpg-methods|unrolled-program
  -source-root <dir>   source root for the ad-hoc lens
  -file -line -var -method -type -inputs -mode -out   lens parameters

LENSES
  entrypoints    where control enters (annotations; params.patterns)
  exitpoints     where control leaves (sink globs; params.sinks)
  control-flow   all paths entry→line, one file per branch (mode=joern handles else-if/switch/loops)
  data-flow      the lines that shape a variable, as trailing comments
  entry-to-exit  all call-graph flows from entrypoints to exitpoints (joern)
  object-flow    how target-type instances are assembled across files (joern)
  cpg-methods    language-neutral CPG method/call surface (joern; Java/Go)
  unrolled-program editable straight-line Java/Go path with scattered two-way sync
  bookmark       two-way verbatim span; or drop in 'pkg/Foo.java:17'
  flow           generic "annotated entry reaches a sink"

EXAMPLES
  file-projections                                   # first run → setup wizard
  file-projections -config config.json               # generate every lens
  file-projections menu                              # add a view interactively
  file-projections watch                             # live regenerate + two-way sync
  file-projections -analyzer control-flow -source-root src/main/java \
      -file com/x/Order.java -line 42 -out paths.projection
  echo 'com/x/Order.java:42' > .projections/o.projection && file-projections bookmarks
  file-projections perf -repo https://github.com/spring-projects/spring-petclinic -timeout 5m

DOCS
  README.md — full reference   ·   skill.md — agent usage   ·   RELEASE_NOTES.md
`, version)
}

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

func defaultExcludeDirs() []string {
	return []string{".git", ".gocache", ".gomodcache", ".projections", "node_modules", "target", "build", "dist", "vendor", "__MACOSX"}
}

func Run(cfg Config, registry Registry) ([]Projection, error) {
	if len(cfg.Lenses) == 0 {
		return nil, errors.New("config has no lenses")
	}

	var results []Projection
	for _, lens := range cfg.Lenses {
		p, err := ExecuteLens(cfg, registry, lens)
		if err != nil {
			return nil, fmt.Errorf("lens %s: %w", lens.Name, err)
		}
		if err := RenderProjection(LensOut(cfg, lens), p); err != nil {
			return nil, err
		}
		for _, ex := range p.Extra {
			if err := RenderProjection(ex.Path, ex.Proj); err != nil {
				return nil, err
			}
		}
		results = append(results, p)
	}
	return results, nil
}

func ExecuteLens(cfg Config, registry Registry, lens LensConfig) (Projection, error) {
	analyzer, ok := registry[lens.Analyzer]
	if !ok {
		return Projection{}, fmt.Errorf("unknown analyzer %q", lens.Analyzer)
	}
	p, err := analyzer.Analyze(cfg, lens)
	if err != nil {
		return Projection{}, err
	}
	p.Lens = lens
	finalizeProjection(&p, lens)
	return p, nil
}

// finalizeProjection sorts blocks/facts and stamps content hashes. It recurses
// into Extra files (e.g. control-flow branches) so every emitted projection is
// stamped consistently.
func finalizeProjection(p *Projection, lens LensConfig) {
	if p.Sync == "" {
		p.Sync = "view-only"
	}
	SortProjection(p)
	for i := range p.Blocks {
		p.Blocks[i].Hash = hash(strings.Join(p.Blocks[i].Lines, "\n") + "\n")
	}
	for i := range p.Extra {
		p.Extra[i].Proj.Lens = lens
		finalizeProjection(&p.Extra[i].Proj, lens)
	}
}

func LensOut(cfg Config, lens LensConfig) string {
	if lens.Out != "" {
		if filepath.IsAbs(lens.Out) {
			return lens.Out
		}
		return filepath.Join(cfg.Root, lens.Out)
	}
	name := lens.Name
	if filepath.Ext(name) == "" {
		name += ".projection"
	}
	return filepath.Join(cfg.Root, cfg.ProjectionsDir, name)
}

func RenderProjection(path string, p Projection) error {
	var b strings.Builder
	body := projectionBody(p)
	sync := p.Sync
	if sync == "" {
		sync = "view-only"
	}
	fmt.Fprintf(&b, "# generated by file-projections\n")
	fmt.Fprintf(&b, "# lens: %s\n", p.Lens.Name)
	fmt.Fprintf(&b, "# analyzer: %s\n", p.Lens.Analyzer)
	fmt.Fprintf(&b, "# sync: %s\n", sync)
	fmt.Fprintf(&b, "# source-hash: %s\n", hash(body))
	fmt.Fprintf(&b, "# generated-at: %s\n\n", nowFunc().UTC().Format(time.RFC3339))
	b.WriteString(body)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

// projectionBody renders the stable, timestamp-free body (blocks + facts). The
// header carries the volatile generated-at; idempotency is defined over this body.
func projectionBody(p Projection) string {
	var b strings.Builder
	for _, block := range p.Blocks {
		b.WriteString(renderAnchor(block))
		b.WriteString("\n")
		for _, line := range block.Lines {
			b.WriteString(line)
			if !strings.HasSuffix(line, "\n") {
				b.WriteString("\n")
			}
		}
		b.WriteString("@@\n")
		for _, fact := range block.Facts {
			fmt.Fprintf(&b, "=> %s: %s\n", block.ID, fact)
		}
		for i, origin := range block.LineOrigins {
			if origin.SrcFile == "" || origin.Line <= 0 {
				continue
			}
			fmt.Fprintf(&b, "=> %s: origin %d src=%s:%d srchash=%s\n", block.ID, i+1, origin.SrcFile, origin.Line, origin.SrcHash)
		}
		b.WriteString("\n")
	}

	for _, fact := range p.Facts {
		if fact.Tool == "" {
			fmt.Fprintf(&b, "=> %s: %s\n", fact.ID, fact.Text)
		} else {
			fmt.Fprintf(&b, "=> %s.%s: %s\n", fact.Tool, fact.ID, fact.Text)
		}
	}
	if len(p.Facts) > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

// renderAnchor builds the "@@ ..." block header. Two-way extract blocks carry
// extra metadata (sync/src/srchash) so SyncProjection can map edits back to source.
func renderAnchor(block ProjectionBlock) string {
	anchor := fmt.Sprintf("@@ %s#%s [%s.%s hash=%s", block.File, block.ID, block.Tool, block.Mode, block.Hash)
	if block.Sync == "two-way" && block.SrcFile != "" {
		anchor += fmt.Sprintf(" sync=two-way src=%s:%d-%d srchash=%s", block.SrcFile, block.SrcStart, block.SrcEnd, block.SrcHash)
	}
	return anchor + "]"
}

func SortProjection(p *Projection) {
	sort.SliceStable(p.Blocks, func(i, j int) bool {
		if p.Blocks[i].File == p.Blocks[j].File {
			return p.Blocks[i].ID < p.Blocks[j].ID
		}
		return p.Blocks[i].File < p.Blocks[j].File
	})
	sort.SliceStable(p.Facts, func(i, j int) bool { return p.Facts[i].ID < p.Facts[j].ID })
}

// JSONL analyzer: generic adapter for external tool outputs.
func AnalyzeJSONL(cfg Config, lens LensConfig) (Projection, error) {
	path := filepath.Join(cfg.Root, lens.Input)
	f, err := os.Open(path)
	if err != nil {
		return Projection{}, err
	}
	defer f.Close()

	var p Projection
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Kind  string   `json:"kind"`
			ID    string   `json:"id"`
			File  string   `json:"file"`
			Mode  string   `json:"mode"`
			Tool  string   `json:"tool"`
			Text  string   `json:"text"`
			Lines []string `json:"lines"`
			Facts []string `json:"facts"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return Projection{}, fmt.Errorf("%s:%d: %w", lens.Input, lineNo, err)
		}
		switch rec.Kind {
		case "block":
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: rec.ID, File: rec.File, Mode: rec.Mode, Tool: coalesce(rec.Tool, "jsonl"), Lines: rec.Lines, Facts: rec.Facts})
		case "fact":
			p.Facts = append(p.Facts, ProjectionFact{ID: rec.ID, Tool: coalesce(rec.Tool, "jsonl"), Text: rec.Text})
		default:
			return Projection{}, fmt.Errorf("%s:%d unknown kind %q", lens.Input, lineNo, rec.Kind)
		}
	}
	return p, sc.Err()
}

// Go symbols analyzer: language adapter, not renderer/core.
type GoFile struct {
	Rel   string
	Lines []string
	Types []GoDecl
	Funcs []GoFunc
}

type GoDecl struct {
	Name string
	Kind string
	Line int
	End  int
	Sig  string
}

type GoFunc struct {
	Name  string
	Line  int
	End   int
	Sig   string
	Calls []string
}

var (
	goTypeRE = regexp.MustCompile(`^\s*type\s+([A-Za-z_][A-Za-z0-9_]*)\s+(\S+)`)
	goFuncRE = regexp.MustCompile(`^\s*func\s*(\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	goPkgRE  = regexp.MustCompile(`^\s*package\s+([A-Za-z_][A-Za-z0-9_]*)`)
)

func AnalyzeGoSymbols(cfg Config, lens LensConfig) (Projection, error) {
	files, err := scanGoFiles(cfg, lens)
	if err != nil {
		return Projection{}, err
	}

	funcNames := map[string]bool{}
	for _, f := range files {
		for _, fn := range f.Funcs {
			funcNames[fn.Name] = true
		}
	}
	for fi := range files {
		for i := range files[fi].Funcs {
			fn := &files[fi].Funcs[i]
			fn.Calls = findCalls(fn.Name, files[fi].Lines[fn.Line-1:fn.End], funcNames)
		}
	}

	var p Projection
	for _, f := range files {
		var typeLines []string
		for _, t := range f.Types {
			typeLines = append(typeLines, fmt.Sprintf("%s %s lines=%d-%d :: %s", t.Kind, t.Name, t.Line, t.End, t.Sig))
		}
		if len(typeLines) > 0 {
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "types", File: f.Rel, Mode: "types", Tool: "go-symbols", Lines: typeLines})
		}

		var funcLines []string
		for _, fn := range f.Funcs {
			funcLines = append(funcLines, fmt.Sprintf("%s lines=%d-%d calls=%s", fn.Sig, fn.Line, fn.End, strings.Join(fn.Calls, ",")))
		}
		if len(funcLines) > 0 {
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "functions", File: f.Rel, Mode: "functions", Tool: "go-symbols", Lines: funcLines})
		}
	}

	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "main-callgraph", File: "model", Mode: "callgraph", Tool: "go-symbols", Lines: goCallGraph(files)})
	p.Facts = append(p.Facts, ProjectionFact{ID: "core", Tool: "go-symbols", Text: "Run -> ExecuteLens -> Analyzer -> RenderProjection is the generic path."})
	p.Facts = append(p.Facts, ProjectionFact{ID: "adapters", Tool: "go-symbols", Text: "Language/tool-specific behavior lives behind analyzer adapters registered in DefaultRegistry."})
	return p, nil
}

func scanGoFiles(cfg Config, lens LensConfig) ([]GoFile, error) {
	root := filepath.Join(cfg.Root, lens.SourceRoot)
	allowed := map[string]bool{}
	for _, inc := range lens.Include {
		allowed[filepath.ToSlash(inc)] = true
		allowed[filepath.Base(inc)] = true
	}

	var out []GoFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if shouldSkipDir(cfg, path, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(cfg.Root, path)
		rel = filepath.ToSlash(rel)
		if len(allowed) > 0 && !allowed[rel] && !allowed[filepath.Base(rel)] {
			return nil
		}
		gf, err := parseGoFile(cfg.Root, path)
		if err != nil {
			return err
		}
		out = append(out, gf)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, err
}

func parseGoFile(root, path string) (GoFile, error) {
	lines, err := readLines(path)
	if err != nil {
		return GoFile{}, err
	}
	rel, _ := filepath.Rel(root, path)
	rel = filepath.ToSlash(rel)
	gf := GoFile{Rel: rel, Lines: lines}

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		_ = goPkgRE.FindStringSubmatch(line)
		if m := goTypeRE.FindStringSubmatch(line); m != nil {
			end := i + 1
			if strings.Contains(line, "{") {
				if close, err := findClosingBrace(lines, i); err == nil {
					end = close + 1
				}
			}
			gf.Types = append(gf.Types, GoDecl{Name: m[1], Kind: m[2], Line: i + 1, End: end, Sig: trimBeforeBrace(line)})
		}
		if m := goFuncRE.FindStringSubmatch(line); m != nil {
			close, err := findClosingBrace(lines, i)
			if err != nil {
				continue
			}
			gf.Funcs = append(gf.Funcs, GoFunc{Name: m[2], Line: i + 1, End: close + 1, Sig: trimBeforeBrace(line)})
			i = close
		}
	}
	return gf, nil
}

func findCalls(current string, lines []string, funcNames map[string]bool) []string {
	rx := regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	seen := map[string]bool{}
	var calls []string
	for _, line := range lines {
		for _, m := range rx.FindAllStringSubmatch(stripLineComment(line), -1) {
			name := m[1]
			if name == current || !funcNames[name] || seen[name] {
				continue
			}
			seen[name] = true
			calls = append(calls, name)
		}
	}
	sort.Strings(calls)
	return calls
}

func goCallGraph(files []GoFile) []string {
	graph := map[string][]string{}
	for _, f := range files {
		for _, fn := range f.Funcs {
			graph[fn.Name] = fn.Calls
		}
	}
	var lines []string
	var walk func(string, int, map[string]bool)
	walk = func(name string, depth int, seen map[string]bool) {
		prefix := strings.Repeat("  ", depth)
		if seen[name] {
			lines = append(lines, prefix+"-> "+name+" (seen)")
			return
		}
		lines = append(lines, prefix+"-> "+name)
		seen[name] = true
		calls := append([]string{}, graph[name]...)
		sort.Strings(calls)
		for _, c := range calls {
			walk(c, depth+1, seen)
		}
	}
	walk("main", 0, map[string]bool{})
	return lines
}

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

var (
	classRE  = regexp.MustCompile(`\b(class|interface|record|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	callRE   = regexp.MustCompile(`\b([a-z][A-Za-z0-9_]*)\s*\(`)
	ifRE     = regexp.MustCompile(`^\s*if\s*\((.*)\)\s*\{?\s*$`)
	retRE    = regexp.MustCompile(`^\s*return\b`)
	rejectRE = regexp.MustCompile(`\brejectValue\s*\(`)
)

// defaultFlowStopCalls are general Java/keyword call names ignored when looking for
// helper methods. Domain-specific names (getBirthDate, addPet, ...) are NOT built in —
// pass them via params.stop_calls so the program stays project-agnostic.
var defaultFlowStopCalls = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true,
	"equals": true, "new": true,
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

func mergeStopSet(base map[string]bool, csv string) map[string]bool {
	out := map[string]bool{}
	for k := range base {
		out[k] = true
	}
	for _, s := range splitCSV(csv) {
		out[s] = true
	}
	return out
}

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

func methodMatchesEntry(m JavaMethod, re *regexp.Regexp) bool {
	for _, a := range m.Annotations {
		if re.MatchString(a) {
			return true
		}
	}
	return len(m.Lines) > 0 && re.MatchString(m.Lines[0])
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

func nearestIf(lines []string, idx int) string {
	depth := 0
	for i := idx - 1; i >= 0 && i >= idx-8; i-- {
		trim := strings.TrimSpace(lines[i])
		for _, ch := range trim {
			if ch == '}' {
				depth++
			}
			if ch == '{' && depth > 0 {
				depth--
			}
		}
		if depth == 0 {
			if m := ifRE.FindStringSubmatch(trim); m != nil {
				return "if " + strings.TrimSpace(m[1])
			}
		}
	}
	return ""
}

// Joern variable-flow analyzer: adapter contract for real Joern plus deterministic Java fallback.
type VarFlowTarget struct {
	Variable string
	File     string
	Line     int
	Method   string
	Mode     string
}

type VarFlowResult struct {
	Target       VarFlowTarget
	MethodName   string
	File         string
	MethodStart  int
	MethodEnd    int
	Lines        []string
	Contributors []string
	Facts        []string
	// Hits are the structured contributing lines (source line + reason), used by
	// the data-flow lens to render trailing padded comments instead of // prefixes.
	Hits []lineHit
}

var (
	javaAssignRE     = regexp.MustCompile(`^\s*(?:(?:final\s+)?[A-Za-z_][A-Za-z0-9_<>,.?[\]\s]*\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.+);\s*$`)
	javaMutatorRE    = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\.\s*(set[A-Za-z0-9_]*|add[A-Za-z0-9_]*|put[A-Za-z0-9_]*|remove[A-Za-z0-9_]*)\s*\((.*)\)`)
	javaIdentRE      = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\b`)
	javaParamStripRE = regexp.MustCompile(`@\w+(?:\([^)]*\))?\s*`)
)

func AnalyzeJoernVarFlow(cfg Config, lens LensConfig) (Projection, error) {
	target, err := varFlowTarget(lens)
	if err != nil {
		return Projection{}, err
	}

	mode := coalesce(target.Mode, "auto")
	farm := joernFarm(cfg) != ""
	if mode != "fallback" {
		if mode == "joern" {
			// RunJoernVarFlow → runJoernQuery handles farm vs local (incl. diagnostics).
			p, err := RunJoernVarFlow(cfg, lens, target)
			if err != nil {
				return Projection{}, fmt.Errorf("joern-var-flow mode=joern: %w", err)
			}
			p.Facts = append(p.Facts, ProjectionFact{ID: "mode", Tool: "joern-var-flow", Text: "used joern CPG (" + joernEngine(cfg) + ")"})
			return p, nil
		}
		// Auto: try joern only if plausibly available (or a farm is configured), else fall back.
		if farm || joernAvailable(cfg) {
			{
				if p, err := RunJoernVarFlow(cfg, lens, target); err == nil {
					p.Facts = append(p.Facts, ProjectionFact{ID: "mode", Tool: "joern-var-flow", Text: "used joern CPG (" + joernEngine(cfg) + ")"})
					return p, nil
				}
			}
		}
	}

	p, err := AnalyzeJavaVarFlowFallback(cfg, lens, target)
	if err != nil {
		return Projection{}, err
	}
	p.Facts = append(p.Facts, ProjectionFact{ID: "mode", Tool: "joern-var-flow", Text: "used fallback static Java slicer; install Joern or set tools.joern.image and mode=joern for CPG data-flow"})
	return p, nil
}

// joernScriptRel ensures the named embedded Joern script is written under
// <projections_dir>/.joern-scripts/ and returns its path relative to cfg.Root. This keeps
// the single binary self-sufficient while placing the script inside the Docker bind mount.
func joernScriptRel(cfg Config, name string) (string, error) {
	data, err := embeddedJoernScripts.ReadFile("tools/joern/" + name)
	if err != nil {
		return "", fmt.Errorf("embedded joern script %q not found: %w", name, err)
	}
	dirRel := filepath.Join(cfg.ProjectionsDir, ".joern-scripts")
	if err := os.MkdirAll(filepath.Join(cfg.Root, dirRel), 0755); err != nil {
		return "", err
	}
	abs := filepath.Join(cfg.Root, dirRel, name)
	if cur, err := os.ReadFile(abs); err != nil || !bytes.Equal(cur, data) {
		if err := os.WriteFile(abs, data, 0644); err != nil {
			return "", err
		}
	}
	return filepath.ToSlash(filepath.Join(dirRel, name)), nil
}

// joernAvailable reports whether Joern can run, as a local binary or via a configured
// Docker image (with the daemon reachable).
// defaultJoernImage is used when no tools.joern.image is configured, so a fresh install
// with only Docker works without any config.
const defaultJoernImage = "ghcr.io/joernio/joern:nightly"

func joernImage(cfg Config) string {
	if tc, ok := cfg.Tools["joern"]; ok && tc.Image != "" {
		return tc.Image
	}
	return defaultJoernImage
}

func joernJVMArgs(cfg Config) string {
	if tc, ok := cfg.Tools["joern"]; ok && tc.JVMArgs != "" {
		return tc.JVMArgs
	}
	return "-Xmx6g"
}

// joernAvailable reports whether Joern can plausibly run: a local binary, or Docker on
// PATH (the image is defaulted and pulled on demand by ensureJoern). Cheap, no daemon call.
func joernAvailable(cfg Config) bool {
	if _, err := exec.LookPath("joern"); err == nil {
		return true
	}
	_, err := exec.LookPath("docker")
	return err == nil
}

func joernEngine(cfg Config) string {
	if _, err := exec.LookPath("joern"); err == nil {
		return "local binary"
	}
	return "docker " + joernImage(cfg)
}

// ensureJoern makes Joern ready to run, with actionable diagnostics at each step and a
// one-time image pull (streamed so the user sees progress). It is the single place that
// turns "joern not available" into a clear, fixable message.
func ensureJoern(cfg Config, out io.Writer) error {
	if _, err := exec.LookPath("joern"); err == nil {
		return nil // local binary — nothing to prepare
	}
	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("Joern is not installed and Docker was not found.\n" +
			"  Fix: install Docker Desktop (https://www.docker.com/products/docker-desktop) — " +
			"file-projections will then run Joern in a container automatically.\n" +
			"  Or install Joern directly: https://docs.joern.io/installation")
	}
	if o, err := exec.Command(dockerBin, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return fmt.Errorf("Docker is installed but its daemon isn't responding.\n"+
			"  Fix: start Docker Desktop and wait until it says \"running\", then retry.\n  (docker info said: %s)",
			strings.TrimSpace(firstLine(string(o))))
	}
	img := joernImage(cfg)
	if err := exec.Command(dockerBin, "image", "inspect", img).Run(); err == nil {
		fmt.Fprintf(out, "joern: using docker image %s\n", img)
		return nil
	}
	fmt.Fprintf(out, "joern: image %s not found locally — pulling it now (one-time, several GB)...\n", img)
	cmd := exec.Command(dockerBin, "pull", img)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not pull the Joern image %s.\n"+
			"  Fix: check your internet/registry access, or pull manually:  docker pull %s\n  (%v)", img, img, err)
	}
	fmt.Fprintf(out, "joern: ✓ pulled %s\n", img)
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// execJoern runs a Joern script with key/value params. Path-valued params (named in
// pathKeys) and the script path are rewritten for the execution context: absolute host
// paths for a local joern binary, or /src-mounted container paths for the Docker
// fallback. jvm_args is forwarded via _JAVA_OPTIONS (Joern is memory-hungry).
func execJoern(cfg Config, scriptRel string, kv map[string]string, pathKeys map[string]bool) ([]byte, error) {
	absRoot, _ := filepath.Abs(cfg.Root)
	joernBin, localErr := exec.LookPath("joern")
	local := localErr == nil

	prefix := "/src"
	if local {
		prefix = filepath.ToSlash(absRoot)
	}
	join := func(rel string) string { return prefix + "/" + filepath.ToSlash(rel) }

	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := kv[k]
		if pathKeys[k] && v != "" {
			v = join(v)
		}
		parts = append(parts, k+"="+v)
	}
	scriptPath := join(scriptRel)
	// Joern takes repeatable `--param key=value` flags (not a single --params list).
	scriptArgs := []string{"--script", scriptPath}
	for _, p := range parts {
		scriptArgs = append(scriptArgs, "--param", p)
	}

	if local {
		cmd := exec.CommandContext(joernContext, joernBin, scriptArgs...)
		cmd.Dir = cfg.Root
		return cmd.CombinedOutput()
	}
	dargs := []string{"run", "--rm", "-v", dockerMount(absRoot), "-w", "/src",
		"-e", "_JAVA_OPTIONS=" + joernJVMArgs(cfg), joernImage(cfg), "joern"}
	dargs = append(dargs, scriptArgs...)
	cmd := exec.CommandContext(joernContext, "docker", dargs...)
	cmd.Dir = cfg.Root
	return cmd.CombinedOutput()
}

// dockerMount builds the `-v host:/src` bind spec. The host path uses forward slashes so
// Docker Desktop on Windows accepts it (e.g. C:/Users/me/proj:/src).
func dockerMount(absRoot string) string {
	return filepath.ToSlash(absRoot) + ":/src"
}

// RunBuildCPG (the `build`/`refresh` subcommand) parses each unique source root once
// into a cached cpg.bin under <projections_dir>/.cpg/. Subsequent joern lenses load the
// cache (importCpg) instead of re-importing source — the basis for incremental refresh:
// re-run `build` for a root after its files change.
func RunBuildCPG(cfg Config) error {
	ordered := joernSourceRoots(cfg)
	if len(ordered) == 0 {
		fmt.Println("build: no joern/control-flow/data-flow/entry-to-exit lenses with a source_root; nothing to build")
		return nil
	}
	// Farm mode: parse each root remotely and download the cpg.bin back.
	if farm := joernFarm(cfg); farm != "" {
		for _, root := range ordered {
			jid, err := farmJobForRoot(cfg, farm, root, os.Stdout)
			if err != nil {
				return err
			}
			if err := farmDownloadCPG(cfg, farm, jid, root, os.Stdout); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ensureJoern(cfg, os.Stdout); err != nil {
		return err
	}
	for _, root := range ordered {
		cur, err := sourceManifest(cfg, root)
		if err != nil {
			return err
		}
		prev := loadManifest(filepath.Join(cfg.Root, cpgManifestRel(cfg, root)))
		added, modified, removed := diffManifest(prev, cur)
		outRel := cpgPathRel(cfg, root)
		_, statErr := os.Stat(filepath.Join(cfg.Root, outRel))
		if statErr == nil && len(added)+len(modified)+len(removed) == 0 {
			fmt.Printf("up to date: %s (%d files)\n", root, len(cur))
			continue
		}
		if statErr == nil {
			fmt.Printf("changes in %s: +%d ~%d -%d\n", root, len(added), len(modified), len(removed))
			for _, f := range added {
				fmt.Println("  + " + f)
			}
			for _, f := range modified {
				fmt.Println("  ~ " + f)
			}
			for _, f := range removed {
				fmt.Println("  - " + f)
			}
		}
		if _, err := buildCPGForRoot(cfg, root, len(cur), os.Stdout); err != nil {
			return err
		}
	}
	return nil
}

// buildCPGForRoot parses one source root into its cached cpg.bin with progress + timing,
// and records the manifest. This is the frontend-driven path Joern recommends for large
// codebases (parse once, then query the cpg) instead of re-importing source per query.
func buildCPGForRoot(cfg Config, root string, fileCount int, out io.Writer) (string, error) {
	outRel := cpgPathRel(cfg, root)
	if err := os.MkdirAll(filepath.Join(cfg.Root, filepath.Dir(outRel)), 0755); err != nil {
		return "", err
	}
	tool, jflags := cpgBuildPlan(cfg, root)
	fmt.Fprintf(out, "joern: building CPG for %s (%d files) with %s %s — one-time parse, large repos can take minutes (raise tools.joern.jvm_args if it's slow)...\n",
		root, fileCount, tool, strings.Join(jflags, " "))
	start := nowFunc()
	o, err := execJoernParse(cfg, root, outRel)
	if err != nil {
		return "", fmt.Errorf("building CPG for %s failed: %w\n%s", root, err, firstLines(string(o), 20))
	}
	if cur, e := sourceManifest(cfg, root); e == nil {
		saveManifest(filepath.Join(cfg.Root, cpgManifestRel(cfg, root)), cur)
	}
	fmt.Fprintf(out, "joern: ✓ CPG built in %s -> %s\n", nowFunc().Sub(start).Round(time.Second), outRel)
	return outRel, nil
}

// ensureCPG returns the cached cpg.bin for a root, building it (with progress) if absent.
func ensureCPG(cfg Config, root string, out io.Writer) (string, error) {
	rel := cpgPathRel(cfg, root)
	if fileExists(filepath.Join(cfg.Root, rel)) {
		return rel, nil
	}
	count := 0
	if m, err := sourceManifest(cfg, root); err == nil {
		count = len(m)
	}
	return buildCPGForRoot(cfg, root, count, out)
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "… (truncated)")
	}
	return strings.Join(lines, "\n")
}

// prepareJoernLens readies Joern (clear diagnostics + one-time image pull) and ensures a
// cached CPG for the lens's source root, so the script loads it fast instead of
// re-importing source on every run.
func prepareJoernLens(cfg Config, sourceRoot string, out io.Writer) error {
	if err := ensureJoern(cfg, out); err != nil {
		return err
	}
	if sourceRoot != "" {
		if _, err := ensureCPG(cfg, sourceRoot, out); err != nil {
			return err
		}
	}
	return nil
}

// runJoernScript runs a script with start/done progress + timing so the user can tell
// Joern is working rather than stuck.
func runJoernScript(cfg Config, name, target, scriptRel string, kv map[string]string, out io.Writer) ([]byte, error) {
	fmt.Fprintf(out, "joern: running %s on %s ...\n", name, target)
	start := nowFunc()
	o, err := execJoern(cfg, scriptRel, kv, joernPathKeys)
	fmt.Fprintf(out, "joern: %s finished in %s\n", name, nowFunc().Sub(start).Round(time.Second))
	return o, err
}

// ============================================================================
// joern-farm client: offload CPG build + queries to a remote service so the
// local machine runs no Joern at all (for slow laptops). See joern-farm/.
// ============================================================================

func joernFarm(cfg Config) string {
	if tc, ok := cfg.Tools["joern"]; ok {
		return strings.TrimRight(tc.Farm, "/")
	}
	return ""
}

// runJoernQuery runs one Joern script for a lens and leaves its JSONL at outRel — either
// locally (docker/binary) or fully offloaded to a joern-farm. Callers AnalyzeJSONL(outRel).
func runJoernQuery(cfg Config, lens LensConfig, scriptName, outRel string, kv map[string]string, out io.Writer) error {
	if farm := joernFarm(cfg); farm != "" {
		jobID, err := farmJobForRoot(cfg, farm, lens.SourceRoot, out)
		if err != nil {
			return err
		}
		return farmRunScript(cfg, farm, jobID, scriptName, outRel, kv, out)
	}
	if err := prepareJoernLens(cfg, lens.SourceRoot, out); err != nil {
		return err
	}
	kv = withCpgParam(cfg, lens, kv)
	scriptRel, err := joernScriptRel(cfg, scriptName)
	if err != nil {
		return err
	}
	output, err := runJoernScript(cfg, strings.TrimSuffix(scriptName, ".sc"), lens.SourceRoot, scriptRel, kv, out)
	if err != nil {
		return fmt.Errorf("joern %s failed: %w\n%s", scriptName, err, firstLines(string(output), 25))
	}
	return nil
}

// farmJobForRoot ensures the farm holds a parsed CPG (job) for the source root, returning
// the job id. It caches the id and reuses it while the source is unchanged and the job still
// exists on the farm; otherwise it (re)uploads + parses and waits.
func farmJobForRoot(cfg Config, farm, sourceRoot string, out io.Writer) (string, error) {
	jobFile := filepath.Join(cfg.Root, cpgPathRel(cfg, sourceRoot)) + ".farmjob"
	cur, _ := sourceManifest(cfg, sourceRoot)
	if id, err := os.ReadFile(jobFile); err == nil {
		add, mod, rem := diffManifest(loadManifest(jobFile+".manifest"), cur)
		if len(add)+len(mod)+len(rem) == 0 {
			if jid := strings.TrimSpace(string(id)); farmJobDone(farm, jid) {
				fmt.Fprintf(out, "farm: reusing parsed job %s for %s\n", jid, sourceRoot)
				return jid, nil
			}
		}
	}
	zipBuf, n, err := zipDir(filepath.Join(cfg.Root, sourceRoot))
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "farm: uploading %s (%d files) to %s and parsing...\n", sourceRoot, n, farm)
	jid, err := farmSubmit(farm, filepath.Base(sourceRoot), zipBuf)
	if err != nil {
		return "", err
	}
	if err := farmWait(farm, jid, out); err != nil {
		return "", err
	}
	os.MkdirAll(filepath.Dir(jobFile), 0755)
	os.WriteFile(jobFile, []byte(jid), 0644)
	saveManifest(jobFile+".manifest", cur)
	fmt.Fprintf(out, "farm: ✓ parsed (job %s)\n", jid)
	return jid, nil
}

func farmRunScript(cfg Config, farm, jobID, scriptName, outRel string, kv map[string]string, out io.Writer) error {
	script, err := embeddedJoernScripts.ReadFile("tools/joern/" + scriptName)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("script", scriptName)
	fw.Write(script)
	for k, v := range kv {
		if k == "out" || k == "cpgPath" { // the farm sets these itself
			continue
		}
		mw.WriteField("param", k+"="+v)
	}
	mw.Close()
	fmt.Fprintf(out, "farm: running %s on job %s ...\n", strings.TrimSuffix(scriptName, ".sc"), jobID)
	req, _ := http.NewRequestWithContext(joernContext, "POST", farm+"/jobs/"+jobID+"/script", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("farm: query request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("farm: query failed (HTTP %d): %s", resp.StatusCode, firstLines(string(data), 20))
	}
	abs := filepath.Join(cfg.Root, outRel)
	os.MkdirAll(filepath.Dir(abs), 0755)
	return os.WriteFile(abs, data, 0644)
}

// farmDownloadCPG fetches the parsed cpg.bin from the farm into the local cache.
func farmDownloadCPG(cfg Config, farm, jobID, sourceRoot string, out io.Writer) error {
	rel := cpgPathRel(cfg, sourceRoot)
	abs := filepath.Join(cfg.Root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return err
	}
	resp, err := http.Get(farm + "/jobs/" + jobID + "/cpg")
	if err != nil {
		return fmt.Errorf("farm: download cpg: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("farm: download cpg (HTTP %d): %s", resp.StatusCode, firstLines(string(b), 5))
	}
	f, err := os.Create(abs)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "farm: ✓ downloaded cpg (%d KB) -> %s\n", n/1024, rel)
	return nil
}

func farmSubmit(farm, name string, zipData *bytes.Buffer) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("metadata", fmt.Sprintf(`{"name":%q,"export":false}`, name))
	fw, _ := mw.CreateFormFile("source", "source.zip")
	io.Copy(fw, zipData)
	mw.Close()
	resp, err := http.Post(farm+"/jobs", mw.FormDataContentType(), &body)
	if err != nil {
		return "", fmt.Errorf("farm: cannot reach %s — is the farm running? (%w)", farm, err)
	}
	defer resp.Body.Close()
	var r struct {
		JobID string `json:"jobId"`
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if resp.StatusCode >= 300 || r.JobID == "" {
		return "", fmt.Errorf("farm: submit failed (HTTP %d): %s", resp.StatusCode, r.Error)
	}
	return r.JobID, nil
}

func farmJobStatus(farm, id string) (status, errMsg string, progress int, ok bool) {
	resp, err := http.Get(farm + "/jobs/" + id)
	if err != nil {
		return "", "", 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", 0, false
	}
	var j struct {
		Status   string `json:"status"`
		Error    string `json:"error"`
		Progress int    `json:"progress"`
	}
	json.NewDecoder(resp.Body).Decode(&j)
	return j.Status, j.Error, j.Progress, true
}

func farmJobDone(farm, id string) bool {
	s, _, _, ok := farmJobStatus(farm, id)
	return ok && s == "done"
}

func farmWait(farm, id string, out io.Writer) error {
	last := ""
	for {
		select {
		case <-joernContext.Done():
			return joernContext.Err()
		default:
		}
		s, e, p, ok := farmJobStatus(farm, id)
		if !ok {
			return fmt.Errorf("farm: lost job %s", id)
		}
		if msg := fmt.Sprintf("%s %d%%", s, p); msg != last {
			fmt.Fprintf(out, "farm: %s\n", msg)
			last = msg
		}
		switch s {
		case "done":
			return nil
		case "failed":
			return fmt.Errorf("farm: parse failed: %s", e)
		}
		time.Sleep(2 * time.Second)
	}
}

// zipDir zips the scannable source files under dir into an in-memory buffer.
func zipDir(dir string) (*bytes.Buffer, int, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isScannableSource(p) {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, cErr := io.Copy(w, f)
		f.Close()
		if cErr != nil {
			return cErr
		}
		n++
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return &buf, n, nil
}

// RunPerf benchmarks the Joern path end-to-end: build the CPG for a (large) repo and run an
// all-to-all entrypoint→exitpoint query, under a hard wall-clock timeout, reporting timings.
// Cross-platform (uses a context deadline, not the `timeout` binary).
func RunPerf(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("perf", flag.ContinueOnError)
	fs.SetOutput(out)
	repo := fs.String("repo", "spring-petclinic-main", "git URL or local path of the repo to benchmark")
	sourceRoot := fs.String("source-root", "", "source root within the repo (auto-detected if empty)")
	budget := fs.Duration("timeout", 5*time.Minute, "hard wall-clock cap for the whole benchmark")
	entry := fs.String("entry", "@(KafkaListener|Scheduled|EventListener|PostMapping|GetMapping|PutMapping|DeleteMapping|RequestMapping)", "entrypoint annotation regex")
	exit := fs.String("exit", `\.(save|send|publish|saveAll|persist)\s*\(`, "exitpoint call regex")
	keep := fs.Bool("keep", false, "keep a cloned repo instead of deleting it")
	jvm := fs.String("jvm", "-Xmx6g", "jvm_args for Joern (raise for very large repos)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Resolve the repo to a local directory (clone shallow if it's a URL).
	root := *repo
	cloned := ""
	if isGitURL(*repo) {
		dir, err := os.MkdirTemp("", "fp-perf-*")
		if err != nil {
			return err
		}
		cloned = dir
		fmt.Fprintf(out, "perf: cloning %s (shallow) ...\n", *repo)
		c := exec.Command("git", "clone", "--depth", "1", *repo, dir)
		c.Stdout, c.Stderr = out, out
		if err := c.Run(); err != nil {
			return fmt.Errorf("git clone failed: %w", err)
		}
		root = dir
	}
	if !*keep && cloned != "" {
		defer os.RemoveAll(cloned)
	}
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("repo path %q not found", root)
	}

	cfg := Config{Root: root, ProjectionsDir: ".projections", ExcludeDirs: defaultExcludeDirs(),
		Tools: map[string]ToolConfig{"joern": {Image: defaultJoernImage, JVMArgs: *jvm}}}
	sr := *sourceRoot
	if sr == "" {
		sr = scanProject(cfg).suggestRoot(cfg, "java")
	}
	files := 0
	if m, err := sourceManifest(cfg, sr); err == nil {
		files = len(m)
	}
	lens := LensConfig{Name: "perf-all-to-all", Out: ".projections/perf.projection", Analyzer: "entry-to-exit",
		SourceRoot: sr, Params: map[string]string{"entry": *entry, "exit": *exit, "max_pairs": "100000"}}
	cfg.Lenses = []LensConfig{lens}

	// Hard wall-clock cap: kill Joern subprocesses when the deadline passes.
	ctx, cancel := context.WithTimeout(context.Background(), *budget)
	defer cancel()
	prev := joernContext
	joernContext = ctx
	defer func() { joernContext = prev }()

	fmt.Fprintf(out, "\nperf benchmark\n  repo:        %s\n  source root: %s (%d source files)\n  budget:      %s\n  query:       all-to-all entrypoints→exitpoints\n\n",
		*repo, sr, files, *budget)

	if err := ensureJoern(cfg, out); err != nil {
		return err
	}

	// Phase 1: build the CPG.
	tBuild := nowFunc()
	if _, err := ensureCPG(cfg, sr, out); err != nil {
		return perfErr("CPG build", *budget, ctx, err)
	}
	buildDur := nowFunc().Sub(tBuild)

	// Phase 2: the all-to-all query.
	tQuery := nowFunc()
	p, err := ExecuteLens(cfg, DefaultRegistry(), lens)
	if err != nil {
		return perfErr("entry-to-exit query", *budget, ctx, err)
	}
	queryDur := nowFunc().Sub(tQuery)

	fmt.Fprintf(out, "\n========== perf result ==========\n")
	fmt.Fprintf(out, "  source files:   %d\n", files)
	fmt.Fprintf(out, "  CPG build:      %s\n", buildDur.Round(time.Millisecond))
	fmt.Fprintf(out, "  all-to-all:     %s\n", queryDur.Round(time.Millisecond))
	fmt.Fprintf(out, "  total:          %s (budget %s)\n", (buildDur + queryDur).Round(time.Millisecond), *budget)
	fmt.Fprintf(out, "  flows found:    %d entrypoint→exitpoint paths\n", len(p.Blocks))
	fmt.Fprintf(out, "=================================\n")
	return nil
}

func perfErr(phase string, budget time.Duration, ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("perf: %s exceeded the %s budget (killed). Raise -timeout or -jvm, or split into smaller source roots", phase, budget)
	}
	return fmt.Errorf("perf: %s failed: %w", phase, err)
}

func isGitURL(s string) bool {
	return strings.Contains(s, "://") || strings.HasPrefix(s, "git@") || strings.HasSuffix(s, ".git")
}

// joernSourceRoots returns the unique source roots used by joern-backed lenses.
func joernSourceRoots(cfg Config) []string {
	roots := map[string]bool{}
	for _, lens := range cfg.Lenses {
		switch lens.Analyzer {
		case "joern-var-flow", "control-flow", "data-flow", "entry-to-exit", "object-flow", "cpg-methods":
			if lens.SourceRoot != "" {
				roots[lens.SourceRoot] = true
			}
		}
	}
	ordered := make([]string, 0, len(roots))
	for r := range roots {
		ordered = append(ordered, r)
	}
	sort.Strings(ordered)
	return ordered
}

func cpgManifestRel(cfg Config, sourceRoot string) string {
	return cpgPathRel(cfg, sourceRoot) + ".manifest"
}

// sourceManifest maps each scannable source file under root to a content hash.
func sourceManifest(cfg Config, rootRel string) (map[string]string, error) {
	base := filepath.Join(cfg.Root, rootRel)
	m := map[string]string{}
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if shouldSkipDir(cfg, path, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !isScannableSource(path) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		m[filepath.ToSlash(rel)] = hash(string(b))
		return nil
	})
	return m, err
}

func loadManifest(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func saveManifest(path string, m map[string]string) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(path, b, 0644)
}

// diffManifest reports files added, modified, and removed between two manifests.
func diffManifest(prev, cur map[string]string) (added, modified, removed []string) {
	for f, h := range cur {
		if ph, ok := prev[f]; !ok {
			added = append(added, f)
		} else if ph != h {
			modified = append(modified, f)
		}
	}
	for f := range prev {
		if _, ok := cur[f]; !ok {
			removed = append(removed, f)
		}
	}
	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(removed)
	return
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

// joernFrontend returns the language-specific Joern frontend binary for a source root, or
// "" to use the generic joern-parse. Invoking the frontend directly (vs joern-parse) avoids
// spawning a second JVM — what Joern recommends for large/memory-heavy codebases.
func joernFrontend(lang string) string {
	switch lang {
	case "java":
		return "javasrc2cpg"
	case "go":
		return "gosrc2cpg"
	default:
		return "" // js/ts/mixed → joern-parse autodetect
	}
}

// frontendJVMFlags converts jvm_args (e.g. "-Xmx6g") into frontend -J flags
// (e.g. "-J-Xmx6g"), which set the frontend JVM heap directly.
func frontendJVMFlags(cfg Config) []string {
	var out []string
	for _, t := range strings.Fields(joernJVMArgs(cfg)) {
		out = append(out, "-J"+t)
	}
	return out
}

// cpgBuildPlan describes how the CPG for a root will be built (for progress logging).
func cpgBuildPlan(cfg Config, sourceRootRel string) (tool string, jflags []string) {
	if fe := joernFrontend(rootLanguage(cfg, sourceRootRel)); fe != "" {
		return fe, frontendJVMFlags(cfg)
	}
	return "joern-parse", nil
}

// execJoernParse builds a cpg.bin. For Java/Go it invokes the language frontend directly
// (javasrc2cpg/gosrc2cpg) with -J memory flags — Joern's recommended path for big repos,
// which avoids the extra joern-parse process. Falls back to joern-parse otherwise.
func execJoernParse(cfg Config, sourceRootRel, outRel string) ([]byte, error) {
	absRoot, _ := filepath.Abs(cfg.Root)
	frontend, jflags := cpgBuildPlan(cfg, sourceRootRel)

	// Local binary path (frontend, else joern-parse).
	localBin := frontend
	if _, err := exec.LookPath(localBin); err != nil {
		localBin = ""
	}
	if localBin == "" {
		if _, err := exec.LookPath("joern-parse"); err == nil {
			localBin = "joern-parse"
			jflags = nil
		}
	}
	if localBin != "" {
		args := append([]string{}, jflags...)
		args = append(args, filepath.Join(absRoot, sourceRootRel), "--output", filepath.Join(absRoot, outRel))
		cmd := exec.CommandContext(joernContext, localBin, args...)
		cmd.Dir = cfg.Root
		return cmd.CombinedOutput()
	}

	if _, err := exec.LookPath("docker"); err != nil {
		return nil, errors.New("no Joern frontend in PATH and Docker not available")
	}
	src := "/src/" + filepath.ToSlash(sourceRootRel)
	out := "/src/" + filepath.ToSlash(outRel)
	dargs := []string{"run", "--rm", "-v", dockerMount(absRoot), "-w", "/src"}
	if frontend == "joern-parse" {
		// joern-parse spawns the frontend as a child; pass heap via env so the child gets it.
		dargs = append(dargs, "-e", "_JAVA_OPTIONS="+joernJVMArgs(cfg), joernImage(cfg), "joern-parse", src, "--output", out)
	} else {
		dargs = append(dargs, joernImage(cfg), frontend)
		dargs = append(dargs, jflags...)
		dargs = append(dargs, src, "--output", out)
	}
	cmd := exec.CommandContext(joernContext, "docker", dargs...)
	cmd.Dir = cfg.Root
	return cmd.CombinedOutput()
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

func RunJoernVarFlow(cfg Config, lens LensConfig, target VarFlowTarget) (Projection, error) {
	outRel := filepath.ToSlash(filepath.Join(cfg.ProjectionsDir, ".joern-var-flow.jsonl"))
	if err := os.MkdirAll(filepath.Join(cfg.Root, cfg.ProjectionsDir), 0755); err != nil {
		return Projection{}, err
	}
	kv := map[string]string{
		"root":         lens.SourceRoot,
		"targetFile":   target.File,
		"targetVar":    target.Variable,
		"targetLine":   strconv.Itoa(target.Line),
		"targetMethod": target.Method,
		"out":          outRel,
	}
	if err := runJoernQuery(cfg, lens, "java-var-flow.sc", outRel, kv, os.Stderr); err != nil {
		return Projection{}, err
	}
	jsonLens := lens
	jsonLens.Input = outRel
	jsonLens.Analyzer = "jsonl"
	return AnalyzeJSONL(cfg, jsonLens)
}

// joernPathKeys are the param keys execJoern rewrites to host/container paths.
var joernPathKeys = map[string]bool{"root": true, "out": true, "cpgPath": true}

// withCpgParam points the script at a prebuilt cpg.bin when one exists for this source
// root (see `build`/`refresh`), so scripts importCpg instead of re-importing source.
// The key is cpgPath (not cpg) to avoid shadowing Joern's global `cpg` query root.
func withCpgParam(cfg Config, lens LensConfig, kv map[string]string) map[string]string {
	rel := cpgPathRel(cfg, lens.SourceRoot)
	if _, err := os.Stat(filepath.Join(cfg.Root, rel)); err == nil {
		kv["cpgPath"] = rel
	}
	return kv
}

func cpgPathRel(cfg Config, sourceRoot string) string {
	return filepath.ToSlash(filepath.Join(cfg.ProjectionsDir, ".cpg", hash(sourceRoot)+".bin"))
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

type lineHit struct {
	Line int
	Text string
	Why  string
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

func identifiers(s string) []string {
	s = stripJavaStrings(s)
	var out []string
	matches := javaIdentRE.FindAllStringIndex(s, -1)
	for _, mm := range matches {
		id := s[mm[0]:mm[1]]
		if !isJavaValueIdent(id) {
			continue
		}
		prev := byteBefore(s, mm[0])
		next := byteAfterSpaces(s, mm[1])
		// Skip method/property names in obj.method(...), but keep obj.
		if prev == '.' || next == '(' {
			continue
		}
		out = append(out, id)
	}
	return dedupe(out)
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

func byteBefore(s string, idx int) byte {
	for i := idx - 1; i >= 0; i-- {
		if s[i] == ' ' || s[i] == '\t' {
			continue
		}
		return s[i]
	}
	return 0
}

func byteAfterSpaces(s string, idx int) byte {
	for i := idx; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			continue
		}
		return s[i]
	}
	return 0
}

func anyContributor(ids []string, c map[string]bool) bool {
	for _, id := range ids {
		if c[id] {
			return true
		}
	}
	return false
}

func filterValueIdents(ids []string) []string {
	var out []string
	for _, id := range ids {
		if isJavaValueIdent(id) {
			out = append(out, id)
		}
	}
	return dedupe(out)
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

func uniqueHits(in []lineHit) []lineHit {
	seen := map[int]bool{}
	var out []lineHit
	for _, h := range in {
		if h.Line <= 0 || seen[h.Line] {
			continue
		}
		seen[h.Line] = true
		out = append(out, h)
	}
	return out
}

func mapKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// JavaScript event-surface analyzer: adapter for composable event-driven structures.
type JSFile struct {
	Rel       string
	Lines     []string
	Exports   []JSSymbol
	Functions []JSSymbol
	Classes   []JSSymbol
	Events    []JSEvent
	Regs      []JSRegistration
}

type JSSymbol struct {
	Name string
	Kind string
	Line int
	Sig  string
}

type JSEvent struct {
	Kind string
	Name string
	Line int
	Code string
}

type JSRegistration struct {
	Kind string
	Name string
	Line int
	Code string
}

var (
	jsExportFuncRE   = regexp.MustCompile(`^\s*export\s+(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
	jsExportClassRE  = regexp.MustCompile(`^\s*export\s+class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	jsFunctionRE     = regexp.MustCompile(`^\s*(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
	jsClassRE        = regexp.MustCompile(`^\s*class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	jsConstFuncRE    = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?\(?[^=]*?\)?\s*=>`)
	jsMethodRE       = regexp.MustCompile(`^\s*(?:async\s+)?([A-Za-z_$][A-Za-z0-9_$]*)\s*\([^)]*\)\s*\{`)
	jsEmitRE         = regexp.MustCompile(`(?:\.|\b)(emit|dispatchEvent)\s*\(\s*(?:new\s+CustomEvent\s*\()?['"` + "`" + `]([^'"` + "`" + `]+)['"` + "`" + `]`)
	jsOnRE           = regexp.MustCompile(`(?:\.|\b)(on|once|addEventListener)\s*\(\s*['"` + "`" + `]([^'"` + "`" + `]+)['"` + "`" + `]`)
	jsRegisterRE     = regexp.MustCompile(`\b(register(?:Action|Scene|Character|Item|Stat|Quest|MiniGame|Hotkey)|core\.register(?:Action|Scene|Character|Item|Stat|Quest|MiniGame|Hotkey))\s*\(\s*['"` + "`" + `]?([^,'"` + "`" + `)\s]+)`)
	jsModsRegisterRE = regexp.MustCompile(`\b(mods\.register|registerMod)\s*\(\s*['"` + "`" + `]?([^,'"` + "`" + `)\s]+)`)
	jsImportRE       = regexp.MustCompile(`^\s*import\b`)
)

func AnalyzeJSEvents(cfg Config, lens LensConfig) (Projection, error) {
	files, err := scanJSFiles(cfg, lens)
	if err != nil {
		return Projection{}, err
	}

	var p Projection
	var summary []string
	totalEmits, totalListeners, totalRegs := 0, 0, 0

	for _, f := range files {
		if len(f.Exports)+len(f.Functions)+len(f.Classes) > 0 {
			var lines []string
			for _, x := range f.Exports {
				lines = append(lines, fmt.Sprintf("export %s %s line=%d :: %s", x.Kind, x.Name, x.Line, x.Sig))
			}
			for _, x := range f.Classes {
				lines = append(lines, fmt.Sprintf("class %s line=%d :: %s", x.Name, x.Line, x.Sig))
			}
			for _, x := range f.Functions {
				lines = append(lines, fmt.Sprintf("function %s line=%d :: %s", x.Name, x.Line, x.Sig))
			}
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "surface", File: f.Rel, Mode: "surface", Tool: "js-events", Lines: dedupe(lines)})
		}

		if len(f.Events) > 0 {
			var lines []string
			var facts []string
			for _, ev := range f.Events {
				lines = append(lines, fmt.Sprintf("%s %s line=%d :: %s", ev.Kind, ev.Name, ev.Line, ev.Code))
				if ev.Kind == "emit" || ev.Kind == "dispatch" {
					totalEmits++
				} else {
					totalListeners++
				}
			}
			facts = append(facts, fmt.Sprintf("event surface: %d events/listeners in %s", len(f.Events), f.Rel))
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "events", File: f.Rel, Mode: "events", Tool: "js-events", Lines: dedupe(lines), Facts: facts})
		}

		if len(f.Regs) > 0 {
			var lines []string
			for _, r := range f.Regs {
				lines = append(lines, fmt.Sprintf("%s %s line=%d :: %s", r.Kind, r.Name, r.Line, r.Code))
				totalRegs++
			}
			p.Blocks = append(p.Blocks, ProjectionBlock{ID: "registrations", File: f.Rel, Mode: "registrations", Tool: "js-events", Lines: dedupe(lines)})
		}
	}

	summary = append(summary, fmt.Sprintf("files scanned: %d", len(files)))
	summary = append(summary, fmt.Sprintf("event emits/dispatches: %d", totalEmits))
	summary = append(summary, fmt.Sprintf("event listeners/subscriptions: %d", totalListeners))
	summary = append(summary, fmt.Sprintf("registrations: %d", totalRegs))
	summary = append(summary, "use this lens to see composable event-driven working surface without opening full files")
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "summary", File: "model", Mode: "summary", Tool: "js-events", Lines: summary})

	return p, nil
}

func scanJSFiles(cfg Config, lens LensConfig) ([]JSFile, error) {
	root := filepath.Join(cfg.Root, lens.SourceRoot)
	include := map[string]bool{}
	for _, inc := range lens.Include {
		include[filepath.ToSlash(inc)] = true
		include[filepath.Base(inc)] = true
	}

	var files []JSFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if shouldSkipDir(cfg, path, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || !isJSFile(path) {
			return nil
		}
		rel, _ := filepath.Rel(cfg.Root, path)
		rel = filepath.ToSlash(rel)
		if strings.Contains(rel, "__MACOSX/") || strings.Contains(rel, "/._") || strings.HasPrefix(filepath.Base(rel), "._") {
			return nil
		}
		if len(include) > 0 && !include[rel] && !include[filepath.Base(rel)] {
			return nil
		}
		f, err := parseJSFile(cfg.Root, path)
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	return files, err
}

func isJSFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx":
		return true
	default:
		return false
	}
}

func parseJSFile(root, path string) (JSFile, error) {
	lines, err := readLines(path)
	if err != nil {
		return JSFile{}, err
	}
	rel, _ := filepath.Rel(root, path)
	rel = filepath.ToSlash(rel)
	f := JSFile{Rel: rel, Lines: lines}

	for i, line := range lines {
		trim := strings.TrimSpace(line)
		lineNo := i + 1
		if trim == "" || strings.HasPrefix(trim, "//") {
			continue
		}
		if m := jsExportFuncRE.FindStringSubmatch(trim); m != nil {
			f.Exports = append(f.Exports, JSSymbol{Name: m[1], Kind: "function", Line: lineNo, Sig: trimBeforeBrace(trim)})
			continue
		}
		if m := jsExportClassRE.FindStringSubmatch(trim); m != nil {
			f.Exports = append(f.Exports, JSSymbol{Name: m[1], Kind: "class", Line: lineNo, Sig: trimBeforeBrace(trim)})
			continue
		}
		if strings.HasPrefix(trim, "export ") {
			f.Exports = append(f.Exports, JSSymbol{Name: compactJSName(trim), Kind: "value", Line: lineNo, Sig: trimBeforeBrace(trim)})
		}
		if m := jsClassRE.FindStringSubmatch(trim); m != nil {
			f.Classes = append(f.Classes, JSSymbol{Name: m[1], Kind: "class", Line: lineNo, Sig: trimBeforeBrace(trim)})
		}
		// Keep the surface compact: top-level functions/arrow functions only.
		// Class internals and inline callbacks are intentionally left to event/registration facts.
		isTopLevel := len(line) > 0 && line[0] != ' ' && line[0] != '\t'
		if isTopLevel {
			if m := jsFunctionRE.FindStringSubmatch(trim); m != nil {
				f.Functions = append(f.Functions, JSSymbol{Name: m[1], Kind: "function", Line: lineNo, Sig: trimBeforeBrace(trim)})
			}
			if m := jsConstFuncRE.FindStringSubmatch(trim); m != nil {
				f.Functions = append(f.Functions, JSSymbol{Name: m[1], Kind: "function", Line: lineNo, Sig: trimBeforeBrace(trim)})
			}
		}
		for _, m := range jsEmitRE.FindAllStringSubmatch(trim, -1) {
			kind := "emit"
			if m[1] == "dispatchEvent" {
				kind = "dispatch"
			}
			f.Events = append(f.Events, JSEvent{Kind: kind, Name: m[2], Line: lineNo, Code: trim})
		}
		for _, m := range jsOnRE.FindAllStringSubmatch(trim, -1) {
			kind := "listen"
			if m[1] == "on" || m[1] == "once" {
				kind = "subscribe"
			}
			f.Events = append(f.Events, JSEvent{Kind: kind, Name: m[2], Line: lineNo, Code: trim})
		}
		for _, m := range jsRegisterRE.FindAllStringSubmatch(trim, -1) {
			f.Regs = append(f.Regs, JSRegistration{Kind: strings.TrimPrefix(m[1], "core."), Name: m[2], Line: lineNo, Code: trim})
		}
		for _, m := range jsModsRegisterRE.FindAllStringSubmatch(trim, -1) {
			f.Regs = append(f.Regs, JSRegistration{Kind: m[1], Name: m[2], Line: lineNo, Code: trim})
		}
	}
	return f, nil
}

func compactJSName(line string) string {
	line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return strings.Trim(fields[1], "{};,")
	}
	return "export"
}

func jsControlWord(s string) bool {
	switch s {
	case "if", "for", "while", "switch", "catch", "function":
		return true
	default:
		return false
	}
}

// Shared source utilities.
func findClosingBrace(lines []string, openLine int) (int, error) {
	depth := 0
	seen := false
	for i := openLine; i < len(lines); i++ {
		for _, ch := range stripLineComment(lines[i]) {
			switch ch {
			case '{':
				depth++
				seen = true
			case '}':
				if !seen {
					// A leading '}' before any '{' on this line belongs to a prior
					// block (e.g. the "} else {" / "} catch" idiom); ignore it.
					continue
				}
				depth--
				if depth == 0 {
					return i, nil
				}
			}
		}
	}
	return -1, errors.New("unclosed brace")
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
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

func trimBeforeBrace(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "{"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}

func stripLineComment(s string) string {
	if idx := strings.Index(s, "//"); idx >= 0 {
		return s[:idx]
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func hash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

// subConfigPath extracts -config/--config from subcommand args (default config.json).
func subConfigPath(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-config" || a == "--config" {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		for _, pre := range []string{"-config=", "--config="} {
			if strings.HasPrefix(a, pre) {
				return strings.TrimPrefix(a, pre)
			}
		}
	}
	return "config.json"
}

// ============================================================================
// External tools (rg / ast-grep / joern) with Docker fallback
// ============================================================================

// runTool runs an external tool, preferring a local binary and falling back to a
// configured Docker image when the binary is absent. JVMArgs is forwarded via
// _JAVA_OPTIONS for memory-hungry tools like Joern.
func runTool(cfg Config, tool string, args ...string) ([]byte, error) {
	if bin, err := exec.LookPath(tool); err == nil {
		cmd := exec.Command(bin, args...)
		cmd.Dir = cfg.Root
		return cmd.CombinedOutput()
	}
	tc, ok := cfg.Tools[tool]
	if !ok || tc.Image == "" {
		return nil, fmt.Errorf("%s not in PATH and no docker image configured (set tools.%s.image)", tool, tool)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("%s not in PATH and docker unavailable for fallback", tool)
	}
	abs, _ := filepath.Abs(cfg.Root)
	dargs := []string{"run", "--rm", "-v", abs + ":/src", "-w", "/src"}
	if tc.JVMArgs != "" {
		dargs = append(dargs, "-e", "_JAVA_OPTIONS="+tc.JVMArgs)
	}
	dargs = append(dargs, tc.Image, tool)
	dargs = append(dargs, args...)
	cmd := exec.Command("docker", dargs...)
	return cmd.CombinedOutput()
}

type grepHit struct {
	File string
	Line int
	Text string
}

// ripgrep searches for a regex under cfg.Root/root using rg, falling back to a
// stdlib regex scan when rg is unavailable. rg exit code 1 (no matches) is not an error.
func ripgrep(cfg Config, pattern, root string) ([]grepHit, error) {
	if _, err := exec.LookPath("rg"); err != nil {
		if tc, ok := cfg.Tools["rg"]; ok && tc.Image != "" {
			if out, derr := runTool(cfg, "rg", "-n", "--no-heading", "--color=never", "-e", pattern, root); derr == nil {
				return parseGrep(out), nil
			}
		}
		return scanRegex(cfg, pattern, root)
	}
	cmd := exec.Command("rg", "-n", "--no-heading", "--color=never", "-e", pattern, root)
	cmd.Dir = cfg.Root
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("rg failed: %w\n%s", err, out)
	}
	return parseGrep(out), nil
}

func parseGrep(out []byte) []grepHit {
	var hits []grepHit
	for _, ln := range strings.Split(string(out), "\n") {
		if ln == "" {
			continue
		}
		p1 := strings.IndexByte(ln, ':')
		if p1 < 0 {
			continue
		}
		rest := ln[p1+1:]
		p2 := strings.IndexByte(rest, ':')
		if p2 < 0 {
			continue
		}
		num, err := strconv.Atoi(rest[:p2])
		if err != nil {
			continue
		}
		hits = append(hits, grepHit{File: filepath.ToSlash(ln[:p1]), Line: num, Text: strings.TrimSpace(rest[p2+1:])})
	}
	return hits
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

func isScannableSource(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".java", ".go", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".kt", ".scala", ".py":
		return true
	default:
		return false
	}
}

// globToRegex converts a shell-glob-ish sink pattern (e.g. *kafka*.send) to a
// regex. '*' matches an identifier/dot run; '.' is literal; other regex meta is escaped.
func globToRegex(g string) string {
	var b strings.Builder
	for _, r := range g {
		switch r {
		case '*':
			b.WriteString(`[A-Za-z0-9_.$]*`)
		case '.':
			b.WriteString(`\.`)
		default:
			if strings.ContainsRune(`+()[]{}^$|?\`, r) {
				b.WriteRune('\\')
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ============================================================================
// Entrypoints / Exitpoints lenses (rg-driven, view-only)
// ============================================================================

type labeledPattern struct {
	Label string
	Regex string
}

// The program ships with NO domain-specific patterns/sinks. They are project-specific
// (e.g. @KafkaListener, *repository*.save) and live entirely in config.json lens params,
// keeping the tool general across stacks.

// parsePatternParam parses "label=regex;label=regex" into labeled patterns.
func parsePatternParam(s string) []labeledPattern {
	var out []labeledPattern
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '='); i > 0 {
			out = append(out, labeledPattern{strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])})
		} else {
			out = append(out, labeledPattern{part, part})
		}
	}
	return out
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

// searchMapProjection runs each labeled pattern via rg and emits a single sorted
// map block plus per-label count facts. Shared by entrypoints and exitpoints.
func searchMapProjection(cfg Config, lens LensConfig, patterns []labeledPattern, tool, mode string) (Projection, error) {
	root := lens.SourceRoot
	if root == "" {
		root = "."
	}
	type row struct {
		file  string
		line  int
		label string
		text  string
	}
	var rows []row
	for _, lp := range patterns {
		hits, err := ripgrep(cfg, lp.Regex, root)
		if err != nil {
			return Projection{}, fmt.Errorf("%s: pattern %q: %w", tool, lp.Label, err)
		}
		for _, h := range hits {
			rows = append(rows, row{h.File, h.Line, lp.Label, h.Text})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].file != rows[j].file {
			return rows[i].file < rows[j].file
		}
		return rows[i].line < rows[j].line
	})
	// Default layout: exact code first, then the file:line locator in a padded second
	// column (meaning first, direction second). params.line_format overrides with
	// {file}/{line}/{label}/{code} placeholders for callers who want the regexp/label.
	tmpl := lens.Params["line_format"]
	var lines []string
	for _, r := range rows {
		if tmpl == "" {
			lines = append(lines, codeLoc(r.text, r.file, r.line))
		} else {
			lines = append(lines, formatRow(tmpl, r.file, r.line, r.label, r.text))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "// no matches under "+root)
	}
	p := Projection{Sync: "view-only"}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: mode, File: "model", Mode: mode, Tool: tool, Lines: lines})
	return p, nil
}

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

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// locCol is the column where the file:line locator sits, so code reads first (meaning)
// and the location lines up as a second column (direction).
const locCol = 140

// codeLoc lays out a row as the exact code left-aligned, padded to locCol, then the
// "<filename>:<line>" locator. Long code just gets a two-space gap.
func codeLoc(code, file string, line int) string {
	code = strings.TrimRight(code, " \t")
	loc := fmt.Sprintf("%s:%d", filepath.Base(file), line)
	if len(code) >= locCol {
		return code + "  " + loc
	}
	return code + strings.Repeat(" ", locCol-len(code)) + loc
}

// reLocLines reformats the joern scripts' "<line>\t<code>" rows into the code-first /
// file:line layout. Rows without a tab pass through unchanged.
func reLocLines(in []string, file string) []string {
	out := make([]string, 0, len(in))
	for _, ln := range in {
		if tab := strings.IndexByte(ln, '\t'); tab >= 0 {
			out = append(out, codeLoc(ln[tab+1:], file, atoi(ln[:tab])))
		} else {
			out = append(out, ln)
		}
	}
	return out
}

// formatRow substitutes {file}/{line}/{label}/{code} placeholders in a line template.
func formatRow(tmpl, file string, line int, label, code string) string {
	r := strings.NewReplacer(
		"{file}", file,
		"{line}", strconv.Itoa(line),
		"{label}", label,
		"{code}", code,
	)
	return r.Replace(tmpl)
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ============================================================================
// Control-flow lens: ways from method entry to a target line, branch per file
// ============================================================================

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

// RunJoernControlFlow runs control-flow.sc to enumerate real CFG paths and renders them
// as branch-per-file projections (an index plus one file per path), matching the lexical
// lens's output shape. Handles else-if chains, switch, and loops.
func RunJoernControlFlow(cfg Config, lens LensConfig, file string, line int, method string) (Projection, error) {
	outRel := filepath.ToSlash(filepath.Join(cfg.ProjectionsDir, ".joern-control-flow.jsonl"))
	if err := os.MkdirAll(filepath.Join(cfg.Root, cfg.ProjectionsDir), 0755); err != nil {
		return Projection{}, err
	}
	kv := map[string]string{
		"root":         lens.SourceRoot,
		"targetFile":   file,
		"targetLine":   strconv.Itoa(line),
		"targetMethod": method,
		"out":          outRel,
		"maxPaths":     coalesce(lens.Params["max_branches"], "32"),
	}
	if err := runJoernQuery(cfg, lens, "control-flow.sc", outRel, kv, os.Stderr); err != nil {
		return Projection{}, fmt.Errorf("control-flow mode=joern: %w", err)
	}
	jsonLens := lens
	jsonLens.Input = outRel
	jsonLens.Analyzer = "jsonl"
	flat, err := AnalyzeJSONL(cfg, jsonLens)
	if err != nil {
		return Projection{}, err
	}

	stem := strings.TrimSuffix(LensOut(cfg, lens), ".projection")
	p := Projection{Sync: "view-only"}
	var indexLines []string
	branchNo := 0
	for _, blk := range flat.Blocks {
		branchNo++
		rel := filepath.Base(stem) + fmt.Sprintf(".branch-%d.projection", branchNo)
		indexLines = append(indexLines, fmt.Sprintf("branch %d -> %s", branchNo, rel))
		branch := Projection{Sync: "view-only"}
		branch.Blocks = append(branch.Blocks, ProjectionBlock{
			ID: fmt.Sprintf("branch-%d", branchNo), File: file, Mode: "cfg-path", Tool: "control-flow:joern",
			Lines: reLocLines(blk.Lines, file),
		})
		p.Extra = append(p.Extra, ExtraFile{Path: fmt.Sprintf("%s.branch-%d.projection", stem, branchNo), Proj: branch})
	}
	if branchNo == 0 {
		indexLines = append(indexLines, fmt.Sprintf("// joern found no CFG path to %s:%d", file, line))
		for _, f := range flat.Facts {
			indexLines = append(indexLines, "// "+f.Text)
		}
	}
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "control-flow", File: file, Mode: "index", Tool: "control-flow:joern", Lines: indexLines})
	return p, nil
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

type unrollLine struct {
	code string
	file string
	line int
	// guards is the conjunction of branch conditions that must hold to reach this
	// line — the line's "assumptions" (e.g. ["score >= 0", "!(score >= 90)"]).
	guards []string
}

type inlineCallChoice struct {
	ID       string `json:"id"`       // file:line of the call site
	Name     string `json:"name"`     // called method/function name
	Origin   string `json:"origin"`   // source_root-relative file:line
	Expanded bool   `json:"expanded"` // currently expanded inline
	Depth    int    `json:"depth"`    // caller nesting depth, entry body is 0
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

type javaUnroller struct {
	cfg          Config
	lens         LensConfig
	env          map[string]string
	decisions    []string
	seenDecision map[string]bool
	// Branch-select mode (UI only; CLI/benchmark leave selectMode=false so the
	// undecidable "show both branches" behavior below is unchanged). When on, an
	// undecidable conditional collapses to one side — forced[id] if the user toggled
	// it, else the longest branch — and is recorded in choices so the UI can offer a
	// per-conditional toggle. id is "file:line" of the `if`.
	selectMode  bool
	forced      map[string]string
	choices     []branchChoice
	choiceSeen  map[string]bool
	inlineDepth int
	inlineSkips map[string]bool
	calls       []inlineCallChoice
	callSeen    map[string]bool
}

// branchChoice describes one undecidable conditional the UI can toggle. Sides are
// the selectable options ("then"+"else", or "then"+"skip" when there is no else).
type branchChoice struct {
	ID     string   `json:"id"`     // file:line of the if header
	Cond   string   `json:"cond"`   // the condition text
	Origin string   `json:"origin"` // source_root-relative file:line
	Side   string   `json:"side"`   // currently shown side
	Sides  []string `json:"sides"`  // available sides
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

// withGuard returns a fresh copy of the guard stack with one more condition pushed,
// so sibling branches never alias each other's assumptions.
func withGuard(guards []string, cond string) []string {
	g := make([]string, 0, len(guards)+1)
	g = append(g, guards...)
	return append(g, cond)
}

var uiExitStmtRE = regexp.MustCompile(`^\s*(return|throw|break|continue)\b`)

// rangeExits reports whether the last meaningful statement in lines[lo..hi] is an
// early exit (return/throw/break/continue) — i.e. a guard clause whose fall-through
// implies the negation of its condition for everything after it.
func rangeExits(lines []string, lo, hi int) bool {
	for i := hi; i >= lo && i < len(lines); i-- {
		t := strings.TrimSpace(stripLineComment(lines[i]))
		if t == "" || t == "{" || t == "}" || strings.HasPrefix(t, "//") {
			continue
		}
		return uiExitStmtRE.MatchString(t)
	}
	return false
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

// When a call is inlined into an assignment (`x = helper(...)`), the helper's
// `return E;` should read as `x = E;` in the flattened program — a bare `return`
// looks like the outer method exits early and misleads readers (and models).
var inlineLHSRE = regexp.MustCompile(`^(?:final\s+)?(?:[A-Za-z_][\w<>\[\].]*\s+)?([A-Za-z_]\w*)\s*=[^=]`)
var inlineRetRE = regexp.MustCompile(`^(\s*)return\s+(.*?);\s*$`)

func inlineAssignTarget(trim string) string {
	if m := inlineLHSRE.FindStringSubmatch(trim); m != nil {
		return m[1]
	}
	return ""
}

func rewriteInlinedReturns(lines []unrollLine, lhs string) {
	if lhs == "" {
		return
	}
	for i := range lines {
		if m := inlineRetRE.FindStringSubmatch(lines[i].code); m != nil {
			lines[i].code = m[1] + lhs + " = " + m[2] + ";"
		}
	}
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

type goUnroller struct {
	cfg         Config
	lens        LensConfig
	functions   map[string]goFuncRef
	inlineDepth int
	inlineSkips map[string]bool
	calls       []inlineCallChoice
	callSeen    map[string]bool
}

type goFuncRef struct {
	file string
	fn   GoFunc
}

func AnalyzeGoUnrolledProgram(cfg Config, lens LensConfig) (Projection, error) {
	file := lens.Params["file"]
	method := lens.Params["method"]
	if file == "" {
		return Projection{}, errors.New("unrolled-program go: params.file is required")
	}
	if method == "" {
		return Projection{}, errors.New("unrolled-program go: params.method is required")
	}
	u, err := newGoUnroller(cfg, lens)
	if err != nil {
		return Projection{}, err
	}
	lines, err := u.unroll(file, method, 0)
	if err != nil {
		return Projection{}, err
	}
	var body []string
	var origins []LineOrigin
	for _, line := range lines {
		body = append(body, line.code)
		src := filepath.ToSlash(filepath.Join(lens.SourceRoot, line.file))
		origins = append(origins, LineOrigin{SrcFile: src, Line: line.line, SrcHash: hash(line.code + "\n")})
	}
	p := Projection{Sync: "two-way"}
	p.Blocks = append(p.Blocks, ProjectionBlock{
		ID: method, File: file, Mode: "unrolled", Tool: "unrolled-program:go",
		Lines: body, LineOrigins: origins, Sync: "two-way",
	})
	p.Facts = append(p.Facts, ProjectionFact{ID: "scope", Tool: "unrolled-program", Text: "go adapter: editable straight-line function path; each line syncs back to its original source line"})
	for i, c := range u.calls {
		if b, err := json.Marshal(c); err == nil {
			p.Facts = append(p.Facts, ProjectionFact{ID: fmt.Sprintf("call-%d", i+1), Tool: "unrolled-program", Text: string(b)})
		}
	}
	return p, nil
}

func newGoUnroller(cfg Config, lens LensConfig) (*goUnroller, error) {
	files, err := scanGoFiles(cfg, lens)
	if err != nil {
		return nil, err
	}
	u := &goUnroller{
		cfg: cfg, lens: lens, functions: map[string]goFuncRef{},
		inlineDepth: parseInlineDepth(lens.Params["inline_depth"]),
		inlineSkips: parseIDSet(lens.Params["inline_skips"]),
		callSeen:    map[string]bool{},
	}
	for _, f := range files {
		rel := strings.TrimPrefix(strings.TrimPrefix(f.Rel, filepath.ToSlash(lens.SourceRoot)+"/"), "./")
		for _, fn := range f.Funcs {
			u.functions[fn.Name] = goFuncRef{file: rel, fn: fn}
		}
	}
	return u, nil
}

func (u *goUnroller) unroll(file, name string, depth int) ([]unrollLine, error) {
	if depth > 10 {
		return nil, fmt.Errorf("unrolled-program go: recursion limit while inlining %s", name)
	}
	ref, ok := u.functions[name]
	if !ok {
		return nil, fmt.Errorf("unrolled-program go: function %q not found", name)
	}
	if file != "" && ref.file != file {
		if byFile, ok := u.findGoFunc(file, name); ok {
			ref = byFile
		}
	}
	path := filepath.Join(u.cfg.Root, u.lens.SourceRoot, filepath.FromSlash(ref.file))
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	var out []unrollLine
	for i := ref.fn.Line; i <= ref.fn.End-2 && i < len(lines); i++ {
		raw := lines[i]
		trim := strings.TrimSpace(stripLineComment(raw))
		if trim == "" || trim == "{" || trim == "}" {
			continue
		}
		if called := simpleGoCall(trim); called != "" && called != name {
			if _, ok := u.functions[called]; ok {
				expanded := depth < u.inlineDepth && !u.inlineSkips[fmt.Sprintf("%s:%d", ref.file, i+1)]
				u.recordCall(ref.file, i+1, called, expanded, depth)
				if !expanded {
					out = append(out, unrollLine{code: strings.TrimRight(raw, " \t"), file: ref.file, line: i + 1})
					continue
				}
				part, err := u.unroll("", called, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, part...)
				continue
			}
		}
		out = append(out, unrollLine{code: strings.TrimRight(raw, " \t"), file: ref.file, line: i + 1})
	}
	return out, nil
}

func (u *goUnroller) recordCall(file string, line int, name string, expanded bool, depth int) {
	id := fmt.Sprintf("%s:%d", file, line)
	if u.callSeen[id] {
		return
	}
	u.callSeen[id] = true
	origin := filepath.ToSlash(filepath.Join(u.lens.SourceRoot, file)) + fmt.Sprintf(":%d", line)
	u.calls = append(u.calls, inlineCallChoice{ID: id, Name: name, Origin: origin, Expanded: expanded, Depth: depth})
}

func (u *goUnroller) findGoFunc(file, name string) (goFuncRef, bool) {
	for _, ref := range u.functions {
		if ref.file == file && ref.fn.Name == name {
			return ref, true
		}
	}
	return goFuncRef{}, false
}

func simpleGoCall(trim string) string {
	if strings.HasPrefix(trim, "return ") {
		trim = strings.TrimSpace(strings.TrimPrefix(trim, "return "))
	}
	if strings.Contains(trim, ":=") {
		_, rhs, _ := strings.Cut(trim, ":=")
		trim = strings.TrimSpace(rhs)
	} else if strings.Contains(trim, "=") {
		_, rhs, _ := strings.Cut(trim, "=")
		trim = strings.TrimSpace(rhs)
	}
	m := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*\(`).FindStringSubmatch(trim)
	if m == nil {
		return ""
	}
	return m[1]
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

func isIfHeader(trim string) bool {
	return strings.HasPrefix(trim, "if ") || strings.HasPrefix(trim, "if(")
}

func isExitStmt(trim string) bool {
	return strings.HasPrefix(trim, "return") || strings.HasPrefix(trim, "throw")
}

func firstBraceLine(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.Contains(stripLineComment(lines[i]), "{") {
			return i
		}
	}
	return from
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

func matchParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
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

func enumeratePaths(nodes []cfgNode, target int) []cfgPath {
	var out []cfgPath
	for _, p := range walkNodes(nodes, 0, target) {
		if p.reached && !p.dead {
			out = append(out, p)
		}
	}
	return out
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

func prependEvent(ev cfgEvent, rest []cfgEvent) []cfgEvent {
	out := make([]cfgEvent, 0, len(rest)+1)
	out = append(out, ev)
	out = append(out, rest...)
	return out
}

func concatEvents(head cfgEvent, mid, tail []cfgEvent) []cfgEvent {
	out := make([]cfgEvent, 0, len(mid)+len(tail)+1)
	out = append(out, head)
	out = append(out, mid...)
	out = append(out, tail...)
	return out
}

// ============================================================================
// Data-flow lens: contributing lines with trailing padded comments (view-only)
// ============================================================================

const dataFlowCommentCol = 56

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

func dataFlowNote(why, varName string) string {
	switch why {
	case "method signature":
		return "<- source: parameter feeding " + varName
	case "assignment":
		return "<- assigns into the flow"
	case "object mutation":
		return "<- mutates " + varName
	case "reachability condition":
		return "<- guards whether the value is set"
	case "early return":
		return "<- could skip this value"
	case "target line":
		return "<- final use of " + varName
	default:
		return "<- " + why
	}
}

// padComment right-pads code to a fixed column then appends a trailing // comment,
// so contributing lines stay scannable. Tabs count as 4 columns for alignment.
func padComment(code, note string) string {
	width := 0
	for _, r := range code {
		if r == '\t' {
			width += 4
		} else {
			width++
		}
	}
	pad := dataFlowCommentCol - width
	if pad < 1 {
		pad = 1
	}
	return code + strings.Repeat(" ", pad) + "// " + note
}

// ============================================================================
// Extract lens + two-way sync engine
// ============================================================================

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

// makeBookmarkProjection builds a two-way bookmark over source span a-b. Shared by the
// bookmark analyzer and the single-line drop-in expander.
func makeBookmarkProjection(cfg Config, sourceRoot, file string, a, b int) (Projection, error) {
	path := filepath.Join(cfg.Root, sourceRoot, filepath.FromSlash(file))
	lines, err := readLines(path)
	if err != nil {
		return Projection{}, err
	}
	if a < 1 || b < a || b > len(lines) {
		return Projection{}, fmt.Errorf("bookmark: invalid line range %d-%d for %s (%d lines)", a, b, file, len(lines))
	}
	span := append([]string{}, lines[a-1:b]...)
	srcRel := filepath.ToSlash(filepath.Join(sourceRoot, file))
	p := Projection{Sync: "two-way"}
	p.Blocks = append(p.Blocks, ProjectionBlock{
		ID: fmt.Sprintf("%s:%d-%d", filepath.Base(file), a, b), File: file, Mode: "bookmark", Tool: "bookmark",
		Lines: span, Sync: "two-way", SrcFile: srcRel, SrcStart: a, SrcEnd: b, SrcHash: hash(strings.Join(span, "\n") + "\n"),
	})
	p.Facts = append(p.Facts, ProjectionFact{ID: "sync", Tool: "bookmark", Text: "two-way: edits inside the block sync back to source on `watch` or `sync`"})
	return p, nil
}

// ============================================================================
// Single-line drop-in bookmarks
// ============================================================================

var dropInRE = regexp.MustCompile(`^([A-Za-z0-9_./\-]+\.[A-Za-z0-9]+):(\d+)(?:-(\d+))?$`)

// expandDropIns scans the projections dir for "drop-in" files — a freshly created
// .projection whose only content is a single `path/File.ext:line` (or `:a-b`) reference —
// and expands each in place into a full two-way bookmark with proper headers. Idempotent:
// once expanded the file has a header, so it is not matched again.
func expandDropIns(cfg Config) ([]string, error) {
	dir := filepath.Join(cfg.Root, cfg.ProjectionsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var expanded []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".projection") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ref, a, b, ok := parseDropIn(path)
		if !ok {
			continue
		}
		sourceRoot, rel, err := resolveSourceFile(cfg, ref)
		if err != nil {
			fmt.Fprintf(os.Stderr, "drop-in %s: %v\n", e.Name(), err)
			continue
		}
		proj, err := makeBookmarkProjection(cfg, sourceRoot, rel, a, b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "drop-in %s: %v\n", e.Name(), err)
			continue
		}
		proj.Lens = LensConfig{Name: strings.TrimSuffix(e.Name(), ".projection"), Analyzer: "bookmark", SourceRoot: sourceRoot}
		finalizeProjection(&proj, proj.Lens)
		if err := RenderProjection(path, proj); err != nil {
			return expanded, err
		}
		expanded = append(expanded, path)
	}
	return expanded, nil
}

// parseDropIn returns the reference if the file's only meaningful content is a single
// `path:line` / `path:a-b` line (and it has no generated header yet).
func parseDropIn(path string) (ref string, a, b int, ok bool) {
	lines, err := readLines(path)
	if err != nil {
		return "", 0, 0, false
	}
	var content []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "@@") {
			return "", 0, 0, false // already a rendered projection
		}
		content = append(content, t)
	}
	if len(content) != 1 {
		return "", 0, 0, false
	}
	m := dropInRE.FindStringSubmatch(content[0])
	if m == nil {
		return "", 0, 0, false
	}
	a = atoi(m[2])
	b = a
	if m[3] != "" {
		b = atoi(m[3])
	}
	if b < a {
		a, b = b, a
	}
	return m[1], a, b, true
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

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func parseLineRange(s string) (int, int) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '-'); i > 0 {
		return atoi(s[:i]), atoi(s[i+1:])
	}
	n := atoi(s)
	return n, n
}

// ParsedBlock is a block recovered from an existing projection file, including
// the source anchor metadata needed for two-way sync.
type ParsedBlock struct {
	Anchor   string
	File     string
	ID       string
	Tool     string
	Mode     string
	Hash     string
	Sync     string
	SrcFile  string
	SrcStart int
	SrcEnd   int
	SrcHash  string
	Lines    []string
	Origins  []LineOrigin
}

var anchorRE = regexp.MustCompile(`^@@ (.+?)#(.+?) \[([^.]+)\.([^ \]]+) hash=([0-9a-f]+)(?: sync=two-way src=(.+?):(\d+)-(\d+) srchash=([0-9a-f]+))?\]$`)
var originFactRE = regexp.MustCompile(`^=> (.+?): origin (\d+) src=(.+):(\d+) srchash=([0-9a-f]+)$`)

// parseProjectionFile recovers blocks (with anchor metadata) from a rendered
// projection so the sync engine can detect edits and map them back to source.
func parseProjectionFile(path string) ([]ParsedBlock, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	var blocks []ParsedBlock
	for i := 0; i < len(lines); i++ {
		m := anchorRE.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		blk := ParsedBlock{Anchor: lines[i], File: m[1], ID: m[2], Tool: m[3], Mode: m[4], Hash: m[5]}
		if m[6] != "" {
			blk.Sync = "two-way"
			blk.SrcFile = m[6]
			blk.SrcStart = atoi(m[7])
			blk.SrcEnd = atoi(m[8])
			blk.SrcHash = m[9]
		}
		j := i + 1
		for j < len(lines) && lines[j] != "@@" {
			blk.Lines = append(blk.Lines, lines[j])
			j++
		}
		for k := j + 1; k < len(lines); k++ {
			if strings.HasPrefix(lines[k], "@@ ") {
				break
			}
			mm := originFactRE.FindStringSubmatch(lines[k])
			if mm == nil || mm[1] != blk.ID {
				if strings.TrimSpace(lines[k]) == "" {
					continue
				}
				continue
			}
			idx := atoi(mm[2])
			if idx <= 0 {
				continue
			}
			for len(blk.Origins) < idx {
				blk.Origins = append(blk.Origins, LineOrigin{})
			}
			blk.Origins[idx-1] = LineOrigin{SrcFile: mm[3], Line: atoi(mm[4]), SrcHash: mm[5]}
			blk.Sync = "two-way"
		}
		blocks = append(blocks, blk)
		i = j
	}
	return blocks, nil
}

// SyncResult reports what SyncProjection did for one projection file.
type SyncResult struct {
	ToProjection int
	ToSource     int
	Conflicts    []string
}

// SyncProjection reconciles a two-way projection file with its source files. For
// each two-way block: if only the source changed, refresh the projection; if only
// the projection changed, write it back to source; if both changed, report a conflict.
func SyncProjection(cfg Config, projPath string) (SyncResult, error) {
	var res SyncResult
	blocks, err := parseProjectionFile(projPath)
	if err != nil {
		return res, err
	}
	// Group edits to source per file; apply with running offset for line-count drift.
	for _, blk := range blocks {
		if len(blk.Origins) > 0 {
			r, err := syncScatteredBlock(cfg, blk)
			if err != nil {
				return res, err
			}
			res.ToProjection += r.ToProjection
			res.ToSource += r.ToSource
			res.Conflicts = append(res.Conflicts, r.Conflicts...)
			continue
		}
		if blk.Sync != "two-way" || blk.SrcFile == "" {
			continue
		}
		srcPath := filepath.Join(cfg.Root, filepath.FromSlash(blk.SrcFile))
		srcLines, err := readLines(srcPath)
		if err != nil {
			return res, err
		}
		if blk.SrcStart < 1 || blk.SrcEnd > len(srcLines) || blk.SrcEnd < blk.SrcStart {
			res.Conflicts = append(res.Conflicts, fmt.Sprintf("%s: stale range %d-%d", blk.SrcFile, blk.SrcStart, blk.SrcEnd))
			continue
		}
		liveSpan := srcLines[blk.SrcStart-1 : blk.SrcEnd]
		liveHash := hash(strings.Join(liveSpan, "\n") + "\n")
		projHash := hash(strings.Join(blk.Lines, "\n") + "\n")

		projEdited := projHash != blk.Hash
		srcChanged := liveHash != blk.SrcHash

		// Header retarget: the user edited the anchor's file/class (before #) or the a-b
		// range in the ID. Re-extract the view for the new target on save.
		wantFile, wantA, wantB := bookmarkTarget(cfg, blk)
		if wantFile != blk.SrcFile || wantA != blk.SrcStart || wantB != blk.SrcEnd {
			res.ToProjection++
			continue
		}

		switch {
		case projEdited && srcChanged:
			res.Conflicts = append(res.Conflicts, fmt.Sprintf("%s#%s: both source and projection changed", blk.File, blk.ID))
		case projEdited && !srcChanged:
			// projection -> source
			newLines := append([]string{}, srcLines[:blk.SrcStart-1]...)
			newLines = append(newLines, blk.Lines...)
			newLines = append(newLines, srcLines[blk.SrcEnd:]...)
			if err := writeLines(srcPath, newLines); err != nil {
				return res, err
			}
			res.ToSource++
		case !projEdited && srcChanged:
			res.ToProjection++
		}
	}
	// If any source was updated or refreshed, rebuild the projection's two-way blocks
	// from current source so anchors (hash/srchash) are consistent again — this keeps
	// round-trips clean and works for both configured bookmarks and drop-ins (no lens).
	if res.ToSource > 0 || res.ToProjection > 0 {
		if err := refreshTwoWayProjection(cfg, projPath); err != nil {
			return res, err
		}
	}
	return res, nil
}

func syncScatteredBlock(cfg Config, blk ParsedBlock) (SyncResult, error) {
	var res SyncResult
	if len(blk.Origins) != len(blk.Lines) {
		res.Conflicts = append(res.Conflicts, fmt.Sprintf("%s#%s: projection line count changed; scattered sync only supports one edited line per origin", blk.File, blk.ID))
		return res, nil
	}
	type change struct {
		line int
		text string
	}
	changes := map[string][]change{}
	for i, origin := range blk.Origins {
		if origin.SrcFile == "" || origin.Line <= 0 {
			res.Conflicts = append(res.Conflicts, fmt.Sprintf("%s#%s line %d: missing origin", blk.File, blk.ID, i+1))
			continue
		}
		srcPath := filepath.Join(cfg.Root, filepath.FromSlash(origin.SrcFile))
		srcLines, err := readLines(srcPath)
		if err != nil {
			return res, err
		}
		if origin.Line > len(srcLines) {
			res.Conflicts = append(res.Conflicts, fmt.Sprintf("%s:%d: stale origin", origin.SrcFile, origin.Line))
			continue
		}
		liveHash := hash(srcLines[origin.Line-1] + "\n")
		projHash := hash(blk.Lines[i] + "\n")
		projEdited := projHash != origin.SrcHash
		srcChanged := liveHash != origin.SrcHash
		switch {
		case projEdited && srcChanged:
			res.Conflicts = append(res.Conflicts, fmt.Sprintf("%s:%d: both source and projection changed", origin.SrcFile, origin.Line))
		case projEdited && !srcChanged:
			changes[srcPath] = append(changes[srcPath], change{line: origin.Line, text: blk.Lines[i]})
		case !projEdited && srcChanged:
			res.ToProjection++
		}
	}
	for srcPath, cs := range changes {
		srcLines, err := readLines(srcPath)
		if err != nil {
			return res, err
		}
		for _, c := range cs {
			if c.line < 1 || c.line > len(srcLines) {
				res.Conflicts = append(res.Conflicts, fmt.Sprintf("%s:%d: stale origin", srcPath, c.line))
				continue
			}
			srcLines[c.line-1] = c.text
			res.ToSource++
		}
		if err := writeLines(srcPath, srcLines); err != nil {
			return res, err
		}
	}
	return res, nil
}

// refreshTwoWayProjection re-reads the source span of every two-way block in a projection
// and rewrites the file with consistent anchors. View-only blocks are preserved verbatim.
// Lens-independent, so it serves configured bookmark lenses and drop-in bookmarks alike.
func refreshTwoWayProjection(cfg Config, projPath string) error {
	name, analyzer, sync := readHeaderMeta(projPath)
	blocks, err := parseProjectionFile(projPath)
	if err != nil {
		return err
	}
	p := Projection{Sync: coalesce(sync, "two-way"), Lens: LensConfig{Name: name, Analyzer: coalesce(analyzer, "bookmark")}}
	for _, blk := range blocks {
		nb := ProjectionBlock{ID: blk.ID, File: blk.File, Tool: blk.Tool, Mode: blk.Mode, Lines: blk.Lines, Sync: blk.Sync}
		if len(blk.Origins) > 0 {
			nb.LineOrigins = make([]LineOrigin, 0, len(blk.Origins))
			nb.Lines = nb.Lines[:0]
			for _, origin := range blk.Origins {
				srcLines, err := readLines(filepath.Join(cfg.Root, filepath.FromSlash(origin.SrcFile)))
				if err != nil {
					return err
				}
				if origin.Line < 1 || origin.Line > len(srcLines) {
					nb.Lines = append(nb.Lines, "")
					nb.LineOrigins = append(nb.LineOrigins, origin)
					continue
				}
				line := srcLines[origin.Line-1]
				nb.Lines = append(nb.Lines, line)
				nb.LineOrigins = append(nb.LineOrigins, LineOrigin{SrcFile: origin.SrcFile, Line: origin.Line, SrcHash: hash(line + "\n")})
			}
			nb.Sync = "two-way"
		} else if blk.Sync == "two-way" && blk.SrcFile != "" {
			// The header (display file + ID range) is authoritative, so editing it retargets
			// the view. Fall back to the anchor's src= span when the header is unchanged.
			wantFile, wantA, wantB := bookmarkTarget(cfg, blk)
			srcLines, err := readLines(filepath.Join(cfg.Root, filepath.FromSlash(wantFile)))
			if err != nil {
				return err
			}
			if wantA >= 1 && wantB <= len(srcLines) && wantB >= wantA {
				span := append([]string{}, srcLines[wantA-1:wantB]...)
				nb.Lines = span
				nb.Sync = "two-way"
				nb.SrcFile = wantFile
				nb.SrcStart = wantA
				nb.SrcEnd = wantB
				nb.SrcHash = hash(strings.Join(span, "\n") + "\n")
				nb.ID = fmt.Sprintf("%s:%d-%d", filepath.Base(wantFile), wantA, wantB)
			}
		}
		p.Blocks = append(p.Blocks, nb)
	}
	finalizeProjection(&p, p.Lens)
	return RenderProjection(projPath, p)
}

var idRangeRE = regexp.MustCompile(`:(\d+)-(\d+)$`)

// bookmarkTarget derives the requested source target from a two-way block's header: the
// display file (before #, repo- or package-relative) and the a-b range in the ID. This is
// what makes editing the header — the class/file or the line range — refresh the view.
func bookmarkTarget(cfg Config, blk ParsedBlock) (srcFile string, a, b int) {
	a, b = blk.SrcStart, blk.SrcEnd
	if m := idRangeRE.FindStringSubmatch(blk.ID); m != nil {
		a, b = atoi(m[1]), atoi(m[2])
	}
	srcFile = blk.SrcFile
	if root, rel, err := resolveSourceFile(cfg, blk.File); err == nil {
		srcFile = filepath.ToSlash(filepath.Join(root, rel))
	}
	return srcFile, a, b
}

// readHeaderMeta extracts the lens name, analyzer, and sync policy from a projection header.
func readHeaderMeta(projPath string) (name, analyzer, sync string) {
	lines, err := readLines(projPath)
	if err != nil {
		return "", "", ""
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "# lens: "):
			name = strings.TrimPrefix(l, "# lens: ")
		case strings.HasPrefix(l, "# analyzer: "):
			analyzer = strings.TrimPrefix(l, "# analyzer: ")
		case strings.HasPrefix(l, "# sync: "):
			sync = strings.TrimPrefix(l, "# sync: ")
		case !strings.HasPrefix(l, "#") && strings.TrimSpace(l) != "":
			return
		}
	}
	return
}

func writeLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

// ============================================================================
// Interactive menu
// ============================================================================

// RunMenu drives an interactive loop to add views, persisting each new lens to the
// config file so views are reproducible. It is scriptable via piped stdin.
func RunMenu(cfg Config, configPath string, in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	prompt := func(label string) string {
		fmt.Fprintf(out, "%s: ", label)
		s, _ := r.ReadString('\n')
		return strings.TrimSpace(s)
	}
	// Background watch toggle: started/stopped without leaving the menu.
	var watchStop chan struct{}
	defer func() {
		if watchStop != nil {
			close(watchStop)
		}
	}()
	for {
		watchState := "off"
		if watchStop != nil {
			watchState = "on"
		}
		fmt.Fprintln(out, "\nfile-projections menu")
		fmt.Fprintln(out, "  1) regenerate all lenses")
		fmt.Fprintln(out, "  2) add control-flow view (entry -> line)")
		fmt.Fprintln(out, "  3) add data-flow view (variable)")
		fmt.Fprintln(out, "  4) add entrypoints view")
		fmt.Fprintln(out, "  5) add exitpoints view")
		fmt.Fprintln(out, "  6) add bookmark view (two-way)")
		fmt.Fprintf(out, "  7) toggle watch (regenerate + sync on change) [%s]\n", watchState)
		fmt.Fprintln(out, "  8) quit")
		choice := prompt("choice")
		switch choice {
		case "1":
			if err := runAndReport(cfg, cfg.Lenses, out); err != nil {
				fmt.Fprintln(out, "error:", err)
			}
		case "2":
			lens := LensConfig{
				Name:       prompt("name"),
				Analyzer:   "control-flow",
				SourceRoot: prompt("source root"),
				Params:     map[string]string{"file": prompt("file (relative to source root)"), "line": prompt("target line")},
			}
			cfg = addLens(cfg, configPath, lens, out)
		case "3":
			lens := LensConfig{
				Name:       prompt("name"),
				Analyzer:   "data-flow",
				SourceRoot: prompt("source root"),
				Params:     map[string]string{"file": prompt("file (relative to source root)"), "line": prompt("target line"), "var": prompt("variable"), "mode": "fallback"},
			}
			cfg = addLens(cfg, configPath, lens, out)
		case "4":
			lens := LensConfig{
				Name: prompt("name"), Analyzer: "entrypoints", SourceRoot: prompt("source root"),
				Params: map[string]string{"patterns": prompt("patterns (label=regex;label=regex)")},
			}
			cfg = addLens(cfg, configPath, lens, out)
		case "5":
			lens := LensConfig{
				Name: prompt("name"), Analyzer: "exitpoints", SourceRoot: prompt("source root"),
				Params: map[string]string{"sinks": prompt("sinks (comma globs, e.g. *repository*.save,*kafka*.send)")},
			}
			cfg = addLens(cfg, configPath, lens, out)
		case "6":
			lens := LensConfig{
				Name:       prompt("name"),
				Analyzer:   "bookmark",
				SourceRoot: prompt("source root"),
				Params:     map[string]string{"file": prompt("file (relative to source root)"), "lines": prompt("line range a-b")},
			}
			cfg = addLens(cfg, configPath, lens, out)
		case "7":
			if watchStop == nil {
				watchStop = make(chan struct{})
				go RunWatchUntil(cfg, out, watchStop)
				fmt.Fprintln(out, "watch started (background)")
			} else {
				close(watchStop)
				watchStop = nil
				fmt.Fprintln(out, "watch stopped")
			}
		case "8", "q", "quit", "":
			return nil
		default:
			fmt.Fprintln(out, "unknown choice")
		}
	}
}

func addLens(cfg Config, configPath string, lens LensConfig, out io.Writer) Config {
	if lens.Out == "" {
		lens.Out = filepath.Join(cfg.ProjectionsDir, lens.Name+".projection")
	}
	cfg.Lenses = append(cfg.Lenses, lens)
	if err := SaveConfig(configPath, cfg); err != nil {
		fmt.Fprintln(out, "error saving config:", err)
		return cfg
	}
	if err := runAndReport(cfg, []LensConfig{lens}, out); err != nil {
		fmt.Fprintln(out, "error:", err)
	}
	return cfg
}

func runAndReport(cfg Config, lenses []LensConfig, out io.Writer) error {
	sub := cfg
	sub.Lenses = lenses
	results, err := Run(sub, DefaultRegistry())
	if err != nil {
		return err
	}
	for _, p := range results {
		fmt.Fprintf(out, "wrote %s (%d blocks", LensOut(cfg, p.Lens), len(p.Blocks))
		if len(p.Extra) > 0 {
			fmt.Fprintf(out, ", %d branch files", len(p.Extra))
		}
		fmt.Fprintln(out, ")")
	}
	return nil
}

// SaveConfig writes the config back as indented JSON (used when the menu adds a lens).
func SaveConfig(path string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0644)
}

// ============================================================================
// First-run setup wizard
// ============================================================================

// RunWizard is the no-config first-run experience: detect the stack, suggest a source
// folder, offer entry/exit/all-paths lenses and a first bookmark, write config.json,
// generate, and (optionally) drop into watch mode. Scriptable via stdin for tests.
func RunWizard(root, configPath string, in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	ask := func(label, def string) string {
		if def != "" {
			fmt.Fprintf(out, "%s [%s]: ", label, def)
		} else {
			fmt.Fprintf(out, "%s: ", label)
		}
		s, _ := r.ReadString('\n')
		if s = strings.TrimSpace(s); s == "" {
			return def
		}
		return s
	}
	yes := func(label string, def bool) bool {
		hint := "Y/n"
		if !def {
			hint = "y/N"
		}
		fmt.Fprintf(out, "%s [%s]: ", label, hint)
		s, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "":
			return def
		case "y", "yes":
			return true
		default:
			return false
		}
	}

	fmt.Fprintln(out, "No config.json found — let's set up file-projections.")
	fmt.Fprintln(out)

	cfg := Config{Root: root, ProjectionsDir: ".projections", ExcludeDirs: defaultExcludeDirs()}
	scan := scanProject(cfg)
	if scan.total == 0 {
		fmt.Fprintf(out, "No Java/Go/JS/TS source files found under %q. Run this from your project root.\n", root)
		return nil
	}
	fmt.Fprintf(out, "Detected %s.\n", scan.summary())
	lang := scan.dominant()
	sourceRoot := ask("Source folder to analyze", scan.suggestRoot(cfg, lang))

	var lenses []LensConfig
	add := func(l LensConfig) { lenses = append(lenses, l) }

	if yes("Add an entrypoints lens (where control enters)?", true) {
		add(LensConfig{Name: "entrypoints", Out: ".projections/entrypoints.projection", Analyzer: "entrypoints", SourceRoot: sourceRoot,
			Params: map[string]string{"patterns": entrypointPatternsFor(lang)}})
	}
	if yes("Add an exitpoints lens (where control leaves)?", true) {
		add(LensConfig{Name: "exitpoints", Out: ".projections/exitpoints.projection", Analyzer: "exitpoints", SourceRoot: sourceRoot,
			Params: map[string]string{"sinks": exitSinksFor(lang)}})
	}
	allLabel := "Add an all-paths lens (every flow from entrypoints to exitpoints)?"
	if !joernAvailable(cfg) {
		allLabel += " (needs Joern or Docker)"
	}
	if yes(allLabel, false) {
		add(LensConfig{Name: "all-paths", Out: ".projections/all-paths.projection", Analyzer: "entry-to-exit", SourceRoot: sourceRoot,
			Params: map[string]string{"entry": entryRegexFor(lang), "exit": exitRegexFor(lang)}})
		// Record the Joern image so it's visible/editable in config; ensureJoern pulls it
		// on first use. Without Docker, tell the user what's needed.
		cfg.Tools = map[string]ToolConfig{"joern": {Image: defaultJoernImage, JVMArgs: "-Xmx6g"}}
		if _, err := exec.LookPath("joern"); err != nil {
			if _, err := exec.LookPath("docker"); err != nil {
				fmt.Fprintln(out, "  note: all-paths needs Joern or Docker. Install Docker Desktop; the image is pulled automatically on first run.")
			} else {
				fmt.Fprintf(out, "  note: the Joern image (%s, several GB) will be pulled on first run.\n", defaultJoernImage)
			}
		}
	}

	if sb, ok := scan.sampleBookmark(cfg, sourceRoot); ok {
		if yes(fmt.Sprintf("Create your first bookmark from %s (%s:%d-%d, two-way)?", sb.label, sb.file, sb.a, sb.b), true) {
			add(LensConfig{Name: "first-bookmark", Out: ".projections/first-bookmark.projection", Analyzer: "bookmark", SourceRoot: sourceRoot,
				Params: map[string]string{"file": sb.file, "lines": fmt.Sprintf("%d-%d", sb.a, sb.b)}})
		}
	}

	cfg.Lenses = lenses
	if err := SaveConfig(configPath, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n✓ Wrote %s (%d lens%s).\n", configPath, len(lenses), plural(len(lenses)))
	if len(lenses) > 0 {
		if _, err := Run(cfg, DefaultRegistry()); err != nil {
			fmt.Fprintln(out, "  (some lenses errored:", err, ")")
		} else {
			fmt.Fprintf(out, "✓ Generated projections in %s/\n", cfg.ProjectionsDir)
		}
	}
	fmt.Fprintln(out, "\n🎉 All set! Tips:")
	fmt.Fprintln(out, "  • edit a bookmark block and it syncs back to source on save (under watch)")
	fmt.Fprintln(out, "  • paste `pkg/Foo.java:17` into a new .projection file for an instant bookmark")
	fmt.Fprintln(out, "  • run `file-projections menu` to add control-flow / data-flow views")

	if yes("\nStart watch mode now (regenerate + sync on save)?", true) {
		fmt.Fprintln(out, "watching for changes (Ctrl-C to stop)...")
		return RunWatchUntil(cfg, out, nil)
	}
	fmt.Fprintln(out, "Done. Re-run `file-projections` to generate, or `file-projections watch`.")
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
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

// Language-appropriate defaults for the wizard's suggested lenses.
func entrypointPatternsFor(lang string) string {
	switch lang {
	case "java":
		return "kafka-listener=@KafkaListener;scheduled=@Scheduled;event-listener=@EventListener;http-mapping=@(Get|Post|Put|Delete|Patch|Request)Mapping"
	case "go":
		return `http-handler=func .*http\.ResponseWriter;route=\.(GET|POST|PUT|DELETE|HandleFunc)\(`
	default:
		return `route=\.(get|post|put|delete)\(;listener=addEventListener\(;handler=\.on\(`
	}
}

func exitSinksFor(lang string) string {
	switch lang {
	case "go":
		return "*repo*.Save,*.Exec,*.Publish"
	default: // java + js share the bean-ish convention well enough
		return "*repository*.save,*kafka*.send,*.publish"
	}
}

func entryRegexFor(lang string) string {
	switch lang {
	case "java":
		return "@(KafkaListener|Scheduled|EventListener|PostMapping|GetMapping)"
	default:
		return "@(KafkaListener|Scheduled|PostMapping|GetMapping)"
	}
}

func exitRegexFor(lang string) string {
	return `\.(save|send|publish|Save|Exec)\s*\(`
}

// ============================================================================
// Watch mode
// ============================================================================

// RunWatch blocks, watching until the process is interrupted (CLI `watch`).
func RunWatch(cfg Config) error {
	fmt.Println("watching for changes (Ctrl-C to stop)...")
	return RunWatchUntil(cfg, os.Stdout, nil)
}

// RunWatchUntil regenerates projections when source files change and syncs two-way
// projection edits back to source when they are edited. It polls mtimes (stdlib)
// until stop is closed; pass nil to run forever. Used both by the CLI and the menu's
// background watch toggle.
func RunWatchUntil(cfg Config, out io.Writer, stop <-chan struct{}) error {
	mtimes := map[string]time.Time{}
	snapshot := func() map[string]time.Time {
		m := map[string]time.Time{}
		for _, lens := range cfg.Lenses {
			base := filepath.Join(cfg.Root, lens.SourceRoot)
			filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if shouldSkipDir(cfg, p, d) {
					return filepath.SkipDir
				}
				if !d.IsDir() && isScannableSource(p) {
					if info, err := d.Info(); err == nil {
						m[p] = info.ModTime()
					}
				}
				return nil
			})
		}
		// Track every .projection in the projections dir so drop-ins and non-lens
		// two-way bookmarks are watched too.
		projDir := filepath.Join(cfg.Root, cfg.ProjectionsDir)
		if entries, err := os.ReadDir(projDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".projection") {
					if info, err := e.Info(); err == nil {
						m["proj:"+filepath.Join(projDir, e.Name())] = info.ModTime()
					}
				}
			}
		}
		return m
	}
	mtimes = snapshot()
	for {
		select {
		case <-stop:
			return nil
		case <-time.After(time.Second):
		}
		cur := snapshot()
		srcChanged := false
		for k, t := range cur {
			if strings.HasPrefix(k, "proj:") {
				continue
			}
			if old, ok := mtimes[k]; !ok || !old.Equal(t) {
				srcChanged = true
				fmt.Fprintln(out, "source changed:", k)
			}
		}
		// Expand any freshly dropped-in single-line bookmarks (path:line) into full
		// two-way bookmarks. New files appear in cur but not mtimes.
		for key := range cur {
			if !strings.HasPrefix(key, "proj:") {
				continue
			}
			if _, known := mtimes[key]; !known {
				if expanded, err := expandDropIns(cfg); err == nil {
					for _, p := range expanded {
						fmt.Fprintln(out, "expanded drop-in bookmark:", p)
					}
				}
				break
			}
		}
		// Two-way projection edits -> sync back to source (any .projection in the dir).
		for key, t := range cur {
			if !strings.HasPrefix(key, "proj:") {
				continue
			}
			projPath := strings.TrimPrefix(key, "proj:")
			old, ok := mtimes[key]
			if !ok || old.Equal(t) || !hasTwoWayBlock(projPath) {
				continue
			}
			if r, err := SyncProjection(cfg, projPath); err == nil && (r.ToSource > 0 || len(r.Conflicts) > 0) {
				fmt.Fprintf(out, "synced %s: %d->source, conflicts=%v\n", projPath, r.ToSource, r.Conflicts)
			}
		}
		if srcChanged {
			// Incremental CPG refresh for joern lenses (skips unchanged roots).
			if joernAvailable(cfg) && len(joernSourceRoots(cfg)) > 0 {
				if err := RunBuildCPG(cfg); err != nil {
					fmt.Fprintln(out, "cpg refresh error:", err)
				}
			}
			if _, err := Run(cfg, DefaultRegistry()); err != nil {
				fmt.Fprintln(out, "regen error:", err)
			} else {
				fmt.Fprintln(out, "regenerated projections")
			}
		}
		mtimes = snapshot()
	}
}

// hasTwoWayBlock reports whether a projection file contains a two-way (bookmark) block.
func hasTwoWayBlock(projPath string) bool {
	blocks, err := parseProjectionFile(projPath)
	if err != nil {
		return false
	}
	for _, b := range blocks {
		if b.Sync == "two-way" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// `ui` command — a small local web UI (single embedded page, stdlib net/http)
// to edit config.json, preview any lens ad-hoc against the real analyzers, and
// search source symbols so the file/method/line/type params a lens needs can be
// picked from real code instead of guessed. Same registry the CLI uses — the UI
// is a thin shell over ExecuteLens, never a parallel implementation.
// ---------------------------------------------------------------------------

type uiServer struct {
	mu         sync.Mutex
	cfg        Config
	configPath string
	registry   Registry
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

// uiAnalyzerLanguages maps each analyzer to the languages it can meaningfully run
// on, so the UI hides lenses that don't apply to the detected language. "any"
// means language-agnostic (text/data lenses).
func uiAnalyzerLanguages() map[string][]string {
	return map[string][]string{
		"control-flow":      {"java"},
		"data-flow":         {"java"},
		"object-flow":       {"java"},
		"unrolled-program":  {"java"},
		"entry-to-exit":     {"java"},
		"cpg-methods":       {"java"},
		"joern-var-flow":    {"java"},
		"entrypoints":       {"java"},
		"exitpoints":        {"java"},
		"flow":              {"java"},
		"java-post-to-save": {"java"},
		"go-symbols":        {"go"},
		"js-events":         {"js"},
		"jsonl":             {"any"},
		"bookmark":          {"java", "go", "js"},
		"extract":           {"java", "go", "js"},
		"ast-grep":          {"java", "go", "js"},
	}
}

func RunUI(cfg Config, configPath, addr string, out io.Writer) error {
	if configPath == "" {
		configPath = "config.json"
	}
	s := &uiServer{cfg: cfg, configPath: configPath, registry: DefaultRegistry()}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, uiHTML)
	})
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/symbols", s.handleSymbols)
	mux.HandleFunc("/api/vars", s.handleVars)
	mux.HandleFunc("/api/dirs", s.handleDirs)
	mux.HandleFunc("/api/detect", s.handleDetect)
	mux.HandleFunc("/api/unroll", s.handleUnroll)
	mux.HandleFunc("/api/unroll/edit", s.handleUnrollEdit)
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
	analyzers := make([]string, 0, len(s.registry))
	for name := range s.registry {
		analyzers = append(analyzers, name)
	}
	sort.Strings(analyzers)
	writeJSON(w, 200, map[string]any{
		"config":        json.RawMessage(raw),
		"analyzers":     analyzers,
		"applicability": uiAnalyzerLanguages(),
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

func suggestUIMethod(cfg Config, sourceRoot, lang string) (file, method string, line int) {
	base := filepath.Join(cfg.Root, sourceRoot)
	type cand struct {
		file   string
		method string
		score  int
		line   int
	}
	var cands []cand
	preferred := map[string]int{"summary": 100, "main": 90, "handle": 80, "process": 70, "checkout": 60, "build": 50, "run": 40}
	if lang == "go" {
		preferred = map[string]int{"run": 120, "main": 100, "executelens": 90, "analyze": 80, "handle": 70, "build": 60}
	}
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if shouldSkipDir(cfg, path, d) {
			return filepath.SkipDir
		}
		if d.IsDir() || strings.Contains(filepath.ToSlash(path), "/test/") {
			return nil
		}
		lines, err := readLines(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		rel = filepath.ToSlash(rel)
		switch filepath.Ext(path) {
		case ".java":
			if lang != "" && lang != "java" {
				return nil
			}
			methods, err := parseJavaMethods(lines)
			if err != nil {
				return nil
			}
			for _, m := range methods {
				score := preferred[strings.ToLower(m.Name)]
				for _, a := range m.Annotations {
					if strings.Contains(a, "Mapping") || strings.Contains(a, "Listener") || strings.Contains(a, "Scheduled") {
						score += 30
					}
				}
				if strings.Contains(strings.ToLower(filepath.Base(rel)), "controller") {
					score += 10
				}
				cands = append(cands, cand{file: rel, method: m.Name, score: score, line: m.Start})
			}
		case ".go":
			if lang != "" && lang != "go" {
				return nil
			}
			gf, err := parseGoFile(base, path)
			if err != nil {
				return nil
			}
			for _, fn := range gf.Funcs {
				score := preferred[strings.ToLower(fn.Name)]
				if strings.EqualFold(filepath.Base(rel), "main.go") {
					score += 20
				}
				if strings.HasSuffix(fn.Name, "Handler") || strings.HasPrefix(fn.Name, "Handle") {
					score += 15
				}
				cands = append(cands, cand{file: rel, method: fn.Name, score: score, line: fn.Line})
			}
		}
		return nil
	})
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

// suggestUIExamples picks a real line/var/type from the entry file so lenses that
// need file+line/var/type are prefilled with something that actually exists.
var uiJavaLocalRE = regexp.MustCompile(`^\s*(?:final\s+)?[A-Z][A-Za-z0-9_<>\[\].]*\s+([a-z][A-Za-z0-9_]*)\s*=`)
var uiGoLocalRE = regexp.MustCompile(`^\s*([a-z][A-Za-z0-9_]*)\s*:?=`)

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

type uiSymbol struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// handleSymbols searches source under a root for declarations (java classes/methods,
// go funcs/types) matching q — so a user picking control-flow/data-flow/object-flow
// params has the real file:line/type to fill in, the way an MCP symbol search would.
func (s *uiServer) handleSymbols(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	root := r.URL.Query().Get("root")
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	syms, err := collectSymbols(cfg, root, q, 200)
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"symbols": syms})
}

// unrollLineView is one line of the straight-line program plus the real source location it
// came from, so the UI can show "discover branches -> choose inputs -> edit", and each edit
// goes back to its true origin via the same two-way sync the CLI uses.
type unrollLineView struct {
	N      int      `json:"n"`
	Code   string   `json:"code"`
	Origin string   `json:"origin"`           // file:line, the real source the line came from
	Branch bool     `json:"branch"`           // an unresolved `if (...)` / `switch` header
	Guards []string `json:"guards,omitempty"` // conditions that must hold to reach this line
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

// unrollChoices pulls the per-conditional toggle metadata the analyzer recorded.
func unrollChoices(p Projection) []branchChoice {
	var out []branchChoice
	for _, f := range p.Facts {
		if f.Tool == "unrolled-program" && strings.HasPrefix(f.ID, "choice-") {
			var c branchChoice
			if json.Unmarshal([]byte(f.Text), &c) == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

// unrollCalls pulls the per-call inline/collapse metadata the analyzer recorded.
func unrollCalls(p Projection) []inlineCallChoice {
	var out []inlineCallChoice
	for _, f := range p.Facts {
		if f.Tool == "unrolled-program" && strings.HasPrefix(f.ID, "call-") {
			var c inlineCallChoice
			if json.Unmarshal([]byte(f.Text), &c) == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

var uiBranchRE = regexp.MustCompile(`^\s*(if|else if|switch|while|for|case)\b`)

func unrollViewLines(p Projection) []unrollLineView {
	var out []unrollLineView
	for _, b := range p.Blocks {
		for i, code := range b.Lines {
			lv := unrollLineView{N: len(out) + 1, Code: strings.TrimRight(code, " \t"), Branch: uiBranchRE.MatchString(code)}
			if i < len(b.LineOrigins) {
				o := b.LineOrigins[i]
				if o.SrcFile != "" {
					lv.Origin = fmt.Sprintf("%s:%d", o.SrcFile, o.Line)
				}
			}
			if i < len(b.LineGuards) {
				lv.Guards = b.LineGuards[i]
			}
			out = append(out, lv)
		}
	}
	return out
}

func unrollDecisionFacts(p Projection) []string {
	var out []string
	for _, f := range p.Facts {
		if f.Tool == "unrolled-program" && strings.HasPrefix(f.ID, "branch-") {
			out = append(out, f.Text)
		}
	}
	sort.Strings(out)
	return out
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

var uiJavaClassRE = regexp.MustCompile(`^\s*(?:public\s+|final\s+|abstract\s+)*(class|interface|enum|record)\s+([A-Za-z_][A-Za-z0-9_]*)`)
var uiGoDeclRE = regexp.MustCompile(`^func(?:\s+\([^)]*\))?\s+([A-Za-z_][A-Za-z0-9_]*)|^type\s+([A-Za-z_][A-Za-z0-9_]*)`)

func collectSymbols(cfg Config, root, q string, limit int) ([]uiSymbol, error) {
	base := filepath.Join(cfg.Root, root)
	var out []uiSymbol
	match := func(name string) bool { return q == "" || strings.Contains(strings.ToLower(name), q) }
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than abort the whole walk
		}
		if d.IsDir() {
			if shouldSkipDir(cfg, path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= limit {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(base, path)
		rel = filepath.ToSlash(rel)
		ext := filepath.Ext(path)
		switch ext {
		case ".java":
			if match(rel) || match(filepath.Base(rel)) {
				out = append(out, uiSymbol{Name: rel, Kind: "file", File: rel, Line: 1})
			}
			lines, err := readLines(path)
			if err != nil {
				return nil
			}
			for i, l := range lines {
				if m := uiJavaClassRE.FindStringSubmatch(l); m != nil && match(m[2]) {
					out = append(out, uiSymbol{Name: m[2], Kind: m[1], File: rel, Line: i + 1})
				}
			}
			if methods, err := parseJavaMethods(lines); err == nil {
				for _, m := range methods {
					if match(m.Name) {
						out = append(out, uiSymbol{Name: m.Name, Kind: "method", File: rel, Line: m.Start})
					}
				}
			}
		case ".go":
			if match(rel) || match(filepath.Base(rel)) {
				out = append(out, uiSymbol{Name: rel, Kind: "file", File: rel, Line: 1})
			}
			lines, err := readLines(path)
			if err != nil {
				return nil
			}
			for i, l := range lines {
				if m := uiGoDeclRE.FindStringSubmatch(l); m != nil {
					name := m[1]
					kind := "func"
					if name == "" {
						name, kind = m[2], "type"
					}
					if match(name) {
						out = append(out, uiSymbol{Name: name, Kind: kind, File: rel, Line: i + 1})
					}
				}
			}
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File == out[j].File {
			return out[i].Line < out[j].Line
		}
		return out[i].File < out[j].File
	})
	return out, err
}

//go:embed ui.html
var uiHTML string
