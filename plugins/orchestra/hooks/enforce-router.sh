#!/bin/bash
# orchestra plugin - PreToolUse enforcement for enforcement.light_class=block.
#
# PURPOSE. Config already prefers an external executor (Copilot gpt-5.6-luna)
# over a direct Claude Haiku subagent for `light`-class implementation work,
# but an instructor keeps picking Haiku anyway because "spawn a Claude
# subagent" is 1 step and "relay through Copilot" used to be 6. This hook is
# the opt-in (`enforcement.light_class: "block"`, default "off") last-layer
# nudge: it denies the FIRST matching direct-Haiku spawn in a session and
# tells the model to call `agent-exec dispatch` instead (now also 1 step).
#
# THIS IS A NUDGE, NEVER A WALL. A permanent block on Haiku is unacceptable —
# there are legitimate reasons to want a real Claude subagent (most notably
# fine-grained tool control / a specialized system prompt via a named
# `subagent_type`, which the Copilot CLI has no equivalent for). Four
# INDEPENDENT escape hatches guarantee this script can never trap a session
# in a deny loop:
#
#   1. PER-SESSION DENY CAP (the actual guarantee). This hook denies AT MOST
#      ONCE per session, full stop -- NOT "once per fingerprint". The cap is
#      a `DENIED` flag persisted in the session's own cache file (see
#      below); the very first deny sets it, and every check thereafter
#      (regardless of subagent_type, model, or how much the prompt has
#      mutated) short-circuits to allow before the fingerprint is even
#      consulted. This is deliberately NOT "dedupe by fingerprint" as the
#      primary mechanism: orchestra's own retry convention (see
#      skills/run/SKILL.md's pipeline() and orchestra-delegate.md) appends
#      real new content every round --
#        "This is retry N of MAX ... apply this feedback exactly: " + JSON.stringify(feedback)
#      -- so a fingerprint over prompt text WOULD change every round and
#      deny again and again: a fingerprint-only design can burn an entire
#      multi-round retry budget on DENY/DENY/DENY without ever reaching a
#      real implementation (confirmed by adversarial review). The
#      per-session cap is what actually makes "at most one deny, ever, this
#      session" hold regardless of how the prompt text evolves round to
#      round. An exact-fingerprint check (SHA-256 of prompt+subagent_type+
#      model, see the python block below) is kept as a fast SECONDARY layer
#      on top of the cap -- useful for the common byte-identical-retry case
#      and for an audit trail of what got denied -- but it is not
#      load-bearing for the guarantee: the cap is checked independently and
#      is what actually holds even if the fingerprint layer were removed
#      entirely.
#
#   2. EXPLICIT ESCAPE MARKER. `[orchestra:allow-claude]` or
#      `[orchestra:allow-claude: <reason>]` anywhere in the prompt or
#      description allows immediately, with no deny ever recorded/spent.
#      This lets a deliberate choice through without spending the one shot.
#
#   3. AUTOMATIC CARVE-OUTS. Always allowed, no exec needed to decide:
#        - the prompt/description mentions `agent-exec` -- this is the relay
#          that RUNS the Copilot dispatch; denying it would deadlock the
#          entire mechanism.
#        - `subagent_type` contains "orchestra-review" (review is
#          Claude-only by design).
#        - THE REAL ANSWER to "I need fine-grained tool control": the Agent
#          tool's actual input schema (confirmed against the live tool
#          definition) has no `tools`/`allowed_tools`/etc. parameter at call
#          time -- its only fields are `description`, `isolation`, `model`,
#          `prompt`, `run_in_background`, `subagent_type`. Tool access is
#          fixed by the TARGET subagent's own frontmatter and selected via
#          `subagent_type`, never passed at call time. So the carve-out that
#          actually matches the user's stated case is: naming ANY specific,
#          non-generic `subagent_type` (Explore, Plan, statusline-setup,
#          claude-code-guide, a project-defined custom agent, ...) IS the
#          tool-control / specialized-system-prompt mechanism, and always
#          allows -- see the eligibility rule below. `orchestra-light` (bare
#          or namespaced `orchestra:orchestra-light`) is the ONE named
#          exception that stays deniable, since redirecting it to
#          `agent-exec dispatch` is the entire point of this hook.
#        - (VESTIGIAL, kept only as harmless defense-in-depth -- do NOT
#          treat this as the working answer to the tool-control case above,
#          see previous bullet) the tool input carrying a
#          `tools`/`allowed_tools`/`allowedTools`/`disallowed_tools`/
#          `disallowedTools` key. These do not appear to be real Agent-tool
#          parameters as of this writing; this check is left in only in
#          case some future/alternate caller passes such a field anyway.
#
#   4. KILL SWITCH + FAIL-OPEN. `ORCHESTRA_ENFORCEMENT=off` short-circuits
#      before any work at all. Beyond that: `agent-exec` missing, `config`/
#      `route` failing/timing out/returning garbage, no python3, empty or
#      unparseable stdin, or ANY unexpected condition all fail OPEN (allow).
#      This script must NEVER exit nonzero -- a failing PreToolUse hook
#      would disrupt every subagent spawn in every session -- so every
#      branch below ends in `exit 0`, and every subprocess call is wrapped
#      in a hard timeout so a hung `agent-exec` cannot stall the session.
#
# MATCHING LOGIC. Deny (the one time this session) only when ALL hold:
#   - `agent-exec config --json` resolves `enforcement.light_class == "block"`
#   - the tool call is the subagent-spawning tool (`tool_name` is "Agent" or,
#     defensively, "Task")
#   - the call is GENERIC (no `subagent_type`, or `subagent_type` in
#     {"general-purpose", "claude"}) AND the model is Haiku-class
#     (case-insensitive substring "haiku"), OR `subagent_type` is
#     orchestra's own light-class worker (`orchestra-light` /
#     `orchestra:orchestra-light`) regardless of model -- naming any OTHER
#     specific subagent_type is itself the escape (hatch 3 above)
#   - none of escape hatches 2-4 above fired
#   - this session has not already denied once (hatch 1's cap)
#   - `agent-exec route --class light --json` names a non-"claude" executor
#     -- i.e. there is genuinely something better to redirect to right now;
#     if route says claude, there is nothing to redirect to, so allow.
#
# OUTPUT CONVENTION. Deny: print a single-line PreToolUse JSON object to
# stdout, then exit 0:
#   {"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":
#    "deny","permissionDecisionReason":"..."}}
# Allow: exit 0 with NO stdout. Per Claude Code's documented PreToolUse
# contract (docs.claude.com hooks reference: exit code 0 without a
# `permissionDecision` falls through to the normal permission system, i.e.
# the call proceeds), printing nothing is the established "allow" signal --
# there is no need to ever emit an explicit `"permissionDecision":"allow"`.
#
# PERFORMANCE / CACHING. This hook fires on EVERY subagent spawn, so the
# common (allow) path must cost no subprocess at all once warm. Both
# `enforcement.light_class` and the `agent-exec route --class light --json`
# verdict, PLUS the per-session deny cap from hatch 1, live together in ONE
# per-session state file (same pattern/location as inject-router.sh); every
# later call in that session just reads that one file. This mirrors the
# "sticky exhaustion" model orchestra already uses elsewhere (once an
# executor is found unavailable for a run, it stays skipped for the rest of
# that run without being re-probed) -- here, once this hook has denied once,
# it stays inert for the rest of the session without re-deciding anything.
#
# Cheapest checks first: the kill switch (no I/O) runs before stdin is even
# read; the escape marker / carve-outs / eligibility checks (all pure string
# work already available from the one stdin parse) run before ever touching
# the session cache; and the session cache (including the deny cap) is
# checked before ever invoking `agent-exec` (the one and only place a real
# subprocess with real latency is spawned).

# --- escape hatch 4a: kill switch, cheapest possible check ---------------
if [ "${ORCHESTRA_ENFORCEMENT:-}" = "off" ]; then
    exit 0
fi

INPUT=$(cat 2>/dev/null || true)

if ! command -v python3 >/dev/null 2>&1; then
    exit 0
fi

# Single python3 pass: parse stdin JSON, and derive every string-only
# verdict (escape marker, carve-outs, eligibility, fingerprint) that does
# not require calling out to `agent-exec`. Emits nothing at all (leaving
# PARSED empty) on any parse failure, which fails everything below open via
# empty TOOL_NAME.
PARSED=$(printf '%s' "$INPUT" | python3 -c '
import sys, json, re, hashlib

def s(v):
    return v if isinstance(v, str) else ""

def d(v):
    return v if isinstance(v, dict) else {}

try:
    data = json.load(sys.stdin)

    session_id = re.sub(r"[^A-Za-z0-9_-]", "", s(data.get("session_id")))
    tool_name = s(data.get("tool_name"))
    tool_input = d(data.get("tool_input"))

    prompt = s(tool_input.get("prompt"))
    description = s(tool_input.get("description"))
    subagent_type = s(tool_input.get("subagent_type"))
    model = s(tool_input.get("model"))

    haystack_lc = (prompt + "\n" + description).lower()

    # Hatch 2: explicit escape marker. Optional ": reason" suffix, closing "]".
    escape_re = re.compile(r"\[orchestra:allow-claude(?::[^\]]*)?\]")
    escape_hit = "1" if escape_re.search(haystack_lc) else "0"

    # Hatch 3: the relay agent itself -- never deadlock the mechanism.
    agent_exec_hit = "1" if "agent-exec" in haystack_lc else "0"

    st_lc = subagent_type.strip().lower()

    # Hatch 3: review is Claude-only by design, kept as an explicit carve-out.
    orchestra_review_hit = "1" if "orchestra-review" in st_lc else "0"

    # Hatch 3 (VESTIGIAL defense-in-depth only -- see header comment; the
    # Agent tool'\''s real input schema does not appear to carry these keys,
    # so this is not relied on as the answer to the tool-control case; the
    # `eligible` computation below, via the named-subagent_type rule, is).
    restriction_keys = ("tools", "allowed_tools", "allowedTools", "disallowed_tools", "disallowedTools")
    restriction_hit = "1" if any(k in tool_input for k in restriction_keys) else "0"

    # Eligibility (the actual "is this even in scope" test): the call is
    # GENERIC (no subagent_type, or subagent_type is "general-purpose"/
    # "claude") AND the model is Haiku-class, OR subagent_type IS orchestra'\''s
    # own light-class worker (bare or namespaced) -- the sole named
    # exception that stays in scope, since redirecting IT is the whole
    # point. Any OTHER named subagent_type (Explore, Plan, statusline-setup,
    # claude-code-guide, a project-defined custom agent, ...) is out of
    # scope by construction: naming a specific agent IS the tool-control /
    # specialized-system-prompt mechanism, and Copilot cannot substitute
    # for it.
    is_generic = st_lc in ("", "general-purpose", "claude")
    is_orchestra_light = st_lc in ("orchestra-light", "orchestra:orchestra-light")
    model_lc = model.strip().lower()
    is_haiku = "haiku" in model_lc
    eligible = "1" if (is_orchestra_light or (is_generic and is_haiku)) else "0"

    # Secondary-layer dedup fingerprint (NOT the safety guarantee -- see
    # header comment hatch 1). SHA-256 over
    # "<prompt>\x1f<subagent_type>\x1f<model>" after whitespace-normalizing
    # each field (collapse runs of whitespace, strip ends), so a retry that
    # is byte-identical in substance but differs only in incidental
    # spacing/newlines still lands on the same fingerprint. The 0x1F unit
    # separator between fields is a byte that cannot occur in ordinary text,
    # so concatenation cannot let two different (prompt, subagent_type,
    # model) triples collapse onto the same joined string. Kept because it
    # is still useful (fast, auditable, catches the common
    # byte-identical-retry case cheaply) but the actual per-session bound
    # comes from the DENIED cap in the cache file (computed in bash below),
    # specifically BECAUSE real retries mutate the prompt text (append
    # "This is retry N of MAX ... apply this feedback exactly: <json>") and
    # so cannot be relied on to keep hashing to the same fingerprint.
    def norm(x):
        return re.sub(r"\s+", " ", x).strip()
    fp_src = "\x1f".join([norm(prompt), norm(subagent_type), norm(model)])
    fingerprint = hashlib.sha256(fp_src.encode("utf-8", "replace")).hexdigest()

    print(session_id)
    print(tool_name)
    print(escape_hit)
    print(agent_exec_hit)
    print(orchestra_review_hit)
    print(restriction_hit)
    print(eligible)
    print(fingerprint)
except Exception:
    pass
' 2>/dev/null) || PARSED=""

SESSION_ID=$(printf '%s' "$PARSED" | sed -n '1p')
TOOL_NAME=$(printf '%s' "$PARSED" | sed -n '2p')
ESCAPE_HIT=$(printf '%s' "$PARSED" | sed -n '3p')
AGENT_EXEC_HIT=$(printf '%s' "$PARSED" | sed -n '4p')
ORCHESTRA_REVIEW_HIT=$(printf '%s' "$PARSED" | sed -n '5p')
RESTRICTION_HIT=$(printf '%s' "$PARSED" | sed -n '6p')
ELIGIBLE=$(printf '%s' "$PARSED" | sed -n '7p')
FINGERPRINT=$(printf '%s' "$PARSED" | sed -n '8p')

# Fail-open: empty/unparseable stdin, or python3 producing nothing, leaves
# every field above empty, so TOOL_NAME will not match below -> allow.

case "$TOOL_NAME" in
    Agent|Task) ;;
    *) exit 0 ;;
esac

if [ -z "$SESSION_ID" ]; then
    # Can't scope a session cache or the deny cap sanely -> fail open.
    exit 0
fi

# --- escape hatch 2: explicit marker --------------------------------------
if [ "$ESCAPE_HIT" = "1" ]; then
    exit 0
fi

# --- escape hatch 3: automatic carve-outs ---------------------------------
if [ "$AGENT_EXEC_HIT" = "1" ] || [ "$ORCHESTRA_REVIEW_HIT" = "1" ] || [ "$RESTRICTION_HIT" = "1" ]; then
    exit 0
fi

# Not eligible at all (a named, non-generic, non-orchestra-light
# subagent_type, or a generic call on a non-Haiku model) -> nothing to
# enforce, no exec needed.
if [ "$ELIGIBLE" != "1" ]; then
    exit 0
fi

# --- session-scoped sticky state: enforcement.light_class + route verdict
#     + the hatch-1 per-session deny cap, all in one file ------------------
CACHE_FILE="${TMPDIR:-/tmp}/orchestra-enforce-config-${SESSION_ID}"
DENY_FILE="${TMPDIR:-/tmp}/orchestra-enforce-denied-${SESSION_ID}"

LIGHT_CLASS=""
ROUTE_EXECUTOR=""
ROUTE_MODEL=""
ROUTE_EFFORT=""
DENIED="0"

if [ -r "$CACHE_FILE" ]; then
    LIGHT_CLASS=$(grep -m1 '^LIGHT_CLASS=' "$CACHE_FILE" 2>/dev/null | cut -d= -f2-)
    ROUTE_EXECUTOR=$(grep -m1 '^ROUTE_EXECUTOR=' "$CACHE_FILE" 2>/dev/null | cut -d= -f2-)
    ROUTE_MODEL=$(grep -m1 '^ROUTE_MODEL=' "$CACHE_FILE" 2>/dev/null | cut -d= -f2-)
    ROUTE_EFFORT=$(grep -m1 '^ROUTE_EFFORT=' "$CACHE_FILE" 2>/dev/null | cut -d= -f2-)
    CACHED_DENIED=$(grep -m1 '^DENIED=' "$CACHE_FILE" 2>/dev/null | cut -d= -f2-)
    [ -n "$CACHED_DENIED" ] && DENIED="$CACHED_DENIED"
fi

write_cache() {
    # Rewrites the ENTIRE per-session state file from current bash vars.
    # Called both when the cache is first warmed (LIGHT_CLASS/ROUTE_* only
    # just resolved) and again the moment a deny fires (DENIED flips to 1) --
    # either way this is the single source of truth for this session.
    {
        printf 'LIGHT_CLASS=%s\n' "$LIGHT_CLASS"
        printf 'ROUTE_EXECUTOR=%s\n' "$ROUTE_EXECUTOR"
        printf 'ROUTE_MODEL=%s\n' "$ROUTE_MODEL"
        printf 'ROUTE_EFFORT=%s\n' "$ROUTE_EFFORT"
        printf 'DENIED=%s\n' "$DENIED"
    } > "$CACHE_FILE" 2>/dev/null || true
}

# --- hatch 1, part A: the cap is checked BEFORE ever touching agent-exec.
# Once denied once this session, this hook is permanently inert for the
# rest of the session -- no need to even re-resolve LIGHT_CLASS/route.
if [ "$DENIED" = "1" ]; then
    exit 0
fi

# run_with_timeout: invoke `agent-exec <args...>` with a hard wall-clock cap
# via python3's own subprocess timeout (NOT the GNU `timeout` binary, which
# is not reliably present on macOS/BSD systems without coreutils). Fails
# open (prints nothing) on timeout, nonzero exit, missing binary, or any
# other error -- callers must treat empty output as "unusable, fail open".
run_with_timeout() {
    secs="$1"
    shift
    python3 -c '
import subprocess, sys
secs = float(sys.argv[1])
cmd = sys.argv[2:]
try:
    r = subprocess.run(cmd, capture_output=True, timeout=secs, text=True)
    if r.returncode == 0:
        sys.stdout.write(r.stdout)
except Exception:
    pass
' "$secs" "$@" 2>/dev/null
}

if [ -z "$LIGHT_CLASS" ]; then
    # Cold cache: the ONLY place in this whole script allowed to spawn
    # `agent-exec`, and it runs at most once per session no matter how many
    # matching Agent-tool calls follow.
    LIGHT_CLASS="off"

    if command -v agent-exec >/dev/null 2>&1; then
        CONFIG_JSON=$(run_with_timeout 5 agent-exec config --json) || CONFIG_JSON=""
        if [ -n "$CONFIG_JSON" ]; then
            CFG_LIGHT_CLASS=$(printf '%s' "$CONFIG_JSON" | python3 -c '
import sys, json
try:
    data = json.load(sys.stdin)
    enforcement = data.get("enforcement")
    if isinstance(enforcement, dict):
        v = enforcement.get("light_class")
        if isinstance(v, str) and v in ("off", "block"):
            sys.stdout.write(v)
except Exception:
    pass
' 2>/dev/null) || CFG_LIGHT_CLASS=""
            if [ "$CFG_LIGHT_CLASS" = "block" ]; then
                LIGHT_CLASS="block"
            fi
        fi
    fi

    if [ "$LIGHT_CLASS" = "block" ]; then
        ROUTE_JSON=$(run_with_timeout 5 agent-exec route --class light --json) || ROUTE_JSON=""
        if [ -n "$ROUTE_JSON" ]; then
            ROUTE_PARSED=$(printf '%s' "$ROUTE_JSON" | python3 -c '
import sys, json

def s(v):
    return v if isinstance(v, str) else ""

try:
    data = json.load(sys.stdin)
    print(s(data.get("executor")))
    print(s(data.get("model")))
    print(s(data.get("effort")))
except Exception:
    pass
' 2>/dev/null) || ROUTE_PARSED=""
            ROUTE_EXECUTOR=$(printf '%s' "$ROUTE_PARSED" | sed -n '1p')
            ROUTE_MODEL=$(printf '%s' "$ROUTE_PARSED" | sed -n '2p')
            ROUTE_EFFORT=$(printf '%s' "$ROUTE_PARSED" | sed -n '3p')
        fi
        if [ -z "$ROUTE_EXECUTOR" ] || [ "$ROUTE_EXECUTOR" = "claude" ]; then
            # Nothing better to redirect to right now -> nothing to enforce
            # for the rest of this session either (sticky, same as an
            # unavailable executor staying skipped for a whole run).
            LIGHT_CLASS="off"
        fi
    fi

    write_cache
fi

if [ "$LIGHT_CLASS" != "block" ]; then
    exit 0
fi

# --- hatch 1, part B: secondary fingerprint dedup (fast path only; NOT the
# guarantee -- see header comment). Useful for the common byte-identical
# retry case, but a real retry that mutates the prompt (feedback appended)
# will produce a different fingerprint and fall through to the cap check.
if [ -n "$FINGERPRINT" ] && [ -r "$DENY_FILE" ] && grep -qxF "$FINGERPRINT" "$DENY_FILE" 2>/dev/null; then
    exit 0
fi

# --- hatch 1, part C: the actual guarantee. DENIED was already confirmed
# "0" in part A (otherwise we would have exited there already), so reaching
# here means this is the first and only deny this session gets.
DENIED="1"
write_cache
if [ -n "$FINGERPRINT" ]; then
    printf '%s\n' "$FINGERPRINT" >> "$DENY_FILE" 2>/dev/null || true
fi

REASON="orchestra: enforcement.light_class=block prefers ${ROUTE_EXECUTOR:-copilot} (model ${ROUTE_MODEL:-gpt-5.6-luna}, effort ${ROUTE_EFFORT:-medium}) over a direct Claude Haiku subagent for light-class work. Use: agent-exec dispatch --class light --prompt-file <prompt-file> --workdir <workdir> --capture -- one call, replaces the manual multi-step relay. This nudge fires at most ONCE per session: every further orchestra-light/Haiku call this session -- this same task, a retry with feedback, or an unrelated task -- will be allowed automatically from here on. To bypass permanently for a legitimate reason (e.g. you need a named subagent for fine-grained tool control or a specialized system prompt, which Copilot has no equivalent for), add [orchestra:allow-claude: <reason>] to the prompt."

python3 - "$REASON" <<'PYEOF' 2>/dev/null
import sys, json
reason = sys.argv[1] if len(sys.argv) > 1 else "orchestra: prefer agent-exec dispatch --class light over a direct Claude Haiku subagent."
sys.stdout.write(json.dumps({
    "hookSpecificOutput": {
        "hookEventName": "PreToolUse",
        "permissionDecision": "deny",
        "permissionDecisionReason": reason,
    }
}))
PYEOF

exit 0
