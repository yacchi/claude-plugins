# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Tests for `agent-exec pi login` (agent_exec_pilogin.py).

No real herdr or pi: `FakeLoginHerdr` plays herdr plus pi's /login screens and
records every argv; the `pi auth check` runner is a separate fake.
"""

import json
import os
import shutil
import sys
import tempfile
import types
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec_pilogin as P  # noqa: E402

BOOT = " v1.2.0\n escape interrupt · ctrl+c/ctrl+d clear/exit · / commands\n"
AUTH_METHOD = (" Select authentication method:\n → Sign in with an account\n"
               "   Sign in with an API key\n")
PROVIDERS = (" Select provider to configure:\n>\n → Anthropic • not configured\n"
             "   OpenAI Codex (legacy) • not configured\n")
CODEX_FILTERED = (" Select provider to configure:\n> OpenAI Codex\n"
                  " → OpenAI Codex (legacy) • not configured\n")
ANTHROPIC_FILTERED = (" Select provider to configure:\n> Anthropic\n"
                      " → Anthropic • not configured\n")
OTHER_FILTERED = (" Select provider to configure:\n> OpenAI Codex\n"
                  " → OpenAI • not configured\n   OpenAI Codex (legacy) • not configured\n")
METHODS_BROWSER = " Select login method:\n → Browser login (default)\n   Device code login (headless)\n"
METHODS_DEVICE = " Select login method:\n   Browser login (default)\n → Device code login (headless)\n"
DEVICE_SCREEN = (" https://auth.openai.com/codex/device\n Enter code: ABCD-1234\n"
                 " Waiting for authentication...\n")
WRAPPED_URL = (" Open this URL to sign in:\n"
               " https://auth.example.com/oauth/authorize?response_type=code&client_id=a\n"
               " bc&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback&state=xyz\n"
               " Cmd+click to open\n"
               " Waiting for authentication...\n paste the final redirect URL here:\n")
WRAPPED_EXPECTED = ("https://auth.example.com/oauth/authorize?response_type=code&client_id=a"
                    "bc&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback&state=xyz")


class FakeLoginHerdr(object):
    """herdr + pi's /login screens as one state machine."""

    def __init__(self, provider_screen=CODEX_FILTERED, final=DEVICE_SCREEN,
                 methods=True, boot=BOOT):
        self.calls = []
        self.popens = []
        self.running = False
        self.stage = "idle"
        self.provider_screen = provider_screen
        self.final = final
        self.methods = methods
        self.boot = boot
        self.pane_alive = True
        self.screen_override = None

    def popen(self, argv, **kwargs):
        self.popens.append(argv)
        self.running = True

    def args(self):
        return [c[3:] for c in self.calls]

    def sessions(self):
        return set(c[2] for c in self.calls)

    def __call__(self, argv, **kwargs):
        self.calls.append(list(argv))
        a = argv[3:]
        if a[:1] == ["status"]:
            return self._ok({"result": {"server": {"running": self.running}}})
        if a[:2] == ["server", "stop"]:
            self.running = False
            return self._ok({})
        if a[:2] == ["workspace", "create"]:
            return self._ok({"result": {"workspace": {"workspace_id": "w1"},
                                        "root_pane": {"pane_id": "w1:p1"}}})
        if a[:2] in (["workspace", "close"], ["pane", "close"]):
            self.pane_alive = False
            return self._ok({})
        if a[:2] == ["pane", "run"]:
            self.stage = "boot"
            return self._ok({})
        if a[:2] == ["pane", "send-text"]:
            text = a[3]
            if text == "/login":
                self.stage = "typed-login"
            elif self.stage == "providers":
                self.stage = "filtered"
            elif self.stage == "paste-wait":
                self.stage = "pasted"
            return self._ok({})
        if a[:2] == ["pane", "send-keys"]:
            keys = a[3:]
            nxt = {("typed-login", "enter"): "auth-method",
                   ("auth-method", "enter"): "providers",
                   ("filtered", "enter"): "methods" if self.methods else "final",
                   ("methods-device", "enter"): "final"}
            if keys == ["down"] and self.stage == "methods":
                self.stage = "methods-device"
            elif (self.stage, keys[0]) in nxt:
                self.stage = nxt[(self.stage, keys[0])]
            return self._ok({})
        if a[:2] == ["pane", "read"]:
            if not self.pane_alive:
                return self._err("pane_not_found")
            if self.screen_override is not None:
                return self._raw(self.screen_override)
            screens = {"boot": self.boot, "auth-method": AUTH_METHOD,
                       "providers": PROVIDERS, "filtered": self.provider_screen,
                       "methods": METHODS_BROWSER, "methods-device": METHODS_DEVICE,
                       "final": self.final}
            return self._raw(screens.get(self.stage, ""))
        return self._err("unknown")

    @staticmethod
    def _raw(text):
        return types.SimpleNamespace(returncode=0, stdout=text, stderr="")

    @staticmethod
    def _ok(payload):
        return types.SimpleNamespace(returncode=0, stdout=json.dumps(payload), stderr="")

    @staticmethod
    def _err(code):
        return types.SimpleNamespace(
            returncode=1, stdout="",
            stderr=json.dumps({"error": {"code": code, "message": code}}))


class FakePiAuth(object):
    def __init__(self, ready=()):
        self.ready = set(ready)
        self.checked = []

    def __call__(self, argv, **kwargs):
        provider = argv[argv.index("--provider") + 1]
        self.checked.append(provider)
        if provider in self.ready:
            return types.SimpleNamespace(returncode=0, stderr="",
                                         stdout='{"status":"ready"}\n')
        return types.SimpleNamespace(
            returncode=1, stderr="warn",
            stdout='{"status":"not_ready","reason":"credentials_not_configured"}\n')


class _Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="orch-pilogin-")
        self.clock = {"now": 0.0}

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def login(self, fake, auth=None, cfg=None):
        def sleep(seconds):
            self.clock["now"] += max(seconds, 1.0)

        self.auth = auth or FakePiAuth()
        return P.PiLogin(
            cfg=cfg if cfg is not None else {}, runner=fake, pi_runner=self.auth,
            popen=fake.popen, sleep=sleep, clock=lambda: self.clock["now"],
            env={"PATH": "/usr/bin", "CLAUDECODE": "1"},
            state_dir=os.path.join(self.tmp, "state"),
            herdr_home=os.path.join(self.tmp, "hx"), herdr_binary="/opt/herdr",
            pi_path="/opt/pi", cwd=self.tmp, uid=501)


class ScreenParsing(unittest.TestCase):
    def test_wrapped_url_joined_without_spaces_or_cmd_click(self):
        self.assertEqual(P.extract_url(WRAPPED_URL), WRAPPED_EXPECTED)

    def test_device_screen(self):
        self.assertEqual(P.extract_url(DEVICE_SCREEN), "https://auth.openai.com/codex/device")
        self.assertEqual(P.extract_code(DEVICE_SCREEN), "ABCD-1234")

    def test_highlight_ignores_annotations(self):
        self.assertEqual(P.highlighted(CODEX_FILTERED), "OpenAI Codex")
        self.assertEqual(P.highlighted(OTHER_FILTERED), "OpenAI")


class StartFlow(_Base):
    def test_device_flow_returns_url_and_code(self):
        fake = FakeLoginHerdr()
        code, out = self.login(fake).start()
        self.assertEqual(code, 0)
        self.assertEqual(out["status"], "waiting")
        self.assertEqual(out["method"], "device")
        self.assertEqual(out["url"], "https://auth.openai.com/codex/device")
        self.assertEqual(out["code"], "ABCD-1234")
        self.assertEqual(out["session"], "orchestra-pilogin-501")
        # Device code, not the browser default, was chosen.
        self.assertIn(["pane", "send-keys", "w1:p1", "down"], fake.args())
        # The pane stays up for --wait.
        self.assertTrue(fake.running)
        self.assertNotIn(["server", "stop"], fake.args())

    def test_browser_flow_scrapes_wrapped_url_and_paste_prompt(self):
        fake = FakeLoginHerdr(final=WRAPPED_URL, methods=False,
                              provider_screen=ANTHROPIC_FILTERED)
        code, out = self.login(fake, cfg={}).start(provider="anthropic")
        self.assertEqual(code, 0)
        self.assertEqual(out["method"], "browser")
        self.assertEqual(out["url"], WRAPPED_EXPECTED)
        self.assertTrue(out["paste_prompt"])
        self.assertNotIn("code", out)

    def test_wrong_provider_highlighted_is_not_confirmed(self):
        fake = FakeLoginHerdr(provider_screen=OTHER_FILTERED)
        code, out = self.login(fake).start()
        self.assertEqual(code, 1)
        self.assertEqual(out["status"], "manual")
        self.assertEqual(out["step"], "provider-select")
        sent = [a for a in fake.args() if a[:2] == ["pane", "send-keys"]]
        # enter was pressed for /login and the auth method only, never for the provider
        self.assertEqual(len([k for k in sent if k[3:] == ["enter"]]), 2)
        self.assertFalse(fake.running)  # cleaned up

    def test_ready_provider_is_skipped_without_touching_herdr(self):
        fake = FakeLoginHerdr()
        code, out = self.login(fake, auth=FakePiAuth(ready=["openai-codex"])).start()
        self.assertEqual((code, out["status"]), (0, "ready"))
        self.assertEqual(fake.calls, [])

    def test_first_not_ready_provider_among_several(self):
        cfg = {"external_executors": {"pi": {"class_policy": {
            "light": {"model": "openai-codex/a"}, "deep": {"model": "anthropic/b"}}}}}
        fake = FakeLoginHerdr(provider_screen=ANTHROPIC_FILTERED)
        code, out = self.login(fake, auth=FakePiAuth(ready=["openai-codex"]),
                               cfg=cfg).start()
        self.assertEqual((code, out["provider"]), (0, "anthropic"))


class WaitCancel(_Base):
    def started(self, **kwargs):
        fake = FakeLoginHerdr(**kwargs)
        login = self.login(fake)
        self.assertEqual(login.start()[0], 0)
        return fake, login

    def assert_clean(self, fake, login):
        self.assertFalse(fake.running)
        self.assertIsNone(login.load_state())
        # only the dedicated login session was ever addressed
        self.assertEqual(fake.sessions() - {"orchestra-pilogin-501"}, set())

    def test_ready_closes_pane_and_stops_server(self):
        fake, login = self.started()
        self.auth.ready.add("openai-codex")
        code, out = login.wait(30)
        self.assertEqual((code, out), (0, {"status": "ready", "provider": "openai-codex"}))
        self.assertIn(["workspace", "close", "w1"], fake.args())
        self.assert_clean(fake, login)

    def test_timeout_leaves_pane_for_retry(self):
        fake, login = self.started()
        code, out = login.wait(10)
        self.assertEqual((code, out["status"]), (1, "timeout"))
        self.assertTrue(fake.running)
        self.assertIsNotNone(login.load_state())

    def test_pane_gone_is_failed_and_cleaned(self):
        fake, login = self.started()
        fake.pane_alive = False
        code, out = login.wait(30)
        self.assertEqual((code, out["status"]), (1, "failed"))
        self.assert_clean(fake, login)

    def test_pi_exited_to_shell_is_failed_with_screen_tail(self):
        fake, login = self.started()
        fake.screen_override = "user@host ~ % \n"
        code, out = login.wait(30)
        self.assertEqual((code, out["status"]), (1, "failed"))
        self.assertIn("user@host", out["screen_tail"])
        self.assert_clean(fake, login)

    def test_paste_sends_text_then_waits(self):
        fake, login = self.started(final=WRAPPED_URL, methods=False)
        fake.stage = "paste-wait"
        self.auth.ready.add("openai-codex")
        code, out = login.paste("http://localhost:1455/auth/callback?code=x", 30)
        self.assertEqual((code, out["status"]), (0, "ready"))
        self.assertIn(["pane", "send-text", "w1:p1", "http://localhost:1455/auth/callback?code=x"],
                      fake.args())

    def test_cancel_cleans_up(self):
        fake, login = self.started()
        code, out = login.cancel()
        self.assertEqual((code, out["status"]), (0, "cancelled"))
        self.assertIn(["pane", "send-keys", "w1:p1", "esc"], fake.args())
        self.assert_clean(fake, login)

    def test_wait_without_login_in_progress_fails(self):
        fake = FakeLoginHerdr()
        code, out = self.login(fake).wait(5)
        self.assertEqual((code, out["status"]), (1, "failed"))


if __name__ == "__main__":
    unittest.main()
