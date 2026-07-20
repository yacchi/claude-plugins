#!/bin/bash
# SessionStart(startup|resume|clear) hook:
# Announces this session's decision journal path via additionalContext.
# The decision-log skill appends decisions (adopted/rejected/constraint/abandoned)
# to the announced path.
set -uo pipefail

command -v jq >/dev/null 2>&1 || exit 0

INPUT=$(cat)
SESSION_ID=$(printf '%s' "$INPUT" | jq -r '.session_id // empty' 2>/dev/null || true)
[[ -z "${SESSION_ID:-}" ]] && exit 0

LOG_DIR="${HOME}/.claude/compact-companion/logs"
mkdir -p "$LOG_DIR" 2>/dev/null || true

# Auto-delete journals older than 14 days (cleanup for finished sessions)
find "$LOG_DIR" -type f -name '*.md' -mtime +14 -delete 2>/dev/null || true

LOG_FILE="$LOG_DIR/${SESSION_ID}.md"

MSG="[compact-companion] Decision journal for this session: \`${LOG_FILE}\`
Whenever a design decision occurs (adopted / rejected / constraint / abandoned), append one line to this file following the decision-log skill."

jq -n --arg m "$MSG" '{
  hookSpecificOutput: {
    hookEventName: "SessionStart",
    additionalContext: $m
  }
}'
exit 0
