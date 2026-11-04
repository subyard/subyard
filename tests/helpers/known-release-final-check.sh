#!/usr/bin/env bash
# The immutable 0.18.6 caller cannot perform the candidate activation follow-up.
# Definition only; callers own fresh inspection, consent and final verification.
known_materialized_final_check() { # inspection, protected journal, current, source
  python3 - "$@" <<'PY'
import json, sys
try:
    report, journal = (json.load(open(path)) for path in sys.argv[1:3])
    expected, source = sys.argv[3:5]
    outcome, summary = report['outcome'], report['journal']
    blockers, decisions = report.get('blockers', []), report['decisions']
    rows = {row['resource']: row for row in decisions}
    pending = rows['activation.materialized-config']
    domains = report['domains']
    valid = all([
        report['current'] == report['owner'] == expected,
        outcome['status'] == 'migration-required' and outcome['code'] == 'transition-required',
        outcome['reachedGoal'] is False,
        outcome['active'] == outcome['target'] == expected and outcome['previous'] == source,
        outcome.get('transaction') is None,
        isinstance(blockers, list) and not blockers,
        journal['checkpoint'] == 'complete',
        journal['goal'] == {'target': expected, 'direction': 'activate-target'},
        journal['releases']['target'] == expected and journal['releases']['from'] == source,
        journal.get('sourceIngress') is None,
        isinstance(journal['steps'], list) and journal['steps'],
        all(isinstance(row, dict) and row.get('checkpoint') == 'verified' for row in journal['steps']),
        summary['checkpoint'] == 'complete' and summary['target'] == expected,
        summary['transaction'] == journal['transaction'],
        isinstance(decisions, list) and len(decisions) == 2,
        all(row['epoch'] == row['requiredEpoch'] and row['pending'] == [] for row in report['domains']),
        set(rows) == {'activation.materialized-config', 'transition.source-metadata'},
        pending['scope'] == 'activation' and pending['decision'] == 'canonicalize' and pending['result'] == 'converged',
        rows['transition.source-metadata']['decision'] == 'preserve',
        isinstance(domains, list) and bool(domains),
    ])
except (KeyError, TypeError, ValueError, OSError):
    valid = False
sys.exit(0 if valid else 1)
PY
}
