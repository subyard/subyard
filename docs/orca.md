# Orca remote server

Subyard can run a pinned stock Orca server inside a selected yard. Orca is an opt-in
profile resource, not a `CODING_TOOL_INTEGRATIONS` entry. Tailscale and SSH remain on the physical
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
registered/total checkout counts, including repository kinds and project-group membership,
without printing a pairing capability. Incomplete scans and registration failures are listed.

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
project-group and checkout reconciliation.

The link is a single-client capability. Keep it private and do not put it in config,
shell history, tickets, or logs.

## Projects and lifecycle

Each Subyard project has one Orca group containing its canonical
`/srv/workspaces/<project-id>/src` root and every nested Git checkout. The root is always
registered: as a Git repository when it is a Git root, or as a folder otherwise. The
group and root initially use the Subyard project's name. Nested checkouts use paths
relative to the root, such as `private` or `packages/backend`.

Discovery includes ignored, hidden, vendor and fixture directories, nested repositories
inside other repositories, initialized submodules and linked worktrees. It skips directory
symlinks and Git's internal directories. Distinct linked-worktree paths remain separate
entries even when they share a repository or remote. Use Orca's native Git diff for each
checkout; Subyard does not create sessions automatically.

`up` and `pair` reconcile this complete set. Later Subyard clone, sync, bind and remove
actions invoke the same hook. An explicit `init` repairs the common dispatcher and retries
installed hooks once for active resources, including when provisioning is already current.
These hooks do not start a stopped Orca service. Run `orca up` to install or repair the
Orca component and register projects accumulated while it was stopped.

There is no background discovery. An ordinary nested `git clone` becomes visible after
the next Subyard project action or explicit sync:

```sh
yard -Y demo orca sync
```

Repeated sync preserves group IDs, manual names, colors and display order. Subyard owns
membership: project checkouts moved elsewhere are returned to the project's group. On
first registration, existing project checkouts in a mixed user group move into a dedicated
project group; unrelated entries and the user group's properties are preserved. Group
names may coincide or be renamed without merging project identities.

If a directory disappears, or a nested checkout loses its Git metadata, old Orca records
and sessions remain with a diagnostic warning. Sync does not restore files or Git history.
Manually deleted Orca entries and groups for existing directories are registered again.
If the root gains or loses Git, its kind changes on the existing entry, preserving its ID
and session data.

An individual registration failure does not undo a successful Subyard project action;
the command displays a warning and other checkouts are still attempted. Explicit `orca sync`
fails when registration is incomplete. `status` reports partial scans, missing entries,
kind mismatches and incorrect membership. Discovery and RPC calls are bounded; reaching
a limit is reported as incomplete. Rerun after resolving the reported problem.

If a creation request has an unknown result, sync first checks the catalog. It avoids
sending another create while the first request could still finish. If the pending result
remains absent, run `orca restart` followed by `orca sync` to retry against a new runtime.

After adding a group, reopen an already connected Orca Desktop if needed to refresh its
group catalog. The reopened client should show the project root and every nested checkout.

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
