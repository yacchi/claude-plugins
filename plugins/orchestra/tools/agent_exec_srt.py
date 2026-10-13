"""sandbox-runtime (srt) plumbing shared by the herdr host and the CLI
executors (pi).

srt confines a process with its own Seatbelt / bubblewrap profile, built
from the settings file orchestra writes (`agent_exec_sandbox.srt_settings`),
and lets it reach only `allowedDomains` through srt's proxy. Run with `-d`,
srt prints `[SandboxDebug] Connection blocked to <host>:<port>` lines on its
stderr; with `--control-fd 3` it reads one full-config JSON line per update,
of which only the domain lists change live.

`SrtRun` is one process under srt: settings file (dir 0700, file 0600),
control channel, debug-log tailing into `<state>/audit/network.jsonl` and
the blocked hosts of this run, and the live push of learned domains (one
full line, only when they changed). A CLI run's control channel is a pipe
its parent holds; the herdr server outlives the process that started it, so
its channel is a FIFO (`<state>/herdr-srt/<session>.ctl`) that any later
process pushes to (`push_line`). `cli_network` plans the network of one CLI
executor run: srt status, and the domains its settings allow -- the
configured allowlist, the package-registry presets of the tree
(`ECOSYSTEM_PRESETS`) and that executor's own API hosts.

Stdlib only.
"""

import errno
import fcntl
import fnmatch
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import threading
import uuid
from urllib.parse import urlparse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_exec_sandbox as sb  # noqa: E402

# Package registries a tree's tracked files say it needs, by basename.
# Registry hosts only: never github.com / raw.githubusercontent.com.
ECOSYSTEM_PRESETS = (
    (("package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock",
      "bun.lock", "bun.lockb"), ("registry.npmjs.org",)),
    (("yarn.lock",), ("registry.yarnpkg.com",)),
    (("pyproject.toml", "requirements*.txt", "uv.lock", "Pipfile", "setup.py"),
     ("pypi.org", "files.pythonhosted.org")),
    (("go.mod",), ("proxy.golang.org", "sum.golang.org")),
    (("Cargo.toml",), ("index.crates.io", "static.crates.io", "crates.io")),
    (("Gemfile",), ("rubygems.org", "index.rubygems.org")),
    (("pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle",
      "settings.gradle.kts"),
     ("repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org",
      "services.gradle.org")),
)

# pi reaches its model through the provider named by the model's prefix
# (`openai-codex/gpt-...`); used when pi's `network_domains` is unset. An
# unknown provider gets nothing: its blocked hosts surface as denials.
PI_PROVIDER_DOMAINS = {
    "openai-codex": ("chatgpt.com", "auth.openai.com"),
    "openai": ("api.openai.com",),
    "anthropic": ("api.anthropic.com",),
    "github-copilot": ("api.githubcopilot.com", "*.githubcopilot.com", "api.github.com"),
    "google": ("generativelanguage.googleapis.com",),
}

DEBUG_PREFIX = b"[SandboxDebug]"
_NPMRC_LIMIT = 1 << 16
_NPMRC_RE = re.compile(r"^\s*(?:@[^\s:=]+:)?registry\s*=\s*(\S+)")


def _tools():
    import agent_exec_tools  # noqa: E402 -- imports agent_exec_sandbox itself
    return agent_exec_tools


# --- availability ------------------------------------------------------------


def srt_status(cfg, which=None, platform=None, binary=None):
    """(path or None, version or None, reason): can sandbox-runtime run
    here? `binary` overrides resolution (False: unavailable). On Linux srt
    also needs bwrap and socat. The sandbox test hook
    (AGENT_EXEC_SANDBOX_BACKEND=none) makes a resolved srt unavailable."""
    which = which or shutil.which
    platform = platform or sys.platform
    version = None
    if binary is False:
        return None, None, "srt not available"
    if binary:
        path = binary
    else:
        if os.environ.get(sb.FORCE_ENV) == "none":
            return None, None, "srt forced unavailable by %s=none" % sb.FORCE_ENV
        info = _tools().resolve("srt", cfg)
        path, version = info.get("path"), info.get("version")
        if not path:
            return None, None, "srt not found (%s)" % (info.get("reason") or "not installed")
    if platform.startswith("linux"):
        missing = [b for b in ("bwrap", "socat") if not which(b)]
        if missing:
            return None, version, "srt needs %s on Linux" % " and ".join(missing)
    return path, version, ""


# --- domains -----------------------------------------------------------------


def _clean_domains(values):
    return sb._dedupe(v.strip().lower() for v in values or ()
                      if isinstance(v, str) and sb.domain_refusal(v) is None)


def _git_out(tree, args, runner):
    try:
        proc = runner(["git", "-C", tree] + list(args), capture_output=True,
                      text=True, timeout=30)
    except (OSError, subprocess.SubprocessError):
        return None
    if proc.returncode != 0:
        return None
    return proc.stdout


def npmrc_hosts(path):
    """Hosts of every `registry=` / `@scope:registry=` URL in an .npmrc;
    anything `domain_refusal` refuses (IPs, localhost, junk) is skipped."""
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            text = fh.read(_NPMRC_LIMIT)
    except OSError:
        return []
    hosts = []
    for line in text.splitlines():
        if line.lstrip().startswith(("#", ";")):
            continue
        match = _NPMRC_RE.match(line)
        if not match:
            continue
        url = match.group(1).strip("\"'")
        if "://" not in url:
            url = "//" + url
        try:
            host = urlparse(url).hostname
        except ValueError:
            continue
        if host and sb.domain_refusal(host) is None:
            hosts.append(host.lower())
    return sb._dedupe(hosts)


def ecosystem_domains(tree, runner=subprocess.run):
    """Registry hosts for the tracked files of the repository holding
    `tree` (`git ls-files`, basename match; untracked and ignored files do
    not count). Only a tracked root `.npmrc` is read."""
    root = (_git_out(tree, ["rev-parse", "--show-toplevel"], runner) or "").strip()
    if not root:
        return []
    listing = _git_out(root, ["ls-files", "-z"], runner)
    if not listing:
        return []
    names = set()
    npmrc = False
    for path in listing.split("\0"):
        if not path:
            continue
        if path == ".npmrc":
            npmrc = True
        names.add(path.rsplit("/", 1)[-1])
    out = []
    for patterns, domains in ECOSYSTEM_PRESETS:
        for pattern in patterns:
            if "*" in pattern:
                hit = any(fnmatch.fnmatchcase(n, pattern) for n in names)
            else:
                hit = pattern in names
            if hit:
                out += domains
                break
    if npmrc:
        out += npmrc_hosts(os.path.join(root, ".npmrc"))
    return sb._dedupe(out)


def executor_domains(executor, ecfg, model=None):
    """The API hosts of one CLI executor run: its `network_domains` when
    set, else (for pi) the hosts of the model's provider prefix. Any other
    executor gets nothing."""
    configured = (ecfg or {}).get("network_domains") if isinstance(ecfg, dict) else None
    if isinstance(configured, list):
        return _clean_domains(configured)
    if executor == "pi":
        prefix = model.split("/", 1)[0] if isinstance(model, str) and "/" in model else ""
        return list(PI_PROVIDER_DOMAINS.get(prefix.strip().lower(), ()))
    return []


def run_id(task=None):
    """The settings/log name of one CLI run: the task id (made path-safe)
    or a random id."""
    if isinstance(task, str) and task.strip():
        safe = re.sub(r"[^A-Za-z0-9._-]", "_", task.strip())[:80].lstrip(".")
        if safe:
            return safe
    return uuid.uuid4().hex[:12]


def cli_network(cfg, executor, tree, model=None, task=None, which=None,
                platform=None, binary=None, runner=subprocess.run):
    """The `network` argument of `agent_exec_sandbox.prepare` for one CLI
    executor run: {srt, reason, domains, id, session}. The domains are
    computed only when srt would be used."""
    cfg = cfg if isinstance(cfg, dict) else {}
    net = sb.normalize_config(cfg.get("sandbox"))["network"]
    rid = run_id(task)
    out = {"srt": None, "reason": "", "domains": [], "id": rid, "session": task or rid}
    if net["mode"] != "allowlist":
        out["reason"] = "sandbox.network.mode is off"
        return out
    path, _version, why = srt_status(cfg, which, platform, binary)
    out["srt"], out["reason"] = path, why
    if path:
        external = cfg.get("external_executors")
        ecfg = external.get(executor) if isinstance(external, dict) else None
        presets = ecosystem_domains(tree, runner) if net["ecosystem"] else []
        out["domains"] = sb._dedupe(list(net["allowed_domains"]) + presets
                                    + executor_domains(executor, ecfg, model))
    return out


# --- one process under srt ---------------------------------------------------


def audit(record):
    """Append one record to `<state>/audit/network.jsonl` (dir 0700, file 0600)."""
    directory = os.path.join(sb.state_dir(), sb.AUDIT_DIR)
    os.makedirs(directory, mode=0o700, exist_ok=True)
    path = os.path.join(directory, sb.NETWORK_AUDIT)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    with os.fdopen(fd, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(record, ensure_ascii=False, sort_keys=True) + "\n")


def make_fifo(path):
    """A fresh control FIFO at `path` (dir 0700, FIFO 0600), opened
    read-write: srt reads it as its control fd and, holding a writer itself,
    never sees EOF when a pushing process closes its end. Returns the fd."""
    directory = os.path.dirname(path)
    os.makedirs(directory, mode=0o700, exist_ok=True)
    os.chmod(directory, 0o700)
    if os.path.lexists(path):
        os.unlink(path)
    os.mkfifo(path, 0o600)
    os.chmod(path, 0o600)
    return os.open(path, os.O_RDWR | os.O_NOFOLLOW)


def push_line(path, data):
    """Write `data` (one full-config line, or b"" to only probe) to the
    control FIFO at `path`. Returns None, or why it cannot be used: missing,
    not a FIFO, or no reader (ENXIO: the srt behind it is gone). Never
    blocks on a FIFO nobody reads; a line is written whole."""
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return "no control channel (%s)" % path
    except OSError as exc:
        return "control channel: %s" % exc
    if not stat.S_ISFIFO(info.st_mode):
        return "control channel is not a FIFO (%s)" % path
    try:
        fd = os.open(path, os.O_WRONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    except OSError as exc:
        if exc.errno == errno.ENXIO:
            return "nothing reads the control channel (%s)" % path
        return "control channel: %s" % exc
    try:
        # Blocking from here: a non-blocking write of a line longer than the
        # pipe buffer could stop half-way and leave srt a broken line.
        os.set_blocking(fd, True)
        view = memoryview(data)
        while view:
            view = view[os.write(fd, view):]
    except OSError as exc:
        return "control channel: %s" % exc
    finally:
        os.close(fd)
    return None


def _control_slot(fd):
    """In the child only: `fd` becomes the (inheritable) control fd."""
    if fd == sb.SRT_CONTROL_FD:
        os.set_inheritable(fd, True)  # dup2 onto itself would keep CLOEXEC
    else:
        os.dup2(fd, sb.SRT_CONTROL_FD)


class SrtRun(object):
    """One process under srt. `settings` is the srt settings dict with the
    static `domains` as allowedDomains; learned domains are added on every
    write and push. Without `settings` only the debug log is watched (a
    server whose control channel is unusable). With `control_path` the
    control channel is that FIFO (`make_fifo` / `push_line`), not a pipe."""

    def __init__(self, settings=None, domains=(), settings_path=None, session=None,
                 quiet=(), pipe=os.pipe, label="srt", control_path=None):
        self.base = settings
        self.domains = list(domains)
        self.settings_path = settings_path
        self.session = session
        self.quiet = list(quiet)
        self.pipe = pipe
        self.label = label
        self.control_path = control_path
        self.control_w = None
        self.stale = None
        self.live = None
        self.log_path = None
        self.offset = 0
        self.partial = ""
        self.blocked = []
        self.lock = threading.Lock()
        self._log = None

    # -- settings --------------------------------------------------------------

    def settings(self, learned):
        settings = json.loads(json.dumps(self.base))
        settings["network"]["allowedDomains"] = sb._dedupe(
            list(self.domains) + list(learned))
        return settings

    def write_settings(self, settings):
        directory = os.path.dirname(self.settings_path)
        os.makedirs(directory, mode=0o700, exist_ok=True)
        os.chmod(directory, 0o700)
        fd = os.open(self.settings_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            os.fchmod(fh.fileno(), 0o600)
            json.dump(settings, fh, indent=1, sort_keys=True)
            fh.write("\n")
        return self.settings_path

    def prepare(self):
        """Write the settings with the learned domains of now; returns the path."""
        learned = sb.learned_domains()
        path = self.write_settings(self.settings(learned))
        self.live = set(learned)
        return path

    # -- spawn -----------------------------------------------------------------

    def spawn(self, popen, argv, **kwargs):
        """`popen(argv, ...)` with the control channel as the child's control
        fd: a fresh pipe's read end (the write end is kept for `push`), or
        the FIFO at `control_path`, of which only srt keeps a descriptor."""
        if self.control_path:
            read_end, write_end = make_fifo(self.control_path), None
        else:
            read_end, write_end = self.pipe()
        try:
            # close_fds=False keeps fd 3; every other fd Python opened is
            # non-inheritable (PEP 446) and so is not leaked.
            proc = popen(argv, close_fds=False,
                         preexec_fn=lambda: _control_slot(read_end), **kwargs)
        except BaseException:
            if write_end is not None:
                os.close(write_end)
            raise
        finally:
            os.close(read_end)
        self.control_w = write_end
        return proc

    def close(self):
        if self.control_w is not None:
            try:
                os.close(self.control_w)
            except OSError:
                pass
            self.control_w = None
        if self._log is not None:
            try:
                self._log.close()
            except OSError:
                pass
            self._log = None

    # -- debug log -------------------------------------------------------------

    def watch(self, path, from_end=True):
        self.log_path = path
        self.partial = ""
        try:
            self.offset = os.path.getsize(path) if from_end else 0
        except OSError:
            self.offset = 0

    def open_log(self):
        """Append handle on the watched log, for a caller that splits srt's
        debug lines out of a shared stderr itself (`StderrSplit`)."""
        os.makedirs(os.path.dirname(self.log_path), mode=0o700, exist_ok=True)
        fd = os.open(self.log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
        self._log = os.fdopen(fd, "ab")
        return self._log

    def log(self, line):
        if self._log is not None:
            self._log.write(line)
            self._log.flush()

    def tail(self):
        if not self.log_path:
            return
        try:
            size = os.path.getsize(self.log_path)
        except OSError:
            return
        if size < self.offset:  # truncated: start over
            self.offset, self.partial = 0, ""
        if size == self.offset:
            return
        with open(self.log_path, "rb") as fh:
            fh.seek(self.offset)
            data = fh.read(size - self.offset)
        self.offset += len(data)
        lines = (self.partial + data.decode("utf-8", "replace")).split("\n")
        self.partial = lines.pop()
        for line in lines:
            parsed = sb.parse_srt_denial(line)
            if parsed is None:
                continue
            host, port = parsed
            self.blocked.append((host, port))
            audit({"ts": sb._now_iso(), "session": self.session,
                   "host": host, "port": port, "decision": "blocked"})

    # -- live domains ----------------------------------------------------------

    def push(self):
        """One full-config line to srt when the learned domains changed."""
        if self.control_path:
            return self._push_fifo()
        if self.control_w is None or self.base is None:
            return
        learned = sb.learned_domains()
        if set(learned) == self.live:
            return
        settings = self.settings(learned)
        line = (json.dumps(settings, sort_keys=True) + "\n").encode("utf-8")
        try:
            os.write(self.control_w, line)
        except OSError as exc:
            sys.stderr.write("agent-exec: %s: srt control pipe: %s\n" % (self.label, exc))
            if self.control_w is not None:
                try:
                    os.close(self.control_w)
                except OSError:
                    pass
                self.control_w = None
            return
        self.live = set(learned)
        try:
            self.write_settings(settings)
        except OSError:
            pass

    def _on_disk(self):
        try:
            with open(self.settings_path, encoding="utf-8") as fh:
                return json.load(fh)
        except (OSError, ValueError):
            return None

    def _push_fifo(self):
        """`push` through the FIFO, from whichever process runs it: one full
        line, only when the settings differ from the file on disk, which is
        rewritten after a push. Pushes of one session are serialized."""
        if self.base is None:
            return
        directory = os.path.dirname(self.settings_path)
        os.makedirs(directory, mode=0o700, exist_ok=True)
        lock = os.open(self.settings_path + ".lock", os.O_WRONLY | os.O_CREAT, 0o600)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX)
            learned = sb.learned_domains()
            settings = self.settings(learned)
            if settings == self._on_disk():
                self.live = set(learned)
                return
            line = (json.dumps(settings, sort_keys=True) + "\n").encode("utf-8")
            why = push_line(self.control_path, line)
            if why:
                if why != self.stale:
                    sys.stderr.write("agent-exec: %s: network settings stale: %s\n"
                                     % (self.label, why))
                self.stale = why
                return
            self.stale = None
            self.live = set(learned)
            self.write_settings(settings)
        finally:
            os.close(lock)

    def tick(self):
        """Tail the log for blocked connections and push newly approved
        domains. Never raises."""
        with self.lock:
            for step in (self.tail, self.push):
                try:
                    step()
                except Exception as exc:
                    sys.stderr.write("agent-exec: %s: network: %s\n" % (self.label, exc))

    def mark(self):
        return len(self.blocked)

    def denials(self, mark=0, quiet=None):
        """Blocked (host, port) pairs logged since `mark`, de-duplicated,
        without the quiet domains."""
        quiet = self.quiet if quiet is None else quiet
        out, seen = [], set()
        for host, port in self.blocked[int(mark or 0):]:
            if sb.domain_matches(host, quiet):
                continue
            if (host, port) not in seen:
                seen.add((host, port))
                out.append({"host": host, "port": port})
        return out


# --- CLI executor runs -------------------------------------------------------


def cli_run(spec, pipe=os.pipe, label="srt"):
    """The SrtRun of a spec with backend `srt` (settings written, log
    watched from its current end), or None for any other spec."""
    if (spec or {}).get("backend") != "srt" or not spec.get("srt"):
        return None
    info = spec["srt"]
    run = SrtRun(info["settings"], info.get("domains") or (), info["settings_path"],
                 info.get("session"), info.get("quiet") or (), pipe, label)
    run.prepare()
    run.watch(info["log_path"], from_end=True)
    return run


def exec_prepare(spec):
    """Before an `exec` of an srt-wrapped argv: write the settings and put
    /dev/null on the control fd (no parent is left to push updates; srt
    refuses to run when the fd it was told to read is closed)."""
    run = cli_run(spec)
    if run is None:
        return
    fd = os.open(os.devnull, os.O_RDONLY)
    if fd == sb.SRT_CONTROL_FD:
        os.set_inheritable(fd, True)
    else:
        os.dup2(fd, sb.SRT_CONTROL_FD)
        os.close(fd)


class StderrSplit(object):
    """srt and the command it runs share one stderr. Complete lines that
    start with `[SandboxDebug]` go to the run's log; everything else is the
    executor's own stderr and is returned by `feed`/`flush`. srt logs its
    network config as pretty-printed JSON whose first line alone carries
    the prefix (`[SandboxDebug] {`): the lines up to the closing bracket at
    column 0 belong to it."""

    def __init__(self, run):
        self.run = run
        self.buf = b""
        self.close_line = None

    def feed(self, chunk):
        self.buf += chunk
        keep = []
        while True:
            nl = self.buf.find(b"\n")
            if nl < 0:
                break
            line, self.buf = self.buf[:nl + 1], self.buf[nl + 1:]
            keep.append(self._route(line))
        return b"".join(keep)

    def flush(self):
        rest, self.buf = self.buf, b""
        return self._route(rest) if rest else b""

    def _route(self, line):
        text = line.rstrip(b"\r\n")
        if self.close_line is not None:
            if text == self.close_line:
                self.close_line = None
            self.run.log(DEBUG_PREFIX + b" " + text + b"\n")
            return b""
        if line.startswith(DEBUG_PREFIX):
            opener = text[len(DEBUG_PREFIX):].strip()
            if opener in (b"{", b"["):
                self.close_line = b"}" if opener == b"{" else b"]"
            self.run.log(text + b"\n")
            return b""
        return line
