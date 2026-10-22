# OpenLIT native agent telemetry

The optional `openlit` environment profile runs OpenLIT, its private ClickHouse database,
and a dedicated OpenTelemetry Collector inside one yard. It collects native OpenTelemetry
logs/events and metrics from the selected Claude Code and Codex integrations, plus Codex traces. Ordinary yards
remain independent of this profile, and it is never selected by default.

Enable it on an existing running yard:

```sh
yard -Y demo profile enable openlit
yard -Y demo status
yard -Y demo profile status openlit
yard -Y demo profile disable openlit
```

Use `-Y default` for the default yard. Selected agents receive exporter configuration even
when their CLI is absent or installed only in the developer's PATH. The profile never installs
or launches an agent. No events from an absent agent is normal; a ready service can have no data.

The commands use Subyard's existing profile selection and confirmation. `yard provision openlit`
can also select the profile through normal yard reconciliation. A stopped yard is left stopped;
its next normal initialization reconciles the profile. No coding-agent wrapper, OpenLIT SDK,
application instrumentation or session-file parser is installed.

The yard must have core Docker Engine and Compose provisioning, internet access for the first
bounded image download, and enough memory for the service environment. The stack caps ClickHouse
and OpenLIT at 2 GiB each and the dedicated Collector at 256 MiB. First image delivery has a
15-minute limit for the large pinned layers; initial Compose startup has a separate three-minute
limit. Failed setup can be retried. Existing cached images skip downloads, and runtime refresh
keeps its short repair budget.
The profile uses public multi-architecture image manifests pinned by SHA-256: OpenLIT 2.1.0, ClickHouse 24.4.1,
and Collector Contrib 0.162.0. See the [shipped stack](../config/profiles/openlit/compose.yaml).

Enabling the profile automatically publishes the UI after stack readiness. Detailed
`yard -Y demo status` lists `openlit dashboard` under shared resources and links to
Telemetry → Logs. Use the Metrics and Traces tabs for the other native signals. For a container yard,
the owned proxy binds exactly the owner's active Tailscale IPv4, with owner loopback as the
fallback. `OPENLIT_HOST_PORT` can select an explicit unprivileged port; its bounded default is
`SSH_PORT + 20000` within 1024..65535. Existing listener reservations and foreign devices are
rejected before guest changes. Disabling removes only this owned publication.

Remote owner-loopback and VM yards show an SSH forwarding command and the browser URL.
Use that command before opening the URL; a VM forwards to its own loopback UI through the
configured yard SSH alias, with an outer owner tunnel for remote use. Status only observes
profile readiness and the owned route. It never starts a stopped yard or repairs a route.
Repeat initialization and release activation reconcile owner-address and port drift.

Inside the yard, the UI remains `127.0.0.1:3000`. The dedicated Collector accepts OTLP
gRPC on `127.0.0.1:4317` and OTLP HTTP on `127.0.0.1:4318`; its health endpoint is
`127.0.0.1:13133`. Those endpoints remain private to yard loopback. ClickHouse and OpenLIT's
internal receiver have no published ports. Only the UI has an owner route. Sign in and manage
the local OpenLIT account through its upstream UI. All agents in the same yard share the
yard's trust boundary and can reach its local services. The pinned UI uses its persistent
SQLite volume for its own auth secret. Credentials auth trusts the request host so the private
owner URL and localhost SSH browser origin share the same account store; OAuth callbacks are
outside this profile's access contract.

On a fresh installation, the profile replaces the upstream seed password with a unique
private password through OpenLIT's normal account API. The local account remains `user@openlit.io`
so its seeded organisation, project and default database links survive upstream restart seeding.
This email is a local login identifier; no external OpenLIT account or email verification is needed.
Read the generated credentials on your own terminal, without sharing the output:

```sh
yard -Y demo shell --root -- cat /var/lib/subyard/openlit/operator.json
```

The file and bootstrap intent are root-private and persist across disable, re-enable and update.
You can change the password through the upstream account settings; repeat setup preserves that
change, and the generated file then contains only the original bootstrap password. Existing
volumes retain their accounts and passwords and have no generated credential file. For an
established installation that still uses the upstream seed password, sign in using its existing
credentials and change the password through account settings. The profile does not reset an
existing account. Tailscale supplies network access; OpenLIT still requires its own login.

Native logs, metrics and Codex traces flow through the dedicated Collector to OpenLIT's internal
OTLP HTTP receiver, which writes `otel_logs`, `otel_metrics_*` and `otel_traces` tables in the `openlit` database.
The profile mounts the integrated receiver's configuration at OpenLIT's upstream path;
its ClickHouse exporter creates the signal tables during startup. OpenLIT readiness requires
the UI, all three signal receivers and the provisioned account's default database links to pass checks.
The canonical trace table, timestamp lookup and materialized view exist before any events arrive,
so an empty trace dashboard can run its queries. Native agent
signals carry their original names and attributes; the profile does not manufacture OpenLIT SDK
spans or rewrite metrics into apparent model calls. Available dashboard detail depends on what
that CLI version exports. LLM and Coding Agents dashboards need their specific GenAI or coding-hook
span attributes. Generic native Codex traces do not establish compatible model, token or cost cards.
The pinned upstream LLM request count can also include generic spans; that count alone does not
measure actual model calls. The profile preserves those spans rather than synthesizing SDK attributes.
Use Telemetry → Logs / Metrics / Traces with an appropriate time range and filters cleared to
inspect the available signals. Native attributes are retained, including Codex's `env`; an SDK
environment or model filter can hide records that lack that attribute. Stdout/stderr capture,
other coding tools and application SDK telemetry are outside this profile's configuration.

The header's environment selector chooses the project's database/connector binding; the seeded
`production` binding is separate from an event's `deployment.environment` attribute. Row filters
apply that attribute only when selected. Clear Filters removes those row filters without changing
the selected database/connector environment.

Claude Code receives its native telemetry environment settings through `~/.claude/settings.json`.
Codex receives its native log, metric and trace exporters through the user-level `~/.codex/config.toml`;
project-local Codex configuration does not enable these exporters. Both use OTLP HTTP/protobuf
and the dedicated loopback Collector. Prompt export is disabled. Existing Codex response,
guardian and tool-result choices are preserved; exporting telemetry can still include tool
metadata and other native event attributes. Consult the upstream native telemetry contracts when
choosing what information belongs in a yard.

Claude's log and metric routes use signal-specific endpoint, protocol and header fields. Its existing
trace exporter and shared OTLP endpoint, protocol and headers remain unchanged. This profile does
not route those traces to its Collector.

An already running agent backend must read the new settings in a new process before it exports
to this stack. Resume the same session using your CLI's normal session recovery when needed;
service readiness does not prove that an old backend reread configuration or emitted any signal.
No telemetry hook is required. The profile configures native OTLP exporters.

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
that owns an overlapping Claude `env` key or Codex `otel.exporter`, `otel.metrics_exporter`, `otel.trace_exporter` or
`otel.log_user_prompt` is rejected before profile service changes. Move those choices out of the
template or use native telemetry independently. The profile requires the standard home configuration
destinations; alternate `AGENT_*_CONFIG_DEST` paths are rejected. Malformed files, symlinked native
configuration, foreign service units and foreign Compose resources are rejected rather than adopted.

Disable retains the database volumes, SQLite account data, pinned images and root-private runtime
state under `/var/lib/subyard/openlit`, together with the Compose network. Attached compatibility
or foreign receivers are not stopped to remove that network. Re-enable reuses them and skips registry downloads when all
pinned images are already present locally. Yard teardown removes the yard's owned storage under
the existing teardown policy. The profile generates a private database password
inside the yard and does not require or transfer model-provider credentials.

Normal profile reconciliation can migrate a proven temporary Codex HTTP trace route on port
4319 to port 4318. It recognizes a protected, pinned, trace-only compatibility recipe and its
exact live container, saves only the temporary configuration leaf privately, and removes that
leaf before native ownership begins. Unsupported recipes or changed ownership remain untouched;
an unrecognized matching route stays unmanaged while the service and native log/metric routes
can remain ready. Ordinary user trace settings retain their normal preservation contract.

Running Codex backends are never restarted or killed. A post-adoption process fence retains the
temporary receiver for older backends. An owning timer checks once a minute, and stops only the
proven original receiver after those backends leave and a new Codex trace arrives through the
HTTP 4318 pipeline with the unique adoption marker. The marker is inserted without overwriting
an existing resource attribute. Missing agents, absent events, stopped services and uncertain
process/resource identities leave retirement pending without changing service readiness.
Readiness checks the owning timer's enabled and active state independently of that progress.
An unavailable ownership probe leaves a protected deferred admission intent without claiming
the receiver. The timer retries that probe without changing native settings or starting services.
Unsupported or foreign layouts stay untouched. Once full proof becomes available, a repair fact
requests a freshly assessed normal reconciliation; the timer does not adopt or recreate the stack.
Disabled/deselected profiles and user trace overrides cancel or ignore deferred admission.
Interrupted migration retries conservatively; disable/re-enable retains the fence and receiver.
The temporary leaf is not restored after adoption, including when the profile is disabled.

Profile-owned host-free checks run with:

```sh
bash config/profiles/openlit/tests/run.sh
```

Physical acceptance uses one fresh 8 GiB leased Linux VM per container/VM lane, through the documented
[VM workflow](test-vms.md#agent-workflow):

```sh
bash config/profiles/openlit/tests/e2e/acceptance.sh --slot N
bash config/profiles/openlit/tests/e2e/acceptance.sh --slot N --kind vm
bash config/profiles/openlit/tests/e2e/acceptance.sh --slot N --migration
```

After an image-delivery failure, run the smallest owning dependency diagnostic before another
full payload:

```sh
bash config/profiles/openlit/tests/e2e/acceptance.sh --slot N --dependency-only
```

It provisions a disposable yard and requires actual cold delivery of all three pinned images
within the normal 15-minute pull budget, then releases the allocation. HTTP registry metadata
alone does not prove blob delivery; this diagnostic does not satisfy the full acceptance lanes.

To compare registry transport on an owner and its standard nested container within the same
lease, use `--network-only` (container only). It runs bounded DNS, IPv4, direct IPv4 and
shared resolved IPv4 probes with auth/registry controls, then tears down the yard. The probes
preserve TLS Host/SNI and report only public addresses, counts, timing and transport stages.
The network branch has a 180-second bound, changes no network settings, and requests no image
manifests or blobs. A completed diagnostic is not a connectivity or image-delivery pass.

For a same-lease comparison with ordinary cold enable, use `--pull-diagnostic` (container only).
It records Docker API readiness, daemon invocation, environment-name presence and shared-IP
owner/guest controls before and after one ordinary enable. On failure it runs the unchanged
dependency helper once and labels its partial cache reuse. The original enable failure remains
the result even if that fallback succeeds; recovery does not establish why the first pull failed.
Raw pull output and captured journals remain root-private in the disposable guest until cleanup;
the controller retains typed projections, not those raw guest captures. This diagnostic stops
before native events and rendered UI checks and does not satisfy the full acceptance lanes.

The fixture installs the exact packaged candidate through the supported installer and verifies
release migration/publication readiness before creating a yard. It checks default-off initialization,
public profile enable, repeat init/config apply,
readiness with no root-visible native CLIs for a declared configuration-only Codex selection,
then real user-local Codex logs/metrics/traces and UI reads. That initial selection does not prove
standard native package convergence. It also checks
canonical empty trace queries, private first-run account/default DB bootstrap and retention,
rendered login and native signal cards in an isolated headless Chromium profile,
then late user-local Claude appearance and native logs/metrics ingestion,
two live container yards, actual owner RPC and rendered SSH tunnels from an isolated synthetic
client namespace, account/session behavior across browser origins, route drift/refusal/recovery,
guest runtime CAS rejection, actual Collector bind replacement, recipe update/rollback, and
disable with custom settings preserved. Model responses come from local synthetic HTTP providers;
it uses no live model account or external model calls. A passing host-free runner alone does not
establish service startup, native telemetry ingestion or release acceptance.

The container route reservation check reads metadata across all projects, including stopped
yards and their deferred receipts; it does not request live instance state, snapshots or backups.
The lifecycle fixture emits `E2E_PHASE` timings for each yard command, preserving its original
exit code. Compare repeat-init timings separately from cold provisioning and image delivery,
using the same candidate version, VM base and full scenario assertions.

The separate migration lane uses one fresh 8 GiB owner VM and a nested container yard. It installs
the authentic public logs/metrics-only predecessor, provisions genuine Codex through the standard
package path, and creates a declared direct-ClickHouse compatibility receiver. An ordinary offline
update to the frozen candidate must preserve the older backend, admit the new HTTP route, and
let the timer retire only the exact receiver after older sessions leave and new traces arrive.
No manual cutover entrypoint is used. This supports the recognized recipe contract; compatibility
with an operator's existing helper remains unknown until its actual protected layout is verified.

Upstream contracts checked for this profile:

- [OpenLIT 2.1.0 deployment](https://github.com/openlit/openlit/blob/openlit-2.1.0/docker-compose.yml)
  and [integrated receiver configuration](https://github.com/openlit/openlit/blob/openlit-2.1.0/assets/otel-collector-config.yaml).
- [Claude Code native telemetry](https://code.claude.com/docs/en/monitoring-usage).
- [Codex native telemetry configuration](https://learn.chatgpt.com/docs/config-file/config-reference)
  and [native configuration schema](https://github.com/openai/codex/blob/main/codex-rs/core/config.schema.json).
- [OpenTelemetry Collector configuration](https://opentelemetry.io/docs/collector/configuration/).
- [Pinned Collector image user and contents](https://github.com/open-telemetry/opentelemetry-collector-releases/blob/v0.162.0/distributions/otelcol-contrib/Dockerfile).

## Predecessor retirement

AI Observer is retired. Compatible reconciliation removes only proven managed service controls,
its stopped container, and its dashboard route/receipts. Persistent databases, session roots and
credentials remain. Foreign or ambiguous controls are preserved with an ownership diagnostic.
Interrupted cleanup can resume from its protected control-hash receipt. Stopped yards are
reconciled during their next ordinary initialization; they are not started for cleanup.

Old persisted selection and port entries do not prevent configuration loading. The original
files retain their source ownership, unrelated selections and explicit empty lists. This does
not enable OpenLIT automatically. OpenLIT uses native telemetry; AI Observer database history
is retained separately and is not imported into the new dashboard.
