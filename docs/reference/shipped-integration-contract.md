# Shipped integration contract

Shipped coding integrations extend the existing package loader, operation-local
configuration catalog and `IntegrationPlan` / `ApplyIntegrations`. They do not
introduce another selection resolver, planner, registry of operator executable
code or desired-state store. Environment profiles retain their own selection and
`profile.Load` remains profile-only.

An optional `config/agents/<id>/integration.json` uses schema version `1`, or
version `2` for declared artifacts. Version 1 cannot declare artifacts. The
common shipped loader rejects unsupported versions, unknown fields, duplicate
names, unsafe or symbolic-link source paths and setting collisions. Integration
IDs retain the directory name and existing `AGENT_<id>_*` metadata contracts.
Package declarations own immutable typed inputs, scalar settings, existing agent
metadata, lifecycle sources and dashboard metadata. Immutable input values cannot
be replaced by persistent settings, command overrides or ambient environment.
Their bytes and values bind package identity and operation scope.

Schema 2 `artifacts` declare config and instruction delivery. The same resolver
serves CLI config assets, runtime, prepared execution and release activation.
Config sources retain file-setting import/edit and `AGENT_<id>_CONFIG[_DEST]`
overrides; instructions reference an optional integration-owned `absolute-path`
scalar. Existing host-file path settings may serve as compatibility references
without being redeclared. Config-status scope excludes instructions. Raw JSONC
stays raw at its default `.jsonc` destination even when an imported source has a
different filename. The `destination` format policy retains legacy JSON/TOML
ownership when CONFIG_DEST changes. See the
[artifact decision and safety contract](integration-artifacts.md) for schema,
precedence, capture, permissions, preview, adoption and rollback rules. Remaining
recommendation, credential and persistence boundaries are documented in
[consumer compatibility](integration-consumer-compatibility.md).

An optional `native` array reuses the shipped profile package/path declaration
for package-owned Go executables. Development uses `.build/agents/<id>/`;
runtime bundles contain them in their owning integration directory. Actual
executable bytes enter the source binding. Plain source copies need no Git
metadata; missing helpers never cause an implicit build or source-hash fallback.

Settings reuse the shipped profile setting contract. A port may declare
`default_from` with a source port setting and integer offset. References must
exist, have port type and be acyclic. For bounds `minimum..maximum`, the default
is `minimum + (source + offset - minimum) % (maximum - minimum + 1)` with a
nonnegative remainder. Explicit values win. Selected integration listeners
participate in the existing owner collision and stopped-peer reservation rules.

Historical lifecycle metadata remains decodable for configuration compatibility. Current
packages use their ordinary provisioning and check hooks; no integration lifecycle handler
is executed. Profile runtime handlers retain their own observation and diagnostic contracts.

Dashboard metadata supplies scheme, path, configured owner port, guest port and
optional container route receipt/device names. Core renders direct owner routes
and local/remote SSH tunnels from that metadata. Existing route receipt versions,
pending recovery, foreign-device guards and persisted state remain compatible.
Concrete package tests and physical acceptance belong to the integration;
compatibility entrypoints preserve the existing release coverage aggregation.

## Retired package compatibility

Supported installations may retain removed integration IDs, settings, disabled profile
selections and native keys. The data declaration `config/retired-components.json` owns removed IDs, discarded settings,
historical adoption keys and route policies. Core reads these generic contracts; it does not
embed concrete package identities. The configuration catalog retains inert retired input metadata;
`retired_settings` are discarded without evaluating assignments against live values or
rewriting authored files. New selections still refuse retired IDs. Historical disabled
profile selections remain authored but never become effective.

Removed packages have no executable cleanup lifecycle, guest probes, retirement proof
publication, temporary boot requirement or reminder. Guests not initialized since retirement
may retain old files, services and data. Only historical keys already accepted by the baseline
remain eligible for yard adoption; removed profile dashboard keys remain foreign. Receipt-owned
host listeners retain the baseline address deferral and restoration required for safe yard start.
Other recognized historical receipts are inert and grant no publication authority. Confirmations,
foreign-device guards and the release migration epoch floor are unchanged. Existing retirement
proof keys remain accepted as inert historical metadata. Removed profile status reports unavailable
runtime without probing or claiming cleanup; disable can remove its authored selection, including
through release selection repair, without invoking a removed owner.
