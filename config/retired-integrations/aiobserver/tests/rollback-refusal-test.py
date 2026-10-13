#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('refusal',Path(__file__).with_name('rollback-refusal.py'))
refusal=importlib.util.module_from_spec(spec)
spec.loader.exec_module(refusal)


class RefusalTest(unittest.TestCase):
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
