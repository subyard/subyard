# Subyard

> Give agents a yard, not the house keys.

Subyard gives AI coding agents a persistent Linux workspace isolated from the
host by default. It runs an unprivileged Incus instance (the **yard**) and
exposes the workflow through one `yard` CLI.

## Model

- **L1 — Yard:** the persistent Incus container where agents and projects live.
- **L2 — Project environment:** an optional Docker Compose stack selected by a
  project profile.
- **Profiles:** reusable project and agent configuration under `config/`.

`yard sync` copies a project into yard-owned storage. `yard bind` may instead
mount any host directory explicitly; the CLI warns because this weakens the
yard's encapsulation. A new project's safe name is also its ID and workspace
directory; basename collisions become `Name-2`, `Name-3`, and so on. Use
`--name NAME` with `sync`, `bind`, or `clone` to choose an explicit name.

## Quick start

Subyard targets a Linux amd64 or arm64 host with Incus. The installer downloads a verified,
self-contained release runtime, so the operator CLI does not require Go or compile source at runtime.
The host needs `curl`, `jq`, `sha256sum`, `tar`, and `gzip`. The installer shows every local change
and asks once before it links `yard` and `sy` into `~/.local/bin`, configures new-shell PATH, and
enables shell completion.

```bash
curl -fsSL --proto '=https' --tlsv1.2 \
  https://github.com/Subyard/Subyard/releases/latest/download/subyard-install.sh | bash
exec "$SHELL" -l
yard check
yard init

yard sync .
yard code .
yard status
```

The runtime contains the engine, public profiles/config, completions and host adapters, but no
source checkout, toolchain or private data.
Subyard has persistent [shared, host and yard configuration](docs/configuration.md). It can be
synchronized between owner hosts through git. Run `yard config sync help` for
setup, status, pull and push examples.

Upgrade with `yard update`; use `yard update --rollback` to swap back to the retained previous
runtime.

Run `yard --help` or `yard <command> --help` for complete command usage.
See the [control-plane architecture](docs/control-plane.md) for module ownership, stable extension
contracts, test topology, and the real-host acceptance lane. Paseo Desktop support is available as
an [opt-in agent package](docs/paseo.md). A pinned
[Orca remote server](docs/orca.md) is available as an opt-in profile resource.
For a dedicated Hermes backend, see the [Hermes profile guide](docs/hermes.md).

## Everyday commands

```text
yard start | stop                  Manage the yard instance
yard security                      Audit the host boundary
yard sync | bind | clone           Add a project
yard list                          List projects
yard space [--refresh]             Show disk usage for every local yard
yard shell | code [project]        Open a project session
yard export | remove [project]     Copy out or remove a project
yard provision [profile]           Apply a project profile
yard test-vms <command>            Manage two disposable nested test VMs (opt-in)
yard up | down | info [project]    Manage an L2 project environment
yard keys <command>                Manage the host-side encrypted credential ledger
yard config <command>              Inspect or sync settings and refresh file consumers
```

## Multiple and remote yards

Use `-Y` or `@name` to select a named local yard. A registered owner host is reached over SSH;
inventory selectors use the stable `<HostID>/<yard>` identity:

```bash
yard -Y openclaw init
yard @openclaw status
yard status
yard @default status
yard space
yard @openclaw space --refresh

yard remote add srv1 me@srv1
yard list
yard -Y owner-host/default list
```

`yard space` reads the latest cached measurement for every local yard. Use
`yard space --refresh` to synchronously recalculate all running local yards, or
combine `space` with `-Y`/`@name` to inspect or refresh only one yard.

Use a full selector
when a short name is ambiguous. Remote yards support `sync` and `clone`;
`bind` is local-only. See
[Subyard configuration](docs/configuration.md), [named yards](config/yards/README.md), and the
[credential ledger](docs/keys.md).

## Security boundary

The yard is an unprivileged container by default. Managed host mounts must stay
under that yard's `HOST_BASE`, while an explicit `yard bind` grants the selected
host path to the yard. Host Docker and Incus control sockets are rejected by
managed configuration. Run `yard security` to audit the effective setup.

A trusted test yard can opt in to two disposable nested VMs without receiving the
L0 Incus socket. See [Disposable nested test VMs](docs/test-vms.md) for the widened
device/syscall boundary, lifecycle and cleanup contract.

Subyard protects the host boundary. It does not isolate credentials between
agents operating inside the same yard.
