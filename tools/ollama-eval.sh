#!/usr/bin/env bash
#
# ollama-eval.sh — measure the token cost and effect of the file-projections skill.
#
# Runs one standard task against a local Ollama model twice: WITHOUT the skill, and WITH
# the compressed skill.md prepended. Reports prompt/output token counts (from Ollama's
# prompt_eval_count / eval_count) so you can see exactly what the skill adds and whether
# it changes the answer. This is why skill.md is kept compressed — every skill token is
# paid on each WITH-skill request.
#
# Usage:  tools/ollama-eval.sh [model]      (default: qwen2.5-coder:3b)
set -euo pipefail

MODEL="${1:-qwen2.5-coder:3b}"
HOST="${OLLAMA_HOST:-http://localhost:11434}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
SKILL_FILE="$HERE/skill.md"

read -r -d '' TASK <<'EOF' || true
Repo context: a CLI tool generates "projection" files that answer one cross-file question
about a codebase. A user asks: "Show me every distinct execution path from the entry of a
Spring controller method to the line where it calls repository.save, with each branch in
its own file — and my code has else-if chains, switch, and loops."
Answer concisely: which command/lens and options should they use, and why?
EOF

ask() { # $1 = full prompt -> emits "<prompt_tokens> <output_tokens>\t<answer first line>"
  local prompt="$1"
  local resp
  resp="$(curl -s "$HOST/api/generate" \
    -d "$(jq -n --arg m "$MODEL" --arg p "$prompt" '{model:$m,prompt:$p,stream:false,options:{temperature:0}}')")"
  local pin pout ans
  pin="$(jq -r '.prompt_eval_count // 0' <<<"$resp")"
  pout="$(jq -r '.eval_count // 0' <<<"$resp")"
  ans="$(jq -r '.response // ""' <<<"$resp" | tr '\n' ' ' | cut -c1-220)"
  printf '%s\t%s\t%s\n' "$pin" "$pout" "$ans"
}

echo "model: $MODEL"
echo "skill: $SKILL_FILE ($(wc -w <"$SKILL_FILE" | tr -d ' ') words)"
echo

SKILL="$(cat "$SKILL_FILE")"

IFS=$'\t' read -r p0 o0 a0 < <(ask "$TASK")
IFS=$'\t' read -r p1 o1 a1 < <(ask "$SKILL

$TASK")

printf '%-14s %12s %12s\n' "" "prompt_tok" "output_tok"
printf '%-14s %12s %12s\n' "without skill" "$p0" "$o0"
printf '%-14s %12s %12s\n' "with skill"    "$p1" "$o1"
printf '%-14s %12s\n'      "skill cost"    "$(( p1 - p0 ))"
echo
echo "answer WITHOUT skill: $a0"
echo
echo "answer WITH skill:    $a1"
echo
echo "Note: 'skill cost' is the extra prompt tokens paid per request to inject the skill."
echo "Keep skill.md compressed to minimize it while preserving the guidance above."
