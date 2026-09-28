"""Deterministic check helpers for agent-exec (no LLM involved).

Kept out of agent_exec.py so the integration/verification machinery can grow
without adding to that file. Standard library only. This module must NOT
import agent_exec: `agent-exec check` resolves the tree, the changed-file
list, and (for `--baseline`) a temporary worktree using agent_exec's own git
helpers, then calls the pure functions below with plain paths and strings.
"""

import fcntl
import os
import re
import shlex
import subprocess
import time
import xml.etree.ElementTree as ET

_ANSI_RE = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)")

# A line that says what failed. Deliberately narrow: `expected` alone matches
# half of any DOM dump, so only the capitalised assertion-diff forms count.
_ANCHOR_RE = re.compile(
    r"\bFAIL(?:ED)?\b|[×✖✗]|\berror TS\d+|\berror\[|\bError:|\bAssertionError\b"
    r"|panicked at|\bnot ok\b|^Traceback|^\s*Expected\b|^\s*Received\b|Timed out|timed out"
)

# Rendered markup (testing-library DOM dumps, JSX snapshots): tags and
# attribute lines. Runs of these carry no failure information.
_MARKUP_RE = re.compile(r"^\s*(?:</?[A-Za-z][\w:-]*(?:\s|/?>|$)|/?>$|[\w:-]+=\"[^\"]*\"\s*/?>?$)")

_MARKUP_RUN = 4
_CONTEXT_AFTER = 3
_TAIL_LINES = 30


def _collapse_markup(lines):
    out = []
    run = []
    for line in lines:
        if _MARKUP_RE.match(line):
            run.append(line)
            continue
        out.extend(run if len(run) < _MARKUP_RUN else ["  … (%d markup lines omitted)" % len(run)])
        run = []
        out.append(line)
    out.extend(run if len(run) < _MARKUP_RUN else ["  … (%d markup lines omitted)" % len(run)])
    return out


def _clip(text, limit):
    if len(text) <= limit:
        return text
    marker = "\n… (%d chars omitted) …\n"
    keep = max(limit - len(marker % len(text)), 0)
    head = keep * 2 // 3
    tail = keep - head
    return text[:head] + marker % (len(text) - keep) + (text[-tail:] if tail else "")


def failure_excerpt(text, limit=4000):
    """The part of a failed command's output worth handing to a fixer.

    Failure anchors (test names, assertion diffs, compiler errors) come first,
    each with a few lines of context, then the tail of the output. The head is
    never sacrificed for the tail: the failing test's name usually sits near
    the top and is exactly what a tail-only cut loses. Markup dumps collapse
    to one line. The result is at most `limit` characters.
    """
    if not text:
        return ""
    lines = _collapse_markup(_ANSI_RE.sub("", text).splitlines())
    picked = []
    seen = set()
    budget = limit * 2 // 3
    used = 0
    for i, line in enumerate(lines):
        if not _ANCHOR_RE.search(line):
            continue
        for j in range(i, min(i + 1 + _CONTEXT_AFTER, len(lines))):
            if j in seen:
                continue
            seen.add(j)
            picked.append(j)
            used += len(lines[j]) + 1
        if used >= budget:
            break
    tail_start = max(len(lines) - _TAIL_LINES, 0)
    tail = [lines[j] for j in range(tail_start, len(lines)) if j not in seen]
    if picked:
        body = "\n".join(lines[j] for j in picked)
        if tail:
            body += "\n--- tail ---\n" + "\n".join(tail)
    else:
        body = "\n".join(lines[-(_TAIL_LINES * 2):])
    return _clip(body, limit)


# --- config-driven check runner ----------------------------------------------
#
# `agent-exec check` runs a config-declared list of lint/test commands over a
# tree's changed files. Everything here is pure with respect to git: the
# caller (agent_exec.cmd_check) resolves the tree, the changed-file list, and
# any baseline worktree, and hands them in as plain strings.

DEFAULT_TIMEOUT = 1800


def _glob_to_regex(pattern):
    """Translate one `paths` glob to an anchored regex.

    `**` (a whole path segment) means "zero or more whole directories"; `*`
    and `?` inside any other segment stay within that segment (never cross
    `/`). No third-party lib, no `PurePath.full_match` (py3.13+ only).
    """
    segments = pattern.split("/")
    n = len(segments)
    parts = []
    prev_was_doublestar = False
    for i, seg in enumerate(segments):
        if seg == "**":
            if parts and not prev_was_doublestar:
                parts.append("/")
            if i == n - 1:
                parts.append(r"(?:[^/]+/)*[^/]*")
            else:
                parts.append(r"(?:[^/]+/)*")
            prev_was_doublestar = True
            continue
        literal = "".join(
            "[^/]*" if ch == "*" else "[^/]" if ch == "?" else re.escape(ch)
            for ch in seg
        )
        if parts and not prev_was_doublestar:
            parts.append("/")
        parts.append(literal)
        prev_was_doublestar = False
    return "^" + "".join(parts) + "$"


def glob_match(pattern, path):
    """True when POSIX-relative `path` matches one `paths` glob entry."""
    return re.match(_glob_to_regex(pattern), path) is not None


def match_files(patterns, files):
    """Changed `files` that satisfy any of `patterns`.

    An empty/None `patterns` list matches every file -- that is what "omitted
    = always runs" means for a check with no `paths` entry.
    """
    if not patterns:
        return list(files)
    return [f for f in files if any(glob_match(p, f) for p in patterns)]


def _files_for_command(tree, cwd_dir, matched):
    """`matched` files that still exist, re-expressed relative to `cwd_dir`.

    A file outside `cwd_dir` is dropped: it cannot be named relative to the
    directory the command will run in without a leading `../` the command's
    own tool would not expect.
    """
    out = []
    for rel in matched:
        abs_path = os.path.join(tree, rel)
        if not os.path.isfile(abs_path):
            continue
        rel_to_cwd = os.path.relpath(abs_path, cwd_dir)
        if rel_to_cwd == os.pardir or rel_to_cwd.startswith(os.pardir + os.sep):
            continue
        out.append(rel_to_cwd)
    return out


def _quote_files(files):
    return " ".join(shlex.quote(f) for f in files)


def _acquire_slot(slot_dir, max_parallel):
    """Block until one of `max_parallel` lock files is free, then hold it.

    Returns the open file handle (release with `_release_slot`), or None when
    `max_parallel` is 0 ("unlimited": no slot is acquired at all).
    """
    if not max_parallel:
        return None
    os.makedirs(slot_dir, exist_ok=True)
    while True:
        for n in range(max_parallel):
            path = os.path.join(slot_dir, "slot-%d.lock" % n)
            handle = open(path, "a+")
            try:
                fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                return handle
            except OSError:
                handle.close()
        time.sleep(0.5)


def _release_slot(handle):
    if handle is None:
        return
    try:
        fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
    finally:
        handle.close()


def _run_shell(command, cwd, timeout, slot_dir, max_parallel):
    """Run `command` via `/bin/sh -c`, killing its whole process group on timeout.

    Returns (exit_code_or_None, stdout, stderr, timed_out, seconds).
    """
    handle = _acquire_slot(slot_dir, max_parallel)
    start = time.time()
    try:
        proc = subprocess.Popen(
            ["/bin/sh", "-c", command], cwd=cwd,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, start_new_session=True,
        )
        timed_out = False
        try:
            stdout, stderr = proc.communicate(timeout=timeout)
            exit_code = proc.returncode
        except subprocess.TimeoutExpired:
            timed_out = True
            try:
                os.killpg(os.getpgid(proc.pid), 9)
            except (OSError, ProcessLookupError):
                pass
            stdout, stderr = proc.communicate()
            exit_code = None
        return exit_code, stdout or "", stderr or "", timed_out, time.time() - start
    finally:
        _release_slot(handle)


def _junit_failure_lines(path):
    """`classname > name: <first line of message>` per failing/erroring
    testcase, or None if `path` does not exist or does not parse as JUnit XML.
    """
    if not os.path.isfile(path):
        return None
    try:
        root = ET.parse(path).getroot()
    except ET.ParseError:
        return None
    lines = []
    for testcase in root.iter("testcase"):
        node = testcase.find("failure")
        if node is None:
            node = testcase.find("error")
        if node is None:
            continue
        message = (node.get("message") or node.text or "").strip()
        first_line = message.splitlines()[0] if message else ""
        lines.append("%s > %s: %s" % (
            testcase.get("classname", ""), testcase.get("name", ""), first_line,
        ))
    return lines


def _build_excerpt(junit_path, combined_output, limit=4000):
    if junit_path:
        lines = _junit_failure_lines(junit_path)
        if lines:
            header = "\n".join(lines)
            if len(header) >= limit:
                return header[:limit]
            rest = failure_excerpt(combined_output, limit - len(header) - 1)
            return (header + ("\n" + rest if rest else ""))[:limit]
    return failure_excerpt(combined_output, limit)


def _empty_result(name, status, reason=""):
    return {
        "name": name, "status": status, "exit": None, "seconds": 0.0,
        "timed_out": False, "excerpt": "", "reason": reason,
    }


def run_check(item, tree, files, slot_dir, max_parallel=2):
    """Run one `checks.items` entry against `files` changed in `tree`.

    Returns a result dict: name/status(pass|fail|skipped)/exit/seconds/
    timed_out/excerpt/reason. Never raises: a malformed item is the caller's
    problem (config validation happens before this is called).
    """
    name = item["name"]
    patterns = item.get("paths")
    cwd_rel = item.get("cwd") or "."
    cwd_dir = os.path.normpath(os.path.join(tree, cwd_rel))
    timeout = item.get("timeout") or DEFAULT_TIMEOUT

    matched = match_files(patterns, files)
    applies = True if not patterns else bool(matched)
    if not applies:
        return _empty_result(name, "skipped", "no matching files")

    run_template = item["run"]
    uses_files = "{files}" in run_template
    quoted = _quote_files(_files_for_command(tree, cwd_dir, matched))
    if uses_files and not quoted:
        return _empty_result(name, "skipped", "no matching files")

    junit_rel = item.get("junit")
    junit_path = os.path.normpath(os.path.join(cwd_dir, junit_rel)) if junit_rel else None
    if junit_path and os.path.isfile(junit_path):
        try:
            os.remove(junit_path)
        except OSError:
            pass

    fix_template = item.get("fix")
    if fix_template:
        fix_cmd = fix_template.replace("{files}", quoted) if "{files}" in fix_template else fix_template
        _run_shell(fix_cmd, cwd_dir, timeout, slot_dir, max_parallel)

    run_cmd = run_template.replace("{files}", quoted) if uses_files else run_template
    exit_code, stdout, stderr, timed_out, seconds = _run_shell(
        run_cmd, cwd_dir, timeout, slot_dir, max_parallel
    )
    passed = exit_code == 0 and not timed_out
    excerpt = "" if passed else _build_excerpt(junit_path, stdout + stderr)
    return {
        "name": name, "status": "pass" if passed else "fail", "exit": exit_code,
        "seconds": seconds, "timed_out": timed_out, "excerpt": excerpt, "reason": "",
    }


def run_checks(items, tree, files, slot_dir, max_parallel=2, run_all=False):
    """Run `items` in config order, stopping at the first failure unless
    `run_all`. Unreached items are reported `skipped`."""
    results = []
    stop = False
    for item in items:
        if stop:
            results.append(_empty_result(
                item["name"], "skipped", "not run after earlier failure"
            ))
            continue
        result = run_check(item, tree, files, slot_dir, max_parallel=max_parallel)
        results.append(result)
        if result["status"] == "fail" and not run_all:
            stop = True
    return results
