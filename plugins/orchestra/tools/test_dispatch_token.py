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

        # The detach state is consumed by a successful `wait`: a second call
        # with no new `--detach` must fail rather than silently re-running.
        again = self.run_cli("dispatch", "wait", "--token", token2)
        self.assertEqual(again.returncode, 2)
        self.assertIn(token2, again.stderr)

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

    def test_wait_without_a_detached_run_is_an_error(self):
        token = self.prepare()
        result = self.run_cli("dispatch", "wait", "--token", token)
        self.assertEqual(result.returncode, 2)
        self.assertIn(token, result.stderr)

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


if __name__ == "__main__":
    unittest.main()
