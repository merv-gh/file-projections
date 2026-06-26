package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestPostgresWatchPollsContainer(t *testing.T) {
	if os.Getenv("FILE_PROJECTIONS_POSTGRES_IT") != "1" {
		t.Skip("set FILE_PROJECTIONS_POSTGRES_IT=1 to run the Postgres container integration test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	port := freeTCPPort(t)
	name := "file-projections-pg-" + strconvBase36(time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "-d",
		"--name", name,
		"-e", "POSTGRES_PASSWORD=postgres",
		"-e", "POSTGRES_DB=fp",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"postgres:16-alpine")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Skipf("could not start postgres container: %v\n%s", err, out)
	}
	defer exec.Command("docker", "rm", "-f", name).Run()

	dsn := fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/fp?sslmode=disable", port)
	db := waitForPostgres(t, dsn)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE events (id BIGSERIAL PRIMARY KEY, kind TEXT NOT NULL, amount INT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO events(kind, amount) VALUES ('seed', 1)`); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	nowFunc = func() time.Time { return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC) }
	defer func() { nowFunc = time.Now }()
	cfg := Config{Root: dir, ProjectionsDir: ".projections", Lenses: []LensConfig{{
		Name:     "pg",
		Out:      filepath.Join(dir, ".projections", "pg.projection"),
		Analyzer: "postgres-watch",
		Params: map[string]string{
			"connections":    fmt.Sprintf(`{"dev":%q}`, dsn),
			"tables":         "events",
			"window_minutes": "5",
			"bootstrap":      "latest",
		},
	}}}
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	first := read(t, filepath.Join(dir, ".projections", "pg.projection"))
	if strings.Contains(first, "seed") || !strings.Contains(first, "bootstrap=true") {
		t.Fatalf("first poll should bootstrap at current max id without replaying seed:\n%s", first)
	}

	if _, err := db.Exec(`INSERT INTO events(kind, amount) VALUES ('created', 7), ('paid', 9)`); err != nil {
		t.Fatal(err)
	}
	nowFunc = func() time.Time { return time.Date(2026, 6, 26, 12, 0, 30, 0, time.UTC) }
	if _, err := Run(cfg, DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(dir, ".projections", "pg.projection"))
	for _, want := range []string{
		"seen_at,env,table,id,kind,amount",
		"2026-06-26T12:00:30Z,dev,events,2,created,7",
		"2026-06-26T12:00:30Z,dev,events,3,paid,9",
		"new_rows=2 last_id=3",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("postgres projection missing %q:\n%s", want, got)
		}
	}
}

func TestLensConfigParamsAcceptStructuredJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	write(t, path, `{
  "root": ".",
  "lenses": [{
    "name": "pg",
    "analyzer": "postgres-watch",
    "params": {
      "connections": {"dev":"postgres://example/db"},
      "tables": ["events","public.audit"]
    }
  }]
}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	params := cfg.Lenses[0].Params
	if params["connections"] != `{"dev":"postgres://example/db"}` {
		t.Fatalf("connections param not JSON-stringified: %q", params["connections"])
	}
	if params["tables"] != `["events","public.audit"]` {
		t.Fatalf("tables param not JSON-stringified: %q", params["tables"])
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForPostgres(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		db, err := sql.Open("postgres", dsn)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err = db.PingContext(ctx)
			cancel()
			if err == nil {
				return db
			}
			db.Close()
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("postgres did not become ready: %v", lastErr)
	return nil
}

func strconvBase36(n int64) string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n < 0 {
		n = -n
	}
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = chars[n%36]
		n /= 36
	}
	return string(b[i:])
}
