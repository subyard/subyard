import copy
import unittest

import support  # Adds the standalone component directory to the import path.
from settings import YOLO, codex_defaults
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
        for arguments in ({}, {"codex": YOLO}):
            with self.subTest(arguments=arguments):
                original = {"agentDefaultArgs": {"claude": "--verbose", **arguments},
                            "agentDefaultEnv": {"codex": {"EXAMPLE": "retained"}},
                            "theme": "dark", "unrelated": {"retained": True}}
                rpc = SettingsRPC(original)
                self.assertFalse(codex_defaults(rpc))
                self.assertEqual([], rpc.writes, "assessment must be read-only")
                self.assertTrue(codex_defaults(rpc, apply=True))
                expected = copy.deepcopy(original)
                expected["agentDefaultArgs"]["codex"] = ""
                self.assertEqual(expected, rpc.settings)
                self.assertEqual([{"agentDefaultArgs": expected["agentDefaultArgs"]}], rpc.writes)
                self.assertTrue(codex_defaults(rpc, apply=True))
                self.assertEqual(1, len(rpc.writes), "repeat apply must be a no-op")

    def test_missing_map_is_initialized_and_explicit_custom_arguments_are_preserved(self):
        rpc = SettingsRPC({})
        self.assertTrue(codex_defaults(rpc, apply=True))
        self.assertEqual({"agentDefaultArgs": {"codex": ""}}, rpc.settings)
        for value in ("", "--model example", "--sandbox workspace-write", "--yolo"):
            with self.subTest(value=value):
                rpc = SettingsRPC({"agentDefaultArgs": {"codex": value}})
                self.assertTrue(codex_defaults(rpc, apply=True))
                self.assertEqual([], rpc.writes)

    def test_invalid_settings_fail_without_mutation(self):
        for settings in (None, [], {"agentDefaultArgs": None},
                         {"agentDefaultArgs": []}, {"agentDefaultArgs": {"codex": False}}):
            with self.subTest(settings=settings):
                rpc = SettingsRPC(settings)
                with self.assertRaises(RpcError):
                    codex_defaults(rpc, apply=True)
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
            codex_defaults(rpc, apply=True)
        self.assertEqual(1, len(rpc.writes))

        def discard(method, params=None):
            return {"settings": {"agentDefaultArgs": {"codex": YOLO}}}

        rpc.call = discard
        with self.assertRaisesRegex(RpcError, "did not converge"):
            codex_defaults(rpc, apply=True)
