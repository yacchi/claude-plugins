"""Generate the P5 parity fixtures from the Python implementation.

    go build -o /tmp/fakepi ./internal/testutil/fakepi
    uv run --no-project --with pyyaml python \
        internal/capture/testdata/gen_fixtures.py /tmp/fakepi

(run from plugins/orchestra/tools/agentexec). Writes, never edited by hand:

  internal/executor/pi/testdata/parse_fixtures.json
      parse_pi_jsonl over hand-made transcripts and over fakepi's own
      output (stdout, stderr, exit code captured verbatim).
  internal/watchdog/testdata/fixtures.json
      agent_exec_watchdog.Watchdog driven by a fake clock: a list of
      line/advance/check ops with every check's return and the final
      result().
  internal/capture/testdata/capture_fixtures.json
      _capture_with_grants end to end with fakepi as `pi` on PATH and the
      sandbox off: prompt, resume, class, watchdog config -> result.
      Runaway elapsed/runtime are timing, recorded as "<S>".
  internal/capture/testdata/grants_fixtures.json
      _capture_with_grants with the executor runs replaced by canned pi
      transcripts (as GrantLoopTests do) under a Seatbelt-shaped policy:
      result and learned store, $HOME spelled <HOME>.
"""

import copy
import json
import os
import shutil
import subprocess
import sys
import tempfile
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
AGENTEXEC = os.path.normpath(os.path.join(HERE, "..", "..", ".."))
TOOLS = os.path.normpath(os.path.join(AGENTEXEC, ".."))

ROOT = os.path.realpath(tempfile.mkdtemp(prefix="p5-fixtures-"))
HOME = os.path.join(ROOT, "home")
os.makedirs(HOME)
os.environ["HOME"] = HOME
os.environ.pop("XDG_STATE_HOME", None)
os.environ.pop("PI_CODING_AGENT_DIR", None)
os.environ.pop("XDG_CONFIG_HOME", None)
os.environ["CLAUDE_CONFIG_DIR"] = os.path.join(HOME, ".claude")
os.environ["AGENT_EXEC_SANDBOX_BACKEND"] = "none"

sys.path.insert(0, TOOLS)
import agent_exec  # noqa: E402
import agent_exec_sandbox as sb  # noqa: E402
import agent_exec_watchdog as wd  # noqa: E402


def write(rel, data):
    path = os.path.join(AGENTEXEC, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(data, fh, ensure_ascii=False, indent=1, sort_keys=False)
        fh.write("\n")


def line(obj):
    return json.dumps(obj, ensure_ascii=False)


def usage(inp, out, cache_read=0, cost=0.0, reasoning=0, cache_write=0):
    return {"input": inp, "output": out, "cacheRead": cache_read,
            "cacheWrite": cache_write, "reasoning": reasoning,
            "totalTokens": inp + out, "cost": {"total": cost}}


def end(text=None, u=None, stop="stop", error=None, content=None, role="assistant"):
    if content is None:
        content = [] if text is None else [{"type": "text", "text": text}]
    message = {"role": role, "content": content, "usage": u or usage(0, 0), "stopReason": stop}
    if error is not None:
        message["errorMessage"] = error
    return line({"type": "message_end", "message": message})


SID = "01a11b14-6032-733f-aa30-17504543f162"
SESSION = line({"type": "session", "version": 3, "id": SID, "cwd": "/w"})


def out(*lines):
    return "\n".join(lines) + "\n"


# --- parse ------------------------------------------------------------------

HAND = [
    ("error-stop-rc0", out(SESSION, end(stop="error", error="Codex error: The 'x' model is not supported")),
     "Warning: Model not found.\n", 0, False),
    ("error-message-rate-limit", out(SESSION, end(stop="error", error="429 rate limit exceeded")), "", 0, False),
    ("no-api-key-stderr", out(SESSION), "No API key found for openai.\n\nUse /login to log in.\n", 1, False),
    ("last-assistant-answer", out(SESSION, end(text="let me look", stop="toolUse",
                                               content=[{"type": "text", "text": "let me look"},
                                                        {"type": "toolCall", "name": "bash"}]),
                                  end(text="DONE")), "", 0, False),
    ("usage-sums-assistant-only", out(
        SESSION,
        line({"type": "message_update", "usage": usage(999, 999, cost=9.0)}),
        end(role="user", u=usage(500, 500, cost=5.0)),
        end(text="a", u=usage(100, 10, cache_read=40, cost=0.001, reasoning=3), stop="toolUse"),
        line({"type": "message_update", "usage": usage(888, 888, cost=8.0)}),
        end(text="b", u=usage(200, 20, cache_read=60, cost=0.002, reasoning=4))), "", 0, False),
    ("tool-output-not-scanned", out(
        SESSION,
        line({"type": "tool_execution_end", "isError": True,
              "result": {"content": [{"type": "text", "text": "rate limit quota 429 exceeded"}]}}),
        end(role="toolResult", content=[{"type": "text", "text": "quota usage limit 401 login"}]),
        end(text="the file talks about a rate limit")), "", 0, False),
    ("u2028-not-split", out(SESSION, end(text="before after")), "", 0, False),
    ("crlf-and-blank-lines", SESSION + "\r\n\r\n   \n" + end(text="ok") + "\r\n", "", 0, True),
    ("no-trailing-newline", SESSION + "\n" + end(text="ok"), "", 0, False),
    ("unparseable-line-scanned", out(SESSION, "Error: not logged in", end(text="ok")), "", 0, False),
    ("unparseable-harmless", out(SESSION, "garbage {", end(text="ok")), "", 0, False),
    ("trailing-data-is-unparseable", out(SESSION, '{"type": "x"} quota', end(text="ok")), "", 0, False),
    ("non-object-json", out(SESSION, "123", '"quota"', "[1, 2]", "null", end(text="ok")), "", 0, False),
    ("error-event-quota", out(SESSION, line({"type": "error", "error": "You have exceeded your usage quota"}),
                              end(text="x")), "", 0, False),
    ("error-type-case-insensitive", out(SESSION, line({"type": "Auth.ERROR", "message": "credit"}),
                                        end(text="x")), "", 0, False),
    ("error-field-falsy-not-scanned", out(
        SESSION,
        line({"type": "x", "error": 0, "m": "quota"}),
        line({"type": "x", "error": "", "m": "quota"}),
        line({"type": "x", "error": [], "m": "quota"}),
        line({"type": "x", "error": {}, "m": "quota"}),
        line({"type": "x", "error": False, "m": "quota"}),
        line({"type": "x", "error": None, "m": "quota"}),
        end(text="x")), "", 0, False),
    ("error-field-truthy-scanned", out(SESSION, line({"type": "x", "error": 1, "m": "credit"}), end(text="x")),
     "", 0, False),
    ("result-success-false", out(SESSION, line({"type": "result", "success": False, "m": "login"}), end(text="x")),
     "", 0, False),
    ("result-ok-false", out(SESSION, line({"type": "result", "ok": False, "m": "quota"}), end(text="x")), "", 0, False),
    ("result-iserror", out(SESSION, line({"type": "result", "isError": "yes", "m": "rate limit"}), end(text="x")),
     "", 0, False),
    ("result-success-zero-not-scanned", out(SESSION, line({"type": "result", "success": 0, "m": "quota"}),
                                            end(text="x")), "", 0, False),
    ("content-event-error-not-scanned", out(SESSION, line({"type": "tool_error", "error": "quota"}),
                                            line({"type": "session_error", "error": "quota"}),
                                            line({"type": "agent.error", "error": "quota"}), end(text="x")),
     "", 0, False),
    ("non-string-type", out(SESSION, line({"type": 5, "error": "quota"}), end(text="x")), "", 0, False),
    ("session-first-wins", out(line({"type": "session", "id": ""}), line({"type": "session", "id": 7}),
                               line({"type": "session", "id": "first"}), line({"type": "session", "id": "second"}),
                               end(text="x")), "", 0, False),
    ("no-session", out(end(text="x")), "", 0, False),
    ("empty-stdout", "", "", 0, False),
    ("nonzero-exit-with-answer", out(SESSION, end(text="DONE")), "", 2, False),
    ("negative-exit", out(SESSION), "", -15, False),
    ("whitespace-answer", out(SESSION, end(text=" \n\t\x1c　")), "", 0, False),
    ("string-content", out(SESSION, line({"type": "message_end", "message": {"role": "assistant",
                                                                             "content": "plain", "stopReason": "stop"}})),
     "", 0, False),
    ("mixed-content-blocks", out(SESSION, end(content=[{"type": "text", "text": "a"}, {"type": "text", "text": 5},
                                                       {"type": "thinking", "text": "z"}, "str", {"type": "text", "text": "b"}])),
     "", 0, False),
    ("message-not-object", out(SESSION, line({"type": "message_end", "message": "x"}),
                               line({"type": "message_end"}), end(text="ok")), "", 0, False),
    ("usage-odd-values", out(SESSION, end(text="a", u={"input": -1, "output": True, "cacheRead": 1.0,
                                                       "cacheWrite": 2, "reasoning": "3", "cost": {"total": True}}),
                             end(text="b", u={"input": 5, "cost": {"total": -0.5}}),
                             end(text="c", u={"output": 7, "cost": {"total": 2}}),
                             end(text="d", u={"cost": "x"}),
                             end(text="e", u="nope")), "", 0, False),
    ("cost-rounding", out(SESSION, end(text="a", u=usage(1, 1, cost=0.0000005)),
                          end(text="b", u=usage(1, 1, cost=0.0000015)),
                          end(text="c", u=usage(1, 1, cost=0.002699))), "", 0, False),
    ("only-cost", out(SESSION, end(text="a", u={"cost": {"total": 0.25}})), "", 0, False),
    ("error-stop-with-answer", out(SESSION, end(text="partial", stop="error")), "", 0, False),
    ("stop-reason-missing", out(SESSION, line({"type": "message_end", "message": {"role": "assistant",
                                                                                  "content": [{"type": "text", "text": "y"}]}})),
     "", 0, False),
    ("pattern-order-quota-before-auth", out(SESSION, end(stop="error", error="login quota")), "", 0, False),
    ("credits-pattern", out(SESSION), "Error: insufficient balance\n", 1, False),
    ("unicode-casefold", out(SESSION, end(stop="error", error="QUOTA exceeded K")), "", 0, False),
    ("duplicate-keys-last-wins", out(SESSION, '{"type": "x", "error": "quota", "error": ""}', end(text="ok")),
     "", 0, False),
    ("resumed-flag", out(SESSION, end(text="ok")), "", 0, True),
]

FAKEPI_PROMPTS = [
    ("fakepi-ok", "hello\nFAKEPI-ANSWER: PONG\n", None),
    ("fakepi-commands", "COMMANDS:\necho hi\nls /nonexistent-p5-zzz\n", None),
    ("fakepi-quota", "FAKEPI-FAULT: quota\n", None),
    ("fakepi-nonzero", "FAKEPI-FAULT: nonzero\n", None),
    ("fakepi-exit3", "FAKEPI-EXIT: 3\nFAKEPI-ANSWER: x\n", None),
    ("fakepi-longline", "FAKEPI-LONGLINE: 200000\n", None),
    ("fakepi-raw-events", 'FAKEPI-EVENT: {"type": "result", "success": false, "m": "x"}\n'
                          'FAKEPI-EVENT: not json at all\nFAKEPI-STDERR: warning: something\n', None),
    ("fakepi-multiline-answer", "FAKEPI-ANSWER: line one\nFAKEPI-ANSWER: line two\n", None),
    ("fakepi-resume", "FAKEPI-ANSWER: again\n", "11111111-2222-4333-a444-555555555555"),
]


def gen_parse(fakepi):
    cases = []
    for name, stdout, stderr, code, resumed in HAND:
        cases.append({"name": name, "stdout": stdout, "stderr": stderr, "exit_code": code,
                      "resumed": resumed,
                      "want": agent_exec.parse_pi_jsonl(stdout, stderr, code, resumed=resumed)})
    work = os.path.join(ROOT, "fakepi-work")
    os.makedirs(work, exist_ok=True)
    for name, prompt, resume in FAKEPI_PROMPTS:
        argv = [fakepi, "-p", "--mode", "json"] + (["--session", resume] if resume else [])
        proc = subprocess.run(argv, input=prompt.encode(), capture_output=True, cwd=work)
        stdout = proc.stdout.decode("utf-8", errors="replace")
        stderr = proc.stderr.decode("utf-8", errors="replace")
        cases.append({"name": name, "stdout": stdout, "stderr": stderr, "exit_code": proc.returncode,
                      "resumed": resume is not None,
                      "want": agent_exec.parse_pi_jsonl(stdout, stderr, proc.returncode,
                                                        resumed=resume is not None)})
    write("internal/executor/pi/testdata/parse_fixtures.json", {"cases": cases})
    return len(cases)


# --- watchdog ---------------------------------------------------------------


def start(call_id, name="bash", args=None, **extra):
    rec = {"type": "tool_execution_start", "toolName": name, "args": {"id": call_id} if args is None else args}
    if call_id is not None:
        rec["toolCallId"] = call_id
    rec.update(extra)
    return {"op": "line", "text": json.dumps(rec, ensure_ascii=False)}


def stop(call_id):
    rec = {"type": "tool_execution_end"}
    if call_id is not None:
        rec["toolCallId"] = call_id
    return {"op": "line", "text": json.dumps(rec)}


def adv(s):
    return {"op": "advance", "s": s}


CHECK = {"op": "check"}

WATCHDOG = [
    ("parallel-idle", {"idle_seconds": 3, "tool_idle_seconds": 5},
     [start("a"), start("b"), stop("b"), adv(4), CHECK, stop("a"), adv(3), CHECK]),
    ("turn-end-clears", {"idle_seconds": 3, "tool_idle_seconds": 50},
     [start("a"), {"op": "line", "text": '{"type": "tool_execution_end", "toolCallId": '},
      {"op": "line", "text": '{"type": "turn_end"}'}, adv(3), CHECK]),
    ("deadline-frozen-at-trip", {"idle_seconds": 3, "tool_idle_seconds": 5},
     [start("a"), adv(4.9), CHECK, adv(0.1), CHECK, adv(5), CHECK]),
    ("wall", {"idle_seconds": 100, "tool_idle_seconds": 100, "wall_seconds": {"deep": 10, "standard": 4}},
     [adv(2), {"op": "line", "text": "not json"}, adv(1.5), CHECK, adv(0.5), CHECK]),
    ("wall-class", {"wall_seconds": {"deep": 10, "standard": 4}},
     [adv(5), CHECK, adv(5), CHECK], "deep"),
    ("wall-unknown-class", {"wall_seconds": {"deep": 1, "standard": 3}},
     [adv(2), CHECK, adv(1), CHECK], "nope"),
    ("wall-no-standard", {"wall_seconds": {"deep": 1}}, [adv(2399), CHECK, adv(1), CHECK]),
    ("wall-not-object", {"wall_seconds": "x"}, [adv(2399), CHECK, adv(1), CHECK]),
    ("repeat-consecutive", {"repeat_limit": 3},
     [start("1", args={"x": 1}), stop("1"), start("2", args={"x": 1}), stop("2"),
      adv(0.25), start("3", args={"x": 1}), adv(1), CHECK]),
    ("repeat-interleaved", {"repeat_limit": 3},
     [start("1", "a", {"x": 1}), stop("1"), start("2", "b", {"x": 1}), stop("2"),
      start("3", "a", {"x": 1}), stop("3"), start("4", "b", {"x": 1}), CHECK]),
    ("repeat-key-order-insensitive", {"repeat_limit": 2},
     [start("1", args={"a": 1, "b": [1.0, "é"]}), start("2", args={"b": [1.0, "é"], "a": 1}), CHECK]),
    ("repeat-int-float-differ", {"repeat_limit": 2},
     [start("1", args={"a": 1}), start("2", args={"a": 1.0}), CHECK]),
    ("anonymous-calls", {"idle_seconds": 1, "tool_idle_seconds": 10},
     [start(None), start(None), stop("never-seen"), adv(2), CHECK, stop("never-seen"), adv(0.5), CHECK,
      adv(1), CHECK]),
    ("anonymous-sorted-lexically", {"idle_seconds": 1, "tool_idle_seconds": 10},
     [start(None, args={"n": i}) for i in range(11)]
     + [stop("x") for _ in range(10)] + [adv(2), CHECK, stop("x"), adv(1), CHECK]),
    ("end-without-id-clears", {"idle_seconds": 1, "tool_idle_seconds": 10},
     [start("a"), start("b"), stop(None), adv(1), CHECK]),
    ("agent-end-clears", {"idle_seconds": 1, "tool_idle_seconds": 10},
     [start("a"), {"op": "line", "text": '{"type": "agent_end"}'}, adv(1), CHECK]),
    ("numeric-call-id", {"idle_seconds": 1, "tool_idle_seconds": 10},
     [start(7), stop(7), adv(1), CHECK]),
    ("long-unicode-preview", {"repeat_limit": 1},
     [start("a", name="ツール", args={"text": "あ" * 300, "esc": "\"\\\n\t\x01/"}), CHECK]),
    ("non-string-tool-name", {"repeat_limit": 1}, [start("a", name=None, args=None), CHECK]),
    ("missing-name-and-args", {"repeat_limit": 1},
     [{"op": "line", "text": '{"type": "tool_execution_start", "toolCallId": "z"}'}, CHECK]),
    ("float-args-repr", {"repeat_limit": 1},
     [start("a", args={"f": [1e16, 1e-5, 0.1, 2.0, 123456789012345678901234567890]}), CHECK]),
    ("records-reset-idle", {"idle_seconds": 2},
     [adv(1.5), {"op": "line", "text": "x"}, adv(1.5), CHECK, adv(0.5), CHECK]),
]


def gen_watchdog():
    cases = []
    for entry in WATCHDOG:
        name, config, ops = entry[0], entry[1], entry[2]
        cls = entry[3] if len(entry) > 3 else "standard"
        now = [100.0]
        dog = wd.Watchdog(type("Proc", (), {"pid": 1})(), config, "pi", cls=cls, clock=lambda: now[0])
        checks = []
        for op in ops:
            if op["op"] == "line":
                dog.on_line(op["text"])
            elif op["op"] == "advance":
                now[0] += op["s"]
            else:
                with dog._lock:
                    checks.append(dog._check(now[0]))
        cases.append({"name": name, "config": config, "cls": cls, "ops": ops,
                      "checks": checks, "tool_running": dog.tool_running, "result": dog.result()})
    write("internal/watchdog/testdata/fixtures.json", {"cases": cases})
    return len(cases)


# --- capture (end to end through fakepi) ------------------------------------

FAST = {"enabled": True, "idle_seconds": 600, "tool_idle_seconds": 900, "repeat_limit": 3,
        "wall_seconds": {"standard": 2400}}

CAPTURE = [
    ("ok", "hello\nFAKEPI-ANSWER: PONG\n", None, "standard", None),
    ("commands", "COMMANDS:\necho hi\nls /nonexistent-p5-zzz\n", None, "light", None),
    ("quota", "FAKEPI-FAULT: quota\n", None, "standard", None),
    ("nonzero", "FAKEPI-FAULT: nonzero\n", None, "standard", None),
    ("exit3", "FAKEPI-EXIT: 3\nFAKEPI-ANSWER: x\n", None, "standard", None),
    ("long-line-2mb", "FAKEPI-LONGLINE: 2000000\n", None, "standard", None),
    ("raw-events", 'FAKEPI-EVENT: {"type": "result", "success": false}\nFAKEPI-STDERR: plain warning\n',
     None, "standard", None),
    ("resume", "FAKEPI-ANSWER: again\n", "11111111-2222-4333-a444-555555555555", "standard", None),
    ("hang-idle", "FAKEPI-HANG\n", None, "standard", dict(FAST, idle_seconds=0.3)),
    ("wall", "FAKEPI-SLEEP: 5000\n", None, "deep", dict(FAST, wall_seconds={"deep": 0.4, "standard": 99})),
    ("repeat", "COMMANDS:\nsleep 1\nsleep 1\nsleep 1\n", None, "standard", FAST),
    ("watchdog-disabled", "FAKEPI-SLEEP: 300\n", None, "standard",
     {"enabled": False, "idle_seconds": 0.01, "wall_seconds": {"standard": 0.01}}),
]


def scrub(value):
    """Runaway timings are not reproducible."""
    if isinstance(value, dict):
        out = {}
        for k, v in value.items():
            out[k] = "<S>" if k in ("elapsed_s", "runtime_s") else scrub(v)
        return out
    if isinstance(value, list):
        return [scrub(v) for v in value]
    return value


def gen_capture(fakepi):
    bindir = os.path.join(ROOT, "bin")
    os.makedirs(bindir, exist_ok=True)
    shutil.copy(fakepi, os.path.join(bindir, "pi"))
    work = os.path.join(ROOT, "work")
    os.makedirs(work, exist_ok=True)
    cases = []
    with mock.patch.dict(os.environ, {"PATH": bindir + os.pathsep + os.environ.get("PATH", "")}):
        for name, prompt, resume, cls, watchdog in CAPTURE:
            cfg = copy.deepcopy(agent_exec.DEFAULTS)
            cfg["sandbox"]["mode"] = "off"
            if watchdog is not None:
                cfg["watchdog"] = watchdog
            spec = agent_exec._sandbox_spec("pi", work, cfg, "m", None)
            with mock.patch.object(agent_exec, "_cfg_or_defaults", return_value=cfg):
                code, result, _ = agent_exec._capture_with_grants(
                    "pi", "m", "low", work, prompt, resume, spec, work, cfg,
                    watchdog=cfg["watchdog"], cls=cls)
            cases.append({"name": name, "prompt": prompt, "resume": resume, "class": cls,
                          "watchdog": watchdog, "exit": code, "want": scrub(result)})
    write("internal/capture/testdata/capture_fixtures.json", {"cases": cases})
    return len(cases)


# --- grant loop (canned transcripts) ----------------------------------------


def tool_end(text):
    return line({"type": "tool_execution_end", "toolName": "bash",
                 "result": {"content": [{"type": "text", "text": text}], "isError": True},
                 "isError": True})


def transcript(answer="DONE", sid="s1", texts=(), stop="stop", tokens=10, stderr=""):
    lines = []
    if sid is not None:
        lines.append(line({"type": "session", "id": sid}))
    lines += [tool_end(t) for t in texts]
    lines.append(end(text=answer, stop=stop, u=usage(tokens, 1, cost=tokens / 1e6)))
    return {"stdout": out(*lines), "stderr": stderr, "exit_code": 0}


def deny(rel):
    return "mkdir: <HOME>/%s: Operation not permitted" % rel


GRANTS = [
    ("safe-denial-resumed", "seatbelt", [transcript(texts=[deny(".pub-cache/p/x")]), transcript(tokens=5)]),
    ("mixed-set-not-resumed", "seatbelt",
     [transcript(answer="mkdir failed: Operation not permitted",
                 texts=[deny(".pub-cache/p/x"), deny(".config/q")])]),
    ("worked-around-stays-ok", "seatbelt", [transcript(answer="wrote it elsewhere", texts=[deny(".config/q"), "ok"])]),
    ("cycle-cap", "seatbelt", [transcript(texts=[deny(".cache/a/x")]), transcript(texts=[deny(".cache/b/x")]),
                               transcript(texts=[deny(".cache/c/x")])]),
    ("no-session-noted", "seatbelt", [transcript(sid=None, texts=[deny(".cache/a/x")])]),
    ("resumed-without-session-keeps-it", "seatbelt",
     [transcript(texts=[deny(".cache/a/x")]), transcript(stop="error", sid=None, answer=None)]),
    ("bwrap-direct-file-grant", "bwrap", [transcript(texts=[deny(".pnpm-store/probe.txt")]), transcript(tokens=5)]),
    ("bwrap-dir-created", "bwrap", [transcript(texts=[deny(".cargo/registry/x/y")]), transcript(tokens=5)]),
    ("grant-did-not-resolve", "seatbelt",
     [transcript(texts=[deny(".cache/a/x")]),
      transcript(answer="still Operation not permitted", texts=[deny(".cache/a/x")])]),
    ("ok-with-unsafe-advisory", "seatbelt", [transcript(texts=[deny(".config/outside")])]),
    ("failed-with-unsafe-needs-permission", "seatbelt",
     [transcript(stop="error", texts=[deny(".config/outside")])]),
    ("stderr-denial", "seatbelt", [dict(transcript(), stderr="touch: <HOME>/.npm/_x/y: Operation not permitted\n"),
                                   transcript(tokens=3)]),
    ("intentional-deny-read", "seatbelt", [transcript(stop="error", texts=["cat: <HOME>/.ssh/id: Operation not permitted"])]),
    ("no-denials", "seatbelt", [transcript()]),
]


def gen_grants():
    cases = []
    for name, backend, runs in GRANTS:
        home = os.path.realpath(tempfile.mkdtemp(dir=ROOT))
        tree = os.path.join(home, "work", "tree")
        os.makedirs(tree)
        os.makedirs(os.path.join(home, ".pnpm-store"))
        with open(os.path.join(home, ".pnpm-store", "probe.txt"), "w") as fh:
            fh.write("x")
        for d in (".config", ".ssh", ".cache", ".pub-cache", ".npm", ".cargo"):
            os.makedirs(os.path.join(home, d), exist_ok=True)
        spec = {"backend": backend, "enforced": True, "reason": "", "limits": [], "writable": [tree],
                "refuse": False,
                "policy": {"writable": [tree], "deny_read": [os.path.join(home, ".ssh")], "deny_write": [],
                           "deny_write_regex": []}}
        canned = [{k: (v.replace("<HOME>", home) if isinstance(v, str) else v) for k, v in r.items()} for r in runs]
        queue = list(canned)
        calls = []

        def fake(profile, model, effort, workdir, prompt, resume, fmt, sandbox=None, watchdog=None,
                 cls="standard", _q=queue, _c=calls):
            _c.append({"prompt": prompt, "resume": resume})
            r = _q.pop(0)
            result = agent_exec.parse_pi_jsonl(r["stdout"], r["stderr"], r["exit_code"],
                                               resumed=resume is not None)
            result["_denial_input"] = {"events": agent_exec._pi_tool_end_events(r["stdout"]),
                                       "stderr": r["stderr"]}
            return 0, result

        with mock.patch.dict(os.environ, {"HOME": home}), \
                mock.patch.object(agent_exec, "_run_executor_capture", fake), \
                mock.patch.object(agent_exec, "_sandbox_spec", return_value=spec):
            _, result, _ = agent_exec._capture_with_grants(
                "pi", "m", "low", tree, "task", None, spec, tree, {})
            learned = [g["path"] for g in sb.load_learned(home)[0]]
        dirs = sorted(os.path.relpath(os.path.join(dp, d), home) for dp, ds, _ in os.walk(home) for d in ds)

        def norm(v):
            return json.loads(json.dumps(v).replace(home, "<HOME>"))

        cases.append({"name": name, "backend": backend, "runs": runs, "calls": norm(calls),
                      "want": norm(result), "learned": norm(learned), "dirs": dirs})
    write("internal/capture/testdata/grants_fixtures.json", {"cases": cases})
    return len(cases)


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: gen_fixtures.py <fakepi binary>")
    fakepi = os.path.abspath(sys.argv[1])
    try:
        counts = {"parse": gen_parse(fakepi), "watchdog": gen_watchdog(),
                  "capture": gen_capture(fakepi), "grants": gen_grants()}
    finally:
        shutil.rmtree(ROOT, ignore_errors=True)
    sys.stderr.write("fixtures: %s\n" % json.dumps(counts))


if __name__ == "__main__":
    main()
