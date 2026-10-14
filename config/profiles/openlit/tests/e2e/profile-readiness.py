#!/usr/bin/env python3
"""Wait for public OpenLIT readiness and project safe release diagnostics."""
import argparse
from collections import Counter
import json
import math
import os
import re
import signal
import subprocess
import time

STATUSES = {'ready', 'migration-required', 'recovering', 'operator-action-required'}
CODES = {'ready', 'transition-required', 'recovery-pending', 'registry-invalid',
         'unsupported-epoch', 'unsupported-kind', 'confirmation-required', 'operation-declined',
         'plan-stale', 'migration-stale', 'resource-conflict', 'precondition-blocked',
         'dependency-unavailable', 'verification-failed', 'activation-ambiguous',
         'recovery-ambiguous', 'journal-invalid', 'rollback-incompatible', 'rollback-expired'}


class ProbeError(Exception):
    def __init__(self, category, exit_code=1):
        self.category, self.exit_code = category, exit_code


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError('duplicate field')
        value[key] = item
    return value


def probe(argv, deadline):
    remaining = deadline-time.monotonic()
    if remaining <= 0:
        raise ProbeError('deadline', 124)
    try:
        process = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                   start_new_session=True)
    except OSError:
        raise ProbeError('command-unavailable') from None
    try:
        output, _ = process.communicate(timeout=max(0, deadline-time.monotonic()))
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.communicate()
        raise ProbeError('deadline', 124) from None
    if process.returncode:
        raise ProbeError('command-failed', process.returncode if 0 < process.returncode <= 255 else 1)
    if time.monotonic() >= deadline:
        raise ProbeError('deadline', 124)
    try:
        if len(output) > 65536:
            raise ValueError('oversized projection')
        value = json.loads(output, object_pairs_hook=unique_object)
        if not isinstance(value, dict):
            raise ValueError('object required')
        return value
    except (ValueError, UnicodeError, RecursionError):
        raise ProbeError('invalid-projection') from None


def emit(prefix, value):
    print(prefix+'='+json.dumps(value, sort_keys=True), flush=True)


def wait(engine, yard, timeout):
    deadline = time.monotonic()+timeout
    while True:
        value = probe([engine, '-Y', yard, 'profile', 'status', 'openlit', '--json'], deadline)
        if (set(value) != {'yard', 'id', 'enabled', 'configured', 'runtime', 'ready'}
                or value['yard'] != yard or value['id'] != 'openlit'
                or any(type(value[key]) is not bool for key in ('enabled', 'configured', 'ready'))
                or value['runtime'] not in ('pending', 'ready', 'removed', 'unavailable')):
            raise ProbeError('invalid-projection')
        if (not value['enabled'] or not value['configured']
                or (value['runtime'], value['ready']) not in (('pending', False), ('ready', True))):
            raise ProbeError('unexpected-profile-state')
        emit('openlit_e2e_profile_readiness', {key: value[key] for key in
                                            ('enabled', 'configured', 'runtime', 'ready')})
        if value['ready']:
            return 0
        remaining = deadline-time.monotonic()
        if remaining <= 0:
            raise ProbeError('deadline', 124)
        time.sleep(min(1, remaining))


def diagnose(engine, timeout):
    # Migrate inspection is host-wide and rejects an explicit yard selector.
    value = probe([engine, 'migrate', '--check', '--json'], time.monotonic()+timeout)
    outcome = value.get('outcome')
    blockers, decisions = value.get('blockers', []), value.get('decisions', [])
    if (type(value.get('schemaVersion')) is not int or value['schemaVersion'] != 1
            or not isinstance(outcome, dict) or not isinstance(outcome.get('status'), str)
            or outcome['status'] not in STATUSES or not isinstance(outcome.get('code'), str)
            or outcome['code'] not in CODES or type(outcome.get('reachedGoal')) is not bool
            or not isinstance(blockers, list) or len(blockers) > 32
            or not isinstance(decisions, list) or len(decisions) > 64
            or any(not isinstance(row, dict) or not isinstance(row.get('code'), str)
                   or row['code'] not in CODES for row in blockers)
            or any(not isinstance(row, dict) for row in decisions)):
        raise ProbeError('invalid-projection')
    return {'schemaVersion': 1, 'status': outcome['status'], 'code': outcome['code'],
            'reachedGoal': outcome['reachedGoal'],
            'blocker_codes': dict(Counter(row['code'] for row in blockers)),
            'activation_decision_count': sum(row.get('scope') == 'activation' for row in decisions)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('wait', 'diagnose'))
    parser.add_argument('--engine', required=True)
    parser.add_argument('--yard')
    parser.add_argument('--timeout', type=float, default=90)
    parser.add_argument('--failed-exit', type=int)
    args = parser.parse_args()
    if (not math.isfinite(args.timeout) or not 0 < args.timeout <= 90
            or args.mode == 'wait' and (not args.yard or not re.fullmatch('[a-zA-Z0-9_-]{1,64}', args.yard)
                                       or args.failed_exit is not None)
            or args.mode == 'diagnose' and (args.yard is not None or args.failed_exit is None
                                           or not 1 <= args.failed_exit <= 255)):
        parser.error('invalid readiness arguments')
    try:
        if args.mode == 'wait':
            return wait(args.engine, args.yard, args.timeout)
        projection = diagnose(args.engine, args.timeout)
    except ProbeError as error:
        projection = {'category': error.category, 'exit_code': error.exit_code}
        if args.mode == 'wait':
            emit('openlit_e2e_profile_readiness', projection)
            return error.exit_code
    projection['failed_exit'] = args.failed_exit
    emit('openlit_e2e_release_diagnostic', projection)
    return args.failed_exit


if __name__ == '__main__':
    raise SystemExit(main())
