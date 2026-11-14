import copy
import unittest

import support  # noqa: F401 - Adds the standalone component directory to the import path.
from settings import STOCK, YOLO, launch_defaults
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
