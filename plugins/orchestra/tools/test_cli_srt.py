"""CLI executors (pi, codex) under sandbox-runtime (srt): backend choice,
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
    def test_srt_replaces_the_os_backend_and_codex_turns_its_own_sandbox_off(self):
        for executor in ("pi", "codex"):
            with self.subTest(executor):
                spec = sb.prepare(executor, self.tree, {"mode": "auto"},
                                  probe_results=SEATBELT, network=self.network())
                self.assertEqual(spec["backend"], "srt")
                argv = sb.wrap_argv(["/bin/x", "-p"], self.tree, spec)
                self.assertEqual(argv[:5], ["/opt/srt", "-d", "-s",
                                            spec["srt"]["settings_path"], "--control-fd"])
                self.assertEqual(argv[-2:], ["-c", "/bin/x -p"])
                self.assertNotIn(sb.SANDBOX_EXEC, argv)
                self.assertNotIn("bwrap", argv)
                self.assertNotIn("_sandbox-exec", argv)
        spec = sb.prepare("codex", self.tree, {"mode": "auto"},
                          probe_results=SEATBELT, network=self.network())
        for resume in (None, "SID"):
            argv = agent_exec._build_executor_argv(
                "codex", "codex", "m", None, self.tree, "p", resume, "json", sandbox=spec)
            self.assertNotIn("workspace-write", " ".join(argv))
            self.assertIn("danger-full-access", " ".join(argv))
        settings = spec["srt"]["settings"]
        self.assertIs(settings["allowPty"], False)
        self.assertEqual(settings["network"]["allowUnixSockets"], [])

    def test_without_srt_codex_keeps_its_sandbox_and_the_fallback_is_named(self):
        for label, probes, network, why in (
                ("srt missing", SEATBELT, self.network(srt=None, reason="srt not found"),
                 "srt not found"),
                ("no seatbelt/bwrap", {"seatbelt": NO, "bwrap": NO,
                                       "landlock": {"ok": True, "abi": 6, "detail": ""}},
                 self.network(), "srt needs a working Seatbelt or bubblewrap")):
            for executor, backend in (("codex", "codex-native"),
                                      ("pi", "seatbelt" if probes is SEATBELT else "landlock")):
                with self.subTest(label=label, executor=executor):
                    spec = sb.prepare(executor, self.tree, {"mode": "auto"},
                                      probe_results=probes, network=network)
                    self.assertEqual(spec["backend"], backend)
                    self.assertTrue(any(n.startswith("network not restricted: " + why)
                                        for n in spec["limits"]), spec["limits"])
                    self.assertIn("network not restricted", spec["reason"])
                    argv = agent_exec._build_executor_argv(
                        executor, executor, "m", None, self.tree, "p", None, "json",
                        sandbox=spec)
                    if executor == "codex":
                        self.assertIn("workspace-write", argv)
                        self.assertNotIn("danger-full-access", argv)
                        self.assertEqual(sb.wrap_argv(argv, self.tree, spec), argv)


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


    def test_failed_codex_run_carries_its_stderr_tail_without_srt_lines(self):
        spec = sb.prepare("codex", self.tree, {"mode": "auto"}, probe_results=SEATBELT,
                          network=self.network(srt=self.fake_srt()))
        codex = os.path.join(self.tmp, "codex")
        with open(codex, "w") as fh:
            fh.write(textwrap.dedent("""\
                #!/bin/sh
                i=1
                while [ $i -le 30 ]; do echo "codex err $i" >&2; i=$((i+1)); done
                echo '{"type":"turn.failed","error":{"message":"boom"}}'
                exit 1
                """))
        os.chmod(codex, 0o755)
        with mock.patch.object(agent_exec, "_resolve_tool", return_value={"path": codex}):
            code, result = agent_exec._run_codex_capture(
                "codex", "m", None, self.tree, "hi", None, sandbox=spec)
        self.assertEqual((code, result["exit_code"]), (0, 1))
        tail = result["stderr_tail"].splitlines()
        self.assertEqual(tail, ["codex err %d" % i for i in range(11, 31)])
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


class SrtChildEnvTests(unittest.TestCase):
    """codex under srt on macOS cannot reach trustd: it is handed a PEM root
    bundle unless the user already chose a CA file."""

    def setUp(self):
        fd, self.bundle = tempfile.mkstemp(suffix=".pem")
        os.close(fd)
        self.addCleanup(os.unlink, self.bundle)
        self.srt = {"backend": "srt", "policy": {}}

    def env(self, spec, base, platform="darwin", bundle=None):
        return sb.child_env(spec, dict(base, TMPDIR="/tmp"), platform=platform,
                            ca_bundle=bundle or self.bundle)

    def test_srt_on_macos_points_codex_at_the_bundle(self):
        self.assertEqual(self.env(self.srt, {})["CODEX_CA_CERTIFICATE"], self.bundle)

    def test_the_bundle_is_not_set_elsewhere_or_over_a_user_choice(self):
        for label, spec, base, platform, bundle in (
                ("linux", self.srt, {}, "linux", None),
                ("not srt", {"backend": "seatbelt", "policy": {}}, {}, "darwin", None),
                ("missing bundle", self.srt, {}, "darwin", self.bundle + ".none"),
                ("user SSL_CERT_FILE", self.srt, {"SSL_CERT_FILE": "/u.pem"}, "darwin", None)):
            with self.subTest(label):
                self.assertNotIn("CODEX_CA_CERTIFICATE",
                                 self.env(spec, base, platform, bundle))
        env = self.env(self.srt, {"CODEX_CA_CERTIFICATE": "/mine.pem"})
        self.assertEqual(env["CODEX_CA_CERTIFICATE"], "/mine.pem")

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
                    "executor": "codex", "dispatch": "cli", "model": "m",
                    "effort": "low", "agent_type": None}),
                mock.patch.object(agent_exec_srt, "srt_status",
                                  return_value=("/opt/srt", "1", "")),
                mock.patch.object(sb, "probes", return_value=SEATBELT),
                mock.patch.object(agent_exec.shutil, "which", return_value="/bin/codex")):
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
        self.assertIn("danger-full-access", exec_line)
        self.assertNotIn(sb.SANDBOX_EXEC, exec_line)

    def test_exec_paths_exec_srt(self):
        for label, call in (
                ("run", lambda: agent_exec.cmd_run([
                    "codex", "--model", "m", "--effort", "low", "--workdir", self.tree,
                    "--prompt-file", self.prompt])),
                ("passthrough", lambda: agent_exec.cmd_dispatch(
                    "codex", ["exec", "--sandbox", "workspace-write", "hi"]))):
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
                self.assertIn("danger-full-access", seen["argv"][-1])
                self.assertNotIn("workspace-write", seen["argv"][-1])
                self.assertNotIn(sb.SANDBOX_EXEC, seen["argv"])


class DomainTests(_StateBase):
    def plan(self, executor, model=None, cfg=None):
        cfg = cfg or agent_exec.copy.deepcopy(agent_exec.DEFAULTS)
        return agent_exec_srt.cli_network(cfg, executor, self.tree, model, "t1",
                                          binary="/opt/srt", platform="darwin")

    def test_api_hosts_stay_with_their_executor(self):
        codex = self.plan("codex")["domains"]
        pi = self.plan("pi", "anthropic/claude-x")["domains"]
        self.assertIn("chatgpt.com", codex)
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
        self.assertIn("registry.npmjs.org", self.plan("codex")["domains"])
        cfg = agent_exec.copy.deepcopy(agent_exec.DEFAULTS)
        cfg["sandbox"]["network"]["ecosystem"] = False
        domains = self.plan("codex", cfg=cfg)["domains"]
        self.assertNotIn("registry.npmjs.org", domains)
        self.assertNotIn("npm.example.com", domains)


if __name__ == "__main__":
    unittest.main()
