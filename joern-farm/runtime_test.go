package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFrontendDispatchAndConfiguredHeap(t *testing.T) {
	tools := ContainerTools{Javasrc2cpg: "/frontend/javasrc2cpg", Jssrc2cpg: "/frontend/jssrc2cpg.sh", JoernParse: "/frontend/joern-parse"}
	for _, language := range []string{"js", "javascript", "ts", "typescript"} {
		command, err := parseCommand(tools, language, "/source/a'b", "/cpg/out", "12g")
		if err != nil || !strings.Contains(command, "jssrc2cpg.sh") || strings.Contains(command, "delombok") || !strings.Contains(command, "-J-Xmx12g") || !strings.Contains(command, shellQuote("/source/a'b")) {
			t.Fatalf("JS dispatch %q: %s / %v", language, command, err)
		}
	}
	command, err := parseCommand(tools, "java", "/source", "/cpg", "768m")
	if err != nil || !strings.Contains(command, "javasrc2cpg") || !strings.Contains(command, "-J-Xmx768m") || !strings.Contains(command, "--delombok-mode no-delombok") {
		t.Fatalf("Java dispatch: %s / %v", command, err)
	}
	command, err = parseCommand(ContainerTools{JoernParse: "joern-parse"}, "java", "/source", "/cpg", "8g")
	if err != nil || !strings.Contains(command, "--language javasrc") {
		t.Fatalf("Java fallback: %s / %v", command, err)
	}
	if _, err := parseCommand(ContainerTools{Javasrc2cpg: "java-only"}, "js", "s", "c", "4g"); err == nil {
		t.Fatal("JS silently used a Java parser")
	}
	if _, err := normalizeLanguage("python"); err == nil {
		t.Fatal("unsupported language accepted")
	}
	if _, err := parseCommand(tools, "java", "s", "c", "4g; id"); err == nil {
		t.Fatal("heap shell injection accepted")
	}
}

func TestQueryCommandAndRuntimeLimits(t *testing.T) {
	c := queryConfig(FarmConfig{})
	if err := validateRuntimeConfig(c); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []FarmConfig{{JoernHeap: "4g;whoami", QueryTimeoutSeconds: 1, QueryMaxOutputBytes: 1}, {JoernHeap: "4g", QueryTimeoutSeconds: -1, QueryMaxOutputBytes: 1}, {JoernHeap: "4g", QueryTimeoutSeconds: 1, QueryMaxOutputBytes: 513 << 20}} {
		if validateRuntimeConfig(invalid) == nil {
			t.Fatalf("invalid config accepted: %#v", invalid)
		}
	}
	for _, params := range [][]string{{"out=another"}, {"cpgPath=another"}, {"name=one", "name=two"}, {"-Xmx=40g"}, {"missingEquals"}, {"value=" + strings.Repeat("a", 4096)}} {
		if _, err := validateQueryParams(params); err == nil {
			t.Fatalf("invalid params accepted: %#v", params)
		}
	}
	command, err := queryCommand("10g", "s.sc", "c.bin", "o.jsonl", []string{"entry=a'b; $(command)"})
	if err != nil || !strings.Contains(command, "-J-Xmx10g") || !strings.Contains(command, shellQuote("entry=a'b; $(command)")) {
		t.Fatalf("query command %s / %v", command, err)
	}
}

func TestTailBufferStaysBoundedAndRetainsFinalDiagnostics(t *testing.T) {
	b := &tailBuffer{limit: 32}
	for i := 0; i < 10000; i++ {
		if _, err := b.Write([]byte("repeated long diagnostic\n")); err != nil {
			t.Fatal(err)
		}
	}
	b.Write([]byte("FINAL ERROR"))
	if len(b.String()) != 32 || !strings.HasSuffix(b.String(), "FINAL ERROR") {
		t.Fatalf("tail: %q", b.String())
	}
	b.Write([]byte(strings.Repeat("z", 64)))
	if b.String() != strings.Repeat("z", 32) {
		t.Fatalf("single large write is not capped: %q", b.String())
	}
}

func setupQuery(t *testing.T) string {
	t.Helper()
	oldCfg, oldStore, oldPool, oldRun := cfg, store, pool, runQuery
	cfg = queryConfig(FarmConfig{DataDir: t.TempDir(), JoernHeap: "7g"})
	store = NewJobStore()
	const id = "abc123"
	store.Add(&Job{ID: id, Status: "done", Worker: "worker-exact", Language: "javascript"})
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "cpg"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "cpg", id+".cpg.bin"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cfg, store, pool, runQuery = oldCfg, oldStore, oldPool, oldRun })
	return id
}

func scriptRequest(t *testing.T, id, script string, params ...string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	f, err := form.CreateFormFile("script", "query.sc")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte(script))
	for _, param := range params {
		form.WriteField("param", param)
	}
	form.Close()
	req := httptest.NewRequest("POST", "/jobs/"+id+"/script", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.SetPathValue("id", id)
	return req
}

func writeQueryOutput(pidFile, data string) error {
	return os.WriteFile(filepath.Join(cfg.DataDir, strings.TrimSuffix(filepath.Base(pidFile), ".pid")+".jsonl"), []byte(data), 0600)
}

func TestScriptFailureNeverPublishesPartialOutput(t *testing.T) {
	id := setupQuery(t)
	runQuery = func(ctx context.Context, container, command, pidFile string, maxOutput int64) (string, error) {
		if container != "worker-exact" || !strings.Contains(command, "-J-Xmx7g") {
			t.Errorf("wrong execution: %s %s", container, command)
		}
		if err := writeQueryOutput(pidFile, "partial-query-result"); err != nil {
			t.Fatal(err)
		}
		return "compilation failed", errors.New("exit status 1")
	}
	w := httptest.NewRecorder()
	handleRunScript(w, scriptRequest(t, id, "broken Scala"))
	if w.Code != 500 || strings.Contains(w.Body.String(), "partial-query-result") || !strings.Contains(w.Body.String(), "compilation failed") {
		t.Fatalf("failed query was published: %d %s", w.Code, w.Body.String())
	}
	files, _ := filepath.Glob(filepath.Join(cfg.DataDir, "q-*"))
	if len(files) != 0 {
		t.Fatalf("temporary query files retained: %v", files)
	}
}

func TestScriptOutputStreamsAndRejectsOversize(t *testing.T) {
	id := setupQuery(t)
	cfg.QueryMaxOutputBytes = 8
	runQuery = func(ctx context.Context, container, command, pidFile string, maxOutput int64) (string, error) {
		return "", writeQueryOutput(pidFile, "{\"a\":1}\n")
	}
	w := httptest.NewRecorder()
	handleRunScript(w, scriptRequest(t, id, "query"))
	if w.Code != 200 || w.Body.String() != "{\"a\":1}\n" || w.Header().Get("Content-Length") != "8" {
		t.Fatalf("stream result: %d %s", w.Code, w.Body.String())
	}
	runQuery = func(ctx context.Context, container, command, pidFile string, maxOutput int64) (string, error) {
		return "", writeQueryOutput(pidFile, "too-large-output")
	}
	w = httptest.NewRecorder()
	handleRunScript(w, scriptRequest(t, id, "query"))
	if w.Code != 413 || strings.Contains(w.Body.String(), "too-large-output") {
		t.Fatalf("oversized output published: %d %s", w.Code, w.Body.String())
	}
}

func TestScriptRejectsOversizeInputAndReservedParamsBeforeExecution(t *testing.T) {
	id := setupQuery(t)
	runQuery = func(context.Context, string, string, string, int64) (string, error) {
		t.Fatal("invalid query executed")
		return "", nil
	}
	for _, req := range []*http.Request{scriptRequest(t, id, strings.Repeat("a", maxScriptBytes+1)), scriptRequest(t, id, "query", "out=overwrite")} {
		w := httptest.NewRecorder()
		handleRunScript(w, req)
		if w.Code != 400 {
			t.Fatalf("invalid query: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestQueryAdmissionAndCancellationAreVisible(t *testing.T) {
	id := setupQuery(t)
	if !acquireQuerySlot() {
		t.Fatal("slot unexpectedly busy")
	}
	w := httptest.NewRecorder()
	handleRunScript(w, scriptRequest(t, id, "query"))
	releaseQuerySlot()
	if w.Code != 429 {
		t.Fatalf("second query admitted: %d", w.Code)
	}
	runQuery = func(ctx context.Context, container, command, pidFile string, maxOutput int64) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	req := scriptRequest(t, id, "query")
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	handleRunScript(w, req)
	if w.Code != 504 {
		t.Fatalf("cancellation hidden: %d %s", w.Code, w.Body.String())
	}
}

func TestSubmitLanguagePersistsAndUnsupportedDoesNotQueue(t *testing.T) {
	setupQuery(t)
	pool = &WorkerPool{queue: make(chan *Job, 2), dataDir: cfg.DataDir}
	for _, language := range []string{"typescript", "python"} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		form.WriteField("metadata", `{"name":"fixture","language":"`+language+`","export":false}`)
		f, _ := form.CreateFormFile("source", "source.zip")
		var archive bytes.Buffer
		z := zip.NewWriter(&archive)
		entry, _ := z.Create("src/index.ts")
		entry.Write([]byte("export const value = 1"))
		z.Close()
		f.Write(archive.Bytes())
		form.Close()
		req := httptest.NewRequest("POST", "/jobs", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		w := httptest.NewRecorder()
		handleSubmitJob(w, req)
		if language == "python" {
			if w.Code != 400 || len(pool.queue) != 0 {
				t.Fatalf("unsupported parse queued: %d", w.Code)
			}
			continue
		}
		if w.Code != 202 {
			t.Fatalf("JS submit: %d %s", w.Code, w.Body.String())
		}
		job := <-pool.queue
		if job.Language != "javascript" || job.Export {
			t.Fatalf("metadata lost: %#v", job)
		}
		var response map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response["jobId"] != job.ID {
			t.Fatalf("submit response: %s", w.Body.String())
		}
	}
}

func TestExecuteQueryCapsDiagnosticsAndStopsRemoteProcessGroupOnDeadline(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "cleanup")
	// Fake the Docker CLI only; exercise the actual bounded exec and deadline path.
	script := "#!/bin/sh\ncase \"$5\" in *'/bin/kill -TERM'*) printf '%s' \"$5\" > \"$DT_TEST_CLEANUP\"; exit 0;; esac\nwhile :; do printf 'bounded diagnostic line\\n'; done\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DT_TEST_CLEANUP", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	output, err := executeQuery(ctx, "worker", "joern --script q.sc", "/data/q-test.pid", 64<<20)
	if !errors.Is(err, context.DeadlineExceeded) || len(output) > maxQueryLogBytes {
		t.Fatalf("deadline/log bound: %v %d", err, len(output))
	}
	cleanup, err := os.ReadFile(marker)
	if err != nil || !bytes.Contains(cleanup, []byte("/bin/kill -KILL")) || !bytes.Contains(cleanup, []byte("/data/q-test.pid")) {
		t.Fatalf("remote JVM group not stopped: %s %v", cleanup, err)
	}
}
