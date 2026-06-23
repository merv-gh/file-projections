#!/usr/bin/env bash
#
# mutation-eval.sh — A/B test: does the file-projections control-flow lens help a small
# local model fix a mutated condition with fewer tokens?
#
# For each mutant (a single wrong boolean condition in a Spring controller) we ask the
# same model the same task twice:
#   CONTROL  — given the whole mutated source file (the "read the file" baseline)
#   SKILL    — given only the control-flow projection (entry -> conditions -> save)
# We record input/output tokens (Ollama's prompt_eval_count / eval_count) and whether the
# reply restores the correct condition ("mutant killed").
#
# Usage: tools/mutation-eval.sh [model]      (default: qwen2.5-coder:3b — the small model)
set -euo pipefail

MODEL="${1:-qwen2.5-coder:3b}"
HOST="${OLLAMA_HOST:-http://localhost:11434}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FP="${FP:-$ROOT/bin/file-projections}"
SRC="spring-petclinic-main/src/main/java"
cd "$ROOT"

read -r -d '' RULES <<'EOF' || true
You are fixing a bug in a Spring MVC controller. Exactly ONE boolean condition is wrong.
The code must enforce these rules:
- A form submit must NOT reach save(...) when validation produced errors
  (it proceeds to save only when there are NO binding errors).
- A birth date or visit date in the future is invalid and must be rejected.
Find the single incorrect condition and reply with ONLY the corrected Java line
(the full `if (...) {` line), nothing else — no prose, no code fence.
EOF

# mutant fields (~-separated, since regexes contain |):
#   file ~ saveLine ~ perl_mutate ~ kill_grep ~ antikill_grep
MUTANTS=(
  "$SRC/org/springframework/samples/petclinic/owner/OwnerController.java~84~s/if \(result\.hasErrors\(\)\) \{/if (!result.hasErrors()) {/~if \(result\.hasErrors\(\)\)~!result\.hasErrors"
  "$SRC/org/springframework/samples/petclinic/owner/PetController.java~124~s/if \(result\.hasErrors\(\)\) \{/if (!result.hasErrors()) {/~if \(result\.hasErrors\(\)\)~!result\.hasErrors"
  "$SRC/org/springframework/samples/petclinic/owner/VisitController.java~109~s/if \(result\.hasErrors\(\)\) \{/if (!result.hasErrors()) {/~if \(result\.hasErrors\(\)\)~!result\.hasErrors"
  "$SRC/org/springframework/samples/petclinic/owner/PetController.java~124~s/getBirthDate\(\) != null && pet\.getBirthDate/getBirthDate() != null || pet.getBirthDate/~!= null && pet\.getBirthDate\(\)\.isAfter~!= null \|\| pet"
  "$SRC/org/springframework/samples/petclinic/owner/VisitController.java~109~s/&& !visit\.getDate\(\)\.isAfter/\&\& visit.getDate().isAfter/~&& !visit\.getDate\(\)\.isAfter~&& visit\.getDate\(\)\.isAfter"
)

ask() { # stdin = prompt -> "<prompt_tok>\t<output_tok>\t<answer collapsed>"
  local prompt; prompt="$(cat)"
  local resp
  resp="$(curl -s "$HOST/api/generate" \
    -d "$(jq -n --arg m "$MODEL" --arg p "$prompt" \
      '{model:$m,prompt:$p,stream:false,options:{temperature:0,num_predict:120}}')")"
  printf '%s\t%s\t%s\n' \
    "$(jq -r '.prompt_eval_count // 0' <<<"$resp")" \
    "$(jq -r '.eval_count // 0' <<<"$resp")" \
    "$(jq -r '.response // ""' <<<"$resp" | tr '\n' ' ')"
}

killed() { # $1=reply $2=kill_grep $3=antikill_grep -> echo KILL|live
  if grep -qE "$2" <<<"$1" && ! grep -qE "$3" <<<"$1"; then echo KILL; else echo live; fi
}

printf 'model: %s\n\n' "$MODEL"
printf '%-26s %-9s %7s %7s %6s\n' "case" "group" "in_tok" "out_tok" "kill"
printf '%s\n' "------------------------------------------------------------------"

c_in=0; c_out=0; c_kill=0; s_in=0; s_out=0; s_kill=0; n=0
: >/tmp/mut-eval-answers.log
for m in "${MUTANTS[@]}"; do
  IFS='~' read -r file saveln mut killg antik <<<"$m"
  n=$((n+1))
  base="$(basename "$file" .java)"
  rel="${file#"$SRC"/}"
  cp "$file" "/tmp/orig.$n.java"
  perl -i -pe "$mut" "$file"

  # CONTROL: whole mutated file
  IFS=$'\t' read -r pin pout ans < <(printf '%s\n\nFILE %s:\n%s\n' "$RULES" "$base" "$(cat "$file")" | ask)
  k="$(killed "$ans" "$killg" "$antik")"
  printf '%-26s %-9s %7s %7s %6s\n' "$base:$saveln" "control" "$pin" "$pout" "$k"
  printf '[%s control %s] %s\n' "$base:$saveln" "$k" "$ans" >>/tmp/mut-eval-answers.log
  c_in=$((c_in+pin)); c_out=$((c_out+pout)); [ "$k" = KILL ] && c_kill=$((c_kill+1))

  # SKILL: control-flow projection of the mutated file
  "$FP" -analyzer control-flow -source-root "$SRC" -file "$rel" -line "$saveln" -out /tmp/mut.projection >/dev/null 2>&1 || true
  proj="$(cat /tmp/mut.projection /tmp/mut.branch-*.projection 2>/dev/null)"
  IFS=$'\t' read -r pin pout ans < <(printf '%s\n\nControl-flow paths (entry -> conditions -> save):\n%s\n' "$RULES" "$proj" | ask)
  k="$(killed "$ans" "$killg" "$antik")"
  printf '%-26s %-9s %7s %7s %6s\n' "$base:$saveln" "skill" "$pin" "$pout" "$k"
  printf '[%s skill %s] %s\n' "$base:$saveln" "$k" "$ans" >>/tmp/mut-eval-answers.log
  s_in=$((s_in+pin)); s_out=$((s_out+pout)); [ "$k" = KILL ] && s_kill=$((s_kill+1))

  cp "/tmp/orig.$n.java" "$file"          # restore
  rm -f /tmp/mut.projection /tmp/mut.branch-*.projection
  printf '%s\n' "------------------------------------------------------------------"
done

echo
printf '%-9s %8s %8s %12s\n' "group" "in_tok" "out_tok" "mutants_killed"
printf '%-9s %8s %8s %10s/%d\n' "control" "$c_in" "$c_out" "$c_kill" "$n"
printf '%-9s %8s %8s %10s/%d\n' "skill"   "$s_in" "$s_out" "$s_kill" "$n"
echo
echo "win condition: skill kills >= control AND uses fewer input tokens."
