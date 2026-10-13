# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""`agent-exec approvals`: the sandbox approvals of a wave, decided by the
instructor (or the user) without an LLM inside the runner.

Three kinds of pending item, each with a stable id:

- `net:<host>`  a host the network allowlist blocked for some package (or
  that a worker named in `ESCALATE: network <host>`), not quiet, not allowed
  already and not decided before in this wave;
- `path:<pkg>:<n>`  denial n of a package's `permission` need;
- `dialog:<pkg>`  an interactive session stopped on a permission dialog.

`approvals decide` applies one decision -- a domain or path grant, a key
sent to the dialog, or a note telling the worker to finish without it --
moves the affected package back to the runner, and appends the decision to
`<orchestra state dir>/audit/decisions.jsonl`. The hard refusals of
agent_exec_sandbox (`domain_refusal`, `user_grant_refusal`) apply to every
decider. The runner itself never decides.
"""

import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec_sandbox  # noqa: E402
import agent_exec_wave  # noqa: E402
import agent_exec_wave_plan  # noqa: E402

APPROVALS_NAME = "approvals.json"
DECISIONS_AUDIT = "decisions.jsonl"
BYS = ("instructor", "user")
DECISIONS = ("allow", "deny")
DEFAULT_PORT = 443

# First line of a worker answer that asks for network hosts:
# `ESCALATE: network <host>[, <host>...]` (a host answer may come back with
# the ESCALATE prefix doubled by an interactive host).
_NET_ESCALATE_RE = re.compile(r"^(?:ESCALATE:\s*)+network\s+(\S.*)$")
_HOST_RE = re.compile(r"^[a-z0-9*]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+"
                      r"(:[0-9]{1,5})?$")
_TERMINAL_RE = re.compile(r"\nterminal: ([^\n]+)")


# --- helpers shared with the runner ------------------------------------------


def note_path(state_dir, pid):
    """Decision notes for a package's next dispatch (the runner sends this
    file with it, then sets it aside)."""
    return os.path.join(state_dir, "corrections", pid + ".approvals.md")


def network_escalate_hosts(answer):
    """[{host, port}] from an `ESCALATE: network <host>[, ...]` first line,
    or None when the answer is not one (a plain ESCALATE stays an escalate)."""
    first = ""
    for line in (answer or "").splitlines():
        if line.strip():
            first = line.strip()
            break
    match = _NET_ESCALATE_RE.match(first)
    if not match:
        return None
    out = []
    for token in re.split(r"[,\s]+", match.group(1)):
        text = token.strip("`'\"()[]<>;.").lower()
        if text.startswith("https://") or text.startswith("http://"):
            text = text.split("://", 1)[1].split("/", 1)[0]
        if not text or not _HOST_RE.match(text):
            continue
        host, port = text, DEFAULT_PORT
        if ":" in text:
            host, port = text.rsplit(":", 1)
            port = int(port)
        entry = {"host": host, "port": port}
        if entry not in out:
            out.append(entry)
    return out or None


# --- config / stores ---------------------------------------------------------


def _sandbox_cfg():
    try:
        import agent_exec
        resolved, _ = agent_exec.resolve_config()
        return (resolved or agent_exec.DEFAULTS).get("sandbox")
    except Exception:
        return None


def _network_cfg():
    return agent_exec_sandbox.normalize_config(_sandbox_cfg())["network"]


def _deny_read():
    cfg = agent_exec_sandbox.normalize_config(_sandbox_cfg())
    home = os.path.expanduser("~")
    return agent_exec_sandbox._dedupe(
        [agent_exec_sandbox._expand(p, home) for p in agent_exec_sandbox.DEFAULT_DENY_READ]
        + [agent_exec_sandbox._expand(p, home) for p in cfg.get("deny_read") or []])


def _approvals_path(state_path):
    return os.path.join(os.path.dirname(os.path.abspath(state_path)), APPROVALS_NAME)


def _load_approvals(state_path):
    try:
        with open(_approvals_path(state_path), encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, ValueError):
        data = {}
    if not isinstance(data, dict):
        data = {}
    for key in ("allowed", "declined"):
        if not isinstance(data.get(key), dict):
            data[key] = {}
    return data


def _save_approvals(state_path, data):
    path = _approvals_path(state_path)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(data, fh, ensure_ascii=False, indent=2, sort_keys=True)
        fh.write("\n")
    os.replace(tmp, path)


def decisions_audit_path():
    return os.path.join(agent_exec_sandbox.state_dir(), agent_exec_sandbox.AUDIT_DIR,
                        DECISIONS_AUDIT)


def _audit(record):
    path = decisions_audit_path()
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    with open(path, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(record, ensure_ascii=False, sort_keys=True) + "\n")


def _specs(state, state_path):
    """{package id: spec path} from the plan and the runner's fix-ups."""
    specs = {}
    try:
        plan = agent_exec_wave_plan.load_plan(state.get("plan") or "")
        specs.update((p["id"], p.get("spec")) for p in plan["packages"])
    except Exception:
        pass
    try:
        with open(os.path.join(os.path.dirname(os.path.abspath(state_path)),
                               "fixups.json"), encoding="utf-8") as fh:
            fixups = json.load(fh)
        for pkg in (fixups or {}).get("packages") or []:
            if isinstance(pkg, dict) and pkg.get("id") not in specs:
                specs[pkg.get("id")] = pkg.get("spec")
    except (OSError, ValueError, AttributeError):
        pass
    return specs


def _need_json(need):
    try:
        data = json.loads(need.get("detail") or "")
    except (TypeError, ValueError):
        return None
    return data if isinstance(data, dict) else None


# --- listing -----------------------------------------------------------------


def _host_state(net, approvals):
    """host -> "quiet" | "allowed" | "decided" | None (pending)."""
    allowed = list(net["allowed_domains"]) + list(agent_exec_sandbox.learned_domains())

    def state_of(host):
        if agent_exec_sandbox.domain_matches(host, net["quiet_domains"]):
            return "quiet"
        if host in approvals["allowed"] or host in approvals["declined"]:
            return "decided"
        if agent_exec_sandbox.domain_matches(host, allowed):
            return "allowed"
        return None

    return state_of


def _first_seen(state_path):
    seen = {}
    for event in agent_exec_wave.read_events(state_path, None):
        if event.get("event") != "network-denied":
            continue
        try:
            denials = json.loads(event.get("detail") or "{}").get("denials") or []
        except (ValueError, AttributeError):
            continue
        for d in denials:
            host = (d or {}).get("host") if isinstance(d, dict) else None
            if host and host not in seen:
                seen[host] = event.get("at")
    return seen


def _dialog(need):
    """(dialog text, terminal) of a dialog-stalled need, or None."""
    detail = None
    if need.get("kind") == "stalled":
        detail = need.get("detail") or ""
    elif need.get("kind") == "delegate":
        data = _need_json(need)
        if data and data.get("orca") == "stalled":
            detail = str(data.get("detail") or "")
    if detail is None or not detail.startswith("dialog:"):
        return None
    match = _TERMINAL_RE.search(detail)
    text = detail[len("dialog:"):]
    if match:
        text = text[:match.start() - len("dialog:")]
    return text.strip("\n"), (match.group(1).strip() if match else None)


def _dialog_host(state_dir, pid, entry):
    executor = (entry or {}).get("executor")
    if executor in ("herdr", "orca"):
        return executor
    try:
        import agent_exec_herdr
        with open(os.path.join(state_dir, agent_exec_herdr.REGISTRY_NAME),
                  encoding="utf-8") as fh:
            if pid in (json.load(fh) or {}):
                return "herdr"
    except (OSError, ValueError, TypeError):
        pass
    return "orca"


def pending_items(state_path, net=None):
    """Every pending approval of the wave at `state_path`, in a stable order."""
    state = agent_exec_wave.StateStore(state_path).load()
    state_dir = os.path.dirname(os.path.abspath(state_path))
    packages = state.get("packages") or {}
    specs = _specs(state, state_path)
    net = net if net is not None else _network_cfg()
    approvals = _load_approvals(state_path)
    state_of = _host_state(net, approvals)
    items = []

    hosts = {}
    for pid in sorted(packages):
        for d in packages[pid].get("network_denials") or []:
            host = str((d or {}).get("host") or "").strip().lower().rstrip(".")
            if not host or state_of(host) is not None:
                continue
            entry = hosts.setdefault(host, {"ports": [], "packages": []})
            if d.get("port") not in entry["ports"]:
                entry["ports"].append(d.get("port"))
            if pid not in [p["id"] for p in entry["packages"]]:
                entry["packages"].append({"id": pid, "status": packages[pid].get("status"),
                                          "spec": specs.get(pid)})
    seen = _first_seen(state_path) if hosts else {}
    for host in sorted(hosts):
        entry = hosts[host]
        items.append({
            "id": "net:" + host, "kind": "net", "host": host,
            "ports": sorted(entry["ports"], key=lambda p: (str(type(p)), p)),
            "packages": entry["packages"], "first_seen": seen.get(host),
            "spec": entry["packages"][0]["spec"],
        })

    for need in state.get("needs") or []:
        pid = need.get("id")
        if pid is None:
            continue
        if need.get("kind") == "permission":
            data = _need_json(need) or {}
            for n, denial in enumerate(data.get("denials") or []):
                if not isinstance(denial, dict):
                    continue
                items.append({
                    "id": "path:%s:%d" % (pid, n), "kind": "path", "pkg": pid,
                    "path": denial.get("path"), "grant_candidate": denial.get("grant_candidate"),
                    "why": denial.get("why"), "spec": specs.get(pid),
                })
            continue
        dialog = _dialog(need)
        if dialog is not None:
            text, terminal = dialog
            items.append({
                "id": "dialog:" + pid, "kind": "dialog", "pkg": pid, "dialog": text,
                "terminal": terminal,
                "host": _dialog_host(state_dir, pid, packages.get(pid)),
                "spec": specs.get(pid),
            })
    return items


# --- deciding ----------------------------------------------------------------


def _append_note(state_dir, pid, line):
    path = note_path(state_dir, pid)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    fresh = not os.path.exists(path)
    with open(path, "a", encoding="utf-8") as fh:
        if fresh:
            fh.write("# Approval decisions for %s\n\n" % pid)
        fh.write("- %s\n" % line)


def _resume(store, pid, detail):
    store.clear_need(pid)
    store.set_status(pid, "pending", detail=detail)


def _network_need_hosts(state, pid):
    need = next((n for n in state.get("needs") or []
                 if n.get("id") == pid and n.get("kind") == "network"), None)
    if need is None:
        return None
    data = _need_json(need) or {}
    return [str(h).strip().lower() for h in data.get("hosts") or []]


def _decide_net(state_path, item, decision, reason, by):
    host = item["host"]
    state_dir = os.path.dirname(os.path.abspath(state_path))
    if decision == "allow":
        try:
            agent_exec_sandbox.add_domain(host, source=by, reason=reason)
        except (OSError, ValueError) as exc:
            return 1, "refused: %s" % exc, []
    approvals = _load_approvals(state_path)
    approvals["allowed" if decision == "allow" else "declined"][host] = {
        "by": by, "reason": reason}
    _save_approvals(state_path, approvals)

    store = agent_exec_wave.StateStore(state_path)
    state = store.load()
    state_of = _host_state(_network_cfg(), approvals)
    resumed = []
    for pkg in item["packages"]:
        pid = pkg["id"]
        need_hosts = _network_need_hosts(state, pid)
        if need_hosts is None or host not in need_hosts:
            continue  # a live session already got the domain pushed
        if decision == "allow":
            _append_note(state_dir, pid, "network access to %s is now allowed (%s). "
                                         "Continue the task." % (host, reason))
        else:
            _append_note(state_dir, pid, "network access to %s was declined: %s. "
                                         "Finish without it or ESCALATE." % (host, reason))
        if all(state_of(h) is not None for h in need_hosts):
            _resume(store, pid, "network %s %s" % (
                host, "allowed" if decision == "allow" else "declined"))
            resumed.append(pid)
    return 0, None, resumed


def _decide_path(state_path, item, decision, reason, by):
    pid = item["pkg"]
    state_dir = os.path.dirname(os.path.abspath(state_path))
    target = item.get("grant_candidate") or item.get("path")
    if decision == "allow":
        if not target:
            return 1, "refused: the denial names no path", []
        real = os.path.realpath(os.path.expanduser(target))
        refusal = agent_exec_sandbox.user_grant_refusal(real, _deny_read())
        if refusal:
            return 1, "refused: %s" % refusal, []
        try:
            agent_exec_sandbox.add_grant(real, by, reason=reason)
        except (OSError, ValueError) as exc:
            return 1, "refused: %s" % exc, []
        _append_note(state_dir, pid, "%s is now granted (%s). Continue the task."
                     % (real, reason))
    else:
        _append_note(state_dir, pid, "%s is off-limits: %s" % (item.get("path"), reason))
    _resume(agent_exec_wave.StateStore(state_path), pid, "permission %s %s" % (
        target, "granted" if decision == "allow" else "declined"))
    return 0, None, [pid]


def _decide_dialog(state_path, item, decision, herdr_factory):
    pid = item["pkg"]
    how = ("approve it in %s (terminal %s), then run: agent-exec wave mark --state %s "
           "--await %s" % (item["host"].capitalize(), item.get("terminal") or "unknown",
                           state_path, pid))
    if item["host"] != "herdr":
        return 2, "no key injection into %s here; %s" % (item["host"].capitalize(), how), []
    import agent_exec_herdr
    if herdr_factory is None:
        herdr_factory = agent_exec_herdr.HerdrExecutor.from_config
    host = herdr_factory()
    state_dir = os.path.dirname(os.path.abspath(state_path))
    host.state_dir = state_dir
    try:
        session = host.session(pid, state_dir)
    except Exception:
        session = None
    terminal = (session or {}).get("terminal")
    if not terminal:
        return 2, "the herdr terminal of %s is gone; %s" % (pid, how), []
    key = "enter" if decision == "allow" else "escape"
    try:
        host._call(["agent", "send-keys", terminal, key],
                   sname=(session or {}).get("herdr_session"))
    except agent_exec_herdr._HerdrError as exc:
        return 2, "could not send keys to %s (%s); %s" % (terminal, exc, how), []
    import agent_exec_wave_run
    code, message = agent_exec_wave_run.await_package(state_path, pid)
    if code != 0:
        return code, "sent %s to %s but %s" % (key, terminal, message), []
    return 0, None, [pid]


def decide(state_path, item_id, decision, reason, by="instructor", herdr_factory=None):
    """Apply one decision. Returns (exit code, result dict)."""
    item = next((i for i in pending_items(state_path) if i["id"] == item_id), None)
    if item is None:
        return 2, {"item": item_id, "applied": False,
                   "error": "no pending approval %s" % item_id}
    if item["kind"] == "net":
        code, error, resumed = _decide_net(state_path, item, decision, reason, by)
        subject = item["host"]
        packages = [p["id"] for p in item["packages"]]
    elif item["kind"] == "path":
        code, error, resumed = _decide_path(state_path, item, decision, reason, by)
        subject = item.get("grant_candidate") or item.get("path")
        packages = [item["pkg"]]
    else:
        code, error, resumed = _decide_dialog(state_path, item, decision, herdr_factory)
        subject = item.get("dialog")
        packages = [item["pkg"]]
    applied = code == 0
    record = {
        "ts": agent_exec_sandbox._now_iso(), "state": os.path.abspath(state_path),
        "item": item_id, "kind": item["kind"], "subject": subject, "decision": decision,
        "by": by, "reason": reason, "packages": packages, "applied": applied,
    }
    _audit(record)
    out = dict(record, resumed=resumed)
    if error:
        out["error"] = error
    return code, out


# --- CLI ---------------------------------------------------------------------


_USAGE = (
    "usage: agent-exec approvals --state STATE [--json]\n"
    "       agent-exec approvals decide --state STATE ITEM allow|deny --reason TEXT\n"
    "                                   [--by instructor|user] [--json]\n"
)


def _line(item):
    if item["kind"] == "net":
        return "%s  ports=%s  packages=%s" % (
            item["id"], ",".join(str(p) for p in item["ports"]),
            ",".join("%s(%s)" % (p["id"], p["status"]) for p in item["packages"]))
    if item["kind"] == "path":
        return "%s  %s  grant=%s  why=%s" % (
            item["id"], item.get("path"), item.get("grant_candidate"), item.get("why"))
    tail = [ln for ln in (item.get("dialog") or "").splitlines() if ln.strip()]
    return "%s  %s terminal=%s  %s" % (item["id"], item["host"], item.get("terminal"),
                                       " / ".join(tail[-3:]))


def cmd_approvals(args):
    deciding = bool(args) and args[0] == "decide"
    rest = list(args[1:] if deciding else args)
    if rest[:1] in (["-h"], ["--help"]):
        sys.stdout.write(_USAGE)
        return 0
    values, positional, as_json = {}, [], False
    i = 0
    while i < len(rest):
        tok = rest[i]
        if tok == "--json":
            as_json = True
            i += 1
        elif tok in ("--state", "--reason", "--by"):
            if i + 1 >= len(rest):
                sys.stderr.write("agent-exec: approvals: missing value for %s\n" % tok)
                return 2
            values[tok[2:]] = rest[i + 1]
            i += 2
        elif tok.startswith("--"):
            sys.stderr.write("agent-exec: approvals: unknown option: %s\n" % tok)
            return 2
        else:
            positional.append(tok)
            i += 1
    state_path = values.get("state")
    if not state_path:
        sys.stderr.write(_USAGE)
        return 2
    if not os.path.isfile(state_path):
        sys.stderr.write("agent-exec: approvals: no state file at %s\n" % state_path)
        return 3
    if not deciding:
        if positional or "reason" in values or "by" in values:
            sys.stderr.write(_USAGE)
            return 2
        items = pending_items(state_path)
        if as_json:
            print(json.dumps(items, ensure_ascii=False))
            return 0
        for item in items:
            print(_line(item))
        if items:
            print("decide: agent-exec approvals decide --state %s ITEM allow|deny "
                  "--reason TEXT" % state_path)
        else:
            print("no pending approvals")
        return 0
    by = values.get("by", "instructor")
    if (len(positional) != 2 or positional[1] not in DECISIONS or by not in BYS
            or not (values.get("reason") or "").strip()):
        sys.stderr.write(_USAGE)
        return 2
    code, out = decide(state_path, positional[0], positional[1], values["reason"].strip(), by)
    if as_json:
        print(json.dumps(out, ensure_ascii=False))
    elif code == 0:
        print("%s %s: %s%s" % (positional[1], positional[0], out["subject"] if out.get(
            "kind") != "dialog" else "dialog", ("  resumed: " + ", ".join(out["resumed"]))
            if out.get("resumed") else ""))
    if code != 0:
        sys.stderr.write("agent-exec: approvals decide: %s\n" % out.get("error"))
    return code
