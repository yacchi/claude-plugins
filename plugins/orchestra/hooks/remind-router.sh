#!/bin/bash
# orchestra plugin - UserPromptSubmit router reminder.
#
# Why this exists: the full <orchestra-router> block is injected once at
# SessionStart, which puts it at the very top of the context. In long
# sessions (hundreds of turns) that single injection decays — transcripts
# show instructor-model sessions doing 200+ direct Edits with zero mention
# of the EXPRESS/ORCHESTRATED classification. Recency is the fix: this hook
# appends a ~45-token reminder on EVERY user prompt so the classification
# step is adjacent to the request it must classify.
#
# Gating: UserPromptSubmit input is not guaranteed to carry a model field,
# so the SessionStart hook (inject-router.sh) persists its gate verdict to
# ${TMPDIR:-/tmp}/orchestra-router-state-<session_id> and this hook reads it:
#   "instructor" -> emit the reminder (no self-gate needed)
#   "cheap"      -> emit nothing (zero context cost on cheap sessions)
#   "unknown" / missing / unreadable -> emit the self-gated reminder
#
# This script must NEVER exit nonzero. Every branch ends in exit 0.

INPUT=$(cat 2>/dev/null || true)

SESSION_ID=""
if command -v python3 >/dev/null 2>&1; then
  SESSION_ID=$(printf '%s' "$INPUT" | python3 -c '
import sys, json, re
try:
    data = json.load(sys.stdin)
    sid = data.get("session_id")
    if isinstance(sid, str):
        sys.stdout.write(re.sub(r"[^A-Za-z0-9_-]", "", sid))
except Exception:
    pass
' 2>/dev/null) || SESSION_ID=""
fi

STATE="unknown"
if [ -n "$SESSION_ID" ]; then
  STATE_FILE="${TMPDIR:-/tmp}/orchestra-router-state-${SESSION_ID}"
  if [ -r "$STATE_FILE" ]; then
    STATE=$(cat "$STATE_FILE" 2>/dev/null) || STATE="unknown"
  fi
fi

print_reminder() {
cat <<'EOF'
<orchestra-router-reminder>Classify THIS request (see <orchestra-router>):
EXPRESS only if ONE small self-contained change, or if you already hold the
whole content in context. Otherwise hand-writing code in 2+ files yourself →
load `orchestra:run` first.</orchestra-router-reminder>
EOF
}

case "$STATE" in
  instructor)
    print_reminder
    ;;
  cheap)
    : # nothing — zero context cost
    ;;
  *)
    # Unknown model: self-gated reminder, mirroring inject-router.sh's fallback.
    echo "(Applies ONLY if this session's main model is Opus or Fable; on Sonnet/Haiku ignore this block.)"
    print_reminder
    ;;
esac

exit 0
