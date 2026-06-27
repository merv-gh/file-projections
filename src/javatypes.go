package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Java type hierarchy + override resolution across repos (CROSS-REPO.md §C). This is
// the primitive that makes dependency-inversion paths resolvable: the library calls
// AbstractPaymentService.pay(); the app declares RealPaymentService extends
// AbstractPaymentService and overrides pay(). To draw a seamless path we must know,
// across both repos, that Real is-a Abstract and provides pay.
//
// Pure-Go, dependency-free, like the rest of the non-joern backend. It is
// scope-resolved by type-name + method-name + arity, NOT a full type checker:
// generics, overload selection by argument type, and runtime proxies are out of
// scope and reported as ambiguity. joern remains the precise upgrade.

// JavaType is a declared class/interface/enum/record across the workspace.
type JavaType struct {
	Name       string       // simple name, e.g. "RealPaymentService"
	Package    string       // "com.acme.shop"
	Kind       string       // class | interface | enum | record
	Abstract   bool         // abstract class / interface (no own body for some methods)
	Extends    string       // simple name of superclass, or ""
	Implements []string     // simple names of implemented interfaces
	Repo       string       // owning workspace repo name
	File       string       // repo-relative slash path
	Line       int          // 1-based decl line
	Methods    []JavaMethod // declared methods (reused parser)
	Fields     []JavaField  // declared fields (for DI field-type lookup)
}

// JavaField is a field declaration: `private AbstractPaymentService paymentService;`.
type JavaField struct {
	Name string
	Type string // simple type name
	Line int
}

// TypeIndex is the workspace-wide hierarchy: every type by simple name, plus reverse
// subtype edges so we can resolve "who concretely implements/extends T".
type TypeIndex struct {
	byName map[string][]*JavaType // simple name -> declarations (>1 = ambiguous)
	subs   map[string][]*JavaType // super simple name -> direct subtypes
	all    []*JavaType
}

var (
	// class/interface/enum/record decl with optional extends/implements clause.
	javaTypeDeclRE = regexp.MustCompile(`\b(?:(abstract)\s+)?(class|interface|enum|record)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	javaExtendsRE  = regexp.MustCompile(`\bextends\s+([A-Za-z_][A-Za-z0-9_.]*(?:\s*<[^>]*>)?)`)
	javaImplRE     = regexp.MustCompile(`\bimplements\s+([^{]+)`)
	javaPackageRE  = regexp.MustCompile(`^\s*package\s+([A-Za-z0-9_.]+)\s*;`)
	// a field: optional modifiers, a TypeName (capitalized), a name, then ; or =.
	javaFieldRE = regexp.MustCompile(`^\s*(?:private|protected|public|final|static|transient|volatile|\s)*([A-Z][A-Za-z0-9_]*)(?:\s*<[^>]*>)?\s+([a-z_][A-Za-z0-9_]*)\s*[;=]`)
)

// buildTypeIndex scans every registered repo for Java type declarations and assembles
// the cross-repo hierarchy. Each repo is walked with the shared source walker so the
// file set matches the symbol index / call graph.
func buildTypeIndex(base Config, ws *Workspace) *TypeIndex {
	idx := &TypeIndex{byName: map[string][]*JavaType{}, subs: map[string][]*JavaType{}}
	for _, repo := range ws.Repos {
		cfg := base
		cfg.Root = repo.Path
		_ = walkSourceFiles(cfg, repo.Path, func(rel string, lines []string) {
			if languageForPath(rel) != "java" {
				return
			}
			for _, t := range parseJavaTypes(rel, lines) {
				t.Repo = repo.Name
				idx.all = append(idx.all, t)
				idx.byName[t.Name] = append(idx.byName[t.Name], t)
			}
		})
	}
	// Build reverse subtype edges (super simple name -> subtype).
	for _, t := range idx.all {
		if t.Extends != "" {
			idx.subs[t.Extends] = append(idx.subs[t.Extends], t)
		}
		for _, i := range t.Implements {
			idx.subs[i] = append(idx.subs[i], t)
		}
	}
	return idx
}

// parseJavaTypes extracts the type declarations (with extends/implements/fields/
// methods) from one Java file's lines.
func parseJavaTypes(rel string, lines []string) []*JavaType {
	// Strip block/javadoc comments first (preserving line numbers): real Java is full
	// of javadoc, and a comment like "calls Foo.bar()" otherwise gets mis-parsed as a
	// method/field. Line comments are handled per-line by the downstream parsers.
	lines = stripJavaBlockComments(lines)
	pkg := ""
	for _, l := range lines {
		if m := javaPackageRE.FindStringSubmatch(l); m != nil {
			pkg = m[1]
			break
		}
	}
	methods, _ := parseJavaMethods(lines)
	var out []*JavaType
	for i, l := range lines {
		m := javaTypeDeclRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		// Skip matches inside comments / strings cheaply.
		if strings.HasPrefix(strings.TrimSpace(l), "//") || strings.HasPrefix(strings.TrimSpace(l), "*") {
			continue
		}
		t := &JavaType{
			Name:     m[3],
			Package:  pkg,
			Kind:     m[2],
			Abstract: m[1] == "abstract" || m[2] == "interface",
			File:     rel,
			Line:     i + 1,
		}
		// extends/implements may be on the decl line or the next couple (wrapped).
		header := l
		for j := i + 1; j < len(lines) && j < i+4 && !strings.Contains(header, "{"); j++ {
			header += " " + lines[j]
		}
		if em := javaExtendsRE.FindStringSubmatch(header); em != nil {
			t.Extends = simpleTypeName(em[1])
		}
		if im := javaImplRE.FindStringSubmatch(header); im != nil {
			for _, part := range strings.Split(im[1], ",") {
				if n := simpleTypeName(part); n != "" {
					t.Implements = append(t.Implements, n)
				}
			}
		}
		// Attach the methods/fields that fall within this type's brace span. For the
		// common one-top-type-per-file case this is just "all of them".
		end := len(lines)
		if open := indexFrom(lines, i, "{"); open >= 0 {
			if c, err := findClosingBrace(lines, open); err == nil {
				end = c + 1
			}
		}
		for _, mm := range methods {
			if mm.Start > t.Line && mm.Start <= end {
				t.Methods = append(t.Methods, mm)
			}
		}
		for fi := i + 1; fi < end && fi < len(lines); fi++ {
			if fm := javaFieldRE.FindStringSubmatch(lines[fi]); fm != nil {
				t.Fields = append(t.Fields, JavaField{Type: fm[1], Name: fm[2], Line: fi + 1})
			}
		}
		out = append(out, t)
	}
	return out
}

// stripJavaBlockComments blanks out /* ... */ (and javadoc) comment content while
// preserving line count, so a method/type name mentioned in a comment is never
// mistaken for a declaration. Line comments are handled per-line downstream.
func stripJavaBlockComments(lines []string) []string {
	out := make([]string, len(lines))
	inBlock := false
	for i, l := range lines {
		var b strings.Builder
		j := 0
		for j < len(l) {
			if inBlock {
				if j+1 < len(l) && l[j] == '*' && l[j+1] == '/' {
					inBlock = false
					b.WriteString("  ")
					j += 2
					continue
				}
				b.WriteByte(' ')
				j++
				continue
			}
			if j+1 < len(l) && l[j] == '/' && l[j+1] == '*' {
				inBlock = true
				b.WriteString("  ")
				j += 2
				continue
			}
			b.WriteByte(l[j])
			j++
		}
		out[i] = b.String()
	}
	return out
}

// simpleTypeName strips package qualifier and generic args: "com.acme.Foo<Bar>" -> "Foo".
func simpleTypeName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "<"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// indexFrom returns the first line index >= from that contains sub, or -1.
func indexFrom(lines []string, from int, sub string) int {
	for i := from; i < len(lines); i++ {
		if strings.Contains(lines[i], sub) {
			return i
		}
	}
	return -1
}

// declares reports whether type t declares a method with the given name.
func (t *JavaType) declares(method string) bool {
	for _, m := range t.Methods {
		if m.Name == method {
			return true
		}
	}
	return false
}

// fieldType returns the declared type of a field by name, or "".
func (t *JavaType) fieldType(field string) string {
	for _, f := range t.Fields {
		if f.Name == field {
			return f.Type
		}
	}
	return ""
}

// ConcreteOverrides resolves dependency inversion: given a (possibly abstract) type
// name and a method, return every concrete type in the workspace that is a transitive
// subtype AND declares that method — i.e. the runtime dispatch targets. If the type
// itself is concrete and declares the method, it's included. Cycles terminate.
func (idx *TypeIndex) ConcreteOverrides(typeName, method string) []*JavaType {
	seen := map[*JavaType]bool{}
	var out []*JavaType
	var visit func(name string)
	visited := map[string]bool{}
	consider := func(t *JavaType) {
		if !t.Abstract && t.declares(method) && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		for _, t := range idx.byName[name] {
			consider(t)
		}
		for _, sub := range idx.subs[name] {
			consider(sub)
			visit(sub.Name)
		}
	}
	visit(typeName)
	return out
}

// findType returns the (first) declaration of a simple type name, or nil.
func (idx *TypeIndex) findType(name string) *JavaType {
	if ts := idx.byName[name]; len(ts) > 0 {
		return ts[0]
	}
	return nil
}

// methodLoc returns the file/line of a method declared on a type, for path rendering.
func (t *JavaType) methodLoc(method string) (string, int) {
	for _, m := range t.Methods {
		if m.Name == method {
			return t.File, m.Start
		}
	}
	return t.File, t.Line
}

// repoRelPath joins a repo's own relative file path for display ("repo/path").
func repoRelPath(repo, rel string) string {
	return filepath.ToSlash(filepath.Join(repo, rel))
}
