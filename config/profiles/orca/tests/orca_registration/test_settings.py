import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import tempfile
import threading
import unittest
from unittest import mock

import support  # noqa: F401 - Adds the standalone component directory to the import path.
import settings
from settings import CHAT_LOCK, CHAT_STATE, STOCK, YOLO, launch_defaults, reset_chat_permissions
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


class Crash(BaseException):
    """The process dies: nothing after this point runs."""


class ChatRPC:
    """Synthetic Orca chat catalog: open tabs, Session History and saved chat options.

    Writes can apply in Orca and then fail on the way back (timeout, crash), read-backs can
    fail, and the runtime can change between any two calls.
    """

    def __init__(self, chats, tabs=(), vault=(), refuse=(), ignore=(), moved=(), history=None,
                 lost=(), crash=(), unreadable=(), tabs_fail=False, history_fail=False):
        self.chats = copy.deepcopy(chats)
        self.tabs, self.vault = list(tabs), list(vault)
        self.refuse, self.ignore = set(refuse), set(ignore)
        self.lost, self.crash, self.unreadable = set(lost), set(crash), set(unreadable)
        self.tabs_fail, self.history_fail = tabs_fail, history_fail
        self.moved = {session: count for session, count in moved}
        self.history = history or {}
        self.calls = []
        self.runtime_id = "runtime-1"
        self.settings = {"agentDefaultArgs": {"codex": "", "claude": ""}}
        self.before = None  # Runs before each call, e.g. to restart Orca or interleave a pass.
        self.state = None  # Directory whose saved state each write observes.
        self.dirty_at_write = []
        self.read_back = set()

    def call(self, method, params=None, before_send=None):
        if self.before:
            self.before(self, method, params)
        if before_send is not None:
            try:
                before_send(self.runtime_id)
            except OSError:
                # The transport converts a failed hook to an unsent connection failure.
                raise RpcError("Orca runtime connection failed") from None
        self.calls.append((method, copy.deepcopy(params)))
        if method == "settings.get":
            return {"settings": copy.deepcopy(self.settings)}
        if method == "session.tabs.listAll":
            if self.tabs_fail:
                raise RpcError("Orca runtime request timed out", timed_out=True)
            return {"snapshots": [{"worktree": "repo::/work", "tabs": copy.deepcopy(self.tabs)}]}
        if method == "aiVault.listSessions":
            assert params == {"unlimited": True, "force": True}
            if self.history_fail:
                raise RpcError("Orca runtime request timed out", timed_out=True)
            return {"issues": [], "scanId": "scan", **copy.deepcopy(self.history), "sessions": copy.deepcopy(self.vault)}
        session = params["sessionId"] if "sessionId" in params else params["envelope"]["sessionId"]
        chat = self.chats[session]
        if method == "agentSession.options":
            if session in self.read_back and session in self.unreadable:
                raise RpcError("Orca runtime request timed out", timed_out=True)
            # A running or starting chat answers with the same saved mode as a resting one.
            return {"current": {"permissionMode": chat["mode"]}}
        if method == "agentSession.history":
            return {"page": {"fence": "fence-" + session, "items": []}}
        if method == "agentSession.setOption":
            assert before_send is not None, "every chat write must record its runtime first"
            if self.state is not None:
                saved = json.loads((Path(self.state) / CHAT_STATE).read_text())
                self.dirty_at_write.append((saved["dirty"], self.runtime_id))
            if self.moved.get(session):
                self.moved[session] -= 1
                return {"ok": False, "refusal": {"code": "agent_session_ownership_unknown",
                                                 "details": {"reason": "leaseMoved"}}}
            if session in self.refuse:
                return {"ok": False, "refusal": {"code": "synthetic"}}
            if session not in self.ignore:
                chat["mode"] = params["value"]
            self.read_back.add(session)
            if session in self.crash:
                raise Crash()
            if session in self.lost:
                raise RpcError("Orca runtime request timed out", unknown=True, timed_out=True)
            return {"ok": True, "value": params["value"]}
        raise AssertionError(method)

    def resets(self):
        return [params["envelope"]["sessionId"] for method, params in self.calls
                if method == "agentSession.setOption"]

    def listed(self):
        return any(method == "session.tabs.listAll" for method, _ in self.calls)

    def heal(self):
        self.refuse, self.ignore, self.lost, self.crash, self.unreadable = set(), set(), set(), set(), set()
        self.moved, self.history, self.tabs_fail, self.history_fail = {}, {}, False, False


def tab(session, agent="claude", kind="agent-session"):
    return {"type": kind, "id": "tab-" + session, "sessionId": session, "agent": agent}


def history_entry(session, agent="claude"):
    return {"agent": agent, "structuredSession": {"sessionId": session, "workspaceId": "workspace"}}


def full(*sessions):
    return {session: {"mode": "full-access"} for session in sessions}


class ChatPermissionMigrationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="subyard-orca-settings.", dir="/tmp")
        self.state = Path(self.tmp.name)
        self.path = self.state / CHAT_STATE

    def tearDown(self):
        self.tmp.cleanup()

    def saved(self):
        return json.loads(self.path.read_text())

    def reset(self, rpc):
        return reset_chat_permissions(rpc, self.state)

    def restart(self, rpc, runtime):
        rpc.runtime_id = runtime

    def assert_never_completes_in(self, rpc, runtime):
        """Repeated passes in the dirty runtime keep the restart pending."""
        rpc.heal()
        rpc.runtime_id = runtime
        for _ in range(2):
            self.assertEqual("restart-pending", self.reset(rpc)[0])
            self.assertFalse(self.saved()["complete"])
            self.assertEqual(runtime, self.saved()["dirty"])

    def test_resets_running_starting_open_and_closed_chats_once_then_waits_for_a_restart(self):
        rpc = ChatRPC({"open": {"mode": "full-access"}, "closed": {"mode": "full-access"},
                       "running": {"mode": "full-access"}, "ask": {"mode": "ask"}, "own": {"mode": "own"},
                       "codex-tab": {"mode": "full-access"}, "codex-history": {"mode": "full-access"}},
                      tabs=[tab("open"), tab("running"), tab("ask"), tab("codex-tab", "codex"),
                            tab("open", kind="terminal"), {"type": "terminal", "id": "plain"}],
                      vault=[history_entry("closed"), history_entry("open"), history_entry("own"),
                             history_entry("codex-history", "codex"), {"agent": "claude", "structuredSession": None}])
        rpc.state = self.state
        self.assertEqual(("restart-pending", 3, False), self.reset(rpc))
        self.assertEqual(["closed", "open", "running"], rpc.resets())
        self.assertEqual({"open": "own", "closed": "own", "running": "own", "ask": "ask", "own": "own",
                          "codex-tab": "full-access", "codex-history": "full-access"},
                         {session: chat["mode"] for session, chat in rpc.chats.items()})
        self.assertEqual({"version": 1, "reset": ["closed", "open", "running"], "dirty": "runtime-1",
                          "complete": False}, self.saved())
        self.assertEqual([("runtime-1", "runtime-1")] * 3, rpc.dirty_at_write)
        self.assertEqual(0o600, stat.S_IMODE(self.path.stat().st_mode))
        self.assertEqual(0o600, stat.S_IMODE((self.state / CHAT_LOCK).stat().st_mode))
        self.assertEqual("restart-pending", settings.reset_state(self.saved(), "runtime-1"))
        self.assertEqual("incomplete", settings.reset_state(self.saved(), "runtime-2"))

    def test_reset_uses_a_fresh_fence_and_retries_once_after_a_lease_move(self):
        rpc = ChatRPC(full("chat"), tabs=[tab("chat")], moved=[("chat", 1)])
        self.assertEqual(("restart-pending", 1, False), self.reset(rpc))
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

    def test_each_chat_is_reset_at_most_once_and_only_a_clean_pass_after_a_restart_completes(self):
        rpc = ChatRPC(full("chat"), tabs=[tab("chat")])
        self.assertEqual(("restart-pending", 1, False), self.reset(rpc))
        rpc.chats["chat"]["mode"] = "full-access"  # An explicit choice after the reset.
        self.assertEqual(("restart-pending", 0, False), self.reset(rpc))
        self.assertEqual(["chat"], rpc.resets())
        self.restart(rpc, "runtime-2")
        self.assertEqual("incomplete", settings.reset_state(self.saved(), "runtime-2"))
        self.assertEqual(("complete", 0, False), self.reset(rpc))
        self.assertEqual({"version": 1, "reset": ["chat"], "dirty": "runtime-1", "complete": True}, self.saved())
        calls = len(rpc.calls)
        rpc.tabs.append(tab("later"))
        rpc.chats["later"] = {"mode": "full-access"}
        self.restart(rpc, "runtime-3")
        self.assertEqual(("complete", 0, False), self.reset(rpc))
        self.assertEqual(calls, len(rpc.calls), "a completed reset must not list or write chats")
        self.assertEqual("full-access", rpc.chats["chat"]["mode"])
        self.assertEqual("full-access", rpc.chats["later"]["mode"])

    def test_nothing_to_reset_completes_in_the_same_runtime(self):
        rpc = ChatRPC({"own": {"mode": "own"}}, tabs=[tab("own")])
        self.assertEqual(("complete", 0, False), self.reset(rpc))
        self.assertEqual({"version": 1, "reset": [], "dirty": None, "complete": True}, self.saved())

    def test_failure_at_every_write_step_keeps_the_restart_pending(self):
        """Orca may have applied the write: only a later runtime's clean pass can complete."""
        cases = {
            "setOption timeout after apply": {"lost": {"b"}},
            "read-back failure after apply": {"unreadable": {"b"}},
            "read-back reports no change": {"ignore": {"b"}},
            "refused after the intent was recorded": {"refuse": {"b"}},
            "lease moved twice": {"moved": [("b", 2)]},
        }
        for name, faults in cases.items():
            with self.subTest(case=name):
                if self.path.exists():
                    os.unlink(self.path)
                rpc = ChatRPC(full("a", "b", "c"), tabs=[tab("a"), tab("b"), tab("c")], **faults)
                rpc.state = self.state
                self.assertEqual(("restart-pending", 2, False), self.reset(rpc))
                self.assertEqual(["a", "c"], self.saved()["reset"])
                self.assertEqual("runtime-1", self.saved()["dirty"])
                self.assertTrue(all(dirty == runtime for dirty, runtime in rpc.dirty_at_write))
                self.assert_never_completes_in(rpc, "runtime-1")
                self.restart(rpc, "runtime-2")
                status, _, _ = self.reset(rpc)
                if rpc.chats["b"]["mode"] == "full-access":
                    # The write never applied: it is retried in the new runtime, which needs a restart too.
                    self.assertEqual("restart-pending", status)
                    self.restart(rpc, "runtime-3")
                    status, _, _ = self.reset(rpc)
                self.assertEqual("complete", status)

    def test_state_write_failures_keep_the_restart_pending(self):
        real = settings._save_chat_state
        for name, failing in {
                # The intent write fails: the chat write is never sent.
                "intent write": lambda data: data["dirty"] is not None and not data["reset"],
                # Orca applied the write, but recording the chat failed.
                "reset record write": lambda data: bool(data["reset"]),
        }.items():
            with self.subTest(step=name):
                if self.path.exists():
                    os.unlink(self.path)
                rpc = ChatRPC(full("chat"), tabs=[tab("chat")])

                def save(state, data):
                    if failing(data):
                        raise OSError("disk full")
                    real(state, data)

                with mock.patch.object(settings, "_save_chat_state", save):
                    status, count, _ = self.reset(rpc)
                    self.assertNotEqual("complete", status)
                    self.assertEqual(0, count)
                    self.assertNotEqual("complete", self.reset(rpc)[0])
                if name == "intent write":
                    self.assertEqual([], rpc.resets(), "an unrecorded intent must not send the write")
                    self.assertEqual("full-access", rpc.chats["chat"]["mode"])
                    self.assertFalse(self.path.exists())
                    self.assertEqual("restart-pending", self.reset(rpc)[0])
                else:
                    self.assertEqual("own", rpc.chats["chat"]["mode"])
                    self.assertEqual({"version": 1, "reset": [], "dirty": "runtime-1", "complete": False},
                                     self.saved())
                self.assert_never_completes_in(rpc, "runtime-1")
                self.restart(rpc, "runtime-2")
                self.assertEqual("complete", self.reset(rpc)[0])

    def test_crash_after_an_applied_write_keeps_the_restart_pending(self):
        rpc = ChatRPC(full("chat"), tabs=[tab("chat")], crash={"chat"})
        with self.assertRaises(Crash):
            self.reset(rpc)
        self.assertEqual("own", rpc.chats["chat"]["mode"])
        self.assertEqual({"version": 1, "reset": [], "dirty": "runtime-1", "complete": False}, self.saved())
        self.assert_never_completes_in(rpc, "runtime-1")
        self.restart(rpc, "runtime-2")
        self.assertEqual(("complete", 0, False), self.reset(rpc))

    def test_incomplete_scan_still_resets_visible_chats_but_never_completes(self):
        for name, faults in {"issues": {"history": {"issues": [{"kind": "unreadable"}]}},
                             "truncated": {"history": {"truncated": True}}, "hasMore": {"history": {"hasMore": True}},
                             "nextFingerprint": {"history": {"nextFingerprint": "next"}},
                             "unchanged": {"history": {"unchanged": True}}, "history failed": {"history_fail": True},
                             "tabs failed": {"tabs_fail": True}}.items():
            with self.subTest(case=name):
                if self.path.exists():
                    os.unlink(self.path)
                rpc = ChatRPC(full("open", "closed"), tabs=[tab("open")], vault=[history_entry("closed")], **faults)
                status, count, _ = self.reset(rpc)
                self.assertEqual("restart-pending", status)
                # Chats a partial listing does show are still reset.
                expected = {"tabs failed": ["closed"], "history failed": ["open"]}.get(name, ["closed", "open"])
                self.assertEqual(expected, rpc.resets())
                self.assertEqual(len(expected), count)
                # A restart without a clean scan does not complete.
                for runtime in ("runtime-2", "runtime-3"):
                    self.restart(rpc, runtime)
                    self.assertEqual(("incomplete", 0, False), self.reset(rpc))
                    self.assertFalse(self.saved()["complete"])
                rpc.heal()
                if len(expected) == 1:
                    # The chat the failed listing hid is reset now and needs its own restart.
                    self.assertEqual(("restart-pending", 1, False), self.reset(rpc))
                    self.restart(rpc, "runtime-4")
                self.assertEqual(("complete", 0, False), self.reset(rpc))

    def test_nothing_reset_and_an_incomplete_scan_does_not_complete(self):
        rpc = ChatRPC({"own": {"mode": "own"}}, tabs=[tab("own")], history={"hasMore": True})
        self.assertEqual(("incomplete", 0, False), self.reset(rpc))
        self.assertFalse(self.path.exists())
        self.assertEqual(("incomplete", 0, False), self.reset(rpc))

    def test_one_refusing_chat_of_three_does_not_stop_the_others_or_complete(self):
        rpc = ChatRPC(full("a", "b", "c"), tabs=[tab("a"), tab("b"), tab("c")], refuse={"b"})
        self.assertEqual(("restart-pending", 2, False), self.reset(rpc))
        self.assertEqual(["a", "c"], self.saved()["reset"])
        # The refused chat is retried in the new runtime; the attempt needs another restart.
        self.restart(rpc, "runtime-2")
        self.assertEqual(("restart-pending", 0, False), self.reset(rpc), "a refusal forbids completion")
        self.assertEqual("runtime-2", self.saved()["dirty"])
        rpc.heal()
        self.assertEqual(("restart-pending", 1, False), self.reset(rpc))
        self.assertEqual(["a", "c", "b"], self.saved()["reset"])
        self.restart(rpc, "runtime-3")
        self.assertEqual(("complete", 0, False), self.reset(rpc))
        self.assertEqual(["a", "b", "b", "b", "c"], sorted(rpc.resets()))

    def test_unreadable_chat_mode_forbids_completion(self):
        rpc = ChatRPC(full("a"), tabs=[tab("a"), tab("gone")])
        self.assertEqual(("restart-pending", 1, False), self.reset(rpc))
        self.restart(rpc, "runtime-2")
        self.assertEqual(("incomplete", 0, False), self.reset(rpc))  # "gone" has no saved options.
        rpc.tabs = [tab("a")]
        self.assertEqual(("complete", 0, False), self.reset(rpc))

    def test_runtime_change_mid_pass_does_not_complete(self):
        rpc = ChatRPC(full("a", "b"), tabs=[tab("a"), tab("b")])
        rpc.state = self.state
        self.assertEqual(("restart-pending", 2, False), self.reset(rpc))
        self.restart(rpc, "runtime-2")

        def restart_during_history(fake, method, params):
            if method == "aiVault.listSessions":
                fake.runtime_id = "runtime-3"
        rpc.before = restart_during_history
        self.assertEqual(("incomplete", 0, False), self.reset(rpc))
        self.assertFalse(self.saved()["complete"])
        rpc.before = None
        self.assertEqual(("complete", 0, False), self.reset(rpc))

    def test_runtime_change_before_a_write_records_the_writing_connection(self):
        rpc = ChatRPC(full("a", "b"), tabs=[tab("a"), tab("b")])
        rpc.state = self.state

        def restart_before_second_write(fake, method, params):
            if method == "agentSession.history" and params["sessionId"] == "b":
                fake.runtime_id = "runtime-2"
        rpc.before = restart_before_second_write
        self.assertEqual(("restart-pending", 2, False), self.reset(rpc))
        self.assertEqual([("runtime-1", "runtime-1"), ("runtime-2", "runtime-2")], rpc.dirty_at_write)
        self.assertEqual("runtime-2", self.saved()["dirty"])
        rpc.before = None
        self.assert_never_completes_in(rpc, "runtime-2")
        self.restart(rpc, "runtime-3")
        self.assertEqual(("complete", 0, False), self.reset(rpc))

    def test_interleaved_passes_are_serialized(self):
        rpc = ChatRPC(full("a", "b"), tabs=[tab("a"), tab("b")])
        rpc.state = self.state
        inside, release = threading.Event(), threading.Event()
        order = []

        def pause_first_pass(fake, method, params):
            order.append((threading.current_thread().name, method))
            if method == "agentSession.history" and params["sessionId"] == "a" and not inside.is_set():
                inside.set()
                self.assertTrue(release.wait(10))
        rpc.before = pause_first_pass
        results = {}

        def run(name):
            results[name] = self.reset(rpc)
        first = threading.Thread(target=run, args=("first",), name="first")
        first.start()
        self.assertTrue(inside.wait(10))
        second = threading.Thread(target=run, args=("second",), name="second")
        second.start()
        second.join(0.5)
        self.assertTrue(second.is_alive(), "a second pass must wait for the lock")
        self.assertNotIn("second", {name for name, _ in order})
        release.set()
        first.join(10)
        second.join(10)
        self.assertEqual(("restart-pending", 2, False), results["first"])
        self.assertEqual(("restart-pending", 0, False), results["second"])
        self.assertEqual(["a", "b"], rpc.resets())
        self.assertEqual({"version": 1, "reset": ["a", "b"], "dirty": "runtime-1", "complete": False}, self.saved())

    def test_a_held_lock_times_out_without_touching_orca(self):
        rpc = ChatRPC(full("a"), tabs=[tab("a")])
        with settings._exclusive(self.state):
            with mock.patch.object(settings, "LOCK_WAIT", 0.1), self.assertRaises(OSError):
                with settings._exclusive(self.state, wait=0.1):
                    pass
            code, _, stderr = self.run_main(rpc, "--reset-chats", lock_wait=0.1)
        self.assertEqual(0, code)
        self.assertIn("could not finish resetting", stderr)
        self.assertEqual([], rpc.calls)

    def test_corrupt_state_requires_one_restart_and_never_wedges(self):
        for value in ("not json", "[]", '{"version": 2}', json.dumps({"version": 1, "reset": [""], "dirty": None,
                                                                       "complete": False}),
                      json.dumps({"version": 1, "reset": [], "runtime": None, "scanned": True, "complete": True})):
            with self.subTest(value=value):
                self.path.write_text(value)
                rpc = ChatRPC({"own": {"mode": "own"}}, tabs=[tab("own")])
                self.assertEqual((0, "launch=ready reset=unreadable\n", ""), self.run_main(rpc, "--check"))
                self.assertEqual(("restart-pending", 0, True), self.reset(rpc))
                self.assertEqual({"version": 1, "reset": [], "dirty": "runtime-1", "complete": False}, self.saved())
                self.assertEqual(0o600, stat.S_IMODE(self.path.stat().st_mode))
                self.assert_never_completes_in(rpc, "runtime-1")
                self.restart(rpc, "runtime-2")
                self.assertEqual(("complete", 0, False), self.reset(rpc))

    def test_corrupt_state_warns_loudly(self):
        self.path.write_text("not json")
        rpc = ChatRPC(full("chat"), tabs=[tab("chat")])
        code, stdout, stderr = self.run_main(rpc)
        self.assertEqual(0, code)
        self.assertIn("reset state was unreadable and was rewritten", stderr)
        self.assertIn("until Orca restarts", stderr)
        self.assertIn("Orca reset 1 saved Claude chat(s)", stdout)

    def run_main(self, rpc, *arguments, lock_wait=None):
        stdout, stderr = io.StringIO(), io.StringIO()
        patches = [mock.patch.object(settings, "RuntimeRPC", return_value=rpc),
                   mock.patch.object(settings.sys, "argv", ["settings.py", *arguments, "--state", str(self.state)])]
        if lock_wait is not None:
            patches.append(mock.patch.object(settings, "LOCK_WAIT", lock_wait))
        with contextlib.ExitStack() as stack:
            for patch in patches:
                stack.enter_context(patch)
            stack.enter_context(contextlib.redirect_stdout(stdout))
            stack.enter_context(contextlib.redirect_stderr(stderr))
            code = settings.main()
        return code, stdout.getvalue(), stderr.getvalue()

    def test_apply_reports_resets_and_warns_until_orca_restarts(self):
        restart = ("Warning: saved Claude chats may still run Git without approval until Orca restarts; "
                   "run yard orca restart\n")
        rpc = ChatRPC(full("chat"), tabs=[tab("chat")])
        self.assertEqual((0, "Orca reset 1 saved Claude chat(s) from Full access to the default permission "
                          "mode; choose Full access in a chat again to opt out\n", restart), self.run_main(rpc))
        self.assertEqual((3, "launch=ready reset=restart-pending\n", ""), self.run_main(rpc, "--check"))
        self.assertEqual((0, "", restart), self.run_main(rpc))
        rpc.runtime_id = "runtime-2"
        self.assertEqual((0, "launch=ready reset=incomplete\n", ""), self.run_main(rpc, "--check"))
        self.assertEqual((0, "", ""), self.run_main(rpc, "--reset-chats"))
        self.assertTrue(self.saved()["complete"])
        self.assertEqual((0, "launch=ready reset=complete\n", ""), self.run_main(rpc, "--check"))

    def test_check_reports_launch_defaults_separately_from_the_reset(self):
        rpc = ChatRPC(full("chat"), tabs=[tab("chat")])
        self.run_main(rpc)
        rpc.settings = {"agentDefaultArgs": {}}
        self.assertEqual((3, "launch=unready reset=restart-pending\n", ""), self.run_main(rpc, "--check"))
        rpc.runtime_id = "runtime-2"
        self.assertEqual((1, "launch=unready reset=incomplete\n", ""), self.run_main(rpc, "--check"))
        rpc.call = lambda *args, **kwargs: (_ for _ in ()).throw(RpcError("Orca runtime connection failed"))
        self.assertEqual((1, "launch=unavailable reset=unknown\n",
                          "Orca agent launch settings unavailable; run yard orca up\n"), self.run_main(rpc, "--check"))

    def test_reset_only_pass_leaves_launch_defaults_alone(self):
        rpc = ChatRPC(full("chat"), tabs=[tab("chat")])
        rpc.settings = {"agentDefaultArgs": {}}
        self.assertEqual(0, self.run_main(rpc, "--reset-chats")[0])
        self.assertNotIn("settings.get", [method for method, _ in rpc.calls])
        self.assertNotIn("settings.update", [method for method, _ in rpc.calls])
        self.assertEqual(["chat"], rpc.resets())

    def test_apply_failure_warns_and_check_never_wedges(self):
        rpc = ChatRPC(full("chat"), tabs=[tab("chat")], refuse={"chat"})
        incomplete = ("Warning: Orca could not finish resetting saved Claude chats from Full access; "
                      "they may still run Git without approval. Run yard orca sync, or switch each chat's "
                      "mode to Ask.\n")
        self.assertEqual((0, "", "Warning: saved Claude chats may still run Git without approval until Orca "
                          "restarts; run yard orca restart\n"), self.run_main(rpc))
        rpc.runtime_id = "runtime-2"
        rpc.refuse, rpc.history = set(), {"hasMore": True}
        self.assertEqual((0, "Orca reset 1 saved Claude chat(s) from Full access to the default permission "
                          "mode; choose Full access in a chat again to opt out\n",
                          "Warning: saved Claude chats may still run Git without approval until Orca "
                          "restarts; run yard orca restart\n"), self.run_main(rpc, "--reset-chats"))
        rpc.runtime_id = "runtime-3"
        self.assertEqual((0, "", incomplete), self.run_main(rpc, "--reset-chats"))
        self.assertEqual((0, "launch=ready reset=incomplete\n", ""), self.run_main(rpc, "--check"))
        rpc.call = lambda *args, **kwargs: (_ for _ in ()).throw(RpcError("Orca runtime connection failed"))
        rpc.runtime_id = None
        self.assertEqual((0, "", incomplete), self.run_main(rpc, "--reset-chats"))

    def test_check_never_runs_the_migration(self):
        rpc = ChatRPC(full("chat"), tabs=[tab("chat")])
        self.assertEqual((0, "launch=ready reset=incomplete\n", ""), self.run_main(rpc, "--check"))
        self.assertEqual(["settings.get"], [method for method, _ in rpc.calls])
        self.assertFalse(self.path.exists())
        self.assertFalse((self.state / CHAT_LOCK).exists())
