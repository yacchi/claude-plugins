# /// script
# requires-python = ">=3.9"
# dependencies = []
# ///
"""agent-exec: generic, TTY-agnostic external-executor wrapper.

Lets orchestra dispatch Copilot (and future CLIs) without requiring users to
hand-configure allow-all env vars in settings.json. The wrapper injects the
target executor's required environment internally, so the only Claude Code
permission rule needed is: Bash(agent-exec:*)

Usage:
  agent-exec install            interactive installer
  agent-exec list                print known profile names
  agent-exec <profile> [args..]  dispatch to the profile's executor
  agent-exec -h | --help          show this help
"""

import glob
import os
import shutil
import sys
from pathlib import Path

PROFILES = {
    "copilot": {
        "exec": "copilot",
        "env": {"COPILOT_ALLOW_ALL": "true"},
        "mode": "headless",
    },
}

USAGE = """\
agent-exec: dispatch external agent CLIs with their required env pre-injected.

Usage:
  agent-exec install            interactive installer (adds a shim to PATH)
  agent-exec list                print known profile names, one per line
  agent-exec <profile> [args...] dispatch: inject profile env, exec the
                                  target CLI with args passed through verbatim
  agent-exec -h | --help          show this help

Known profiles: {profiles}
""".format(profiles=", ".join(PROFILES))


def print_usage(stream=sys.stdout):
    stream.write(USAGE)


def cmd_list():
    for name in PROFILES:
        print(name)
    return 0


def cmd_dispatch(profile_name, args):
    profile = PROFILES.get(profile_name)
    if profile is None:
        sys.stderr.write(
            "agent-exec: unknown profile or command: %s (run 'agent-exec list')\n"
            % profile_name
        )
        return 2

    mode = profile["mode"]
    if mode != "headless":
        sys.stderr.write(
            "agent-exec: profile %s uses mode=%s, not yet implemented\n"
            % (profile_name, mode)
        )
        return 3

    exec_name = profile["exec"]
    profile_env = profile["env"]

    if os.environ.get("AGENT_EXEC_DRYRUN"):
        print("PROFILE: %s" % profile_name)
        print("MODE: %s" % mode)
        print("ENV: " + ", ".join("%s=%s" % (k, v) for k, v in profile_env.items()))
        print("EXEC: %s %s" % (exec_name, " ".join(args)))
        return 0

    resolved = shutil.which(exec_name)
    if resolved is None:
        sys.stderr.write(
            "agent-exec: executor '%s' not found on PATH\n" % exec_name
        )
        return 127

    env = dict(os.environ)
    env.update(profile_env)
    os.execvpe(exec_name, [exec_name] + args, env)  # never returns


# --- install subcommand -----------------------------------------------------


def _prompt(prompt_text, default=None):
    try:
        raw = input(prompt_text)
    except EOFError:
        return default
    raw = raw.strip()
    if raw == "":
        return default
    return raw


def _resolve_bootstrap():
    candidates = sorted(
        glob.glob(
            os.path.expanduser(
                "~/.claude/plugins/marketplaces/*/plugins/orchestra/tools/agent-exec"
            )
        )
    )

    if len(candidates) == 1:
        bootstrap = candidates[0]
    elif len(candidates) > 1:
        this_file = os.path.abspath(__file__)
        preferred = None
        marker = "/marketplaces/"
        idx = this_file.find(marker)
        if idx != -1:
            rest = this_file[idx + len(marker):]
            mp_name = rest.split("/", 1)[0]
            needle = "/marketplaces/%s/" % mp_name
            for c in candidates:
                if needle in c:
                    preferred = c
                    break
        if preferred is not None:
            bootstrap = preferred
        else:
            print("Multiple orchestra plugin installs found:")
            for i, c in enumerate(candidates, start=1):
                print("  %d) %s" % (i, c))
            choice = _prompt("Choose one [1]: ", default="1")
            try:
                i = int(choice)
                if i < 1 or i > len(candidates):
                    raise ValueError
            except ValueError:
                i = 1
            bootstrap = candidates[i - 1]
    else:
        this_dir = os.path.dirname(os.path.abspath(__file__))
        bootstrap = os.path.join(this_dir, "agent-exec")
        if "/cache/" in bootstrap:
            print(
                "WARNING: no marketplace clone found; falling back to the "
                "version-stamped cache path:\n  %s\n"
                "This shim will BREAK on the next plugin update. Re-run "
                "'agent-exec install' from the marketplace clone once "
                "available." % bootstrap
            )

    if not os.path.isfile(bootstrap):
        sys.stderr.write(
            "agent-exec: install: resolved bootstrap does not exist: %s\n"
            % bootstrap
        )
        return None

    return os.path.abspath(bootstrap)


def _choose_bindir():
    path_dirs = os.environ.get("PATH", "").split(os.pathsep)
    path_dirs_abs = {os.path.abspath(p) for p in path_dirs if p}

    raw_candidates = [
        "~/.local/bin",
        "/usr/local/bin",
        "~/bin",
        "~/.claude/bin",
    ]
    candidates = [os.path.expanduser(c) for c in raw_candidates]

    on_path = [os.path.abspath(c) in path_dirs_abs for c in candidates]

    default_idx = 0
    for i, c in enumerate(candidates):
        if on_path[i] and os.path.abspath(c) == os.path.abspath(
            os.path.expanduser("~/.local/bin")
        ):
            default_idx = i
            break
    else:
        for i in range(len(candidates)):
            if on_path[i]:
                default_idx = i
                break

    print("Choose an install directory for the agent-exec shim:")
    for i, c in enumerate(candidates, start=1):
        tag = "(on PATH)" if on_path[i - 1] else "(NOT on PATH)"
        marker = " [default]" if (i - 1) == default_idx else ""
        print("  %d) %s %s%s" % (i, c, tag, marker))
    print("  %d) enter a custom absolute path" % (len(candidates) + 1))

    choice = _prompt(
        "Choice [%d]: " % (default_idx + 1), default=str(default_idx + 1)
    )

    try:
        i = int(choice)
    except ValueError:
        i = default_idx + 1

    if i == len(candidates) + 1:
        custom = _prompt("Custom absolute path: ", default=None)
        if not custom:
            return candidates[default_idx]
        return os.path.expanduser(custom)

    if 1 <= i <= len(candidates):
        return candidates[i - 1]

    return candidates[default_idx]


def cmd_install():
    bootstrap = _resolve_bootstrap()
    if bootstrap is None:
        return 1

    bindir = _choose_bindir()
    os.makedirs(bindir, exist_ok=True)
    target = os.path.join(bindir, "agent-exec")

    if os.path.exists(target):
        answer = _prompt(
            "%s already exists. Overwrite? (y/N): " % target, default="n"
        )
        if answer.lower() not in ("y", "yes"):
            print("Aborted: not overwriting existing %s" % target)
            return 1

    shim_content = '#!/bin/sh\nexec "%s" "$@"\n' % bootstrap
    with open(target, "w") as f:
        f.write(shim_content)
    os.chmod(target, 0o755)

    print("installed %s -> %s" % (target, bootstrap))

    path_dirs_abs = {
        os.path.abspath(p) for p in os.environ.get("PATH", "").split(os.pathsep) if p
    }
    if os.path.abspath(bindir) not in path_dirs_abs:
        print("")
        print("WARNING: %s is not on your PATH." % bindir)
        print("Add this to your shell rc file:")
        print('  export PATH="%s:$PATH"' % bindir)

    print("")
    print("Required: add \"Bash(agent-exec:*)\" to permissions.allow in")
    print("  .claude/settings.json (project) or ~/.claude/settings.json (user)")
    print("  You can also run /permissions in Claude Code to add it.")
    print("")
    print("Quick test: agent-exec list")

    return 0


def main(argv):
    if len(argv) == 0 or argv[0] in ("-h", "--help"):
        print_usage()
        return 0

    tok = argv[0]

    if tok == "install":
        return cmd_install()

    if tok == "list":
        return cmd_list()

    return cmd_dispatch(tok, argv[1:])


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
