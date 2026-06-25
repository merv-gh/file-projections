package main

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Interactive commands: menu, setup wizard, watch.

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
