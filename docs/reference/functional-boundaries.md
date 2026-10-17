# Functional declaration boundaries

Optional declarations have two trust boundaries. Semantic decoding checks schema,
fields, identities, relative paths and ownership contracts from bytes. Package
adapters then inspect the actual roots, source files and executables. A semantic
result alone never authorizes hook execution.

`profile.Decode` and `DecodeIntegration` produce closed metadata. Their `Data`
snapshots contain semantic values without native roots and clone nested mutable
values. `ValidateMetadata` checks ownership collisions across declarations.
`profile.Load` and `LoadIntegrations` call the same semantic logic, then retain
native definitions with `ValidationInputs`. Executable/source revalidation,
pinned descriptor roots, symlink policies and `SourceBinding` remain physical
adapter responsibilities.

`config.BuildCatalog` builds an operation-local settings catalog from metadata.
`LoadCatalog` binds the same catalog to inspected packages. Settings and declaration accessors
return independent nested values; mutating them cannot change retained catalog policy.
Command manifest lookup/list follows the same ownership contract. Existing
`LoadOptions.Catalog` injection supports policy consumers without loading a
package tree. Catalogs built directly have unbound compatibility declarations;
consumers that execute hooks must obtain definitions from the package adapter.

`config.EvaluateAssignments` evaluates a reader sequentially over a copy of its
base environment. `Catalog.EvaluateLayer` additionally checks scopes, normalizes
legacy names and rejects conflicts before returning committed values and ordered
line provenance. Source labels are diagnostics, not paths to open. File adapters
retain access diagnostics, absent-file behavior and physical layer selection.
Explicit empty values, multiline records, bounded lines and parameter-default
assignment effects use the existing evaluator.

`resource.ParseResource` produces a closed `ResourceSpec` from a reader and
logical profile/resource IDs. `BuildRegistry` checks collisions and indexes its
semantic action, endpoint and lifecycle contracts. `Load` additionally resolves
handlers and returns bound definitions. Direct handler symlinks remain forbidden;
an intermediate symlink within the owning resource profile remains permitted.
A registry built directly has no native handler paths.

Runtime observation decoding and stale-plan comparison consume native facts.
The process adapter still bounds output, captures source bindings and checks
those bindings before comparison. Conditional resolution and allowed convergence
retain their current rules; stale rejection precedes mutation.

Semantic matrices use bytes or typed values. Adapter tests retain permissions,
symlinks, pinned roots, output bounds and pre-mutation rejection. The architecture
contract follows calls from the selected semantic entrypoints within their owning
packages and the domain package; it rejects reachable filesystem/process adapters.
These boundaries share existing packages and do not introduce a virtual filesystem,
manifest schema, global registry or storage format.

Config materialization also separates desired artifacts and observations from
source reads and guest execution. `SelectMaterializationSources` takes an explicit
`config-assets` or `runtime` scope and adapter-supplied instruction declarations.
Config status checks CONFIG/RULES only; runtime additionally selects allowed host
instructions and generated preview content. `PrepareMaterializationContent`
preserves strict missing sources for status, optional missing raw sources for
runtime, and preview fallback for missing instructions. The source adapter still
forbids CONFIG/RULES symlinks and FIFOs while allowing explicit instruction links
to regular files.

`AssessMaterializedArtifact` compares raw guest hash output or decoded structured
facts. `AssessMaterialization` assembles target state and fingerprints;
`AssessMaterializationActivation` composes release targets and integration facts.
These inputs contain logical identities and safely acquired bytes/digests, without
native roots or a CLI dependency. JSON semantic desired digests and TOML exact-byte
digests retain the existing `configmaterial` protocol. Raw and structured guest UID
choices, integration inventory intent/commit and physical atomic writes remain in
the adapters.

Snapshot field order, sorted assets/targets, omission rules and ordinary Go JSON
encoding remain compatibility contracts, including the composed actual target
fingerprint and the converged actual identity that aliases desired. Golden data
tests characterize those bytes; source/security and guest-protocol adapter tests
remain alongside the functional scope, preview and drift matrices.

Settings migration policy consumes logical nested/legacy yard snapshots, inherited
source snapshots and command ownership IDs, plus the adapter's candidate-template
support fact. `planSettingsV2Yard` edits bytes with the existing assignment codec;
`classifySettingsV2Inheritance` preserves the existing sorted ownership digest.
Planned paths are logical labels. The adapter binds them to absolute paths before
journal/recovery work and native compare-and-swap. Diagnostics keep the yard,
relative source, role, key and line without rendering assignment values.

Fresh and prospective inspection, journal observation and source-import waiting
share a layout observation boundary. Prospective views never fall through to a
physical root identity check. Resume and pre-apply checks use fresh native reads,
including inherited ownership, so an earlier prospective overlay cannot mask late
drift. The source-import waiting decision itself uses only existence/layout facts.
Native ownership, symlink, no-create, fsync/rename and CAS checks remain adapter
contracts. Journal, ledger, checkpoint and recovery encodings are unchanged;
logical snapshot fixtures characterize the existing resource digest bytes.

Release activation can implement `V2SourceActivationPlanner` to assess its Desired
state from the prepared source, final settings view and exact owner before-state
before the original consent grant. Owners project their registration and backend
changes through their own contracts. The view grants no authority and does not
replace native Actual or Converged observations. After source verification, the
whole activation Desired vector must match the approved scope; each owner is
checked again before and after reconciliation. Restart reconstructs the same
scope from bound inputs. A completed historical checkpoint grants no new work.

`V2PreparedInputPaths` describes finite captured inputs for discovery and presence.
`V2PreparedInputReader` returns exact bytes only for a matched captured input;
matched absence and errors remain authoritative. Final settings overlays forward
this contract. `LoadOptions.ProspectiveFiles` uses the native prospective reader;
its `ReadPreparedInput` callback resolves captured snapshots only when the loader
requests them, preserving logical provenance and native layer grammar.
Unconsumed archive and QA inputs are not read during activation planning.
Captured imported assets use the ingress reader's existing per-file byte, inode,
ownership and mode guards. Persistent settings retain their own reader limits;
uncaptured assets retain `MaterializedAsset.ReadSource`, including its explicit
instruction symlink policy. Planning reads captured snapshots without temporary
draft copies; native prospective snapshot bounds and identity checks still apply.

For activation and reconciliation operations, the CLI retains the full projected
native integration plan before consent, including instance identity, ownership,
adoption and readiness facts. Native
`CheckIntegrationPlan`, source bindings and mutation guards remain authoritative
under the yard lock and before effects; reconciliation does not obtain fresh
adoption permission. The existing journal persists Desired and reconstructs
native readiness facts after restart. This boundary introduces no stronger
durable readiness guarantee or additional state format.

Bounded local settings source authoring validates prospective configuration and
assets without granting runtime activation authority.
