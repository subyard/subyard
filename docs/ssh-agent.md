# Per-yard SSH agent

Subyard can unlock one SSH private key into a separate, owner-host agent for each yard. The agent
continues running after the command and terminal exit, and its key expires after the required
time-to-live. This leaves the user's normal SSH agent unchanged.

Run these commands on the yard's owner host. The first release does not route them from a remote
controller because the key path and passphrase prompt belong to the owner host.

```bash
yard ssh-agent unlock --key ~/.ssh/id_ed25519 --ttl 2h
yard ssh-agent status
yard ssh-agent status --json
yard ssh-agent lock
```

`--key` must name an encrypted, owner-only private-key file. `--ttl` is required, must resolve to
a whole number of seconds from 1 second to 24 hours, and accepts duration units `s`, `m`, and `h`,
such as `900s`, `30m`, `1h30m`, or `2h`. There is no unlimited lifetime.
Unlock asks for confirmation before granting access. `--yes`
answers that confirmation only; it never supplies or bypasses the private-key passphrase. The
key passphrase is read from the operator terminal. The yard must be running with its managed SSH
transport configured; run `yard init` if the command reports that transport needs reconciliation.

Status reports `locked`, `pending` (waiting for key loading), `unlocked`, or `reconnecting`, with
the remaining lifetime after activation. Reconnecting does not extend the lifetime. A host reboot
or agent worker restart requires a new explicit unlock.

Unlock replaces the key available to the selected yard. Replacement revokes the old access before
the new key becomes available. A wrong passphrase or cancelled prompt does not grant new access.
`lock` needs no confirmation and prevents new authentication with the per-yard agent. It does not
terminate SSH connections, multiplexed OpenSSH ControlMaster sessions, or transfers that were
already established. Expiry has the same limitation.

Subyard exposes a stable agent socket at `/home/dev/.ssh/subyard-agent.sock` inside the selected
yard through a reverse Unix-socket SSH tunnel. The tunnel filters the agent protocol to identity
listing and signing operations. It does not copy the private key, passphrase, or agent state into
the yard, and `status` does not enumerate loaded identities.

When `SSH_AUTH_SOCK` is unset, the managed SSH client default selects this socket while it
exists, so Git also works in
existing guest sessions and through `yard clone`. Explicit user `IdentityAgent` settings take
precedence; custom `IdentitiesOnly` or Git `core.sshCommand` settings may override this default.
When the shared socket is absent, ordinary session agent handling remains available.

New guest shells use that socket as `SSH_AUTH_SOCK` when no other socket was supplied. Orca receives
the same setting through its managed environment. The first unlock may install that environment
and restart Orca once; later unlock and lock operations do not restart it. Open a new yard login
after the initial setup if an existing terminal does not see the socket.

## Git access

Use an SSH Git remote, for example `git@github.com:owner/repository.git`, to use the per-yard agent.
The Git host still needs a normal, verified `known_hosts` entry in the yard. Unlocking a key does not
trust a Git host automatically.

`yard teardown` revokes the selected yard's grant before removing the yard. `yard security`
reports a warning while temporary access is granted.

The socket is a delegated signing capability. A process in the yard that can reach it can request
signatures until the key expires or the agent is locked. Processes running as the yard developer
share this access.
