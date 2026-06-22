package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	store *JobStore
	pool  *WorkerPool
	cfg   FarmConfig
)

func main() {
	cfg = loadConfig()
	store = NewJobStore()
	pool = NewWorkerPool(cfg.WorkerCount, cfg.DataDir)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", handleSubmitJob)
	mux.HandleFunc("GET /jobs", handleListJobs)
	mux.HandleFunc("GET /jobs/{id}", handleGetJob)
	mux.HandleFunc("GET /jobs/{id}/logs", handleJobLogs)
	mux.HandleFunc("GET /jobs/{id}/result", handleDownloadResult)
	mux.HandleFunc("GET /jobs/{id}/cpg", handleDownloadCpg)
	mux.HandleFunc("POST /jobs/{id}/script", handleRunScript)
	mux.HandleFunc("DELETE /jobs/{id}", handleDeleteJob)
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /test", handleTestParse)
	mux.HandleFunc("GET /probe", handleProbe)

	log.Printf("joern-farm listening on :%s  workers=%d  data=%s", cfg.Port, cfg.WorkerCount, cfg.DataDir)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, withCORS(mux)))
}

func loadConfig() FarmConfig {
	c := FarmConfig{
		Port:        envOr("PORT", "9090"),
		DataDir:     envOr("DATA_DIR", "/data"),
		WorkerCount: envInt("WORKER_COUNT", 2),
		JoernHeap:   envOr("JOERN_HEAP", "4g"),
	}
	return c
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// POST /jobs  (multipart: metadata JSON + source zip)
func handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(500 << 20); err != nil { // 500MB max
		errResp(w, 400, "parse multipart: "+err.Error())
		return
	}

	metaStr := r.FormValue("metadata")
	if metaStr == "" {
		errResp(w, 400, "missing metadata field")
		return
	}
	var meta struct {
		Name   string `json:"name"`
		Branch string `json:"branch"`
		Export *bool  `json:"export"` // pointer: absent → default true (export to neo4jcsv)
	}
	if err := json.Unmarshal([]byte(metaStr), &meta); err != nil {
		errResp(w, 400, "bad metadata json: "+err.Error())
		return
	}
	if meta.Name == "" {
		errResp(w, 400, "name is required in metadata")
		return
	}
	export := true
	if meta.Export != nil {
		export = *meta.Export
	}

	file, _, err := r.FormFile("source")
	if err != nil {
		errResp(w, 400, "missing source zip: "+err.Error())
		return
	}
	defer file.Close()

	job := &Job{
		ID:        generateID(),
		Name:      meta.Name,
		Branch:    meta.Branch,
		Export:    export,
		Status:    "queued",
		StartedAt: time.Now(),
	}

	// Save zip to disk
	srcDir := filepath.Join(cfg.DataDir, "sources", job.ID)
	os.MkdirAll(srcDir, 0755)
	zipPath := filepath.Join(cfg.DataDir, "sources", job.ID+".zip")

	out, err := os.Create(zipPath)
	if err != nil {
		errResp(w, 500, "save zip: "+err.Error())
		return
	}
	written, _ := io.Copy(out, file)
	out.Close()

	job.addLog(fmt.Sprintf("Received source zip (%.1f MB)", float64(written)/1024/1024), "info")

	// Unzip
	if err := unzipSource(zipPath, srcDir); err != nil {
		os.Remove(zipPath)
		os.RemoveAll(srcDir)
		errResp(w, 400, "unzip failed: "+err.Error())
		return
	}
	os.Remove(zipPath)

	job.addLog("Source extracted, queued for processing", "info")
	store.Add(job)
	pool.Submit(job)

	jsonResp(w, 202, map[string]string{"jobId": job.ID, "status": job.Status})
}

// GET /jobs
func handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs := store.List()
	type jobSummary struct {
		ID       string     `json:"id"`
		Name     string     `json:"name"`
		Branch   string     `json:"branch,omitempty"`
		Status   string     `json:"status"`
		Progress int        `json:"progress"`
		Worker   string     `json:"worker,omitempty"`
		Started  time.Time  `json:"startedAt"`
		Done     *time.Time `json:"doneAt,omitempty"`
	}
	out := make([]jobSummary, len(jobs))
	for i, j := range jobs {
		j.mu.Lock()
		out[i] = jobSummary{
			ID: j.ID, Name: j.Name, Branch: j.Branch,
			Status: j.Status, Progress: j.Progress, Worker: j.Worker,
			Started: j.StartedAt, Done: j.DoneAt,
		}
		j.mu.Unlock()
	}
	jsonResp(w, 200, out)
}

// GET /jobs/{id}
func handleGetJob(w http.ResponseWriter, r *http.Request) {
	job := store.Get(r.PathValue("id"))
	if job == nil {
		errResp(w, 404, "job not found")
		return
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	jsonResp(w, 200, job)
}

// GET /jobs/{id}/logs  (SSE stream)
func handleJobLogs(w http.ResponseWriter, r *http.Request) {
	job := store.Get(r.PathValue("id"))
	if job == nil {
		errResp(w, 404, "job not found")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		errResp(w, 500, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	lastIdx := 0
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		job.mu.Lock()
		logLen := len(job.Logs)
		var newLogs []LogEntry
		if lastIdx < logLen {
			newLogs = make([]LogEntry, logLen-lastIdx)
			copy(newLogs, job.Logs[lastIdx:logLen])
			lastIdx = logLen
		}
		status := job.Status
		progress := job.Progress
		job.mu.Unlock()

		for _, entry := range newLogs {
			data, _ := json.Marshal(entry)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}

		pData, _ := json.Marshal(map[string]interface{}{"status": status, "progress": progress})
		fmt.Fprintf(w, "event: progress\ndata: %s\n\n", pData)
		flusher.Flush()

		if status == "done" || status == "failed" {
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", status)
			flusher.Flush()
			return
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// GET /jobs/{id}/result  -> tar.gz download
func handleDownloadResult(w http.ResponseWriter, r *http.Request) {
	job := store.Get(r.PathValue("id"))
	if job == nil {
		errResp(w, 404, "job not found")
		return
	}

	job.mu.Lock()
	status := job.Status
	job.mu.Unlock()

	if status != "done" {
		errResp(w, 400, "job not done yet, status: "+status)
		return
	}

	resultPath := filepath.Join(cfg.DataDir, "results", job.ID+".tar.gz")
	if _, err := os.Stat(resultPath); os.IsNotExist(err) {
		errResp(w, 404, "result file not found")
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, job.Name))
	http.ServeFile(w, r, resultPath)
}

// GET /jobs/{id}/cpg  -> raw cpg.bin (only for export:false jobs, which keep it)
func handleDownloadCpg(w http.ResponseWriter, r *http.Request) {
	job := store.Get(r.PathValue("id"))
	if job == nil {
		errResp(w, 404, "job not found")
		return
	}
	job.mu.Lock()
	status := job.Status
	job.mu.Unlock()
	if status != "done" {
		errResp(w, 400, "job not done yet, status: "+status)
		return
	}
	cpgPath := filepath.Join(cfg.DataDir, "cpg", job.ID+".cpg.bin")
	if _, err := os.Stat(cpgPath); err != nil {
		errResp(w, 404, "cpg not found (was this job submitted with export:false?)")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.cpg.bin"`, job.Name))
	http.ServeFile(w, r, cpgPath)
}

// POST /jobs/{id}/script  (multipart: "script" file + repeated "param" k=v fields)
// Runs a Joern script against the job's kept cpg in a worker container and returns the
// script's output file (the JSONL our lenses emit). This is how a thin client offloads
// queries entirely — no Joern on the client.
func handleRunScript(w http.ResponseWriter, r *http.Request) {
	job := store.Get(r.PathValue("id"))
	if job == nil {
		errResp(w, 404, "job not found")
		return
	}
	job.mu.Lock()
	status := job.Status
	job.mu.Unlock()
	if status != "done" {
		errResp(w, 400, "job not done yet, status: "+status)
		return
	}
	containerCpg := "/data/cpg/" + job.ID + ".cpg.bin"
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "cpg", job.ID+".cpg.bin")); err != nil {
		errResp(w, 404, "cpg not found (submit with export:false to keep it for queries)")
		return
	}
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		errResp(w, 400, "parse multipart: "+err.Error())
		return
	}
	file, _, err := r.FormFile("script")
	if err != nil {
		errResp(w, 400, "missing script file: "+err.Error())
		return
	}
	defer file.Close()
	scriptBytes, _ := io.ReadAll(file)

	// Materialize script + output path on the shared /data volume.
	qid := generateID()
	scriptHost := filepath.Join(cfg.DataDir, "q-"+qid+".sc")
	outHost := filepath.Join(cfg.DataDir, "q-"+qid+".jsonl")
	if err := os.WriteFile(scriptHost, scriptBytes, 0644); err != nil {
		errResp(w, 500, "write script: "+err.Error())
		return
	}
	defer os.Remove(scriptHost)
	defer os.Remove(outHost)
	scriptCt := "/data/q-" + qid + ".sc"
	outCt := "/data/q-" + qid + ".jsonl"

	// Build the joern command: the farm sets cpgPath + out; the client supplies the rest.
	args := []string{"--script", scriptCt, "--param", "cpgPath=" + containerCpg, "--param", "out=" + outCt}
	for _, p := range r.Form["param"] {
		args = append(args, "--param", p)
	}
	container := resolveContainerID("joern-worker-1")
	joinArgs := "joern"
	for _, a := range args {
		joinArgs += " '" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	log.Printf("job %s: running script query: %s", job.ID, joinArgs)
	cmdOut, runErr := dockerExec(container, "sh", "-c", "unset _JAVA_OPTIONS JAVA_TOOL_OPTIONS; "+joinArgs)
	data, readErr := os.ReadFile(outHost)
	if readErr != nil {
		errResp(w, 500, "script produced no output.\njoern said:\n"+tailStr(cmdOut, 4000))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Write(data)
	_ = runErr
}

func tailStr(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// DELETE /jobs/{id}
func handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job := store.Get(id)
	if job == nil {
		errResp(w, 404, "job not found")
		return
	}

	// Clean up all artifacts for this job
	os.Remove(filepath.Join(cfg.DataDir, "results", id+".tar.gz"))
	os.RemoveAll(filepath.Join(cfg.DataDir, "exports", id))
	os.Remove(filepath.Join(cfg.DataDir, "cpg", id+".cpg.bin"))
	os.RemoveAll(filepath.Join(cfg.DataDir, "sources", id))
	store.Delete(id)
	jsonResp(w, 200, map[string]string{"status": "deleted"})
}

// GET /health
func handleHealth(w http.ResponseWriter, r *http.Request) {
	jobs := store.List()
	var running, queued, done int
	for _, j := range jobs {
		j.mu.Lock()
		switch j.Status {
		case "queued":
			queued++
		case "done", "failed":
			done++
		default:
			running++
		}
		j.mu.Unlock()
	}
	jsonResp(w, 200, map[string]interface{}{
		"workers": cfg.WorkerCount,
		"running": running,
		"queued":  queued,
		"done":    done,
	})
}

func jsonResp(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errResp(w http.ResponseWriter, status int, msg string) {
	jsonResp(w, status, map[string]string{"error": msg})
}

// POST /test - creates a tiny Java project and runs the full pipeline.
// Use this to validate that parsing/export/packaging works before running on real code.
func handleTestParse(w http.ResponseWriter, r *http.Request) {
	job := &Job{
		ID:        generateID(),
		Name:      "_test_",
		Export:    true,
		Status:    "queued",
		StartedAt: time.Now(),
	}

	srcDir := filepath.Join(cfg.DataDir, "sources", job.ID)
	os.MkdirAll(filepath.Join(srcDir, "com", "example"), 0755)

	testJava := `package com.example;

import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping("/api")
public class HelloController {

    private final GreetingService greetingService;

    public HelloController(GreetingService greetingService) {
        this.greetingService = greetingService;
    }

    @GetMapping("/hello")
    public String hello(@RequestParam(defaultValue = "World") String name) {
        return greetingService.greet(name);
    }

    @PostMapping("/echo")
    public String echo(@RequestBody String body) {
        return body;
    }
}
`
	serviceJava := `package com.example;

import org.springframework.stereotype.Service;

@Service
public class GreetingService {

    public String greet(String name) {
        return "Hello, " + name + "!";
    }
}
`
	os.WriteFile(filepath.Join(srcDir, "com", "example", "HelloController.java"), []byte(testJava), 0644)
	os.WriteFile(filepath.Join(srcDir, "com", "example", "GreetingService.java"), []byte(serviceJava), 0644)

	job.addLog("Test job: 2 Java files (HelloController + GreetingService)", "info")
	store.Add(job)
	pool.Submit(job)

	jsonResp(w, 202, map[string]string{
		"jobId":   job.ID,
		"status":  "queued",
		"message": "Test parse submitted. GET /jobs/" + job.ID + " for status, GET /jobs/" + job.ID + "/logs for SSE stream.",
	})
}

// GET /probe - shows what tools are available in each worker container.
func handleProbe(w http.ResponseWriter, r *http.Request) {
	type workerInfo struct {
		ID        int            `json:"id"`
		Container string         `json:"container"`
		Tools     ContainerTools `json:"tools"`
	}

	var results []workerInfo
	for i := 1; i <= cfg.WorkerCount; i++ {
		baseName := fmt.Sprintf("joern-worker-%d", i)
		cid := resolveContainerID(baseName)
		tools := probeContainer(cid)
		results = append(results, workerInfo{
			ID:        i,
			Container: cid,
			Tools:     tools,
		})
	}

	jsonResp(w, 200, results)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
