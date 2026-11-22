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

Pairing prints the host, port, user, Ed25519 host-key fingerprint and `hc machine
add` command. In desktop Herdr, use Add Machine with `user@host:port`. On mobile,
copy the public key from the app for pairing, then enter the printed host, port
and user. Verify the host-key fingerprint when prompted. Use a separate key per
client. `yard herdr pair --list` lists non-owner SSH grants and distinguishes
Herdr labels from unmanaged lines. `yard herdr pair --revoke SHA256:...` removes
matching Herdr grants for future authentication. Both commands work after `down`
while the yard is running. Connected sessions can persist.

The preferred owner port is `6768`. The allocator can choose a different port.
Use `yard herdr status` to find the endpoint and grant TCP access to that port in
your Tailscale policy. Restrict the grant to the clients that need yard access.
`HERDR_ADVERTISE_HOST` and `HERDR_HOST_PORT` override discovery and allocation.

## Spaces and lifecycle

Each registered project gets a named space without a waiting shell. Closing a
space in Herdr closes its terminals. Use `yard herdr sync --restore` to reopen
spaces for registered projects. If a removed project's space has user layout or
live terminals, `yard herdr status` reports pending cleanup.

Core keeps terminals alive when clients disconnect. `yard herdr update` selects
the latest supported protocol generation release and keeps live terminals across
handover. `yard herdr update --force` selects Subyard's pinned release. An update
leaves Core running as-is when Core already runs the installed selected release. `up`
repairs a Core below the supported minimum. The startup marker in `status` is a
hint. Use `yard herdr restart` to refresh Core's login environment.

`yard herdr down` removes the managed listener and SSH drop-in, then leaves the
yard's SSH configuration to its remaining settings. It stops the whole Core,
including running programs. Project layout and authorization lines are retained.
A local Herdr command or a separately authorized SSH connection can start Core
again. A guest reboot loses running processes. Kept panes can return as shells.

Desktop Herdr has an upstream telemetry setting under Settings → Privacy.
Protocol and desktop acceptance exercise the connection path. Official clients
on a physical phone are outside the Subyard test coverage.
