"""Tests for `agent-exec approvals` (agent_exec_approvals.py): listing,
deciding, resuming the package and auditing. herdr is faked; nothing real
is run."""

import contextlib
import io
import json
import os
import shutil
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_exec_approvals as ap  # noqa: E402
import agent_exec_herdr  # noqa: E402
import agent_exec_sandbox as sb  # noqa: E402
import agent_exec_wave  # noqa: E402

_NET = {"mode": "allowlist", "allowed_domains": ["api.anthropic.com", "*.pypi.org"],
        "denied_domains": [], "quiet_domains": ["statsig.anthropic.com"]}


class FakeHerdr(object):
    def __init__(self, sessions):
        self.sessions = sessions
        self.calls = []
        self.state_dir = None

    def session(self, pkg, state_dir=None):
        return self.sessions.get(pkg)

    def _call(self, args, timeout=None, sname=None):
        self.calls.append(list(args))
        return {}, ""


class _Case(unittest.TestCase):
    def setUp(self):
        self.tmp = os.path.realpath(tempfile.mkdtemp(prefix="orch-approvals-"))
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.home = os.path.join(self.tmp, "home")
        os.makedirs(os.path.join(self.home, ".claude"))
        env = mock.patch.dict(os.environ, {"HOME": self.home,
                                           "XDG_STATE_HOME": os.path.join(self.tmp, "xs")})
        env.start()
        self.addCleanup(env.stop)
        cfg = mock.patch.object(ap, "_network_cfg", return_value=dict(_NET))
        cfg.start()
        self.addCleanup(cfg.stop)
        self.wave = os.path.join(self.tmp, "wave")
        os.makedirs(self.wave)
        self.state_path = os.path.join(self.wave, "state.json")
        self.store = agent_exec_wave.StateStore(self.state_path)
        self.store.init(os.path.join(self.wave, "plan.json"), ["A", "B"], "wave-int")

    def items(self):
        return ap.pending_items(self.state_path)

    def ids(self):
        return [i["id"] for i in self.items()]

    def decide(self, item, decision, reason="r", **kw):
        return ap.decide(self.state_path, item, decision, reason, **kw)

    def audit(self):
        with open(ap.decisions_audit_path(), encoding="utf-8") as fh:
            return [json.loads(line) for line in fh]

    def pkg(self, pid):
        return self.store.load()["packages"][pid]

    def needs(self):
        return [(n["id"], n["kind"]) for n in self.store.load()["needs"]]

    def network_need(self, pid, hosts):
        self.store.update(pid, network_denials=[{"host": h, "port": 443} for h in hosts])
        self.store.add_need(pid, "network", json.dumps({"hosts": hosts, "answer": "x"}))

    def permission_need(self, pid, path, candidate=None):
        self.store.add_need(pid, "permission", json.dumps({"denials": [
            {"path": path, "grant_candidate": candidate or path, "why": "not cache"}]}))


class ListingTests(_Case):
    def test_quiet_allowed_learned_and_declined_hosts_are_not_pending(self):
        sb.add_domain("learned.example.com")
        self.store.update("A", network_denials=[
            {"host": h, "port": 443} for h in (
                "statsig.anthropic.com", "files.pypi.org", "api.anthropic.com",
                "learned.example.com", "new.example.com", "later.example.com")])
        self.assertEqual(self.ids(), ["net:later.example.com", "net:new.example.com"])
        self.assertEqual(self.decide("net:new.example.com", "deny")[0], 0)
        self.assertEqual(self.ids(), ["net:later.example.com"])

    def test_ids_are_stable_across_calls(self):
        self.store.update("B", network_denials=[{"host": "b.example.com", "port": 443}])
        self.store.update("A", network_denials=[{"host": "a.example.com", "port": 80},
                                                {"host": "b.example.com", "port": 443}])
        self.permission_need("A", "/x/one")
        first = self.items()
        self.assertEqual(first, self.items())
        self.assertEqual([i["id"] for i in first],
                         ["net:a.example.com", "net:b.example.com", "path:A:0"])
        self.assertEqual([p["id"] for p in first[1]["packages"]], ["A", "B"])


class NetDecideTests(_Case):
    def test_refused_domain_is_not_recorded_and_is_audited(self):
        self.network_need("A", ["10.0.0.1"])
        code, out = self.decide("net:10.0.0.1", "allow")
        self.assertEqual(code, 1)
        self.assertFalse(out["applied"])
        self.assertEqual(sb.learned_domains(), [])
        self.assertEqual(self.needs(), [("A", "network")])
        self.assertEqual(self.ids(), ["net:10.0.0.1"])
        self.assertEqual([(r["item"], r["applied"]) for r in self.audit()],
                         [("net:10.0.0.1", False)])

    def test_allow_resumes_only_packages_waiting_on_that_host(self):
        self.network_need("A", ["x.example.com"])
        self.store.update("B", network_denials=[{"host": "x.example.com", "port": 443}])
        self.store.set_status("B", "implementing")
        code, out = self.decide("net:x.example.com", "allow", by="user")
        self.assertEqual(code, 0, out)
        self.assertEqual(out["resumed"], ["A"])
        self.assertEqual(self.needs(), [])
        self.assertEqual(self.pkg("A")["status"], "pending")
        self.assertEqual(self.pkg("B")["status"], "implementing")
        entry = sb.load_domains()[0][0]
        self.assertEqual((entry["domain"], entry["source"], entry["reason"]),
                         ("x.example.com", "user", "r"))
        self.assertTrue(self.audit()[0]["applied"])

    def test_deny_writes_a_note_and_waits_for_the_other_host(self):
        self.network_need("A", ["x.example.com", "y.example.com"])
        self.assertEqual(self.decide("net:x.example.com", "deny", reason="unrelated")[0], 0)
        self.assertEqual(self.needs(), [("A", "network")])  # y still undecided
        self.assertEqual(self.decide("net:y.example.com", "deny")[0], 0)
        self.assertEqual(self.needs(), [])
        self.assertEqual(self.pkg("A")["status"], "pending")
        with open(ap.note_path(self.wave, "A"), encoding="utf-8") as fh:
            self.assertIn("x.example.com was declined: unrelated", fh.read())
        self.assertEqual(sb.learned_domains(), [])


class PathDecideTests(_Case):
    def test_refused_path_is_not_granted_and_need_stays(self):
        self.permission_need("A", os.path.join(self.home, ".claude", "settings.json"))
        code, out = self.decide("path:A:0", "allow")
        self.assertEqual(code, 1)
        self.assertEqual(sb.load_learned()[0], [])
        self.assertEqual(self.needs(), [("A", "permission")])
        self.assertFalse(self.audit()[0]["applied"])

    def test_allow_grants_and_resumes(self):
        target = os.path.join(self.home, ".toolcache")
        os.makedirs(target)
        self.permission_need("A", os.path.join(target, "f"), candidate=target)
        code, out = self.decide("path:A:0", "allow")
        self.assertEqual(code, 0, out)
        grant = sb.load_learned()[0][0]
        self.assertEqual((grant["path"], grant["source"]), (target, "instructor"))
        self.assertEqual(self.needs(), [])
        self.assertEqual(self.pkg("A")["status"], "pending")

    def test_deny_resumes_with_note(self):
        self.permission_need("A", "/elsewhere/x")
        self.assertEqual(self.decide("path:A:0", "deny", reason="not needed")[0], 0)
        self.assertEqual(self.needs(), [])
        self.assertEqual(self.pkg("A")["status"], "pending")
        with open(ap.note_path(self.wave, "A"), encoding="utf-8") as fh:
            self.assertIn("/elsewhere/x is off-limits: not needed", fh.read())


class DialogDecideTests(_Case):
    def dialog(self, pid, executor):
        tree = os.path.join(self.tmp, "tree-" + pid)
        os.makedirs(tree)
        self.store.update(pid, executor=executor)
        self.store.add_need(pid, "delegate", json.dumps({
            "orca": "stalled",
            "detail": "dialog:\nAllow write?\nterminal: t-%s\nworktree: %s\nresult: r"
                      % (pid, tree)}))
        with open(os.path.join(self.wave, agent_exec_herdr.REGISTRY_NAME), "w") as fh:
            json.dump({pid: {"terminal": "t-" + pid, "worktree": tree}}, fh)
        return tree

    def test_orca_dialog_sends_nothing_and_keeps_the_need(self):
        self.dialog("A", "orca")
        herdr = FakeHerdr({})
        code, out = self.decide("dialog:A", "allow", herdr_factory=lambda: herdr)
        self.assertEqual(code, 2)
        self.assertIn("wave mark", out["error"])
        self.assertEqual(herdr.calls, [])
        self.assertEqual(self.needs(), [("A", "delegate")])
        self.assertFalse(self.audit()[0]["applied"])

    def test_dead_herdr_terminal_keeps_the_need(self):
        self.dialog("A", "herdr")
        herdr = FakeHerdr({"A": {"terminal": None, "worktree": "/x"}})
        code, _ = self.decide("dialog:A", "allow", herdr_factory=lambda: herdr)
        self.assertEqual(code, 2)
        self.assertEqual(herdr.calls, [])
        self.assertEqual(self.needs(), [("A", "delegate")])

    def test_herdr_allow_and_deny_send_keys_and_await(self):
        for pid, decision, key in (("A", "allow", "enter"), ("B", "deny", "escape")):
            tree = self.dialog(pid, "herdr")
            herdr = FakeHerdr({pid: {"terminal": "t-" + pid, "worktree": tree}})
            code, out = self.decide("dialog:" + pid, decision, herdr_factory=lambda: herdr)
            self.assertEqual(code, 0, out)
            self.assertEqual(herdr.calls, [["agent", "send-keys", "t-" + pid, key]])
            self.assertEqual((self.pkg(pid)["status"], self.pkg(pid)["detail"]),
                             ("implementing", "await-orca"))
        self.assertEqual(self.needs(), [])


class EscalateParseTests(unittest.TestCase):
    def test_network_escalate_vs_plain_escalate(self):
        self.assertEqual(ap.network_escalate_hosts(
            "ESCALATE: network registry.npmjs.org, `ghcr.io:8443`\nmore"),
            [{"host": "registry.npmjs.org", "port": 443}, {"host": "ghcr.io", "port": 8443}])
        self.assertEqual(ap.network_escalate_hosts("ESCALATE: ESCALATE: network a.example.com"),
                         [{"host": "a.example.com", "port": 443}])
        for plain in ("ESCALATE: spec unclear", "ESCALATE: networking code is broken",
                      "ESCALATE: network is down", "done"):
            self.assertIsNone(ap.network_escalate_hosts(plain), plain)

    def test_prose_after_the_host_list_yields_no_host(self):
        # Observed: a doubled prefix, then prose with an abbreviation in it.
        self.assertEqual(ap.network_escalate_hosts(
            "ESCALATE: ESCALATE: network example.com. curl failed with 403, i.e. blocked "
            "by the proxy (see docs.example.org)"),
            [{"host": "example.com", "port": 443}])
        self.assertEqual(ap.network_escalate_hosts(
            "ESCALATE: ESCALATE: network a.example.com, b.example.net: needed for tests"),
            [{"host": "a.example.com", "port": 443}, {"host": "b.example.net", "port": 443}])


class CliTests(_Case):
    def test_decide_requires_reason(self):
        self.permission_need("A", "/x")
        with contextlib.redirect_stderr(io.StringIO()):
            rc = ap.cmd_approvals(["decide", "--state", self.state_path, "path:A:0", "deny"])
        self.assertEqual(rc, 2)
        self.assertEqual(self.needs(), [("A", "permission")])


if __name__ == "__main__":
    unittest.main()
