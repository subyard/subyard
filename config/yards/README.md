# Named yards

Create an installed yard at:

```text
~/.config/subyard/yards/<name>/config.env
```

Source checkouts may still use `private/yards/<name>.env`; release installation migrates it to
the installed path. Start from [`example.env`](example.env). `SSH_PORT` must be unique; the yard
name derives the instance, Incus project, SSH alias, storage volume and host-data root.

```sh
yard -Y openclaw init
yard @openclaw status
yard status
yard -Y default status
yard yards
```

`yard status` summarizes all local and owner-inventory yards. An explicit selector, including
`-Y default`, requests detailed status for exactly one yard; `yard status --all` remains the
explicit summary form.

Scalar-setting precedence is:

```text
shipped defaults
  -> explicitly shareable scalar settings
  -> host-wide config.env
  -> yard derivations + selected shipped profile
  -> yards/<name>/config.env
  -> command environment
```

`overrides/shared/config.env` is the typed shared scalar layer. The other files below
`overrides/shared`, `overrides/host` and yard `overrides` replace catalog-known file settings; they
are not arbitrary configuration trees.

Public profiles remain in the immutable runtime. Set `YARD_TEMPLATE=<profile>` for a reusable yard
template, `ENVIRONMENT_PROFILES="<profile> ..."` to limit project EnvironmentProfiles, and
`CODING_TOOL_INTEGRATIONS="<integration> ..."` independently for coding tools. Neither selection is
derived from the yard name. Run
`yard -Y <name> config show` to inspect effective settings and `yard config paths` to inspect storage
roles. `yard config status --all-local` / `yard config apply --all-local` verify or refresh
materialized agent files in local yards; remote yards are excluded from `--all-local`.

`yard teardown` removes only the selected yard and preserves the host credential ledger. Managed
mounts stay under that yard's `HOST_BASE`; `yard bind` is the explicit exception.

For the disposable two-VM profile:

```sh
YARD_TEMPLATE=test-vms
SSH_PORT=2223
```

For an isolated Hermes backend, see the [Hermes profile guide](../../docs/hermes.md).

See [Subyard configuration](../../docs/configuration.md), [`docs/test-vms.md`](../../docs/test-vms.md)
and [`docs/keys.md`](../../docs/keys.md).
