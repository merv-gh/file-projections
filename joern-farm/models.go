package main

import (
	"sync"
	"time"
)

type FarmConfig struct {
	Port        string `json:"port"`
	DataDir     string `json:"data_dir"`
	WorkerCount int    `json:"worker_count"`
	JoernHeap   string `json:"joern_heap"`
}

type Job struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Branch    string     `json:"branch,omitempty"`
	Status    string     `json:"status"` // queued, parsing, exporting, packaging, done, failed
	Progress  int        `json:"progress"`
	Error     string     `json:"error,omitempty"`
	Worker    string     `json:"worker,omitempty"`
	Export    bool       `json:"export"` // false → keep the cpg.bin, skip neo4jcsv export (for CPG download / remote queries)
	Logs      []LogEntry `json:"logs"`
	StartedAt time.Time  `json:"startedAt"`
	DoneAt    *time.Time `json:"doneAt,omitempty"`
	mu        sync.Mutex
}

type LogEntry struct {
	Time    string `json:"time"`
	Message string `json:"message"`
	Level   string `json:"level"` // info, error, done
}

func (j *Job) addLog(message, level string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Logs = append(j.Logs, LogEntry{
		Time:    time.Now().Format("15:04:05"),
		Message: message,
		Level:   level,
	})
}

func (j *Job) setStatus(status string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = status
}

func (j *Job) setProgress(pct int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Progress = pct
}

func (j *Job) fail(msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = "failed"
	j.Error = msg
	now := time.Now()
	j.DoneAt = &now
}

func (j *Job) complete() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = "done"
	j.Progress = 100
	now := time.Now()
	j.DoneAt = &now
}

// Edge types needed by the boundary-map backend.
// Everything else (CDG, DDG, DOMINATE, etc.) is dropped to reduce transfer size.
var NeededEdges = map[string]bool{
	"AST": true, "CALL": true, "CONTAINS": true, "INHERITS_FROM": true,
	"REF": true, "BINDS_TO": true, "ARGUMENT": true, "RECEIVER": true,
	"CFG": true, "SOURCE_FILE": true, "EVAL_TYPE": true,
}
