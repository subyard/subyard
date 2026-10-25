#!/usr/bin/env python3
"""Read helpers release real SQLite/WAL descriptors before filesystem cleanup."""
import gc
import importlib.util
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest


def fixture(name):
    path = Path(__file__).parent/'e2e'/name
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def descriptors(data):
    found = 0
    for path in Path('/proc/self/fd').iterdir():
        try:
            target = os.readlink(path)
        except FileNotFoundError:
            continue
        found += target.startswith(str(data/'telemetry.sqlite'))
    return found


class SQLiteFixtureTest(unittest.TestCase):
    def assert_released(self, read):
        with tempfile.TemporaryDirectory(dir='/tmp') as name:
            data = Path(name)
            writer = sqlite3.connect(data/'telemetry.sqlite')
            writer.execute('PRAGMA journal_mode=WAL')
            writer.execute('CREATE TABLE records(kind TEXT)')
            writer.commit()
            writer.close()
            collecting = gc.isenabled()
            gc.disable()
            try:
                before = descriptors(data)
                read(data)
                self.assertEqual(descriptors(data), before, 'read helper retained SQLite/WAL descriptors without garbage collection')
            finally:
                gc.collect()
                if collecting:
                    gc.enable()

    def test_storage_count_releases_descriptors(self):
        self.assert_released(fixture('storage-failure.py').count)

    def test_native_query_releases_descriptors(self):
        native = fixture('native-signals.py')
        self.assert_released(lambda data: native.query(data, 'SELECT count(*) FROM records'))

    def test_native_query_failure_releases_descriptors(self):
        native = fixture('native-signals.py')
        def invalid(data):
            with self.assertRaises(sqlite3.OperationalError):
                native.query(data, 'SELECT missing_column FROM records')
        self.assert_released(invalid)


if __name__ == '__main__':
    unittest.main()
