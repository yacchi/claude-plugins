#!/bin/bash
# SessionStart(compact) hook:
# Right after context compaction, injects the decision journal via additionalContext.
# Complements the built-in compaction summary, which does not preserve decision
# rationale: rejected alternatives, constraints, or abandoned approaches.
set -uo pipefail

command -v jq >/dev/null 2>&1 || exit 0

INPUT=$(cat)
SESSION_ID=$(printf '%s' "$INPUT" | jq -r '.session_id // empty' 2>/dev/null || true)
[[ -z "${SESSION_ID:-}" ]] && exit 0

LOG_FILE="${HOME}/.claude/compact-companion/logs/${SESSION_ID}.md"
[[ -s "$LOG_FILE" ]] || exit 0

# Injection size cap is ~16KB; on overflow, prefer the tail (newest decisions)
CONTENT=$(tail -c 16384 "$LOG_FILE" 2>/dev/null || true)
[[ -z "$CONTENT" ]] && exit 0

MSG="[compact-companion] Context compaction just ran. The built-in compaction summary does NOT preserve decision rationale: reasons behind adopted approaches, rejected alternatives, non-security constraints, or abandoned approaches. Below is the decision journal recorded in this session.

${CONTENT}

Instructions:
- If the compaction summary contradicts this journal, treat the journal as the source of truth
- Do not re-propose rejected alternatives. Do not retry abandoned approaches
- Journal file: \`${LOG_FILE}\`. Keep appending future decisions per the decision-log skill"

jq -n --arg m "$MSG" '{
  hookSpecificOutput: {
    hookEventName: "SessionStart",
    additionalContext: $m
  }
}'
exit 0
