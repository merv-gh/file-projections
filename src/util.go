package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Small shared helpers (fs, hashing, string utils).

func LensOut(cfg Config, lens LensConfig) string {
	if lens.Out != "" {
		if filepath.IsAbs(lens.Out) {
			return lens.Out
		}
		return filepath.Join(cfg.Root, lens.Out)
	}
	name := lens.Name
	if filepath.Ext(name) == "" {
		name += ".projection"
	}
	return filepath.Join(cfg.Root, cfg.ProjectionsDir, name)
}

// renderAnchor builds the "@@ ..." block header. Two-way extract blocks carry
// extra metadata (sync/src/srchash) so SyncProjection can map edits back to source.
func renderAnchor(block ProjectionBlock) string {
	anchor := fmt.Sprintf("@@ %s#%s [%s.%s hash=%s", block.File, block.ID, block.Tool, block.Mode, block.Hash)
	if block.Sync == "two-way" && block.SrcFile != "" {
		anchor += fmt.Sprintf(" sync=two-way src=%s:%d-%d srchash=%s", block.SrcFile, block.SrcStart, block.SrcEnd, block.SrcHash)
	}
	return anchor + "]"
}

func mergeStopSet(base map[string]bool, csv string) map[string]bool {
	out := map[string]bool{}
	for k := range base {
		out[k] = true
	}
	for _, s := range splitCSV(csv) {
		out[s] = true
	}
	return out
}

func methodMatchesEntry(m JavaMethod, re *regexp.Regexp) bool {
	for _, a := range m.Annotations {
		if re.MatchString(a) {
			return true
		}
	}
	return len(m.Lines) > 0 && re.MatchString(m.Lines[0])
}

func nearestIf(lines []string, idx int) string {
	depth := 0
	for i := idx - 1; i >= 0 && i >= idx-8; i-- {
		trim := strings.TrimSpace(lines[i])
		for _, ch := range trim {
			if ch == '}' {
				depth++
			}
			if ch == '{' && depth > 0 {
				depth--
			}
		}
		if depth == 0 {
			if m := ifRE.FindStringSubmatch(trim); m != nil {
				return "if " + strings.TrimSpace(m[1])
			}
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// dockerMount builds the `-v host:/src` bind spec. The host path uses forward slashes so
// Docker Desktop on Windows accepts it (e.g. C:/Users/me/proj:/src).
func dockerMount(absRoot string) string {
	return filepath.ToSlash(absRoot) + ":/src"
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "… (truncated)")
	}
	return strings.Join(lines, "\n")
}

func perfErr(phase string, budget time.Duration, ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("perf: %s exceeded the %s budget (killed). Raise -timeout or -jvm, or split into smaller source roots", phase, budget)
	}
	return fmt.Errorf("perf: %s failed: %w", phase, err)
}

func isGitURL(s string) bool {
	return strings.Contains(s, "://") || strings.HasPrefix(s, "git@") || strings.HasSuffix(s, ".git")
}

// frontendJVMFlags converts jvm_args (e.g. "-Xmx6g") into frontend -J flags
// (e.g. "-J-Xmx6g"), which set the frontend JVM heap directly.
func frontendJVMFlags(cfg Config) []string {
	var out []string
	for _, t := range strings.Fields(joernJVMArgs(cfg)) {
		out = append(out, "-J"+t)
	}
	return out
}

func identifiers(s string) []string {
	s = stripJavaStrings(s)
	var out []string
	matches := javaIdentRE.FindAllStringIndex(s, -1)
	for _, mm := range matches {
		id := s[mm[0]:mm[1]]
		if !isJavaValueIdent(id) {
			continue
		}
		prev := byteBefore(s, mm[0])
		next := byteAfterSpaces(s, mm[1])
		// Skip method/property names in obj.method(...), but keep obj.
		if prev == '.' || next == '(' {
			continue
		}
		out = append(out, id)
	}
	return dedupe(out)
}

func byteBefore(s string, idx int) byte {
	for i := idx - 1; i >= 0; i-- {
		if s[i] == ' ' || s[i] == '\t' {
			continue
		}
		return s[i]
	}
	return 0
}

func byteAfterSpaces(s string, idx int) byte {
	for i := idx; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			continue
		}
		return s[i]
	}
	return 0
}

func anyContributor(ids []string, c map[string]bool) bool {
	for _, id := range ids {
		if c[id] {
			return true
		}
	}
	return false
}

func filterValueIdents(ids []string) []string {
	var out []string
	for _, id := range ids {
		if isJavaValueIdent(id) {
			out = append(out, id)
		}
	}
	return dedupe(out)
}

func uniqueHits(in []lineHit) []lineHit {
	seen := map[int]bool{}
	var out []lineHit
	for _, h := range in {
		if h.Line <= 0 || seen[h.Line] {
			continue
		}
		seen[h.Line] = true
		out = append(out, h)
	}
	return out
}

func mapKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func compactJSName(line string) string {
	line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return strings.Trim(fields[1], "{};,")
	}
	return "export"
}

// Shared source utilities.
func findClosingBrace(lines []string, openLine int) (int, error) {
	depth := 0
	seen := false
	for i := openLine; i < len(lines); i++ {
		for _, ch := range stripLineComment(lines[i]) {
			switch ch {
			case '{':
				depth++
				seen = true
			case '}':
				if !seen {
					// A leading '}' before any '{' on this line belongs to a prior
					// block (e.g. the "} else {" / "} catch" idiom); ignore it.
					continue
				}
				depth--
				if depth == 0 {
					return i, nil
				}
			}
		}
	}
	return -1, errors.New("unclosed brace")
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

func trimBeforeBrace(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "{"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}

func stripLineComment(s string) string {
	if idx := strings.Index(s, "//"); idx >= 0 {
		return s[:idx]
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func hash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

// ripgrep searches for a regex under cfg.Root/root using rg, falling back to a
// stdlib regex scan when rg is unavailable. rg exit code 1 (no matches) is not an error.
func ripgrep(cfg Config, pattern, root string) ([]grepHit, error) {
	if _, err := exec.LookPath("rg"); err != nil {
		if tc, ok := cfg.Tools["rg"]; ok && tc.Image != "" {
			if out, derr := runTool(cfg, "rg", "-n", "--no-heading", "--color=never", "-e", pattern, root); derr == nil {
				return parseGrep(out), nil
			}
		}
		return scanRegex(cfg, pattern, root)
	}
	cmd := exec.Command("rg", "-n", "--no-heading", "--color=never", "-e", pattern, root)
	cmd.Dir = cfg.Root
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("rg failed: %w\n%s", err, out)
	}
	return parseGrep(out), nil
}

func parseGrep(out []byte) []grepHit {
	var hits []grepHit
	for _, ln := range strings.Split(string(out), "\n") {
		if ln == "" {
			continue
		}
		p1 := strings.IndexByte(ln, ':')
		if p1 < 0 {
			continue
		}
		rest := ln[p1+1:]
		p2 := strings.IndexByte(rest, ':')
		if p2 < 0 {
			continue
		}
		num, err := strconv.Atoi(rest[:p2])
		if err != nil {
			continue
		}
		hits = append(hits, grepHit{File: filepath.ToSlash(ln[:p1]), Line: num, Text: strings.TrimSpace(rest[p2+1:])})
	}
	return hits
}

// globToRegex converts a shell-glob-ish sink pattern (e.g. *kafka*.send) to a
// regex. '*' matches an identifier/dot run; '.' is literal; other regex meta is escaped.
func globToRegex(g string) string {
	var b strings.Builder
	for _, r := range g {
		switch r {
		case '*':
			b.WriteString(`[A-Za-z0-9_.$]*`)
		case '.':
			b.WriteString(`\.`)
		default:
			if strings.ContainsRune(`+()[]{}^$|?\`, r) {
				b.WriteRune('\\')
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ============================================================================
// Entrypoints / Exitpoints lenses (rg-driven, view-only)
// ============================================================================

// parsePatternParam parses "label=regex;label=regex" into labeled patterns.
func parsePatternParam(s string) []labeledPattern {
	var out []labeledPattern
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '='); i > 0 {
			out = append(out, labeledPattern{strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])})
		} else {
			out = append(out, labeledPattern{part, part})
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// unixDate formats a unix timestamp as YYYY-MM-DD (used by the git-blame lens).
func unixDate(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02")
}

// codeLoc lays out a row as the exact code left-aligned, padded to locCol, then the
// "<filename>:<line>" locator. Long code just gets a two-space gap.
func codeLoc(code, file string, line int) string {
	code = strings.TrimRight(code, " \t")
	loc := fmt.Sprintf("%s:%d", filepath.Base(file), line)
	if len(code) >= locCol {
		return code + "  " + loc
	}
	return code + strings.Repeat(" ", locCol-len(code)) + loc
}

// reLocLines reformats the joern scripts' "<line>\t<code>" rows into the code-first /
// file:line layout. Rows without a tab pass through unchanged.
func reLocLines(in []string, file string) []string {
	out := make([]string, 0, len(in))
	for _, ln := range in {
		if tab := strings.IndexByte(ln, '\t'); tab >= 0 {
			out = append(out, codeLoc(ln[tab+1:], file, atoi(ln[:tab])))
		} else {
			out = append(out, ln)
		}
	}
	return out
}

// formatRow substitutes {file}/{line}/{label}/{code} placeholders in a line template.
func formatRow(tmpl, file string, line int, label, code string) string {
	r := strings.NewReplacer(
		"{file}", file,
		"{line}", strconv.Itoa(line),
		"{label}", label,
		"{code}", code,
	)
	return r.Replace(tmpl)
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ============================================================================
// Control-flow lens: ways from method entry to a target line, branch per file
// ============================================================================

func isIfHeader(trim string) bool {
	return strings.HasPrefix(trim, "if ") || strings.HasPrefix(trim, "if(")
}

func isExitStmt(trim string) bool {
	return strings.HasPrefix(trim, "return") || strings.HasPrefix(trim, "throw")
}

func firstBraceLine(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.Contains(stripLineComment(lines[i]), "{") {
			return i
		}
	}
	return from
}

func matchParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func enumeratePaths(nodes []cfgNode, target int) []cfgPath {
	var out []cfgPath
	for _, p := range walkNodes(nodes, 0, target) {
		if p.reached && !p.dead {
			out = append(out, p)
		}
	}
	return out
}

func prependEvent(ev cfgEvent, rest []cfgEvent) []cfgEvent {
	out := make([]cfgEvent, 0, len(rest)+1)
	out = append(out, ev)
	out = append(out, rest...)
	return out
}

func concatEvents(head cfgEvent, mid, tail []cfgEvent) []cfgEvent {
	out := make([]cfgEvent, 0, len(mid)+len(tail)+1)
	out = append(out, head)
	out = append(out, mid...)
	out = append(out, tail...)
	return out
}

// ============================================================================
// Data-flow lens: contributing lines with trailing padded comments (view-only)
// ============================================================================

func dataFlowNote(why, varName string) string {
	switch why {
	case "method signature":
		return "<- source: parameter feeding " + varName
	case "assignment":
		return "<- assigns into the flow"
	case "object mutation":
		return "<- mutates " + varName
	case "reachability condition":
		return "<- guards whether the value is set"
	case "early return":
		return "<- could skip this value"
	case "target line":
		return "<- final use of " + varName
	default:
		return "<- " + why
	}
}

// padComment right-pads code to a fixed column then appends a trailing // comment,
// so contributing lines stay scannable. Tabs count as 4 columns for alignment.
func padComment(code, note string) string {
	width := 0
	for _, r := range code {
		if r == '\t' {
			width += 4
		} else {
			width++
		}
	}
	pad := dataFlowCommentCol - width
	if pad < 1 {
		pad = 1
	}
	return code + strings.Repeat(" ", pad) + "// " + note
}

// ============================================================================
// Extract lens + two-way sync engine
// ============================================================================

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func parseLineRange(s string) (int, int) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '-'); i > 0 {
		return atoi(s[:i]), atoi(s[i+1:])
	}
	n := atoi(s)
	return n, n
}

// bookmarkTarget derives the requested source target from a two-way block's header: the
// display file (before #, repo- or package-relative) and the a-b range in the ID. This is
// what makes editing the header — the class/file or the line range — refresh the view.
func bookmarkTarget(cfg Config, blk ParsedBlock) (srcFile string, a, b int) {
	a, b = blk.SrcStart, blk.SrcEnd
	if m := idRangeRE.FindStringSubmatch(blk.ID); m != nil {
		a, b = atoi(m[1]), atoi(m[2])
	}
	srcFile = blk.SrcFile
	if root, rel, err := resolveSourceFile(cfg, blk.File); err == nil {
		srcFile = filepath.ToSlash(filepath.Join(root, rel))
	}
	return srcFile, a, b
}

// readHeaderMeta extracts the lens name, analyzer, and sync policy from a projection header.
func readHeaderMeta(projPath string) (name, analyzer, sync string) {
	lines, err := readLines(projPath)
	if err != nil {
		return "", "", ""
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "# lens: "):
			name = strings.TrimPrefix(l, "# lens: ")
		case strings.HasPrefix(l, "# analyzer: "):
			analyzer = strings.TrimPrefix(l, "# analyzer: ")
		case strings.HasPrefix(l, "# sync: "):
			sync = strings.TrimPrefix(l, "# sync: ")
		case !strings.HasPrefix(l, "#") && strings.TrimSpace(l) != "":
			return
		}
	}
	return
}

func writeLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

// ============================================================================
// Interactive menu
// ============================================================================

func addLens(cfg Config, configPath string, lens LensConfig, out io.Writer) Config {
	if lens.Out == "" {
		lens.Out = filepath.Join(cfg.ProjectionsDir, lens.Name+".projection")
	}
	cfg.Lenses = append(cfg.Lenses, lens)
	if err := SaveConfig(configPath, cfg); err != nil {
		fmt.Fprintln(out, "error saving config:", err)
		return cfg
	}
	if err := runAndReport(cfg, []LensConfig{lens}, out); err != nil {
		fmt.Fprintln(out, "error:", err)
	}
	return cfg
}

func runAndReport(cfg Config, lenses []LensConfig, out io.Writer) error {
	sub := cfg
	sub.Lenses = lenses
	results, err := Run(sub, DefaultRegistry())
	if err != nil {
		return err
	}
	for _, p := range results {
		fmt.Fprintf(out, "wrote %s (%d blocks", LensOut(cfg, p.Lens), len(p.Blocks))
		if len(p.Extra) > 0 {
			fmt.Fprintf(out, ", %d branch files", len(p.Extra))
		}
		fmt.Fprintln(out, ")")
	}
	return nil
}

// SaveConfig writes the config back as indented JSON (used when the menu adds a lens).
func SaveConfig(path string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0644)
}

// ============================================================================
// First-run setup wizard
// ============================================================================

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

// Language-appropriate defaults for the wizard's suggested lenses. These now live
// on the Language registry (language.go) so a new language ships its own; these
// helpers are thin lookups kept for call-site brevity.
func entrypointPatternsFor(lang string) string {
	if l := languageByID(lang); l != nil {
		return l.EntrypointPatterns
	}
	return languageByID("js").EntrypointPatterns
}

func exitSinksFor(lang string) string {
	if l := languageByID(lang); l != nil {
		return l.ExitSinks
	}
	return languageByID("js").ExitSinks
}

func entryRegexFor(lang string) string {
	if l := languageByID(lang); l != nil {
		return l.EntryRegex
	}
	return languageByID("js").EntryRegex
}

func exitRegexFor(lang string) string {
	if l := languageByID(lang); l != nil {
		return l.ExitRegex
	}
	return languageByID("js").ExitRegex
}

// ============================================================================
// Watch mode
// ============================================================================

// hasTwoWayBlock reports whether a projection file contains a two-way (bookmark) block.
func hasTwoWayBlock(projPath string) bool {
	blocks, err := parseProjectionFile(projPath)
	if err != nil {
		return false
	}
	for _, b := range blocks {
		if b.Sync == "two-way" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// `ui` command — a small local web UI (single embedded page, stdlib net/http)
// to edit config.json, preview any lens ad-hoc against the real analyzers, and
// search source symbols so the file/method/line/type params a lens needs can be
// picked from real code instead of guessed. Same registry the CLI uses — the UI
// is a thin shell over ExecuteLens, never a parallel implementation.
// ---------------------------------------------------------------------------

// clampInt returns n constrained to the inclusive range [lo, hi].
func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// regexpCompile is a thin wrapper used by tests/markers to compile a pattern.
func regexpCompile(p string) (*regexp.Regexp, error) { return regexp.Compile(p) }

// regexpFindAll returns the first submatch of every match of pat in s (test helper).
func regexpFindAll(s, pat string) []string {
	re := regexp.MustCompile(pat)
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		if len(m) > 1 {
			out = append(out, m[1])
		}
	}
	return out
}
