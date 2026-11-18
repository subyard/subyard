#!/usr/bin/env python3
"""Exercise both installed agent documents on the disposable local stage."""
import hashlib
import json
import os
from pathlib import Path
import pwd
import stat
import subprocess
import sys
import tomllib


def main():
    phase, stage = sys.argv[1:]
    assert os.getuid() == 0 and Path('/run/subyard-e2e-lease.json').is_file()
    account = pwd.getpwnam('dev')
    home = Path(account.pw_dir)
    staged = Path(stage)
    installed = Path('/usr/local/lib/subyard-telemetry')
    state = '/var/lib/subyard/config-materialization'
    for name, relative, file_format, section, field, prior, active, initial, template, conflict in (
        ('claude', '.claude/settings.json', 'json', 'env', 'OTEL_LOGS_EXPORTER', 'console', 'otlp',
         b'{"fixture_unrelated":true,"env":{"OTEL_LOGS_EXPORTER":"console"}}', b'{"fixture_template":true}',
         b'{"env":{"OTEL_LOGS_EXPORTER":"console"}}'),
        ('codex', '.codex/config.toml', 'toml', 'otel', 'log_user_prompt', True, False,
         b'fixture_unrelated = true\n[otel]\nlog_user_prompt = true\n', b'fixture_template = true\n',
         b'[otel]\nlog_user_prompt = true\n'),
    ):
        path = home/relative
        if phase == 'seed':
            path.parent.mkdir(mode=0o700, exist_ok=True)
            assert not path.parent.is_symlink() and not path.is_symlink()
            if path.exists():
                info = path.stat()
                assert stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_uid == account.pw_uid, name
            os.chown(path.parent, account.pw_uid, account.pw_gid)
            path.write_bytes(initial)
            os.chown(path, account.pw_uid, account.pw_gid)
            path.chmod(0o644)
            continue
        assert stat.S_IMODE(path.stat().st_mode) == 0o600, name
        content = path.read_text()
        document = (json.loads if file_format == 'json' else tomllib.loads)(content)
        assert document['fixture_unrelated'] is True, name
        if phase == 'disabled':
            assert document == {'fixture_unrelated': True, section: {field: prior}}, name
            continue
        assert phase == 'retire' and document[section][field] == active, name
        assert 'Bearer ' in content, name
        for asset in ('guest_json.py', 'native.py', 'tomli_w.py'):
            assert (installed/asset).read_bytes() == (staged/asset).read_bytes(), asset
        program = (installed/'guest_json.py').read_text().replace('@STATE_ROOT@', state).replace('@STATE_UID@', '0').replace('@FORMAT@', file_format)
        if file_format == 'toml':
            program = '_toml_writer = {}\nexec('+repr((installed/'tomli_w.py').read_text())+', _toml_writer)\n'+program

        def materialize(mode, payload):
            digest = hashlib.sha256(payload).hexdigest()
            return subprocess.run([sys.executable, '-B', '-c', program, mode, 'dev', str(path),
                                   str(account.pw_uid), digest, str(home), digest], input=payload, capture_output=True)

        path.chmod(0o644)
        refused = materialize('apply', conflict)
        assert refused.returncode == 2 and b'configuration contribution conflicts at' in refused.stderr, name
        assert path.read_text() == content and stat.S_IMODE(path.stat().st_mode) == 0o600, name
        assert materialize('apply', template).returncode == 0, name
        for _ in range(2):
            path.chmod(0o644)
            retired = materialize('retire', b'{}' if file_format == 'json' else b'')
            assert retired.returncode == 0 and json.loads(retired.stdout)['converged'], name
            assert stat.S_IMODE(path.stat().st_mode) == 0o600, name
            content = path.read_text()
            assert 'fixture_template' not in content and 'Bearer ' in content, name
    if phase == 'disabled':
        print('ok: installed Claude and Codex configuration permissions, template retirement and exporter restoration')


if __name__ == '__main__':
    main()
