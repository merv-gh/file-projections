package main

import (
	"embed" // for the //go:embed VERSION and ui/ asset vars below
	"regexp"
	"strings"
	"time"
)

// Package main: core data types, embedded assets, and small globals.

//go:embed VERSION
var versionRaw string

// version is the released semver, sourced from the VERSION file (bumped by `make release-*`).
var version = strings.TrimSpace(versionRaw)

// nowFunc is indirected so tests can pin timestamps deterministically.
var nowFunc = time.Now

type Config struct {
	Root           string                `json:"root"`
	ProjectionsDir string                `json:"projections_dir"`
	ExcludeDirs    []string              `json:"exclude_dirs"`
	Tools          map[string]ToolConfig `json:"tools,omitempty"`
	// Workspace is the cross-repo projects model — the single source of truth for
	// "what repos form a logical service" (CROSS-REPO-UX.md). Optional; absent for
	// single-repo configs.
	Workspace *WorkspaceConfig `json:"workspace,omitempty"`
	Lenses    []LensConfig     `json:"lenses"`
}

// WorkspaceConfig is the cross-repo model embedded in config.json: a set of named
// projects (each an app repo + its internal libraries) and which one is active.
type WorkspaceConfig struct {
	Projects []ProjectConfig `json:"projects"`
	Active   string          `json:"active,omitempty"`
}

// ProjectConfig is one logical service: an app repo plus the internal libraries it
// depends on. Repos are analyzed together for symbol search, trace and service graph.
type ProjectConfig struct {
	Name  string       `json:"name"`
	Repos []RepoConfig `json:"repos"`
}

// RepoConfig is one repo in a project. Path is relative to cfg.Root (or absolute).
// Role is "app" | "library" (drives the include-libraries scope and entrypoint bias).
type RepoConfig struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Role string `json:"role,omitempty"`
}

// ToolConfig describes how to invoke an external tool that may not be installed
// locally. When the binary is missing from PATH, runTool falls back to a Docker
// image. JVMArgs is forwarded to memory-hungry tools like Joern via _JAVA_OPTIONS.
type ToolConfig struct {
	Image   string `json:"image,omitempty"`
	JVMArgs string `json:"jvm_args,omitempty"`
	// Farm is a joern-farm base URL (e.g. http://farmhost:9090). When set for "joern",
	// CPG building AND queries are offloaded to the farm — the local machine runs no Joern.
	Farm string `json:"farm,omitempty"`
}

type LensConfig struct {
	Name       string            `json:"name"`
	Out        string            `json:"out,omitempty"`
	Analyzer   string            `json:"analyzer"`
	SourceRoot string            `json:"source_root,omitempty"`
	Include    []string          `json:"include,omitempty"`
	Input      string            `json:"input,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
}

type ExtraFile struct {
	Path string
	Proj Projection
}

type LineOrigin struct {
	SrcFile string
	Line    int
	SrcHash string
}

type Analyzer interface {
	Name() string
	Analyze(cfg Config, lens LensConfig) (Projection, error)
}

var (
	goTypeRE = regexp.MustCompile(`^\s*type\s+([A-Za-z_][A-Za-z0-9_]*)\s+(\S+)`)
	goFuncRE = regexp.MustCompile(`^\s*func\s*(\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	goPkgRE  = regexp.MustCompile(`^\s*package\s+([A-Za-z_][A-Za-z0-9_]*)`)
)

var (
	tsImportRE  = regexp.MustCompile(`(?m)(?:import|export)\s[^'"]*from\s*["']([^"']+)["']|import\s*["']([^"']+)["']|import\(\s*["']([^"']+)["']\s*\)`)
	goImportRE  = regexp.MustCompile(`["]([a-zA-Z0-9_./\-]+)["]`)
	goAddRoutRE = regexp.MustCompile(`AddRoute\(\s*["']([^"']+)["']\s*,\s*(?:deps\.)?([A-Za-z_][A-Za-z0-9_]*)`)
	goRecvFnRE  = regexp.MustCompile(`^\s*func\s*\([^)]*\)\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

var (
	classRE  = regexp.MustCompile(`\b(class|interface|record|enum)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	callRE   = regexp.MustCompile(`\b([a-z][A-Za-z0-9_]*)\s*\(`)
	ifRE     = regexp.MustCompile(`^\s*if\s*\((.*)\)\s*\{?\s*$`)
	retRE    = regexp.MustCompile(`^\s*return\b`)
	rejectRE = regexp.MustCompile(`\brejectValue\s*\(`)
)

// defaultFlowStopCalls are general Java/keyword call names ignored when looking for
// helper methods. Domain-specific names (getBirthDate, addPet, ...) are NOT built in —
// pass them via params.stop_calls so the program stays project-agnostic.
var defaultFlowStopCalls = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true,
	"equals": true, "new": true,
}

// Joern variable-flow analyzer: adapter contract for real Joern plus deterministic Java fallback.
type VarFlowTarget struct {
	Variable string
	File     string
	Line     int
	Method   string
	Mode     string
}

type VarFlowResult struct {
	Target       VarFlowTarget
	MethodName   string
	File         string
	MethodStart  int
	MethodEnd    int
	Lines        []string
	Contributors []string
	Facts        []string
	// Hits are the structured contributing lines (source line + reason), used by
	// the data-flow lens to render trailing padded comments instead of // prefixes.
	Hits []lineHit
}

var (
	javaAssignRE     = regexp.MustCompile(`^\s*(?:(?:final\s+)?[A-Za-z_][A-Za-z0-9_<>,.?[\]\s]*\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.+);\s*$`)
	javaMutatorRE    = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\.\s*(set[A-Za-z0-9_]*|add[A-Za-z0-9_]*|put[A-Za-z0-9_]*|remove[A-Za-z0-9_]*)\s*\((.*)\)`)
	javaIdentRE      = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\b`)
	javaParamStripRE = regexp.MustCompile(`@\w+(?:\([^)]*\))?\s*`)
)

type lineHit struct {
	Line int
	Text string
	Why  string
}

// JavaScript event-surface analyzer: adapter for composable event-driven structures.
type JSFile struct {
	Rel       string
	Lines     []string
	Exports   []JSSymbol
	Functions []JSSymbol
	Classes   []JSSymbol
	Events    []JSEvent
	Regs      []JSRegistration
}

type JSSymbol struct {
	Name string
	Kind string
	Line int
	Sig  string
}

type JSEvent struct {
	Kind string
	Name string
	Line int
	Code string
}

type JSRegistration struct {
	Kind string
	Name string
	Line int
	Code string
}

var (
	jsExportFuncRE   = regexp.MustCompile(`^\s*export\s+(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
	jsExportClassRE  = regexp.MustCompile(`^\s*export\s+class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	jsFunctionRE     = regexp.MustCompile(`^\s*(?:async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
	jsClassRE        = regexp.MustCompile(`^\s*class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	jsConstFuncRE    = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?\(?[^=]*?\)?\s*=>`)
	jsMethodRE       = regexp.MustCompile(`^\s*(?:async\s+)?([A-Za-z_$][A-Za-z0-9_$]*)\s*\([^)]*\)\s*\{`)
	jsEmitRE         = regexp.MustCompile(`(?:\.|\b)(emit|dispatchEvent)\s*\(\s*(?:new\s+CustomEvent\s*\()?['"` + "`" + `]([^'"` + "`" + `]+)['"` + "`" + `]`)
	jsOnRE           = regexp.MustCompile(`(?:\.|\b)(on|once|addEventListener)\s*\(\s*['"` + "`" + `]([^'"` + "`" + `]+)['"` + "`" + `]`)
	jsRegisterRE     = regexp.MustCompile(`\b(register(?:Action|Scene|Character|Item|Stat|Quest|MiniGame|Hotkey)|core\.register(?:Action|Scene|Character|Item|Stat|Quest|MiniGame|Hotkey))\s*\(\s*['"` + "`" + `]?([^,'"` + "`" + `)\s]+)`)
	jsModsRegisterRE = regexp.MustCompile(`\b(mods\.register|registerMod)\s*\(\s*['"` + "`" + `]?([^,'"` + "`" + `)\s]+)`)
	jsImportRE       = regexp.MustCompile(`^\s*import\b`)
)

type grepHit struct {
	File string
	Line int
	Text string
}

type labeledPattern struct {
	Label string
	Regex string
}

// The program ships with NO domain-specific patterns/sinks. They are project-specific
// (e.g. @KafkaListener, *repository*.save) and live entirely in config.json lens params,
// keeping the tool general across stacks.

// locCol is the column where the file:line locator sits, so code reads first (meaning)
// and the location lines up as a second column (direction).
const locCol = 140

type javaUnroller struct {
	cfg          Config
	lens         LensConfig
	env          map[string]string
	decisions    []string
	seenDecision map[string]bool
	// Branch-select mode (UI only; CLI/benchmark leave selectMode=false so the
	// undecidable "show both branches" behavior below is unchanged). When on, an
	// undecidable conditional collapses to one side — forced[id] if the user toggled
	// it, else the longest branch — and is recorded in choices so the UI can offer a
	// per-conditional toggle. id is "file:line" of the `if`.
	selectMode  bool
	forced      map[string]string
	choices     []branchChoice
	choiceSeen  map[string]bool
	inlineDepth int
	inlineSkips map[string]bool
	calls       []inlineCallChoice
	callSeen    map[string]bool
}

const dataFlowCommentCol = 56

var dropInRE = regexp.MustCompile(`^([A-Za-z0-9_./\-]+\.[A-Za-z0-9]+):(\d+)(?:-(\d+))?$`)

// ParsedBlock is a block recovered from an existing projection file, including
// the source anchor metadata needed for two-way sync.
type ParsedBlock struct {
	Anchor   string
	File     string
	ID       string
	Tool     string
	Mode     string
	Hash     string
	Sync     string
	SrcFile  string
	SrcStart int
	SrcEnd   int
	SrcHash  string
	Lines    []string
	Origins  []LineOrigin
}

var anchorRE = regexp.MustCompile(`^@@ (.+?)#(.+?) \[([^.]+)\.([^ \]]+) hash=([0-9a-f]+)(?: sync=two-way src=(.+?):(\d+)-(\d+) srchash=([0-9a-f]+))?\]$`)

var originFactRE = regexp.MustCompile(`^=> (.+?): origin (\d+) src=(.+):(\d+) srchash=([0-9a-f]+)$`)

// SyncResult reports what SyncProjection did for one projection file.
type SyncResult struct {
	ToProjection int
	ToSource     int
	Conflicts    []string
}

var idRangeRE = regexp.MustCompile(`:(\d+)-(\d+)$`)

//go:embed ui
var uiFS embed.FS
