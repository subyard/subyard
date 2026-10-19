#!/usr/bin/env python3
"""Host-free package boundary regressions; real DEB validation is a separate check."""
import importlib.util
from pathlib import Path
import os
import io
import tarfile
import tempfile
import shutil
import subprocess
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('package_veranda',Path(__file__).with_name('package-veranda.py'))
package=importlib.util.module_from_spec(spec);spec.loader.exec_module(package)


check_spec=importlib.util.spec_from_file_location('check_veranda_package',Path(__file__).with_name('check-veranda-package.py'))
checker=importlib.util.module_from_spec(check_spec);check_spec.loader.exec_module(checker)


class PackageBoundaryTest(unittest.TestCase):
    def test_only_stable_product_versions_are_accepted(self):
        for version in ['0.0.0','1.2.3','100.200.300']:
            self.assertEqual(package.stable_version(version),version)
        for version in ['', 'v1.2.3','01.2.3','1.2.3-dev','1.2.3+build','1.2','1.2.3\nDepends: injected','../../1.2.3','1.2.3;true','9'*65+'.0.0']:
            with self.assertRaises(ValueError): package.stable_version(version)

    def test_output_cannot_escape_into_sources_or_symlinked_directory(self):
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-package-test-') as temporary:
            root=Path(temporary);checkout=root/'repo';checkout.mkdir(mode=0o700)
            (root/'alias').symlink_to(checkout,target_is_directory=True)
            with patch.object(package,'ROOT',checkout):
                for path in [checkout,checkout/'veranda/output',root,root/'alias/build']:
                    with self.assertRaises(ValueError): package.output_path(path)
                self.assertEqual(package.output_path(checkout/'.build/release'),checkout/'.build/release')

    def test_output_traversal_is_rejected_before_any_source_write(self):
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-output-test-') as temporary:
            root=Path(temporary);checkout=root/'repo';checkout.mkdir(mode=0o700)
            with patch.object(package,'ROOT',checkout):
                for path in [checkout/'.build/../veranda',checkout/'.build/nested/../../dev',checkout/'.build/..',checkout/'.build/../../repo']:
                    with self.subTest(path=path):
                        with self.assertRaises(ValueError):package.output_path(path)
            self.assertEqual(list(checkout.iterdir()),[])
            target=checkout/'.build/target';target.mkdir(parents=True)
            (target/'release').symlink_to(checkout,target_is_directory=True)
            with patch.object(package,'ROOT',checkout):
                with self.assertRaises(ValueError):package.output_path(target)

    def test_owning_source_symlinks_cannot_disappear_from_inventory(self):
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-inventory-test-') as temporary:
            root=Path(temporary);checkout=root/'repo';checkout.mkdir(mode=0o700)
            external=root/'external';external.mkdir();(external/'lib.rs').write_text('pub fn fixture() {}\n')
            for name in ['client','desktop']:
                crate=checkout/'veranda'/name;crate.mkdir(parents=True);(crate/'Cargo.toml').write_text('[package]\nname="fixture"\nversion="0.1.0"\n')
            (checkout/'veranda/client/src').symlink_to(external,target_is_directory=True)
            # Actual rg's default behavior omits the linked directory; no mocked enumeration.
            enumerator=checkout/'tests/helpers/source-files.sh';enumerator.parent.mkdir(parents=True)
            enumerator.write_text('cd "$1"\nexec rg --files --hidden --null --no-require-git\n')
            with patch.object(package,'ROOT',checkout):
                with self.assertRaises(ValueError):package.inventory()

    def test_plain_and_notices_reject_symlinked_parent_directories(self):
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-parent-test-') as temporary:
            root=Path(temporary);external=root/'external';external.mkdir()
            (external/'LICENSE').write_text('External fixture MIT text\n')
            (external/'Cargo.toml').write_text('[package]\n')
            alias=root/'alias';alias.symlink_to(external,target_is_directory=True)
            with self.assertRaises(ValueError):package.plain(alias/'LICENSE')
            metadata={'resolve':{'nodes':[{'id':'fixture@1'}]},'packages':[{'id':'fixture@1','name':'fixture','version':'1.0.0','manifest_path':str(alias/'Cargo.toml'),'source':'registry+https://github.com/rust-lang/crates.io-index','license':'MIT','license_file':None}]}
            with self.assertRaises(ValueError):package.notices(metadata,root/'notices')

    def test_notices_use_actual_text_and_identity_without_cache_paths(self):
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-notice-test-') as temporary:
            root=Path(temporary);crate=root/'cache/crate';crate.mkdir(parents=True)
            license=crate/'LICENSE';license.write_text('Actual copyright and license fixture\n');license.chmod(0o644)
            (crate/'Cargo.toml').write_text('[package]\nname="crate"\nversion="1.0.0"\n')
            metadata={'resolve':{'nodes':[{'id':'crate@1'}]},'packages':[{'id':'crate@1','name':'crate','version':'1.0.0','manifest_path':str(crate/'Cargo.toml'),'source':'registry+https://github.com/rust-lang/crates.io-index','license':'MIT','license_file':None}]}
            report=package.notices(metadata,root/'notices')
            notice=root/'notices/rust/crate-1.0.0/LICENSE'
            self.assertEqual(notice.read_bytes(),license.read_bytes())
            self.assertEqual(notice.stat().st_mode & 0o777,0o644)
            self.assertNotIn(str(root),(root/'notices/rust-dependencies.json').read_text())
            self.assertEqual(report[0]['notices'][0]['sha256'],package.sha(license))
            metadata['packages'][0]['license_file']='../../outside'
            with self.assertRaises(ValueError): package.notices(metadata,root/'rejected')
            metadata['packages'][0]['license_file']=None
            license.unlink()
            with self.assertRaises(ValueError): package.notices(metadata,root/'missing')

    @unittest.skipUnless(all(shutil.which(tool) for tool in ['cc','dpkg','dpkg-deb','dpkg-shlibdeps']), 'native Debian package fixture tools')
    def test_published_sdk_control_and_shlibs_are_used_without_installation(self):
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-sdk-test-') as temporary:
            root=Path(temporary);sdk=root/'sdk';archives=root/'archives';archives.mkdir()
            arch=subprocess.check_output(['dpkg','--print-architecture'],text=True).strip()
            library=sdk/'usr/lib/libfixture.so.0'
            package.write(root/'library.c',b'int fixture(void) { return 1; }\n')
            package.write(root/'main.c',b'extern int fixture(void); int main(void) { return fixture()-1; }\n')
            library.parent.mkdir(parents=True)
            subprocess.run(['cc','-shared','-fPIC','-Wl,-soname,libfixture.so.0',str(root/'library.c'),'-o',str(library)],check=True)
            binary=root/'debian/subyard-veranda/usr/bin/subyard-veranda';binary.parent.mkdir(parents=True)
            subprocess.run(['cc',str(root/'main.c'),'-L'+str(library.parent),'-l:libfixture.so.0','-o',str(binary)],check=True)
            package.write(sdk/'DEBIAN/control',f'Package: libfixture0\nVersion: 1.2.3\nArchitecture: {arch}\nMaintainer: Fixture <fixture@example.test>\nDescription: synthetic package fixture\n'.encode())
            (sdk/'DEBIAN').chmod(0o755)
            package.write(sdk/'DEBIAN/shlibs',b'libfixture 0 libfixture0 (>= 1.2.0)\n')
            archive=archives/'fixture.deb'
            subprocess.run(['dpkg-deb','--root-owner-group','--build',str(sdk),str(archive)],check=True,stdout=subprocess.DEVNULL)
            deps,inputs=package.shared_dependencies(binary,root,archives)
            self.assertIn('libfixture0 (>= 1.2.0)',deps)
            self.assertEqual(inputs,[{'package':'libfixture0','version':'1.2.3','architecture':arch,'archive_sha256':package.sha(archive)}])
            self.assertTrue((root/'sdk-packages/libfixture0/DEBIAN/shlibs').is_file())

    def test_archive_rejects_escape_wrong_owner_mode_and_links(self):
        for name,mode,owner,kind in [('../escape',0o644,0,tarfile.REGTYPE),('usr/bin/subyard-veranda',0o755,1000,tarfile.REGTYPE),('usr/share/doc/file',0o666,0,tarfile.REGTYPE),('usr/share/doc/link',0o644,0,tarfile.SYMTYPE)]:
            data=io.BytesIO()
            with tarfile.open(fileobj=data,mode='w') as archive:
                member=tarfile.TarInfo(name);member.mode=mode;member.uid=owner;member.gid=0;member.type=kind
                if kind==tarfile.REGTYPE:
                    member.size=7;archive.addfile(member,io.BytesIO(b'fixture'))
                else:
                    member.linkname='/outside';archive.addfile(member)
            with patch.object(checker.subprocess,'check_output',return_value=data.getvalue()):
                with self.assertRaises(ValueError):checker.checked_archive(Path('fixture.deb'),'fsys')

    def test_plain_input_refuses_symlinks_and_hardlinks(self):
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-input-test-') as temporary:
            root=Path(temporary);plain=root/'plain';plain.write_text('fixture');plain.chmod(0o644)
            link=root/'link';link.symlink_to(plain)
            with self.assertRaises(ValueError): package.plain(link)
            os.link(plain,root/'hardlink')
            with self.assertRaises(ValueError): package.plain(plain)

    def test_ignored_rust_and_compiled_include_inputs_must_be_in_inventory(self):
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-omitted-test-') as temporary:
            checkout=Path(temporary)/'repo';crate=checkout/'veranda/client';crate.mkdir(parents=True)
            manifest=crate/'Cargo.toml';manifest.write_text('[package]\n')
            module=crate/'hidden.rs';module.write_text('pub fn fixture() {}\n')
            metadata={'packages':[{'manifest_path':str(manifest),'source':None,'targets':[{'src_path':str(module)}]}]}
            with patch.object(package,'ROOT',checkout):
                with self.assertRaises(ValueError):package.workspace_inputs(metadata,[{'path':'veranda/client/Cargo.toml'}])
                inputs=[{'path':'veranda/client/hidden.rs'}]
                include=crate/'ignored text.txt';include.write_text('fixture include\n')
                depfile=Path(temporary)/'veranda.d'
                depfile.write_text('/tmp/generated: '+str(module)+' '+str(include).replace(' ',r'\ ')+'\n')
                with self.assertRaises(ValueError):package.compiled_inputs(depfile,inputs,checkout/'.build/target')
                inputs.append({'path':'veranda/client/ignored text.txt'})
                self.assertEqual(package.compiled_inputs(depfile,inputs,checkout/'.build/target'),['veranda/client/hidden.rs','veranda/client/ignored text.txt'])

    def test_separate_build_generated_inputs_preserve_public_source_validation(self):
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-build-root-test-') as temporary:
            root=Path(temporary);checkout=root/'repo';checkout.mkdir()
            module=checkout/'main.rs';module.write_text('fn main() {}');module.chmod(0o644)
            build=root/'shared-build';build.mkdir()
            generated=build/'generated.rs';generated.write_text('pub fn generated() {}');generated.chmod(0o644)
            depfile=root/'veranda.d';depfile.write_text('veranda: '+str(module)+' '+str(generated)+'\n')
            with patch.object(package,'ROOT',checkout):
                self.assertEqual(package.compiled_inputs(depfile,[{'path':'main.rs'}],root/'target',build),['main.rs'])
                with self.assertRaises(ValueError):package.compiled_inputs(depfile,[],root/'target',build)
                foreign=root/'foreign.rs';foreign.write_text('foreign');foreign.chmod(0o644)
                depfile.write_text('veranda: '+str(module)+' '+str(foreign)+'\n')
                with self.assertRaises(ValueError):package.compiled_inputs(depfile,[{'path':'main.rs'}],root/'target',build)
                generated.unlink();generated.symlink_to(module)
                depfile.write_text('veranda: '+str(module)+' '+str(generated)+'\n')
                with self.assertRaises(ValueError):package.compiled_inputs(depfile,[{'path':'main.rs'}],root/'target',build)

    @unittest.skipUnless(all(shutil.which(tool) for tool in ['bash','dpkg','dpkg-deb']), 'Bash/Debian orchestration fixture tools')
    def test_normal_package_cost_sequence_precedes_fixture_tools(self):
        # Execute real helper orchestration with apt/lease/GTK boundaries replaced
        # by fixture-owned commands. Nothing installs or acquires a lease.
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-cost-order-') as temporary:
            root=Path(temporary);bin=root/'bin';bin.mkdir();events=root/'events'
            def executable(name,text):
                file=bin/name;file.write_text('#!/bin/bash\n'+text);file.chmod(0o755)
            executable('sudo','if [ "$1" = -n ]; then shift; fi\nif [ "$1" = test ]; then exit 0; fi\nexec "$@"\n')
            executable('apt-get','case " $* " in *" openbox "*) printf "harness\\n" >> "$ORDER_EVENTS";; esac\n')
            executable('dpkg-query','if [ "$2" = subyard-veranda ]; then exit 1; fi\nprintf "base\\t10\\tinstall ok installed\\n"\n')
            debs=[]
            arch=subprocess.check_output(['dpkg','--print-architecture'],text=True).strip()
            for version in ['0.1.0','0.1.1']:
                package.write(root/'stage/DEBIAN/control',f'Package: subyard-veranda\nVersion: {version}\nArchitecture: {arch}\nMaintainer: Fixture <fixture@example.test>\nDescription: fixture\n'.encode())
                (root/'stage/DEBIAN').chmod(0o755)
                deb=root/f'fixture-{version}.deb';debs.append(str(deb))
                subprocess.run(['dpkg-deb','--root-owner-group','--build',str(root/'stage'),str(deb)],check=True,stdout=subprocess.DEVNULL)
            source=Path(__file__).parent.joinpath('e2e/veranda-package.sh').read_text()
            prefix,tail=source.split('\nphase install "$BASE"',1)
            runner=root/'repo/dev/e2e/veranda-package.sh';runner.parent.mkdir(parents=True)
            runner.write_text(prefix+'\nphase() { printf "phase:%s:%s\\n" "$1" "${4:-legacy}" >> "$ORDER_EVENTS"; }\nphase install "$BASE"'+tail)
            environment=dict(os.environ,PATH=str(bin)+os.pathsep+os.environ['PATH'],SUBYARD_E2E_VM='1',ORDER_EVENTS=str(events),TMPDIR='/tmp')
            subprocess.run(['bash',str(runner),*debs],check=True,env=environment,stdout=subprocess.DEVNULL,stderr=subprocess.PIPE)
            self.assertEqual(events.read_text().splitlines(),['phase:install:package','phase:upgrade:package','phase:rollback:package','harness','phase:install:gtk','phase:upgrade:gtk','phase:rollback:gtk'])

    def test_cost_metrics_keep_clean_baseline_and_upgrade_rollback_context(self):
        source=Path(__file__).parent.joinpath('e2e/veranda-package.sh').read_text()
        metrics=source.split("<<'PYMETRICS'\n",1)[1].split('\nPYMETRICS',1)[0]
        with tempfile.TemporaryDirectory(dir='/tmp',prefix='veranda-cost-metrics-') as temporary:
            root=Path(temporary)
            def snapshot(name,packages):
                path=root/name;path.write_text(''.join(f'{package}\t{size}\tinstall ok installed\n' for package,size in packages.items()));return path
            baseline=snapshot('clean',{'libbase':10})
            first=snapshot('installed',{'libbase':10,'libgtk':100,'libcairo':50,'openssh-client':20,'subyard-veranda':40})
            next=snapshot('upgraded',{'libbase':10,'libgtk':110,'libcairo':50,'openssh-client':20,'libnew':30,'subyard-veranda':60})
            rollback=snapshot('rolled-back',{'libbase':10,'libgtk':110,'libcairo':50,'openssh-client':20,'libnew':30,'subyard-veranda':40})
            archive=root/'fixture.deb';archive.write_bytes(b'fixture');log=root/'apt.log';log.write_text('')
            for phase,before,after,new,phase_new,delta in [('install',baseline,first,170,170,170),('upgrade',first,next,210,30,40),('rollback',next,rollback,210,0,0)]:
                output=subprocess.check_output(['python3','-',str(baseline),str(before),str(after),str(archive),str(log),phase],input=metrics,text=True)
                fields=dict(field.split('=',1) for field in output.splitlines()[0].split('\t')[2:])
                self.assertEqual(fields['context'],'normal-lifecycle-before-fixture-tools')
                self.assertEqual(int(fields['newOSDependencyInstalledKiB']),new)
                self.assertEqual(int(fields['phaseNewOSDependencyInstalledKiB']),phase_new)
                self.assertEqual(int(fields['phaseOSInstalledSizeDeltaKiB']),delta)


if __name__=='__main__': unittest.main()
