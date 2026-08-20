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
        self.env = dict(os.environ, HOME=str(self.home))

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
            ("--class", "standard", "--prompt-file", str(self.root / "missing"),
             "--workdir", str(self.root)),
            ("--prompt-file", str(self.contract), "--workdir", str(self.root)),
            ("--class", "standard", "--prompt-file", str(self.root / "missing")),
            ("--class", "standard", "--prompt-file", str(self.contract)),
        ]
        for extra in cases:
            result = self.run_cli("dispatch", "prepare", *extra)
            self.assertEqual(result.returncode, 2)
            self.assertEqual(result.stdout, "")
        self.assertFalse(self.token_dir().exists())

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
            ("--class", "standard"), ("--prompt-file", str(self.contract)),
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
