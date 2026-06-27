package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Gradle/group detection (CROSS-REPO.md §B). Parses build.gradle(.kts) and
// settings.gradle to learn a repo's group and its declared dependencies, then
// classifies dependencies as internal (an internal library — same group prefix as
// another registered repo, or a project(:...) reference) vs external. This is what
// lets the trace + UI say "this edge crosses into an internal library" and groups
// the repos of one logical service together. Pure regex, dependency-free, like the
// rest of the non-joern backend. Gradle (not Maven) first; the shape generalizes.

// GradleDep is one declared dependency.
type GradleDep struct {
	Group    string `json:"group"`    // "com.acme.billing" (or "" for project deps)
	Artifact string `json:"artifact"` // "billing-lib"
	Version  string `json:"version,omitempty"`
	Project  string `json:"project,omitempty"` // ":billing-lib" for project(...) deps
	Raw      string `json:"raw"`
}

// GradleInfo is what we learn from a repo's build files.
type GradleInfo struct {
	Group      string      `json:"group"`       // declared group, or "" if none
	GroupGuess bool        `json:"group_guess"` // true if group was inferred, not declared
	Deps       []GradleDep `json:"deps"`        // declared dependencies
	HasGradle  bool        `json:"has_gradle"`  // a build.gradle(.kts) was found
}

var (
	// group = "com.acme.shop"  /  group 'com.acme'  /  group("com.acme")
	gradleGroupRE = regexp.MustCompile(`(?m)^\s*group\s*[=(]?\s*["']([A-Za-z0-9_.\-]+)["']`)
	// implementation "com.acme.billing:billing-lib:1.0.0"  (any config keyword)
	gradleDepRE = regexp.MustCompile(`(?m)^\s*(?:implementation|api|compileOnly|runtimeOnly|testImplementation|testRuntimeOnly|annotationProcessor)\s*[(\s]\s*["']([^"':]+):([^"':]+)(?::([^"']+))?["']`)
	// project deps: implementation project(":billing-lib")
	gradleProjectRE = regexp.MustCompile(`(?m)project\s*\(\s*["'](:[^"']+)["']`)
)

// detectGradle reads a repo's build files and returns what it can learn. Missing
// files yield a zero-value (HasGradle=false) — callers report that honestly.
func detectGradle(repoPath string) GradleInfo {
	var info GradleInfo
	var content strings.Builder
	for _, name := range []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"} {
		b, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		info.HasGradle = true
		content.Write(b)
		content.WriteString("\n")
	}
	text := content.String()
	if text == "" {
		// No gradle build file: guess identity from git remote, else dir name.
		info.Group = guessGroup(repoPath)
		info.GroupGuess = info.Group != ""
		return info
	}

	if m := gradleGroupRE.FindStringSubmatch(text); m != nil {
		info.Group = m[1]
	} else {
		info.Group = guessGroup(repoPath)
		info.GroupGuess = info.Group != ""
	}

	seen := map[string]bool{}
	for _, m := range gradleDepRE.FindAllStringSubmatch(text, -1) {
		d := GradleDep{Group: m[1], Artifact: m[2], Version: m[3], Raw: m[0]}
		key := d.Group + ":" + d.Artifact
		if seen[key] {
			continue
		}
		seen[key] = true
		info.Deps = append(info.Deps, d)
	}
	for _, m := range gradleProjectRE.FindAllStringSubmatch(text, -1) {
		key := "project" + m[1]
		if seen[key] {
			continue
		}
		seen[key] = true
		info.Deps = append(info.Deps, GradleDep{Project: m[1], Raw: m[0]})
	}
	return info
}

// guessGroup derives a fallback identity for a repo with no declared group: the git
// remote org (github.com/ORG/repo -> "ORG"), else "".
func guessGroup(repoPath string) string {
	url := gitRemoteOrigin(repoPath)
	if url == "" {
		return ""
	}
	// strip protocol/host, keep the first path segment as the org.
	url = strings.TrimSuffix(url, ".git")
	if i := strings.LastIndexAny(url, ":/"); i >= 0 {
		// e.g. .../ORG/repo -> find ORG by splitting the remainder
	}
	parts := strings.FieldsFunc(url, func(r rune) bool { return r == '/' || r == ':' })
	for i := len(parts) - 2; i >= 0; i-- {
		if p := parts[i]; p != "" && !strings.Contains(p, ".") {
			return p
		}
	}
	return ""
}

// internalDepsAmong classifies a repo's deps against the other registered repos:
// a dep is INTERNAL when it's a project(:...) ref, or its group shares a meaningful
// prefix with another repo's group (e.g. com.acme.shop and com.acme.billing share
// "com.acme"). Returns the names of repos this one internally depends on.
func internalDepsAmong(self GradleInfo, repos []WorkspaceRepo, selfName string) []string {
	hit := map[string]bool{}
	for _, d := range self.Deps {
		if d.Project != "" {
			// project(":billing-lib") -> match a repo whose name is the segment.
			seg := strings.TrimPrefix(d.Project, ":")
			for _, r := range repos {
				if r.Name == seg {
					hit[r.Name] = true
				}
			}
			continue
		}
		for _, r := range repos {
			if r.Name == selfName || r.Group == "" {
				continue
			}
			if sharedGroupPrefix(d.Group, r.Group) {
				hit[r.Name] = true
			}
		}
	}
	var out []string
	for n := range hit {
		out = append(out, n)
	}
	return out
}

// sharedGroupPrefix reports whether two gradle groups share a meaningful (>=2
// segment) prefix, e.g. com.acme.shop & com.acme.billing -> true (share com.acme),
// but com.acme & com.google -> false.
func sharedGroupPrefix(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n >= 2
}
