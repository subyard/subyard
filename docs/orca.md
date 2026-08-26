# Orca remote server

Subyard can run a pinned stock Orca server inside a selected yard. Orca is an opt-in
profile resource, not an `CODING_TOOL_INTEGRATIONS` entry. Tailscale and SSH remain on the physical
owner host; Subyard does not install either inside the yard.

## Tailscale on the owner host

`ENVIRONMENT_PROFILES` is a complete whitespace-separated list. The minimal example
below uses only Orca. If the yard already uses other profiles, keep them in the value,
for example `"android orca"`. Choose a host port unique to the yard and use the owner's
existing MagicDNS name:

```sh
yard -Y demo config set ENVIRONMENT_PROFILES orca --scope yard
yard -Y demo config set ORCA_ADVERTISE_HOST owner.example-tailnet.ts.net --scope yard
yard -Y demo config set ORCA_HOST_PORT 17678 --scope yard
yard -Y demo init
yard -Y demo orca up
yard -Y demo orca status
yard -Y demo orca pair
```

Rerun `yard init` after changing the profile list so the Orca profile is selected in
the yard.

`up` verifies that the name resolves to exactly one active owner-host Tailscale IPv4
address. The Incus proxy listens only on that address and forwards to Orca inside the
yard. `status` confirms profile selection, automatic project-hook readiness, and
registered/total canonical project counts without printing a pairing capability.

## SSH forwarding

When the laptop reaches the owner host through ordinary SSH, keep the owner endpoint on
loopback:

```sh
yard -Y demo config set ENVIRONMENT_PROFILES orca --scope yard
yard -Y demo config set ORCA_ADVERTISE_HOST 127.0.0.1 --scope yard
yard -Y demo config set ORCA_HOST_PORT 17678 --scope yard
yard -Y demo init
yard -Y demo orca up
```

On each laptop, keep this tunnel running:

```sh
ssh -N -L 17678:127.0.0.1:17678 operator@owner-host
```

The local and owner ports are intentionally the same because the Orca pairing link
advertises `127.0.0.1:17678`.

## Pair laptops

Create one link per laptop:

```sh
yard -Y demo orca pair
```

Paste the final `orca://pair?...` line into **Settings → Remote Orca Servers → Add
Server** on that laptop. `pair` briefly restarts the headless service because stock
`orca serve` creates its access link at startup. Existing grants and server state
survive the restart. Before returning the link, `pair` also runs the idempotent
canonical-project reconciliation.

The link is a single-client capability. Keep it private and do not put it in config,
shell history, tickets, or logs.

## Projects and lifecycle

`up` registers every canonical `/srv/workspaces/<project-id>/src` project with stock
`orca repo add`. Later Subyard clone, sync, bind, and remove actions invoke the same
additive hook. It never removes repos or changes user-created groups.

Run the idempotent reconciliation manually when needed:

```sh
yard -Y demo orca sync
```

Inspect or stop the service:

```sh
yard -Y demo orca status
yard -Y demo orca restart
yard -Y demo orca logs
yard -Y demo orca logs --follow
yard -Y demo orca down
```

`restart` recovers the existing service without returning a pairing link. `logs`
prints at most the latest 18,000 journal lines; `--follow` prints the same bounded
history and then follows new entries.

`down` removes the owner proxy and stops Orca while preserving the installed package,
projects, sessions, and paired-client state.
