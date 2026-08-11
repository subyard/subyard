# Paseo Desktop

Subyard can run one headless Paseo daemon inside a yard and expose its coding agents to Paseo
Desktop through Paseo's hosted relay. The package is optional and is not installed by the shipped
agent defaults.

## Enable and pair

Add `paseo` to the effective `AGENTS` setting for the selected yard:

```sh
AGENTS="paseo"
```

Paseo declares Codex as a required provider. Subyard expands the dependency automatically and
installs its pinned Codex package before Paseo, regardless of the order of entries in `AGENTS`.
Include other agents in the same setting when that yard should expose their command-line tools too.

Put this in the host-wide Subyard settings for the default yard, or in the selected named-yard
settings. Apply it with the normal provisioning command:

```sh
yard init
yard -Y <name> init
```

Provisioning installs the pinned headless runtime, starts it as the yard's `dev` user, checks its
loopback API and hosted-relay connection, and registers existing Subyard checkouts. It never prints
the pairing offer and does not start or modify Codex login. Authenticate Codex explicitly as the
yard's `dev` user when needed, then retrieve the trust anchor only when you are ready to add the
connection in Paseo Desktop:

```sh
ssh yard -- paseo-pair
ssh yard-<name> -- paseo-pair
```

Scan the QR code or paste the printed offer into Paseo Desktop. The SSH command then exits; normal
Desktop use does not require an SSH tunnel or an open terminal. Pair each yard separately.

## Projects and network boundary

The daemon listens only on `127.0.0.1:6767` inside the yard. It makes an outbound TLS connection to
the hosted Paseo relay at `relay.paseo.sh:443`; no public yard port is opened. Availability from
Desktop therefore depends on that hosted service. The relay can observe connection metadata such as
IP addresses, timing, message sizes, session identifiers, and public handshake frames, while Paseo's
application channel uses its upstream end-to-end handshake.

Valid checkouts under `/srv/workspaces` and their immediate child directories with an independent
`.git` directory are registered automatically. Successful `yard sync`, `bind`, `clone`, and `remove`
operations trigger an event-driven additive reconcile; daemon startup and `yard init` also retry it.
Subyard does not rename, archive, or delete Paseo-only projects.

## Recovery and troubleshooting

Run the package check from the yard when Desktop cannot connect:

```sh
ssh yard -- paseo-check
ssh yard-<name> -- paseo-check
```

The check runs directly as the SSH `dev` user, verifies the Subyard-managed Codex package and its
`app-server` capability, and does not require `sudo`, SSH agent forwarding, or a login attempt.

Then inspect `systemctl status paseo.service` from a shell in the yard and rerun `yard init` to
repair package, configuration, or unit drift. A hosted-relay outage can make Desktop unavailable
while the local health check remains green.

Treat every pairing URL or QR code like a password. If one is exposed, enter the yard, run the
following as root, and pair every client again:

```sh
sudo paseo-rotate-identity
```

The command requires explicit confirmation, backs up the current daemon identity, restarts and
checks the daemon, and restores the old identity if readiness fails. Rotation invalidates all saved
Paseo connections for that yard.
