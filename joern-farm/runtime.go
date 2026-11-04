package main

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxScriptBytes = 1 << 20
const maxQueryLogBytes = 128 << 10

var heapPattern = regexp.MustCompile(`^[1-9][0-9]{0,6}[mMgG]$`)
var paramPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
var querySlots = make(chan struct{}, 1)
var runQuery = executeQuery

func acquireQuerySlot() bool {
	select {
	case querySlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseQuerySlot() { <-querySlots }

func normalizeLanguage(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "java", "javasrc":
		return "java", nil
	case "js", "javascript", "typescript", "ts", "jssrc":
		return "javascript", nil
	default:
		return "", fmt.Errorf("unsupported language %q: choose java or javascript (JS/TS)", value)
	}
}

func queryConfig(c FarmConfig) FarmConfig {
	if c.JoernHeap == "" {
		c.JoernHeap = "4g"
	}
	if c.QueryTimeoutSeconds == 0 {
		c.QueryTimeoutSeconds = 300
	}
	if c.QueryMaxOutputBytes == 0 {
		c.QueryMaxOutputBytes = 64 << 20
	}
	return c
}

func validateRuntimeConfig(c FarmConfig) error {
	if !heapPattern.MatchString(c.JoernHeap) {
		return fmt.Errorf("JOERN_HEAP must be a positive JVM heap such as 4096m or 4g")
	}
	if c.QueryTimeoutSeconds < 1 || c.QueryTimeoutSeconds > 3600 {
		return fmt.Errorf("QUERY_TIMEOUT_SECONDS must be between 1 and 3600")
	}
	if c.QueryMaxOutputBytes < 1 || c.QueryMaxOutputBytes > 512<<20 {
		return fmt.Errorf("QUERY_MAX_OUTPUT_BYTES must be between 1 and 536870912")
	}
	return nil
}

func shellQuote(value string) string { return "'" + escapeShell(value) + "'" }

func parseCommand(tools ContainerTools, language, src, cpg, heap string) (string, error) {
	language, err := normalizeLanguage(language)
	if err != nil {
		return "", err
	}
	if !heapPattern.MatchString(heap) {
		return "", fmt.Errorf("invalid JOERN_HEAP")
	}
	prefix := "unset _JAVA_OPTIONS JAVA_TOOL_OPTIONS JDK_JAVA_OPTIONS; "
	flags := "-J-Xmx" + heap
	if language == "javascript" {
		if tools.Jssrc2cpg == "" {
			return "", fmt.Errorf("jssrc2cpg.sh not found: JavaScript/TypeScript jobs require the explicit JS frontend")
		}
		return prefix + shellQuote(tools.Jssrc2cpg) + " " + flags + " " + shellQuote(src) + " -o " + shellQuote(cpg), nil
	}
	if tools.Javasrc2cpg != "" {
		return prefix + shellQuote(tools.Javasrc2cpg) + " " + flags + " " + shellQuote(src) + " --delombok-mode no-delombok -o " + shellQuote(cpg), nil
	}
	if tools.JoernParse != "" {
		return prefix + shellQuote(tools.JoernParse) + " " + flags + " " + shellQuote(src) + " --language javasrc --output " + shellQuote(cpg) + " --frontend-args --delombok-mode no-delombok", nil
	}
	return "", fmt.Errorf("no Java parse tool found in container")
}

func discoverJssrcFrontend(container string) string {
	for _, candidate := range []string{"jssrc2cpg.sh", "jssrc2cpg"} {
		if out, err := dockerExec(container, "sh", "-c", "command -v "+candidate+" 2>/dev/null"); err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
	}
	for _, candidate := range []string{"/opt/joern/joern-cli/frontends/jssrc2cpg.sh", "/opt/joern/frontends/jssrc2cpg.sh", "/opt/joern/joern-cli/frontends/jssrc2cpg/bin/jssrc2cpg", "/opt/joern/frontends/jssrc2cpg/bin/jssrc2cpg"} {
		if _, err := dockerExec(container, "test", "-x", candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func validateQueryParams(params []string) ([]string, error) {
	if len(params) > 64 {
		return nil, fmt.Errorf("at most 64 query parameters are allowed")
	}
	seen := make(map[string]bool)
	for _, param := range params {
		parts := strings.SplitN(param, "=", 2)
		if len(param) > 4096 || len(parts) != 2 || !paramPattern.MatchString(parts[0]) || strings.ContainsRune(param, 0) {
			return nil, fmt.Errorf("query parameters must be bounded name=value pairs")
		}
		if parts[0] == "cpgPath" || parts[0] == "out" || seen[parts[0]] {
			return nil, fmt.Errorf("duplicate or farm-owned query parameter %q", parts[0])
		}
		seen[parts[0]] = true
	}
	return params, nil
}

func queryCommand(heap, script, cpg, out string, params []string) (string, error) {
	if !heapPattern.MatchString(heap) {
		return "", fmt.Errorf("invalid JOERN_HEAP")
	}
	if _, err := validateQueryParams(params); err != nil {
		return "", err
	}
	args := []string{"joern", "-J-Xmx" + heap, "--script", script, "--param", "cpgPath=" + cpg, "--param", "out=" + out}
	for _, param := range params {
		args = append(args, "--param", param)
	}
	for i, value := range args {
		args[i] = shellQuote(value)
	}
	return strings.Join(args, " "), nil
}

// tailBuffer bounds captured JVM diagnostics even when a query floods stdout/stderr.
type tailBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
	} else {
		if excess := len(b.data) + len(p) - b.limit; excess > 0 {
			copy(b.data, b.data[excess:])
			b.data = b.data[:len(b.data)-excess]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (b *tailBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }

// A process group is required so timeout/disconnect stops the JVM, not just docker exec.
func executeQuery(ctx context.Context, container, command, pidFile string, maxOutput int64) (string, error) {
	inner := "ulimit -f " + strconv.FormatInt((maxOutput+511)/512, 10) + " || exit 125; echo $$ > " + shellQuote(pidFile) + "; exec " + command
	wrapped := "unset _JAVA_OPTIONS JAVA_TOOL_OPTIONS JDK_JAVA_OPTIONS; command -v setsid >/dev/null || exit 127; exec setsid --wait sh -c " + shellQuote(inner)
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-c", wrapped)
	output := &tailBuffer{limit: maxQueryLogBytes}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if ctx.Err() != nil {
		// The shared pid file contains a worker-created PID; validate it before signaling.
		cleanup := "if test -s " + shellQuote(pidFile) + "; then p=$(cat " + shellQuote(pidFile) + "); case $p in ''|*[!0-9]*) exit 1;; esac; /bin/kill -TERM -- -\"$p\" 2>/dev/null || true; sleep 1; /bin/kill -KILL -- -\"$p\" 2>/dev/null || true; fi"
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stop := exec.CommandContext(cleanupCtx, "docker", "exec", container, "sh", "-c", cleanup)
		stop.Stdout = output
		stop.Stderr = output
		stopErr := stop.Run()
		cancel()
		if stopErr != nil {
			return output.String(), fmt.Errorf("%w; worker cleanup could not be confirmed: %v", ctx.Err(), stopErr)
		}
		return output.String(), ctx.Err()
	}
	return output.String(), err
}
