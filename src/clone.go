package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Cloning a GitHub (or any git) repo into a local working directory so a lens can
// run against it. Shared by the `clone` CLI command and the UI's /api/clone, so
// every way to drive the tool gets the same behavior.

var cloneSlugRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// normalizeGitURL accepts a full git URL or a bare "owner/repo" GitHub slug and
// returns a clonable URL plus a stable directory name ("owner-repo").
func normalizeGitURL(in string) (url, name string, err error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", "", fmt.Errorf("empty repo reference")
	}
	// bare GitHub slug: owner/repo
	if cloneSlugRE.MatchString(in) {
		slug := strings.TrimSuffix(in, ".git")
		return "https://github.com/" + slug + ".git", strings.ReplaceAll(slug, "/", "-"), nil
	}
	if !isGitURL(in) {
		return "", "", fmt.Errorf("not a git URL or owner/repo slug: %q", in)
	}
	// derive a name from the last path segment
	base := in
	if i := strings.LastIndexAny(base, "/:"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".git")
	if base == "" {
		base = "repo"
	}
	return in, base, nil
}

// cloneRepo shallow-clones url into <dest>/<name> (skipping the clone if it
// already exists) and returns the absolute path. dest is created if missing.
func cloneRepo(url, name, dest string, out *os.File) (string, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	target := filepath.Join(dest, name)
	if st, err := os.Stat(filepath.Join(target, ".git")); err == nil && st.IsDir() {
		return target, nil // already cloned; reuse it (idempotent)
	}
	c := exec.Command("git", "clone", "--depth", "1", url, target)
	if out != nil {
		c.Stdout, c.Stderr = out, out
	}
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git clone failed: %w", err)
	}
	return target, nil
}
