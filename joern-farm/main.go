package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

var (
	store *JobStore
	pool  *WorkerPool
	cfg   FarmConfig
)

func main() {
	cfg = loadConfig()
	if err := validateRuntimeConfig(cfg); err != nil {
		log.Fatal(err)
	}
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
		Port:                envOr("PORT", "9090"),
		DataDir:             envOr("DATA_DIR", "/data"),
		WorkerCount:         envInt("WORKER_COUNT", 2),
		JoernHeap:           envOr("JOERN_HEAP", "4g"),
		QueryTimeoutSeconds: envInt("QUERY_TIMEOUT_SECONDS", 300),
		QueryMaxOutputBytes: int64(envInt("QUERY_MAX_OUTPUT_BYTES", 64<<20)),
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
	r.Body = http.MaxBytesReader(w, r.Body, 500<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil { // spool large uploads to disk
		errResp(w, 400, "parse multipart: "+err.Error())
		return
	}

	defer r.MultipartForm.RemoveAll()
	metaStr := r.FormValue("metadata")
	if metaStr == "" {
		errResp(w, 400, "missing metadata field")
		return
	}
	var meta struct {
		Name     string `json:"name"`
		Branch   string `json:"branch"`
		Language string `json:"language"`
		Export   *bool  `json:"export"` // pointer: absent → default true (export to neo4jcsv)
	}
	if err := json.Unmarshal([]byte(metaStr), &meta); err != nil {
		errResp(w, 400, "bad metadata json: "+err.Error())
		return
	}
	if meta.Name == "" {
		errResp(w, 400, "name is required in metadata")
		return
	}
	language, err := normalizeLanguage(meta.Language)
	if err != nil {
		errResp(w, 400, err.Error())
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
		Language:  language,
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
		Language string     `json:"language"`
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
			ID: j.ID, Name: j.Name, Branch: j.Branch, Language: j.Language,
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
	if !acquireQuerySlot() {
		errResp(w, http.StatusTooManyRequests, "query slot busy; retry after the active query completes")
		return
	}
	defer releaseQuerySlot()
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := r.ParseMultipartForm(128 << 10); err != nil {
		errResp(w, 400, "parse multipart: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, _, err := r.FormFile("script")
	if err != nil {
		errResp(w, 400, "missing script file: "+err.Error())
		return
	}
	defer file.Close()
	scriptBytes, err := io.ReadAll(io.LimitReader(file, maxScriptBytes+1))
	if err != nil || len(scriptBytes) == 0 || len(scriptBytes) > maxScriptBytes {
		errResp(w, 400, "script must contain 1..1048576 bytes")
		return
	}
	params, err := validateQueryParams(r.Form["param"])
	if err != nil {
		errResp(w, 400, err.Error())
		return
	}
	runtimeCfg := queryConfig(cfg)
	if err := validateRuntimeConfig(runtimeCfg); err != nil {
		errResp(w, 500, err.Error())
		return
	}
	qid := generateID()
	scriptHost := filepath.Join(cfg.DataDir, "q-"+qid+".sc")
	outHost := filepath.Join(cfg.DataDir, "q-"+qid+".jsonl")
	pidHost := filepath.Join(cfg.DataDir, "q-"+qid+".pid")
	if err := os.WriteFile(scriptHost, scriptBytes, 0644); err != nil {
		errResp(w, 500, "write script: "+err.Error())
		return
	}
	defer os.Remove(scriptHost)
	defer os.Remove(outHost)
	defer os.Remove(pidHost)
	scriptCt := "/data/q-" + qid + ".sc"
	outCt := "/data/q-" + qid + ".jsonl"
	pidCt := "/data/q-" + qid + ".pid"
	command, err := queryCommand(runtimeCfg.JoernHeap, scriptCt, containerCpg, outCt, params)
	if err != nil {
		errResp(w, 400, err.Error())
		return
	}
	container := job.Worker
	if container == "" {
		container = resolveContainerID("joern-worker-1")
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(runtimeCfg.QueryTimeoutSeconds)*time.Second)
	defer cancel()
	log.Printf("job %s: starting bounded script query %s", job.ID, qid)
	cmdOut, runErr := runQuery(ctx, container, command, pidCt, runtimeCfg.QueryMaxOutputBytes)
	// A failed script can leave partial output. Never publish it as a successful query.
	if runErr != nil {
		code := 500
		if ctx.Err() != nil {
			code = http.StatusGatewayTimeout
		}
		errResp(w, code, "script execution failed: "+runErr.Error()+"\njoern said:\n"+tailStr(cmdOut, 4000))
		return
	}
	output, err := os.Open(outHost)
	if err != nil {
		errResp(w, 500, "script produced no output.\njoern said:\n"+tailStr(cmdOut, 4000))
		return
	}
	defer output.Close()
	stat, err := output.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		errResp(w, 500, "script output must be a regular file")
		return
	}
	if stat.Size() > runtimeCfg.QueryMaxOutputBytes {
		errResp(w, http.StatusRequestEntityTooLarge, "script output exceeds QUERY_MAX_OUTPUT_BYTES; narrow the query")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	// Stream the result instead of allocating the whole NDJSON file in the API heap.
	io.CopyN(w, output, stat.Size())
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
