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

INPUT=$(cat 2>/dev/null || true)

MODEL=""
if command -v python3 >/dev/null 2>&1; then
  MODEL=$(printf '%s' "$INPUT" | python3 -c '
import sys, json
try:
    data = json.load(sys.stdin)
    model = data.get("model")
    if isinstance(model, str):
        sys.stdout.write(model.strip())
except Exception:
    pass
' 2>/dev/null) || MODEL=""
fi

print_body() {
cat <<'EOF'
Before acting on EACH user request, classify it:

EXPRESS — all of the following hold:
- The request resolves as ONE self-contained change (or is conversational /
  read-only / a question).
- No task decomposition, no design decisions, no cross-task coordination.
- Expected context bloat is small: few tool calls, little file reading,
  mechanical or localized change.
File count is NOT a criterion; incidental doc updates may ride along.
→ Handle it directly yourself, or via a single cheap subagent
  (model: haiku or sonnet). No verifier pipeline, no workflow.

ORCHESTRATED — anything else, or WHENEVER IN DOUBT:
→ Load the `orchestrate` skill (Skill tool) and follow its playbook:
  decompose, write contracts, delegate to cheap workers with adversarial
  verifiers, receive only structured verdicts.

If an EXPRESS task turns out to need decomposition or design judgment
mid-flight, abort the express path and re-route to ORCHESTRATED.
</orchestra-router>
EOF
}

if [ -n "$MODEL" ]; then
  # Model is known: gate here in the hook.
  MODEL_LC=$(printf '%s' "$MODEL" | tr '[:upper:]' '[:lower:]')
  case "$MODEL_LC" in
    *opus*|*fable*)
      # Expensive instructor model: inject, no self-gate paragraph needed.
      echo "<orchestra-router>"
      print_body
      ;;
    *)
      # Cheap model (sonnet/haiku/...): inject nothing.
      :
      ;;
  esac
else
  # Model unknown: safe fallback -- full router with the self-gate
  # paragraph, identical to the pre-hybrid behavior.
  cat <<'EOF'
<orchestra-router>
This protocol applies ONLY when this session's main model is Opus or Fable
(check the model named in your system prompt; on Sonnet/Haiku, ignore this
entire block).

EOF
  print_body
fi

exit 0
