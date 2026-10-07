# Veranda UX design

Veranda is the graphical Subyard client for users who prefer not to use the terminal for routine
configuration, navigation, monitoring, and diagnostics. It is a Tauri 2 desktop application with a
thin Rust shell and a Svelte/TypeScript interface over the typed Yard RPC protocol.

This directory records the initial UX contract. It is a planning artifact, not a promise that every
illustrated control is implemented by the current engine. A surface must remain hidden or disabled
until capability negotiation establishes its typed owner-side RPC support.

Open [`wireframes.html`](wireframes.html) in a browser to review the approved low-fidelity screens.

## Current implementation

The implementation under [`../../veranda/`](../../veranda/) includes local fleet navigation,
SSH host registration and repair, profile selection and observed convergence, typed host/yard
settings and diagnostics, offline synchronization status, exact operation review and cancellation,
yard creation, and shell/editor launch descriptors. Persistent native RPC sessions handle bounded
requests and ordered events; a lost or inconsistent stream triggers an authoritative refresh.
An interrupted mutation stays unknown until the owner provides a confirmed result.

The native boundary checks the exact product version, RPC v1, and required capabilities before
exposing each surface. The language-neutral contracts and production-codec fixtures live in
[`../../api/yard-rpc/v1/`](../../api/yard-rpc/v1/); the client requirements are recorded in
[`../../veranda/compatibility.json`](../../veranda/compatibility.json). The frontend receives narrow
validated DTOs and never parses human CLI output. First-use and replacement host keys require
explicit native consent; domain mutations execute the digest of the reviewed owner plan once.

Authentication currently uses the system SSH agent. Remote VS Code additionally requires its
Remote - SSH extension and an agent key already authorized in the selected yard. Veranda does
not transfer the owner's private guest key or grant guest access implicitly. Native Windows/macOS
builds, interactive launch behavior, signed delivery, package upgrade/rollback, and resource
acceptance still require their platform checks. A successful candidate build alone does not
establish release readiness. Development and verification commands are documented in
[`../../veranda/README.md`](../../veranda/README.md).

The advisory [change-impact testing workflow](../testing.md) may recommend Veranda unit tests,
static checks, the production build, and Rust tests without desktop features. It recommends these
host-free checks without executing them.

## Product model

The interface is object-centric:

```text
Owner host
└── Yard
    └── Project
```

The persistent fleet navigator shows only owner hosts and yards. Projects remain inside the selected
yard's `Projects` tab so that the navigator stays compact.

On startup, Veranda restores the last available selection. If it is unavailable, the application
selects the current local owner host and its current/default yard. Remote onboarding never replaces
the normal startup screen.

## Application shell

The left side contains the persistent owner-host and yard fleet. The right side shows the selected
object. Actions are always scoped to that selected object.

When an owner host is selected, the available tabs are:

- `Overview`
- `Yards`
- `Sync`
- `Diagnostics`
- `Settings`

Host actions include opening a host shell, copying its shell command, and opening a terminal window
that runs `htop` for CPU and memory inspection. Veranda does not introduce a metrics backend for this
initial slice. If `htop` is unavailable, it offers a normal host shell and does not install packages.

When a yard is selected, the available tabs are:

- `Overview`
- `Projects`
- `Profiles`
- `Diagnostics`
- `Settings`

Yard actions include opening a yard shell and copying its shell command. The header may show the
yard's authoritative RPC/Incus state, such as `RUNNING` or `STOPPED`. That state belongs to the yard
and must not be attributed to its projects.
Starting, stopping and reconciling a yard are explicit owner operations with exact-plan review.
Creating a named yard selects a reported profile preset and prepares its bootstrap through an
existing owner session; opening an unknown yard is never a substitute for creation.

## Projects

The first project surface is deliberately small. It displays an ordinary list of project names with
two actions per row:

- `Open in VS Code`
- `Shell`

The UI does not infer whether an agent is using a project. It does not invent project health,
activity, or running state. Additional project facts require an explicit typed API contract.
Remote VS Code requires a client agent key already authorized in the yard, in addition to trust
and authentication for the owner host.

## Profiles

The selected yard's `Profiles` tab shows a simple list containing:

- selection checkbox;
- profile name and short description;
- `Applied` or `Available` state;
- `Details` action.

The first version uses Subyard's current shipped profile model through a narrow typed owner-side
RPC projection. It includes profiles with an optional `profile.json` descriptor, profiles without a
descriptor, and resource-only profiles. A future component registry is not a prerequisite. The UI
does not discover profile files, execute hooks, or maintain its own profile allowlist.

Changing a checkbox updates selection through the server-side exact-plan and confirmation contract.
Deselecting a profile does not uninstall its artifacts. Provisioning or reconciliation remains a
separate explicit owner-side operation. Desired selection, effective selection, and provenance come
from the owner; the UI never becomes a second desired-state authority.

`Applied` requires an observed owner-side check; selection alone is insufficient. An unavailable or
inapplicable check displays `Unavailable` or `Not applicable`. Reading status never starts a stopped
yard. Only confirmed owner data supplies profile details and descriptions.

An incompatible product version, profile/RPC schema, or required capability shows an actionable
error before affected mutations. The message includes the Veranda and Subyard versions and recommends
installing both from the same release. The UI does not migrate an unknown schema or fall back to
parsing human CLI output.

## Diagnostics

The selected yard's diagnostic surface renders typed RPC facts without deriving a composite health
score. Its initial fields include:

- yard state and desired power;
- initialization and autostart;
- SSH configuration, IP, and development user;
- VS Code and service readiness;
- storage, managed mounts, and security facts.

An unknown fact is displayed as `Unavailable`, not interpreted as a failure.

The host has its own `Diagnostics` tab. Host diagnostics must likewise use typed owner-side facts or
an explicitly launched host terminal tool rather than frontend inference.

## Settings

Host and yard `Settings` show typed, non-secret effective Subyard settings. Each row contains the
setting name, effective value, provenance/scope, and an `Edit` action. Set and unset operations use
typed validation and the server-side exact plan.

Profiles, credentials, projects, and observed runtime facts are separate domain surfaces and are not
placed in the generic settings table. Secret values never appear in this surface.

## Host synchronization

Synchronization belongs to the selected owner host. The host-level `Sync` tab contains two visibly
separate cards:

1. `Configuration repository` manages sanitized Git registration, status, pull, push, and import for
   versioned non-secret configuration. Git credentials are managed by the owner account and are not
   stored by Veranda.
2. `Credentials` displays only redacted credential metadata, trusted peers, last success/failure,
   conflicts, and a `Sync now` action. Secret payload never crosses the frontend IPC boundary.

If no configuration repository is registered, the first card offers `Connect Git repository`.

## Remote owner-host connection

`Connect remote host` is a link in the fleet navigator. Selecting it temporarily replaces the right
workspace with an inline flow:

1. enter an SSH destination; authentication uses the system SSH agent;
2. verify the host key before the first session;
3. negotiate Yard RPC version and capabilities;
4. discover the owner host's yards;
5. save the connection and add the owner host with its yards to the fleet.

The connection represents one owner host. Veranda does not register every remote yard as an
independent connection.

On Linux, Windows, and macOS, Veranda's native shell owns one app-local connection and trust store.
Records contain the authoritative HostID, SSH destination, authentication references, and pinned
public host keys. CLI controller registrations have their own authority. The desktop store uses
private permissions or ACLs, atomic updates, and stale-baseline checks.

Authentication currently uses the system SSH agent; additional key/keychain reference types require
a separate native implementation. Private keys, passphrases, and
tokens do not enter frontend state or the connection store. Native metadata changes expose a typed
assessment and follow the shared confirmation policy before commit and verification. First-use host
keys require explicit acceptance before a trusted RPC session; replacement requires a separate
repair decision. Domain mutations continue to use the owner engine's exact plan.

## Initial delivery order

The first mutating GUI vertical slice is `Connect remote host`. It exercises transport, host-key
verification, compatibility handling, discovery, reconnect behavior, and fleet state without adding
a second domain model. The next mutating slices are yard creation and profile selection.

Read-only host/yard navigation and the minimal project list should precede broad configuration
coverage. Every new UI field requires a typed owner-side source or a separately designed RPC slice.

GUI development and focused local/SSH verification can proceed using the implemented owner
exact-plan contract. Outstanding application/provider acceptance and defects remain tracked by
their owning tasks and must be resolved before releasing affected functionality. Required release,
artifact, and profile checks still apply; focused GUI checks do not replace them.

## Safety and accessibility

- The frontend receives no arbitrary shell or filesystem capability.
- Owner mutations display the authoritative server-side plan; native connection/trust changes display
  their typed local assessment. Both follow the shared confirmation policy.
- Shell, VS Code, and resource-terminal launches are navigation/session-entry actions rather than
  implied domain mutations.
- Host keys are verified before first remote use; secrets and private keys do not enter frontend
  state or logs.
- All status information has a text label and never relies on color alone.
- Controls support keyboard navigation, visible focus, and reduced motion.
