#!/usr/bin/env python3
"""Inspect real producer transport without guest namespaces or installation."""
import os
import sys
import tarfile

arguments = sys.argv[1:]
assert arguments[0] == 'exec', arguments
environment = {}
for index, argument in enumerate(arguments[:arguments.index('--')]):
    if argument == '--env':
        key, value = arguments[index+1].split('=', 1)
        environment[key] = value
assert environment.get('GITHUB_GIT_MODE') == os.environ['EXPECTED_MODE'], environment
assert 'UNDECLARED_SECRET' not in environment, environment
assert arguments[-1] == os.environ['EXPECTED_ACTION'], arguments
# The producer keeps stdin open as a lifetime signal; tar EOF ends this read.
with tarfile.open(fileobj=sys.stdin.buffer, mode='r|') as archive:
    names = {member.name for member in archive}
assert {'provision.sh', 'git_wiring.py', 'agent-skills.py'} <= names, names
