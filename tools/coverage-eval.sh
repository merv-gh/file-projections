#!/usr/bin/env bash
#
# coverage-eval.sh — dogfooding A/B/C: can a small local model raise main.go test coverage,
# and does the context source matter? Same task, three context variants, isolated worktrees,
# run sequentially:
#   read   — raw source slices around the target funcs (the "read the file" baseline)
#   graph  — code-review-graph structural context (signatures only, no bodies)
#   proj   — file-projections output (go-symbols index + verbatim slices)
# Records input/output tokens (Ollama counts), whether the suite compiles + passes, and the
# resulting line coverage. Baseline coverage is the untouched tree.
#
# Usage: tools/coverage-eval.sh [model]   (default: qwen2.5-coder:3b)
set -uo pipefail

MODEL="${1:-qwen2.5-coder:3b}"
HOST="${OLLAMA_HOST:-http://localhost:11434}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GOIMPORTS="${GOIMPORTS:-$(go env GOPATH)/bin/goimports}"
cd "$ROOT"

read -r -d '' TASK <<'EOF' || true
Write a Go test file for package `main` that increases coverage of these pure helper
functions from main.go: firstLine, truncate, dockerMount.
Write one separate TestXxx function per helper (NOT one shared table). Give each a few
cases including edge cases. Every struct literal must list ALL its fields, in order.
Behavior with exact examples (compute expected values exactly like these):
  firstLine("a\nb") == "a"   ; firstLine("solo") == "solo"
  truncate("abcdef", 3) == "abc…"   ; truncate("hi", 5) == "hi"   (cut to n BYTES, append "…" only when len(s)>n)
  dockerMount("/x/y") == "/x/y:/src"
Reply with ONLY the Go source (start at `package main`), no markdown, no prose.
EOF

baseline_cov() { go test -coverprofile=/tmp/base.cov ./... >/dev/null 2>&1; go tool cover -func=/tmp/base.cov | awk '/^total:/{print $3}'; }

ask() { # stdin=prompt -> "<in_tok>\t<out_tok>" ; response -> /tmp/cov.resp
  local resp; resp="$(curl -s "$HOST/api/generate" \
    -d "$(jq -n --arg m "$MODEL" --arg p "$(cat)" '{model:$m,prompt:$p,stream:false,options:{temperature:0,num_predict:1800}}')")"
  jq -r '.response // ""' <<<"$resp" >/tmp/cov.resp
  printf '%s\t%s' "$(jq -r '.prompt_eval_count // 0' <<<"$resp")" "$(jq -r '.eval_count // 0' <<<"$resp")"
}

# strip markdown fences and anything before `package`, keep from package line on
clean_go() { awk 'f{print} /^[[:space:]]*package main/{print; f=1}' "$1" | sed '/^```/d'; }

printf 'model: %s\nbaseline coverage: %s\n\n' "$MODEL" "$(baseline_cov)"
printf '%-7s %7s %7s %9s %7s %9s\n' "variant" "in_tok" "out_tok" "compiles" "passes" "coverage"
printf '%s\n' "---------------------------------------------------------------"

for v in read graph proj; do
  wt="/tmp/cov-$v"
  git worktree remove --force "$wt" >/dev/null 2>&1
  rm -rf "$wt"; git worktree prune
  git worktree add --detach "$wt" HEAD >/dev/null 2>&1 || { echo "$v: worktree add failed"; continue; }

  IFS=$'\t' read -r pin pout < <(printf '%s\n\nCONTEXT (%s):\n%s\n' "$TASK" "$v" "$(cat "/tmp/$v.ctx")" | ask)
  clean_go /tmp/cov.resp > "$wt/zz_cover_test.go"
  # Equal "apply" normalization for every variant: fix std imports + gofmt. This is what a
  # real apply step does; it removes import-forgetting noise so the comparison reflects the
  # test *logic* the context produced, not boilerplate the model dropped.
  "$GOIMPORTS" -w "$wt/zz_cover_test.go" >/dev/null 2>&1 || true

  comp=no; pass=no; cov="-"
  if (cd "$wt" && go vet ./... >"/tmp/$v.vet" 2>&1); then
    comp=yes
    if (cd "$wt" && go test -coverprofile=/tmp/$v.cov ./... >/dev/null 2>&1); then
      pass=yes
    fi
    cov="$( (cd "$wt" && go test -coverprofile=/tmp/$v.cov ./... >/dev/null 2>&1); go tool cover -func=/tmp/$v.cov 2>/dev/null | awk '/^total:/{print $3}')"
    [ -z "$cov" ] && cov="-"
  fi
  printf '%-7s %7s %7s %9s %7s %9s\n' "$v" "$pin" "$pout" "$comp" "$pass" "$cov"
  cp /tmp/cov.resp "/tmp/cov-$v.resp"
  git worktree remove --force "$wt" >/dev/null 2>&1
done

echo
echo "answers saved: /tmp/cov-{read,graph,proj}.resp"
