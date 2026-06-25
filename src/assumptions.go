package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Cross-language unroll/assumption helpers (guards, inlining, line views).

type unrollLine struct {
	code string
	file string
	line int
	// guards is the conjunction of branch conditions that must hold to reach this
	// line — the line's "assumptions" (e.g. ["score >= 0", "!(score >= 90)"]).
	guards []string
}

type inlineCallChoice struct {
	ID       string `json:"id"`       // file:line of the call site
	Name     string `json:"name"`     // called method/function name
	Origin   string `json:"origin"`   // source_root-relative file:line
	Expanded bool   `json:"expanded"` // currently expanded inline
	Depth    int    `json:"depth"`    // caller nesting depth, entry body is 0
}

// branchChoice describes one undecidable conditional the UI can toggle. Sides are
// the selectable options ("then"+"else", or "then"+"skip" when there is no else).
type branchChoice struct {
	ID     string   `json:"id"`     // file:line of the if header
	Cond   string   `json:"cond"`   // the condition text
	Origin string   `json:"origin"` // source_root-relative file:line
	Side   string   `json:"side"`   // currently shown side
	Sides  []string `json:"sides"`  // available sides
}

// withGuard returns a fresh copy of the guard stack with one more condition pushed,
// so sibling branches never alias each other's assumptions.
func withGuard(guards []string, cond string) []string {
	g := make([]string, 0, len(guards)+1)
	g = append(g, guards...)
	return append(g, cond)
}

var uiExitStmtRE = regexp.MustCompile(`^\s*(return|throw|break|continue)\b`)

// rangeExits reports whether the last meaningful statement in lines[lo..hi] is an
// early exit (return/throw/break/continue) — i.e. a guard clause whose fall-through
// implies the negation of its condition for everything after it.
func rangeExits(lines []string, lo, hi int) bool {
	for i := hi; i >= lo && i < len(lines); i-- {
		t := strings.TrimSpace(stripLineComment(lines[i]))
		if t == "" || t == "{" || t == "}" || strings.HasPrefix(t, "//") {
			continue
		}
		return uiExitStmtRE.MatchString(t)
	}
	return false
}

// When a call is inlined into an assignment (`x = helper(...)`), the helper's
// `return E;` should read as `x = E;` in the flattened program — a bare `return`
// looks like the outer method exits early and misleads readers (and models).
var inlineLHSRE = regexp.MustCompile(`^(?:final\s+)?(?:[A-Za-z_][\w<>\[\].]*\s+)?([A-Za-z_]\w*)\s*=[^=]`)

var inlineRetRE = regexp.MustCompile(`^(\s*)return\s+(.*?);\s*$`)

func inlineAssignTarget(trim string) string {
	if m := inlineLHSRE.FindStringSubmatch(trim); m != nil {
		return m[1]
	}
	return ""
}

func rewriteInlinedReturns(lines []unrollLine, lhs string) {
	if lhs == "" {
		return
	}
	for i := range lines {
		if m := inlineRetRE.FindStringSubmatch(lines[i].code); m != nil {
			lines[i].code = m[1] + lhs + " = " + m[2] + ";"
		}
	}
}

// unrollLineView is one line of the straight-line program plus the real source location it
// came from, so the UI can show "discover branches -> choose inputs -> edit", and each edit
// goes back to its true origin via the same two-way sync the CLI uses.
type unrollLineView struct {
	N      int      `json:"n"`
	Code   string   `json:"code"`
	Origin string   `json:"origin"`           // file:line, the real source the line came from
	Branch bool     `json:"branch"`           // an unresolved `if (...)` / `switch` header
	Guards []string `json:"guards,omitempty"` // conditions that must hold to reach this line
}

// unrollChoices pulls the per-conditional toggle metadata the analyzer recorded.
func unrollChoices(p Projection) []branchChoice {
	var out []branchChoice
	for _, f := range p.Facts {
		if f.Tool == "unrolled-program" && strings.HasPrefix(f.ID, "choice-") {
			var c branchChoice
			if json.Unmarshal([]byte(f.Text), &c) == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

// unrollCalls pulls the per-call inline/collapse metadata the analyzer recorded.
func unrollCalls(p Projection) []inlineCallChoice {
	var out []inlineCallChoice
	for _, f := range p.Facts {
		if f.Tool == "unrolled-program" && strings.HasPrefix(f.ID, "call-") {
			var c inlineCallChoice
			if json.Unmarshal([]byte(f.Text), &c) == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

var uiBranchRE = regexp.MustCompile(`^\s*(if|else if|switch|while|for|case)\b`)

func unrollViewLines(p Projection) []unrollLineView {
	var out []unrollLineView
	for _, b := range p.Blocks {
		for i, code := range b.Lines {
			lv := unrollLineView{N: len(out) + 1, Code: strings.TrimRight(code, " \t"), Branch: uiBranchRE.MatchString(code)}
			if i < len(b.LineOrigins) {
				o := b.LineOrigins[i]
				if o.SrcFile != "" {
					lv.Origin = fmt.Sprintf("%s:%d", o.SrcFile, o.Line)
				}
			}
			if i < len(b.LineGuards) {
				lv.Guards = b.LineGuards[i]
			}
			out = append(out, lv)
		}
	}
	return out
}

func unrollDecisionFacts(p Projection) []string {
	var out []string
	for _, f := range p.Facts {
		if f.Tool == "unrolled-program" && strings.HasPrefix(f.ID, "branch-") {
			out = append(out, f.Text)
		}
	}
	sort.Strings(out)
	return out
}
