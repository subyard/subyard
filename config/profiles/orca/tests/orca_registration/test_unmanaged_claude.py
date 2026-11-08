import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('unmanaged_claude', Path(__file__).parents[2] / 'resources/orca/registration/unmanaged_claude.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class UnmanagedClaudeTest(unittest.TestCase):
    def test_native_copy_classification_preserves_everything(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as root:
            data = Path(root) / 'data'
            versions = data / 'claude/versions'
            versions.mkdir(parents=True, mode=0o700)
            for directory in (data, data / 'claude', versions):
                directory.chmod(0o700)
            self.assertFalse(module.installed(Path(root), os.getuid(), data))
            binary = versions / '2.1.319'
            binary.write_text('fixture binary\n')
            binary.chmod(0o755)
            self.assertTrue(module.installed(Path(root), os.getuid(), data))
            binary.chmod(0o777)
            self.assertFalse(module.installed(Path(root), os.getuid(), data))
            binary.unlink()
            binary.symlink_to('/usr/bin/true')
            self.assertFalse(module.installed(Path(root), os.getuid(), data))
            self.assertTrue(binary.is_symlink())
            binary.unlink()
            binary = versions / 'unknown'
            binary.write_text('keep\n')
            binary.chmod(0o755)
            self.assertFalse(module.installed(Path(root), os.getuid(), data))
            self.assertEqual(binary.read_text(), 'keep\n')


if __name__ == '__main__':
    unittest.main()
