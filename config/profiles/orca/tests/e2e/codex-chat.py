#!/usr/bin/env python3
"""Exercise stock Orca native Codex chat with a synthetic loopback model."""

import hashlib
import http.server
import importlib.util
import json
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import threading
import time
import uuid

sys.path.insert(0, '/usr/local/libexec/subyard/orca-registration')
from transport import RuntimeRPC


def envelope(method, session, fields, fence=None):
    payload = {'method': method, 'sessionId': session, 'fields': fields}
    digest = hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
    # Stock Orca's durable ledger requires a timestamp and 32 hex entropy digits.
    operation_id = str(time.time_ns() // 1_000_000) + '-' + uuid.uuid4().hex
    return {'sessionId': session, 'clientOperationId': operation_id,
            'expectedRuntimeFence': fence, 'payloadFingerprint': digest}


def accepted(result, operation):
    if result.get('ok') is not True:
        refusal = result.get('refusal', {})
        code = refusal.get('code')
        reason = refusal.get('details', {}).get('reason')
        # Only closed protocol enums are safe to report; messages may contain runtime data.
        codes = {'agent_session_operation_invalid', 'agent_session_operation_conflict',
                 'agent_session_operation_expired', 'agent_session_conflict',
                 'agent_session_ownership_unknown', 'structured_agent_session_unsupported'}
        reasons = {'operationIdInvalid', 'fingerprintMismatch', 'operationIdReused',
                   'operationExpired', 'requestMalformed', 'cliMissing', 'notSignedIn',
                   'providerStartFailed', 'providerRejected', 'attachFailed',
                   'clientCapabilityMissing', 'hostDisabled', 'hostUnsupported',
                   'managedAccountUnsupported', 'managedAccountEnvOverride',
                   'launchFolderMissing', 'optionRejected', 'agentCommandNotRunnable',
                   'conversationHeldElsewhere', 'sessionNotAttached', 'noLiveOwner',
                   'ownerUnproven', 'previousExitUnverifiable', 'noProviderChild'}
        print('orca-chat-refusal: ' + json.dumps({
            'operation': operation, 'code': code if code in codes else 'unrecognized',
            'reason': reason if reason in reasons else 'unspecified'}, sort_keys=True), flush=True)
        raise AssertionError(operation + ' was refused')
    return result


def approval_checks(rpc, provider, worktree, sessions):
    """Drive the stock native-chat journal and its revision-fenced approval RPC."""
    with tempfile.TemporaryDirectory(prefix='subyard-orca-chat-git-', dir='/tmp') as temporary:
        repo = Path(temporary)
        def git(*arguments):
            result = subprocess.run(['git', '-C', str(repo), *arguments], capture_output=True,
                                    text=True, timeout=10)
            return result.stdout.strip() if result.returncode == 0 else None
        git('init', '-q')
        git('config', 'user.name', 'Fixture')
        git('config', 'user.email', 'fixture@example.invalid')
        session = str(uuid.uuid4())
        fields = {'worktree': worktree, 'agent': 'codex'}
        accepted(rpc.call('agentSession.create', {
            **fields, 'envelope': envelope('agentSession.create', session, fields)}), 'create-approvals')
        sessions.append(session)

        def history():
            return accepted(rpc.call('agentSession.history', {
                'sessionId': session, 'direction': 'tail'}), 'approval-history')['page']

        for index, (command, choice) in enumerate([
                ('git commit --allow-empty -m denied', 'cancel'),
                ('git commit --allow-empty -m first', 'accept'),
                ('git commit --allow-empty -m repeated', 'accept'),
                ('git commit --amend --allow-empty -m amended', 'accept'),
                ('git commit --amend --allow-empty -m amended-again', 'accept'),
                ('git push', 'cancel'), ('git push', 'cancel')]):
            before = git('rev-parse', '--verify', 'HEAD')
            provider.workspace, provider.command = repo, command
            body = {'kind': 'message', 'role': 'user', 'blocks': [
                {'type': 'text', 'text': 'Run the fixture command.'}]}
            page = history()
            accepted(rpc.call('agentSession.send', {
                'envelope': envelope('agentSession.send', session, {'body': body}, page['fence']),
                'body': body}), 'send-approval')
            deadline = time.monotonic() + 30
            prompt = None
            while time.monotonic() < deadline:
                page = history()
                prompts = [item for item in page['items'] if item['body'].get('kind') == 'approval'
                           and item['body']['resolution']['state'] == 'pending']
                if prompts:
                    assert len(prompts) == 1, 'duplicate native chat approval'
                    prompt = prompts[0]
                    break
                time.sleep(0.1)
            if prompt is None:
                latest = (page.get('latestTurn') or {}).get('turn', {})
                state = latest.get('state')
                print('orca-chat-approval-timeout: ' + json.dumps({
                    'operation': index + 1, 'model_requests': getattr(provider, 'request_count', 0),
                    'command_pending': provider.command is not None,
                    'turn_state': state if state in ('running', 'completed', 'interrupted',
                                                     'failed', 'unverifiable') else 'unknown',
                    'approval_items': sum(item['body'].get('kind') == 'approval'
                                          for item in page['items']),
                    'history_changed': git('rev-parse', '--verify', 'HEAD') != before,
                }, sort_keys=True), flush=True)
                raise AssertionError('native chat did not request a fresh Git approval')
            approval_turn = (page.get('latestTurn') or {}).get('itemId')
            assert approval_turn, 'native approval has no current turn'
            options = [option['id'] for option in prompt['body']['options']]
            assert options and set(options) <= {'accept', 'decline', 'cancel'}, 'persistent chat choice exposed'
            assert choice in options, 'required single-use chat choice missing'
            assert git('rev-parse', '--verify', 'HEAD') == before, 'chat executed Git before approval'
            # Every RPC opens a new connection. A fresh client must see the same pending card.
            reconnected = RuntimeRPC('/srv/agents/orca/config/orca/orca-runtime.json', timeout=30)
            reread = accepted(reconnected.call('agentSession.history', {
                'sessionId': session, 'direction': 'tail'}), 'reconnect-history')['page']
            assert any(item['itemId'] == prompt['itemId'] and
                       item['body'].get('resolution', {}).get('state') == 'pending'
                       for item in reread['items']), 'pending approval lost on reconnect'
            fields = {'itemId': prompt['itemId'], 'expectedRevision': prompt['revision'], 'optionId': choice}
            accepted(reconnected.call('agentSession.respondToApproval', {
                # Orca promptPlan hashes its canonical approval mutation, not the RPC alias.
                **fields, 'envelope': envelope('agentSession.respondTo:approval', session, fields,
                                              reread['fence'])}), 'respond-approval')
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                page = history()
                latest = page.get('latestTurn') or {}
                if (latest.get('itemId') == approval_turn and
                        latest.get('turn', {}).get('state') in ('completed', 'interrupted')):
                    break
                time.sleep(0.1)
            else:
                raise AssertionError('native chat approval turn did not finish')
            after = git('rev-parse', '--verify', 'HEAD')
            assert (after != before) == (choice == 'accept'), 'native chat approval effect mismatch'
            assert provider.failure is None, 'synthetic chat provider failed'
            print('ok: native chat single-use approval/reconnect ' + str(index + 1), flush=True)
        rpc.call('agentSession.close', {'sessionId': session})
        sessions.remove(session)


def main():
    assert len(sys.argv) == 3, 'usage: codex-chat.py <provider-helper> <workspace>'
    spec = importlib.util.spec_from_file_location('provider', sys.argv[1])
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    rpc = RuntimeRPC('/srv/agents/orca/config/orca/orca-runtime.json', timeout=30)
    workspace = Path(sys.argv[2]).resolve()
    repos = rpc.call('repo.list')['repos']
    repo = next(r for r in repos if r['path'] == str(workspace))
    worktree = 'id:' + repo['id'] + '::' + str(workspace)
    support = rpc.call('agentSession.createSupport', {'worktree': worktree, 'agent': 'codex'})
    assert support.get('supported') is True, 'stock Orca native Codex chat is unavailable'
    settings = rpc.call('settings.get')['settings']
    arguments = settings.get('agentDefaultArgs', {})
    config = Path('/home/dev/.codex/config.toml')
    original = config.read_bytes()
    provider = http.server.ThreadingHTTPServer(('127.0.0.1', 0), module.Provider)
    provider.command, provider.failure = None, None
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    sessions = []
    try:
        config.write_bytes(original + (
            '\n[model_providers.fixture]\nname = "Fixture"\n'
            f'base_url = "http://127.0.0.1:{provider.server_port}/v1"\n'
            'wire_api = "responses"\nrequires_openai_auth = false\n'
            'request_max_retries = 0\nstream_max_retries = 0\n').encode())
        rpc.call('settings.update', {'agentDefaultArgs': {
            **arguments, 'codex': ' '.join(shlex.quote(a) for a in module.FIXTURE_CONFIG)}})
        # Native workspace-write may allow /tmp independently of the workspace.
        # This fixture-owned home directory is outside those temporary roots.
        with tempfile.TemporaryDirectory(prefix='subyard-orca-chat-', dir='/home/dev') as temporary:
            root = Path(temporary)
            session = str(uuid.uuid4())
            native_thread = None
            context_count = 0
            for number in (1, 2):
                if number == 1:
                    fields = {'worktree': worktree, 'agent': 'codex'}
                    created = accepted(rpc.call('agentSession.create', {
                        **fields, 'envelope': envelope('agentSession.create', session, fields)}), 'create')
                    fence = created['fence']
                else:
                    # Close retains ownership of the saved thread in this Orca session.
                    # Reopen that chat as the native history UI does; send reacquires its child.
                    revealed = accepted(rpc.call('agentSession.reveal', {'sessionId': session}), 'reveal')
                    assert revealed.get('readable') is True, 'saved native chat is unreadable'
                    history = accepted(rpc.call('agentSession.history', {
                        'sessionId': session, 'direction': 'tail'}), 'history')
                    assert history.get('providerSession', {}).get('id') == native_thread, 'saved native chat thread drift'
                    fence = history['page']['fence']
                sessions.append(session)
                marker = root / ('outside-workspace-' + str(number))
                provider.workspace = workspace
                provider.command = 'printf chat > ' + shlex.quote(str(marker))
                body = {'kind': 'message', 'role': 'user', 'blocks': [
                    {'type': 'text', 'text': 'Run the fixture command.'}]}
                accepted(rpc.call('agentSession.send', {
                    'envelope': envelope('agentSession.send', session, {'body': body}, fence),
                    'body': body}), 'send')
                deadline = time.monotonic() + 30
                while time.monotonic() < deadline and not marker.exists():
                    time.sleep(0.1)
                assert marker.exists(), 'native chat did not inherit yard filesystem permissions'
                assert provider.failure is None, 'synthetic provider failed'
                print('ok: native chat wrote outside workspace ' + str(number), flush=True)
                rpc.call('agentSession.close', {'sessionId': session})
                sessions.remove(session)
                # The closed thread's native rollout proves the next create really resumes it.
                # Stock Orca pins the real ~/.codex for the default host account.
                native = config.parent / 'sessions'
                matches = []
                contexts = []
                for rollout in native.rglob('*.jsonl'):
                    with rollout.open() as source:
                        first = json.loads(source.readline())
                        records = [json.loads(line) for line in source]
                    metadata = first.get('payload', {})
                    if metadata.get('cwd') == str(workspace):
                        matches.append(metadata['id'])
                        contexts.extend(record['payload'] for record in records
                                        if record.get('type') == 'turn_context')
                print('orca-chat-rollouts: ' + json.dumps({
                    'system_root_exists': native.is_dir(), 'matching_rollouts': len(matches),
                    'turn_contexts': len(contexts)}, sort_keys=True), flush=True)
                assert len(matches) == 1, 'fixture did not produce one native Codex rollout'
                assert contexts, 'native chat did not save its effective turn permissions'
                if native_thread:
                    assert matches[0] == native_thread, 'native chat resumed a different Codex thread'
                    assert len(contexts) > context_count, 'native chat did not save a resumed turn'
                context = contexts[-1]
                assert context.get('approval_policy') == 'on-request', 'native chat approval policy drift'
                assert context.get('approvals_reviewer') == 'user', 'native chat reviewer drift'
                assert context.get('sandbox_policy', {}).get('type') == 'danger-full-access', 'native chat sandbox drift'
                native_thread, context_count = matches[0], len(contexts)
                print('ok: stock Orca Codex chat ' + ('start' if number == 1 else 'resume'), flush=True)
        approval_checks(rpc, provider, worktree, sessions)
    finally:
        for session in sessions:
            rpc.call('agentSession.close', {'sessionId': session})
        config.write_bytes(original)
        rpc.call('settings.update', {'agentDefaultArgs': arguments})
        provider.shutdown()


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # All fixture data is synthetic, but upstream exceptions can include runtime details.
        print('orca-codex-chat: ' + (str(error) if isinstance(error, AssertionError)
                                    else type(error).__name__), file=sys.stderr)
        raise SystemExit(1)
