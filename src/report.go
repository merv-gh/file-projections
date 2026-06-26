package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strings"
)

// `report` — bake a service-graph (with its first-class side-effect tags) plus a
// findings markdown file into ONE self-contained HTML file: static inline SVG, no
// network, no external assets. A single shareable artifact for a review.
//
// Why static SVG (not the live UI): a report must open anywhere (email, PR, gist)
// with no server, and stay readable years later. The same effect taxonomy and colors
// the UI uses are reproduced here so the report and the live studio match.

var seColor = map[string]string{
	SEDatabase: "#b07a2b", SENetwork: "#3f6f9f", SEFileWrite: "#b54848",
	SEFileRead: "#7a8a3a", SEProcess: "#7a4fa0",
}

// RunReport renders a service-graph lens + a findings markdown into a self-contained HTML.
func RunReport(args []string, out *os.File) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", "config.json", "config file")
	lensName := fs.String("lens", "", "service-graph lens name (default: first in config)")
	sourceRoot := fs.String("source-root", "", "source root (overrides lens) for an ad-hoc graph")
	servicesJSON := fs.String("services", "", "ad-hoc services JSON [{name,root,lang}] (with -source-root)")
	findings := fs.String("findings", "", "markdown file to embed as the review")
	title := fs.String("title", "file-projections report", "report title")
	outPath := fs.String("out", "report.html", "output HTML file")
	effectsOnly := fs.Bool("effects-only", false, "only include nodes that perform a side effect (+neighbors)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		cfg = Config{Root: ".", ProjectionsDir: ".projections", ExcludeDirs: defaultExcludeDirs()}
	}

	// Resolve the graph: ad-hoc (-source-root + -services) or a named/first config lens.
	var lens LensConfig
	if *sourceRoot != "" && *servicesJSON != "" {
		lens = LensConfig{Name: "report", Analyzer: "service-graph", SourceRoot: *sourceRoot,
			Params: map[string]string{"services": *servicesJSON}}
	} else {
		found := false
		for _, l := range cfg.Lenses {
			if l.Analyzer == "service-graph" && (*lensName == "" || l.Name == *lensName) {
				lens, found = l, true
				break
			}
		}
		if !found {
			return fmt.Errorf("report: no service-graph lens (use -lens NAME, or -source-root + -services)")
		}
	}

	p, err := ExecuteLens(cfg, DefaultRegistry(), lens)
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	var graph sgGraph
	for _, f := range p.Facts {
		if f.ID == "graph" && f.Tool == "service-graph" {
			if err := json.Unmarshal([]byte(f.Text), &graph); err != nil {
				return fmt.Errorf("report: bad graph json: %w", err)
			}
		}
	}
	if *effectsOnly {
		graph = effectSubgraph(graph)
	}

	var findingsHTML string
	if *findings != "" {
		md, err := os.ReadFile(*findings)
		if err != nil {
			return fmt.Errorf("report: findings: %w", err)
		}
		findingsHTML = miniMarkdown(string(md))
	}

	htmlDoc := buildReportHTML(*title, lens, graph, findingsHTML)
	if err := os.WriteFile(*outPath, []byte(htmlDoc), 0o644); err != nil {
		return err
	}
	eff := 0
	for _, n := range graph.Nodes {
		if len(n.Effects) > 0 {
			eff++
		}
	}
	fmt.Fprintf(out, "wrote %s (%d nodes, %d edges, %d with side effects)\n", *outPath, len(graph.Nodes), len(graph.Edges), eff)
	return nil
}

// effectSubgraph keeps nodes that perform a side effect plus their direct neighbors,
// so a shared report focuses on the parts that touch the outside world.
func effectSubgraph(g sgGraph) sgGraph {
	keep := map[string]bool{}
	for _, n := range g.Nodes {
		if len(n.Effects) > 0 {
			keep[n.ID] = true
		}
	}
	for _, e := range g.Edges {
		if keep[e.From] {
			keep[e.To] = true
		}
		if keep[e.To] {
			keep[e.From] = true
		}
	}
	var out sgGraph
	out.Services = g.Services
	for _, n := range g.Nodes {
		if keep[n.ID] {
			out.Nodes = append(out.Nodes, n)
		}
	}
	for _, e := range g.Edges {
		if keep[e.From] && keep[e.To] {
			out.Edges = append(out.Edges, e)
		}
	}
	return out
}

// renderGraphSVG lays the graph out in service columns (same model as the UI) and
// returns static SVG with side-effect dots per node.
func renderGraphSVG(g sgGraph) string {
	const colW, rowH, padTop, padX, nodeW, nodeH = 270, 26, 40, 16, 220, 20
	services := make([]string, 0, len(g.Services))
	for _, s := range g.Services {
		services = append(services, s.Name)
	}
	byCol := map[string][]sgNode{}
	for _, n := range g.Nodes {
		byCol[n.Service] = append(byCol[n.Service], n)
	}
	pos := map[string][2]int{}
	col := map[string]int{}
	maxRows := 0
	for ci, s := range services {
		for ri, n := range byCol[s] {
			pos[n.ID] = [2]int{padX + ci*colW, padTop + ri*rowH}
			col[n.ID] = ci
		}
		if len(byCol[s]) > maxRows {
			maxRows = len(byCol[s])
		}
	}
	w := padX*2 + len(services)*colW
	h := padTop + maxRows*rowH + 24
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" width="%d" height="%d" xmlns="http://www.w3.org/2000/svg" font-family="ui-monospace,Menlo,monospace">`, w, h, w, h)
	// column headers
	for ci, s := range services {
		x := padX + ci*colW - 6
		fmt.Fprintf(&b, `<rect x="%d" y="6" width="%d" height="%d" rx="8" fill="#f4f0e8"/>`, x, nodeW+12, h-12)
		fmt.Fprintf(&b, `<text x="%d" y="24" font-size="11" font-weight="600" fill="#746b5d">%s (%d)</text>`, x+8, html.EscapeString(s), len(byCol[s]))
	}
	// edges
	for _, e := range g.Edges {
		a, ok1 := pos[e.From]
		c, ok2 := pos[e.To]
		if !ok1 || !ok2 {
			continue
		}
		ax, ay := a[0]+nodeW, a[1]+nodeH/2
		bx, by := c[0], c[1]+nodeH/2
		stroke := "#b9b0a0"
		if e.Kind == "api-call" {
			stroke = "#3f6f9f"
		} else if e.Kind == "registers" {
			stroke = "#c6a14a"
		}
		mx := (ax + bx) / 2
		fmt.Fprintf(&b, `<path d="M%d,%d C%d,%d %d,%d %d,%d" fill="none" stroke="%s" stroke-width="%s" opacity="0.8"/>`,
			ax, ay, mx, ay, mx, by, bx, by, stroke, map[bool]string{true: "2", false: "1"}[e.Kind == "api-call"])
	}
	// nodes + effect dots
	for _, n := range g.Nodes {
		p, ok := pos[n.ID]
		if !ok {
			continue
		}
		fill, stroke := "#efe9dc", "#cec6b8"
		if n.Kind == "entrypoint" {
			fill, stroke = "#dce8f3", "#7fa8cf"
		} else if n.Kind == "router" {
			fill, stroke = "#f3e6c4", "#d8b85a"
		}
		label := n.Label
		if len(n.Effects) > 0 && len(label) > 24 {
			label = "…" + label[len(label)-23:]
		} else if len(label) > 30 {
			label = "…" + label[len(label)-29:]
		}
		fmt.Fprintf(&b, `<g transform="translate(%d,%d)">`, p[0], p[1])
		fmt.Fprintf(&b, `<rect width="%d" height="%d" rx="5" fill="%s" stroke="%s" stroke-width="%s"/>`,
			nodeW, nodeH, fill, stroke, map[bool]string{true: "1.5", false: "1"}[len(n.Effects) > 0])
		fmt.Fprintf(&b, `<text x="7" y="14" font-size="11" fill="#25231f">%s</text>`, html.EscapeString(label))
		for i, k := range n.Effects {
			fmt.Fprintf(&b, `<circle cx="%d" cy="%d" r="4" fill="%s" stroke="#fbf8f0" stroke-width="0.5"><title>%s</title></circle>`,
				nodeW-9-i*11, nodeH/2, seColor[k], html.EscapeString(k))
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func buildReportHTML(title string, lens LensConfig, g sgGraph, findingsHTML string) string {
	// Effect legend + summary counts.
	counts := map[string]int{}
	for _, n := range g.Nodes {
		for _, k := range n.Effects {
			counts[k]++
		}
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var legend strings.Builder
	for _, k := range kinds {
		fmt.Fprintf(&legend, `<span class="lg"><i style="background:%s"></i>%s · %d</span>`, seColor[k], html.EscapeString(k), counts[k])
	}
	graphJSON, _ := json.Marshal(g)

	return `<!doctype html><html><head><meta charset=utf-8><title>` + html.EscapeString(title) + `</title>
<style>
body{margin:0;font:14px/1.6 ui-sans-serif,-apple-system,Segoe UI,Roboto,sans-serif;background:#ebe7dd;color:#25231f}
header{padding:1rem 1.5rem;border-bottom:1px solid #cec6b8;background:#f4f0e8}
header b{font-size:1.2rem}
main{max-width:1100px;margin:0 auto;padding:1.5rem}
h1,h2,h3{line-height:1.25}h2{margin-top:1.8rem;border-bottom:1px solid #cec6b8;padding-bottom:.3rem}
.meta{color:#746b5d;font-size:.85rem}
.legend{display:flex;gap:1rem;flex-wrap:wrap;margin:.6rem 0}
.lg{display:inline-flex;align-items:center;gap:.35rem;font-size:.8rem;color:#5a5346}
.lg i{width:.8rem;height:.8rem;border-radius:50%;display:inline-block}
.graphbox{border:1px solid #cec6b8;border-radius:10px;background:#fbf8f0;overflow:auto;padding:.5rem}
pre{background:#1f2329;color:#e6e1d6;border-radius:8px;padding:.8rem 1rem;overflow:auto;font:12.5px/1.5 ui-monospace,Menlo,monospace}
code{font-family:ui-monospace,Menlo,monospace;background:#e2ddd2;padding:.05rem .35rem;border-radius:4px;font-size:.9em}
pre code{background:none;padding:0}
.review{background:#f4f0e8;border:1px solid #cec6b8;border-radius:10px;padding:.5rem 1.5rem;margin-top:1rem}
a{color:#3f6f9f}
.anchor{opacity:0;text-decoration:none;color:#a99;font-weight:400;margin-left:.3rem}
h2:hover .anchor,h3:hover .anchor{opacity:1}
:target{background:#fff3cd;scroll-margin-top:1rem}
.foot{color:#746b5d;font-size:.8rem;margin-top:2rem;border-top:1px solid #cec6b8;padding-top:.6rem}
</style></head><body>
<header><b>` + html.EscapeString(title) + `</b>
<div class=meta>service-graph lens <code>` + html.EscapeString(lens.Name) + `</code> · source root <code>` + html.EscapeString(lens.SourceRoot) + `</code> · ` + fmt.Sprintf("%d nodes, %d edges", len(g.Nodes), len(g.Edges)) + `</div></header>
<main>
<h2>Side-effect map</h2>
<p class=meta>Colored dots mark what each file touches. Self-contained — no network, share this file as-is.</p>
<div class=legend>` + legend.String() + `</div>
<div class=graphbox>` + renderGraphSVG(g) + `</div>
` + reviewSection(findingsHTML) + `
<div class=foot>Generated by file-projections · graph data is embedded below for re-rendering.
<script type="application/json" id="graph">` + string(graphJSON) + `</script></div>
</main></body></html>`
}

func reviewSection(findingsHTML string) string {
	if findingsHTML == "" {
		return ""
	}
	return `<h2>Review &amp; findings</h2><div class=review>` + findingsHTML + `</div>`
}

// miniMarkdown renders a safe subset of markdown (headings, fenced code, inline code,
// lists, links, paragraphs) to HTML. Deliberately tiny and dependency-free — enough
// for a findings doc, not a general renderer. All text is HTML-escaped first.
var (
	mdInlineCode = regexp.MustCompile("`([^`]+)`")
	mdLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	mdBold       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	slugStripRE  = regexp.MustCompile(`[^a-z0-9]+`)
	idTokenRE    = regexp.MustCompile(`^[a-z]+[0-9]+$`) // e.g. f1, f12 — a short finding id
)

// heading renders a markdown heading with a stable slug id and a hover "#" anchor,
// so an individual finding is shareable via report.html#f1 — the user asked for the
// parts to be shareable.
func heading(tag, text string, inline func(string) string) string {
	slug := strings.Trim(slugStripRE.ReplaceAllString(strings.ToLower(text), "-"), "-")
	// If the heading leads with a short id token (e.g. "F1", "F12"), use just that as
	// the anchor so findings get stable short links (report.html#f1).
	if i := strings.Index(slug, "-"); i > 0 && i <= 4 {
		if lead := slug[:i]; idTokenRE.MatchString(lead) {
			slug = lead
		}
	}
	return fmt.Sprintf(`<%s id="%s">%s <a class="anchor" href="#%s">#</a></%s>`+"\n",
		tag, html.EscapeString(slug), inline(text), html.EscapeString(slug), tag)
}

func miniMarkdown(src string) string {
	lines := strings.Split(src, "\n")
	var b strings.Builder
	inCode := false
	inList := false
	closeList := func() {
		if inList {
			b.WriteString("</ul>\n")
			inList = false
		}
	}
	inline := func(s string) string {
		s = html.EscapeString(s)
		s = mdInlineCode.ReplaceAllString(s, "<code>$1</code>")
		s = mdBold.ReplaceAllString(s, "<strong>$1</strong>")
		s = mdLink.ReplaceAllString(s, `<a href="$2">$1</a>`)
		return s
	}
	for _, ln := range lines {
		if strings.HasPrefix(ln, "```") {
			if inCode {
				b.WriteString("</code></pre>\n")
				inCode = false
			} else {
				closeList()
				b.WriteString("<pre><code>")
				inCode = true
			}
			continue
		}
		if inCode {
			b.WriteString(html.EscapeString(ln) + "\n")
			continue
		}
		t := strings.TrimSpace(ln)
		switch {
		case t == "":
			closeList()
		case strings.HasPrefix(t, "### "):
			closeList()
			b.WriteString(heading("h3", t[4:], inline))
		case strings.HasPrefix(t, "## "):
			closeList()
			b.WriteString(heading("h3", t[3:], inline))
		case strings.HasPrefix(t, "# "):
			closeList()
			b.WriteString(heading("h2", t[2:], inline))
		case t == "---":
			closeList()
			b.WriteString("<hr>\n")
		case strings.HasPrefix(t, "- "):
			if !inList {
				b.WriteString("<ul>\n")
				inList = true
			}
			b.WriteString("<li>" + inline(t[2:]) + "</li>\n")
		default:
			closeList()
			b.WriteString("<p>" + inline(t) + "</p>\n")
		}
	}
	closeList()
	if inCode {
		b.WriteString("</code></pre>\n")
	}
	return b.String()
}
