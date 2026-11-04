#!/usr/bin/env python3
"""The released-caller fixture admits only the captured item-20 final check."""
import copy
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

SOURCE = Path(__file__).with_name('test-yard-source-repair.sh')
function = re.search(r'^source_repair_finish_known_final_check\(\) \{\n.*?^}\n', SOURCE.read_text(), re.M | re.S).group()
expected = '0.25.6-p0.3-aaaaaaaaaaaa'
source = '0.18.6-bbbbbbbbbbbb'
journal = dict(checkpoint='complete', transaction='tx-fixture',
               goal=dict(target=expected, direction='activate-target'),
               releases=dict(target=expected, **{'from': source}),
               steps=[dict(checkpoint='verified')])
report = dict(current=expected, owner=expected,
              outcome=dict(status='migration-required', code='transition-required', reachedGoal=False,
                           active=expected, target=expected, previous=source),
              journal=dict(checkpoint='complete', target=expected, transaction='tx-fixture'),
              domains=[dict(epoch=3, requiredEpoch=3, pending=[])],
              decisions=[dict(resource='activation.materialized-config', scope='activation',
                              decision='canonicalize', result='converged'),
                         dict(resource='transition.source-metadata', decision='preserve')])
ready = dict(outcome=dict(status='ready', code='ready', reachedGoal=True, active=expected, target=expected),
             decisions=[dict(resource='transition.source-metadata', decision='preserve')])
script = r'''set -euo pipefail
STATE_ROOT=$1 OPERATOR_HOME=$1 CANDIDATE_VERSION=0.25.6-p0.3
. "$2"
OLD_RELEASE_TARGET=releases/0.18.6-bbbbbbbbbbbb RELEASE_ROOT=/fixture
V2_JOURNAL=$1/journal.json
operator_yard() {
  case "$*" in
    --version) printf 'yard %s\n' "${MOCK_VERSION:-$CANDIDATE_VERSION}" ;;
    'migrate --check --json')
      if [ -e "$STATE_ROOT/applied" ]; then cat "$STATE_ROOT/ready.json"; else cat "$STATE_ROOT/report.json"; fi ;;
    *) return 99 ;;
  esac
}
operator_env() {
  case "$1" in
    readlink) printf '%s\n' "${MOCK_LINK:-releases/0.25.6-p0.3-aaaaaaaaaaaa}" ;;
    cat) command cat "$2" ;;
    env)
      [ "$*" = 'env YARD_RELEASE_BASE_URL=file:///fixture '\
"$OPERATOR_HOME/.local/bin/yard update --version $CANDIDATE_VERSION --yes" ] || return 98
      printf 'repair\n' >> "$STATE_ROOT/applied"
      return "${MOCK_REPAIR_EXIT:-0}" ;;
    *) return 99 ;;
  esac
}
'''
message = 'yard: update: final update check: the inspected release transition has not started; next: run yard update\n'
cases = [('', None, None)]
for field, value in [('status', 'recovering'), ('code', 'verification-failed'), ('active', 'foreign'),
                     ('target', 'foreign'), ('previous', 'foreign'), ('transaction', 'unfinished'), ('reachedGoal', True)]:
    cases.append(('outcome-'+field, lambda r, j, f=field, v=value: r['outcome'].__setitem__(f, v), None))
for name, mutation in [
    ('blockers', lambda r, j: r.update(blockers=[{'code': 'blocked'}])),
    ('null-blockers', lambda r, j: r.update(blockers=None)),
    ('owner', lambda r, j: r.update(owner='foreign')),
    ('incomplete', lambda r, j: j.update(checkpoint='reconciling')),
    ('goal', lambda r, j: j['goal'].update(direction='restore-source')),
    ('release', lambda r, j: j['releases'].update(target='foreign')),
    ('summary', lambda r, j: r['journal'].update(transaction='other')),
    ('step', lambda r, j: j['steps'][0].update(checkpoint='intent')),
    ('empty-steps', lambda r, j: j.update(steps=[])),
    ('epoch', lambda r, j: r['domains'][0].update(epoch=2)),
    ('empty-domains', lambda r, j: r.update(domains=[])),
    ('null-domains', lambda r, j: r.update(domains=None)),
    ('extra-decision', lambda r, j: r['decisions'].append(dict(resource='activation.other', decision='canonicalize'))),
    ('other-owner', lambda r, j: r['decisions'][0].update(resource='activation.other')),
    ('source-work', lambda r, j: r['decisions'][1].update(decision='canonicalize')),
    ('explicit-empty-blockers', lambda r, j: r.update(blockers=[])),
]:
    cases.append((name, mutation, None))
for name, environment in [('native-exit', {'MOCK_ORIGINAL': '2'}), ('old-message', {}),
                          ('version', {'MOCK_VERSION': 'other'}), ('link', {'MOCK_LINK': 'releases/foreign'}),
                          ('repair-fails', {'MOCK_REPAIR_EXIT': '23'}), ('not-ready', {})]:
    cases.append((name, None, environment))
for name, mutation, environment in cases:
    r, j, fresh = copy.deepcopy(report), copy.deepcopy(journal), copy.deepcopy(ready)
    if mutation:
        mutation(r, j)
    if name == 'not-ready':
        fresh = r
    with tempfile.TemporaryDirectory(prefix='source-final-check-', dir='/tmp') as temporary:
        root = Path(temporary)
        for path, value in [('report.json', r), ('journal.json', j), ('ready.json', fresh)]:
            (root/path).write_text(json.dumps(value))
        (root/'source-update.log').write_text('unrelated failure\n' if name == 'old-message' else message)
        result = subprocess.run(['bash', '-c', script+function+'\nsource_repair_finish_known_final_check "${MOCK_ORIGINAL:-1}"',
                                 'fixture', temporary, str(SOURCE.with_name('known-release-final-check.sh'))], env={**os.environ, **(environment or {})},
                                capture_output=True, text=True, timeout=10)
        accepted = name in ('', 'explicit-empty-blockers')
        assert result.returncode == (0 if accepted else 1), (name, result.returncode, result.stdout, result.stderr)
        effects = (root/'applied').read_text().splitlines() if (root/'applied').exists() else []
        assert effects == (['repair'] if accepted or name in ('repair-fails', 'not-ready') else []), (name, effects)
print('ok: released final-check classification, one candidate repair and fresh ready verification')
