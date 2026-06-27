package main

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Database model across the workspace (TABLES.md §A): JPA entities, Spring Data
// repositories, @Table mappings and SQL migrations, resolved into the join chain
//
//	repository type -> entity -> physical table -> migration / postgres-watch
//
// so a database table becomes a first-class node on the service graph and a trace
// target ("how do we end up writing to orders?"). Pure-Go, regex, dependency-free.
// Coverage is Spring Data repository conventions + @Table + Flyway/Liquibase
// CREATE/ALTER — reported honestly; full ORM/SQL coverage is a non-goal.

// repoBaseInterfaces are the Spring Data base types whose first generic arg is the
// managed entity.
var repoBaseInterfaces = map[string]bool{
	"JpaRepository": true, "CrudRepository": true, "PagingAndSortingRepository": true,
	"ListCrudRepository": true, "ReactiveCrudRepository": true, "Repository": true,
	"MongoRepository": true, "JpaSpecificationExecutor": true,
}

// TableInfo is one physical database table discovered in the workspace.
type TableInfo struct {
	Name       string   // physical table name, e.g. "ledger_entries"
	Entity     string   // mapping entity simple name, or "" for migration-only tables
	EntityRepo string   // repo that declares the entity
	Migrations []string // migration files (repo-relative) that create/alter it
	MigRepo    string   // repo owning the migration(s)
	mapNote    string   // how the entity->table name was derived
}

// RepoTable links a repository type to the table it reads/writes.
type RepoTable struct {
	RepoType *JavaType
	Table    string
	Writes   bool
	Reads    bool
}

// DBModel is the workspace-wide database picture.
type DBModel struct {
	Tables     map[string]*TableInfo // physical table name -> info
	RepoTables []RepoTable           // repository type -> table access
	byRepoType map[string][]RepoTable
}

var (
	jpaTableNameRE = regexp.MustCompile(`@Table\s*\(\s*(?:[A-Za-z]+\s*=\s*[^,)]+,\s*)*name\s*=\s*"([^"]+)"`)
	jpaEntityRE    = regexp.MustCompile(`@Entity\b`)
	sqlCreateRE    = regexp.MustCompile(`(?i)\b(?:create|alter)\s+table\s+(?:if\s+not\s+exists\s+)?["` + "`" + `]?([A-Za-z_][A-Za-z0-9_."` + "`" + `]*)`)
	repoWriteRE    = regexp.MustCompile(`\b(save|saveAll|saveAndFlush|delete|deleteAll|deleteById|persist|merge|insert|update|upsert)\w*`)
	repoReadRE     = regexp.MustCompile(`\b(find|get|read|query|exists|count|stream|search)\w*`)
)

// buildDBModel scans the workspace's type index + each repo's migrations and resolves
// the entity/repository/table/migration mapping.
func buildDBModel(base Config, ws *Workspace, idx *TypeIndex) *DBModel {
	m := &DBModel{Tables: map[string]*TableInfo{}, byRepoType: map[string][]RepoTable{}}

	// entity simple name -> physical table (from @Table or default rule).
	entityTable := map[string]*JavaType{}
	for _, t := range idx.all {
		if hasAnnotation(t, jpaEntityRE) {
			entityTable[t.Name] = t
		}
	}
	tableNameFor := func(entity *JavaType) (string, string) {
		for _, a := range entity.Annotations {
			if mm := jpaTableNameRE.FindStringSubmatch(a); mm != nil {
				return mm[1], "@Table(name)"
			}
		}
		return toSnake(entity.Name), "default (snake_case of entity)"
	}

	// Register entity-backed tables.
	for _, entity := range entityTable {
		tbl, note := tableNameFor(entity)
		m.Tables[tbl] = &TableInfo{Name: tbl, Entity: entity.Name, EntityRepo: entity.Repo, mapNote: note}
	}

	// Repositories: extends a Spring Data base with the entity as the first generic.
	for _, t := range idx.all {
		entity := repositoryEntity(t)
		if entity == "" {
			continue
		}
		tbl := ""
		if e, ok := entityTable[entity]; ok {
			tbl, _ = tableNameFor(e)
		} else {
			tbl = toSnake(entity) // entity not found (maybe in a jar) — best effort.
			if m.Tables[tbl] == nil {
				m.Tables[tbl] = &TableInfo{Name: tbl, Entity: entity, mapNote: "default (entity not in workspace)"}
			}
		}
		writes, reads := repoAccessKinds(t)
		rt := RepoTable{RepoType: t, Table: tbl, Writes: writes, Reads: reads}
		m.RepoTables = append(m.RepoTables, rt)
		m.byRepoType[t.Name] = append(m.byRepoType[t.Name], rt)
	}

	// Migrations: Flyway (db/migration/V*.sql) + Liquibase (db/changelog/**.sql).
	for _, repo := range ws.Repos {
		cfg := base
		cfg.Root = repo.Path
		_ = walkAllFiles(cfg, repo.Path, func(rel string, lines []string) {
			low := strings.ToLower(rel)
			if !strings.HasSuffix(low, ".sql") {
				return
			}
			if !strings.Contains(low, "db/migration") && !strings.Contains(low, "db/changelog") && !strings.Contains(low, "migration") {
				return
			}
			for _, l := range lines {
				if mm := sqlCreateRE.FindStringSubmatch(l); mm != nil {
					tbl := normalizeTableName(mm[1])
					ti := m.Tables[tbl]
					if ti == nil {
						ti = &TableInfo{Name: tbl, mapNote: "from migration"}
						m.Tables[tbl] = ti
					}
					if !containsStr(ti.Migrations, rel) {
						ti.Migrations = append(ti.Migrations, rel)
						ti.MigRepo = repo.Name
					}
				}
			}
		})
	}
	return m
}

// repositoryEntity returns the managed entity simple name if t is a Spring Data
// repository (extends/implements a base repo interface with a generic entity arg).
func repositoryEntity(t *JavaType) string {
	if repoBaseInterfaces[t.Extends] && len(t.ExtendsArgs) > 0 {
		return t.ExtendsArgs[0]
	}
	// Some define `interface FooRepo extends Repository<Foo, Long>` with the base in
	// implements when declared on a class; ExtendsArgs already covers the common case.
	return ""
}

// repoAccessKinds inspects a repository's declared method names to decide whether it
// writes, reads, or both. Spring Data's built-in save/find always apply, so a plain
// `extends JpaRepository` (no custom methods) still counts as read+write.
func repoAccessKinds(t *JavaType) (writes, reads bool) {
	writes, reads = true, true // JpaRepository/CrudRepository give save+find by default
	return writes, reads
}

func repoAccessForCall(method string) (write, read bool) {
	if repoWriteRE.MatchString(method) {
		return true, false
	}
	if repoReadRE.MatchString(method) {
		return false, true
	}
	return false, false
}

func hasAnnotation(t *JavaType, re *regexp.Regexp) bool {
	for _, a := range t.Annotations {
		if re.MatchString(a) {
			return true
		}
	}
	return false
}

// tablesForRepoType returns the table access records for a repository type name.
func (m *DBModel) tablesForRepoType(name string) []RepoTable { return m.byRepoType[name] }

// reposWriting / reposReading return repository types that write / read a table.
func (m *DBModel) accessorsOf(table string) (writers, readers []*JavaType) {
	for _, rt := range m.RepoTables {
		if rt.Table != table {
			continue
		}
		if rt.Writes {
			writers = append(writers, rt.RepoType)
		}
		if rt.Reads {
			readers = append(readers, rt.RepoType)
		}
	}
	return
}

// sortedTableNames returns table names in stable order.
func (m *DBModel) sortedTableNames() []string {
	var out []string
	for t := range m.Tables {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// normalizeTableName strips quotes and a schema qualifier: `public."Orders"` -> orders.
func normalizeTableName(s string) string {
	s = strings.Trim(s, "\"`")
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(strings.Trim(s, "\"`"))
}

// toSnake converts an entity simple name to Spring's default table name (lower snake).
func toSnake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// walkAllFiles is like walkSourceFiles but visits every file (not just known source
// languages), so .sql migrations under resources/ are seen.
func walkAllFiles(cfg Config, base string, fn func(rel string, lines []string)) error {
	return fsWalkDir(cfg, base, func(path, rel string) {
		lines, err := readLines(path)
		if err != nil {
			return
		}
		fn(filepath.ToSlash(rel), lines)
	})
}

// fsWalkDir walks every file under base (applying the same dir-skip filtering as the
// source walker) and calls fn with the absolute path and the base-relative path.
func fsWalkDir(cfg Config, base string, fn func(path, rel string)) error {
	return filepath.WalkDir(base, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipDir(cfg, path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		fn(path, rel)
		return nil
	})
}
