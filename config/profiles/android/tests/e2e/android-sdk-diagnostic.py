#!/usr/bin/env python3
"""One fixture-only SDK retry; the original image failure remains authoritative."""
import argparse
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile
import threading
import urllib.parse


def public_error(output):
    text = output.decode('utf-8', 'replace')
    def url(match):
        try:
            parsed = urllib.parse.urlsplit(match[0])
            return parsed.scheme+'://'+(parsed.hostname or '<host>')+parsed.path
        except ValueError:
            return '<url>'
    text = re.sub(r'https?://[^\s<>\"\']+', url, text)
    text = re.sub(r'(?i)\b(?:authorization\s*[:=]\s*)?bearer\s+[^\s,;]+', '<credential>', text)
    text = re.sub(r'(?i)\b(?:authorization|api[_-]?key|token|password|cookie)[\"\']?\s*[:=]\s*[^\s,;]+', '<credential>', text)
    text = re.sub(r'(?<![A-Za-z0-9:/])/[A-Za-z0-9_./-]+', '<path>', text)
    return text if len(text) <= 4096 else text[:1000]+'\n<truncated>\n'+text[-3000:]


def diagnose(module, config, directory, api=36):
    if type(api) is not int or api not in (35, 36):
        raise ValueError('SDK diagnostic API must be 35 or 36')
    # Never construct Images on the live pool root: its constructor prunes stages.
    images = module.Images(directory/'images', config['sdk_root'], config.get('java_home', '/opt/jdk-17'),
                           (2*config['size']+1)*1024**3)
    normalizer = module.Pool.__new__(module.Pool)
    normalizer.config = config
    image = images.resolve(normalizer.normalize(dict(api=api)))
    result = dict(schema='subyard.android.sdk-diagnostic/v1', api=api, original_failure_preserved=True)
    try:
        images.ensure(image, threading.Event())
    except module.PoolError as error:
        result.update(retry='failed', sdk_output_present=False)
        trace = error.__traceback__
        while trace:
            if trace.tb_frame.f_code is module.Images.ensure.__code__:
                output = trace.tb_frame.f_locals.get('output')
                if isinstance(output, bytes) and len(output) <= 16385:
                    process = trace.tb_frame.f_locals.get('process')
                    status = getattr(process, 'returncode', None)
                    result.update(sdk_output_present=True, sdk_exit_code=status if type(status) is int else None,
                                  sdk_output_sha256=hashlib.sha256(output).hexdigest(), sdk_error=public_error(output))
            trace = trace.tb_next
    else:
        result.update(retry='succeeded', sdk_output_present=False)
    return result


def interrupted(_signal, _frame):
    # Unwind Images.ensure so its finally kills the SDK's separate process group.
    raise InterruptedError('SDK diagnostic interrupted')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--api', type=int, choices=(35, 36), default=36)
    arguments = parser.parse_args()
    assert os.geteuid() == 0, 'SDK diagnostic requires the pool owner'
    pid = subprocess.check_output(['systemctl', 'show', 'subyard-android-pool.service', '--property=MainPID', '--value'], text=True).strip()
    assert pid.isdigit() and int(pid) > 1
    status = dict(line.split(':', 1) for line in Path('/proc', pid, 'status').read_text().splitlines() if ':' in line)
    assert int(status['Uid'].split()[1]) == os.geteuid() and int(status['Gid'].split()[1]) == os.getegid()
    config = json.loads(Path('/etc/subyard-android.json').read_text())
    spec = importlib.util.spec_from_file_location('installed_android_pool', '/usr/local/lib/subyard-android/pool.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    signal.signal(signal.SIGTERM, interrupted)
    with tempfile.TemporaryDirectory(prefix='subyard-android-sdk-diagnostic.', dir='/var/tmp') as name:
        directory = Path(name)
        directory.chmod(0o700)
        marker = directory/'.marker'
        marker.write_bytes(b'subyard-android-sdk-diagnostic-v1\n')
        marker.chmod(0o600)
        with open(config.get('sdk_lock', '/run/lock/subyard-android/sdk.lock'), 'rb') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            print(json.dumps(diagnose(module, config, directory, arguments.api), sort_keys=True), flush=True)


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print(json.dumps(dict(schema='subyard.android.sdk-diagnostic/v1', retry='unavailable',
                              original_failure_preserved=True)), flush=True)
        raise SystemExit(1)
