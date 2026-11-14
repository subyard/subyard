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
from settings import MIGRATED, STOCK, YOLO, launch_defaults, reset_chat_permissions
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
        for settings in (None, [], {"agentDefaultArgs": None},
                         {"agentDefaultArgs": []}, {"agentDefaultArgs": {"codex": False}},
                         {"agentDefaultArgs": {"codex": "", "claude": None}}):
            with self.subTest(settings=settings):
                rpc = SettingsRPC(settings)
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

    def __init__(self, chats, tabs=(), vault=(), refuse=(), ignore=()):
        self.chats = copy.deepcopy(chats)
        self.tabs, self.vault = list(tabs), list(vault)
        self.refuse, self.ignore = set(refuse), set(ignore)
        self.calls = []
        self.settings = {"agentDefaultArgs": {"codex": "", "claude": ""}}

    def call(self, method, params=None):
        self.calls.append((method, copy.deepcopy(params)))
        if method == "settings.get":
            return {"settings": copy.deepcopy(self.settings)}
        if method == "session.tabs.listAll":
            return {"snapshots": [{"worktree": "repo::/work", "tabs": copy.deepcopy(self.tabs)}]}
        if method == "aiVault.listSessions":
            assert params == {"unlimited": True, "force": True}
            return {"sessions": copy.deepcopy(self.vault), "issues": []}
        session = params["sessionId"] if "sessionId" in params else params["envelope"]["sessionId"]
        chat = self.chats[session]
        if method == "agentSession.options":
            current = {"permissionMode": chat["mode"]}
            if chat.get("live"):
                current["reportedPermissionMode"] = "full-access"
            return {"current": current}
        if method == "agentSession.history":
            return {"page": {"fence": "fence-" + session, "items": []}}
        if method == "agentSession.setOption":
            if session in self.refuse:
                return {"ok": False, "refusal": {"code": "synthetic"}}
            if session not in self.ignore:
                chat["mode"] = params["value"]
            return {"ok": True, "value": params["value"]}
        raise AssertionError(method)

    def resets(self):
        return [params["envelope"]["sessionId"] for method, params in self.calls
                if method == "agentSession.setOption"]


def tab(session, agent="claude", kind="agent-session"):
    return {"type": kind, "id": "tab-" + session, "sessionId": session, "agent": agent}


def history(session, agent="claude"):
    return {"agent": agent, "structuredSession": {"sessionId": session, "workspaceId": "workspace"}}


class ChatPermissionMigrationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="subyard-orca-settings.", dir="/tmp")
        self.state = Path(self.tmp.name)
        self.marker = self.state / MIGRATED

    def tearDown(self):
        self.tmp.cleanup()

    def test_resets_full_access_claude_chats_from_tabs_and_history_then_writes_marker(self):
        rpc = ChatRPC({"open": {"mode": "full-access"}, "closed": {"mode": "full-access"},
                       "ask": {"mode": "ask"}, "own": {"mode": "own"},
                       "codex-tab": {"mode": "full-access"}, "codex-history": {"mode": "full-access"}},
                      tabs=[tab("open"), tab("ask"), tab("codex-tab", "codex"),
                            tab("open", kind="terminal"), {"type": "terminal", "id": "plain"}],
                      vault=[history("closed"), history("open"), history("own"),
                             history("codex-history", "codex"), {"agent": "claude", "structuredSession": None}])
        self.assertEqual((2, 0), reset_chat_permissions(rpc, self.state))
        self.assertEqual(["closed", "open"], rpc.resets())
        self.assertEqual({"open": "own", "closed": "own", "ask": "ask", "own": "own",
                          "codex-tab": "full-access", "codex-history": "full-access"},
                         {session: chat["mode"] for session, chat in rpc.chats.items()})
        self.assertEqual(b"1\n", self.marker.read_bytes())
        self.assertEqual(0o600, stat.S_IMODE(self.marker.stat().st_mode))

    def test_reset_uses_the_saved_chat_fence_and_payload_fingerprint(self):
        rpc = ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")])
        reset_chat_permissions(rpc, self.state)
        params = next(params for method, params in rpc.calls if method == "agentSession.setOption")
        fields = {"key": "permissionMode", "value": "own"}
        payload = json.dumps({"method": "agentSession.setOption", "sessionId": "chat", "fields": fields},
                             sort_keys=True, separators=(",", ":")).encode()
        self.assertEqual(fields, {key: params[key] for key in ("key", "value")})
        self.assertEqual({"sessionId": "chat", "expectedRuntimeFence": "fence-chat",
                          "payloadFingerprint": hashlib.sha256(payload).hexdigest()},
                         {key: params["envelope"][key] for key in
                          ("sessionId", "expectedRuntimeFence", "payloadFingerprint")})
        self.assertRegex(params["envelope"]["clientOperationId"], r"^[0-9]{13}-[0-9a-f]{32}$")

    def test_live_chats_stay_pending_without_marker_and_a_later_pass_completes(self):
        rpc = ChatRPC({"live": {"mode": "full-access", "live": True}, "idle": {"mode": "full-access"}},
                      tabs=[tab("live"), tab("idle")])
        self.assertEqual((1, 1), reset_chat_permissions(rpc, self.state))
        self.assertEqual(["idle"], rpc.resets())
        self.assertEqual("full-access", rpc.chats["live"]["mode"])
        self.assertFalse(self.marker.exists())
        rpc.chats["live"]["live"] = False
        self.assertEqual((1, 0), reset_chat_permissions(rpc, self.state))
        self.assertEqual(["idle", "live"], rpc.resets())
        self.assertTrue(self.marker.exists())

    def test_marker_leaves_later_explicit_full_access_untouched_without_rpc(self):
        rpc = ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")])
        self.assertEqual((1, 0), reset_chat_permissions(rpc, self.state))
        rpc.chats["chat"]["mode"] = "full-access"
        calls = len(rpc.calls)
        for _ in range(2):
            self.assertEqual((0, 0), reset_chat_permissions(rpc, self.state))
        self.assertEqual(calls, len(rpc.calls))
        self.assertEqual("full-access", rpc.chats["chat"]["mode"])

    def test_no_saved_chats_completes_at_once(self):
        self.assertEqual((0, 0), reset_chat_permissions(ChatRPC({}), self.state))
        self.assertTrue(self.marker.exists())

    def test_failures_never_write_the_marker(self):
        cases = {
            "refused": ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")], refuse={"chat"}),
            "not converged": ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")], ignore={"chat"}),
            "invalid catalog": ChatRPC({}, tabs=[{"type": "agent-session", "agent": "claude"}]),
            "unknown chat": ChatRPC({}, vault=[history("missing")]),
        }
        for name, rpc in cases.items():
            with self.subTest(case=name):
                with self.assertRaises((RpcError, KeyError)):
                    reset_chat_permissions(rpc, self.state)
                self.assertFalse(self.marker.exists())

    def test_apply_reports_resets_and_warns_until_migration_completes(self):
        for name, rpc, code, out, err in (
                ("reset", ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")]), 0,
                 "Orca reset 1 saved Claude chat(s) from Full access to the default permission mode; "
                 "choose Full access in a chat again to opt out\n", ""),
                ("live", ChatRPC({"chat": {"mode": "full-access", "live": True}}, tabs=[tab("chat")]), 0,
                 "", "Warning: 1 open saved Claude chat(s) still use Full access and may run Git without "
                 "approval. Close them and run yard orca sync, or switch each chat's mode to Ask.\n"),
                ("failure", ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")], refuse={"chat"}), 0,
                 "", "Warning: Orca could not reset saved Claude chats from Full access; they may still run "
                 "Git without approval. Run yard orca sync, or switch each chat's mode to Ask.\n")):
            with self.subTest(case=name):
                if self.marker.exists():
                    os.unlink(self.marker)
                stdout, stderr = io.StringIO(), io.StringIO()
                with mock.patch.object(settings, "RuntimeRPC", return_value=rpc), \
                        mock.patch.object(settings.sys, "argv", ["settings.py", "--state", str(self.state)]), \
                        contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                    self.assertEqual(code, settings.main())
                self.assertEqual((out, err), (stdout.getvalue(), stderr.getvalue()))
                self.assertEqual(name == "reset", self.marker.exists())

    def test_check_never_runs_the_migration(self):
        rpc = ChatRPC({"chat": {"mode": "full-access"}}, tabs=[tab("chat")])
        with mock.patch.object(settings, "RuntimeRPC", return_value=rpc), \
                mock.patch.object(settings.sys, "argv", ["settings.py", "--check", "--state", str(self.state)]):
            self.assertEqual(0, settings.main())
        self.assertEqual(["settings.get"], [method for method, _ in rpc.calls])
        self.assertFalse(self.marker.exists())
