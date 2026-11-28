# Herdr

Herdr Core runs in a container yard. Desktop and mobile clients connect to it
through SSH over the yard owner's tailnet. The client machine needs the official
Herdr app and access to the tailnet. The yard runs Core without the desktop app.

## Enable and connect

The guest OpenSSH server must allow public-key authentication and use the
development user's `.ssh/authorized_keys`. `up` installs a global Herdr drop-in
and verifies that password and keyboard-interactive authentication are disabled
before publishing the owner proxy. Remove any overriding directives from the
guest SSH configuration. Herdr is unavailable on
VM and nested E2E yards. Alternate OpenSSH configs set through `SSHD_OPTS` are
outside the supported setup.

```sh
yard herdr up
yard herdr pair --key 'ssh-ed25519 AAAA...' --name laptop
```

Pairing prints the host, port, user, Ed25519 host-key fingerprint, an `hc machine
add` command and an SSH config `Host` block. Herdr names a machine only on the
client, so both suggest `<host-id>-<yard>`, lowercased, such as
`owner-host-default`. The label is omitted when it would not be a valid Herdr
machine name: at most 32 characters of `a-z`, `0-9`, `.`, `_` and `-`, starting
with a letter or digit. In desktop Herdr, use Add Machine with `user@host:port`. On mobile,
copy the public key from the app for pairing, then enter the printed host, port
and user. Verify the host-key fingerprint when prompted. Use a separate key per
client. `yard herdr pair --list` lists non-owner SSH grants and distinguishes
Herdr labels from unmanaged lines. `yard herdr pair --revoke SHA256:...` removes
matching Herdr grants for future authentication. Both commands work after `down`
while the yard is running. Connected sessions can persist.

To reuse keys that may already log in to the owner host account, run
`yard herdr pair --from-host`. It lists each key in that account's
`~/.ssh/authorized_keys` with a label derived from the key comment and one
status:

- `new`: the key can be paired.
- `granted`: the key already has a Herdr grant; the label is the grant's.
- `unmanaged`: the yard already accepts the key through a line outside Herdr.
  Pairing refuses it and leaves that line unchanged; `--list` shows it the same
  way.
- `unknown`: the guest grants could not be read, for example before `up`
  installed the helper. Pairing still checks the guest when it applies.
- `skipped:owner`, `skipped:options` or `skipped:certificate`: the key is never
  offered.

Lines that are not public keys are reported as `line N  unrecognized`.
`yard herdr pair --from-host --all` pairs every new key after one
confirmation, and `--from-host SHA256:...` pairs only the listed fingerprints.
Only bare public-key lines are offered. Lines with options such as `from=`,
`command=`, `restrict` or `cert-authority`, certificates and the Subyard owner
key are skipped, because Herdr bypasses the host's SSH policy. To pair a skipped
client key, review it and use `--key`, which also sets a custom label. Removing
a key from the host file does not revoke its Herdr grant; use `--revoke`. Keys
from `AuthorizedKeysCommand`, other `AuthorizedKeysFile` paths or Tailscale SSH
are not read.

The preferred owner port is `6768`. The allocator can choose a different port.
Use `yard herdr status` to find the endpoint and machine label and grant TCP access to that port in
your Tailscale policy. Restrict the grant to the clients that need yard access.
`HERDR_ADVERTISE_HOST` and `HERDR_HOST_PORT` override discovery and allocation.

## Spaces and lifecycle

Each registered project gets a named space for `src` without a waiting shell.
Every other checkout gets its own space, named after the project and the path:
each Git repository below `src` at any depth, including nested repositories,
submodules and linked worktrees, as Orca finds them, as `Project/private`, and
each Git repository directly in the project directory beside `src`, such as a
sibling worktree, as `Project/wt-feature`. Repositories inside a sibling
checkout get no space. Herdr's sidebar nests worktrees under the checkout they
share a Git directory with.

Project hooks, `up` and `sync` recheck the known checkouts and list each project
directory for sibling repositories, without a full scan. A guest timer resumes a
bounded scan below `src` 15 seconds after boot and after each run, so a new
nested clone appears within about a minute and a half. Directory symlinks and
`.git` contents are not traversed.

A checkout proven deleted loses its space together with its tabs once no
terminal in it is live; until then `yard herdr status` reports pending cleanup.
A scan error, a missing `src` or a checkout that lost its `.git` removes nothing:
the other checkouts are still reconciled, and `sync` and readiness report the
error.
When a whole project is removed, its checkout spaces follow the same rule, and
the `src` space stays while it has user layout or live terminals. No terminal is
stopped. Closing a space in Herdr closes its terminals. Use
`yard herdr sync --restore` to reopen closed spaces for present checkouts.

Core keeps terminals alive when clients disconnect. `yard herdr update` selects
the latest supported protocol generation release and keeps live terminals across
handover. `yard herdr update --force` selects Subyard's pinned release. An update
leaves Core running as-is when Core already runs the installed selected release. `up`
repairs a Core below the supported minimum. The startup marker in `status` is a
hint. Use `yard herdr restart` to refresh Core's login environment.

`yard herdr down` removes the managed listener and SSH drop-in, disables the
checkout discovery timer, then leaves the yard's SSH configuration to its
remaining settings. It stops the whole Core,
including running programs. Project layout and authorization lines are retained.
A local Herdr command or a separately authorized SSH connection can start Core
again. A guest reboot loses running processes. Kept panes can return as shells.

Desktop Herdr has an upstream telemetry setting under Settings → Privacy.
Protocol and desktop acceptance exercise the connection path. Official clients
on a physical phone are outside the Subyard test coverage.
