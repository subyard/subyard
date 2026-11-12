"""Exercise the real fixture's startup choices with reordered Claude screens."""
from pathlib import Path
import importlib.util
import subprocess
from types import SimpleNamespace
from unittest.mock import patch
import tempfile
import unittest


FIXTURE = Path(__file__).resolve().parents[1] / 'e2e/managed-agents.sh'


class ManagedClaudeTerminalTest(unittest.TestCase):
    def startup(self, offer, ordering, disclaimer=False, linger=False):
        source = FIXTURE.read_text()
        names = ('terminal_shows', 'terminal_wait', 'terminal_press', 'claude_terminal_ready')
        functions = '\n'.join(name + '() {' + source.split(name + '() {', 1)[1].split('\n}', 1)[0] + '\n}'
                              for name in names)
        # State is in files because terminal_screen runs in a command substitution.
        script = r'''
set -euo pipefail
cd "$1"
choice=$2 ordering=$3 disclaimer=$4 linger=$5
SECONDS=1
printf '%s\n' "$([ "$disclaimer" = True ] && echo disclaimer || echo initial)" > stage
printf '0\n' > reads
printf 'yes\n' > selected
sleep() { SECONDS=$((SECONDS + $1)); }
terminal_fail() { printf '%s\n' "$1" >&2; exit 1; }
orca_terminal_open() { :; }
terminal_screen() {
  local stage count
  stage=$(cat stage)
  count=$(cat reads)
  printf '%s\n' "$((count + 1))" > reads
  case "$stage" in
    disclaimer) printf 'Yes, I accept\n' ;;
    initial)
      if [ "$ordering" = missing ] || { [ "$ordering" = after ] && [ "$count" -lt 4 ]; }; then
        printf 'bypass permissions on\n'
      else
        printf 'Make auto mode your default permission mode?\nYes, set auto mode as my default permission mode\nNo, keep bypass permissions\n'
      fi ;;
    answered)
      if [ "$linger" = True ] && [ ! -e stale ]; then
        touch stale
        printf 'Make auto mode your default permission mode?\n'
      else
        cat mode
      fi ;;
  esac
}
terminal_send() {
  [ "$1" = --text ] || exit 90
  case "$2" in
    $'\x1b[B') printf 'no\n' > selected ;;
    $'\x1b[A') printf 'yes\n' > selected ;;
    $'\r')
      stage=$(cat stage)
      if [ "$stage" = disclaimer ]; then
        [ "$(cat selected)" = no ] || exit 91
        printf 'initial\n' > stage
        printf 'yes\n' > selected
      else
        [ "$stage" = initial ] || exit 92
        selected=$(cat selected)
        printf '%s\n' "$selected" >> answers
        if [ "$selected" = yes ]; then printf 'auto mode on\n'; else printf 'bypass permissions on\n'; fi > mode
        printf 'answered\n' > stage
      fi ;;
    *) exit 93 ;;
  esac
}
'''
        script += functions + '\nclaude_terminal_ready fixture "$choice"\n'
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            result = subprocess.run(['bash', '-c', script, 'startup', temporary, offer, ordering,
                                     str(disclaimer), str(linger)], stdin=subprocess.DEVNULL,
                                    capture_output=True, text=True, timeout=10)
            answers = Path(temporary, 'answers')
            return result, answers.read_text().splitlines() if answers.exists() else []

    def test_offer_before_or_after_bypass_status_uses_explicit_choice(self):
        for choice in ('accept', 'decline'):
            for ordering in ('before', 'after'):
                for disclaimer in (False, True):
                    with self.subTest(choice=choice, ordering=ordering, disclaimer=disclaimer):
                        result, answers = self.startup(choice, ordering, disclaimer)
                        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                        self.assertEqual(answers, ['yes' if choice == 'accept' else 'no'])

    def test_lingering_offer_is_not_answered_twice(self):
        for choice in ('accept', 'decline'):
            with self.subTest(choice=choice):
                result, answers = self.startup(choice, 'before', linger=True)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(answers, ['yes' if choice == 'accept' else 'no'])

    def test_absent_offer_never_synthesizes_acceptance(self):
        result, answers = self.startup('accept', 'missing')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(answers, [])

    def test_no_offer_keeps_bypass_without_enter(self):
        result, answers = self.startup('decline', 'missing')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(answers, [])


class ManagedClaudeLaunchTest(unittest.TestCase):
    def test_isolated_launch_declines_offer_before_submitting_prompt(self):
        spec = importlib.util.spec_from_file_location('managed_agents', FIXTURE.with_suffix('.py'))
        helper = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(helper)
        for offer in (False, True):
            with self.subTest(offer=offer), tempfile.TemporaryDirectory(dir='/tmp') as temporary:
                clock, writes, output = [0], [], [b'Make auto mode your default permission mode?' if offer else b'Ready']

                def now():
                    clock[0] += 0.5
                    return clock[0]

                arguments = SimpleNamespace(cli='/fixture/claude', xdg=temporary, seconds=25,
                                            expect_link=False, pid_file=None)
                with patch.object(helper, 'model', return_value=SimpleNamespace(server_port=1234, requests=1)), \
                     patch.object(helper.tempfile, 'mkdtemp', return_value=temporary), \
                     patch.object(helper.pty, 'fork', return_value=(12345, 42)), \
                     patch.object(helper.fcntl, 'ioctl'), \
                     patch.object(helper, 'resolved_exe', return_value=arguments.cli), \
                     patch.object(helper, 'SHADOW', Path(temporary) / 'absent'), \
                     patch.object(helper.time, 'monotonic', side_effect=now), \
                     patch.object(helper.time, 'sleep'), \
                     patch.object(helper.select, 'select', side_effect=lambda *args: ([42] if output else [], [], [])), \
                     patch.object(helper.os, 'read', side_effect=lambda *args: output.pop(0)), \
                     patch.object(helper.os, 'write', side_effect=lambda fd, data: writes.append(data)), \
                     patch.object(helper.os, 'kill'), patch.object(helper.os, 'waitpid'):
                    self.assertEqual(helper.launch(arguments), 0)
                self.assertEqual(writes, ([b'\x1b[B', b'\r'] if offer else []) + [b'hello', b'\r'])


class ManagedCodexDisconnectTest(unittest.TestCase):
    def test_request_start_not_peer_lookup_completion_defines_disconnect(self):
        spec = importlib.util.spec_from_file_location('managed_agents', FIXTURE.with_suffix('.py'))
        helper = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(helper)
        requests = helper.RoutingRequests()
        killed_at, exited_at = 2, 3

        def delayed_peer(*_):
            # Lookup is still pending after the kill, but this request began before it.
            self.assertEqual(requests.peers_since(killed_at), set())
            self.assertEqual(requests.evidence(killed_at, exited_at)['requests'][0]['peer'], 'pending')
            return '/fixture/codex'

        with patch.object(helper.time, 'monotonic', return_value=1), \
                patch.object(helper, 'peer_executable', side_effect=delayed_peer):
            requests.record(1, 2, '/v1/responses?token=private')
        self.assertEqual(requests.peers_since(killed_at), set())
        for instant in (killed_at, exited_at + 1):
            with patch.object(helper.time, 'monotonic', return_value=instant), \
                    patch.object(helper, 'peer_executable', return_value='unknown'):
                requests.record(1, 2, '/v1/responses')
            self.assertEqual(requests.peers_since(killed_at), {'unknown'})
        evidence = requests.evidence(killed_at, exited_at)
        self.assertEqual([entry['after_kill'] for entry in evidence['requests']], [False, True, True])
        self.assertEqual([entry['after_exit'] for entry in evidence['requests']], [False, False, True])
        self.assertEqual(evidence['requests'][0]['path'], '/v1/responses')
        self.assertNotIn('private', str(evidence))
        for _ in range(40):
            with patch.object(helper, 'peer_executable', return_value='/fixture/codex'):
                requests.record(1, 2, '/v1/responses')
        self.assertEqual(requests.evidence(killed_at, exited_at)['total'], 43)
        self.assertEqual(len(requests.evidence(killed_at, exited_at)['requests']), 40)
        # Truncating diagnostics must never discard a leak from the assertion's history.
        self.assertIn('unknown', requests.peers_since(killed_at))


if __name__ == '__main__':
    unittest.main()
