#!/usr/bin/env python3
"""Compare rewritten product pipelines with their predecessor using stub producers."""

import argparse
from functools import cache
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
BASE = "cb385b7c931f6536e1339c712916f577c06967e1"
BASH = ["bash", "-o", "pipefail", "-uc"]
SETUP = """
producer() { printf '%s' "$PAYLOAD"; return "$PRODUCER_EXIT"; }
set -- fixture fixture
HERMES_DEVICE=fixture ORCA_DEVICE=fixture candidate=fixture note=fixture group=fixture
HERMES_OWNER_IP=192.0.2.1 ORCA_OWNER_IP=192.0.2.1
HERMES_DASHBOARD_HOST_PORT=8080 ORCA_HOST_PORT=8080
EMU_PROCESS_PATTERN='^fixture$'
"""

# Each marker ends at the producer. Keep intermediate filters intact and stub only
# the producer, so these cases execute the actual changed consumers from both trees.
GREP = [
    ("dev/build-engine.sh", '-newer "$OUTPUT" -print -quit | ', "\n", "fixture\n"),
    ("scripts/01-install-incus.sh", 'id -nG "$OPERATOR_USER" 2>/dev/null | ', "; then", "dev incus-admin\n"),
    ("scripts/02-create-project.sh", 'restricted.devices.disk.paths 2>/dev/null | ', "; then", "/fixture\n"),
    ("scripts/02-create-project.sh", 'device list default --project "$INCUS_PROJECT" 2>/dev/null | ', "\n", "fixture\n"),
    ("scripts/04-provision-subyard.sh", 'iptables -S FORWARD | ', " \\", "-P FORWARD DROP\n"),
    ("scripts/04-provision-subyard.sh", 'list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null | ', "; then", "kvm\n"),
    ("scripts/04-provision-subyard.sh", '"$err" | ', "; then", "nested container\n"),
    *[(f"scripts/{name}.sh", 'list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null | ', "; }", "fixture\n")
      for name in ("05-mount-host-paths", "07-ssh-access", "09-yard-extras")],
    ("scripts/e2e-lab/provision.sh", 'id -nG "$user" 2>/dev/null | ', " ||", "dev fixture\n"),
    ("scripts/install-runtime-release.sh", 'find "$candidate" -type l -print -quit | ', "; then", "link\n"),
    ("scripts/install-runtime-release.sh", 'find "$destination" -type l -print -quit | ', " \\", "link\n"),
    ("scripts/lib-vm-page-reporting.sh", '"$help" | ', " \\", "free-page-reporting\n"),
    ("scripts/lib/host.sh", 'id -nG "$(id -un)" 2>/dev/null | ', "; then", "dev incus-admin\n"),
    ("scripts/lib/host.sh", 'ip -d -o link show dev "$1" 2>/dev/null | ', "; }", "link veth state\n"),
    ("scripts/teardown-physical.sh", "approved_names profile | ", "; then", "default\n"),
    ("config/profiles/android/resources/emulator/process-identity.sh",
     '''printf '%s\\n' "$1" | ''', "\n", "fixture\n"),
    ("config/profiles/hermes/resources/dashboard/handler.sh",
     'device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null |', "\n", "fixture\n"),
    ("config/profiles/hermes/resources/dashboard/handler.sh",
     '"${tailscale_ips[@]}" | ', " \\", "fixture\n"),
    ("config/profiles/hermes/resources/dashboard/handler.sh",
     "ip -4 -brief address show scope global |", " \\", "dev UP fixture/24\n"),
    ("config/profiles/hermes/resources/dashboard/handler.sh",
     'ss -Hltn "sport = :$HERMES_DASHBOARD_HOST_PORT" |', "; then", "a b c 192.0.2.1:8080\n"),
    ("config/profiles/openclaw/resources/qa-bot-broker/handler.sh", '"$existing" | ', "; then", "fixture\n"),
    ("config/profiles/orca/resources/orca/handler.sh",
     'device list "$YARD_INSTANCE_NAME" "${PROJ[@]}" 2>/dev/null |', "\n", "fixture\n"),
    ("config/profiles/orca/resources/orca/handler.sh",
     '"${resolved_ips[@]}" | ', " &&", "fixture\n"),
    ("config/profiles/orca/resources/orca/handler.sh",
     "ip -4 -brief address show scope global |", "; then", "dev UP fixture/24\n"),
    ("config/profiles/orca/resources/orca/handler.sh",
     'ss -Hltn "sport = :$ORCA_HOST_PORT" |', "; then", "a b c 192.0.2.1:8080\n"),
]
FORMATTERS = [
    ("config/profiles/android/provision.sh", '"$JDK_HOME/bin/java" -version 2>&1 | ', ') sdk=', "java version\nsecond\n"),
    ("config/profiles/subyard-dev/provision.sh", "shellcheck --version | ", ')"', "version: 1\nversion: 2\n"),
    ("scripts/lib-power.sh", '"$rows" | ', "\n", "fixture,first\nfixture,second\n"),
    ("scripts/lib/host.sh", "ip -4 route show default | ", ')"', "default dev eth0 dev eth1\n"),
    ("scripts/lib/host.sh", 'address="$(printf \'%s\\n\' "$output" | ', ')"', "route eth0\na b c 192.0.2.1/24\n"),
    ("scripts/lib/host.sh", 'case "$(printf \'%s\\n\' "$output" | ', ')"', "route eth0\nroute eth1\n"),
    ("scripts/lib/host.sh", 'default route on $(printf \'%s\\n\' "$output" | ', ') without', "route eth0\nroute eth1\n"),
    ("dev/ci/periodic-e2e.sh", "sudo -n iptables -S FORWARD 2>&1 | ", " ||", "first\nsecond\n"),
    ("dev/ci/periodic-e2e.sh", "sudo -n iptables -S FORWARD | ", "\n", "first\nsecond\n"),
]
ORCA = "config/profiles/orca/resources/orca/handler.sh"
DEVICE = "fixture:\n  bind: host\n  connect: target\n  listen: owner\n  type: proxy\nother:\nfixture:\n  ignored: yes\n"


def compare(name, before, after, payload, directory, statuses, *, compare_output=False):
    cases = (
        ("success", payload, 0),
        ("failure", payload, 23),
        ("silent success", "", 0),
        ("silent failure", "", 23),
    )
    for index, (case, data, status) in enumerate(cases):
        results = [
            subprocess.run(BASH + [SETUP + script], cwd=directory, capture_output=True,
                           env=dict(os.environ, LC_ALL="C", PAYLOAD=data, PRODUCER_EXIT=str(status)), timeout=5)
            for script in (before, after)
        ]
        exits = tuple(result.returncode for result in results)
        expected = statuses[index]
        if isinstance(expected, int):
            expected = (expected, expected)
        assert exits == expected, (name, case, exits, expected, [result.stderr for result in results])
        if compare_output:
            assert results[0].stdout == results[1].stdout, (name, case)
    print(f"PASS {name} ({len(cases)} cases)")


def fragment(source, start, end):
    _, match, rest = source.partition(start)
    rest = rest.lstrip()
    assert match and end in rest, (start, end)
    return rest.partition(end)[0].strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default=BASE)
    args = parser.parse_args()

    @cache
    def sources(path):
        return (subprocess.check_output(["git", "show", f"{args.base}:{path}"], cwd=ROOT, text=True),
                (ROOT / path).read_text())

    with tempfile.TemporaryDirectory(prefix="subyard-pipe-status-", dir="/tmp") as directory:
        fixture = Path(directory)
        version = fixture / "version"
        version.write_text('#!/bin/bash\nprintf "%s" "$PAYLOAD"\nexit "$PRODUCER_EXIT"\n')
        version.chmod(0o755)
        for table, statuses, compare_output in (
                (GREP, (0, 23, 1, 1), False), (FORMATTERS, (0, 23, 0, 23), True)):
            for path, marker, end, output in table:
                stages = ["producer | " + fragment(source, marker, end) for source in sources(path)]
                compare(f"{path} [{marker.strip()}]", *stages, output, fixture, statuses,
                        compare_output=compare_output)

        for function, end in (("orca_device_fingerprint", ')"'),
                              ("orca_device_exact_shape", ')"'),
                              ("remove_receipt_matches", ' <<<"$show"')):
            stages = []
            for source in sources(ORCA):
                body = fragment(source, f"{function}() {{", "\n}")
                stage = "awk -v device="
                stages.append(stage + fragment(body, stage, end))
            compare(f"{ORCA} [{function}]", *["producer | " + stage for stage in stages],
                    DEVICE, fixture, (0, 23, 0, 23), compare_output=True)

        path = "scripts/install-key-tools.sh"
        functions = [fragment(source, "\ntool_version_matches() ", "\n}") for source in sources(path)]
        compare(path, *["tool_version_matches() " + function + '\n}\ntool_version_matches ./version 1.3.1'
                        for function in functions],
                "age v1.3.1\n", fixture, (0, 23, 0, 23))

        path = "config/agents/codex/provision.sh"
        listing = 'tar -tvzf "$archive"'
        setup = 'tmp=.; archive=fixture; die() { exit 1; }; tar() { case "$1" in -tvzf) producer;; esac; }\n'
        checks = [setup + listing + ' ' + fragment(source, listing, '\nfi\n') + '\nfi'
                  for source in sources(path)]
        compare(path, *checks, "lrwxrwxrwx member\n", fixture, (1, 1, 0, 1))

        path = "scripts/install-runtime-release.sh"
        before, after = sources(path)
        before = fragment(before, '\nif tar -tzf "$BUNDLE" | ', "; then")
        after = fragment(after, '\nunsafe_path="', '\nif [ "$unsafe_path" = yes ]; then')
        compare(f"{path} [archive listing]",
                "BUNDLE=fixture; tar() { producer; }; if tar -tzf \"$BUNDLE\" | " + before + "; then exit 1; fi",
                'BUNDLE=fixture; tar() { producer; }; unsafe_path="' + after + '\n[ "$unsafe_path" != yes ]',
                "../fixture\n", fixture, (1, (0, 1), 0, (0, 1)))

        path = ".github/workflows/periodic-e2e.yml"
        stage = 'tar -tf "$stage/bundle.tar"'
        before, after = sources(path)
        before = 'if ' + stage + ' ' + fragment(before, 'if ' + stage, "\n          fi") + '\nfi'
        after = stage + ' ' + fragment(after, stage, "\n          fi") + '\nfi'
        setup = 'set -e; stage=.; tar() { producer; }\n'
        compare(path, setup + before, setup + after, ".subyard-acceptance/yard\n", fixture,
                (1, (0, 23), 0, (0, 23)))

        path = "dev/e2e/integration-selection.sh"
        logs = 'cat "$STATE/drift-disable-init-first.log" "$STATE/drift-init-first.log"'
        assertions = [logs + ' ' + fragment(source, '\n' + logs, " \\\n  || die 'init-first")
                      for source in sources(path)]
        setup = '''STATE=.; YARD_NAME=fixture
printf '%s' "$PAYLOAD" >drift-disable-init-first.log
if [ "$PRODUCER_EXIT" = 0 ]; then : >drift-init-first.log; else rm -f drift-init-first.log; fi
'''
        compare(path, *[setup + assertion for assertion in assertions],
                "yard fixture: released ownership of /home/dev/.claude/settings.json: owned fields changed\n",
                fixture, (0, 1, 1, 1))


if __name__ == "__main__":
    main()
