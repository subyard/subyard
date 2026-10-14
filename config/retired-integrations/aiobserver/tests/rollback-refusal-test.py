#!/usr/bin/env python3
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('refusal',Path(__file__).with_name('rollback-refusal.py'))
refusal=importlib.util.module_from_spec(spec)
spec.loader.exec_module(refusal)


class RefusalTest(unittest.TestCase):
    def test_code_transport_does_not_require_operator_access_to_source(self):
        program=Path(__file__).with_name('rollback-refusal.py').read_text()
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root=Path(temporary)
            source=root/'source'; source.mkdir(mode=0o700)
            helper=source/'refusal.py'; helper.write_text(program)
            config=root/'operator-config'; config.mkdir(mode=0o700)
            (config/'history').write_text('retained')
            runtime=root/'operator-runtime'; runtime.mkdir(mode=0o700)
            (runtime/'current').symlink_to('candidate')
            (runtime/'previous').symlink_to('legacy')
            command=[sys.executable,'-c',"import sys; print('code=rollback-incompatible',file=sys.stderr); sys.exit(1)"]
            arguments=[str(config),str(runtime),*command]
            before=refusal.snapshot(config,runtime)
            os.chmod(source,0)
            try:
                if os.geteuid()!=0:
                    inaccessible=subprocess.run([sys.executable,str(helper),*arguments],capture_output=True)
                    self.assertNotEqual(inaccessible.returncode,0)
                transported=subprocess.run([sys.executable,'-c',program,*arguments],capture_output=True)
                self.assertEqual(transported.returncode,0,transported.stderr.decode())
                self.assertIn(b'refuses epoch-1 rollback without mutation',transported.stdout)
                self.assertEqual(refusal.snapshot(config,runtime),before)
                self.assertEqual(source.stat().st_mode&0o777,0)
            finally:
                os.chmod(source,0o700)

    def test_typed_refusal_preserves_config_and_links(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root=Path(temporary)
            config=root/'config'; config.mkdir()
            (config/'history').write_text('retained')
            runtime=root/'runtime'; runtime.mkdir()
            (runtime/'current').symlink_to('candidate')
            (runtime/'previous').symlink_to('legacy')
            command=[sys.executable,'-c',"import sys; print('code=rollback-incompatible',file=sys.stderr); sys.exit(1)"]
            refusal.verify(config,runtime,command)
            for program in ("import sys; sys.exit(1)","print('code=rollback-incompatible')",
                            "from pathlib import Path; import sys; Path(sys.argv[1]).write_text('changed'); print('code=rollback-incompatible'); sys.exit(1)",
                            "from pathlib import Path; import sys; p=Path(sys.argv[2]); p.unlink(); p.symlink_to('legacy'); print('code=rollback-incompatible'); sys.exit(1)"):
                with self.subTest(program=program), self.assertRaises(ValueError):
                    refusal.verify(config,runtime,[sys.executable,'-c',program,str(config/'history'),str(runtime/'current')])


if __name__=='__main__': unittest.main()
