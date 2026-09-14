# Subyard workflows

Subyard organizes development into two runtime levels:

- **L1 — Yard:** a persistent Incus container or VM where agents and project workspaces live.
- **L2 — Project environment:** an optional Docker-based environment selected by a project profile.
- **Profiles:** reusable project toolchains and resources under `config/profiles/`.
  Yard templates and coding-tool integrations are selected independently; see
  [named yards](../config/yards/README.md).

The canonical entity chain is `OwnerHost → Yard → Project → optional ProjectEnv`. A project can run
directly in its yard, or it can select a profile with `--target <profile>` and use `yard up`, `down`,
and `info` for the optional L2 environment.

Each yard can select `YARD_KIND=container` (the default) or `YARD_KIND=vm`, its own
`YARD_IMAGE`, and optional `LIMITS_CPU` / `LIMITS_MEMORY`. The shipped system images are Debian 13
with Ubuntu 24.04 as fallback; provisioning expects Debian-family package tools. VM yards require
hardware virtualization exposed by the owner host. A VPS must provide the relevant devices to run
VM workloads; see [host prerequisites](getting-started.md) and the [test VM pool](test-vms.md).

## Put a project in a yard

Subyard supports three admission modes:

| Command | Result | Relationship to the source |
| --- | --- | --- |
| `yard sync [path]` | A snapshot in yard-owned storage | Later edits on either side are independent. |
| `yard clone <url>` | A Git clone created directly in yard-owned storage | The repository remote remains the source relationship. |
| `yard bind [path]` | An explicit Incus disk mount at the project workspace | Yard processes read and write the selected host directory directly. Local yards only. |

A new project's safe name is also its project ID and its workspace directory under
`/srv/workspaces/<name>/src`. The source basename or repository name is used by default. If it is
already taken, Subyard allocates `<name>-2`, `<name>-3`, and so on. Pass `--name NAME` to `sync`,
`bind`, or `clone` when the identity should be explicit.

Repeated `sync` and `clone` commands create independent copies, even when their source is the same.
They do not update an earlier copy. An explicit name must be available instead of silently replacing
an existing project.

For a synced project, `yard export [project]` compares the original host directory with the current
yard copy, excludes `.git`, and writes a portable patch under Subyard's protected data directory.
It does not overwrite the host checkout. Bound and cloned projects cannot be exported through this
command.

`yard remove [project]` detaches a bind without deleting its host directory. For yard-owned projects,
normal removal deletes the yard workspace; `--soft` keeps that copy after removing its registration.

## Everyday commands

| Command | Purpose |
| --- | --- |
| `yard check` | Verify that the owner host can run a yard. |
| `yard init` | Install or reconcile the yard and its core configuration. |
| `yard start`, `yard stop` | Manage the selected yard instance. |
| `yard status` | Summarize every known yard, or show details for an explicitly selected yard. |
| `yard security` | Audit static and available live host-boundary invariants. |
| `yard sync`, `yard bind`, `yard clone` | Add a project using one of the modes above. |
| `yard list` | List registered projects across the selected inventory scope. |
| `yard shell [project]` | Open a development shell, optionally in a project directory. |
| `yard code [project]` | Open a project with VS Code Remote-SSH. |
| `yard export [project]` | Create a patch from a synced yard copy. |
| `yard remove [project]` | Detach or remove a project. |
| `yard provision [profile]` | Apply a toolchain profile directly to the yard. |
| `yard up`, `yard down`, `yard info [project]` | Manage or inspect an optional L2 project environment. |
| `yard space [--refresh]` | Show disk usage for local yards. |
| `yard test-vms <command>` | Inspect or manage the retained nested test-VM pool. |
| `yard keys <command>` | Manage the host-side encrypted credential ledger. |
| `yard ssh-agent <command>` | Grant, inspect, or stop temporary SSH-key access. |
| `yard host <command>` | Register and manage remote owner hosts. |
| `yard config <command>` | Inspect, author, synchronize, and apply configuration. |
| `yard update`, `yard migrate` | Change the installed release or finish its migrations and repairs. |

Use [temporary SSH access](ssh-agent.md) when Git inside a yard needs one of the owner's SSH keys.

## Select local and remote yards

One owner host can run several independent local yards, each with its own instance, persistent
`/srv`, SSH port, host-mount root, and projects. Use `-Y <yard>` or the first-token `@<yard>` shorthand
to select one:

```bash
yard -Y openclaw init
yard @openclaw status
yard status
yard @default status
yard space
yard @openclaw space --refresh
```

Without a selector, most commands use the default yard. Bare `yard status` and `yard list` are
inventory views across known owners and yards; selecting a yard scopes them. Bare `yard space` reads
the latest cached measurement for every local yard. `--refresh` synchronously recalculates running
local yards, while `-Y` or `@name` limits the read or refresh to one local yard. Remote yards do not
support `space`.

Register another owner host over SSH, then address its yards by the stable `<HostID>/<yard>` identity:

```bash
yard host add me@srv1
yard host list
yard list
yard -Y owner-host/default list
```

Replace `owner-host` with the authoritative HostID reported by `yard host add`. A short yard name is
accepted only when it identifies one known yard; when owners contain the same name, use the full
selector. Remote yards support project `sync` and `clone`. `bind` remains host-local because it
mounts a path from the yard's physical owner.

Read [configuration](configuration.md) for OwnerHost registration and trust, and [named yards](../config/yards/README.md)
for local-yard definitions.

## Product environments and shared staging

A yard can host agents, their project checkouts, and shared environments for the services they
are developing. A project environment supplies a checkout's runtime; a shared resource can serve
several agents or checkouts in the same yard. Profiles define the toolchains, dependencies, and
resource lifecycle needed to reproduce the product and verify its behavior.

Use these environments to run services, reproduce failures, inspect logs, and exercise integration
or end-to-end tests. Services and test resources may stay available for reuse or be started and
stopped as needed. Some resources use broker leases for exclusive access. The concrete
profile or resource defines its prerequisites, sharing rules, and lifecycle; the following
integrations are examples of that model.

### Project environments

Use `yard sync --target <profile> PATH` or the corresponding option on `bind` or `clone` when a
project needs an L2 environment. `yard info PROJECT` displays its contract; `yard up PROJECT` builds
or starts it, `yard up --rebuild PROJECT` rebuilds it, and `yard down PROJECT` stops it. Project
profiles can also contribute L1 requirements, reconciled by `yard init`, and named resources
managed through their own lifecycle commands.
The [control-plane guide](control-plane.md#profile-resources) documents that extension contract.

### Resource examples

The `test-vms` profile is specifically an agent E2E facility. Its root-owned broker leases an
explicit slot containing a retained pair of nested VMs. Ordinary lease release fences access, trims
and stops the pair while preserving its disks. The broker API is limited to that test pool and its
lease lifecycle.
Follow the ownership, allocation, and recovery rules in the [Agent E2E VM pool guide](test-vms.md).

OpenClaw's `yard staging start|status|stop` resource is likewise owned by the OpenClaw profile: it is
an opt-in staging gateway isolated from production. The same profile owns `yard qa-pool`, an in-yard
credential broker that leases distinct staging test bots to concurrent OpenClaw QA runs. These
commands require the OpenClaw profile's prerequisites and resource configuration. The shipped
[OpenClaw L1 guide](../config/profiles/openclaw/openclaw-l1.md) covers that profile's build and test
lane.

Use [`yard keys`](keys.md) for selected static staging and QA credentials. The ledger stays on the
owner host and materializes only an authorized consumer file; it does not synchronize coding-agent
OAuth or session stores.

[AI Observer](ai-observer.md) is enabled in the default coding-tool integrations. It imports and
watches Claude Code and Codex session files through read-only mounts and keeps its statistics database
on the yard's persistent `/srv` volume, so ordinary restarts and disable/re-enable cycles preserve
the collected history. By default, Subyard also keeps the session data for Claude Code, Codex,
OpenCode, and pi on the host-backed agent-session store across yard resets; their credentials remain
separate. AI Observer currently reports only the Claude Code and Codex formats.
