"""CLI executors (pi) under sandbox-runtime (srt): backend choice,
spawn paths, stderr split, per-executor domains and registry presets.
srt itself is never run: a fake `srt` script stands in where a process is
spawned, everything else is argv/spec level."""

import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import threading
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_exec  # noqa: E402
import agent_exec_sandbox as sb  # noqa: E402
import agent_exec_srt  # noqa: E402

OK = {"ok": True, "detail": "ok"}
NO = {"ok": False, "detail": "no"}
SEATBELT = {"seatbelt": OK, "bwrap": NO, "landlock": dict(NO, abi=0)}


def _git(cwd, *args):
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True)


class _StateBase(unittest.TestCase):
    def setUp(self):
        self.tmp = os.path.realpath(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)
        patcher = mock.patch.dict(os.environ, {"XDG_STATE_HOME": os.path.join(self.tmp, "x")})
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop(sb.FORCE_ENV, None)
        self.tree = os.path.join(self.tmp, "tree")
        os.makedirs(self.tree)

    def network(self, srt="/opt/srt", domains=("api.anthropic.com",), reason=""):
        return {"srt": srt, "reason": reason, "domains": list(domains), "id": "t1",
                "session": "t1"}


class BackendTests(_StateBase):
    def test_srt_replaces_the_os_backend(self):
        spec = sb.prepare("pi", self.tree, {"mode": "auto"},
                          probe_results=SEATBELT, network=self.network())
        self.assertEqual(spec["backend"], "srt")
        argv = sb.wrap_argv(["/bin/x", "-p"], self.tree, spec)
        self.assertEqual(argv[:5], ["/opt/srt", "-d", "-s",
                                    spec["srt"]["settings_path"], "--control-fd"])
        self.assertEqual(argv[-2:], ["-c", "/bin/x -p"])
        self.assertNotIn(sb.SANDBOX_EXEC, argv)
        self.assertNotIn("bwrap", argv)
        self.assertNotIn("_sandbox-exec", argv)
        settings = spec["srt"]["settings"]
        self.assertIs(settings["allowPty"], False)
        self.assertEqual(settings["network"]["allowUnixSockets"], [])

    def test_without_srt_the_fallback_is_named(self):
        for label, probes, network, why, backend in (
                ("srt missing", SEATBELT, self.network(srt=None, reason="srt not found"),
                 "srt not found", "seatbelt"),
                ("no seatbelt/bwrap", {"seatbelt": NO, "bwrap": NO,
                                       "landlock": {"ok": True, "abi": 6, "detail": ""}},
                 self.network(), "srt needs a working Seatbelt or bubblewrap", "landlock")):
            with self.subTest(label=label):
                spec = sb.prepare("pi", self.tree, {"mode": "auto"},
                                  probe_results=probes, network=network)
                self.assertEqual(spec["backend"], backend)
                self.assertTrue(any(n.startswith("network not restricted: " + why)
                                    for n in spec["limits"]), spec["limits"])
                self.assertIn("network not restricted", spec["reason"])


class SpawnTests(_StateBase):
    """A fake srt: writes debug lines (one of them srt's multi-line JSON
    config dump) to stderr, checks fd 3 is readable, then runs the -c
    command like srt would (shared stdio)."""

    def fake_srt(self):
        path = os.path.join(self.tmp, "srt")
        with open(path, "w") as fh:
            fh.write(textwrap.dedent("""\
                #!/bin/sh
                for last; do :; done
                [ -r /dev/fd/3 ] || { echo "control fd missing" >&2; exit 9; }
                printf '[SandboxDebug] {\\n  "allowedHosts": [\\n    "a.example"\\n  ]\\n}\\n' >&2
                echo "[SandboxDebug] Connection blocked to example.com:443" >&2
                echo "[SandboxDebug] Connection blocked to example.com:443" >&2
                echo "[SandboxDebug] Connection blocked to mcp-proxy.anthropic.com:443" >&2
                exec /bin/sh -c "$last"
                """))
        os.chmod(path, 0o755)
        return path

    def test_debug_lines_go_to_the_log_not_to_the_answer(self):
        spec = sb.prepare("pi", self.tree, {"mode": "auto"}, probe_results=SEATBELT,
                          network=self.network(srt=self.fake_srt()))
        spec["srt"]["quiet"] = ["mcp-proxy.anthropic.com"]
        child = "import sys; print('{\"answer\": 1}'); sys.stderr.write('real error\\n')"
        network = []
        code, out, err = agent_exec._spawn_capture(
            [sys.executable, "-c", child], cwd=self.tree, env=None, input_text="",
            sandbox=spec, network_result=network)
        self.assertEqual(code, 0, err)
        self.assertEqual(out, '{"answer": 1}\n')
        self.assertEqual(err, "real error\n")
        self.assertEqual(network, [[{"host": "example.com", "port": 443}]])
        with open(spec["srt"]["log_path"]) as fh:
            self.assertEqual(fh.read().count("[SandboxDebug]"), 8)
        audit = os.path.join(sb.state_dir(), "audit", "network.jsonl")
        with open(audit) as fh:
            records = [json.loads(line) for line in fh]
        self.assertEqual([(r["session"], r["host"]) for r in records],
                         [("t1", "example.com")] * 2 + [("t1", "mcp-proxy.anthropic.com")])


    def test_failed_pi_run_carries_its_stderr_tail_without_srt_lines(self):
        spec = sb.prepare("pi", self.tree, {"mode": "auto"}, probe_results=SEATBELT,
                          network=self.network(srt=self.fake_srt()))
        pi = os.path.join(self.tmp, "pi")
        with open(pi, "w") as fh:
            fh.write(textwrap.dedent("""\
                #!/bin/sh
                i=1
                while [ $i -le 30 ]; do echo "pi err $i" >&2; i=$((i+1)); done
                exit 1
                """))
        os.chmod(pi, 0o755)
        with mock.patch.object(agent_exec, "_resolve_tool", return_value={"path": pi}):
            code, result = agent_exec._run_pi_capture(
                "pi", "m", None, self.tree, "hi", None, sandbox=spec)
        self.assertEqual((code, result["exit_code"]), (0, 1))
        tail = result["stderr_tail"].splitlines()
        self.assertEqual(tail, ["pi err %d" % i for i in range(11, 31)])
        self.assertNotIn("SandboxDebug", result["stderr_tail"])
        self.assertNotIn("allowedHosts", result["stderr_tail"])

    def test_stderr_tail_is_bounded_and_absent_on_success_or_without_srt(self):
        long_line = "x" * 5000
        tail = agent_exec._stderr_tail("[SandboxDebug] a\n\n" + long_line + "\n")
        self.assertEqual(len(tail), agent_exec._STDERR_TAIL_CHARS)
        for sandbox, code in (({"backend": "srt"}, 0), ({"backend": "seatbelt"}, 1),
                              (None, 1)):
            result = {}
            agent_exec._note_srt_failure(result, sandbox, code, "err\n")
            self.assertNotIn("stderr_tail", result)
        result = {}
        agent_exec._note_srt_failure(result, {"backend": "srt"}, 1, "[SandboxDebug] x\n")
        self.assertNotIn("stderr_tail", result)


class SpawnPathTests(_StateBase):
    """Every spawn path wraps with srt when it applies."""

    def setUp(self):
        _StateBase.setUp(self)
        self.prompt = os.path.join(self.tmp, "p.md")
        with open(self.prompt, "w") as f:
            f.write("hi")
        cfg = agent_exec.copy.deepcopy(agent_exec.DEFAULTS)
        cfg["ledger"]["enabled"] = False
        cfg["cooldown"]["path"] = os.path.join(self.tmp, "state.json")
        for p in (
                mock.patch.object(agent_exec, "resolve_config", return_value=(cfg, None)),
                mock.patch.object(agent_exec, "_build_doctor_report", return_value={}),
                mock.patch.object(agent_exec, "resolve_route", return_value={
                    "executor": "pi", "dispatch": "cli", "model": "m",
                    "effort": "low", "agent_type": None}),
                mock.patch.object(agent_exec_srt, "srt_status",
                                  return_value=("/opt/srt", "1", "")),
                mock.patch.object(sb, "probes", return_value=SEATBELT),
                mock.patch.object(agent_exec.shutil, "which", return_value="/bin/pi")):
            p.start()
            self.addCleanup(p.stop)

    def test_dryrun_previews_srt(self):
        buf = io.StringIO()
        with mock.patch.dict(os.environ, {"AGENT_EXEC_DRYRUN": "1"}), \
                mock.patch.object(sys, "stdout", buf):
            agent_exec.cmd_dispatch_route([
                "--class", "standard", "--prompt-file", self.prompt,
                "--workdir", self.tree, "--isolate", "never", "--task", "t1"])
        exec_line = [line for line in buf.getvalue().splitlines()
                     if line.startswith("EXEC: ")][0]
        self.assertTrue(exec_line.startswith("EXEC: /opt/srt -d -s "), exec_line)
        self.assertIn("cli-srt/t1.json", exec_line)
        self.assertNotIn(sb.SANDBOX_EXEC, exec_line)

    def test_exec_paths_exec_srt(self):
        for label, call in (
                ("run", lambda: agent_exec.cmd_run([
                    "pi", "--model", "m", "--effort", "low", "--workdir", self.tree,
                    "--prompt-file", self.prompt])),
                ("passthrough", lambda: agent_exec.cmd_dispatch(
                    "pi", ["-p", "hi"]))):
            with self.subTest(label):
                seen = {}
                with mock.patch.object(agent_exec.os, "getcwd", return_value=self.tree), \
                        mock.patch.object(agent_exec_srt, "exec_prepare",
                                          side_effect=lambda spec: seen.update(prepared=spec)), \
                        mock.patch.object(agent_exec.os, "execvpe",
                                          side_effect=lambda f, a, e: seen.update(argv=a)):
                    call()
                self.assertEqual(seen["argv"][0], "/opt/srt")
                self.assertEqual(seen["prepared"]["backend"], "srt")
                self.assertNotIn(sb.SANDBOX_EXEC, seen["argv"])


class ControlFifoTests(_StateBase):
    """The herdr server's control FIFO, pushed to by any later process."""

    def setUp(self):
        _StateBase.setUp(self)
        self.path = os.path.join(sb.state_dir(), sb.SRT_SETTINGS_DIR, "s.ctl")

    def test_a_long_line_arrives_whole(self):
        fd = agent_exec_srt.make_fifo(self.path)  # srt's descriptor
        self.addCleanup(os.close, fd)
        line = (json.dumps({"pad": "x" * (1 << 18)}) + "\n").encode()
        got = []

        def read():
            buf = b""
            while not buf.endswith(b"\n"):
                buf += os.read(fd, 1 << 16)
            got.append(buf)
        reader = threading.Thread(target=read)
        reader.start()
        self.assertIsNone(agent_exec_srt.push_line(self.path, line))
        reader.join(10)
        self.assertEqual(got, [line])
        self.assertEqual(os.stat(self.path).st_mode & 0o777, 0o600)

    def test_no_reader_or_no_fifo_is_reported_without_blocking(self):
        os.makedirs(os.path.dirname(self.path))
        os.mkfifo(self.path)
        self.assertIn("nothing reads", agent_exec_srt.push_line(self.path, b"{}\n"))
        os.unlink(self.path)
        self.assertIn("no control channel", agent_exec_srt.push_line(self.path, b"{}\n"))
        open(self.path, "w").close()
        self.assertIn("not a FIFO", agent_exec_srt.push_line(self.path, b"{}\n"))

    def test_push_is_skipped_when_unchanged_and_stale_without_a_reader(self):
        settings = {"network": {"allowedDomains": ["a.example.com"]}}
        fd = agent_exec_srt.make_fifo(self.path)
        os.set_blocking(fd, False)
        run = agent_exec_srt.SrtRun(settings, ["a.example.com"],
                                    self.path[:-4] + ".json", control_path=self.path)
        disk = run.prepare()
        run.push()
        with self.assertRaises(BlockingIOError):
            os.read(fd, 1 << 16)  # unchanged vs the file on disk: nothing written
        sb.add_domain("b.example.com")
        run.push()
        pushed = json.loads(os.read(fd, 1 << 16))
        self.assertEqual(pushed["network"]["allowedDomains"], ["a.example.com", "b.example.com"])
        with open(disk) as fh:
            self.assertEqual(json.load(fh), pushed)
        os.close(fd)  # srt is gone
        sb.add_domain("c.example.com")
        with mock.patch("sys.stderr", io.StringIO()):
            run.push()
        self.assertIn("nothing reads", run.stale)
        with open(disk) as fh:
            self.assertEqual(json.load(fh), pushed)  # not claimed as applied


class DomainTests(_StateBase):
    def plan(self, executor, model=None, cfg=None):
        cfg = cfg or agent_exec.copy.deepcopy(agent_exec.DEFAULTS)
        return agent_exec_srt.cli_network(cfg, executor, self.tree, model, "t1",
                                          binary="/opt/srt", platform="darwin")

    def test_api_hosts_stay_with_their_executor(self):
        chatgpt = self.plan("pi", "openai-codex/gpt-5.6-luna")["domains"]
        pi = self.plan("pi", "anthropic/claude-x")["domains"]
        self.assertIn("chatgpt.com", chatgpt)
        self.assertNotIn("chatgpt.com", pi)
        self.assertNotIn("api.openai.com", pi)
        self.assertEqual(self.plan("pi", "unknown/x")["domains"], ["api.anthropic.com"])

    def test_presets_count_tracked_files_only_and_filter_npmrc_hosts(self):
        _git(self.tree, "init", "-q")
        files = {
            "package.json": "{}",
            "sub/requirements-dev.txt": "",
            ".gitignore": "Cargo.toml\n",
            "Cargo.toml": "",          # ignored
            "go.mod": "module x\n",    # untracked
            ".npmrc": ("registry=https://npm.example.com/\n"
                       "@x:registry=http://127.0.0.1:4873/\n"
                       "@y:registry = https://localhost/\n"
                       "# registry=https://commented.example.com/\n"),
        }
        for name, text in files.items():
            os.makedirs(os.path.dirname(os.path.join(self.tree, name)), exist_ok=True)
            with open(os.path.join(self.tree, name), "w") as fh:
                fh.write(text)
        _git(self.tree, "add", "package.json", "sub/requirements-dev.txt", ".gitignore",
             ".npmrc")
        self.assertEqual(agent_exec_srt.ecosystem_domains(self.tree),
                         ["registry.npmjs.org", "pypi.org", "files.pythonhosted.org",
                          "npm.example.com"])
        self.assertIn("registry.npmjs.org", self.plan("pi", "anthropic/claude-x")["domains"])
        cfg = agent_exec.copy.deepcopy(agent_exec.DEFAULTS)
        cfg["sandbox"]["network"]["ecosystem"] = False
        domains = self.plan("pi", "anthropic/claude-x", cfg=cfg)["domains"]
        self.assertNotIn("registry.npmjs.org", domains)
        self.assertNotIn("npm.example.com", domains)


if __name__ == "__main__":
    unittest.main()
