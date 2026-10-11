# OpenLIT native agent telemetry

The optional `openlit` environment profile runs OpenLIT, its private ClickHouse database,
and a dedicated OpenTelemetry Collector inside one yard. It collects native OpenTelemetry
logs/events and metrics from the selected Claude Code and Codex integrations. Ordinary yards
remain independent of this profile, and it is never selected by default.

Enable it on an existing running yard:

```sh
yard -Y demo profile enable openlit
yard -Y demo profile status openlit
yard -Y demo profile disable openlit
```

The commands use Subyard's existing profile selection and confirmation. `yard provision openlit`
can also select the profile through normal yard reconciliation. A stopped yard is left stopped;
its next normal initialization reconciles the profile. No coding-agent wrapper, OpenLIT SDK,
application instrumentation or session-file parser is installed.

The yard must have core Docker Engine and Compose provisioning, internet access for the first
bounded image download, and enough memory for the service environment. The stack caps ClickHouse
and OpenLIT at 2 GiB each and the dedicated Collector at 256 MiB. Image pulls have a four-minute
limit; initial Compose startup has a separate three-minute limit. Failed setup can be retried.
The profile uses public multi-architecture image manifests pinned by SHA-256: OpenLIT 2.1.0, ClickHouse 24.4.1,
and Collector Contrib 0.162.0. See the [shipped stack](../config/profiles/openlit/compose.yaml).

OpenLIT listens at `http://127.0.0.1:3000` **inside the yard**. The dedicated Collector accepts OTLP
gRPC on `127.0.0.1:4317` and OTLP HTTP on `127.0.0.1:4318`; its health endpoint is
`127.0.0.1:13133`. Docker publishes these ports only on yard loopback. ClickHouse and OpenLIT's
internal receiver have no published ports. Use an existing trusted SSH connection with forwarding
for browser access; the profile does not open an owner-host listener or public endpoint. Sign in
and manage the local OpenLIT account through its upstream UI. All agents in the same yard share
the yard's trust boundary and can reach its local services.

Native logs and metrics flow through the dedicated Collector to OpenLIT's internal OTLP HTTP
receiver, which writes `otel_logs` and `otel_metrics_*` tables in the `openlit` database.
OpenLIT's coding-agent views and generic telemetry queries consume these tables. Native agent
signals carry their original names and attributes; the profile does not manufacture OpenLIT SDK
spans or rewrite metrics into apparent model calls. Available dashboard detail depends on what
that CLI version exports. Traces, stdout/stderr capture, other coding tools and application SDK
telemetry are outside this profile's configuration.

Claude Code receives its native telemetry environment settings through `~/.claude/settings.json`.
Codex receives its native log and metric exporters through the user-level `~/.codex/config.toml`;
project-local Codex configuration does not enable these exporters. Both use OTLP HTTP/protobuf
and the dedicated loopback Collector. Prompt export is disabled. Existing Codex response,
guardian, tool-result and trace choices are preserved; exporting telemetry can still include tool
metadata and other native event attributes. Consult the upstream native telemetry contracts when
choosing what information belongs in a yard.

Claude's log and metric routes use signal-specific endpoint, protocol and header fields. Its existing
trace exporter and shared OTLP endpoint, protocol and headers remain unchanged. This profile does
not route those traces to its Collector.

Repeated setup and ordinary agent config refresh preserve the telemetry fields. The profile owns
only its declared native telemetry leaves, records their previous values in a root-private file,
and preserves unrelated custom settings. Disabling stops/removes the Compose containers, disables
the systemd unit, and restores still-owned leaves to their previous values (or removes them when
they were absent). A telemetry field edited while enabled causes reconciliation to fail without
overwriting it; disable preserves that edit. Disable then re-enable when changing native telemetry
settings intentionally. Selecting or deselecting Claude Code or Codex adds or removes only that
agent's telemetry leaves during normal reconciliation.

Release update and rollback refresh the enabled profile and verify its running services,
configuration and native telemetry settings. A stopped yard remains stopped. Changes since the
release assessment cause apply to fail; retry with a fresh assessment. Failed or interrupted setup
remains incomplete until the desired stack passes verification.

Runtime refresh is limited to 110 seconds. If that bound is insufficient, retry normal yard
initialization to complete the longer initial setup, then reassess the release operation. See the
[runtime checks](../config/profiles/openlit/runtime.py) and
[guest lifecycle](../config/profiles/openlit/guest.sh) for implementation details.

Persistent agent CONFIG templates must leave the profile's telemetry leaves unmanaged. A template
that owns an overlapping Claude `env` key or Codex `otel.exporter`, `otel.metrics_exporter` or
`otel.log_user_prompt` is rejected before profile service changes. Move those choices out of the
template or use native telemetry independently. The profile requires the standard home configuration
destinations; alternate `AGENT_*_CONFIG_DEST` paths are rejected. Malformed files, symlinked native
configuration, foreign service units and foreign Compose resources are rejected rather than adopted.

Disable retains the database volumes, SQLite account data, pinned images and root-private runtime
state under `/var/lib/subyard/openlit`. Re-enable reuses them and skips registry downloads when all
pinned images are already present locally. Yard teardown removes the yard's owned storage under
the existing teardown policy. The profile generates a private database password
inside the yard and does not require or transfer model-provider credentials.

Profile-owned host-free checks run with:

```sh
bash config/profiles/openlit/tests/run.sh
```

Physical acceptance uses one fresh standard leased VM, through the documented
[VM workflow](test-vms.md#agent-workflow):

```sh
bash config/profiles/openlit/tests/e2e/acceptance.sh --slot N
```

The fixture checks default-off initialization, public profile enable, repeat init/config apply,
actual native Claude Code and Codex exporter ingestion into OpenLIT logs and metrics tables,
guest runtime CAS rejection, actual Collector bind replacement, recipe update/rollback, and
disable with custom settings preserved. Model responses come from local synthetic HTTP providers;
it uses no live model account or external model calls. A passing host-free runner alone does not
establish service startup, native telemetry ingestion or release acceptance.

Upstream contracts checked for this profile:

- [OpenLIT 2.1.0 deployment](https://github.com/openlit/openlit/blob/openlit-2.1.0/docker-compose.yml)
  and [integrated receiver configuration](https://github.com/openlit/openlit/blob/openlit-2.1.0/assets/otel-collector-config.yaml).
- [Claude Code native telemetry](https://code.claude.com/docs/en/monitoring-usage).
- [Codex native telemetry configuration](https://learn.chatgpt.com/docs/config-file/config-reference)
  and [native configuration schema](https://github.com/openai/codex/blob/main/codex-rs/core/config.schema.json).
- [OpenTelemetry Collector configuration](https://opentelemetry.io/docs/collector/configuration/).
- [Pinned Collector image user and contents](https://github.com/open-telemetry/opentelemetry-collector-releases/blob/v0.162.0/distributions/otelcol-contrib/Dockerfile).
