# Temporary SSH access for a yard

Run on the yard's **owner host**, where the private Git key is stored:

```sh
yard ssh-agent start --key ~/.ssh/id_ed25519 --ttl 14d
yard ssh-agent status
yard ssh-agent stop
```

Use `-Y NAME` to select another local yard. A duration is required: seconds, minutes,
hours, or whole days, from `1s` through `30d`. `start` confirms the selected yard and
access before reading the key. OpenSSH asks for an encrypted key's passphrase in the
owner terminal. No existing SSH agent is required. For unattended tests, an
unencrypted disposable key and explicit `--yes` can be used.

After `start` succeeds, ordinary Git-over-SSH commands use the shared agent from
existing and new `dev` terminals, `yard shell`, and `yard clone`. No `export
SSH_AUTH_SOCK` is needed. HTTPS Git authentication is unchanged.

## Prerequisites

Run current `yard init` on the owner first. It provisions the dedicated SSH login,
pins the guest host key, installs the guest SSH client default, and enables the
owner's systemd user manager with lingering. The yard must be running when granting
access. No host network, firewall, Incus mount, or SSH server policy is changed by
`ssh-agent start`.

The command is owner-local. For a yard on another server, connect to that owner and
run the command there. The command does not transfer private key input through
Subyard RPC. Ordinary per-session `FORWARD_SSH_AGENT` remains a separate opt-in.

## Scope and lifetime

Each selected yard gets a dedicated host SSH agent containing the selected key.
**Every process running as `dev` in that yard can use the key's existing upstream
permissions**, including push if allowed. The feature does not enforce read-only
Git, repository restrictions, or approval for individual Git operations.

The private key is read by `ssh-add` on the owner and is never copied into the yard.
The background connection uses ordinary OpenSSH agent forwarding. A private guest
runtime link at `~/.subyard/run/ssh-agent.sock` points to the current session socket.
The managed `/etc/ssh/ssh_config.d/50-subyard-agent.conf` chooses it only while a
shared socket exists; otherwise ordinary `SSH_AUTH_SOCK` handling remains available.
Explicit user SSH configuration takes precedence. In particular, a custom
`IdentityAgent`, `IdentitiesOnly`, or Git `core.sshCommand` may override this default.

The maximum lifetime starts when `start` creates the background service, including
time spent entering a passphrase. Both the loaded key and the service are bounded.
Closing the owner terminal does not stop access. A broken connection or yard restart
can reconnect using the original, still-loaded agent, without extending the deadline.
The worker never rereads or automatically reloads the private key. An owner reboot
or dead agent requires a new explicit `start`.

`stop` and `yard teardown` terminate this yard's service and agent.
`yard security` reports a warning while temporary access is granted. Expiry and stop prevent **new SSH
authentications using this agent**; they do not terminate sessions already
authenticated, revoke other copies of the key, or disable another available identity.
A repeated `start` while access is active is rejected; inspect `status` or explicitly
`stop` before replacing it. A repeated `stop` is safe.

## Diagnostics

```sh
yard ssh-agent status --json
```

Status reports `yard`, `state` and, when known, `expires_at`. States are `loading`,
`connecting`, `active`, `expired`, and `stopped`. `active` means the managed forwarding
session is connected, not that a particular Git provider has accepted the key.
Status does not list identities, source key paths, or internal runtime paths.

If startup fails, its dedicated service is stopped. Check the owner's dedicated yard
SSH access and reconcile with `yard init`. A stopped owner user manager or disabled
lingering also requires owner-side initialization. Git provider authorization and
host-key verification must still be configured normally inside the yard.
