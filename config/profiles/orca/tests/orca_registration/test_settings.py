import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest import mock

import support  # noqa: F401 - Adds the standalone component directory to the import path.
import settings
from settings import CHAT_STATE, STOCK, YOLO, launch_defaults, reset_chat_permissions
from transport import RpcError


class SettingsRPC:
    def __init__(self, settings):
        self.settings = copy.deepcopy(settings)
        self.writes = []

    def call(self, method, params=None):
        if method == "settings.update":
            self.writes.append(copy.deepcopy(params))
            self.settings.update(params)
        elif method != "settings.get":
            raise AssertionError(method)
        return {"settings": copy.deepcopy(self.settings)}


class SettingsTests(unittest.TestCase):
    def test_fresh_and_existing_stock_defaults_use_config_without_losing_other_settings(self):
        claude = STOCK["claude"]
        for arguments, repaired in (({"claude": "--verbose"}, ["codex"]),
                                    ({"claude": "--verbose", "codex": YOLO}, ["codex"]),
                                    ({"codex": ""}, ["claude"]),
                                    ({"codex": "", "claude": " " + claude + " "}, ["claude"]),
                                    ({}, ["claude", "codex"]),
                                    ({"codex": YOLO, "claude": claude}, ["claude", "codex"])):
            with self.subTest(arguments=arguments):
                original = {"agentDefaultArgs": {"opencode": "--verbose", **arguments},
                            "agentDefaultEnv": {"codex": {"EXAMPLE": "retained"}},
                            "futurePreference": False,
                            "theme": "dark", "unrelated": {"retained": True}}
                rpc = SettingsRPC(original)
                self.assertFalse(launch_defaults(rpc))
                self.assertEqual([], rpc.writes, "assessment must be read-only")
                self.assertTrue(launch_defaults(rpc, apply=True))
                expected = copy.deepcopy(original)
                expected["agentDefaultArgs"].update(dict.fromkeys(repaired, ""))
                self.assertEqual(expected, rpc.settings)
                self.assertEqual([{"agentDefaultArgs": expected["agentDefaultArgs"]}], rpc.writes)
                self.assertTrue(launch_defaults(rpc, apply=True))
                self.assertEqual(1, len(rpc.writes), "repeat apply must be a no-op")

    def test_missing_map_is_initialized_and_explicit_custom_arguments_are_preserved(self):
        rpc = SettingsRPC({})
        self.assertTrue(launch_defaults(rpc, apply=True))
        self.assertEqual({"agentDefaultArgs": {"codex": "", "claude": ""}}, rpc.settings)
        for agent, values in (("codex", ("", "--model example", "--sandbox workspace-write", "--yolo")),
                              ("claude", ("", "--model example", "--permission-mode default",
                                          STOCK["claude"] + " --model example"))):
            for value in values:
                with self.subTest(agent=agent, value=value):
                    other = {"codex": "", "claude": ""}
                    rpc = SettingsRPC({"agentDefaultArgs": {**other, agent: value}})
                    self.assertTrue(launch_defaults(rpc, apply=True))
                    self.assertEqual([], rpc.writes)

    def test_invalid_settings_fail_without_mutation(self):
        for value in (None, [], {"agentDefaultArgs": None},
                      {"agentDefaultArgs": []}, {"agentDefaultArgs": {"codex": False}},
                      {"agentDefaultArgs": {"codex": "", "claude": None}}):
            with self.subTest(settings=value):
                rpc = SettingsRPC(value)
                with self.assertRaises(RpcError):
                    launch_defaults(rpc, apply=True)
                self.assertEqual([], rpc.writes)

    def test_failed_write_is_not_retried_and_success_requires_readback(self):
        rpc = SettingsRPC({"agentDefaultArgs": {"codex": YOLO}})
        original = rpc.call

        def reject(method, params=None):
            if method == "settings.update":
                rpc.writes.append(params)
                raise RpcError("write result unknown", unknown=True)
            return original(method, params)

        rpc.call = reject
        with self.assertRaises(RpcError):
            launch_defaults(rpc, apply=True)
        self.assertEqual(1, len(rpc.writes))

        def discard(method, params=None):
            return {"settings": {"agentDefaultArgs": {"codex": YOLO}}}

        rpc.call = discard
        with self.assertRaisesRegex(RpcError, "did not converge"):
            launch_defaults(rpc, apply=True)


class ChatRPC:
    """Synthetic Orca chat catalog: open tabs, Session History and saved chat options."""

    def __init__(self, chats, tabs=(), vault=(), refuse=(), ignore=(), moved=(), history=None):
        self.chats = copy.deepcopy(chats)
        self.tabs, self.vault = list(tabs), list(vault)
        self.refuse, self.ignore = set(refuse), set(ignore)
        self.moved = {session: count for session, count in moved}
        self.history = history or {}
        self.calls = []
        self.runtime_id = "runtime-1"
        self.settings = {"agentDefaultArgs": {"codex": "", "claude": ""}}

    def call(self, method, params=None):
        self.calls.append((method, copy.deepcopy(params)))
        if method == "settings.get":
            return {"settings": copy.deepcopy(self.settings)}
        if method == "session.tabs.listAll":
            return {"snapshots": [{"worktree": "repo::/work", "tabs": copy.deepcopy(self.tabs)}]}
        if method == "aiVault.listSessions":
            assert params == {"unlimited": True, "force": True}
            return {"issues": [], "scanId": "scan", **copy.deepcopy(self.history), "sessions": copy.deepcopy(self.vault)}
        session = params["sessionId"] if "sessionId" in params else params["envelope"]["sessionId"]
        chat = self.chats[session]
        if method == "agentSession.options":
            # A running or starting chat answers with the same saved mode as a resting one.
            return {"current": {"permissionMode": chat["mode"]}}
        if method == "agentSession.history":
            return {"page": {"fence": "fence-" + session, "items": []}}
        if method == "agentSession.setOption":
            if self.moved.get(session):
                self.moved[session] -= 1
                return {"ok": False, "refusal": {"code": "agent_session_ownership_unknown",
                                                 "details": {"reason": "leaseMoved"}}}
            if session in self.refuse:
                return {"ok": False, "refusal": {"code": "synthetic"}}
            if session not in self.ignore:
                chat["mode"] = params["value"]
            return {"ok": True, "value": params["value"]}
        raise AssertionError(method)

    def resets(self):
        return [params["envelope"]["sessionId"] for method, params in self.calls
                if method == "agentSession.setOption"]

    def listed(self):
        return any(method == "session.tabs.listAll" for method, _ in self.calls)


def tab(session, agent="claude", kind="agent-session"):
    return {"type": kind, "id": "tab-" + session, "sessionId": session, "agent": agent}


def history_entry(session, agent="claude"):
    return {"agent": agent, "structuredSession": {"sessionId": session, "workspaceId": "workspace"}}


class ChatPermissionMigrationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="subyard-orca-settings.", dir="/tmp")
        self.state = Path(self.tmp.name)
        self.path = self.state / CHAT_STATE

    def tearDown(self):
        self.tmp.cleanup()

    def saved(self):
        return json.loads(self.path.read_text())

    def test_resets_running_starting_open_and_closed_chats_once_then_waits_for_a_restart(self):
        rpc = ChatRPC({"open": {"mode": "full-access"}, "closed": {"mode": "full-access"},
                       "running": {"mode": "full-access"}, "ask": {"mode": "ask"}, "own": {"mode": "own"},
                       "codex-tab": {"mode": "full-access"}, "codex-history": {"mode": "full-access"}},
                      tabs=[tab("open"), tab("running"), tab("ask"), tab("codex-tab", "codex"),
                            tab("open", kind="terminal"), {"type": "terminal", "id": "plain"}],
                      vault=[history_entry("closed"), history_entry("open"), history_entry("own"),
                             history_entry("codex-history", "codex"), {"agent": "claude", "structuredSession": None}])
        self.assertEqual((False, 3), reset_chat_permissions(rpc, self.state, "runtime-1"))
        self.assertEqual(["closed", "open", "running"], rpc.resets())
        self.assertEqual({"open": "own", "closed": "own", "running": "own", "ask": "ask", "own": "own",
                          "codex-tab": "full-access", "codex-history": "full-access"},
                         {session: chat["mode"] for session, chat in rpc.chats.items()})
        self.assertEqual({"version": 1, "reset": ["closed", "open", "running"], "runtime": "runtime-1",
                          "scanned": True, "complete": False}, self.saved())
        self.assertEqual(0o600, stat.S_IMODE(self.path.stat().st_mode))
        self.assertTrue(settings.restart_pending(self.saved(), "runtime-1"))
        self.assertFalse(settings.restart_pending(self.saved(), "runtime-2"))

    def test_reset_uses_a_fresh_fence_and_retries_once_after_a_lease_move(self):
        rpc = ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")], moved=[("chat", 1)])
        self.assertEqual((False, 1), reset_chat_permissions(rpc, self.state, "runtime-1"))
        writes = [params for method, params in rpc.calls if method == "agentSession.setOption"]
        fields = {"key": "permissionMode", "value": "own"}
        payload = json.dumps({"method": "agentSession.setOption", "sessionId": "chat", "fields": fields},
                             sort_keys=True, separators=(",", ":")).encode()
        self.assertEqual(2, len(writes))
        self.assertEqual(2, sum(method == "agentSession.history" for method, _ in rpc.calls))
        for params in writes:
            self.assertEqual(fields, {key: params[key] for key in ("key", "value")})
            self.assertEqual({"sessionId": "chat", "expectedRuntimeFence": "fence-chat",
                              "payloadFingerprint": hashlib.sha256(payload).hexdigest()},
                             {key: params["envelope"][key] for key in
                              ("sessionId", "expectedRuntimeFence", "payloadFingerprint")})
            self.assertRegex(params["envelope"]["clientOperationId"], r"^[0-9]{13}-[0-9a-f]{32}$")
        self.assertNotEqual(writes[0]["envelope"]["clientOperationId"], writes[1]["envelope"]["clientOperationId"])

    def test_each_chat_is_reset_at_most_once_and_a_restart_completes(self):
        rpc = ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")])
        self.assertEqual((False, 1), reset_chat_permissions(rpc, self.state, "runtime-1"))
        rpc.chats["chat"]["mode"] = "full-access"  # An explicit choice after the reset.
        self.assertEqual((False, 0), reset_chat_permissions(rpc, self.state, "runtime-1"))
        self.assertEqual(["chat"], rpc.resets())
        calls = len(rpc.calls)
        self.assertEqual((True, 0), reset_chat_permissions(rpc, self.state, "runtime-2"))
        self.assertEqual(calls, len(rpc.calls), "a completed reset must not list or write chats")
        self.assertTrue(self.saved()["complete"])
        rpc.tabs.append(tab("later"))
        rpc.chats["later"] = {"mode": "full-access"}
        self.assertEqual((True, 0), reset_chat_permissions(rpc, self.state, "runtime-3"))
        self.assertEqual("full-access", rpc.chats["chat"]["mode"])
        self.assertEqual("full-access", rpc.chats["later"]["mode"])

    def test_nothing_to_reset_completes_in_the_same_runtime(self):
        rpc = ChatRPC({"own": {"mode": "own"}}, tabs=[tab("own")])
        self.assertEqual((True, 0), reset_chat_permissions(rpc, self.state, "runtime-1"))
        self.assertEqual({"version": 1, "reset": [], "runtime": None, "scanned": True, "complete": True},
                         self.saved())

    def test_incomplete_history_scan_never_completes(self):
        for history in ({"issues": [{"kind": "unreadable"}]}, {"truncated": True}, {"hasMore": True},
                        {"nextFingerprint": "next"}, {"unchanged": True}):
            with self.subTest(history=history):
                rpc = ChatRPC({"closed": {"mode": "full-access"}}, vault=[history_entry("closed")], history=history)
                with self.assertRaises(RpcError):
                    reset_chat_permissions(rpc, self.state, "runtime-1")
                self.assertEqual([], rpc.resets())
                self.assertFalse(self.path.exists())
                with self.assertRaises(RpcError):
                    reset_chat_permissions(rpc, self.state, "runtime-2")
                self.assertFalse(self.path.exists())

    def test_failures_keep_recorded_resets_and_never_complete(self):
        for name, rpc in {
                "refused": ChatRPC({"a": {"mode": "full-access"}, "b": {"mode": "full-access"}},
                                   tabs=[tab("a"), tab("b")], refuse={"b"}),
                "moved twice": ChatRPC({"a": {"mode": "full-access"}, "b": {"mode": "full-access"}},
                                       tabs=[tab("a"), tab("b")], moved=[("b", 2)]),
                "not converged": ChatRPC({"a": {"mode": "full-access"}, "b": {"mode": "full-access"}},
                                         tabs=[tab("a"), tab("b")], ignore={"b"}),
        }.items():
            with self.subTest(case=name):
                if self.path.exists():
                    os.unlink(self.path)
                with self.assertRaises(RpcError):
                    reset_chat_permissions(rpc, self.state, "runtime-1")
                self.assertEqual({"version": 1, "reset": ["a"], "runtime": "runtime-1", "scanned": False,
                                  "complete": False}, self.saved())
                self.assertTrue(settings.restart_pending(self.saved(), "runtime-1"))
                # A restart ends the reset chat's process; the next full pass may still need to reset b.
                rpc.refuse, rpc.ignore, rpc.moved = set(), set(), {}
                self.assertEqual((False, 1), reset_chat_permissions(rpc, self.state, "runtime-2"))
                self.assertEqual(["a", "b"], self.saved()["reset"])
                self.assertEqual((True, 0), reset_chat_permissions(rpc, self.state, "runtime-3"))

    def test_invalid_state_is_refused(self):
        for value in ("[]", '{"version": 2}', json.dumps({"version": 1, "reset": [""], "runtime": None,
                                                           "scanned": False, "complete": False})):
            with self.subTest(value=value):
                self.path.write_text(value)
                with self.assertRaises(ValueError):
                    reset_chat_permissions(ChatRPC({}), self.state, "runtime-1")

    def run_main(self, rpc, *arguments):
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.object(settings, "RuntimeRPC", return_value=rpc), \
                mock.patch.object(settings.sys, "argv", ["settings.py", *arguments, "--state", str(self.state)]), \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = settings.main()
        return code, stdout.getvalue(), stderr.getvalue()

    def test_apply_reports_resets_and_warns_until_orca_restarts(self):
        restart = ("Warning: saved Claude chats may still run Git without approval until Orca restarts; "
                   "run yard orca restart\n")
        rpc = ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")])
        self.assertEqual((0, "Orca reset 1 saved Claude chat(s) from Full access to the default permission "
                          "mode; choose Full access in a chat again to opt out\n", restart), self.run_main(rpc))
        self.assertEqual((3, "", ""), self.run_main(rpc, "--check"))
        self.assertEqual((0, "", restart), self.run_main(rpc))
        rpc.runtime_id = "runtime-2"
        self.assertEqual((0, "", ""), self.run_main(rpc, "--check"))
        self.assertEqual((0, "", ""), self.run_main(rpc))
        self.assertTrue(self.saved()["complete"])

    def test_apply_failure_warns_and_check_never_wedges(self):
        rpc = ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")], refuse={"chat"})
        self.assertEqual((0, "", "Warning: Orca could not finish resetting saved Claude chats from Full access; "
                          "they may still run Git without approval. Run yard orca sync, or switch each chat's "
                          "mode to Ask.\n"), self.run_main(rpc))
        self.assertEqual((0, "", ""), self.run_main(rpc, "--check"))
        self.path.write_text("not json")
        self.assertEqual((0, "", ""), self.run_main(rpc, "--check"))
        self.assertEqual(0, self.run_main(rpc)[0])

    def test_check_never_runs_the_migration(self):
        rpc = ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")])
        self.assertEqual((0, "", ""), self.run_main(rpc, "--check"))
        self.assertEqual(["settings.get"], [method for method, _ in rpc.calls])
        self.assertFalse(self.path.exists())
