#!/usr/bin/env python3
import argparse
import importlib.util
import os
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "trim", Path(__file__).resolve().parents[1] / "trim-gocache.py"
)
trim = importlib.util.module_from_spec(spec)
spec.loader.exec_module(trim)


class TrimTest(unittest.TestCase):
    def test_trim_safety_and_watermark(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as tmp:
            root = Path(tmp) / "cache"
            shard = root / "ab"
            shard.mkdir(parents=True)
            root.chmod(0o750)
            identity = root.stat()
            outside = Path(tmp) / "outside"
            outside.mkdir()
            victim = outside / "keep"
            victim.write_bytes(b"x" * 8192)
            os.utime(victim, (1, 1))
            (root / "cd").symlink_to(outside)
            (shard / "link").symlink_to(victim)
            files = [shard / str(i) for i in range(5)]
            for i, entry in enumerate(files):
                entry.write_bytes(b"x" * 8192)
                age = time.time() if i == 4 else i + 1
                os.utime(entry, (age, age))
            size = files[0].stat().st_blocks * 512
            trim.trim(str(root), size * 6)
            self.assertTrue(all(p.exists() for p in files))
            trim.trim(str(root), size * 4)
            self.assertEqual([p.exists() for p in files], [False, False, True, True, True])
            self.assertTrue(victim.exists())
            self.assertTrue((shard / "link").is_symlink())
            self.assertEqual((root.stat().st_ino, root.stat().st_mode),
                             (identity.st_ino, identity.st_mode))
            trim.trim(str(root), 1)
            self.assertTrue(files[4].exists())  # Fresh even when above limit.
            with self.assertRaises(OSError):
                trim.trim(str(root / "cd"), 1)

    def test_overlap_with_build_cache_use(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as tmp:
            root = Path(tmp)
            shard = root / "ab"
            shard.mkdir()
            selected = shard / "selected-d"
            touched = shard / "touched-d"
            old = shard / "old-d"
            for path in (selected, touched, old):
                path.write_bytes(b"x" * 8192)
                os.utime(path, (1, 1))
            # A build selected this entry without refreshing its mtime, which Go
            # coalesces hourly. It can cross the old one-hour cutoff before open.
            age = time.time() - 90 * 60
            os.utime(selected, (age, age))
            snapshot = trim.entry

            def overlapping_use(parent, name, device):
                result = snapshot(parent, name, device)
                if name == touched.name and result[0] == 1:
                    os.utime(touched, None)  # Build uses it after trim scans it.
                return result

            with patch.object(trim, "entry", side_effect=overlapping_use):
                trim.trim(str(root), 1)
            self.assertFalse(old.exists())
            # Model the delayed linker's open after trim completes.
            self.assertEqual(selected.read_bytes(), b"x" * 8192)
            self.assertEqual(touched.read_bytes(), b"x" * 8192)

    def test_executable_directory_entries(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as tmp:
            root = Path(tmp) / "cache"
            shard = root / "ab"
            shard.mkdir(parents=True)
            outside = Path(tmp) / "outside"
            outside.write_bytes(b"keep")
            old = shard / "old-d"
            fresh = shard / "fresh-d"
            unsafe = shard / "unsafe-d"
            for directory in (old, fresh, unsafe):
                directory.mkdir()
                executable = directory / "program"
                executable.write_bytes(b"x" * 16384)
                os.utime(executable, (1, 1))
                os.utime(directory, (1, 1))
            (unsafe / "link").symlink_to(outside)
            os.utime(unsafe, (1, 1))
            os.utime(fresh / "program", None)
            # Only executable directories contribute bytes: they must trigger trim.
            trim.trim(str(root), 1)
            self.assertFalse(old.exists())
            self.assertTrue((fresh / "program").exists())
            self.assertTrue((unsafe / "link").is_symlink())
            self.assertEqual(outside.read_bytes(), b"keep")
            self.assertTrue((unsafe / "program").exists())

    def test_limits(self):
        self.assertEqual(trim.limit_bytes("30G"), 30 * 1024**3)
        for invalid in ("0", "-1", "1.5G", "30GB", "", "1\n"):
            with self.assertRaises(argparse.ArgumentTypeError):
                trim.limit_bytes(invalid)


if __name__ == "__main__":
    unittest.main()
