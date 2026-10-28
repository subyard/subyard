#!/usr/bin/env python3
"""Independent negative cases for retained evidence and resumable staging."""
import os
from pathlib import Path
import runpy
import tempfile
import unittest

helper = runpy.run_path(str(Path(__file__).resolve().parents[2]/'dev/verify-release-upgrades.py'))
capture, verify = helper['capture_history_evidence'], helper['verify_history_evidence']
DESTINATION = 'settings-v2.applied.json'
PENDING = '.'+DESTINATION+'.pending'


def write(path, payload, mode=0o600):
    path.write_bytes(payload)
    path.chmod(mode)


class EvidenceOracleTest(unittest.TestCase):
    def test_published_identity_changes_are_independently_rejected(self):
        for mutant in ('bytes', 'mode', 'inode', 'mtime'):
            with self.subTest(mutant=mutant), tempfile.TemporaryDirectory(prefix='subyard-evidence-negative-', dir='/tmp') as directory:
                root = Path(directory)
                path = root/'settings-v2.captured.json'
                write(path, b'published bytes')
                before = capture(root)
                info = path.stat()
                if mutant == 'bytes':
                    path.write_bytes(b'foreign bytes!!')
                    os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns))
                elif mutant == 'mode': path.chmod(0o644)
                elif mutant == 'inode':
                    replacement = root/'replacement'
                    write(replacement, path.read_bytes())
                    os.utime(replacement, ns=(info.st_atime_ns, info.st_mtime_ns))
                    replacement.replace(path)
                else: os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns+1_000_000))
                with self.assertRaises(RuntimeError): verify(root, before)

    def test_staged_identity_and_prefix_changes_are_independently_rejected(self):
        for mutant in ('inode', 'mode', 'prefix', 'leftover', 'wrong-destination'):
            with self.subTest(mutant=mutant), tempfile.TemporaryDirectory(prefix='subyard-staging-negative-', dir='/tmp') as directory:
                root = Path(directory)
                pending = root/PENDING
                write(pending, b'{"schemaVersion":')
                before = capture(root, [DESTINATION])
                if mutant == 'inode':
                    replacement = root/'replacement'
                    write(replacement, pending.read_bytes())
                    replacement.replace(pending)
                elif mutant == 'mode': pending.chmod(0o644)
                elif mutant == 'prefix': pending.write_bytes(b'foreign prefix')
                if mutant != 'leftover':
                    with pending.open('ab') as output: output.write(b'1}')
                    pending.rename(root/(DESTINATION if mutant != 'wrong-destination' else 'foreign.applied.json'))
                with self.assertRaises(RuntimeError): verify(root, before, [DESTINATION])

    def test_unknown_hidden_staged_and_colliding_files_are_refused(self):
        for mutant in ('hidden', 'empty-pending-name', 'unknown-pending', 'visible-pending', 'collision'):
            with self.subTest(mutant=mutant), tempfile.TemporaryDirectory(prefix='subyard-staging-layout-', dir='/tmp') as directory:
                root = Path(directory)
                name = {'hidden':'.unknown', 'empty-pending-name':'.pending', 'unknown-pending':'.foreign.applied.json.pending',
                        'visible-pending':'foreign.pending', 'collision':PENDING}[mutant]
                write(root/name, b'prefix')
                if mutant == 'collision': write(root/DESTINATION, b'published')
                with self.assertRaises(RuntimeError): capture(root, [DESTINATION])

    def test_unsafe_objects_and_destination_paths_are_refused(self):
        for mutant in ('symlink', 'hardlink', 'directory-link', 'root-link', 'fifo'):
            with self.subTest(mutant=mutant), tempfile.TemporaryDirectory(prefix='subyard-evidence-object-', dir='/tmp') as directory:
                root = Path(directory)/'evidence'
                root.mkdir(mode=0o700)
                outside = Path(directory)/'outside'
                outside.mkdir(mode=0o700)
                if mutant == 'root-link':
                    root.rmdir(); root.symlink_to(outside, target_is_directory=True)
                elif mutant == 'directory-link': (root/'foreign').symlink_to(outside, target_is_directory=True)
                elif mutant == 'symlink': (root/'foreign.json').symlink_to(outside/'missing')
                elif mutant == 'hardlink':
                    write(outside/'target', b'foreign')
                    os.link(outside/'target', root/'foreign.json')
                else: os.mkfifo(root/'foreign', 0o600)
                with self.assertRaises(RuntimeError): capture(root)
        with tempfile.TemporaryDirectory(prefix='subyard-evidence-destination-', dir='/tmp') as directory:
            for destination in ('/absolute.applied.json', '../outside.applied.json', 'step.unknown.json', '.hidden.applied.json'):
                with self.subTest(destination=destination), self.assertRaises(RuntimeError): capture(Path(directory), [destination])


if __name__ == '__main__':
    unittest.main()
