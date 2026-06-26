package main

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Joern/CPG integration: build, parse, and query the code property graph.

// joernContext bounds the running time of Joern subprocesses. It defaults to no limit; the
// `perf` benchmark sets a deadline so a runaway parse is killed instead of hanging.
var joernContext = context.Background()

// Joern scripts are embedded so the binary is self-sufficient — no tools/ dir needs to
// ship alongside it. They are materialized under <projections_dir>/.joern-scripts/ at run
// time (that path is inside the Docker bind mount, so containerized Joern can read them).
//
//go:embed tools/joern/*.sc
var embeddedJoernScripts embed.FS

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

// joernFrontend returns the language-specific Joern frontend binary for a source root, or
// "" to use the generic joern-parse. Invoking the frontend directly (vs joern-parse) avoids
// spawning a second JVM — what Joern recommends for large/memory-heavy codebases.
func joernFrontend(lang string) string {
	if l := languageByID(lang); l != nil {
		return l.JoernFrontend
	}
	return "" // unknown → joern-parse autodetect
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

// cpgBuildPlan describes how the CPG for a root will be built (for progress logging).
func cpgBuildPlan(cfg Config, sourceRootRel string) (tool string, jflags []string) {
	if fe := joernFrontend(rootLanguage(cfg, sourceRootRel)); fe != "" {
		return fe, frontendJVMFlags(cfg)
	}
	return "joern-parse", nil
}

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
