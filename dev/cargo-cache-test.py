#!/usr/bin/env python3
"""Exercise native Cargo reuse, invalidation and caller-owned final artifacts."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest

from cargo_cache import environment


@unittest.skipUnless(shutil.which('cargo') and shutil.which('rustc'), 'Rust toolchain required')
class CargoCacheTest(unittest.TestCase):
    def test_reuse_invalidation_parallel_outputs_and_profiles(self):
        with tempfile.TemporaryDirectory(dir='/tmp', prefix='subyard-cargo-cache-') as temporary:
            root = Path(temporary)
            dependency = root/'dependency'
            (dependency/'src').mkdir(parents=True)
            (dependency/'Cargo.toml').write_text('[package]\nname="cache-dependency"\nversion="0.1.0"\nedition="2021"\n')
            (dependency/'src/lib.rs').write_text('pub fn value() -> u32 { 7 }\n')
            for command in [['git', 'init', '-q'], ['git', 'add', '.'], ['git', '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.test', 'commit', '-qm', 'fixture']]:
                subprocess.run(command, cwd=dependency, check=True)
            for name in ['a', 'b']:
                crate = root/name
                (crate/'src').mkdir(parents=True)
                (crate/'dev').mkdir()
                shutil.copyfile(Path(__file__).with_name('cargo-workspace-rustc.sh'), crate/'dev/cargo-workspace-rustc.sh')
                (crate/'dev/cargo-workspace-rustc.sh').chmod(0o755)
                (crate/'dep/src').mkdir(parents=True)
                (crate/'dep/Cargo.toml').write_text('[package]\nname="cache-dependency"\nversion="0.1.0"\nedition="2021"\n')
                (crate/'dep/src/lib.rs').write_text('pub fn value() -> u32 { 7 }\n')
                (crate/'build.rs').write_text('fn main() { std::fs::write(std::path::Path::new(&std::env::var("OUT_DIR").unwrap()).join("generated.rs"), format!("fn generated() -> &\'static str {{ {:?} }}", std::env::var("CARGO_MANIFEST_DIR").unwrap())).unwrap(); }\n')
                (crate/'Cargo.toml').write_text('[package]\nname="cache-fixture"\nversion="0.1.0"\nedition="2021"\n[features]\nextra=[]\n[dependencies]\ncache-dependency={git="'+dependency.as_uri()+'"}\n')
                (crate/'src/main.rs').write_text('include!(concat!(env!("OUT_DIR"),"/generated.rs")); fn main() { assert!(cfg!(debug_assertions)); assert_eq!(cache_dependency::value(),7); assert_eq!(generated(),env!("CARGO_MANIFEST_DIR")); println!("{}:{}", env!("CARGO_MANIFEST_DIR"), cfg!(feature="extra")); }\n')
            base = dict(os.environ, SUBYARD_CARGO_BUILD_ROOT=str(root/'shared'), CARGO_HOME=str(root/'cargo-home'))
            for key in ['CARGO_BUILD_BUILD_DIR', 'CARGO_TARGET_DIR', 'RUSTFLAGS', 'CARGO_ENCODED_RUSTFLAGS']:
                base.pop(key, None)
            subprocess.run(['cargo', 'fetch', '--manifest-path', str(root/'a/Cargo.toml')], env=base, check=True, capture_output=True)
            measurements = []

            def build(name, suffix, flags=(), extra=None):
                env = environment(root/suffix, snapshot=True, env=base, source_root=root/name, manifest_path=root/name/'Cargo.toml')
                env.update(extra or {})
                start = time.monotonic()
                result = subprocess.run(['cargo', 'build', '--offline', '--manifest-path', str(root/name/'Cargo.toml'), '-v', *flags], env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                binary = root/suffix/'debug/cache-fixture'
                output = subprocess.check_output([str(binary)], text=True).strip()
                self.assertEqual(output, str(root/name)+':'+str('--features' in flags).lower())
                measurements.append(dict(name=name, suffix=suffix, seconds=round(time.monotonic()-start, 4), bytes=sum(p.stat().st_size for p in Path(env['CARGO_BUILD_BUILD_DIR']).rglob('*') if p.is_file())))
                return result.stderr, binary, env

            _, first, env = build('a', 'final-a')
            digest = hashlib.sha256(first.read_bytes()).hexdigest()
            # The workspace wrapper binds built-in manifest paths to each checkout.
            reused, _, _ = build('b', 'final-b')
            self.assertNotIn('Compiling cache-dependency', reused)
            fresh, _, _ = build('b', 'final-b-again')
            self.assertNotIn('Compiling cache-fixture', fresh)
            build('a', 'features', ('--features', 'extra'))
            build('b', 'flags', extra={'RUSTFLAGS': '--cfg custom_cache_fixture'})
            for name in ['a', 'b']:
                build(name, 'remap-'+name, extra={'RUSTFLAGS': '--remap-path-prefix='+str(root/name)+'=public-source'})
            # Source changes invalidate the native cache, preserving earlier final bytes.
            source = root/'b/src/main.rs'
            source.write_text(source.read_text()+'\n// changed source\n')
            changed, _, _ = build('b', 'changed')
            self.assertIn('Compiling cache-fixture', changed)
            commands = []
            for name in ['a', 'b']:
                parallel_env = environment(root/('parallel-'+name), snapshot=True, env=base, source_root=root/name, manifest_path=root/name/'Cargo.toml')
                commands.append(subprocess.Popen(['cargo', 'build', '--offline', '--manifest-path', str(root/name/'Cargo.toml')], env=parallel_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True))
            self.assertEqual(hashlib.sha256(first.read_bytes()).hexdigest(), digest)
            shutil.rmtree(root/'final-b-again')
            for command in commands:
                _, stderr = command.communicate(timeout=60)
                self.assertEqual(command.returncode, 0, stderr)
            shutil.rmtree(root/'parallel-a')
            self.assertEqual(hashlib.sha256(first.read_bytes()).hexdigest(), digest)
            self.assertTrue((root/'parallel-b/debug/cache-fixture').is_file())
            for label, settings in [('full', {'CARGO_INCREMENTAL':'1','CARGO_PROFILE_DEV_DEBUG':'2'}), ('lines', {'CARGO_INCREMENTAL':'1','CARGO_PROFILE_DEV_DEBUG':'line-tables-only'}), ('snapshot-lines', {'CARGO_INCREMENTAL':'0','CARGO_PROFILE_DEV_DEBUG':'line-tables-only'})]:
                build('a', label, extra=settings)
            first_run = environment(root/'runs', env=base, manifest_path=root/'a/Cargo.toml', run_target=True)
            second_run = environment(root/'runs', env=base, manifest_path=root/'a/Cargo.toml', run_target=True)
            self.assertNotEqual(first_run['CARGO_TARGET_DIR'], second_run['CARGO_TARGET_DIR'])
            self.assertEqual(environment(root/'ignored', env=dict(base,CARGO_TARGET_DIR=str(root/'explicit')), manifest_path=root/'a/Cargo.toml', run_target=True)['CARGO_TARGET_DIR'], str(root/'explicit'))
            self.assertEqual(environment(root/'ordinary', env=base, manifest_path=root/'a/Cargo.toml').get('CARGO_INCREMENTAL'), base.get('CARGO_INCREMENTAL'))
            alternate = dict(base, RUSTC=str(root/'rustc-other'))
            wrapper = root/'rustc-other'
            wrapper.write_text('#!/bin/sh\nif [ "$1" = -vV ]; then printf "rustc fixture different compiler\\n"; else exec rustc "$@"; fi\n');wrapper.chmod(0o755)
            self.assertNotEqual(environment(root/'other', env=alternate, manifest_path=root/'a/Cargo.toml')['CARGO_BUILD_BUILD_DIR'], env['CARGO_BUILD_BUILD_DIR'])
            # Non-member local crates get checkout-specific flags: their built-in
            # manifest paths are not covered by Cargo's workspace wrapper.
            for name in ['a', 'b']:
                manifest = root/name/'Cargo.toml'
                manifest.write_text(manifest.read_text().replace('git="'+dependency.as_uri()+'"', 'path="dep"'))
                local = root/name/'dep/src/lib.rs'
                local.write_text('pub fn value() -> u32 { assert!(env!("CARGO_MANIFEST_DIR").ends_with("/'+name+'/dep")); 7 }\n')
                build(name, 'local-'+name)
            print(json.dumps(measurements, sort_keys=True))


if __name__ == '__main__':
    unittest.main()
