# /// script
# requires-python = ">=3.9"
# ///
"""Plan loading + scheduler for `agent-exec wave` (pure, no I/O beyond reading
the plan/spec/preamble files themselves; no git, no subprocesses).

Kept separate from agent_exec.py (and must not import it) so the scheduling
logic can be unit-tested without any of agent_exec's process/dispatch
machinery. See shared-schema.md (frozen, written by the planning agents) for
the plan.json and state.json shapes this module works with.
"""

import json
import os
import re

_VALID_CLS = ("light", "standard", "deep")

# The full package-status enum (shared-schema.md). Order here is only used
# by `summary` to guarantee every status key is present, even at zero.
_STATUSES = (
    "pending",
    "implementing",
    "verifying",
    "fixing",
    "ready",
    "integrated",
    "needs",
    "failed",
    "blocked",
)

# Statuses that count as "in flight" per shared-schema.md.
_IN_FLIGHT = frozenset(("implementing", "verifying", "fixing", "ready"))

# Statuses that make a dependency count as done for scheduling purposes.
_DEP_SATISFIED = frozenset(("integrated",))

# Statuses that propagate "blocked" to dependents.
_BLOCKING = frozenset(("failed", "needs", "blocked"))


class PlanError(ValueError):
    """Raised for any structurally- or semantically-invalid plan.json."""


def _resolve(path, base_dir):
    if not os.path.isabs(path):
        path = os.path.join(base_dir, path)
    return path


def load_plan(path):
    """Read plan.json, fill defaults, validate, return the normalized dict.

    `packages` stays a list, in file order. Relative `spec`/`preamble` paths
    are resolved against the plan file's directory and rewritten to absolute
    paths in the returned dict.
    """
    base_dir = os.path.dirname(os.path.abspath(path))

    try:
        with open(path) as f:
            raw = json.load(f)
    except (OSError, json.JSONDecodeError) as exc:
        raise PlanError("invalid JSON in plan %s: %s" % (path, exc)) from exc

    if not isinstance(raw, dict) or "packages" not in raw:
        raise PlanError("plan %s: missing required key 'packages'" % path)

    rules = raw.get("rules", [])
    if not isinstance(rules, list):
        raise PlanError("plan rules must be a list")
    for rule in rules:
        if (not isinstance(rule, dict) or not isinstance(rule.get("if"), str)
                or not isinstance(rule.get("require"), list)
                or not all(isinstance(item, str) for item in rule["require"])):
            raise PlanError("plan rules entries must be {'if': <glob>, 'require': [<glob>...]}")

    plan = {
        "preamble": [_resolve(p, base_dir) for p in raw.get("preamble", [])],
        "external_done": list(raw.get("external_done", [])),
        "rules": [{"if": rule["if"], "require": list(rule["require"])} for rule in rules],
        "packages": [],
    }

    for p in plan["preamble"]:
        if not os.path.exists(p):
            raise PlanError("preamble path does not exist: %s" % p)

    seen_ids = set()
    external_done = set(plan["external_done"])
    normalized = []

    for entry in raw["packages"]:
        pkg_id = entry.get("id")
        if not pkg_id:
            raise PlanError("package missing required field 'id': %r" % (entry,))
        if pkg_id in seen_ids:
            raise PlanError("duplicate package id: %s" % pkg_id)
        seen_ids.add(pkg_id)

        if "spec" not in entry or not entry["spec"]:
            raise PlanError("%s: missing required field 'spec'" % pkg_id)
        if "cls" not in entry or not entry["cls"]:
            raise PlanError("%s: missing required field 'cls'" % pkg_id)
        if entry["cls"] not in _VALID_CLS:
            raise PlanError(
                "%s: invalid cls %r (must be one of %s)" % (pkg_id, entry["cls"], ", ".join(_VALID_CLS))
            )

        spec_path = _resolve(entry["spec"], base_dir)
        if not os.path.exists(spec_path):
            raise PlanError("%s: spec path does not exist: %s" % (pkg_id, spec_path))

        normalized.append(
            {
                "id": pkg_id,
                "spec": spec_path,
                "cls": entry["cls"],
                "depends_on": list(entry.get("depends_on", [])),
                "files_owned": list(entry.get("files_owned", [])),
                "prio": entry.get("prio", 0),
            }
        )

    all_ids = seen_ids
    for pkg in normalized:
        for dep in pkg["depends_on"]:
            if dep not in all_ids and dep not in external_done:
                raise PlanError("%s: depends_on unknown id %r" % (pkg["id"], dep))

    _check_cycles(normalized)

    plan["packages"] = normalized
    return plan


def _check_cycles(packages):
    deps = {p["id"]: p["depends_on"] for p in packages}
    WHITE, GRAY, BLACK = 0, 1, 2
    color = {pkg_id: WHITE for pkg_id in deps}
    stack = []

    def visit(pkg_id):
        if color.get(pkg_id, BLACK) == BLACK:
            return
        if color[pkg_id] == GRAY:
            cycle_start = stack.index(pkg_id)
            cycle = stack[cycle_start:] + [pkg_id]
            raise PlanError("dependency cycle: %s" % " -> ".join(cycle))
        color[pkg_id] = GRAY
        stack.append(pkg_id)
        for dep in deps.get(pkg_id, ()):
            if dep in deps:  # external_done deps can't cycle
                visit(dep)
        stack.pop()
        color[pkg_id] = BLACK

    for pkg_id in deps:
        if color[pkg_id] == WHITE:
            visit(pkg_id)


def stem(pattern):
    """The literal path prefix before the first glob wildcard segment."""
    pattern = pattern[2:] if pattern.startswith("./") else pattern
    pattern = pattern[:-1] if pattern.endswith("/") else pattern
    parts = pattern.split("/")
    kept = []
    for part in parts:
        if any(ch in part for ch in "*?["):
            break
        kept.append(part)
    return "/".join(kept)


def _stem_overlap(a, b):
    if a == "" or b == "":
        return True
    if a == b:
        return True
    if len(a) < len(b):
        shorter, longer = a, b
    else:
        shorter, longer = b, a
    return longer == shorter or longer.startswith(shorter + "/")


def overlaps(files_a, files_b):
    """True if any pattern in files_a overlaps any pattern in files_b."""
    stems_a = [stem(p) for p in files_a]
    stems_b = [stem(p) for p in files_b]
    return any(_stem_overlap(a, b) for a in stems_a for b in stems_b)


_DELETION_WORDS = re.compile(r"削除|撤去|取り除|外す|remove|delete|drop|deprecat", re.IGNORECASE)


def lint_plan(plan):
    """Return non-blocking ownership warnings for a normalized plan."""
    warnings = []
    for pkg in plan.get("packages", []):
        owned = pkg.get("files_owned", [])
        try:
            with open(pkg.get("spec", "")) as fh:
                spec_text = fh.read()
        except (OSError, TypeError, ValueError):
            spec_text = None
        if (spec_text is not None and _DELETION_WORDS.search(spec_text)
                and not any(any(ch in path for ch in "*?[") for path in owned)):
            warnings.append({
                "pkg": pkg.get("id"),
                "message": "deletes code but owns only literal paths; leftover references "
                           "(tests, other features) will be out of reach — consider a glob",
            })
        for rule in plan.get("rules", []):
            if not overlaps(owned, [rule["if"]]):
                continue
            missing = [required for required in rule["require"]
                       if not overlaps(owned, [required])]
            if missing:
                warnings.append({
                    "pkg": pkg.get("id"),
                    "message": "files_owned matching %s are missing required ownership: %s"
                               % (rule["if"], ", ".join(missing)),
                })
    return warnings


def _status_of(pkg_id, pkg_states):
    return pkg_states.get(pkg_id, {}).get("status", "pending")


def blocked(plan, pkg_states):
    """Ids whose dependency (transitively) has status failed/needs/blocked."""
    deps = {p["id"]: p["depends_on"] for p in plan["packages"]}
    result = set()

    def is_blocked(pkg_id, seen):
        if pkg_id in result:
            return True
        if pkg_id in seen:
            return False  # cycle guard; load_plan already rejects real cycles
        seen = seen | {pkg_id}
        for dep in deps.get(pkg_id, ()):
            if dep not in deps:
                continue  # external_done dependency: never blocking
            if _status_of(dep, pkg_states) in _BLOCKING:
                return True
            if is_blocked(dep, seen):
                return True
        return False

    for pkg_id in deps:
        if is_blocked(pkg_id, set()):
            result.add(pkg_id)
    return result


def select(plan, pkg_states, max_in_flight, serialize_overlap=True):
    """Ids ready to start now, most-eligible first, capped by free capacity."""
    blocked_ids = blocked(plan, pkg_states)
    in_flight_count = sum(1 for p in plan["packages"] if _status_of(p["id"], pkg_states) in _IN_FLIGHT)
    slots = max(max_in_flight - in_flight_count, 0)
    if slots == 0:
        return []

    in_flight_files = []
    for p in plan["packages"]:
        if _status_of(p["id"], pkg_states) in _IN_FLIGHT:
            in_flight_files.append(p["files_owned"])

    candidates = []
    for order, p in enumerate(plan["packages"]):
        if p["id"] in blocked_ids:
            continue
        if _status_of(p["id"], pkg_states) != "pending":
            continue
        deps_ok = all(
            _status_of(dep, pkg_states) in _DEP_SATISFIED or dep not in {q["id"] for q in plan["packages"]}
            for dep in p["depends_on"]
        )
        if not deps_ok:
            continue
        candidates.append((p, order))

    candidates.sort(key=lambda item: (-item[0]["prio"], item[1]))

    selected = []
    selected_files = []
    for p, _order in candidates:
        if len(selected) >= slots:
            break
        if serialize_overlap:
            if any(overlaps(p["files_owned"], f) for f in in_flight_files):
                continue
            if any(overlaps(p["files_owned"], f) for f in selected_files):
                continue
        selected.append(p["id"])
        selected_files.append(p["files_owned"])

    return selected


def summary(plan, pkg_states):
    """Counts per status (all enum values present) plus total and runnable."""
    counts = {status: 0 for status in _STATUSES}
    blocked_ids = blocked(plan, pkg_states)
    for p in plan["packages"]:
        status = _status_of(p["id"], pkg_states)
        if p["id"] in blocked_ids and status not in ("failed", "needs"):
            status = "blocked"
        counts[status] = counts.get(status, 0) + 1
    counts["total"] = len(plan["packages"])
    counts["runnable"] = len(select(plan, pkg_states, max_in_flight=10**9))
    return counts


def topo_order(plan):
    """A deterministic topological order; ties broken by plan order."""
    deps = {p["id"]: p["depends_on"] for p in plan["packages"]}
    plan_ids = set(deps)
    order_index = {p["id"]: i for i, p in enumerate(plan["packages"])}
    result = []
    visited = set()

    def visit(pkg_id):
        if pkg_id in visited:
            return
        visited.add(pkg_id)
        for dep in sorted(deps.get(pkg_id, ()), key=lambda d: order_index.get(d, -1)):
            if dep in plan_ids:
                visit(dep)
        result.append(pkg_id)

    for p in sorted(plan["packages"], key=lambda p: order_index[p["id"]]):
        visit(p["id"])

    return result
