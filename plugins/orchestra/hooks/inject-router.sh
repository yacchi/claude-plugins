#!/bin/bash
# orchestra plugin - SessionStart context injection (hybrid gate).
#
# On SessionStart, anything printed to stdout (with exit 0) is added to
# Claude's context. The hook input JSON arrives on stdin and includes a
# top-level "model" string field (the active model identifier, e.g.
# "claude-sonnet-5") -- but the field is documented as omittable, so it
# must be existence-checked before use.
#
# Gate logic:
#   1. model readable AND matches opus/fable (case-insensitive substring)
#      -> inject the router WITHOUT the self-gate paragraph (already gated here)
#   2. model readable AND matches neither
#      -> inject nothing (zero context cost on cheap sessions)
#   3. model missing/empty/unparseable/non-string, bad JSON, empty stdin,
#      or no python3 available
#      -> safe fallback: inject the full self-gated router (previous behavior)
#
# This script must NEVER exit nonzero: a failing SessionStart hook would
# get in the way of every session. Every branch ends in exit 0.
#
# Side channel: the gate verdict ("instructor" / "cheap") is persisted to a
# per-session state file so the UserPromptSubmit hook (remind-router.sh) can
# reuse it without re-deriving the model (UserPromptSubmit input is not
# guaranteed to carry a model field). Write failures are ignored.

INPUT=$(cat 2>/dev/null || true)

MODEL=""
SESSION_ID=""
if command -v python3 >/dev/null 2>&1; then
  PARSED=$(printf '%s' "$INPUT" | python3 -c '
import sys, json, re
try:
    data = json.load(sys.stdin)
    model = data.get("model")
    sid = data.get("session_id")
    model = model.strip() if isinstance(model, str) else ""
    sid = re.sub(r"[^A-Za-z0-9_-]", "", sid) if isinstance(sid, str) else ""
    sys.stdout.write(model + "\n" + sid)
except Exception:
    pass
' 2>/dev/null) || PARSED=""
  MODEL=$(printf '%s' "$PARSED" | sed -n '1p')
  SESSION_ID=$(printf '%s' "$PARSED" | sed -n '2p')
fi

write_state() {
  # $1: verdict string. Best-effort only; never fail the hook.
  if [ -n "$SESSION_ID" ]; then
    printf '%s' "$1" > "${TMPDIR:-/tmp}/orchestra-router-state-${SESSION_ID}" 2>/dev/null || true
  fi
}

print_body() {
cat <<'EOF'
Classify EACH user request before acting.

EXPRESS — ALL of: one self-contained change (or conversational/read-only/a
question); no decomposition, no design decisions, no cross-task coordination;
small expected context bloat. File count is NOT a criterion.
→ Do it yourself, or via ONE cheap subagent (haiku/sonnet). No verifier
  pipeline, no workflow. Hand the test/build run to a cheap subagent and read
  back only its verdict.
The gate is context bloat, NOT your model tier. Before judging size, ask "do I
already hold this content?" — if writing the delegation prompt would mean
spelling out the full spec, you already authored it: that is EXPRESS, write it
yourself however many files it touches. Delegation only pays for work needing
context you do NOT yet hold.

ORCHESTRATED — anything else, and WHENEVER IN DOUBT.
→ Load the `orchestra:run` skill and follow it: decompose, write contracts,
  delegate to cheap workers with adversarial verifiers, take back only
  structured verdicts.

A lane never persists — re-classify every request and at every phase shift.
Forced ORCHESTRATED, even mid-conversation: you are about to hand-write or
edit substantive code in 2+ files; a design discussion just turned into
"implement it" / 「PRを出しましょう」/「直しておきましょう」; or you are planning your
own multi-step implement-then-verify sequence. Then STOP and load
`orchestra:run` first.
</orchestra-router>
EOF
}

if [ -n "$MODEL" ]; then
  # Model is known: gate here in the hook.
  MODEL_LC=$(printf '%s' "$MODEL" | tr '[:upper:]' '[:lower:]')
  case "$MODEL_LC" in
    *opus*|*fable*)
      # Expensive instructor model: inject, no self-gate paragraph needed.
      write_state "instructor"
      echo "<orchestra-router>"
      print_body
      ;;
    *)
      # Cheap model (sonnet/haiku/...): inject nothing.
      write_state "cheap"
      ;;
  esac
else
  # Model unknown: safe fallback -- full router with the self-gate
  # paragraph, identical to the pre-hybrid behavior.
  write_state "unknown"
  cat <<'EOF'
<orchestra-router>
This protocol applies ONLY when this session's main model is Opus or Fable
(check the model named in your system prompt; on Sonnet/Haiku, ignore this
entire block).

EOF
  print_body
fi

exit 0
