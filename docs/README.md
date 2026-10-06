# Subyard documentation

Start with [Getting started](getting-started.md) to install Subyard and create a first project copy.
[Workflows](workflows.md) explains yards, project environments, project copy modes, everyday commands
and local or remote yard selection. [Security](security.md) describes the host boundary and the
choices that widen it.

## Operator guides

- [CLI reference](cli-reference.md) — generated public command help and invocation syntax.
- [Configuration](configuration.md) — inspect, author and synchronize shared, host and per-yard
  settings.
- [Named yards](../config/yards/README.md) — run independent yards on one owner host.
- [Credential ledger](keys.md) — manage selected encrypted staging and QA credentials outside yards
  and repositories.
- [Per-yard SSH agent](ssh-agent.md) — grant temporary access to one owner-host SSH key.
- [AI Observer](ai-observer.md) — inspect persistent Claude Code and Codex usage statistics.
- [Agent E2E VM pool](test-vms.md) — configure and use disposable leased test VMs.

## Optional integrations

- [Android emulator leases](android.md) — request isolated phone/tablet runtimes and clean image caches.

- [Paseo Desktop](paseo.md) — expose coding agents through a headless Paseo daemon and its hosted
  relay.
- [Orca remote server](orca.md) — run a pinned Orca server in a yard and connect over Tailscale or
  SSH forwarding.
- [Hermes yard](hermes.md) — prepare a dedicated yard while leaving Hermes installation and
  authentication to its upstream workflow.
- [GitHub App access](github.md) — provide short-lived GitHub authentication to selected yards.
- [Veranda desktop client](../veranda/README.md) — build and run the current read-only desktop
  implementation.
- [Veranda UX contract](veranda/README.md) — review the product model, interaction contract and
  wireframes for the desktop client.

## Contributor guides

- [Development](development.md) — toolchain, build and release workflow.
- [Testing](testing.md) — select and run checks in proportion to a change.
- [Agent E2E VM pool](test-vms.md) — use the real GNU/Linux test hosts and their lease contract.
- [Real-host acceptance](real-host-acceptance.md) — verify physical host and release boundaries.
- [Control-plane reference](control-plane.md) — current command, adapter, reconciliation and extension
  contracts.

The shipped [OpenClaw L1 guide](../config/profiles/openclaw/openclaw-l1.md) documents that profile's
build and test lane. Profile-specific resources such as OpenClaw staging remain part of their owning
profile rather than a generic Subyard service.

## References

- [Decisions and glossary](reference/decisions-and-glossary.md) — current accepted decisions,
  their rationale and project terminology.
- [Architecture](reference/control-plane-architecture.md) — CLI, control-plane and Veranda design,
  ownership boundaries and resource budgets.
- [Release transitions](reference/resumable-release-transition-design.md) — migration, activation
  and recovery invariants, decision matrices and design tradeoffs.
