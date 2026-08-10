# Veranda UX design

Veranda is the graphical Subyard client for users who prefer not to use the terminal for routine
configuration, navigation, monitoring, and diagnostics. It is a Tauri 2 desktop application with a
thin Rust shell and a Svelte/TypeScript interface over the typed Yard RPC protocol.

This directory records the initial UX contract. It is a planning artifact, not a promise that every
illustrated control is implemented by the current engine. A surface must remain hidden or disabled
until capability negotiation establishes its typed owner-side RPC support.

Open [`wireframes.html`](wireframes.html) in a browser to review the approved low-fidelity screens.

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

## Projects

The first project surface is deliberately small. It displays an ordinary list of project names with
two actions per row:

- `Open in VS Code`
- `Shell`

The UI does not infer whether an agent is using a project. It does not invent project health,
activity, or running state. Additional project facts require an explicit typed API contract.

## Profiles

The selected yard's `Profiles` tab shows a simple list containing:

- selection checkbox;
- profile name and short description;
- `Applied` or `Available` state;
- `Details` action.

Changing selection uses the server-side exact-plan and confirmation contract. Once the registry
surface is available, the UI distinguishes desired selection, effective selection, and provenance;
it never becomes a second desired-state authority.

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

1. enter an SSH destination and choose an authentication reference;
2. verify the host key before the first session;
3. negotiate Yard RPC version and capabilities;
4. discover the owner host's yards;
5. save the connection and add the owner host with its yards to the fleet.

The connection represents one owner host. Veranda does not register every remote yard as an
independent connection.

## Initial delivery order

The first mutating GUI vertical slice is `Connect remote host`. It exercises transport, host-key
verification, compatibility handling, discovery, reconnect behavior, and fleet state without adding
a second domain model. The next mutating slices are yard creation and profile selection.

Read-only host/yard navigation and the minimal project list should precede broad configuration
coverage. Every new UI field requires a typed owner-side source or a separately designed RPC slice.

## Safety and accessibility

- The frontend receives no arbitrary shell or filesystem capability.
- Mutating operations display the authoritative server-side plan and follow its confirmation policy.
- Shell, VS Code, and resource-terminal launches are navigation/session-entry actions rather than
  implied domain mutations.
- Host keys are verified before first remote use; secrets and private keys do not enter frontend
  state or logs.
- All status information has a text label and never relies on color alone.
- Controls support keyboard navigation, visible focus, and reduced motion.
