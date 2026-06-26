package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// git-blame lens — "what happened to this code": annotate a file (or a line span)
// with the commit, author and date that last touched each line. Answers the Observe
// intent's "who last changed this / history of file:line" without leaving the tool.
// Exact confidence (it's git, not a heuristic). Requires a git checkout + the git
// binary; degrades to a clear error otherwise.

type blameLine struct {
	commit string
	author string
	date   string // YYYY-MM-DD
	line   int
	code   string
}

func AnalyzeGitBlame(cfg Config, lens LensConfig) (Projection, error) {
	file := strings.TrimSpace(lens.Params["file"])
	if file == "" {
		return Projection{}, errors.New("git-blame: params.file is required")
	}
	// Run git from the source-root directory and blame the file relative to it, so
	// this works whether the git repo is cfg.Root or a nested clone (the source root
	// is inside whichever repo owns the file).
	workDir := filepath.Join(cfg.Root, lens.SourceRoot)
	args := []string{"blame", "--porcelain"}
	if r := strings.TrimSpace(lens.Params["lines"]); r != "" {
		a, b := parseLineRange(r)
		if a > 0 && b >= a {
			args = append(args, "-L", fmt.Sprintf("%d,%d", a, b))
		}
	}
	args = append(args, "--", filepath.FromSlash(file))
	if _, err := exec.LookPath("git"); err != nil {
		return Projection{}, errors.New("git-blame: git not found in PATH")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Projection{}, fmt.Errorf("git-blame failed (is %s in a git repo with %s committed?): %w\n%s", workDir, file, err, truncate(string(out), 200))
	}
	blames := parsePorcelainBlame(string(out))

	var lines []string
	authors := map[string]int{}
	for _, b := range blames {
		short := b.commit
		if len(short) > 8 {
			short = short[:8]
		}
		meta := fmt.Sprintf("%s %s %s", short, b.date, b.author)
		lines = append(lines, codeLoc(b.code, meta, b.line))
		authors[b.author]++
	}
	if len(lines) == 0 {
		lines = append(lines, "// no blame output for "+file)
	}
	p := Projection{Sync: "view-only"}
	// Note the locator column here is "<commit> <date> <author>", not file:line — the
	// origin file is fixed (it's this one), and the per-line provenance is what matters.
	p.Blocks = append(p.Blocks, ProjectionBlock{ID: "git-blame", File: file, Mode: "git-blame", Tool: "git-blame", Lines: lines})
	for a, n := range authors {
		p.Facts = append(p.Facts, ProjectionFact{ID: "author-" + a, Tool: "git-blame", Text: fmt.Sprintf("%s: %d lines", a, n)})
	}
	p.Facts = append(p.Facts, confidenceFact("exact", "git blame --porcelain"))
	return p, nil
}

// parsePorcelainBlame parses `git blame --porcelain` into per-line provenance. The
// porcelain format emits a commit header line (`<sha> <orig> <final> [n]`) followed
// by metadata (`author …`, `author-time …`) and the content line prefixed by a tab.
func parsePorcelainBlame(out string) []blameLine {
	var res []blameLine
	commits := map[string]struct{ author, date string }{}
	var curSHA, curAuthor, curDate string
	var curFinalLine int
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case ln == "":
			continue
		case ln[0] == '\t':
			// content line for the current commit/line.
			info := commits[curSHA]
			if curAuthor != "" {
				info.author = curAuthor
			}
			if curDate != "" {
				info.date = curDate
			}
			commits[curSHA] = info
			res = append(res, blameLine{commit: curSHA, author: info.author, date: info.date, line: curFinalLine, code: ln[1:]})
			curAuthor, curDate = "", ""
		case strings.HasPrefix(ln, "author "):
			curAuthor = strings.TrimPrefix(ln, "author ")
		case strings.HasPrefix(ln, "author-time "):
			if ts, err := strconv.ParseInt(strings.TrimPrefix(ln, "author-time "), 10, 64); err == nil {
				curDate = unixDate(ts)
			}
		default:
			// possible commit header: "<40-hex> <origLine> <finalLine> [group]"
			fields := strings.Fields(ln)
			if len(fields) >= 3 && len(fields[0]) >= 7 && isHex(fields[0]) {
				curSHA = fields[0]
				if n, err := strconv.Atoi(fields[2]); err == nil {
					curFinalLine = n
				}
				if c, ok := commits[curSHA]; ok {
					curAuthor, curDate = c.author, c.date
				}
			}
		}
	}
	return res
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return len(s) > 0
}
