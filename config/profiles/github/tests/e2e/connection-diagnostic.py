import grp, json, os, stat, subprocess, sys

def collect(arguments):
    try:
        process = subprocess.Popen(arguments, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        payload = process.stdout.read(65537)
        if len(payload) > 65536:
            process.kill()
        code = process.wait()
        return {'capture': 'oversize' if len(payload) > 65536 else 'native-failure' if code else 'ok',
                'native_exit': code}, payload
    except OSError:
        return {'capture': 'unavailable', 'native_exit': None}, b''

def manager_group():
    query = ['systemctl', 'show', 'user@'+str(os.getuid())+'.service', '--property=MainPID', '--value']
    result, payload = collect(query)
    pid = payload.strip()
    if result['capture'] != 'ok' or not pid.isdigit() or len(pid) > 10 or not 0 < int(pid) < 2**31:
        return 'unknown'
    try:
        with open('/proc/'+pid.decode('ascii')+'/status', encoding='ascii') as source:
            status = source.read(65537)
        if len(status) > 65536:
            return 'unknown'
        fields = dict(line.split(':', 1) for line in status.splitlines()
                      if line.startswith(('Name:', 'Uid:', 'Groups:')))
        if fields['Name'].strip() != 'systemd' or fields['Uid'].split() != [str(os.getuid())]*4:
            return 'unknown'
        groups = fields['Groups'].split()
        if any(not group.isascii() or not group.isdecimal() or int(group) >= 2**32 for group in groups):
            return 'unknown'
        wanted = grp.getgrnam('incus-admin').gr_gid
        after, current = collect(query)
        if after['capture'] != 'ok' or current.strip() != pid:
            return 'unknown'
        return wanted in map(int, groups)
    except (OSError, UnicodeError, ValueError, KeyError):
        return 'unknown'

def main():
    unit, payload = collect(['systemctl', '--user', 'show', sys.argv[1],
        '--property=Result,ExecMainCode,ExecMainStatus,ActiveState,SubState'])
    if unit['capture'] == 'ok':
        try:
            values = dict(line.split('=', 1) for line in payload.decode().splitlines())
            enums = {
                'Result': ('success', 'exit-code', 'signal', 'core-dump', 'timeout', 'watchdog',
                           'start-limit-hit', 'resources', 'protocol', 'exec-condition', 'oom-kill'),
                'ActiveState': ('active', 'inactive', 'failed', 'activating', 'deactivating', 'reloading', 'maintenance'),
                'SubState': ('dead', 'start', 'start-pre', 'start-post', 'running', 'exited', 'stop',
                             'stop-sigterm', 'stop-sigkill', 'stop-post', 'failed', 'auto-restart', 'cleaning'),
            }
            if set(values) != {*enums, 'ExecMainCode', 'ExecMainStatus'}:
                raise ValueError()
            if any(values[key] not in allowed for key, allowed in enums.items()):
                raise ValueError()
            for key in ('ExecMainCode', 'ExecMainStatus'):
                if not values[key].isascii() or not values[key].isdecimal() or not 0 <= int(values[key]) <= 255:
                    raise ValueError()
                values[key] = int(values[key])
            unit['fields'] = values
        except (ValueError, UnicodeError):
            unit['capture'] = 'malformed'

    peer = {'capture': 'malformed'}
    try:
        with os.fdopen(os.open(sys.argv[2], os.O_RDONLY | os.O_NOFOLLOW), 'rb') as source:
            info = os.fstat(source.fileno())
            if (not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600
                    or info.st_uid != os.getuid() or info.st_nlink != 1):
                raise ValueError()
            state = json.loads(source.read(65537))
        keys = ('lastAttempt', 'lastSuccess', 'nextRetry', 'consecutiveFailures')
        if not isinstance(state, dict) or any(type(state.get(key)) is not int or not 0 <= state[key] <= 2**53 for key in keys):
            raise ValueError()
        peer = {'capture': 'ok', 'attempted': state['lastAttempt'] > 0,
                'succeeded': state['lastSuccess'] > 0, 'retry_scheduled': state['nextRetry'] > 0,
                'failure_count': state['consecutiveFailures']}
    except (OSError, ValueError, TypeError):
        pass

    journal, payload = collect(['journalctl', '--user', '--unit', sys.argv[1], '--lines=20',
                               '--no-pager', '--output=json', '--output-fields=MESSAGE'])
    journal.update(categories={}, unknown_count=0)
    if journal['capture'] == 'ok':
        try:
            categories = (
                ('release-inspection-unavailable', ('release transition inspection failed',)),
                ('journal-store-unavailable', ('the journal-selected release store cannot be inspected safely',)),
                ('journal-assets-unverified', ('the journal-selected release is unavailable or unverified',)),
                ('journal-identity-inconsistent', ('the journal-selected release identity is inconsistent',)),
                ('transition-inspection-unavailable', ('the journal-selected release cannot provide a valid transition inspection',)),
                ('source-verification-unavailable', ('verify source support:',)),
                ('materialized-config-unavailable', ('cannot inspect materialized config for release activation',)),
                ('release-dependency-unavailable', ('dependency-unavailable',)),
                ('release-migration-required', ('migration-required',)),
                ('release-transition-required', ('transition-required',)),
                ('release-admission', ('release transition',)),
                ('incus-permission', ('incus: permission denied', 'incus socket: permission denied')),
                ('no-new-privileges', ('no new privileges', 'nonewprivileges')),
                ('ssh-transport', ('ssh:', 'permission denied (publickey)', 'host key verification failed', 'connection refused', 'connection timed out')),
                ('tool-exec', ('no such file or directory', 'failed at step exec', 'failed to execute', 'command not found')),
            )
            for line in payload.decode().splitlines():
                message = json.loads(line).get('MESSAGE')
                if not isinstance(message, str):
                    raise ValueError()
                category = next((name for name, terms in categories if any(term in message.lower() for term in terms)), None)
                if category:
                    journal['categories'][category] = journal['categories'].get(category, 0) + 1
                else:
                    journal['unknown_count'] += 1
        except (ValueError, UnicodeError, AttributeError):
            journal['capture'] = 'malformed'
    print('github_connection_auto_sync_diagnostic='+json.dumps({'unit':unit, 'peer':peer, 'journal':journal,
        'manager_incus_admin':manager_group()}, sort_keys=True))


if __name__ == "__main__":
    main()
