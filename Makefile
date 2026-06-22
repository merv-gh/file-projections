# file-projections — all dev flows go through make.
BIN      := file-projections
BINDIR   := bin
CONFIG   ?= config.json
PKG      := .

.PHONY: all build run cpg bookmarks menu watch test eval fmt vet clean cross

all: build

## build: compile the binary for the host platform into bin/
build:
	go build -o $(BINDIR)/$(BIN) $(PKG)

## run: generate every projection defined in $(CONFIG)
run: build
	./$(BINDIR)/$(BIN) -config $(CONFIG)

## cpg: build/refresh the cached Joern CPG for $(CONFIG)'s source roots
cpg: build
	./$(BINDIR)/$(BIN) build -config $(CONFIG)

## bookmarks: expand single-line drop-in .projection files into two-way bookmarks
bookmarks: build
	./$(BINDIR)/$(BIN) bookmarks -config $(CONFIG)

## menu: launch the interactive view builder (adds lenses, persists to config)
menu: build
	./$(BINDIR)/$(BIN) menu -config $(CONFIG)

## watch: regenerate on source change + sync two-way extract edits back to source
watch: build
	./$(BINDIR)/$(BIN) watch -config $(CONFIG)

## test: run the test suite (lens output, control-flow branches, round-trip sync)
test:
	go test ./...

## eval: compare a standard task WITH vs WITHOUT the skill on a local Ollama model
eval:
	bash tools/ollama-eval.sh $(MODEL)

fmt:
	go fmt ./...

vet:
	go vet ./...

## cross: build mac (amd64+arm64), linux, windows binaries into bin/
cross:
	GOOS=darwin  GOARCH=amd64 go build -o $(BINDIR)/$(BIN)-darwin-amd64  $(PKG)
	GOOS=darwin  GOARCH=arm64 go build -o $(BINDIR)/$(BIN)-darwin-arm64  $(PKG)
	GOOS=linux   GOARCH=amd64 go build -o $(BINDIR)/$(BIN)-linux-amd64   $(PKG)
	GOOS=windows GOARCH=amd64 go build -o $(BINDIR)/$(BIN)-windows-amd64.exe $(PKG)

clean:
	rm -rf $(BINDIR)
