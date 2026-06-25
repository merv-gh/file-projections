package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain runs from the package dir (src/), but the fixtures and the project's
// own sources live at the repo root. Hop up one level so the repo-root-relative
// paths the tests use keep resolving after the main.go split into src/.
func TestMain(m *testing.M) {
	if _, err := os.Stat("fixtures"); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join("..", "fixtures")); err == nil {
			_ = os.Chdir("..")
		}
	}
	os.Exit(m.Run())
}

const sampleRoot = "fixtures/spring-sample/src/main/java"

const samplePatterns = "kafka-listener=@KafkaListener;scheduled=@Scheduled;event-listener=@EventListener;http-mapping=@(Get|Post|Put|Delete|Patch|Request)Mapping"

func TestEntrypointsLensFindsKafkaScheduledAndMappings(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Root: ".", Lenses: []LensConfig{{
		Name: "ep", Out: filepath.Join(dir, "ep.projection"), Analyzer: "entrypoints", SourceRoot: "fixtures/spring-sample",
		Params: map[string]string{"patterns": samplePatterns},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(dir, "ep.projection"))
	for _, want := range []string{
		"# sync: view-only",
		"@KafkaListener",
		"@Scheduled",
		"@EventListener",
		"@PostMapping",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("entrypoints missing %q\n%s", want, got)
		}
	}
	// Code-first layout: the matched code precedes the file:line locator; no regexp label.
	if strings.Contains(got, " :: ") {
		t.Fatalf("entrypoints should not carry the old :: label layout\n%s", got)
	}
}

func TestExitpointsLensFindsRepositorySaveAndKafkaSend(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Root: ".", Lenses: []LensConfig{{
		Name: "xp", Out: filepath.Join(dir, "xp.projection"), Analyzer: "exitpoints", SourceRoot: "fixtures/spring-sample",
		Params: map[string]string{"sinks": "*repository*.save,*kafka*.send"},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(dir, "xp.projection"))
	for _, want := range []string{
		"this.orderRepository.save(order);",
		"this.kafkaTemplate.send(",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("exitpoints missing %q\n%s", want, got)
		}
	}
}

func TestControlFlowEmitsBranchPerPath(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Root: ".", Lenses: []LensConfig{{
		Name: "cf", Out: filepath.Join(dir, "cf.projection"), Analyzer: "control-flow", SourceRoot: sampleRoot,
		Params: map[string]string{"file": "com/example/shop/OrderController.java", "line": "35"},
	}}}
	results, err := Run(cfg, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(results[0].Extra); n != 4 {
		t.Fatalf("expected 4 branch files, got %d", n)
	}
	// Each branch is its own file with the expected guard combination.
	for i := 1; i <= 4; i++ {
		p := filepath.Join(dir, "cf.branch-"+itoa(i)+".projection")
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing branch file %s", p)
		}
	}
	// New format: entry signature, the active conditions (negated on the false branch),
	// then the exitpoint — code first, file:line in the second column. Intermediate
	// statements and guard/summary chatter are dropped. Find the express vs standard
	// branch by their condition rather than by index (enumeration order is incidental).
	var express, standard string
	for i := 1; i <= 4; i++ {
		b := read(t, filepath.Join(dir, "cf.branch-"+itoa(i)+".projection"))
		if strings.Contains(b, "!(order.isExpress())") {
			standard = b
		} else if strings.Contains(b, "order.isExpress()") {
			express = b
		}
	}
	if express == "" || standard == "" {
		t.Fatalf("expected one express and one standard branch")
	}
	// Every reaching path guards out the early validation-error return.
	if !strings.Contains(express, "!(result.hasErrors())") {
		t.Fatalf("express branch missing the hasErrors guard:\n%s", express)
	}
	// The exitpoint (target line) is present; intermediate setShipping is not.
	if !strings.Contains(express, "this.orderRepository.save(order);") {
		t.Fatalf("express branch should reach the save target:\n%s", express)
	}
	if strings.Contains(express, "setShipping") {
		t.Fatalf("intermediate statements should be dropped:\n%s", express)
	}
	// Code precedes the locator: the signature row carries the file:line in column 2.
	if !strings.Contains(express, "OrderController.java:21") {
		t.Fatalf("express branch missing entry locator:\n%s", express)
	}
}

func TestControlFlowJoernModeErrorsWithoutEngine(t *testing.T) {
	// Portable: with no joern binary and no tools.joern.image, mode=joern must fail
	// with a clear message rather than silently falling back. (The real CPG path is
	// exercised manually against the Docker image; CI stays engine-free.)
	if joernAvailable(Config{}) {
		t.Skip("joern available in this environment; skipping the no-engine assertion")
	}
	dir := t.TempDir()
	cfg := Config{Root: ".", Lenses: []LensConfig{{
		Name: "cf", Out: filepath.Join(dir, "cf.projection"), Analyzer: "control-flow", SourceRoot: sampleRoot,
		Params: map[string]string{"file": "com/example/shop/OrderController.java", "line": "35", "mode": "joern"},
	}}}
	_, err := Run(cfg, DefaultRegistry())
	if err == nil || !strings.Contains(err.Error(), "Docker was not found") {
		t.Fatalf("expected joern-not-available error, got %v", err)
	}
}

func TestEntryToExitRequiresParamsAndEngine(t *testing.T) {
	dir := t.TempDir()
	// Missing entry/exit -> clear error, regardless of engine.
	cfg := Config{Root: ".", Lenses: []LensConfig{{
		Name: "e2e", Out: filepath.Join(dir, "e2e.projection"), Analyzer: "entry-to-exit", SourceRoot: sampleRoot,
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "params.entry and params.exit") {
		t.Fatalf("expected params error, got %v", err)
	}
	// With params but no engine, the 1-to-1 form must fail clearly (CPG path is verified
	// manually against the Docker image; CI stays engine-free).
	if joernAvailable(Config{}) {
		t.Skip("joern available; skipping no-engine assertion")
	}
	cfg.Lenses[0].Params = map[string]string{"entry": "@KafkaListener", "exit": `\.save\s*\(`, "entry_name": "onIncoming", "exit_file": "OrderEventService"}
	if _, err := Run(cfg, DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "Docker was not found") {
		t.Fatalf("expected joern-not-available error, got %v", err)
	}
}

func TestAstGrepRequiresParamsAndEngine(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Root: ".", Lenses: []LensConfig{{
		Name: "ag", Out: filepath.Join(dir, "ag.projection"), Analyzer: "ast-grep", SourceRoot: sampleRoot,
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "params.pattern and params.lang") {
		t.Fatalf("expected params error, got %v", err)
	}
	if _, err := exec.LookPath("ast-grep"); err == nil {
		t.Skip("ast-grep installed; skipping no-engine assertion")
	}
	if _, err := exec.LookPath("sg"); err == nil {
		t.Skip("sg installed; skipping no-engine assertion")
	}
	cfg.Lenses[0].Params = map[string]string{"pattern": "$X.save($Y)", "lang": "java"}
	if _, err := Run(cfg, DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "no tools.ast-grep.image") {
		t.Fatalf("expected no-engine error, got %v", err)
	}
}

func TestWizardDetectsStackAndWritesConfig(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "src", "main", "java", "com", "x", "Foo.java"),
		"package com.x;\nclass Foo {\n\t@org.springframework.web.bind.annotation.GetMapping(\"/p\")\n\tpublic String ping() {\n\t\treturn repo.save(x);\n\t}\n}\n")
	cfgPath := filepath.Join(dir, "config.json")
	// answers: source(default) / entrypoints Y / exitpoints Y / all-paths N / bookmark Y / watch n
	in := strings.NewReader("\n\n\n\n\nn\n")
	var out strings.Builder
	if err := RunWizard(dir, cfgPath, in, &out); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("wizard did not write a loadable config: %v", err)
	}
	byName := map[string]LensConfig{}
	for _, l := range cfg.Lenses {
		byName[l.Name] = l
	}
	for _, want := range []string{"entrypoints", "exitpoints", "first-bookmark"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("missing %q lens; got %v", want, cfg.Lenses)
		}
	}
	if got := byName["entrypoints"].SourceRoot; got != "src/main/java" {
		t.Fatalf("expected suggested source root src/main/java, got %q", got)
	}
	if byName["first-bookmark"].Params["file"] != "com/x/Foo.java" {
		t.Fatalf("bookmark file = %q", byName["first-bookmark"].Params["file"])
	}
	if !fileExists(filepath.Join(dir, ".projections", "entrypoints.projection")) {
		t.Fatal("wizard did not generate projections")
	}
	if !strings.Contains(out.String(), "All set") {
		t.Fatalf("wizard output missing the congrats:\n%s", out.String())
	}
}

func TestFarmClientOffloadsBuildAndQuery(t *testing.T) {
	// Mock joern-farm: accept upload, report done, run "script" by returning JSONL.
	var gotParams []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		w.Write([]byte(`{"jobId":"J1","status":"queued"}`))
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"done","progress":100}`))
	})
	mux.HandleFunc("POST /jobs/{id}/script", func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		gotParams = r.Form["param"]
		w.Write([]byte(`{"kind":"block","id":"checkout->save","file":"C.java","mode":"entry-to-exit","tool":"joern","lines":["entry checkout"],"facts":["entrypoint: checkout"]}` + "\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	write(t, filepath.Join(dir, "src", "C.java"), "class C { void m(){ repo.save(o); } }\n")
	cfg := Config{Root: dir, ProjectionsDir: ".projections",
		Tools: map[string]ToolConfig{"joern": {Farm: srv.URL}},
		Lenses: []LensConfig{{
			Name: "e2e", Out: filepath.Join(dir, ".projections", "e2e.projection"), Analyzer: "entry-to-exit", SourceRoot: "src",
			Params: map[string]string{"entry": "@X", "exit": `\.save\(`},
		}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatalf("farm-backed run failed: %v", err)
	}
	got := read(t, filepath.Join(dir, ".projections", "e2e.projection"))
	if !strings.Contains(got, "checkout->save") || !strings.Contains(got, "entry-to-exit") {
		t.Fatalf("farm result not rendered:\n%s", got)
	}
	// The farm must receive the lens params but NOT out/cpgPath (it sets those itself).
	joined := strings.Join(gotParams, " ")
	if !strings.Contains(joined, "entry=@X") || strings.Contains(joined, "out=") || strings.Contains(joined, "cpgPath=") {
		t.Fatalf("unexpected params sent to farm: %v", gotParams)
	}
	// Job id was cached for reuse.
	if !fileExists(filepath.Join(dir, cpgPathRel(cfg, "src")) + ".farmjob") {
		t.Fatal("farm job id not cached")
	}
}

func TestPerfHelpers(t *testing.T) {
	for _, u := range []string{"https://github.com/x/y", "git@github.com:x/y.git", "x/y.git"} {
		if !isGitURL(u) {
			t.Fatalf("%q should be a git URL", u)
		}
	}
	for _, p := range []string{"spring-petclinic-main", "./local", "/abs/path"} {
		if isGitURL(p) {
			t.Fatalf("%q should be a local path", p)
		}
	}
	// A blown budget reports a clear, actionable timeout message.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deadlined, c2 := context.WithTimeout(context.Background(), 0)
	defer c2()
	<-deadlined.Done()
	err := perfErr("CPG build", 5*time.Minute, deadlined, ctx.Err())
	if err == nil || !strings.Contains(err.Error(), "exceeded the 5m0s budget") {
		t.Fatalf("expected timeout message, got %v", err)
	}
}

func TestResolveSourceFileRejectsTraversal(t *testing.T) {
	cfg := Config{Root: t.TempDir()}
	for _, ref := range []string{"../../etc/passwd", "/etc/passwd", "a/../../b.java", ".."} {
		if _, _, err := resolveSourceFile(cfg, ref); err == nil {
			t.Fatalf("expected %q to be rejected as unsafe", ref)
		}
	}
}

func TestBookmarkHeaderEditRefreshesView(t *testing.T) {
	dir := t.TempDir()
	src := "class A {\n\tint a = 1;\n\tint b = 2;\n\tint c = 3;\n\tint d = 4;\n}\n"
	write(t, filepath.Join(dir, "src", "A.java"), src)
	projPath := filepath.Join(dir, "bm.projection")
	cfg := Config{Root: dir, Lenses: []LensConfig{{
		Name: "bm", Out: projPath, Analyzer: "bookmark", SourceRoot: "src",
		Params: map[string]string{"file": "A.java", "lines": "2-3"},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	got := read(t, projPath)
	if !strings.Contains(got, "int b = 2;") || strings.Contains(got, "int d = 4;") {
		t.Fatalf("initial bookmark should be lines 2-3:\n%s", got)
	}
	// User edits the line range in the anchor ID (2-3 -> 2-5) and saves.
	edited := strings.Replace(got, "#A.java:2-3 ", "#A.java:2-5 ", 1)
	if edited == got {
		t.Fatal("could not find ID range to edit")
	}
	write(t, projPath, edited)

	res, err := SyncProjection(cfg, projPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.ToProjection != 1 {
		t.Fatalf("expected a header-driven refresh, got %+v", res)
	}
	got2 := read(t, projPath)
	for _, want := range []string{"int b = 2;", "int c = 3;", "int d = 4;", "#A.java:2-5 ", "src=src/A.java:2-5"} {
		if !strings.Contains(got2, want) {
			t.Fatalf("refreshed view missing %q\n%s", want, got2)
		}
	}
}

func TestDropInBookmarkExpandsResolvesAndSyncs(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "src", "com", "example", "demo", "UserEventConsumer.java"),
		"package com.example.demo;\nclass UserEventConsumer {\n\tvoid onMsg() {\n\t\trepo.save(x);\n\t}\n}\n")
	// Drop-in: a new .projection whose only content is a package-relative path:line.
	projPath := filepath.Join(dir, ".projections", "bm.projection")
	write(t, projPath, "com/example/demo/UserEventConsumer.java:4\n")

	cfg := Config{Root: dir, ProjectionsDir: ".projections"}
	expanded, err := expandDropIns(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 1 {
		t.Fatalf("expected 1 expansion, got %d", len(expanded))
	}
	got := read(t, projPath)
	for _, want := range []string{
		"# sync: two-way",
		"sync=two-way src=src/com/example/demo/UserEventConsumer.java:4-4",
		"repo.save(x);",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expanded drop-in missing %q\n%s", want, got)
		}
	}
	// Idempotent: a second pass finds nothing to expand.
	again, _ := expandDropIns(cfg)
	if len(again) != 0 {
		t.Fatalf("expected no re-expansion, got %v", again)
	}
	// Edit the bookmark block -> syncs back to source.
	write(t, projPath, strings.Replace(got, "\t\trepo.save(x);", "\t\trepo.save(y);", 1))
	res, err := SyncProjection(cfg, projPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.ToSource != 1 {
		t.Fatalf("expected 1 source write, got %+v", res)
	}
	src := read(t, filepath.Join(dir, "src", "com", "example", "demo", "UserEventConsumer.java"))
	if !strings.Contains(src, "repo.save(y);") {
		t.Fatalf("edit not synced to source:\n%s", src)
	}
}

func TestSourceManifestAndDiffDetectChanges(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "src", "A.java"), "class A {}\n")
	write(t, filepath.Join(dir, "src", "B.java"), "class B {}\n")
	cfg := Config{Root: dir}

	m1, err := sourceManifest(cfg, "src")
	if err != nil {
		t.Fatal(err)
	}
	if len(m1) != 2 {
		t.Fatalf("expected 2 files, got %d", len(m1))
	}

	// Modify A, add C, remove B.
	write(t, filepath.Join(dir, "src", "A.java"), "class A { int x; }\n")
	write(t, filepath.Join(dir, "src", "C.java"), "class C {}\n")
	if err := os.Remove(filepath.Join(dir, "src", "B.java")); err != nil {
		t.Fatal(err)
	}
	m2, _ := sourceManifest(cfg, "src")
	added, modified, removed := diffManifest(m1, m2)
	if len(added) != 1 || added[0] != "C.java" {
		t.Fatalf("added=%v", added)
	}
	if len(modified) != 1 || modified[0] != "A.java" {
		t.Fatalf("modified=%v", modified)
	}
	if len(removed) != 1 || removed[0] != "B.java" {
		t.Fatalf("removed=%v", removed)
	}
	// No change -> empty diff (the skip-rebuild condition).
	a, mm, r := diffManifest(m2, m2)
	if len(a)+len(mm)+len(r) != 0 {
		t.Fatalf("expected no diff, got +%v ~%v -%v", a, mm, r)
	}
}

func TestDataFlowInlineUsesTrailingComments(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Root: ".", Lenses: []LensConfig{{
		Name: "df", Out: filepath.Join(dir, "df.projection"), Analyzer: "data-flow", SourceRoot: sampleRoot,
		Params: map[string]string{"file": "com/example/shop/OrderController.java", "line": "35", "var": "order", "mode": "fallback"},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(dir, "df.projection"))
	for _, want := range []string{
		"order.setShipping(\"express\");",
		"// <- mutates order",
		"// <- final use of order",
		"// <- source: parameter feeding order",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("data-flow inline missing %q\n%s", want, got)
		}
	}
	// Trailing comment must be padded past the code (not a leading // comment).
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "// <- mutates order") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				t.Fatalf("expected trailing comment, got leading: %q", line)
			}
		}
	}
}

func TestBookmarkRoundTripIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	// Copy a source file we can safely mutate.
	srcRel := "shop/Sample.java"
	srcAbs := filepath.Join(dir, "src", srcRel)
	write(t, srcAbs, "package shop;\n\nclass Sample {\n\tint value = 1;\n\tint other = 2;\n}\n")
	projPath := filepath.Join(dir, "out", "bookmark.projection")

	cfg := Config{Root: dir, Lenses: []LensConfig{{
		Name: "bm", Out: projPath, Analyzer: "bookmark", SourceRoot: "src",
		Params: map[string]string{"file": srcRel, "lines": "4-5"},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}

	// Edit the projection block (keep same line count) and sync back to source.
	proj := read(t, projPath)
	edited := strings.Replace(proj, "\tint value = 1;", "\tint value = 42;", 1)
	if edited == proj {
		t.Fatal("test setup: expected to find the block line to edit")
	}
	write(t, projPath, edited)

	res, err := SyncProjection(cfg, projPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.ToSource != 1 || len(res.Conflicts) != 0 {
		t.Fatalf("expected 1 source write, no conflicts; got %+v", res)
	}
	src := read(t, srcAbs)
	if !strings.Contains(src, "int value = 42;") {
		t.Fatalf("edit not synced to source:\n%s", src)
	}

	// A second sync is a no-op (idempotent) and leaves no leftovers.
	before := read(t, projPath)
	res2, err := SyncProjection(cfg, projPath)
	if err != nil {
		t.Fatal(err)
	}
	if res2.ToSource != 0 || res2.ToProjection != 0 {
		t.Fatalf("second sync should be a no-op, got %+v", res2)
	}
	after := read(t, projPath)
	if stripVolatile(before) != stripVolatile(after) {
		t.Fatalf("projection changed on idempotent re-sync:\n--- before\n%s\n--- after\n%s", before, after)
	}
	if strings.Count(after, "@@ shop/Sample.java") != 1 {
		t.Fatalf("expected exactly one block anchor (no leftovers):\n%s", after)
	}
}

// TestTwoWaySyncWritesNewTests is the "two-way for test writing" spike: a bookmark over a
// sentinel line in a *_test.go file is grown — the projection block is edited to append a
// new test function — and sync writes the expanded block back, so the test file gains a
// test. This is the round-trip an agent uses to author tests through a projection.
func TestTwoWaySyncWritesNewTests(t *testing.T) {
	dir := t.TempDir()
	srcRel := "pkg/widget_test.go"
	srcAbs := filepath.Join(dir, "src", srcRel)
	write(t, srcAbs, "package widget\n\nimport \"testing\"\n\nfunc TestExisting(t *testing.T) {}\n// add tests below\n")
	projPath := filepath.Join(dir, "out", "tests.projection")

	cfg := Config{Root: dir, Lenses: []LensConfig{{
		Name: "tw", Out: projPath, Analyzer: "bookmark", SourceRoot: "src",
		Params: map[string]string{"file": srcRel, "lines": "6-6"}, // the sentinel line
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}

	// Author a new test inside the projection block (the block grows from 1 to 4 lines).
	proj := read(t, projPath)
	edited := strings.Replace(proj,
		"// add tests below\n",
		"// add tests below\nfunc TestGenerated(t *testing.T) {\n\t_ = TestExisting\n}\n", 1)
	if edited == proj {
		t.Fatal("test setup: sentinel line not found in projection block")
	}
	write(t, projPath, edited)

	res, err := SyncProjection(cfg, projPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.ToSource != 1 || len(res.Conflicts) != 0 {
		t.Fatalf("expected 1 source write, no conflicts; got %+v", res)
	}
	src := read(t, srcAbs)
	if !strings.Contains(src, "func TestGenerated(t *testing.T) {") {
		t.Fatalf("generated test not synced into the test file:\n%s", src)
	}
	if !strings.Contains(src, "func TestExisting(t *testing.T) {}") {
		t.Fatalf("existing test should be preserved:\n%s", src)
	}
}

func TestExtractSyncDetectsConflict(t *testing.T) {
	dir := t.TempDir()
	srcRel := "shop/Sample.java"
	srcAbs := filepath.Join(dir, "src", srcRel)
	write(t, srcAbs, "package shop;\n\nclass Sample {\n\tint value = 1;\n\tint other = 2;\n}\n")
	projPath := filepath.Join(dir, "out", "extract.projection")
	cfg := Config{Root: dir, Lenses: []LensConfig{{
		Name: "ex", Out: projPath, Analyzer: "extract", SourceRoot: "src",
		Params: map[string]string{"file": srcRel, "lines": "4-5"},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	// Change BOTH sides independently.
	write(t, projPath, strings.Replace(read(t, projPath), "\tint value = 1;", "\tint value = 99;", 1))
	write(t, srcAbs, "package shop;\n\nclass Sample {\n\tint value = 7;\n\tint other = 2;\n}\n")

	res, err := SyncProjection(cfg, projPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.ToSource != 0 {
		t.Fatalf("expected a conflict and no source write, got %+v", res)
	}
	if !strings.Contains(read(t, srcAbs), "int value = 7;") {
		t.Fatal("source must be left untouched on conflict")
	}
}

func TestMenuAddsLensAndPersistsConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	cfg := Config{Root: ".", ProjectionsDir: filepath.Join(dir, "proj"), Lenses: nil}
	if err := SaveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _ = LoadConfig(configPath)
	// Script: choose 4 (entrypoints), name, source root, patterns, then 8 (quit).
	in := strings.NewReader("4\nsample-eps\nfixtures/spring-sample\n" + samplePatterns + "\n8\n")
	var out strings.Builder
	if err := RunMenu(cfg, configPath, in, &out); err != nil {
		t.Fatal(err)
	}
	saved, _ := LoadConfig(configPath)
	if len(saved.Lenses) != 1 || saved.Lenses[0].Analyzer != "entrypoints" {
		t.Fatalf("menu did not persist the lens: %+v", saved.Lenses)
	}
	got := read(t, filepath.Join(dir, "proj", "sample-eps.projection"))
	if !strings.Contains(got, "@KafkaListener") {
		t.Fatalf("generated projection missing entrypoints:\n%s", got)
	}
}

func TestProjectionHeaderCarriesSyncAndHash(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Root: ".", Lenses: []LensConfig{{
		Name: "ep", Out: filepath.Join(dir, "ep.projection"), Analyzer: "entrypoints", SourceRoot: "fixtures/spring-sample",
		Params: map[string]string{"patterns": samplePatterns},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(dir, "ep.projection"))
	for _, want := range []string{"# sync: view-only", "# source-hash: ", "# generated-at: "} {
		if !strings.Contains(got, want) {
			t.Fatalf("header missing %q\n%s", want, got)
		}
	}
}

// stripVolatile removes the generated-at header line so idempotent comparisons
// ignore the timestamp.
func stripVolatile(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "# generated-at:") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func TestGenericJSONLLensUsesSharedRenderer(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "tool.jsonl"),
		`{"kind":"block","id":"b1","file":"virtual","mode":"note","tool":"fixture","lines":["line 1"],"facts":["fact on block"]}`+"\n"+
			`{"kind":"fact","id":"f1","tool":"fixture","text":"top level fact"}`+"\n")

	cfg := Config{
		Root: dir,
		Lenses: []LensConfig{{
			Name:     "jsonl",
			Out:      ".projections/jsonl.projection",
			Analyzer: "jsonl",
			Input:    "tool.jsonl",
		}},
	}
	results, err := Run(cfg, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(results[0].Blocks) != 1 || len(results[0].Facts) != 1 {
		t.Fatalf("bad result shape: %#v", results)
	}
	got := read(t, filepath.Join(dir, ".projections/jsonl.projection"))
	for _, want := range []string{
		"# analyzer: jsonl",
		"@@ virtual#b1 [fixture.note hash=",
		"=> b1: fact on block",
		"=> fixture.f1: top level fact",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n%s", want, got)
		}
	}
}

func TestUnrolledProgramScatteredSyncWritesBackToOriginLine(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src/main/java/sample")
	write(t, filepath.Join(src, "App.java"), `package sample;

public class App {
    public Receipt build(String coupon, int amount) {
        Receipt r = new Receipt();
        new CodeStage().apply(r, coupon);
        if (amount >= 100) {
            new GoldLabelStage().apply(r, amount);
        } else {
            new LabelStage().apply(r, amount);
        }
        new TierStage().apply(r, amount);
        return r;
    }

    public String summary(String coupon, int amount) {
        Receipt r = build(coupon, amount);
        return r.getCode() + "/" + r.getLabel() + "/" + r.getTier();
    }
}
`)
	write(t, filepath.Join(src, "Receipt.java"), `package sample;

public class Receipt {
    public void setCode(String c) { }
    public void setLabel(String l) { }
    public void setTier(String t) { }
}
`)
	write(t, filepath.Join(src, "CodeStage.java"), `package sample;

public class CodeStage {
    public void apply(Receipt r, String coupon) {
        r.setCode(coupon.toUpperCase());
    }
}
`)
	write(t, filepath.Join(src, "GoldLabelStage.java"), `package sample;

public class GoldLabelStage {
    public void apply(Receipt r, int amount) {
        r.setLabel("$gold");
    }
}
`)
	write(t, filepath.Join(src, "LabelStage.java"), `package sample;

public class LabelStage {
    public void apply(Receipt r, int amount) {
        int net = amount - amount / 10;
        r.setLabel("$" + amount);
    }
}
`)
	write(t, filepath.Join(src, "TierStage.java"), `package sample;

public class TierStage {
    public void apply(Receipt r, int amount) {
        String t = amount >= 100 ? "GOLD" : "SILVER";
        r.setLabel(t);
    }
}
`)

	projPath := filepath.Join(dir, ".projections/unrolled.projection")
	cfg := Config{Root: dir, ProjectionsDir: ".projections", Lenses: []LensConfig{{
		Name:       "unrolled",
		Out:        ".projections/unrolled.projection",
		Analyzer:   "unrolled-program",
		SourceRoot: "src/main/java",
		Params: map[string]string{
			"file":   "sample/App.java",
			"method": "summary",
			"inputs": "coupon=save,amount=50",
		},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	got := read(t, projPath)
	for _, want := range []string{
		"# sync: two-way",
		"r.setCode(coupon.toUpperCase());",
		"int net = amount - amount / 10;",
		"r.setLabel(\"$\" + amount);",
		"r.setLabel(t);",
		"=> summary: origin ",
		"src=src/main/java/sample/TierStage.java:5",
		"=> unrolled-program.branch-1: sample/App.java:7 if (amount >= 100) -> else (decided from inputs)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "$gold") {
		t.Fatalf("selected amount=50 path should not include gold branch:\n%s", got)
	}

	edited := strings.Replace(got, "        r.setLabel(t);", "        r.setTier(t);", 1)
	if edited == got {
		t.Fatal("test did not edit projection")
	}
	if err := os.WriteFile(projPath, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := SyncProjection(cfg, projPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.ToSource != 1 || len(res.Conflicts) != 0 {
		t.Fatalf("sync result = %#v", res)
	}
	tier := read(t, filepath.Join(src, "TierStage.java"))
	if !strings.Contains(tier, "r.setTier(t);") {
		t.Fatalf("TierStage was not updated:\n%s", tier)
	}
}

func TestUnrolledProgramBranchChoicesToggleUndecidableCondition(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src/main/java/sample")
	write(t, filepath.Join(src, "App.java"), `package sample;

public class App {
    public String summary(int amount) {
        if (amount >= 100) {
            String label = "gold";
            label = label + "!";
            return label;
        } else {
            return "silver";
        }
    }
}
`)
	base := LensConfig{
		Name:       "unrolled",
		Analyzer:   "unrolled-program",
		SourceRoot: "src/main/java",
		Params: map[string]string{
			"file":          "sample/App.java",
			"method":        "summary",
			"branch_select": "1",
		},
	}
	cfg := Config{Root: dir, ProjectionsDir: ".projections"}
	p, err := ExecuteLens(cfg, DefaultRegistry(), base)
	if err != nil {
		t.Fatal(err)
	}
	choices := unrollChoices(p)
	if len(choices) != 1 {
		t.Fatalf("choices=%#v", choices)
	}
	if choices[0].Side != "then" {
		t.Fatalf("default side = %q, want then (longest branch)", choices[0].Side)
	}
	got := projectionBody(p)
	if !strings.Contains(got, `label = label + "!";`) || strings.Contains(got, `return "silver";`) {
		t.Fatalf("default path should use the longer then branch:\n%s", got)
	}

	forced := base
	forced.Params = map[string]string{}
	for k, v := range base.Params {
		forced.Params[k] = v
	}
	forced.Params["branches"] = choices[0].ID + "=else"
	p, err = ExecuteLens(cfg, DefaultRegistry(), forced)
	if err != nil {
		t.Fatal(err)
	}
	choices = unrollChoices(p)
	if len(choices) != 1 || choices[0].Side != "else" {
		t.Fatalf("forced choices=%#v", choices)
	}
	got = projectionBody(p)
	if !strings.Contains(got, `return "silver";`) || strings.Contains(got, `label = label + "!";`) {
		t.Fatalf("forced path should use else branch:\n%s", got)
	}
}

func TestUnrolledProgramInlineDepthAndCallSkips(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src/main/java/sample")
	write(t, filepath.Join(src, "App.java"), `package sample;

public class App {
    public String summary() {
        String v = helper();
        return v;
    }

    public String helper() {
        String v = leaf();
        return v;
    }

    public String leaf() {
        return "done";
    }
}
`)
	cfg := Config{Root: dir, ProjectionsDir: ".projections"}
	base := LensConfig{
		Name:       "unrolled",
		Analyzer:   "unrolled-program",
		SourceRoot: "src/main/java",
		Params: map[string]string{
			"file":   "sample/App.java",
			"method": "summary",
		},
	}
	run := func(depth string, skips string) (Projection, string) {
		lens := base
		lens.Params = map[string]string{}
		for k, v := range base.Params {
			lens.Params[k] = v
		}
		lens.Params["inline_depth"] = depth
		if skips != "" {
			lens.Params["inline_skips"] = skips
		}
		p, err := ExecuteLens(cfg, DefaultRegistry(), lens)
		if err != nil {
			t.Fatal(err)
		}
		return p, projectionBody(p)
	}

	p0, body0 := run("0", "")
	if !strings.Contains(body0, "String v = helper();") || strings.Contains(body0, "String v = leaf();") {
		t.Fatalf("depth 0 should keep direct call collapsed:\n%s", body0)
	}
	calls := unrollCalls(p0)
	if len(calls) != 1 || calls[0].ID != "sample/App.java:5" || calls[0].Expanded {
		t.Fatalf("depth 0 calls=%#v", calls)
	}

	p1, body1 := run("1", "")
	if !strings.Contains(body1, "String v = leaf();") || strings.Contains(body1, `return "done";`) {
		t.Fatalf("depth 1 should inline helper but not leaf:\n%s", body1)
	}
	calls = unrollCalls(p1)
	if len(calls) != 2 || !calls[0].Expanded || calls[1].Expanded {
		t.Fatalf("depth 1 calls=%#v", calls)
	}

	_, body2 := run("2", "")
	if !strings.Contains(body2, `"done";`) || strings.Contains(body2, "String v = leaf();") {
		t.Fatalf("depth 2 should inline nested leaf:\n%s", body2)
	}

	_, skipped := run("2", "sample/App.java:5")
	if !strings.Contains(skipped, "String v = helper();") || strings.Contains(skipped, `return "done";`) {
		t.Fatalf("inline_skips should roll helper back to source call:\n%s", skipped)
	}
}

func TestUnrolledProgramGoAdapterSyncsHelperLine(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	write(t, filepath.Join(src, "main.go"), `package sample

func Summary(coupon string, amount int) string {
	code := codeStage(coupon)
	label := labelStage(amount)
	tier := tierStage(amount)
	return code + "/" + label + "/" + tier
}

func codeStage(coupon string) string {
	return "SAVE"
}

func labelStage(amount int) string {
	net := amount - amount/10
	return "$" + "amount"
}

func tierStage(amount int) string {
	return "SILVER"
}
`)

	projPath := filepath.Join(dir, ".projections/go.projection")
	cfg := Config{Root: dir, ProjectionsDir: ".projections", Lenses: []LensConfig{{
		Name:       "go-unrolled",
		Out:        ".projections/go.projection",
		Analyzer:   "unrolled-program",
		SourceRoot: "src",
		Params: map[string]string{
			"file":   "main.go",
			"method": "Summary",
		},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	got := read(t, projPath)
	for _, want := range []string{
		"# analyzer: unrolled-program",
		"return \"SAVE\"",
		"net := amount - amount/10",
		"return \"$\" + \"amount\"",
		"src=src/main.go:15",
		"=> unrolled-program.scope: go adapter:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n%s", want, got)
		}
	}
	edited := strings.Replace(got, `	return "$" + "amount"`, `	return "$45"`, 1)
	if edited == got {
		t.Fatal("test did not edit projection")
	}
	if err := os.WriteFile(projPath, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := SyncProjection(cfg, projPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.ToSource != 1 || len(res.Conflicts) != 0 {
		t.Fatalf("sync result = %#v", res)
	}
	srcAfter := read(t, filepath.Join(src, "main.go"))
	if !strings.Contains(srcAfter, `return "$45"`) {
		t.Fatalf("go helper was not updated:\n%s", srcAfter)
	}
}

func TestGoSymbolsSelfLensIsGeneric(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Root: ".",
		Lenses: []LensConfig{{
			Name:       "self",
			Out:        filepath.Join(dir, "self.projection"),
			Analyzer:   "go-symbols",
			SourceRoot: "src",
			Include:    []string{"registry.go", "projection.go"},
		}},
	}
	results, err := Run(cfg, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected one projection")
	}
	got := read(t, filepath.Join(dir, "self.projection"))
	for _, want := range []string{
		"# analyzer: go-symbols",
		"@@ src/registry.go#functions [go-symbols.functions hash=",
		"func Run(cfg Config, registry Registry) ([]Projection, error)",
		"func ExecuteLens(cfg Config, registry Registry, lens LensConfig) (Projection, error)",
		"func RenderProjection(path string, p Projection) error",
		"=> go-symbols.core: Run -> ExecuteLens -> Analyzer -> RenderProjection is the generic path.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, `@PostMapping("/owners/new")`) || strings.Contains(got, "this.owners.save(owner)") {
		t.Fatalf("self projection leaked Petclinic source\n%s", got)
	}
}

func TestPetclinicJavaLensStillFindsFiveFlows(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Root: ".",
		Lenses: []LensConfig{{
			Name:       "pet",
			Out:        filepath.Join(dir, "pet.projection"),
			Analyzer:   "flow",
			SourceRoot: "spring-petclinic-main/src/main/java",
			Params: map[string]string{
				"entry":       "@PostMapping",
				"sink":        `\.\s*save\s*\(`,
				"file_suffix": "Controller.java",
				"mode":        "post-to-save",
				"tool":        "flow",
				"stop_calls":  "hasErrors,addFlashAttribute,rejectValue,getId,getName,getBirthDate,isAfter,now,isNew,getPet,addPet,save,hasText,state,setName,setBirthDate,setType",
			},
		}},
	}
	results, err := Run(cfg, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatal("expected one result")
	}
	if len(results[0].Blocks) != 5 {
		var ids []string
		for _, b := range results[0].Blocks {
			ids = append(ids, b.ID)
		}
		t.Fatalf("expected 5 flow blocks, got %d: %v", len(results[0].Blocks), ids)
	}
	got := read(t, filepath.Join(dir, "pet.projection"))
	for _, want := range []string{
		"OwnerController.processCreationForm",
		"OwnerController.processUpdateOwnerForm",
		"PetController.processCreationForm",
		"PetController.processUpdateForm",
		"VisitController.processNewVisitForm",
		"=> PetController.processUpdateForm: helper: processUpdateForm calls updatePetDetails, which reaches the sink",
		"=> VisitController.processNewVisitForm: sink:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n%s", want, got)
		}
	}
}

func TestJSEventsLensFindsGame3DSurface(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Root:        ".",
		ExcludeDirs: []string{".git", ".gocache", ".gomodcache", ".projections", "node_modules", "target", "build", "__MACOSX"},
		Lenses: []LensConfig{{
			Name:       "js-sample",
			Out:        filepath.Join(dir, "js-sample.projection"),
			Analyzer:   "js-events",
			SourceRoot: "fixtures/js-sample",
			Include:    []string{"core.js"},
		}},
	}
	results, err := Run(cfg, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected one result")
	}
	got := read(t, filepath.Join(dir, "js-sample.projection"))
	for _, want := range []string{
		"# analyzer: js-events",
		"@@ fixtures/js-sample/core.js#events [js-events.events hash=",
		"emit state:changed",
		"subscribe state:changed",
		"@@ fixtures/js-sample/core.js#registrations [js-events.registrations hash=",
		"registerAction",
		"@@ model#summary [js-events.summary hash=",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "__MACOSX") || strings.Contains(got, "._core.js") {
		t.Fatalf("projection leaked macOS metadata files:\n%s", got)
	}
}

func TestJoernVarFlowFallbackOwnerUpdate(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Root: ".",
		Lenses: []LensConfig{{
			Name:       "owner-var-flow",
			Out:        filepath.Join(dir, "owner.projection"),
			Analyzer:   "joern-var-flow",
			SourceRoot: "spring-petclinic-main/src/main/java",
			Params: map[string]string{
				"mode": "fallback",
				"file": "org/springframework/samples/petclinic/owner/OwnerController.java",
				"line": "156",
				"var":  "owner",
			},
		}},
	}
	results, err := Run(cfg, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || len(results[0].Blocks) != 1 {
		t.Fatalf("bad result shape: %#v", results)
	}
	got := read(t, filepath.Join(dir, "owner.projection"))
	for _, want := range []string{
		"# analyzer: joern-var-flow",
		"@@ org/springframework/samples/petclinic/owner/OwnerController.java#OwnerController.processUpdateOwnerForm:owner@156 [joern-var-flow:fallback.var-flow hash=",
		"// target variable owner at org/springframework/samples/petclinic/owner/OwnerController.java:156",
		"public String processUpdateOwnerForm",
		"if (result.hasErrors())",
		"if (!Objects.equals(owner.getId(), ownerId))",
		"owner.setId(ownerId);",
		"this.owners.save(owner);",
		"=> OwnerController.processUpdateOwnerForm:owner@156: mutation: owner.setId receives ownerId",
		"=> joern-var-flow.contributors:",
		"=> joern-var-flow.mode: used fallback static Java slicer",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n%s", want, got)
		}
	}
}

func TestJoernVarFlowFallbackPetCreateShowsPetContribution(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Root: ".",
		Lenses: []LensConfig{{
			Name:       "pet-create-owner-var-flow",
			Out:        filepath.Join(dir, "pet.projection"),
			Analyzer:   "joern-var-flow",
			SourceRoot: "spring-petclinic-main/src/main/java",
			Params: map[string]string{
				"mode": "fallback",
				"file": "org/springframework/samples/petclinic/owner/PetController.java",
				"line": "124",
				"var":  "owner",
			},
		}},
	}
	_, err := Run(cfg, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(dir, "pet.projection"))
	for _, want := range []string{
		"owner.addPet(pet);",
		"this.owners.save(owner);",
		"mutation: owner.addPet receives pet",
		"condition: if result.hasErrors()",
		"contributors:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q\n%s", want, got)
		}
	}
}

func TestUnknownAnalyzerErrorsBeforeRendering(t *testing.T) {
	cfg := Config{
		Root: ".",
		Lenses: []LensConfig{{
			Name:     "bad",
			Out:      filepath.Join(t.TempDir(), "bad.projection"),
			Analyzer: "missing-tool",
		}},
	}
	_, err := Run(cfg, DefaultRegistry())
	if err == nil || !strings.Contains(err.Error(), `unknown analyzer "missing-tool"`) {
		t.Fatalf("expected unknown analyzer error, got %v", err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
