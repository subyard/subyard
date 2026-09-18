# Orca remote server

Subyard runs a pinned stock Orca server inside a selected yard. The public resource
profile at `config/profiles/orca/resources/orca.res` declares its endpoint defaults
and first-run bootstrap. Tailscale and SSH stay on the physical owner host.

## Connect over Tailscale

Install Subyard on the server and Orca Desktop on the laptop. Connect both machines
to Tailscale and allow the laptop to reach the server's selected TCP port under your
tailnet access policy. On the server, run:

```sh
yard orca up
yard orca pair
```

These commands use the default yard. Add `-Y <yard>` for another registered yard.
`up` adds Orca to that yard's profiles without replacing existing selections, runs
the required yard initialization, installs Orca and registers existing projects.
It shows one combined plan before applying changes. Repeating it converges the same
configuration and preserves server state.

Paste the final `orca://pair?...` line into **Settings → Remote Orca Servers → Add
Server** on the laptop. This is a private single-client capability: do not put it in
config, shell history, tickets or logs. Generate a separate link for each laptop.
`pair` briefly restarts the service; existing grants and server state survive.

For another client computer, run `yard orca pair` again on the server and import the new
link on that laptop. Existing clients may briefly disconnect during the restart,
but keep their saved access. Ordinary reconnects do not require another pairing.

Orca connects directly over Tailscale. SSH over Tailscale is sufficient for running
these server commands; an SSH port-forward is only needed for the alternative
loopback setup below.

## Connect Orca Mobile

After updating Subyard, run `up` once to install the current pairing wrapper, then
request a mobile link on the owner host:

```sh
yard orca up
yard orca pair --mobile
# For another registered yard:
yard -Y <yard> orca pair --mobile
```

Install [Orca Mobile](https://www.onorca.dev/docs/mobile) on iOS or Android and
connect the phone to the owner's Tailscale network. In the app, choose **Pair**
and paste the final `orca://pair?...` line. Connect that phone before requesting
a link for the next one: Orca reuses a pending invitation until a client connects,
then issues a new link. Keep these access capabilities out of config, shell
history and logs.

Mobile pairing briefly restarts the existing server and synchronizes project
groups and checkouts. Connected clients may disconnect temporarily; their saved
grants, projects and server state survive. The mobile request applies to that
startup only. Ordinary `yard orca pair` continues to issue Desktop links.

The phone must keep network access to the advertised owner address and selected
TCP port, including any automatically allocated nondefault port. For a Tailscale
endpoint, keep Tailscale connected on both phone and owner. An explicitly selected
loopback endpoint requires an SSH tunnel on the phone. A pairing link does not
create a tunnel. The pinned headless Orca server does not initialize Orca Relay;
this profile does not provide an automatic relay through Orca's Internet servers.

### Allow the owner port in Tailscale

An access policy allowing SSH (`tcp:22`), HTTPS (`tcp:443`) or ICMP does not allow
Orca's preferred `tcp:6768` port. In Tailscale **Access controls**, allow your client
group to reach your owner-host tag on the selected TCP port. Using a tag covers
other owner hosts with that tag and port without a separate rule for each IP.

For example, if your policy already defines `group:developers` and `tag:orca`, add
this entry to its existing `grants` array:

```json
{
  "src": ["group:developers"],
  "dst": ["tag:orca"],
  "ip": ["tcp:6768"]
}
```

Use your existing group and host tag, and replace `6768` if `yard orca status`
reports another port. See the [Tailscale policy reference](https://tailscale.com/docs/reference/syntax/policy-file).
Subyard configures the host-to-yard proxy; the tailnet administrator controls this
network access policy.

If Desktop reports **Host unavailable**, run this on the laptop, replacing `HOST`
and `PORT` with the endpoint from `yard orca status`:

```sh
curl --noproxy '*' --connect-timeout 5 --max-time 10 -sS -o /dev/null \
  -w 'HTTP %{http_code}\n' http://HOST:PORT/
```

A response from the owner host itself only verifies its local route into the
yard. The laptop must also reach the port. A successful `tailscale ping --tsmp`
does not prove policy access: it stops before the access-policy check, as described
in [Tailscale's policy diagnostics](https://tailscale.com/kb/1338/acl-edit).

## Endpoint defaults and overrides

On first bring-up, the profile discovers the owner's Tailscale MagicDNS name,
falling back to its Tailscale IPv4 address when no name is available or the owner
cannot resolve the name to its Tailscale address. It verifies
that the endpoint belongs to the active owner-host Tailscale interface. If Tailscale
is unavailable, bring-up reports how to configure the host or choose SSH forwarding.

The preferred owner port is **6768**, matching Orca's internal service port. If that
port is occupied or reserved for another yard, initial setup selects a subsequent
available port. Owner-wide allocation is serialized; if another operation changes
the proposed allocation while confirmation is pending, rerun `orca up` to obtain
a fresh plan.

The selected address and port are saved under the owner's
`$SUBYARD_HOME/resource-endpoints/`. They survive restart, `down`/`up`, and runtime
updates. They are local runtime state and are not transferred by configuration sync.
An occupied saved port produces an error instead of silently changing the endpoint.

Inspect the effective values and their provenance:

```sh
yard config show ORCA_ADVERTISE_HOST
yard config show ORCA_HOST_PORT
yard orca status
```

Explicit address and port settings take precedence independently. For example,
to select a different owner port:

```sh
yard config set ORCA_HOST_PORT 17678 --scope yard
yard orca up
```

`17678` here is only an override example. Per-yard overrides, including the default
yard's overrides, live in `yards/<yard>/config.env`; host-wide settings remain in
`config.env`. Changing an endpoint requires clients to connect to the new address;
`pair` refuses to issue a link until `up` has applied the endpoint settings.

`up` publishes only on the selected Tailscale IPv4 address or explicit loopback.
`status` reports profile selection, project-hook readiness, registered checkouts
and the owner route without returning a pairing capability.

## SSH forwarding

For ordinary SSH forwarding, explicitly select loopback before bring-up:

```sh
yard config set ORCA_ADVERTISE_HOST 127.0.0.1 --scope yard
yard orca up
yard config show ORCA_HOST_PORT
```

On the laptop, replace `PORT` with the effective port shown above and keep this
terminal running:

```sh
ssh -N -o ExitOnForwardFailure=yes -L PORT:127.0.0.1:PORT operator@owner-host
```

The laptop and owner ports must match because the pairing link advertises that
loopback endpoint. Then run `yard orca pair` on the server and import its link in
Desktop as described above.

## Projects and lifecycle

Subyard replaces Orca's stock Codex YOLO launch default with an explicit empty
argument setting. With the default account, Codex then reads the yard's
`~/.codex/config.toml`, including
its approval policy and reviewer. The shipped configuration allows local work
inside the yard while keeping user approval for matching commit/push rules.
Orca may label this launch mode **Manual**; that label does not mean a read-only
Codex sandbox.

Paired desktops can also send their own stock YOLO argument. In the yard's
default Bash shell, an Orca-only login function removes that exact argument
before executing the native Codex CLI. This keeps remote agent launches on the
yard configuration too. SSH/VS Code shells and the installed Codex binary are
unchanged. A desktop may still display its local YOLO preference; it does not
describe the effective server launch. Custom shells and explicit executable
paths bypass this Bash integration. Account-specific `CODEX_HOME` is preserved;
an account configured with a different home reads its own Codex configuration.

`orca up` repairs fresh and existing stock defaults through Orca's settings API.
The installed project hook also checks them on `orca sync` and subsequent
`yard init` runs while Orca is active. Other settings and explicitly customized
Codex arguments are preserved. Put persistent Codex policy in the yard's agent
configuration; selecting YOLO or another session mode in a client can override
it. Existing Codex sessions are not reconfigured by this repair.

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
yard orca sync
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
yard orca status
yard orca restart
yard orca logs
yard orca logs --follow
yard orca down
```

`restart` recovers the existing service without returning a pairing link. `logs`
prints at most the latest 18,000 journal lines; `--follow` prints the same bounded
history and then follows new entries.

`down` removes the owner proxy and stops Orca while preserving the installed package,
projects, sessions, and paired-client state.
