# Getting started

You need a **GNU/Linux host** — laptop, workstation, or server; `yard init` installs
Incus for you. Native Windows/macOS support isn't planned — run Subyard inside a
Linux VM and follow the steps below.

Release runtimes support amd64 and arm64. The host also needs `curl`, `jq`,
`sha256sum`, `tar`, and `gzip`. The release installer downloads and verifies a self-contained
runtime, so the operator CLI does not need Go or a source checkout at runtime.

## Install a release

```bash
curl -fsSL --proto '=https' --tlsv1.2 \
  https://github.com/Subyard/Subyard/releases/latest/download/subyard-install.sh | bash
exec "$SHELL" -l
yard check
yard init
```

The installer shows every local change and asks once before it links `yard` and `sy` into
`~/.local/bin`, configures `PATH` for new shells, and enables shell completion. The installed runtime
contains the engine, public profiles and configuration, completions, and host adapters. It contains
no source checkout, toolchain, or private data.

`yard check` is a read-only host preflight. `yard init` installs or reconciles Incus, the selected
yard, and its core configuration. Both `yard init` and later reconciliation runs are designed to be
repeatable.

Run `yard --help` or `yard <command> --help` for complete command usage.

## Add a first project

From a directory that contains `my-project`, make a named snapshot in the default yard:

```bash
yard sync --name demo ./my-project
yard shell demo
```

Or open the same project through VS Code Remote-SSH:

```bash
yard code demo
```

The copy is stored inside the yard at `/srv/workspaces/demo/src`; editing it does not edit
`./my-project`. See [Workflows](workflows.md) before choosing `bind`, cloning a repository, or
exporting changes.

## Configure Subyard

Subyard has persistent shared, owner-host, and per-yard configuration. Inspect the effective values
and their sources before changing them:

```bash
yard config fields
yard config show
yard config paths
```

Common non-secret settings can be synchronized between owner hosts through an explicitly connected
Git checkout. The runtime never connects a configuration repository automatically. Run
`yard config sync help` for setup, status, pull, and push examples, and read the full
[configuration guide](configuration.md) for scopes, validation, file settings, and synchronization.

Credentials use the separate [host-side encrypted ledger](keys.md); they do not belong in the
configuration checkout.

## Update or finish an installed migration

Upgrade the installed release with:

```bash
yard update
```

Subyard retains the previous verified runtime. Swap back to it with:

```bash
yard update --rollback
```

An installed release can also report migrations or runtime repairs that still need to finish.
Inspect them without changing state, then apply them with the already installed release:

```bash
yard migrate --check
yard migrate
```

`yard migrate` does not download an update. If inspection says another release must be activated,
use `yard update` instead.
