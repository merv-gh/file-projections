# file-projections — all dev flows go through make.
BIN          := file-projections
BINDIR       := bin
CONFIG       ?= config.json
PKG          := .
VERSION_FILE := VERSION
VERSION      := $(shell cat $(VERSION_FILE) 2>/dev/null || echo 0.0.0)

.PHONY: all help build run cpg bookmarks menu watch test eval perf farm-up farm-down fmt vet clean cross \
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

## test: gofmt + vet + the test suite (lens output, control-flow branches, round-trip sync)
test:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...
	go test ./...

## eval: compare a standard task WITH vs WITHOUT the skill on a local Ollama model
eval:
	bash tools/ollama-eval.sh $(MODEL)

## perf: benchmark all-to-all entry->exit on a repo (REPO=url|path) with a 5m cap
perf: build
	./$(BINDIR)/$(BIN) perf $(if $(REPO),-repo $(REPO),) $(PERF_ARGS)

## farm-up: start the bundled joern-farm (remote Joern) on :9090
farm-up:
	cd joern-farm && docker compose up -d --build

## farm-down: stop the bundled joern-farm
farm-down:
	cd joern-farm && docker compose down

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

# The release message is the trailing word(s) after the target, e.g.
#   make release-minor "better format"
# Make sees those words as extra goals; we filter them back out into RELEASE_MSG and the
# catch-all `%:` rule below turns them into no-ops so make doesn't error on them.
RELEASE_MSG := $(strip $(filter-out release-patch release-minor release-major,$(MAKECMDGOALS)))

# release <part>: test, bump major/minor/patch, write the message as the entire release
# notes, stage everything, commit with exactly the message, tag, and push the branch and
# tags (`--tags` because `--follow-tags` does not reliably push the new tag here). The tag
# push fires the GitHub release workflow.
define release
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	@go vet ./...
	@go test ./... >/dev/null
	@msg='$(RELEASE_MSG)'; \
	test -n "$$msg" || { echo 'usage: make release-$(1) "message"'; exit 1; }; \
	old=$$(cat $(VERSION_FILE)); \
	maj=$$(echo $$old | cut -d. -f1); min=$$(echo $$old | cut -d. -f2); pat=$$(echo $$old | cut -d. -f3); \
	case "$(1)" in \
	  major) maj=$$((maj+1)); min=0; pat=0;; \
	  minor) min=$$((min+1)); pat=0;; \
	  patch) pat=$$((pat+1));; \
	esac; \
	new=$$maj.$$min.$$pat; \
	echo $$new > $(VERSION_FILE); \
	printf '%s\n' "$$msg" > RELEASE_NOTES.md; \
	git add -A; \
	git commit -m "$$msg" >/dev/null; \
	git tag "v$$new"; \
	git push origin HEAD; \
	git push origin --tags; \
	echo "released v$$old -> v$$new: $$msg"
endef

## release-patch: stage all, bump x.y.Z, commit+tag with MSG, push (make release-patch "msg")
release-patch:
	$(call release,patch)

## release-minor: stage all, bump x.Y.0, commit+tag with MSG, push (make release-minor "msg")
release-minor:
	$(call release,minor)

## release-major: stage all, bump X.0.0, commit+tag with MSG, push (make release-major "msg")
release-major:
	$(call release,major)

# Swallow the free-text release message words (e.g. "better", "format") as no-op goals so
# `make release-minor "better format"` doesn't fail with "No rule to make target".
%:
	@:
