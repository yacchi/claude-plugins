# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
import json
import os
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

import agent_exec


ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "tools" / "agent_exec.py"


class DispatchTokenTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.home = self.root / "home"
        self.home.mkdir()
        self.contract = self.root / "contract.md"
        self.contract.write_text("DISTINCT CONTRACT SENTENCE\n", encoding="utf-8")
        # `--detach` re-executes whichever `agent-exec` is first on PATH; put
        # this checkout's own first so the child runs the code under test, not
        # an installed copy.
        self.env = dict(
            os.environ, HOME=str(self.home),
            PATH=str(SCRIPT.parent) + os.pathsep + os.environ.get("PATH", ""))

    def tearDown(self):
        self.temp.cleanup()

    def run_cli(self, *args):
        return subprocess.run(
            [sys.executable, str(SCRIPT), *args],
            cwd=self.root,
            env=self.env,
            text=True,
            capture_output=True,
        )

    def prepare(self):
        result = self.run_cli(
            "dispatch", "prepare", "--class", "standard",
            "--prompt-file", str(self.contract), "--workdir", str(self.root),
            "--isolate", "never", "--json",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)["token"]

    def token_dir(self):
        return self.home / ".claude" / "orchestra" / "tokens"

    def test_prepare_mints_opaque_tokens_without_payload(self):
        first = self.prepare()
        second = self.prepare()
        self.assertRegex(first, r"^dsp-[0-9a-f]{12}$")
        self.assertRegex(second, r"^dsp-[0-9a-f]{12}$")
        self.assertNotEqual(first, second)
        self.assertEqual(stat.S_IMODE(self.token_dir().stat().st_mode), 0o700)
        self.assertNotIn(
            b"DISTINCT CONTRACT SENTENCE",
            b"".join(path.read_bytes() for path in self.token_dir().glob("*.json")),
        )

    def test_prepare_rejects_invalid_inputs_without_spec(self):
        cases = [
            ("--prompt-file", str(self.contract), "--workdir", str(self.root)),
            ("--class", "standard", "--prompt-file", str(self.contract)),
            ("--class", "standard", "--workdir", str(self.root)),
        ]
        for extra in cases:
            result = self.run_cli("dispatch", "prepare", *extra)
            self.assertEqual(result.returncode, 2)
            self.assertEqual(result.stdout, "")
        self.assertFalse(self.token_dir().exists())

    def test_prepare_accepts_a_prompt_file_that_does_not_exist_yet(self):
        """A correction round's feedback file is written between prepare and
        dispatch, so prepare must not stat the prompt files."""
        missing = self.root / "feedback-not-written-yet.md"
        result = self.run_cli(
            "dispatch", "prepare", "--class", "standard",
            "--prompt-file", str(missing), "--workdir", str(self.root),
            "--isolate", "never", "--json",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        token = json.loads(result.stdout)["token"]
        self.assertRegex(token, r"^dsp-[0-9a-f]{12}$")

        # Existence is enforced at dispatch time, with the path named.
        failed = self.run_cli("dispatch", "--token", token, "--capture")
        self.assertEqual(failed.returncode, 2)
        self.assertIn(str(missing), failed.stderr)
        self.assertEqual(failed.stdout, "")

        # ... and the same token works once the file lands.
        missing.write_text("late contract\n", encoding="utf-8")
        ok = self.run_cli("dispatch", "--token", token, "--capture")
        self.assertEqual(ok.returncode, 0, ok.stderr)
        self.assertIn("status", json.loads(ok.stdout))

    def test_token_dispatch_matches_direct_and_is_reusable(self):
        token = self.prepare()
        direct = self.run_cli(
            "dispatch", "--class", "standard", "--prompt-file", str(self.contract),
            "--workdir", str(self.root), "--isolate", "never", "--capture",
        )
        by_token = self.run_cli(
            "dispatch", "--token", token, "--capture",
        )
        self.assertEqual(direct.returncode, 0)
        self.assertEqual(by_token.returncode, 0)
        self.assertEqual(json.loads(by_token.stdout), json.loads(direct.stdout))
        again = self.run_cli("dispatch", "--token", token, "--capture")
        self.assertEqual(again.returncode, 0)
        self.assertTrue((self.token_dir() / (token + ".json")).exists())

    def test_token_conflicts_and_exhausted(self):
        token = self.prepare()
        for flag, value in (
            ("--prompt-file", str(self.contract)),
            ("--workdir", str(self.root)), ("--archetype", "default"),
            ("--run-id", "run-1"), ("--isolate", "never"), ("--task", "task-1"),
        ):
            result = self.run_cli("dispatch", "--token", token, flag, value)
            self.assertEqual(result.returncode, 2)
            self.assertIn(flag, result.stderr)
        result = self.run_cli("dispatch", "--token", token, "--exhausted", "claude")
        self.assertEqual(result.returncode, 0)
        output = json.loads(result.stdout)
        self.assertIn(output["status"], ("delegate", "unroutable"))
        self.assertTrue(any(
            item["executor"] == "claude" and item["reason"] == "exhausted"
            for item in output["route"]["skipped"]
        ))

    def test_class_may_escalate_a_token(self):
        # --class alongside --token overrides the token's own class (e.g. a
        # correction round escalating light -> deep); the token itself still
        # supplies everything else (prompt files, workdir, isolate mode).
        token = self.prepare()
        result = self.run_cli("dispatch", "--token", token, "--class", "deep", "--exhausted", "claude")
        self.assertEqual(result.returncode, 0)
        output = json.loads(result.stdout)
        self.assertEqual(output["route"]["class"], "deep")

    def test_detach_and_wait_matches_a_synchronous_dispatch(self):
        token = self.prepare()
        direct = self.run_cli("dispatch", "--token", token, "--capture")
        self.assertEqual(direct.returncode, 0, direct.stderr)

        token2 = self.prepare()
        detached = self.run_cli("dispatch", "--token", token2, "--capture", "--detach")
        self.assertEqual(detached.returncode, 0, detached.stderr)
        detached_out = json.loads(detached.stdout)
        self.assertEqual(detached_out["status"], "detached")
        self.assertIn("pid", detached_out)

        waited = self.run_cli("dispatch", "wait", "--token", token2, "--max-wait", "20")
        self.assertEqual(waited.returncode, 0, waited.stderr)
        self.assertEqual(json.loads(waited.stdout), json.loads(direct.stdout))

        # The result is persisted: a second `wait` returns it unchanged.
        again = self.run_cli("dispatch", "wait", "--token", token2)
        self.assertEqual(again.returncode, 0, again.stderr)
        self.assertEqual(json.loads(again.stdout), json.loads(direct.stdout))

    def test_detach_is_idempotent_while_the_child_is_running(self):
        # A slow-executing detached run must report "running" (not start a
        # second child, not error) if the relay's `--detach` call is retried.
        token = self.prepare()
        first = self.run_cli("dispatch", "--token", token, "--capture", "--detach")
        self.assertEqual(first.returncode, 0, first.stderr)
        first_out = json.loads(first.stdout)
        self.assertEqual(first_out["status"], "detached")

        second = self.run_cli("dispatch", "--token", token, "--capture", "--detach")
        self.assertEqual(second.returncode, 0, second.stderr)
        second_out = json.loads(second.stdout)
        self.assertIn(second_out["status"], ("running", "done"))
        if second_out["status"] == "running":
            self.assertEqual(second_out["pid"], first_out["pid"])

        # Drain it either way so the test does not leak a background process.
        self.run_cli("dispatch", "wait", "--token", token, "--max-wait", "20")

    def test_detach_after_collect_is_a_fresh_redispatch(self):
        token = self.prepare()
        first = self.run_cli("dispatch", "--token", token, "--capture", "--detach")
        self.assertEqual(json.loads(first.stdout)["status"], "detached")
        first_pid = json.loads(first.stdout)["pid"]
        first_result = self.run_cli("dispatch", "wait", "--token", token, "--max-wait", "20")
        self.assertEqual(first_result.returncode, 0, first_result.stderr)

        second = self.run_cli(
            "dispatch", "--token", token, "--capture", "--detach", "--exhausted", "claude")
        self.assertEqual(second.returncode, 0, second.stderr)
        second_out = json.loads(second.stdout)
        self.assertEqual(second_out["status"], "detached")

        # (3) fresh state: no leftover finished/collected, wait_calls reset.
        state = json.loads(
            (self.detach_dir() / (token + ".json")).read_text(encoding="utf-8"))
        self.assertEqual(state["pid"], second_out["pid"])
        self.assertEqual(state["wait_calls"], 0)
        self.assertNotIn("collected", state)
        self.assertNotIn("finished", state)

        waited = self.run_cli("dispatch", "wait", "--token", token, "--max-wait", "20")
        self.assertEqual(waited.returncode, 0, waited.stderr)
        direct = self.run_cli("dispatch", "--token", token, "--capture", "--exhausted", "claude")
        self.assertEqual(json.loads(waited.stdout), json.loads(direct.stdout))
        self.assertNotEqual(json.loads(waited.stdout), json.loads(first_result.stdout))
        self.assertIsInstance(first_pid, int)

    def test_detach_after_exit_without_collect_reports_done_and_keeps_result(self):
        token = self.prepare()
        first = self.run_cli("dispatch", "--token", token, "--capture", "--detach")
        self.assertEqual(json.loads(first.stdout)["status"], "detached")
        pid = json.loads(first.stdout)["pid"]
        # Let the child exit without any `wait` collecting it.
        deadline = time.time() + 20
        while agent_exec._pid_alive(pid) and time.time() < deadline:
            try:
                os.waitpid(pid, os.WNOHANG)
            except OSError:
                pass
            time.sleep(0.1)
        again = self.run_cli(
            "dispatch", "--token", token, "--capture", "--detach", "--exhausted", "claude")
        self.assertEqual(json.loads(again.stdout), {"status": "done", "token": token})
        waited = self.run_cli("dispatch", "wait", "--token", token, "--max-wait", "20")
        direct = self.run_cli("dispatch", "--token", token, "--capture")
        self.assertEqual(json.loads(waited.stdout), json.loads(direct.stdout))

    def test_wait_without_a_detached_run_is_unknown_token(self):
        token = self.prepare()
        result = self.run_cli("dispatch", "wait", "--token", token)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            json.loads(result.stdout), {"status": "unknown-token", "token": token})

    def test_wait_with_malformed_token_is_a_usage_error(self):
        result = self.run_cli("dispatch", "wait", "--token", "nope")
        self.assertEqual(result.returncode, 2)

    # --- fake detached children -------------------------------------------

    def detach_dir(self):
        path = self.token_dir() / "detach"
        path.mkdir(parents=True, exist_ok=True)
        return path

    def fake_detached(self, token, out_text, running=False):
        """Write a detach state file + captured output as if `--detach` ran."""
        if running:
            proc = subprocess.Popen(["sleep", "30"])
            self.addCleanup(proc.wait)
            self.addCleanup(proc.kill)
        else:
            proc = subprocess.Popen(["true"])
            proc.wait()
        out = self.detach_dir() / (token + ".out")
        out.write_text(out_text, encoding="utf-8")
        state = self.detach_dir() / (token + ".json")
        state.write_text(json.dumps({
            "pid": proc.pid, "token": token, "started": 0, "out": str(out),
        }), encoding="utf-8")
        return state

    def test_wait_is_idempotent_and_keeps_state(self):
        token = "dsp-aaaaaaaaaaaa"
        state = self.fake_detached(token, json.dumps({"status": "ok", "answer": "hi"}))
        first = self.run_cli("dispatch", "wait", "--token", token, "--max-wait", "0")
        second = self.run_cli("dispatch", "wait", "--token", token, "--max-wait", "0")
        self.assertEqual(first.returncode, 0, first.stderr)
        self.assertEqual(first.stdout, second.stdout)
        self.assertEqual(json.loads(first.stdout)["status"], "ok")
        saved = json.loads(state.read_text(encoding="utf-8"))
        self.assertIs(saved["collected"], True)
        self.assertEqual(saved["wait_calls"], 2)
        self.assertIn("finished", saved)
        self.assertTrue((self.detach_dir() / (token + ".result.json")).exists())

    def test_wait_summary_truncates_a_long_japanese_answer(self):
        token = "dsp-bbbbbbbbbbbb"
        answer = "\n".join("行%d: これは長い回答です。" % n for n in range(100))[:1000]
        self.assertEqual(len(answer), 1000)
        self.fake_detached(token, json.dumps({"status": "ok", "answer": answer, "x": 1}))
        result = self.run_cli(
            "dispatch", "wait", "--token", token, "--max-wait", "0", "--summary")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(result.stdout.splitlines()), 1)
        self.assertTrue(result.stdout.endswith("\n"))
        data = json.loads(result.stdout)
        self.assertEqual(len(data["answer"]), 400)
        self.assertEqual(data["answer"], answer[:400])
        self.assertIs(data["answer_truncated"], True)
        self.assertEqual(data["x"], 1)
        self.assertEqual(
            Path(data["answer_path"]).read_text(encoding="utf-8"), answer)

    def test_wait_summary_short_answer_is_not_truncated(self):
        token = "dsp-cccccccccccc"
        self.fake_detached(token, json.dumps({"status": "ok", "answer": "short"}))
        result = self.run_cli(
            "dispatch", "wait", "--token", token, "--max-wait", "0", "--summary")
        data = json.loads(result.stdout)
        self.assertEqual(data["answer"], "short")
        self.assertNotIn("answer_truncated", data)
        self.assertEqual(Path(data["answer_path"]).read_text(encoding="utf-8"), "short")

    def test_wait_reports_running_with_counters(self):
        token = "dsp-dddddddddddd"
        self.fake_detached(token, "", running=True)
        result = self.run_cli("dispatch", "wait", "--token", token, "--max-wait", "0")
        self.assertEqual(result.returncode, 0, result.stderr)
        data = json.loads(result.stdout)
        self.assertEqual(data["status"], "running")
        self.assertEqual(data["token"], token)
        self.assertIn("pid", data)
        self.assertIsInstance(data["waited_s"], int)
        self.assertEqual(data["wait_calls"], 1)
        again = json.loads(self.run_cli(
            "dispatch", "wait", "--token", token, "--max-wait", "0").stdout)
        self.assertEqual(again["wait_calls"], 2)

    def test_wait_unparsable_output_is_a_persisted_relay_failure(self):
        token = "dsp-eeeeeeeeeeee"
        self.fake_detached(token, "garbage")
        first = self.run_cli("dispatch", "wait", "--token", token, "--max-wait", "0")
        second = self.run_cli("dispatch", "wait", "--token", token, "--max-wait", "0")
        self.assertEqual(json.loads(first.stdout)["status"], "relay-failed")
        self.assertEqual(first.stdout, second.stdout)

    def test_list_detached_dispatches_reports_result_state(self):
        cfg = {"ledger": {"dir": str(self.home / ".claude" / "orchestra" / "runs")}}
        done = "dsp-111111111111"
        running = "dsp-222222222222"
        self.fake_detached(done, json.dumps({"status": "ok"}))
        self.fake_detached(running, "", running=True)
        before = {e["token"]: e for e in agent_exec.list_detached_dispatches(cfg)}
        self.assertTrue(before[done]["result_present"])
        self.assertFalse(before[done]["collected"])
        self.assertIsNone(before[done]["finished"])
        self.assertEqual(before[done]["wait_calls"], 0)
        self.assertFalse(before[running]["result_present"])
        self.run_cli("dispatch", "wait", "--token", done, "--max-wait", "0")
        after = {e["token"]: e for e in agent_exec.list_detached_dispatches(cfg)}
        self.assertEqual(len(after), 2)
        self.assertTrue(after[done]["result_present"])
        self.assertTrue(after[done]["collected"])
        self.assertIsInstance(after[done]["finished"], str)
        self.assertEqual(after[done]["wait_calls"], 1)

    def test_retention_removes_old_detach_files_for_dead_pids_only(self):
        cfg = {"ledger": {"dir": str(self.root / "runs"), "retention_days": 1}}
        directory = Path(agent_exec._detach_dir_from_cfg(cfg))
        directory.mkdir(parents=True)
        dead = subprocess.Popen(["true"])
        dead.wait()
        old_files = []
        for name in ("dsp-aaaaaaaaaaa1.json", "dsp-aaaaaaaaaaa1.result.json",
                     "dsp-aaaaaaaaaaa1.answer.txt"):
            path = directory / name
            path.write_text(json.dumps({"pid": dead.pid}), encoding="utf-8")
            os.utime(path, (0, 0))
            old_files.append(path)
        fresh = directory / "dsp-aaaaaaaaaaa2.result.json"
        fresh.write_text("{}", encoding="utf-8")
        live = directory / "dsp-aaaaaaaaaaa3.json"
        live.write_text(json.dumps({"pid": os.getpid()}), encoding="utf-8")
        os.utime(live, (0, 0))
        agent_exec._ledger_retention_ran = False
        agent_exec._sweep_retention(cfg)
        for path in old_files:
            self.assertFalse(path.exists(), path.name)
        self.assertTrue(fresh.exists())
        self.assertTrue(live.exists())

    def test_capture_summary_writes_answer_file(self):
        token = self.prepare()
        result = self.run_cli("dispatch", "--token", token, "--capture", "--summary")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(result.stdout.splitlines()), 1)
        self.assertIn("status", json.loads(result.stdout))

    def test_detach_requires_token(self):
        result = self.run_cli(
            "dispatch", "--class", "standard", "--prompt-file", str(self.contract),
            "--workdir", str(self.root), "--isolate", "never", "--detach",
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("--detach requires --token", result.stderr)

    def test_bad_tokens_never_escape_token_directory(self):
        outside = self.root / "outside"
        outside.write_text("sentinel", encoding="utf-8")
        for token in ("missing", "dsp-../../etc/passwd", "..", "", "dsp-00000000000a\n"):
            result = self.run_cli("dispatch", "--token", token)
            self.assertEqual(result.returncode, 2)
            self.assertIn(token, result.stderr)
            self.assertEqual(outside.read_text(encoding="utf-8"), "sentinel")
        self.assertFalse((self.root / "etc").exists())

    def test_corrupt_spec_is_rejected(self):
        token = self.prepare()
        (self.token_dir() / (token + ".json")).write_text("{", encoding="utf-8")
        result = self.run_cli("dispatch", "--token", token)
        self.assertEqual(result.returncode, 2)
        self.assertIn(token, result.stderr)

    def test_retention_sweeps_only_old_json_specs(self):
        cfg = {"ledger": {"dir": str(self.root / "runs"), "retention_days": 1}}
        directory = Path(agent_exec._token_dir_from_cfg(cfg))
        directory.mkdir(parents=True)
        old = directory / "dsp-000000000001.json"
        new = directory / "dsp-000000000002.json"
        other = directory / "keep.txt"
        old.write_text("{}", encoding="utf-8")
        new.write_text("{}", encoding="utf-8")
        other.write_text("keep", encoding="utf-8")
        os.utime(old, (0, 0))
        agent_exec._ledger_retention_ran = False
        agent_exec._sweep_retention(cfg)
        self.assertFalse(old.exists())
        self.assertTrue(new.exists())
        self.assertTrue(other.exists())

    def test_real_subprocess_prepare_then_dispatch(self):
        token = self.prepare()
        result = self.run_cli("dispatch", "--token", token, "--capture")
        self.assertEqual(result.returncode, 0)
        self.assertIn(json.loads(result.stdout)["status"], ("delegate", "unroutable"))


class ConfigRootTests(unittest.TestCase):
    LOCAL_YAML = (
        "external_executors:\n"
        "  pi:\n"
        "    enabled: false\n"
        "  codex:\n"
        "    enabled: false\n"
    )

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(os.path.realpath(self.temp.name))
        self.home = self.root / "home"
        self.home.mkdir()
        self.repo = self.root / "repo"
        self.outside = self.root / "outside"
        self.outside.mkdir()
        self.repo.mkdir()
        subprocess.run(["git", "init", "-q"], cwd=self.repo, check=True)
        self.contract = self.root / "contract.md"
        self.contract.write_text("CONTRACT\n", encoding="utf-8")
        self.env = dict(os.environ, HOME=str(self.home))
        self.env.pop("ORCHESTRA_CONFIG_ROOT", None)
        self._old_home = os.environ.get("HOME")
        self._old_cwd = os.getcwd()
        os.environ["HOME"] = str(self.home)
        os.environ.pop("ORCHESTRA_CONFIG_ROOT", None)

    def tearDown(self):
        os.chdir(self._old_cwd)
        if self._old_home is None:
            os.environ.pop("HOME", None)
        else:
            os.environ["HOME"] = self._old_home
        os.environ.pop("ORCHESTRA_CONFIG_ROOT", None)
        self.temp.cleanup()

    def write_layer(self, directory, name="orchestra.local.yaml", text=None):
        target = directory / ".claude"
        target.mkdir(parents=True, exist_ok=True)
        path = target / name
        path.write_text(self.LOCAL_YAML if text is None else text, encoding="utf-8")
        return path

    def cli(self, cwd, *args):
        return subprocess.run(
            [sys.executable, str(SCRIPT), *args],
            cwd=cwd, env=self.env, text=True, capture_output=True,
        )

    def prepare(self, cwd):
        result = self.cli(
            cwd, "dispatch", "prepare", "--class", "standard",
            "--prompt-file", str(self.contract), "--workdir", str(self.repo),
            "--isolate", "never", "--json",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_nested_cwd_finds_repo_root_layer(self):
        layer = self.write_layer(self.repo)
        nested = self.repo / "sub" / "dir"
        nested.mkdir(parents=True)
        os.chdir(nested)
        self.assertEqual(
            [os.path.realpath(p) for p in agent_exec._ordered_layer_paths()],
            [os.path.realpath(layer)],
        )
        resolved, err = agent_exec.resolve_config()
        self.assertIsNone(err)
        self.assertIs(resolved["external_executors"]["pi"]["enabled"], False)

    def test_nearest_layer_wins(self):
        self.write_layer(self.repo)
        inner = self.repo / "pkg"
        inner_layer = self.write_layer(inner, "orchestra.yaml", "tiers: {}\n")
        nested = inner / "src"
        nested.mkdir()
        os.chdir(nested)
        self.assertEqual(
            [os.path.realpath(p) for p in agent_exec._ordered_layer_paths()],
            [os.path.realpath(inner_layer)],
        )

    def test_no_layer_or_no_git_repo_uses_cwd(self):
        os.chdir(self.outside)
        self.assertEqual(agent_exec._ordered_layer_paths(), [])
        self.assertEqual(agent_exec._project_config_base(), str(self.outside))
        os.chdir(self.repo)
        self.assertEqual(agent_exec._ordered_layer_paths(), [])

    def test_prepare_stores_config_root_and_lists_layers(self):
        layer = self.write_layer(self.repo)
        nested = self.repo / "a"
        nested.mkdir()
        out = self.prepare(nested)
        self.assertEqual(
            [os.path.realpath(p) for p in out["config_layers"]],
            [os.path.realpath(layer)],
        )
        spec = json.loads(
            (self.home / ".claude" / "orchestra" / "tokens"
             / (out["token"] + ".json")).read_text(encoding="utf-8")
        )
        self.assertEqual(os.path.realpath(spec["config_root"]), str(self.repo))

    def test_prepare_without_layers_reports_empty_list(self):
        self.assertEqual(self.prepare(self.repo)["config_layers"], [])

    def test_token_dispatch_uses_recorded_config_root_from_other_cwd(self):
        self.write_layer(self.repo)
        token = self.prepare(self.repo)["token"]
        with_root = self.cli(self.outside, "dispatch", "--token", token, "--capture")
        self.assertEqual(with_root.returncode, 0, with_root.stderr)
        # Same dispatch resolved from the outside cwd with no recorded root
        # would see default config (executors enabled).
        spec_path = (self.home / ".claude" / "orchestra" / "tokens"
                     / (token + ".json"))
        spec = json.loads(spec_path.read_text(encoding="utf-8"))
        del spec["config_root"]
        spec_path.write_text(json.dumps(spec), encoding="utf-8")
        without_root = self.cli(self.outside, "dispatch", "--token", token, "--capture")
        self.assertEqual(without_root.returncode, 0, without_root.stderr)
        self.assertNotEqual(with_root.stdout, without_root.stdout)

    def test_old_token_without_config_root_still_works(self):
        token = self.prepare(self.repo)["token"]
        spec_path = (self.home / ".claude" / "orchestra" / "tokens"
                     / (token + ".json"))
        spec = json.loads(spec_path.read_text(encoding="utf-8"))
        del spec["config_root"]
        spec_path.write_text(json.dumps(spec), encoding="utf-8")
        result = self.cli(self.repo, "dispatch", "--token", token, "--capture")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(json.loads(result.stdout)["status"], ("delegate", "unroutable"))


if __name__ == "__main__":
    unittest.main()
