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

Provisioning installs only these generic OS prerequisites:

```text
build-essential ca-certificates curl git libffi-dev python3-dev xz-utils
```

Subyard does not install, locate, validate or update Hermes software. It does not select a Hermes
release or component, run an upstream installer, assume an installation layout, or create a
launcher, configuration or service. Installation, setup and updates remain independent of the
Subyard release lifecycle and belong to Hermes and the operator.

Install and manage Hermes with the current upstream instructions as the ordinary `dev` user after
provisioning the yard:

```sh
yard -Y hermes shell
# Follow the upstream Hermes installation and setup instructions here.
```

The yard root filesystem is persistent. Files installed or created independently by the operator
survive repeat provision, stop/start and an instance restart. Repeat `yard provision` reconciles
only the generic prerequisites and does not inspect or change Hermes-owned files. A confirmed
`yard teardown` destroys the yard and its state while retaining the reusable named-yard definition.

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

The real-host lane verifies the generic prerequisites, proves that fresh provision does not install
Hermes, and checks isolation. It then creates opaque operator-managed state and proves that repeat
provision, stop/start and an instance restart preserve it. The lane checks the typed Tailscale route
with an application-neutral loopback fixture; it does not install, configure or test Hermes.

```sh
dev/agent-e2e.sh --prepare
dev/agent-e2e.sh --status
slot=1  # Choose an available configured slot from status.
dev/agent-e2e.sh --slot "$slot" --purpose hermes-profile --vm both -- \
  ./dev/e2e/hermes-profile.sh
```

Upstream references:

- [Linux installation guide](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/getting-started/installation.md)
- [Official installer](https://github.com/NousResearch/hermes-agent/blob/main/scripts/install.sh)
- [GitHub releases](https://github.com/NousResearch/hermes-agent/releases)
