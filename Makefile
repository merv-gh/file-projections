# file-projections — all dev flows go through make.
BIN          := file-projections
BINDIR       := bin
CONFIG       ?= config.json
PKG          := .
VERSION_FILE := VERSION
VERSION      := $(shell cat $(VERSION_FILE) 2>/dev/null || echo 0.0.0)

.PHONY: all help build run cpg bookmarks menu watch test eval perf fmt vet clean cross \
        version release-patch release-minor release-major

all: build

## help: list the available make targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'

## version: print the current version (from the VERSION file)
version:
	@echo $(VERSION)

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

## perf: benchmark all-to-all entry->exit on a repo (REPO=url|path) with a 5m cap
perf: build
	./$(BINDIR)/$(BIN) perf $(if $(REPO),-repo $(REPO),) $(PERF_ARGS)

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

# bump <part>: read VERSION, increment major/minor/patch, write it back, then commit and
# tag. Runs tests first and requires a clean tree. Pushing (which fires the release
# workflow) is left as an explicit follow-up so the outward-facing step stays deliberate.
define bump
	@test -z "$$(git status --porcelain)" || { echo "working tree not clean — commit or stash first"; exit 1; }
	@go test ./... >/dev/null
	@old=$$(cat $(VERSION_FILE)); \
	maj=$$(echo $$old | cut -d. -f1); min=$$(echo $$old | cut -d. -f2); pat=$$(echo $$old | cut -d. -f3); \
	case "$(1)" in \
	  major) maj=$$((maj+1)); min=0; pat=0;; \
	  minor) min=$$((min+1)); pat=0;; \
	  patch) pat=$$((pat+1));; \
	esac; \
	new=$$maj.$$min.$$pat; \
	echo $$new > $(VERSION_FILE); \
	git add $(VERSION_FILE); \
	git commit -m "release v$$new" >/dev/null; \
	git tag "v$$new"; \
	echo "bumped $$old -> v$$new and tagged. Publish with: git push --follow-tags"
endef

## release-patch: bump x.y.Z, commit, tag (then `git push --follow-tags` to release)
release-patch:
	$(call bump,patch)

## release-minor: bump x.Y.0, commit, tag (then `git push --follow-tags` to release)
release-minor:
	$(call bump,minor)

## release-major: bump X.0.0, commit, tag (then `git push --follow-tags` to release)
release-major:
	$(call bump,major)
