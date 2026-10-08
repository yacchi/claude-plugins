#!/usr/bin/env python3
"""Fake `pi` for the Linux sandbox check: no model, no auth.

The prompt (stdin) lists shell commands, one per line after a `COMMANDS:`
line. Each runs with bash in the cwd; the output is pi 1.1.0-shaped JSONL.
`--session <id>` resumes: the id is echoed, a line is appended to the
session file, and a prompt without `COMMANDS:` (agent-exec's grant-resume
prompt) re-runs the commands the session was started with. Every other
flag agent-exec passes is ignored.
"""

import json
import os
import subprocess
import sys
import uuid


def emit(record):
    sys.stdout.write(json.dumps(record) + "\n")
    sys.stdout.flush()


def commands_of(prompt):
    lines = prompt.splitlines()
    if "COMMANDS:" not in [line.strip() for line in lines]:
        return None
    start = [line.strip() for line in lines].index("COMMANDS:") + 1
    return [line for line in lines[start:] if line.strip()]


def main(argv):
    session = None
    if "--session" in argv:
        i = argv.index("--session")
        if i + 1 < len(argv):
            session = argv[i + 1]
    resumed = session is not None
    session = session or str(uuid.uuid4())
    prompt = sys.stdin.read()

    sessions = os.path.expanduser("~/.pi/agent/sessions")
    os.makedirs(sessions, exist_ok=True)
    path = os.path.join(sessions, session + ".jsonl")
    commands = commands_of(prompt)
    if commands is None and resumed and os.path.exists(path):
        with open(path, encoding="utf-8") as fh:
            first = json.loads(fh.readline())
        commands = first.get("commands") or []
    commands = commands or []
    with open(path, "a", encoding="utf-8") as fh:
        fh.write(json.dumps({"resumed": resumed, "cwd": os.getcwd(),
                             "commands": commands}) + "\n")

    emit({"type": "session", "version": 3, "id": session,
          "timestamp": "2026-01-01T00:00:00.000Z", "cwd": os.getcwd()})
    for n, command in enumerate(commands):
        call_id = "call_%d" % n
        args = {"command": command}
        emit({"type": "tool_execution_start", "toolCallId": call_id,
              "toolName": "bash", "args": args})
        proc = subprocess.run(["bash", "-c", command], stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
        text = proc.stdout.decode("utf-8", errors="replace")
        emit({"type": "tool_execution_end", "toolCallId": call_id,
              "toolName": "bash", "args": args,
              "result": {"content": [{"type": "text", "text": text}]},
              "isError": proc.returncode != 0})
    usage = {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0,
             "reasoning": 0, "totalTokens": 0,
             "cost": {"input": 0, "output": 0, "cacheRead": 0,
                      "cacheWrite": 0, "total": 0}}
    emit({"type": "message_end",
          "message": {"role": "assistant",
                      "content": [{"type": "text", "text": "DONE"}],
                      "usage": usage, "stopReason": "stop"}})
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
