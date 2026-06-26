package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// PostgreSQL analyzer: stateful high-water polling for append-heavy tables.

type postgresWatchState struct {
	Version int                                                `json:"version"`
	Env     map[string]map[string]map[string]postgresTableMark `json:"env"`
	Rows    []postgresSeenRow                                  `json:"rows"`
}

type postgresTableMark struct {
	LastID     int64  `json:"last_id"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

type postgresSeenRow struct {
	Env     string   `json:"env"`
	DB      string   `json:"db"`
	Table   string   `json:"table"`
	ID      int64    `json:"id"`
	SeenAt  string   `json:"seen_at"`
	Columns []string `json:"columns"`
	Values  []string `json:"values"`
}

type postgresPollResult struct {
	Env       string
	DB        string
	Table     string
	Columns   []string
	NewRows   int
	LastID    int64
	LastSeen  string
	Bootstrap bool
}

type postgresProjectionGroup struct {
	env   string
	db    string
	table string
	rows  []postgresSeenRow
}

var postgresIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

type postgresConfig struct {
	Environments map[string]map[string]string
	Watch        postgresWatchSelection
}

type postgresWatchSelection struct {
	Env    string                `json:"env"`
	Tables []postgresTableTarget `json:"tables"`
}

type postgresTableTarget struct {
	DB    string `json:"db"`
	Table string `json:"table"`
}

func AnalyzePostgresWatch(cfg Config, lens LensConfig) (Projection, error) {
	pg, err := parsePostgresConfig(lens.Params)
	if err != nil {
		return Projection{}, err
	}
	idColumn := coalesce(lens.Params["id_column"], "id")
	if !postgresIdentRE.MatchString(idColumn) || strings.Contains(idColumn, ".") {
		return Projection{}, fmt.Errorf("postgres-watch: invalid id_column %q", idColumn)
	}
	windowMinutes := atoiDefault(lens.Params["window_minutes"], 10)
	if windowMinutes <= 0 {
		return Projection{}, errors.New("postgres-watch: window_minutes must be positive")
	}
	limit := atoiDefault(lens.Params["limit"], 1000)
	if limit <= 0 {
		return Projection{}, errors.New("postgres-watch: limit must be positive")
	}
	bootstrap := strings.ToLower(coalesce(lens.Params["bootstrap"], "latest"))
	if bootstrap != "latest" && bootstrap != "all" {
		return Projection{}, errors.New("postgres-watch: bootstrap must be latest or all")
	}

	statePath := postgresStatePath(cfg, lens)
	state, err := loadPostgresState(statePath)
	if err != nil {
		return Projection{}, err
	}
	now := nowFunc().UTC()
	var pollResults []postgresPollResult
	envDBs := pg.Environments[pg.Watch.Env]
	for _, target := range pg.Watch.Tables {
		dsn := envDBs[target.DB]
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return Projection{}, fmt.Errorf("postgres-watch %s.%s: %w", pg.Watch.Env, target.DB, err)
		}
		db.SetMaxOpenConns(1)
		res, err := pollPostgresTable(db, &state, pg.Watch.Env, target.DB, target.Table, idColumn, bootstrap, limit, now)
		if err != nil {
			db.Close()
			return Projection{}, fmt.Errorf("postgres-watch %s.%s.%s: %w", pg.Watch.Env, target.DB, target.Table, err)
		}
		pollResults = append(pollResults, res)
		if err := db.Close(); err != nil {
			return Projection{}, fmt.Errorf("postgres-watch %s.%s: close: %w", pg.Watch.Env, target.DB, err)
		}
	}
	state.Rows = prunePostgresRows(state.Rows, now.Add(-time.Duration(windowMinutes)*time.Minute))
	sort.SliceStable(state.Rows, func(i, j int) bool {
		if state.Rows[i].SeenAt != state.Rows[j].SeenAt {
			return state.Rows[i].SeenAt < state.Rows[j].SeenAt
		}
		if state.Rows[i].Env != state.Rows[j].Env {
			return state.Rows[i].Env < state.Rows[j].Env
		}
		if state.Rows[i].DB != state.Rows[j].DB {
			return state.Rows[i].DB < state.Rows[j].DB
		}
		if state.Rows[i].Table != state.Rows[j].Table {
			return state.Rows[i].Table < state.Rows[j].Table
		}
		return state.Rows[i].ID < state.Rows[j].ID
	})
	if err := savePostgresState(statePath, state); err != nil {
		return Projection{}, err
	}
	return postgresProjection(lens, state, pollResults, windowMinutes, statePath), nil
}

func postgresWatchLenses(cfg Config) []LensConfig {
	var lenses []LensConfig
	for _, lens := range cfg.Lenses {
		if lens.Analyzer == "postgres-watch" {
			lenses = append(lenses, lens)
		}
	}
	return lenses
}

func postgresPollEvery(lenses []LensConfig) time.Duration {
	seconds := 30
	for _, lens := range lenses {
		if lens.Params == nil {
			continue
		}
		n := atoiDefault(lens.Params["poll_seconds"], 30)
		if n > 0 && n < seconds {
			seconds = n
		}
	}
	return time.Duration(seconds) * time.Second
}

func parsePostgresConfig(params map[string]string) (postgresConfig, error) {
	envs, err := parsePostgresEnvironments(params)
	if err != nil {
		return postgresConfig{}, err
	}
	watch, err := parsePostgresWatch(params, envs)
	if err != nil {
		return postgresConfig{}, err
	}
	return postgresConfig{Environments: envs, Watch: watch}, nil
}

func parsePostgresEnvironments(params map[string]string) (map[string]map[string]string, error) {
	raw := strings.TrimSpace(params["environments"])
	if raw == "" {
		raw = strings.TrimSpace(params["connections"])
	}
	if raw == "" {
		raw = strings.TrimSpace(params["envs"])
	}
	if raw == "" {
		raw = strings.TrimSpace(params["dsn"])
	}
	if raw == "" {
		return nil, errors.New("postgres-watch: params.environments is required")
	}
	if !strings.HasPrefix(raw, "{") {
		return map[string]map[string]string{"default": map[string]string{"default": raw}}, nil
	}
	var tree map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &tree); err != nil {
		return nil, fmt.Errorf("postgres-watch: environments must be a JSON object: %w", err)
	}
	if len(tree) == 0 {
		return nil, errors.New("postgres-watch: environments must not be empty")
	}
	envs := map[string]map[string]string{}
	for envName, rawEnv := range tree {
		envName = strings.TrimSpace(envName)
		if envName == "" {
			return nil, errors.New("postgres-watch: environment name cannot be empty")
		}
		var dsn string
		if err := json.Unmarshal(rawEnv, &dsn); err == nil {
			if strings.TrimSpace(dsn) == "" {
				return nil, fmt.Errorf("postgres-watch: environment %q has an empty DSN", envName)
			}
			envs[envName] = map[string]string{"default": dsn}
			continue
		}
		dbs := map[string]string{}
		if err := json.Unmarshal(rawEnv, &dbs); err != nil {
			return nil, fmt.Errorf("postgres-watch: environment %q must map db names to DSNs: %w", envName, err)
		}
		if len(dbs) == 0 {
			return nil, fmt.Errorf("postgres-watch: environment %q has no databases", envName)
		}
		for dbName, dbDSN := range dbs {
			if strings.TrimSpace(dbName) == "" || strings.TrimSpace(dbDSN) == "" {
				return nil, fmt.Errorf("postgres-watch: environment %q has an empty db name or DSN", envName)
			}
		}
		envs[envName] = dbs
	}
	return envs, nil
}

func parsePostgresWatch(params map[string]string, envs map[string]map[string]string) (postgresWatchSelection, error) {
	watch := postgresWatchSelection{}
	if raw := strings.TrimSpace(params["watch"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &watch); err != nil {
			return watch, fmt.Errorf("postgres-watch: watch must be {env,tables}: %w", err)
		}
	}
	if watch.Env == "" {
		watch.Env = strings.TrimSpace(params["env"])
	}
	if watch.Env == "" {
		watch.Env = firstSortedEnv(envs)
	}
	if _, ok := envs[watch.Env]; !ok {
		return watch, fmt.Errorf("postgres-watch: active env %q not found in environments", watch.Env)
	}
	if len(watch.Tables) == 0 {
		tables, err := parsePostgresTableTargets(params["tables"], envs[watch.Env])
		if err != nil {
			return watch, err
		}
		watch.Tables = tables
	}
	seen := map[string]bool{}
	out := watch.Tables[:0]
	for _, target := range watch.Tables {
		target.DB = strings.TrimSpace(target.DB)
		target.Table = strings.TrimSpace(target.Table)
		if target.DB == "" {
			if len(envs[watch.Env]) == 1 {
				target.DB = firstSortedDB(envs[watch.Env])
			} else {
				return watch, fmt.Errorf("postgres-watch: table %q must include db because env %q has multiple databases", target.Table, watch.Env)
			}
		}
		if _, ok := envs[watch.Env][target.DB]; !ok {
			return watch, fmt.Errorf("postgres-watch: db %q not found in env %q", target.DB, watch.Env)
		}
		if !postgresIdentRE.MatchString(target.Table) {
			return watch, fmt.Errorf("postgres-watch: invalid table %q; use table or schema.table identifiers", target.Table)
		}
		key := target.DB + "\x00" + target.Table
		if !seen[key] {
			seen[key] = true
			out = append(out, target)
		}
	}
	if len(out) == 0 {
		return watch, errors.New("postgres-watch: watch.tables is empty")
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DB == out[j].DB {
			return out[i].Table < out[j].Table
		}
		return out[i].DB < out[j].DB
	})
	watch.Tables = out
	return watch, nil
}

func parsePostgresTableTargets(raw string, dbs map[string]string) ([]postgresTableTarget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("postgres-watch: watch.tables or params.tables is required")
	}
	var targets []postgresTableTarget
	if strings.HasPrefix(raw, "[") {
		var objs []postgresTableTarget
		if err := json.Unmarshal([]byte(raw), &objs); err == nil && len(objs) > 0 {
			return objs, nil
		}
		var names []string
		if err := json.Unmarshal([]byte(raw), &names); err != nil {
			return nil, fmt.Errorf("postgres-watch: tables must be a JSON array or comma list: %w", err)
		}
		for _, name := range names {
			targets = append(targets, parsePostgresTableName(name, dbs))
		}
		return targets, nil
	}
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			targets = append(targets, parsePostgresTableName(s, dbs))
		}
	}
	return targets, nil
}

func parsePostgresTableName(name string, dbs map[string]string) postgresTableTarget {
	name = strings.TrimSpace(name)
	if len(dbs) > 1 {
		for dbName := range dbs {
			prefix := dbName + "."
			if strings.HasPrefix(name, prefix) {
				return postgresTableTarget{DB: dbName, Table: strings.TrimPrefix(name, prefix)}
			}
		}
	}
	return postgresTableTarget{Table: name}
}

func postgresStatePath(cfg Config, lens LensConfig) string {
	if lens.Params["state"] != "" {
		return filepath.Join(cfg.Root, filepath.FromSlash(lens.Params["state"]))
	}
	name := lens.Name
	if name == "" {
		name = "postgres-watch"
	}
	return filepath.Join(cfg.Root, cfg.ProjectionsDir, ".postgres-watch", safeFileName(name)+".json")
}

func loadPostgresState(path string) (postgresWatchState, error) {
	state := postgresWatchState{Version: 2, Env: map[string]map[string]map[string]postgresTableMark{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, err
	}
	if state.Version == 0 {
		state.Version = 2
	}
	if state.Env == nil {
		state.Env = map[string]map[string]map[string]postgresTableMark{}
	}
	return state, nil
}

func savePostgresState(path string, state postgresWatchState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0644)
}

func pollPostgresTable(db *sql.DB, state *postgresWatchState, envName, dbName, table, idColumn, bootstrap string, limit int, now time.Time) (postgresPollResult, error) {
	if state.Env == nil {
		state.Env = map[string]map[string]map[string]postgresTableMark{}
	}
	if state.Env[envName] == nil {
		state.Env[envName] = map[string]map[string]postgresTableMark{}
	}
	if state.Env[envName][dbName] == nil {
		state.Env[envName][dbName] = map[string]postgresTableMark{}
	}
	qTable := quotePostgresName(table)
	qID := quotePostgresName(idColumn)
	columns, err := postgresColumns(db, qTable)
	if err != nil {
		return postgresPollResult{}, err
	}
	mark, hadMark := state.Env[envName][dbName][table]
	if !hadMark && bootstrap == "latest" {
		maxID, err := postgresMaxID(db, qTable, qID)
		if err != nil {
			return postgresPollResult{}, err
		}
		mark = postgresTableMark{LastID: maxID, LastSeenAt: now.Format(time.RFC3339)}
		state.Env[envName][dbName][table] = mark
		return postgresPollResult{Env: envName, DB: dbName, Table: table, Columns: columns, LastID: maxID, LastSeen: mark.LastSeenAt, Bootstrap: true}, nil
	}
	rows, maxID, err := postgresRowsAfter(db, qTable, qID, idColumn, mark.LastID, limit)
	if err != nil {
		return postgresPollResult{}, err
	}
	seenAt := now.Format(time.RFC3339)
	for _, row := range rows {
		id, err := rowID(row.columns, row.values, idColumn)
		if err != nil {
			return postgresPollResult{}, err
		}
		if id > maxID {
			maxID = id
		}
		state.Rows = append(state.Rows, postgresSeenRow{
			Env: envName, DB: dbName, Table: table, ID: id, SeenAt: seenAt,
			Columns: row.columns, Values: row.values,
		})
	}
	if maxID > mark.LastID || len(rows) > 0 {
		mark.LastID = maxID
		mark.LastSeenAt = seenAt
		state.Env[envName][dbName][table] = mark
	}
	return postgresPollResult{Env: envName, DB: dbName, Table: table, Columns: columns, NewRows: len(rows), LastID: mark.LastID, LastSeen: mark.LastSeenAt}, nil
}

type postgresFetchedRow struct {
	columns []string
	values  []string
}

func postgresColumns(db *sql.DB, qTable string) ([]string, error) {
	rows, err := db.Query("SELECT * FROM " + qTable + " WHERE false")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	return cols, rows.Err()
}

func postgresMaxID(db *sql.DB, qTable, qID string) (int64, error) {
	var max sql.NullInt64
	if err := db.QueryRow("SELECT COALESCE(MAX(" + qID + "), 0) FROM " + qTable).Scan(&max); err != nil {
		return 0, err
	}
	if !max.Valid {
		return 0, nil
	}
	return max.Int64, nil
}

func postgresRowsAfter(db *sql.DB, qTable, qID, idColumn string, lastID int64, limit int) ([]postgresFetchedRow, int64, error) {
	rows, err := db.Query("SELECT * FROM "+qTable+" WHERE "+qID+" > $1 ORDER BY "+qID+" ASC LIMIT $2", lastID, limit)
	if err != nil {
		return nil, lastID, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, lastID, err
	}
	var out []postgresFetchedRow
	maxID := lastID
	for rows.Next() {
		raw := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, lastID, err
		}
		values := make([]string, len(cols))
		for i, v := range raw {
			values[i] = postgresValueString(v)
		}
		if id, err := rowID(cols, values, idColumn); err == nil && id > maxID {
			maxID = id
		}
		out = append(out, postgresFetchedRow{columns: cols, values: values})
	}
	if err := rows.Err(); err != nil {
		return nil, lastID, err
	}
	return out, maxID, nil
}

func rowID(columns, values []string, idColumn string) (int64, error) {
	for i, c := range columns {
		if c == idColumn && i < len(values) {
			id, err := strconv.ParseInt(strings.TrimSpace(values[i]), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("id column %q value %q is not an integer", idColumn, values[i])
			}
			return id, nil
		}
	}
	return 0, fmt.Errorf("id column %q not found", idColumn)
}

func postgresValueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(t)
	}
}

func prunePostgresRows(rows []postgresSeenRow, cutoff time.Time) []postgresSeenRow {
	out := rows[:0]
	for _, r := range rows {
		t, err := time.Parse(time.RFC3339, r.SeenAt)
		if err != nil {
			out = append(out, r)
			continue
		}
		if !t.Before(cutoff) {
			out = append(out, r)
		}
	}
	return out
}

func postgresProjection(lens LensConfig, state postgresWatchState, pollResults []postgresPollResult, windowMinutes int, statePath string) Projection {
	p := Projection{Sync: "view-only"}
	colsByTable := map[string][]string{}
	for _, res := range pollResults {
		colsByTable[postgresResultKey(res.Env, res.DB, res.Table)] = res.Columns
	}
	for _, row := range state.Rows {
		key := postgresResultKey(row.Env, row.DB, row.Table)
		if len(colsByTable[key]) == 0 {
			colsByTable[key] = row.Columns
		}
	}
	groups := map[string]*postgresProjectionGroup{}
	for _, res := range pollResults {
		key := postgresResultKey(res.Env, res.DB, res.Table)
		groups[key] = &postgresProjectionGroup{env: res.Env, db: res.DB, table: res.Table}
	}
	for _, row := range state.Rows {
		key := postgresResultKey(row.Env, row.DB, row.Table)
		if groups[key] == nil {
			groups[key] = &postgresProjectionGroup{env: row.Env, db: row.DB, table: row.Table}
		}
		groups[key].rows = append(groups[key].rows, row)
	}
	keys := sortedKeysGroup(groups)
	for _, key := range keys {
		g := groups[key]
		columns := append([]string{"seen_at", "env", "db", "table"}, colsByTable[key]...)
		lines := []string{csvLine(columns)}
		for _, row := range g.rows {
			values := append([]string{row.SeenAt, row.Env, row.DB, row.Table}, row.Values...)
			lines = append(lines, csvLine(values))
		}
		p.Blocks = append(p.Blocks, ProjectionBlock{
			ID:    postgresBlockID(g.env, g.db, g.table),
			File:  "postgres/" + g.env + "/" + g.db,
			Mode:  "postgres-watch",
			Tool:  "postgres",
			Lines: lines,
		})
	}
	for _, res := range pollResults {
		p.Facts = append(p.Facts, ProjectionFact{
			ID:   postgresBlockID(res.Env, res.DB, res.Table),
			Tool: "postgres",
			Text: fmt.Sprintf("env=%s db=%s table=%s new_rows=%d last_id=%d last_seen=%s bootstrap=%t", res.Env, res.DB, res.Table, res.NewRows, res.LastID, res.LastSeen, res.Bootstrap),
		})
	}
	p.Facts = append(p.Facts,
		ProjectionFact{ID: "window", Tool: "postgres", Text: fmt.Sprintf("%d minute rolling window by poll seen_at", windowMinutes)},
		ProjectionFact{ID: "state", Tool: "postgres", Text: filepath.ToSlash(statePath)},
	)
	p.Lens = lens
	return p
}

func csvLine(values []string) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write(values)
	w.Flush()
	return strings.TrimRight(b.String(), "\r\n")
}

func quotePostgresName(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstSortedEnv(envs map[string]map[string]string) string {
	keys := make([]string, 0, len(envs))
	for k := range envs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func firstSortedDB(dbs map[string]string) string {
	keys := sortedKeys(dbs)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func sortedKeysGroup(m map[string]*postgresProjectionGroup) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func postgresResultKey(envName, dbName, table string) string {
	return envName + "\x00" + dbName + "\x00" + table
}

func postgresBlockID(envName, dbName, table string) string {
	return envName + "." + dbName + "." + strings.ReplaceAll(table, ".", "_")
}

func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "postgres-watch"
	}
	return b.String()
}
