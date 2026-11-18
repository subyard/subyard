import hmac
import json
import os

import records


DEFAULT_CAPTURE = records.POLICY_VERSION + ':71626c7f614522a2d0ffb6b15ef86fc45334322dd94e86f704a104b545b9c6db'


def prepare(root):
    root = records.private_directory(root)
    key = bytes(range(32))
    fd = os.open(root/'.identity-key', os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(key)
    value = dict(attributes=[*records.BODY_RULES['session_attributes'][1], 'subyard.agent_run.id',
                             'subyard.agent_run.evidence'], policy_version=DEFAULT_CAPTURE,
                 key_verifier=hmac.digest(key, b'subyard.telemetry/identity-key/v1', 'sha256').hex())
    signature = hmac.digest(key, b'spool-policy/v1\0'+records.compact_json(value), 'sha256').hex()
    records.atomic_json(root/'spool-policy.json', dict(settings=value, signature=signature))
    records.atomic_json(root/'producer.json', dict(source_id='fixture-producer'))
    fd = os.open(root/'spool-loss', os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
    try:
        os.fchmod(fd, 0o600)
        os.posix_fallocate(fd, 0, 2*records.MARKER_SLOT)
    finally:
        os.close(fd)
    return root


def spooled(root):
    for path in sorted((root/'spool').glob('*')):
        if path.suffix not in ('.open', '.sealed'):
            continue
        frames = path.read_bytes().split(b'\n')[1:-1]
        yield from (json.loads(frame)['record'] for frame in frames)
