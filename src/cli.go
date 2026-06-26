package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CLI entry point: flag parsing and subcommand dispatch.

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
		case "clone":
			must(RunClone(os.Args[2:], os.Stdout))
			return
		case "report":
			must(RunReport(os.Args[2:], os.Stdout))
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
	paramsJSON := flag.String("params-json", "", "extra lens params as a JSON object (e.g. service-graph services/packages)")
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
		if *paramsJSON != "" {
			extra := map[string]any{}
			if err := json.Unmarshal([]byte(*paramsJSON), &extra); err != nil {
				must(fmt.Errorf("-params-json: %w", err))
			}
			for k, v := range extra {
				switch t := v.(type) {
				case string:
					params[k] = t
				default:
					b, _ := json.Marshal(v)
					params[k] = string(b)
				}
			}
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
  clone         shallow-clone a GitHub repo (owner/repo or URL) into a local working dir
  report        bake a service-graph + side-effects + findings into one shareable HTML file
  perf          benchmark all-to-all entry→exit on a repo, with a wall-clock cap  version       print the version
  help          show this help

FLAGS (run one ad-hoc lens without a config)
  -config <path>       config file (default config.json)
  -analyzer <name>     entrypoints|exitpoints|control-flow|data-flow|entry-to-exit|bookmark|flow|ast-grep|joern-var-flow|object-flow|cpg-methods|unrolled-program|postgres-watch
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
  postgres-watch poll Postgres tables by id into a rolling CSV projection
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

// RunClone shallow-clones a GitHub repo (full URL or owner/repo slug) into a local
// working dir and prints the path. Shares clone logic with the UI's /api/clone so every
// surface clones the same way. Default dest keeps clones beside the repo so the UI
// can use the result directly as a source root.
func RunClone(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)
	fs.SetOutput(out)
	dest := fs.String("dest", "workspace/clones", "directory to clone into (relative to cwd)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: file-projections clone <owner/repo | git-url> [-dest dir]")
	}
	url, name, err := normalizeGitURL(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cloning %s (shallow) ...\n", url)
	target, err := cloneRepo(url, name, *dest, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cloned into %s\n", target)
	return nil
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

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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
