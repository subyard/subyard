#!/usr/bin/env python3
"""Check the guest policy lifecycle and preserve the developer's home documents."""
import json
import os
from pathlib import Path
import pwd
import stat
import sys
import tomllib

HELPER = '/usr/local/lib/subyard-telemetry/otlp-headers'


def main():
    phase, = sys.argv[1:]
    assert os.getuid() == 0 and Path('/run/subyard-e2e-lease.json').is_file()
    account = pwd.getpwnam('dev')
    home = Path(account.pw_dir)
    token = Path('/var/lib/subyard/telemetry-private/auth.token').read_bytes().strip() if phase == 'enabled' else None
    for relative, policy, initial, parse, section in (
        ('.claude/settings.json', '/etc/claude-code/managed-settings.d/60-subyard-telemetry.json',
         b'{"env":{"OTEL_LOGS_EXPORTER":"console"}}\n', json.loads, 'env'),
        ('.codex/config.toml', '/etc/codex/managed_config.toml',
         b'[otel]\nlog_user_prompt = true\n', lambda data: tomllib.loads(data.decode()), 'otel'),
    ):
        document, managed = home/relative, Path(policy)
        if phase == 'seed':
            assert not managed.exists() and not managed.is_symlink()
            document.parent.mkdir(mode=0o700, exist_ok=True)
            assert not document.parent.is_symlink() and not document.is_symlink()
            if document.exists():
                info = document.stat()
                assert stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_uid == account.pw_uid
            os.chown(document.parent, account.pw_uid, account.pw_gid)
            document.write_bytes(initial)
            os.chown(document, account.pw_uid, account.pw_gid)
            document.chmod(0o400)
            continue
        assert document.read_bytes() == initial
        assert stat.S_IMODE(document.stat().st_mode) == 0o400
        if phase == 'disabled':
            assert not managed.exists()
        else:
            assert phase == 'enabled'
            info = managed.stat()
            assert (info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)) == (0, account.pw_gid, 0o640)
            content = managed.read_bytes()
            # Exporters read the bearer through the root-owned helper only.
            assert token not in content and b'Authorization' not in content
            helper = Path(HELPER).stat()
            assert (helper.st_uid, stat.S_IMODE(helper.st_mode)) == (0, 0o755)
            document = parse(content)
            fields = document[section]
            if section == 'env':
                assert fields['OTEL_LOGS_EXPORTER'] == 'otlp'
                assert document['otelHeadersHelper'] == HELPER
            else:
                assert fields['log_user_prompt'] is False and fields['log_agent_responses'] is True
                assert fields['headers_helper'] == HELPER
                for key in ('exporter', 'metrics_exporter', 'trace_exporter'):
                    assert fields[key]['otlp-http']['tls'] == {}
    if phase == 'disabled':
        print('ok: managed policies removed and Claude and Codex home documents preserved')


if __name__ == '__main__':
    main()
