# Hermes Agent yard

The `hermes` profile provides a private, persistent Linux yard for an upstream-managed Hermes
Agent. Subyard owns the container boundary and the optional owner-host route. Hermes owns its
components, configuration, credentials, services and application behavior.

## Create the yard

```sh
yard -Y hermes init --profile hermes
yard -Y hermes provision
```

The shipped preset deliberately selects no coding-tool integration, host mount, host link,
capability, device or forwarded SSH agent. The `dev` user has no passwordless sudo and Tailscale is
not installed inside the yard. Inspect the effective boundary with:

```sh
yard -Y hermes config show ENVIRONMENT_PROFILES
yard -Y hermes config show CODING_TOOL_INTEGRATIONS
yard -Y hermes config show HOST_MOUNTS
yard -Y hermes config show HOST_LINKS
yard -Y hermes config show FORWARD_SSH_AGENT
yard -Y hermes security --require-live
```

Provisioning resolves the latest non-draft, non-prerelease GitHub release, verifies its immutable
tag commit, and reads `scripts/install.sh` from a shallow checkout of that exact release. It runs
the installer as `dev` with the supported non-root layout and defers interactive setup. Subyard
does not execute anything from the user-owned Hermes tree as root, select individual Hermes
components, or add its own package, model, plugin, launcher, updater or service policy.

The resulting upstream paths are:

| Purpose | Path |
| --- | --- |
| Hermes state and installation root | `/home/dev/.hermes` |
| Source checkout | `/home/dev/.hermes/hermes-agent` |
| Python environment and CLI | `/home/dev/.hermes/hermes-agent/venv` |
| User-facing CLI launcher | `/home/dev/.local/bin/hermes` |

These paths live inside the yard's persistent filesystem. Repeat provision, stop/start and an
instance restart preserve them. A confirmed `yard teardown` destroys the yard and its state while
retaining the reusable named-yard definition.

The official installer may create its own configuration skeleton and directories. That is upstream
behavior: Subyard neither creates nor interprets Hermes `.env`, `config.yaml`, credentials,
memories, sessions, skills or task data. Run the normal upstream setup from an ordinary shell after
provisioning:

```sh
yard -Y hermes shell
hermes setup
```

Use upstream Hermes commands for subsequent setup, component installation and updates. Repeat
`yard provision` only reconciles the substrate and leaves a healthy existing installation and its
state untouched.

## Optional browser route over Tailscale

The yard can publish an already-running Hermes loopback endpoint through the owner host. The owner
host must have Tailscale; the yard does not receive Tailscale credentials or a Tailscale device.

After the operator has used upstream Hermes setup to provide an authenticated browser endpoint on
`127.0.0.1:9119`, select one active owner-host Tailscale hostname or IPv4 address and a unique host
port:

```sh
yard -Y hermes config set HERMES_DASHBOARD_ADVERTISE_HOST owner.example-tailnet.ts.net --scope yard
yard -Y hermes config set HERMES_DASHBOARD_HOST_PORT 19119 --scope yard
yard -Y hermes dashboard up
```

The operator can then open `http://owner.example-tailnet.ts.net:19119/` from a browser on an
authorized tailnet device.

The resource adds one typed Incus proxy:

```text
tcp:<exact-owner-tailscale-ip>:19119 -> tcp:127.0.0.1:9119 (bind=host)
```

It refuses wildcard, non-Tailscale and ambiguous owner addresses, requires the guest loopback port
to be listening, and records an owner-side fingerprint of the exact device it creates. That durable
ownership record lets the route be safely replaced or removed after its hostname or port setting
changes without authorizing deletion of a foreign same-name device. The resource does not start,
stop or reconfigure Hermes. Application authentication remains an upstream/operator responsibility.
Remove only the owned route with:

```sh
yard -Y hermes dashboard down
```

## Maintainer acceptance

The real-host lane installs through the official release bootstrap, verifies the canonical paths
and isolation, writes opaque state, and proves it survives repeat provision, stop/start and an
instance restart. It checks the typed Tailscale route with an application-neutral loopback fixture;
it does not run Hermes setup or test Hermes features.

```sh
dev/agent-e2e.sh --prepare
dev/agent-e2e.sh --purpose hermes-profile --vm both -- \
  ./dev/e2e/hermes-profile.sh
```

Upstream references:

- [Linux installation guide](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/getting-started/installation.md)
- [Official installer](https://github.com/NousResearch/hermes-agent/blob/main/scripts/install.sh)
- [GitHub releases](https://github.com/NousResearch/hermes-agent/releases)
