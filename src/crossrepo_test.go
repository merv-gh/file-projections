package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRepos returns the two cross-repo fixtures (billing-lib + shop-app) as a
// workspace repos JSON param, using absolute paths so the test is location-stable.
func fixtureRepos(t *testing.T) string {
	t.Helper()
	abs := func(p string) string {
		a, err := filepath.Abs(filepath.Join("fixtures", p))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	repos := []WorkspaceRepo{
		{Name: "billing-lib", Path: abs("billing-lib"), Kind: "link"},
		{Name: "shop-app", Path: abs("shop-app"), Kind: "link"},
	}
	b, _ := json.Marshal(repos)
	return string(b)
}

// TestGradleDetectsGroupAndInternalDeps covers gradle.go: group parsing and the
// internal-vs-external dependency classification (the "same group = internal lib" rule).
func TestGradleDetectsGroupAndInternalDeps(t *testing.T) {
	shop, _ := filepath.Abs(filepath.Join("fixtures", "shop-app"))
	billing, _ := filepath.Abs(filepath.Join("fixtures", "billing-lib"))

	si := detectGradle(shop)
	if si.Group != "com.acme.shop" {
		t.Errorf("shop group = %q, want com.acme.shop", si.Group)
	}
	if !si.HasGradle {
		t.Error("shop should have gradle")
	}
	bi := detectGradle(billing)
	if bi.Group != "com.acme.billing" {
		t.Errorf("billing group = %q, want com.acme.billing", bi.Group)
	}

	repos := []WorkspaceRepo{
		{Name: "billing-lib", Path: billing, Group: bi.Group},
		{Name: "shop-app", Path: shop, Group: si.Group},
	}
	internal := internalDepsAmong(si, repos, "shop-app")
	found := false
	for _, n := range internal {
		if n == "billing-lib" {
			found = true
		}
	}
	if !found {
		t.Errorf("shop-app should internally depend on billing-lib (shared com.acme group); got %v", internal)
	}
	// guava (com.google) must NOT be internal.
	for _, n := range internal {
		if strings.Contains(n, "guava") || strings.Contains(n, "google") {
			t.Errorf("guava wrongly classified internal: %v", internal)
		}
	}
}

// TestTypeIndexResolvesCrossRepoOverride covers javatypes.go: the workspace type
// hierarchy must resolve AbstractPaymentService.pay() (declared in billing-lib) to
// the concrete RealPaymentService.pay() override (declared in shop-app).
func TestTypeIndexResolvesCrossRepoOverride(t *testing.T) {
	cfg := Config{ExcludeDirs: defaultExcludeDirs()}
	var ws Workspace
	if err := json.Unmarshal([]byte(fixtureRepos(t)), &ws.Repos); err != nil {
		t.Fatal(err)
	}
	idx := buildTypeIndex(cfg, &ws)

	abs := idx.findType("AbstractPaymentService")
	if abs == nil || !abs.Abstract {
		t.Fatalf("AbstractPaymentService not found or not abstract: %+v", abs)
	}
	real := idx.findType("RealPaymentService")
	if real == nil || real.Extends != "AbstractPaymentService" {
		t.Fatalf("RealPaymentService missing or wrong extends: %+v", real)
	}
	if real.Repo != "shop-app" || abs.Repo != "billing-lib" {
		t.Errorf("repos wrong: real=%s abs=%s", real.Repo, abs.Repo)
	}

	overrides := idx.ConcreteOverrides("AbstractPaymentService", "pay")
	if len(overrides) != 1 || overrides[0].Name != "RealPaymentService" {
		t.Fatalf("ConcreteOverrides(Abstract,pay) = %v, want [RealPaymentService]", typeNames(overrides))
	}
	if overrides[0].Repo != "shop-app" {
		t.Errorf("override resolved to wrong repo: %s", overrides[0].Repo)
	}
}

// TestTraceToLineCrossRepoDIPath is the headline test: tracing the ledger.write line
// in shop-app must produce path(s) that start at the library's @PostMapping
// controller, cross the repo boundary, and pass through the DI hop
// (AbstractPaymentService.pay → RealPaymentService.pay).
func TestTraceToLineCrossRepoDIPath(t *testing.T) {
	cfg := Config{ExcludeDirs: defaultExcludeDirs()}
	lens := LensConfig{
		Name:     "trace",
		Analyzer: "trace-to-line",
		Params: map[string]string{
			"repos": fixtureRepos(t),
			"file":  "RealPaymentService.java",
			"line":  "29",
		},
	}
	p, err := AnalyzeTraceToLine(cfg, lens)
	if err != nil {
		t.Fatal(err)
	}

	// Gather all rendered text (summary + every answer's Extra projection).
	var all strings.Builder
	for _, bl := range p.Blocks {
		for _, l := range bl.Lines {
			all.WriteString(l + "\n")
		}
	}
	if len(p.Extra) == 0 {
		t.Fatal("expected at least one answer (Extra projection)")
	}
	for _, ex := range p.Extra {
		for _, bl := range ex.Proj.Blocks {
			for _, l := range bl.Lines {
				all.WriteString(l + "\n")
			}
		}
	}
	got := all.String()

	checks := []struct {
		want, why string
	}{
		{"PaymentController", "path must start at the library controller"},
		{"PostMapping", "entrypoint annotation must be detected"},
		{"DI:", "dependency-inversion hop must be marked"},
		{"RealPaymentService", "must resolve to the concrete override in shop-app"},
		{"crosses repo boundary", "must mark the billing-lib → shop-app crossing"},
		{"ledger.write", "must reach the target line"},
	}
	for _, c := range checks {
		if !strings.Contains(got, c.want) {
			t.Errorf("trace output missing %q (%s).\n--- output ---\n%s", c.want, c.why, got)
		}
	}

	// Confidence must reflect DI resolution.
	var diConf bool
	for _, f := range p.Facts {
		if f.ID == "confidence" && strings.Contains(f.Text, "di") {
			diConf = true
		}
	}
	if !diConf {
		t.Errorf("expected a structural (di) confidence fact; facts=%+v", p.Facts)
	}

	// The express branch also reaches pay via process(); assume-guard for express
	// should appear in at least one answer.
	if !strings.Contains(got, "assume:") {
		t.Errorf("expected guard assumptions in the path; output:\n%s", got)
	}
}

func typeNames(ts []*JavaType) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// TestTraceBySymbolFromConfigProject covers the Phase 4 UX path: a config workspace
// project drives the trace, and the target is given as a SYMBOL (no file/line/repo).
func TestTraceBySymbolFromConfigProject(t *testing.T) {
	billing, _ := filepath.Abs(filepath.Join("fixtures", "billing-lib"))
	shop, _ := filepath.Abs(filepath.Join("fixtures", "shop-app"))
	cfg := Config{
		ExcludeDirs: defaultExcludeDirs(),
		Workspace: &WorkspaceConfig{
			Active: "shop",
			Projects: []ProjectConfig{{
				Name: "shop",
				Repos: []RepoConfig{
					{Name: "shop-app", Path: shop, Role: "app"},
					{Name: "billing-lib", Path: billing, Role: "library"},
				},
			}},
		},
	}
	// Symbol-only input; workspace resolved from the active config project.
	lens := LensConfig{Name: "trace", Analyzer: "trace-to-line", Params: map[string]string{"symbol": "RealPaymentService.pay"}}
	p, err := AnalyzeTraceToLine(cfg, lens)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, ex := range p.Extra {
		for _, bl := range ex.Proj.Blocks {
			for _, l := range bl.Lines {
				all.WriteString(l + "\n")
			}
		}
	}
	got := all.String()
	for _, want := range []string{"PaymentController", "DI:", "crosses repo boundary", "ledger.write"} {
		if !strings.Contains(got, want) {
			t.Errorf("symbol trace missing %q\n%s", want, got)
		}
	}

	// include_libraries=false restricts to the app repo: no library entrypoint, so
	// the cross-repo path should disappear (no controller, no boundary crossing).
	lens.Params["include_libraries"] = "false"
	p2, err := AnalyzeTraceToLine(cfg, lens)
	if err != nil {
		t.Fatal(err)
	}
	var all2 strings.Builder
	for _, ex := range p2.Extra {
		for _, bl := range ex.Proj.Blocks {
			for _, l := range bl.Lines {
				all2.WriteString(l + "\n")
			}
		}
	}
	if strings.Contains(all2.String(), "crosses repo boundary") {
		t.Errorf("app-only trace should not cross repo boundary:\n%s", all2.String())
	}
}

// TestServiceGraphCrossRepoFromProject covers the cross-repo service graph derived
// from a config project: the library controller and the app override appear as one
// graph with a cross-repo edge.
func TestServiceGraphCrossRepoFromProject(t *testing.T) {
	billing, _ := filepath.Abs(filepath.Join("fixtures", "billing-lib"))
	shop, _ := filepath.Abs(filepath.Join("fixtures", "shop-app"))
	cfg := Config{
		ExcludeDirs: defaultExcludeDirs(),
		Workspace: &WorkspaceConfig{
			Active: "shop",
			Projects: []ProjectConfig{{
				Name: "shop",
				Repos: []RepoConfig{
					{Name: "shop-app", Path: shop, Role: "app"},
					{Name: "billing-lib", Path: billing, Role: "library"},
				},
			}},
		},
	}
	p, err := AnalyzeServiceGraph(cfg, LensConfig{Name: "g", Analyzer: "service-graph"})
	if err != nil {
		t.Fatal(err)
	}
	var graph string
	for _, f := range p.Facts {
		if f.ID == "graph" {
			graph = f.Text
		}
	}
	for _, want := range []string{"PaymentController", "RealPaymentService", "\"cross\":true", "billing-lib", "shop-app"} {
		if !strings.Contains(graph, want) {
			t.Errorf("cross-repo service graph missing %q\n%s", want, graph)
		}
	}
}
