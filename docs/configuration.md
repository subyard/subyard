# Subyard configuration

Subyard combines immutable settings shipped with its runtime, persistent local settings, and
temporary command overrides into one effective configuration. Use the CLI to inspect that result;
the files remain the source of truth.

```sh
yard config fields
yard config fields SSH_PORT
yard config show
yard config show SSH_PORT
yard -Y demo config show
yard config paths
```

`config fields` is the public typed field reference. It reports the shipped default, kind, type,
allowed scopes, syncability, merge mode, application mode and domain owner from the same catalog
used by the production resolver. `config show` lists effective non-secret settings, their winning
scope and source, and how they are consumed. Passing one setting name shows every applicable layer
as `effective`, `overridden`, or `unset`. Unknown fields, wrong scopes and invalid values fail
closed; secret inputs and unrelated environment variables are not settings.

## Storage roles

The default configuration root is `~/.config/subyard`. It contains several roles, not one monolithic
configuration:

| Path | Role |
|---|---|
| `overrides/shared/config.env` | Explicitly shareable scalar settings |
| `config.env` | Host-wide scalar settings |
| `overrides/shared/` | Explicitly shareable non-secret file settings |
| `overrides/host/` | File settings specific to this owner host |
| `yards/<name>/config.env` | Named-yard definition and scalar settings |
| `yards/<name>/overrides/` | File settings specific to one yard |
| `secrets/` | Secret inputs, not settings |
| `generated/` | Materialized consumers, not settings |
| `keys/` | Encrypted credential ledger and its state |
| `projects/` | Runtime project state |
| `tools/` | Subyard-managed support tools |

Run `yard config paths` to resolve these roles for the current installation and selected yard.
Immutable shipped defaults stay in the installed runtime and do not belong in the configuration root.

Managed configuration paths must be real (not symbolic links), operator-owned files and directories.
Subyard rejects any managed path that is writable by its group or by other users. The operator may
choose the group and other read bits for existing files; Subyard does not treat those read bits as a
confidentiality policy. New sensitive files are created with mode `0600` by default, without
silently changing the mode of existing files during update or apply.

## Scalar settings

Common portable values may go in `overrides/shared/config.env` only when `config fields` lists the
`shared` scope. Host-wide values go in `config.env`:

```sh
DEV_SUDO=1
```

A named yard has its own settings:

```sh
# ~/.config/subyard/yards/demo/config.env
SSH_PORT=2223
YARD_TEMPLATE=test-vms
```

Scalar precedence is:

```text
shipped defaults
  -> explicitly shareable scalar settings
  -> host-wide scalar settings
  -> named-yard derivations and selected shipped profile
  -> named-yard scalar settings
  -> current command environment
```

The default yard omits the named-yard layers. A command environment value is temporary and has the
highest precedence. It is never persisted by config sync. `yard [-Y <yard>] config show <SETTING>`
is the authoritative explanation of the actual chain, including derived values. Start from
[`config/settings.env.example`](../config/settings.env.example).

## File settings

Known file settings, such as coding-agent configuration and rules, start with a shipped file and may
be replaced by the matching file under `overrides/shared`, `overrides/host`, or a named yard's
`overrides` directory. Their precedence is shipped, shared, host, yard, then a command override.

These directories currently override known file settings only. They are not generic scalar
configuration directories.

## Applying changes

Settings are resolved on every `yard` command. The `APPLIES` column in `yard config show` identifies
the consumer:

- `next command` means the resolver uses the new value on the next invocation;
- `yard init` means the value controls infrastructure or provisioning reconciled by `yard init`;
- `config apply` means a file setting can be refreshed in a running local yard with
  `yard config apply`.

`yard config status [--all-local]` checks only materialized file settings in running local yards.
`yard config apply [--all-local]` refreshes those consumers after confirmation. Neither command is a
Git transport command. Use the typed persistent writers before publishing a change:

```sh
yard config set <SETTING> <VALUE> --scope shared|host|yard
yard config unset <SETTING> --scope shared|host|yard
yard config import <FILE_SETTING> <path> --scope shared|host|yard
yard config edit <FILE_SETTING> --scope shared|host|yard
```

Select a non-default yard with `-Y <yard>` before using `--scope yard`. Each writer validates the
catalog type and scope, rejects secret-looking content and asks before changing the persistent
configuration. `config edit` requires `VISUAL` or `EDITOR` to name one executable.

Register a remote owner once with `yard host add <user@host-or-ssh-alias>`. The confirmation includes
the concrete SSH server-key SHA256 fingerprint, authoritative HostID and discovered yards. Subsequent
refreshes use a controller-managed strict host-key pin; they never silently accept a changed key.
Use `yard host repair <HostID>` to review and explicitly accept a changed fingerprint (and an observed
HostID rename, if both changed). `yard host list` shows registered OwnerHosts, while `yard yards`
discovers their authoritative yards without creating controller-side yard aliases. `--all-local`
never changes remote owner hosts implicitly. The old `remote` command is an input-only compatibility
adapter for installations that still have per-yard contexts.
Legacy discovery snapshots remain untrusted. Confirmed `yard host add` upgrades an exact
same-endpoint, same-HostID snapshot atomically to managed SSH trust.

## Entity and migration vocabulary

Subyard uses `OwnerHost → Yard → Project → optional ProjectEnv`. A yard is identified as
`<HostID>/<yard-name>`; `local` and `remote` describe access, while `container` and `vm` describe its
runtime kind. The desired L1 Incus image is separate from the image fingerprint observed on a
created instance. Project-environment Docker images are an L2 setting.

Canonical configuration names are:

| Legacy input (one-minor compatibility) | Canonical writer/output |
|---|---|
| `YARD_TYPE` | `ACCESS_KIND` |
| `INSTANCE_TYPE` | `YARD_KIND` |
| `INSTANCE_NAME` | `YARD_INSTANCE_NAME` |
| `REMOTE_DEST` | `OWNER_ENDPOINT` |
| `REMOTE_YARD` | `OWNER_YARD_NAME` |
| `BASE_IMAGE`, `BASE_IMAGE_FALLBACK` | `YARD_IMAGE`, `YARD_IMAGE_FALLBACK` |
| profile-level `BASE_IMAGE` | `PROJECT_ENV_BASE_IMAGE` |
| `YARD_PROFILES` | `ENVIRONMENT_PROFILES` |
| `AGENTS` | `CODING_TOOL_INTEGRATIONS` |

Legacy names are accepted only while reading an old input. A file or command environment containing
conflicting old and canonical values is rejected. `config set`, generated context, JSON and RPC
writers emit canonical names only.

## Versioned private configuration

Keep private desired settings in a separate clean Git checkout. Do not turn the entire
`$SUBYARD_CONFIG_HOME` into a checkout: it also contains project state, credential records, generated
consumers and support tools. `.gitignore`, a symlink farm or recursive `rsync --delete` is not an
ownership boundary.

Release installation and migration never ask for a Git URL or require network access. Connect the
private repository explicitly once on each physical owner host:

```sh
yard config sync connect \
  git@github.com:you/subyard-config.git \
  --host-id workstation-a
```

`sync connect` prepares the clone in a private temporary directory, validates its selected HostID
and exact adoption plan, then asks once before installing the checkout, registering its path and
applying that plan. The default destination is `~/.local/share/subyard-config`; use `--checkout` to
select an existing checkout or another destination. An existing checkout must have the requested
`origin`. A declined or invalid staged clone is removed.

Yard does not store Git credentials. Configure SSH or a Git credential helper on that owner host;
credential-bearing URLs, URL queries and fragments are rejected. Use `--init` only when connecting
an empty remote: after the same preview it creates and pushes a minimal initial manifest using the
operator account's configured Git identity. No background fetch, pull, commit or push runs
automatically.

The checkout root contains a tracked `subyard-config.json`:

```json
{
  "schemaVersion": 1
}
```

Its fixed managed layout is:

```text
subyard-config.json
shared/                         # optional common scope
  config.env
  overrides/
    agents/...
hosts/                          # optional host overlays
  <HostID>/                     # optional selected-host overlay
    config.env                  # optional host-wide scalars
    overrides/
      agents/...
    yards/
      <yard>/
        config.env              # required for an existing yard entry
        overrides/
          agents/...
```

`shared/` and the selected `hosts/<HostID>/` overlay are independent and optional. A source with
only the manifest is a valid empty desired state. A selected host directory may contain only
`overrides/` or `yards/`; its `config.env` is optional. An existing
`hosts/<HostID>/yards/<yard>/` entry still requires `config.env`, because the directory declares a
yard that needs an unambiguous scalar definition. Scalar assignments must be syncable in their
exact shared, host or yard scope. Versioned file settings are regular, non-executable files at
catalog-known paths below `overrides/agents`; path assignments in `config.env` are rejected. The
source manifest schema is
[`config/subyard-config.schema.json`](../config/subyard-config.schema.json).

The first sync snapshots an owner-host ID. By default it uses the current hostname; set
`SUBYARD_HOST_ID=<safe-id>` for the first invocation to choose another value. The saved
`$SUBYARD_CONFIG_HOME/host-id` is local identity state, is never imported from Git and is not renamed
by later syncs. It is not published or added to the source automatically. Each host selects only
`shared` and its exact `hosts/<HostID>` overlay, so another host subtree is neither applied nor an
enrollment requirement. Two hosts may have yards with the same name without collapsing them.

After onboarding, use the bounded sync commands for transport and history. The registered path is
available through `config sync path`, and bare `config sync` imports the current checkout without
network access:

```sh
yard config sync status
yard config sync pull --apply
yard config sync push -m "Update host configuration" --apply
```

`sync status` fetches by default and reports registration, sanitized remote, branch/upstream,
checkout HEAD, dirty/conflict counts, ahead/behind/diverged relation, last fetch, applied commit,
generation and recovery state. `--offline` uses cached refs. A fetch/auth failure still prints the
available local diagnostics and exits non-zero. Automation is manual.

`sync pull` permits only a clean fast-forward of the exact upstream, validates the candidate before
one confirmation, then imports it transactionally. `sync push` exports only explicit
catalog-known, syncable, non-secret persistent settings, creates one commit from `-m` using the
operator's Git identity, validates/imports it locally and pushes only `HEAD` to the exact upstream
without force. It never reads configuration back from a running container and never exports keys,
secrets, projects, generated state or arbitrary runtime files. Dirty, conflicted, detached,
upstream-less or diverged checkouts fail closed; Subyard does not stash, merge, rebase, reset or
resolve conflicts.

`--check` is read-only, never prompts, and exits non-zero when an apply or local manifest update is
needed. A changing sync prints the source commit and exact redacted managed-path plan, then asks once.
`--apply` composes the import with `yard config apply` for affected running local yards under the
same top-level confirmation. It does not run `yard init`, start, stop, teardown, project operations
or remote fan-out. Remaining follow-up commands are printed by application mode.

For bare checkout-to-live import, an existing unmanaged target requires a reviewed first import with
`--adopt`. `sync push` instead adopts only its own exact validated persistent export. Later local
edits are reported as managed drift and restored only through the confirmed exact plan. A path
removed from Git is deleted only when the local manifest owned its previous exact digest. Removing a
yard definition fails while its Incus yard or project state still exists; sync never becomes
teardown. Removing the selected host subtree means an intentionally empty host overlay: the exact
plan removes only its previously managed paths, subject to the same digest, drift and in-use guards.
Unmanaged local paths are left alone.

The source must be an operator-owned Git worktree root with a clean selected subtree. Tracked,
untracked, ignored, unmerged, symlinked, hard-linked, executable or group/world-writable inputs fail
closed. `projects`, desired power, observed Incus state, SSH trust, secrets, keys, generated
consumers, exports, logs, storage and support tools are outside the source schema and local manifest.

To roll back desired settings, check out or revert the intended Git revision and run the same check
and sync commands. An interrupted confirmed transaction is recovered before the next mutating sync;
`--check` reports pending recovery without changing the live root.

When invoked for a registered remote yard, the `config sync` family runs on that owner host. For
example, for HostID `owner-host` and its `default` yard:

```sh
yard -Y owner-host/default config sync connect \
  git@github.com:you/subyard-config.git \
  --host-id owner-host
yard -Y owner-host/default config sync status
yard -Y owner-host/default config sync pull --apply
```

The checkout and Git authentication stay on the owner host; the controller does not upload or cache
the repository and there is no implicit all-host fan-out.

### Bootstrapping an existing host

Before creating optional settings, classify current values with `yard config fields` and inspect
their provenance with `yard config show`. Only fields marked `syncable: yes` may be copied:

- portable fields that explicitly allow `shared` go to `shared/config.env`;
- host-specific fields go to `hosts/<HostID>/config.env`;
- named-yard fields go to `hosts/<HostID>/yards/<yard>/config.env`;
- catalog-known agent config and rules files keep their relative path below the matching
  `overrides/agents` directory;
- secrets, keys, generated consumers, project state, host identity, desired power and support tools
  stay local and are never copied.

A minimal empty remote can be initialized without reading or copying the whole live root:

```sh
yard config sync connect \
  git@github.com:you/subyard-config.git \
  --host-id replace-with-stable-host-id \
  --init
```

For an existing repository, connect it directly. To publish real persistent settings, change them
through the typed writer and let `sync push` build the exact managed export:

```sh
yard config sync connect \
  git@github.com:you/subyard-config.git \
  --host-id replace-with-stable-host-id
yard config set SSH_PORT 2222 --scope host
yard config sync push -m "Set host SSH port"
```

Do not copy `~/.config/subyard` recursively. In particular, do not add ignored secret or runtime
paths just to make the worktree appear clean: selected ignored and untracked source paths are
rejected. After `connect`, the checkout path and saved local `host-id` are authoritative. Each
additional owner host runs its own `sync connect`; a matching subtree does not need to exist in Git
when that host uses only shared settings.
