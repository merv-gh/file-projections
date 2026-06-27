package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// User-level workspace — the multi-repo layer (CROSS-REPO.md §A). The rest of the
// tool is single-root (one cfg.Root + per-lens SourceRoot); a Workspace is a set of
// repo checkouts registered at the user level (~/.file-projections) so a logical
// service spread across an app repo and several internal libraries can be loaded and
// queried together. The per-repo symbol index and call graph caches (keyed by
// absolute root) are reused unchanged; the cross-repo layer queries across them.

// WorkspaceRepo is one registered repo: a clone or a link to an existing folder.
type WorkspaceRepo struct {
	Name  string `json:"name"`            // stable id, e.g. "shop-app"
	Path  string `json:"path"`            // absolute path to the repo root
	Kind  string `json:"kind"`            // "clone" | "link"
	Group string `json:"group,omitempty"` // gradle group, detected (gradle.go)
	// Origin records where a clone came from (the git URL) for provenance.
	Origin string `json:"origin,omitempty"`
}

// Workspace is the registered set of repos plus where it lives on disk.
type Workspace struct {
	Home  string          `json:"-"` // ~/.file-projections (or FILE_PROJECTIONS_HOME)
	Repos []WorkspaceRepo `json:"repos"`
}

// workspaceHome returns the user-level workspace dir, honoring FILE_PROJECTIONS_HOME,
// else ~/.file-projections. The directory is created on demand.
func workspaceHome() (string, error) {
	if h := strings.TrimSpace(os.Getenv("FILE_PROJECTIONS_HOME")); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".file-projections"), nil
}

// LoadWorkspace reads ~/.file-projections/workspace.json, returning an empty (but
// valid) workspace if none exists yet.
func LoadWorkspace() (*Workspace, error) {
	home, err := workspaceHome()
	if err != nil {
		return nil, err
	}
	ws := &Workspace{Home: home}
	raw, err := os.ReadFile(filepath.Join(home, "workspace.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return ws, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, ws); err != nil {
		return nil, fmt.Errorf("workspace.json: %w", err)
	}
	ws.Home = home
	return ws, nil
}

// save persists the workspace to disk (creating the home dir if needed).
func (ws *Workspace) save() error {
	if err := os.MkdirAll(ws.Home, 0o755); err != nil {
		return err
	}
	sort.Slice(ws.Repos, func(i, j int) bool { return ws.Repos[i].Name < ws.Repos[j].Name })
	raw, err := json.MarshalIndent(ws, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ws.Home, "workspace.json"), raw, 0o644)
}

// find returns the repo with the given name, or nil.
func (ws *Workspace) find(name string) *WorkspaceRepo {
	for i := range ws.Repos {
		if ws.Repos[i].Name == name {
			return &ws.Repos[i]
		}
	}
	return nil
}

// AddLink registers an existing local folder as a repo (no copy). The path must be
// an existing directory; the name defaults to its base name.
func (ws *Workspace) AddLink(path, name string) (*WorkspaceRepo, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", abs)
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	repo := WorkspaceRepo{Name: name, Path: abs, Kind: "link"}
	ws.upsert(repo)
	if err := ws.detectGroup(repo.Name); err != nil {
		return nil, err
	}
	return ws.find(repo.Name), ws.save()
}

// AddClone clones a git repo (url or owner/repo slug) into the workspace's repos/
// dir and registers it. Idempotent (reuses an existing clone).
func (ws *Workspace) AddClone(ref string, out *os.File) (*WorkspaceRepo, error) {
	url, name, err := normalizeGitURL(ref)
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(ws.Home, "repos")
	target, err := cloneRepo(url, name, dest, out)
	if err != nil {
		return nil, err
	}
	repo := WorkspaceRepo{Name: name, Path: target, Kind: "clone", Origin: url}
	ws.upsert(repo)
	if err := ws.detectGroup(repo.Name); err != nil {
		return nil, err
	}
	return ws.find(repo.Name), ws.save()
}

// Remove deregisters a repo. For clones it also deletes the checkout; links are only
// forgotten (never deleting user folders).
func (ws *Workspace) Remove(name string) error {
	repo := ws.find(name)
	if repo == nil {
		return fmt.Errorf("no repo named %q", name)
	}
	if repo.Kind == "clone" {
		// Only remove inside our own repos/ dir, never an arbitrary path.
		reposDir := filepath.Join(ws.Home, "repos")
		if strings.HasPrefix(repo.Path, reposDir+string(os.PathSeparator)) {
			_ = os.RemoveAll(repo.Path)
		}
	}
	var kept []WorkspaceRepo
	for _, r := range ws.Repos {
		if r.Name != name {
			kept = append(kept, r)
		}
	}
	ws.Repos = kept
	return ws.save()
}

func (ws *Workspace) upsert(repo WorkspaceRepo) {
	if existing := ws.find(repo.Name); existing != nil {
		group := existing.Group
		*existing = repo
		if existing.Group == "" {
			existing.Group = group
		}
		return
	}
	ws.Repos = append(ws.Repos, repo)
}

// detectGroup fills in a repo's gradle group (gradle.go). Best-effort: a repo with no
// build file just gets an empty group (reported honestly in the UI).
func (ws *Workspace) detectGroup(name string) error {
	repo := ws.find(name)
	if repo == nil {
		return fmt.Errorf("no repo named %q", name)
	}
	info := detectGradle(repo.Path)
	repo.Group = info.Group
	return nil
}

// configForRepo builds a single-root Config rooted at a registered repo, so existing
// single-root analyzers (symbol index, call graph, unroller) run against it unchanged.
func (ws *Workspace) configForRepo(base Config, name string) (Config, bool) {
	repo := ws.find(name)
	if repo == nil {
		return Config{}, false
	}
	cfg := base
	cfg.Root = repo.Path
	return cfg, true
}

// gitRemoteOrigin returns the origin remote URL of a checkout, or "" — used as a
// group/identity fallback when no gradle group is declared.
func gitRemoteOrigin(repoPath string) string {
	c := exec.Command("git", "-C", repoPath, "config", "--get", "remote.origin.url")
	out, err := c.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// RunWorkspace is the `workspace` CLI command: list, add (link/clone), or remove
// repos in the user-level workspace — parity with the UI's /api/workspace.
//
//	file-projections workspace list
//	file-projections workspace link <path> [name]
//	file-projections workspace clone <url|owner/repo>
//	file-projections workspace rm <name>
func RunWorkspace(args []string, out io.Writer) error {
	ws, err := LoadWorkspace()
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list", "ls":
		if len(ws.Repos) == 0 {
			fmt.Fprintf(out, "workspace %s: no repos (add with `workspace link <path>` or `workspace clone <url>`)\n", ws.Home)
			return nil
		}
		// resolve groups + internal deps for display
		infos := make([]GradleInfo, len(ws.Repos))
		for i, r := range ws.Repos {
			infos[i] = detectGradle(r.Path)
			if ws.Repos[i].Group == "" {
				ws.Repos[i].Group = infos[i].Group
			}
		}
		fmt.Fprintf(out, "workspace %s:\n", ws.Home)
		for i, r := range ws.Repos {
			internal := internalDepsAmong(infos[i], ws.Repos, r.Name)
			dep := ""
			if len(internal) > 0 {
				dep = "  ↳ internal: " + strings.Join(internal, ", ")
			}
			fmt.Fprintf(out, "  %-16s %-6s group=%-18s %s%s\n", r.Name, r.Kind, coalesce(r.Group, "—"), r.Path, dep)
		}
		return nil
	case "link":
		if len(args) < 2 {
			return fmt.Errorf("usage: workspace link <path> [name]")
		}
		name := ""
		if len(args) >= 3 {
			name = args[2]
		}
		repo, err := ws.AddLink(args[1], name)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "linked %s (%s) group=%s\n", repo.Name, repo.Path, coalesce(repo.Group, "—"))
		return nil
	case "clone":
		if len(args) < 2 {
			return fmt.Errorf("usage: workspace clone <url|owner/repo>")
		}
		f, _ := out.(*os.File)
		repo, err := ws.AddClone(args[1], f)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "cloned %s (%s) group=%s\n", repo.Name, repo.Path, coalesce(repo.Group, "—"))
		return nil
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: workspace rm <name>")
		}
		if err := ws.Remove(args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown workspace command %q (list|link|clone|rm)", sub)
	}
}
