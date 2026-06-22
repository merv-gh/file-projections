package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type WorkerPool struct {
	queue   chan *Job
	workers int
	dataDir string
}

func NewWorkerPool(workers int, dataDir string) *WorkerPool {
	wp := &WorkerPool{
		queue:   make(chan *Job, 100),
		workers: workers,
		dataDir: dataDir,
	}
	for i := 1; i <= workers; i++ {
		go wp.runWorker(i)
	}
	log.Printf("worker pool started: %d workers", workers)
	return wp
}

func (wp *WorkerPool) Submit(job *Job) {
	wp.queue <- job
}

func (wp *WorkerPool) runWorker(id int) {
	baseName := fmt.Sprintf("joern-worker-%d", id)
	log.Printf("worker %d starting (base name: %s)", id, baseName)

	container := resolveContainerID(baseName)
	log.Printf("worker %d resolved container: %s", id, container)

	// Discover available tools in the container and log prominently
	tools := probeContainer(container)
	if tools.Javasrc2cpg != "" {
		log.Printf("worker %d: WILL USE javasrc2cpg at %s (fast, single JVM)", id, tools.Javasrc2cpg)
	} else {
		log.Printf("worker %d: WARNING javasrc2cpg NOT found, will use joern-parse (slow, double JVM, needs more RAM)", id)
	}
	log.Printf("worker %d tools: joern-parse=%s joern-export=%s stdbuf=%v",
		id, tools.JoernParse, tools.JoernExport, tools.HasStdbuf)

	for job := range wp.queue {
		job.mu.Lock()
		job.Worker = container
		job.mu.Unlock()
		log.Printf("worker %d picked up job %s (%s)", id, job.ID, job.Name)
		wp.processJob(job, container, tools)
	}
}

type ContainerTools struct {
	Javasrc2cpg string // path to javasrc2cpg binary (empty = not found)
	JoernParse  string // path to joern-parse
	JoernExport string // path to joern-export
	HasStdbuf   bool   // stdbuf available for line-buffering
}

// probeContainer discovers all available Joern tools in the worker container.
func probeContainer(container string) ContainerTools {
	var tools ContainerTools

	// List everything joern-related for debugging
	out, _ := dockerExec(container, "sh", "-c",
		"find / -maxdepth 5 \\( -name 'javasrc2cpg*' -o -name 'joern-parse*' -o -name 'joern-export*' \\) -type f 2>/dev/null | head -30")
	if strings.TrimSpace(out) != "" {
		log.Printf("container %s: found joern files:\n%s", container, out)
	}

	// Also check PATH
	pathOut, _ := dockerExec(container, "sh", "-c", "echo $PATH")
	log.Printf("container %s: PATH=%s", container, strings.TrimSpace(pathOut))

	// Find javasrc2cpg - this is critical for performance (single JVM vs double JVM)
	for _, candidate := range []string{
		"javasrc2cpg",
		"javasrc2cpg.sh",
	} {
		out, err := dockerExec(container, "sh", "-c", "command -v "+candidate+" 2>/dev/null")
		if err == nil && strings.TrimSpace(out) != "" {
			tools.Javasrc2cpg = strings.TrimSpace(out)
			break
		}
	}
	if tools.Javasrc2cpg == "" {
		// Search common installation paths
		for _, p := range []string{
			"/opt/joern/joern-cli/frontends/javasrc2cpg/bin/javasrc2cpg",
			"/opt/joern/frontends/javasrc2cpg/bin/javasrc2cpg",
			"/opt/joern/frontends/javasrc2cpg.sh",
		} {
			_, err := dockerExec(container, "test", "-f", p)
			if err == nil {
				tools.Javasrc2cpg = p
				break
			}
		}
	}
	if tools.Javasrc2cpg == "" {
		// Last resort: deep find (slow but thorough)
		out, _ := dockerExec(container, "sh", "-c",
			"find / -maxdepth 6 -name 'javasrc2cpg*' -type f 2>/dev/null | head -5")
		if strings.TrimSpace(out) != "" {
			log.Printf("container %s: javasrc2cpg candidates from deep find:\n%s", container, out)
			// Use the first executable match
			for _, line := range strings.Split(out, "\n") {
				p := strings.TrimSpace(line)
				if p == "" {
					continue
				}
				_, err := dockerExec(container, "test", "-x", p)
				if err == nil {
					tools.Javasrc2cpg = p
					break
				}
			}
		}
	}

	if tools.Javasrc2cpg != "" {
		log.Printf("container %s: javasrc2cpg FOUND at %s", container, tools.Javasrc2cpg)
	} else {
		log.Printf("container %s: javasrc2cpg NOT FOUND anywhere - will use joern-parse (slower, double RAM)", container)
	}

	// Find joern-parse and joern-export
	for _, tool := range []struct {
		name string
		dest *string
	}{
		{"joern-parse", &tools.JoernParse},
		{"joern-export", &tools.JoernExport},
	} {
		out, err := dockerExec(container, "sh", "-c", "command -v "+tool.name+" 2>/dev/null")
		if err == nil && strings.TrimSpace(out) != "" {
			*tool.dest = strings.TrimSpace(out)
		}
	}

	// Check for stdbuf (forces line-buffered output)
	_, err := dockerExec(container, "sh", "-c", "command -v stdbuf 2>/dev/null")
	tools.HasStdbuf = err == nil

	return tools
}

func (wp *WorkerPool) processJob(job *Job, container string, tools ContainerTools) {
	totalStart := time.Now()
	srcDir := filepath.Join(wp.dataDir, "sources", job.ID)
	cpgFile := filepath.Join(wp.dataDir, "cpg", job.ID+".cpg.bin")
	exportDir := filepath.Join(wp.dataDir, "exports", job.ID)
	resultPath := filepath.Join(wp.dataDir, "results", job.ID+".tar.gz")

	os.MkdirAll(filepath.Join(wp.dataDir, "cpg"), 0755)
	os.MkdirAll(filepath.Join(wp.dataDir, "exports"), 0755)
	os.MkdirAll(filepath.Join(wp.dataDir, "results"), 0755)

	logFile := "/data/joern-" + job.ID + ".log"
	containerSrc := "/data/sources/" + job.ID
	containerCpg := "/data/cpg/" + job.ID + ".cpg.bin"
	containerExport := "/data/exports/" + job.ID

	// Clean up any leftovers from previous runs of the same job ID.
	// Both host-side AND container-side paths must be cleaned, because
	// volume mounts may not be perfectly bidirectional or the defer
	// from a crashed previous run may not have executed.
	os.RemoveAll(exportDir)
	os.Remove(resultPath)
	dockerExec(container, "rm", "-rf", containerExport)
	dockerExec(container, "rm", "-f", logFile)
	log.Printf("cleaned leftovers for job %s", job.ID)

	parseDone := false
	defer func() {
		os.RemoveAll(srcDir)
		if !parseDone {
			os.Remove(cpgFile)
			dockerExec(container, "rm", "-f", containerCpg)
		} else {
			log.Printf("keeping CPG at %s (parse succeeded)", containerCpg)
		}
		os.RemoveAll(exportDir)
		dockerExec(container, "rm", "-rf", containerExport)
		dockerExec(container, "rm", "-f", logFile)
	}()

	resolvedSrc := resolveContainerSrcDir(container, containerSrc)
	if resolvedSrc != containerSrc {
		job.addLog(fmt.Sprintf("Resolved source dir: %s", resolvedSrc), "info")
	}

	// Count source files
	countOut, _ := dockerExec(container, "sh", "-c",
		fmt.Sprintf("find '%s' -name '*.java' | wc -l", resolvedSrc))
	fileCount := strings.TrimSpace(countOut)
	job.addLog(fmt.Sprintf("Java source files: %s", fileCount), "info")

	// ── Step 1: Parse ──────────────────────────────────────────────
	job.setStatus("parsing")
	job.setProgress(10)
	start := time.Now()

	// Unset _JAVA_OPTIONS so subprocesses (Delombok, etc.) don't inherit huge heaps.
	// We pass JVM memory directly to the specific tool via -J flags instead.
	jvmFlags := "-J-Xmx4g"

	var parseCmd string
	if tools.Javasrc2cpg != "" {
		job.addLog(fmt.Sprintf("Using javasrc2cpg at %s (single JVM, no overlay passes)", tools.Javasrc2cpg), "info")
		// --delombok-mode no-delombok: skip Delombok subprocess (OOM-prone, not needed for boundary mapping)
		parseCmd = fmt.Sprintf("unset _JAVA_OPTIONS JAVA_TOOL_OPTIONS; %s %s %s --delombok-mode no-delombok -o %s",
			tools.Javasrc2cpg, jvmFlags, resolvedSrc, containerCpg)
	} else if tools.JoernParse != "" {
		job.addLog("WARNING: javasrc2cpg not found, using joern-parse (spawns subprocess, needs more RAM)", "info")
		parseCmd = fmt.Sprintf("unset _JAVA_OPTIONS JAVA_TOOL_OPTIONS; %s %s %s --language javasrc --output %s --frontend-args --delombok-mode no-delombok",
			tools.JoernParse, jvmFlags, resolvedSrc, containerCpg)
	} else {
		job.addLog("FATAL: neither javasrc2cpg nor joern-parse found in container!", "error")
		job.fail("no parse tool found in container")
		return
	}
	job.addLog(fmt.Sprintf("CMD: %s", parseCmd), "info")

	out, err := dockerExecWithProgress(container, job, logFile, parseCmd)

	// Always dump the log file contents to job logs (Joern output often only appears here)
	dumpLogFileToJob(container, job, logFile)

	if err != nil {
		if strings.Contains(err.Error(), "137") || strings.Contains(out, "137") {
			job.addLog("EXIT CODE 137 = OOM KILLED. Container ran out of memory.", "error")
			job.addLog("If using joern-parse (2 JVMs), increase container memory to 8g+ or find javasrc2cpg for single JVM mode.", "error")
		}
		job.addLog("Parse FAILED: "+err.Error(), "error")
		job.fail(fmt.Sprintf("parse failed: %s (status 137 = OOM)", err.Error()))
		return
	}

	// Verify the CPG file actually exists
	_, chkErr := dockerExec(container, "test", "-f", containerCpg)
	if chkErr != nil {
		job.addLog("Parse command succeeded but CPG file not found at "+containerCpg, "error")
		job.fail("CPG file not created")
		return
	}

	parseElapsed := time.Since(start).Round(time.Second)
	cpgSizeOut, _ := dockerExec(container, "sh", "-c",
		fmt.Sprintf("du -sh '%s' 2>/dev/null | cut -f1", containerCpg))
	job.addLog(fmt.Sprintf("Parse done in %s (CPG: %s)",
		parseElapsed, strings.TrimSpace(cpgSizeOut)), "info")
	job.setProgress(50)
	parseDone = true

	// export:false → keep the cpg.bin for download / remote queries; skip export+package.
	if !job.Export {
		job.addLog("export disabled — keeping CPG for download / script queries", "info")
		job.setProgress(100)
		totalElapsed := time.Since(totalStart).Round(time.Second)
		job.addLog(fmt.Sprintf("TOTAL: %s (parse only)", totalElapsed), "info")
		job.addLog("Done", "done")
		job.complete()
		return
	}

	// ── Step 2: Export ─────────────────────────────────────────────
	job.setStatus("exporting")
	start = time.Now()

	// joern-export insists on creating the output directory itself and
	// refuses to run if it already exists. Since the host exportDir and
	// container containerExport are the same path via the shared volume,
	// we must remove from BOTH sides and NOT recreate before joern-export.
	os.RemoveAll(exportDir)
	dockerExec(container, "rm", "-rf", containerExport)

	exported := false

	// Strategy A: Use joern-export (works if CPG has all overlays, i.e. from joern-parse)
	job.addLog("Trying joern-export --format neo4jcsv...", "info")
	exportCmd := fmt.Sprintf("joern-export --format neo4jcsv --repr all -o %s %s",
		containerExport, containerCpg)
	out, err = dockerExecWithProgress(container, job, logFile, exportCmd)
	dumpLogFileToJob(container, job, logFile)

	// If joern-export fails because "Output directory already exists", clean and retry once
	if err != nil && strings.Contains(out, "already exists") {
		job.addLog("Export dir conflict — cleaning and retrying...", "info")
		os.RemoveAll(exportDir)
		dockerExec(container, "rm", "-rf", containerExport)
		time.Sleep(500 * time.Millisecond)
		out, err = dockerExecWithProgress(container, job, logFile, exportCmd)
		dumpLogFileToJob(container, job, logFile)
	}

	if err == nil {
		// Verify files were created
		checkOut, _ := dockerExec(container, "sh", "-c",
			fmt.Sprintf("ls %s/*.csv 2>/dev/null | wc -l", containerExport))
		csvCount, _ := strconv.Atoi(strings.TrimSpace(checkOut))
		if csvCount > 0 {
			exported = true
			job.addLog(fmt.Sprintf("joern-export succeeded (%d CSV files)", csvCount), "info")
		}
	}

	if !exported {
		// Strategy B: Direct flatgraph export via Joern script (no overlay computation).
		// This works when javasrc2cpg created the CPG without overlays.
		job.addLog("joern-export failed (likely missing overlays). Trying direct flatgraph export...", "info")
		os.RemoveAll(exportDir)
		dockerExec(container, "rm", "-rf", containerExport)
		os.MkdirAll(exportDir, 0755)

		exportScript := fmt.Sprintf(`
import io.shiftleft.codepropertygraph.cpgloading.CpgLoader
import flatgraph.formats.neo4jcsv.Neo4jCsvExporter
import java.nio.file.Paths

val cpg = CpgLoader.load("%s")
Neo4jCsvExporter.runExport(cpg.graph, Paths.get("%s"))
cpg.close()
`, containerCpg, containerExport)

		scriptPath := "/data/export-raw-" + job.ID + ".sc"
		// Write script into container
		dockerExec(container, "sh", "-c",
			fmt.Sprintf("cat > %s << 'ENDSCRIPT'\n%s\nENDSCRIPT", scriptPath, exportScript))

		scriptCmd := fmt.Sprintf("joern --script %s --nocolors", scriptPath)
		out, err = dockerExecWithProgress(container, job, logFile, scriptCmd)
		dumpLogFileToJob(container, job, logFile)
		dockerExec(container, "rm", "-f", scriptPath)

		if err == nil {
			checkOut, _ := dockerExec(container, "sh", "-c",
				fmt.Sprintf("ls %s/*.csv 2>/dev/null | wc -l", containerExport))
			csvCount, _ := strconv.Atoi(strings.TrimSpace(checkOut))
			if csvCount > 0 {
				exported = true
				job.addLog(fmt.Sprintf("Flatgraph export succeeded (%d CSV files)", csvCount), "info")
			}
		}
	}

	if !exported {
		// Strategy C: Last resort — try `joern` REPL with loadCpg (no overlays) + save
		job.addLog("Flatgraph export failed. Trying joern REPL loadCpg approach...", "info")
		os.RemoveAll(exportDir)
		dockerExec(container, "rm", "-rf", containerExport)
		os.MkdirAll(exportDir, 0755)

		replCmd := fmt.Sprintf(
			`echo 'loadCpg("%s"); save' | joern --nocolors 2>&1; `+
				`joern-export --format neo4jcsv --repr all -o %s %s`,
			containerCpg, containerExport, containerCpg)
		out, err = dockerExecWithProgress(container, job, logFile, replCmd)
		dumpLogFileToJob(container, job, logFile)

		if err == nil {
			checkOut, _ := dockerExec(container, "sh", "-c",
				fmt.Sprintf("ls %s/*.csv 2>/dev/null | wc -l", containerExport))
			csvCount, _ := strconv.Atoi(strings.TrimSpace(checkOut))
			if csvCount > 0 {
				exported = true
				job.addLog(fmt.Sprintf("REPL + export succeeded (%d CSV files)", csvCount), "info")
			}
		}
	}

	if !exported {
		job.addLog("All export strategies failed. CPG is preserved at "+containerCpg, "error")
		job.addLog("You can manually export by exec'ing into the worker container.", "error")
		job.fail("export failed - CPG preserved for manual export")
		return
	}

	exportElapsed := time.Since(start).Round(time.Second)
	job.addLog(fmt.Sprintf("Export done in %s", exportElapsed), "info")
	job.setProgress(80)

	// ── Step 3: Package (filter edges + tar.gz) ────────────────────
	job.setStatus("packaging")
	job.addLog("Packaging (filtering unneeded edge types)...", "info")
	start = time.Now()

	included, skipped, err := packageResults(exportDir, resultPath)
	if err != nil {
		job.addLog("Packaging FAILED: "+err.Error(), "error")
		job.fail("packaging failed: " + err.Error())
		return
	}

	fi, _ := os.Stat(resultPath)
	sizeMB := float64(0)
	if fi != nil {
		sizeMB = float64(fi.Size()) / 1024 / 1024
	}
	packElapsed := time.Since(start).Round(time.Second)
	job.addLog(fmt.Sprintf("Packaged: %d files, %d edge files skipped (%.1f MB) in %s",
		included, skipped, sizeMB, packElapsed), "info")

	// Full success — safe to clean up CPG now
	parseDone = false // allow defer to clean up
	os.Remove(cpgFile)
	dockerExec(container, "rm", "-f", containerCpg)

	totalElapsed := time.Since(totalStart).Round(time.Second)
	job.addLog(fmt.Sprintf("TOTAL: %s (parse %s + export %s + pack %s)",
		totalElapsed, parseElapsed, exportElapsed, packElapsed), "info")
	job.setProgress(100)
	job.addLog("Done", "done")
	job.complete()
}

// dockerExecWithProgress runs a shell command inside a container.
//
// Architecture: the command's stdout/stderr are redirected to a log file inside
// the container. A blocking `docker exec` (in a goroutine) waits for the actual
// process to finish — no polling or pgrep race conditions. Meanwhile:
//   - `tail -f` streams the log file to job logs in real-time
//   - A heartbeat goroutine reports RAM/CPU every 30s even if Joern is silent
//
// The exit code is captured via an embedded marker in the log file.
func dockerExecWithProgress(container string, job *Job, logFile, shellCmd string) (string, error) {
	// Embed exit code marker so we can reliably detect success/failure.
	// The shell runs the command, captures its exit code, writes a marker, then exits with it.
	wrappedCmd := fmt.Sprintf(
		`%s > %s 2>&1; rc=$?; echo "JOERN_FARM_EXIT_CODE:$rc" >> %s; exit $rc`,
		shellCmd, logFile, logFile)

	log.Printf("docker exec %s sh -c <cmd> (blocking, output -> %s)", container, logFile)

	// Touch the log file first so tail -f doesn't fail if the command hasn't started writing yet
	dockerExec(container, "sh", "-c", "touch "+logFile)

	// Channel to receive the result of the blocking docker exec
	type execResult struct {
		output string
		err    error
	}
	resultCh := make(chan execResult, 1)

	// Run the command in a BLOCKING docker exec (in a goroutine).
	// This is race-free: docker exec returns only when the command actually finishes.
	go func() {
		cmdArgs := []string{"exec", container, "sh", "-c", wrappedCmd}
		cmd := exec.Command("docker", cmdArgs...)
		out, err := cmd.CombinedOutput()
		resultCh <- execResult{string(out), err}
	}()

	// done signal for heartbeat and tail goroutines
	done := make(chan struct{})

	// Heartbeat: report process stats every 30s
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		start := time.Now()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				elapsed := time.Since(start).Round(time.Second)
				stats := getJavaProcessStats(container)
				if stats != "" {
					job.addLog(fmt.Sprintf("Still running... %s elapsed | %s", elapsed, stats), "info")
				} else {
					job.addLog(fmt.Sprintf("Still running... %s elapsed", elapsed), "info")
				}
			}
		}
	}()

	// Tail the log file for real-time output streaming
	tailArgs := []string{"exec", container, "tail", "-f", logFile}
	tailCmd := exec.Command("docker", tailArgs...)
	tailOut, pipeErr := tailCmd.StdoutPipe()
	tailStarted := false
	if pipeErr == nil {
		tailCmd.Stderr = tailCmd.Stdout
		if tailCmd.Start() == nil {
			tailStarted = true
			go func() {
				scanner := bufio.NewScanner(tailOut)
				scanner.Buffer(make([]byte, 256*1024), 256*1024)
				for scanner.Scan() {
					line := scanner.Text()
					trimmed := strings.TrimSpace(line)
					// Don't log our internal exit code marker
					if trimmed != "" && !strings.HasPrefix(trimmed, "JOERN_FARM_EXIT_CODE:") {
						job.addLog(trimmed, "info")
					}
				}
			}()
		}
	}

	// Wait for the blocking docker exec to finish (the ONLY source of truth)
	result := <-resultCh

	// Give tail a moment to catch the last lines
	time.Sleep(2 * time.Second)

	// Stop tail and heartbeat
	if tailStarted && tailCmd.Process != nil {
		tailCmd.Process.Kill()
		tailCmd.Wait()
	}
	close(done)

	// Read the complete log file for the full output
	fullLog, _ := dockerExec(container, "sh", "-c", "cat "+logFile+" 2>/dev/null")

	// Parse exit code from the log file marker
	var exitErr error
	if strings.Contains(fullLog, "JOERN_FARM_EXIT_CODE:0") {
		exitErr = nil
	} else if result.err != nil {
		exitErr = result.err
	} else {
		// docker exec succeeded but the inner command may have failed
		exitErr = fmt.Errorf("command failed (non-zero exit code)")
	}

	return fullLog, exitErr
}

// dumpLogFileToJob reads the log file from the container and adds any
// lines not already in the job logs. This catches output that tail -f
// may have missed (e.g., due to buffering or OOM kill before flush).
func dumpLogFileToJob(container string, job *Job, logFile string) {
	content, err := dockerExec(container, "sh", "-c", "cat "+logFile+" 2>/dev/null")
	if err != nil || strings.TrimSpace(content) == "" {
		job.addLog("(log file empty or not readable)", "info")
		return
	}

	lines := strings.Split(content, "\n")
	added := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "JOERN_FARM_EXIT_CODE:") {
			continue
		}
		job.addLog("[log] "+trimmed, "info")
		added++
		if added > 200 {
			job.addLog(fmt.Sprintf("[log] ... truncated (%d more lines)", len(lines)-added), "info")
			break
		}
	}
}

func getJavaProcessStats(container string) string {
	out, err := dockerExec(container, "sh", "-c",
		"ps -o rss=,pcpu= -C java 2>/dev/null | head -1")
	if err != nil || strings.TrimSpace(out) == "" {
		// Try broader match
		out, err = dockerExec(container, "sh", "-c",
			"ps aux 2>/dev/null | grep '[j]ava' | awk '{print $6, $3}'| head -1")
		if err != nil || strings.TrimSpace(out) == "" {
			return ""
		}
	}

	parts := strings.Fields(strings.TrimSpace(out))
	if len(parts) >= 2 {
		rssKB, _ := strconv.Atoi(parts[0])
		cpu := parts[1]
		if rssKB > 0 {
			return fmt.Sprintf("RAM: %dMB, CPU: %s%%", rssKB/1024, cpu)
		}
	}
	return ""
}

func escapeShell(s string) string {
	return strings.ReplaceAll(s, "'", "'\"'\"'")
}

// resolveContainerID finds the actual container ID, handling Docker Compose
// hash-prefixed names like "8b705204b91d_joern-worker-2".
func resolveContainerID(baseName string) string {
	for attempt := 0; attempt < 15; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}

		out, err := exec.Command("docker", "inspect", "-f", "{{.Id}}", baseName).CombinedOutput()
		if err == nil {
			id := strings.TrimSpace(string(out))
			if len(id) >= 12 {
				log.Printf("container %s -> %s", baseName, id[:12])
				return id[:12]
			}
		}

		out, err = exec.Command("docker", "ps", "-q", "--filter", "name="+baseName).CombinedOutput()
		if err == nil {
			id := strings.TrimSpace(string(out))
			if id != "" {
				if idx := strings.Index(id, "\n"); idx > 0 {
					id = id[:idx]
				}
				log.Printf("container %s resolved by filter -> %s", baseName, id)
				return id
			}
		}

		if attempt == 0 {
			log.Printf("container %s not found yet, retrying...", baseName)
		}
	}

	log.Printf("WARNING: could not resolve container %s, using base name", baseName)
	return baseName
}

// resolveContainerSrcDir finds src/main/java inside the source dir.
func resolveContainerSrcDir(container, srcDir string) string {
	for _, sub := range []string{"/src/main/java", "/src/main", "/src"} {
		candidate := srcDir + sub
		_, err := dockerExec(container, "test", "-d", candidate)
		if err == nil {
			return candidate
		}
	}
	return srcDir
}

// dockerExec runs a command synchronously and returns full output.
func dockerExec(container string, args ...string) (string, error) {
	cmdArgs := append([]string{"exec", container}, args...)
	cmd := exec.Command("docker", cmdArgs...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// packageResults creates a tar.gz of only the needed node/edge CSV files.
func packageResults(exportDir, resultPath string) (int, int, error) {
	entries, err := os.ReadDir(exportDir)
	if err != nil {
		return 0, 0, fmt.Errorf("read export dir: %w", err)
	}

	outFile, err := os.Create(resultPath)
	if err != nil {
		return 0, 0, err
	}
	defer outFile.Close()

	gw := gzip.NewWriter(outFile)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	var included, skipped int
	for _, e := range entries {
		name := e.Name()

		if strings.HasPrefix(name, "edges_") {
			edgeType := strings.TrimPrefix(name, "edges_")
			edgeType = strings.TrimSuffix(edgeType, "_cypher.csv")
			edgeType = strings.TrimSuffix(edgeType, "_data.csv")
			if !NeededEdges[strings.ToUpper(edgeType)] {
				skipped++
				continue
			}
		}

		fpath := filepath.Join(exportDir, name)
		fi, err := os.Stat(fpath)
		if err != nil || fi.IsDir() {
			continue
		}

		header, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return included, skipped, err
		}
		header.Name = name

		if err := tw.WriteHeader(header); err != nil {
			return included, skipped, err
		}
		f, err := os.Open(fpath)
		if err != nil {
			return included, skipped, err
		}
		io.Copy(tw, f)
		f.Close()
		included++
	}

	log.Printf("packaged %d files, skipped %d edge files", included, skipped)
	return included, skipped, nil
}

// unzipSource extracts a zip file into destDir.
func unzipSource(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		target := filepath.Join(destDir, f.Name)

		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)) {
			continue
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}

		os.MkdirAll(filepath.Dir(target), 0755)
		outFile, err := os.Create(target)
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}
		io.Copy(outFile, rc)
		rc.Close()
		outFile.Close()
	}
	return nil
}
