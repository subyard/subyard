# Testing changes

Test-selection policy lives in [Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).
This guide documents how to run checks and interpret their evidence.

## Reading map by task type

Read only the sections needed for the task. Combine rows when more than one applies.
Test selection still follows the skill's risk-based policy.

| Task | Read |
| --- | --- |
| Documentation, formatting, or mechanical changes with unchanged behavior | No testing-guide or VM-guide sections are required; inspect the diff and use relevant syntax, link, or format validation. |
| Behavior changes | [Keep tests proportional](#keep-tests-proportional), the affected row in [Coverage ownership](#coverage-ownership), and [Run the core checks](#run-the-core-checks). |
| Long checks or observer work | [Execution and reporting principles](#execution-and-reporting-principles), including only your client's [Codex mechanics](#codex-mechanics) or [Claude Code mechanics](#claude-code-mechanics). |
| VM checks | [Running tests](test-vms.md#running-tests), [Test lanes and gates](test-vms.md#test-lanes-and-gates), and [Fixture preflight](test-vms.md#fixture-preflight). |
| Release preparation, acceptance, or publication | [Release publication](reference/release-publication.md), including the [agent release workflow](reference/release-publication.md#agent-release-workflow). |
| Test-VM pool setup, capacity, or client availability | [Pool facts for agents](test-vms.md#pool-facts-for-agents) and the [subyard-test-vms skill](../config/profiles/test-vms-client/skills/subyard-test-vms/SKILL.md). |

## Static checks and dependency audits

`make lint` runs golangci-lint, actionlint (including ShellCheck on workflow `run:` blocks),
offline zizmor over `.github/workflows`, and Ruff's `F` rules over every tracked Python file.
`make verify` and CI's `verify-go` job include this gate. The pinned official Linux amd64/arm64
tools are downloaded with SHA-256 verification and cached in `.build/tools`.
Run `bash dev/lint-workflows.sh` or `bash dev/lint-python.sh` for a focused check;
workflow linting requires ShellCheck on PATH.

## Required CI checks

Core CI runs `verify`, `compatibility` and `native-engine-arm64` on pull request opens, reopens
and head updates. GitHub evaluates the checks for the current PR revision, using the test merge
commit when available. Core CI also runs on pushes to `master` and `release/**` for post-merge
and standalone release validation. A PR sourced from a release branch can receive both events.
Periodic E2E probes on `ci-probe/**` use their own push trigger.

The `verify` check requires success from `verify-go`, `verify-shell` and `verify-profiles`.
Each job builds its own runtime. The Go and shell jobs use `./tests/run.sh --go` or
`./tests/run.sh --shell` to select their checks. The profiles job runs profile permission
checks before its build and runs the full profile suites including the installer prerequisite.
Local and Release verification use the full `make verify` entrypoint.

## Veranda and dependency audits

`make verify-veranda` runs client formatting, Clippy with
`--locked --all-targets -- -D warnings`, and native tests, locally and in Veranda CI.
Install `stable` with Clippy and rustfmt for formatting, Clippy and tests.
Veranda CI runs the native client on Linux (x86-64 and arm64) and Windows on path-filtered pull
requests and pushes to `master` and `release/**`. It also runs on `v*` tag pushes and manual
dispatch. The macOS run (`veranda-macos.yml`) is scheduled daily, runs on release tags and on
manual dispatch, and is not a merge gate, because hosted macOS arm64 capacity is scarce.
The GTK desktop job runs the same format/Clippy checks for `veranda/desktop/Cargo.toml`;
local desktop compilation also requires `libgtk-4-dev`.

Dependency audits use current vulnerability databases separately from `make lint`/`make verify`.
`dependency-audit.yml` runs daily at 04:47 UTC, on manual dispatch, and on pushes changing
Go module files, either Veranda Cargo lockfile, or the audit workflow. Run
`bash dev/audit-go.sh` and `bash dev/audit-rust.sh` locally to audit Go and both Rust lockfiles.
Go audits block on every symbol-reachable ID outside
[`dev/govulncheck-allowlist.json`](../dev/govulncheck-allowlist.json), matching govulncheck's
default symbol-level policy: these are the called findings that make govulncheck's own text
mode exit 3. Module/package-only findings are shown but do not fail the job or require exemptions.
Analyzer, installation, malformed policy/report and invalid review-date
errors also fail. Every allowlist entry needs an `id`, a nonempty `reason` explaining why the
finding is accepted for Subyard's actual use, and a `review_by` date in `YYYY-MM-DD` form.
The date remains valid through that UTC day; every expired entry fails, even if its ID no longer
appears in the scan. Expiry alone turns the scheduled audit red the next UTC day without any code
change, so review and renew entries before their date. All unlisted IDs and independent policy errors are reported in stable order.
Allowlist IDs without a symbol finding in the scan produce warnings in stable order in the output
and step summary; these warnings alone do not fail the job.

Before adding or renewing an entry, run the pinned tool with `-show traces ./...`, inspect the
upstream advisory and the Subyard call sites, and check for a compatible fixed dependency.
The tool is cached as `.build/tools/govulncheck-1.1.4-$(go env GOVERSION)` after an audit run.
Document the affected path and why it is not exercised (or the accepted risk) in the reason;
set review no more than 92 days after the UTC run date; later dates fail the gate.
Remove entries when fixed or no longer needed.
Subyard uses the `github.com/lxc/incus/v7` client at v7.6.0; the former 24 Incus v6
allowlist entries have been removed. Client dependency audits do not cover the separately
installed daemon. The daemon minimum is 6.0.6: it remains supported, is never refused
for its version and is not upgraded automatically. The recommended and tested version
is 6.0.7. Distributions below the minimum install from Zabbly LTS-6.0, which serves the
latest 6.0.x. Disposable VM fixtures require at least 6.0.7; compatibility lanes exercise
the v7 client against that LTS daemon, rather than an Incus 7 daemon.
Run `TMPDIR=/tmp bash tests/lint-tools.sh` for the host-free policy and audit integration tests;
the core contract suite already includes this check.

CI publishes the full Go report and policy result in the job log; its step summary is
capped at 900000 bytes, with a truncation notice pointing to the full job log.
OSV-Scanner findings remain blocking. These checks need network access; tool downloads and
Go build/module caches are reused without adding tool dependencies to `go.mod`.

## Keep tests proportional

Use the skill's [risk-based selection policy](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk)
for both adding tests and choosing which existing checks to run.

A fixture must not repeat a product command until it succeeds. Fix nondeterminism in the
product and assert the first invocation's result. Bounded waits for observable conditions
(such as a file, listener, or reboot completion) remain appropriate; they must not hide a
failed mutation by submitting another command.

Use `testkit.WriteFile` for exact file modes and `testkit.TempDir` for private
fixture roots. Shell/Python fixtures can use `install -m`, `chmod` on fixture inputs,
or `tempfile.TemporaryDirectory`. Keep raw creation calls when creation is the operation
under test; do not chmod its outputs to make an assertion pass.

`internal/cli` tests build their environment with `newFixture` in
[`fixture_test.go`](../internal/cli/fixture_test.go). `withCheckoutConfig()` makes `Root` a
fixture-owned copy of the checkout's tracked `bin/`, `completions/`, `config/` and `scripts/`
files, without links, `.git` or `private/`, and loads the shipped config from it; tests that
read other checkout files for reference use `repositoryRoot` read-only. The homes are fixed when
the fixture is built: `withEnv` and `withoutEnv` refuse the home, config and `PATH` settings,
so a test that needs another home passes `Options.Environment`. `Write` accepts a path below
`Root` or below the fixture's own directories. `withCommands` replaces external tools with
[`testkit.ExecFake`](../internal/testkit/execfake.go) shims that use only shell builtins: assert
their argv with `AssertCalls`; an unexpected call exits 127 and fails the test at cleanup. Create
the fixture before anything whose cleanup can still call a fake tool, because cleanups run in
reverse order.

A slice that moves tests onto the fixture keeps every assertion statement byte-identical: assign
the old local names from the fixture (`root, environment := fixture.Root, fixture.Environment`)
instead of renaming them. The only assertion changes allowed are removing the `New` error check
and the error checks of setup the fixture now does, and turning a stub-log compare into
`AssertCalls`; each such conversion gets a mutant row, which may mutate the expected argv in the
test file. Run `dev/cli-fixture-slice-check.sh --base REF`: it requires every `go test` run to
exit 0, equal test lists and pass sets, no lost coverage blocks, and every mutant in
`dev/cli-fixture-mutants.tsv` for a changed test file to build and fail its tests.

The normal host-free entrypoints (`make test`, `make verify`, `./tests/run.sh`,
`bash dev/test-profiles.sh` and each profile's `tests/run.sh`) use baseline umask `0022`,
including their Shell/Python checks. This changes only the runner's process and children.
Live `dev/test-profiles.sh --e2e` keeps the caller's environment and mask.

Short permission checks run before builds and Go suites. Core owns
[`tests/suites/preflight.list`](../tests/suites/preflight.list); profiles can own a
`tests/permissions.sh` hook, discovered by the existing profile runner. The hook's checks
must be omitted from the later normal profile loop. `make test` and `make verify` first
run `bash dev/test-profiles.sh --permissions-only`, then core checks, then
`bash dev/test-profiles.sh --after-permissions`. These two modes split a single invocation's
work; use `--after-permissions` only after that same unchanged checkout's preflight passed.
Ordinary profile discovery runs both phases itself; a direct profile runner runs its own
preflight. There is no persistent PASS marker.

[`permission-umasks.sh`](../tests/helpers/permission-umasks.sh) runs only the supplied short
Shell/Python checks under `0022`, `0002` and `0077` in separate processes. It prints each
actual mask and selection. It rejects empty/missing selections; Python unittest modules
must contain cases and finish without failures or skips. A renamed/deleted module or an
empty module fails before expensive checks. Run one owning regression directly, for example:

```sh
bash config/profiles/telemetry/tests/permissions.sh
bash tests/agent-skills.sh
```

| Contract | Existing owner and selected coverage | Remaining baseline coverage |
| --- | --- | --- |
| Private root and durable private files, exact `0700`/`0600`, idempotent replacement | Telemetry `records-test.py` (6 cases), `resources-test.py` (24 cases): real record emission and missing-root `request → held`, durable intent retry | Launcher admission and guest initialization remain in the normal Telemetry suite |
| Public `0644`/`0755`, private receipt, replacement/repair and unsafe existing objects retained | Shared synthetic `config/skills/tests/delivery-test.py` (17 cases) and `guest-facts-test.py` (9 cases), checked under all three masks | Full packaging and installer contracts remain in core |
| Unsafe mode, owner, symlink and hardlink refusal | Existing owning Go permission selections and profile suites; selected shared delivery and Telemetry cases retain their refusals | Explicitly prepared unsafe fixtures retain full-suite assertions; a matrix is not a replacement for these |

The two Telemetry modules are a small selection from its full suite (30 cases per mask);
keeping these modules together preserves their existing fixtures without another case registry.
The shared skill delivery and guest-facts modules use three-mask coverage. Neither selection repeats
later in the same normal run. Together these checks normally take seconds, without installed
services, credentials or VM allocation.

Telemetry's `records.private_directory` guarantees and validates only its leaf. Its parents
must already satisfy their owning contract; recursive `mkdir` does not guarantee private
intermediate directories and never repairs unsafe existing parents. The real correlation
regression starts with an absent data root: `request` initializes it before `held` creates
nested sources/intents. Isolated held fixtures must explicitly provide the private data root.

The full core Go race suite runs uncached once under `0022`. Its explicit file-creation
selection in [`tests/run.sh`](../tests/run.sh) repeats only under `0002` and `0077`, preserving
private persistence and public output checks. The existing
[`go-permission-umasks.sh`](../tests/helpers/go-permission-umasks.sh) validates every selected
test name; update that selection when renaming a case or adding a distinct creation boundary.
Profile settings/descriptor Go checks run once under `0022` through `profile-go.sh`.
The GitHub profile repeats only its explicit broker protected-file checks under the extra masks.
Baseline runners do not wrap the selected-matrix children, so they cannot overwrite those masks.

## Coverage ownership

The GitHub owning release-activation fixture checks an authentic published predecessor upgrade,
delivery/restart interruption and fresh-consent retry on a synthetic successor, loaded broker/client identities, core helper
delivery, differing local profile selections, stopped-yard deferred convergence and retained data.
It also checks supported rollback between two compatible packaged fixture versions. The second
version contains explicit broker/skill/preview fixture deltas; it is synthetic compatibility evidence. Run its local `--preflight` before allocating a VM,
then provide the frozen public candidate bundle and SHA-256 to
`config/profiles/github/tests/e2e/release-activation.sh --slot N` as described in the VM guide.
The candidate identity comes from its verified frozen metadata; the predecessor uses the
`0.28.0` direct TSV pin, with GitHub selected in the live yard and off in the named yard.
This focused lane does not replace release publication or external GitHub-account acceptance.

Logical matrices belong to their unit or local integration owner. Physical E2E keeps
installed-runtime, real Incus/systemd, routing, reboot and retained-data evidence.
Choose the owning packages, scripts and lanes below by the changed contract, rather
than repeating their logical matrices in P0.

Published-release tests start at **v0.28.0**, the minimum supported upgrade test
point. No host-free compatibility job, Periodic lane or release acceptance fixture
installs an older release. Product migration and recovery support is unchanged;
synthetic state and frozen format/ownership contracts still cover its readers.

The published-baseline matrix defaults to no coding integrations. Its optional agent selection
(`published-upgrade.sh VERSION all`, or a quoted selection) exercises the published release's real
init before update, including structured configuration evidence and raw Codex rules when selected.
The v0.28.0 anchor selects Claude, OpenCode and pi because its Codex provisioner can hit HTTP 403
on GitHub's anonymous release metadata endpoint. The v0.30.0 and v0.31.0 lanes include Codex
and its provisioning fix ([#62](https://github.com/Subyard/Subyard/pull/62)). Run selected agent
jobs at the ownership, manifest and provisioning boundaries described in the VM guide.

Bootstrap checks runtime ownership before interpreting checkout markers. Another runtime gets
`--runtime-root` guidance; only a dispatcher or valid pending recovery is classified as legacy.
A native-only checkout gets neutral guidance. The diagnostic environment value
`SUBYARD_SOURCE_INGRESS_V1_CLASS=legacy` accompanies its descriptor; it grants no authority.

| Contract | Owning unit checks | Local integration | Physical gate | Trigger |
| --- | --- | --- | --- | --- |
| Read-only installation check | `go:application`: `TestConformance*` golden JSON/text and contract errors; owning release, host, credential, config, project-store and journal inspection checks; `go:adapters/hostruntime`: `TestHostCheckPreflightGoldenAndInspection` pins `check --host` output | `go:cli`: `TestInstallationCheck*` with non-writable homes and every independent finding, admission and usage cases | `p0:peer`: `yard check --json` is `ok` on the clean release install | `yard check`, its owners or `check --host` changes |
| Incus AppArmor capability and nested Docker compatibility | `go:adapters/reconcileruntime`: `TestAppArmor*` | `tests/create-subyard-docker-apparmor.sh`: shell probe failure, exact mask and restored-state convergence | Periodic-only `vm:incus-apparmor-probe`: installed candidate, real Incus/systemd transitions, failed probes preserve Docker and runtime, override restoration and teardown | AppArmor/nested Docker handling in `scripts/01-install-incus.sh`, `scripts/03-create-subyard.sh`, or AppArmor-related reconciliation in `internal/adapters/reconcileruntime/{apparmor.go,runtime.go}` |
| Cache cleanup and project-preserving recreation | `go:cli`: soft and hard cleanup contracts | Cleanup assessment, registry round-trip, and interruption checkpoint tests | Optional Periodic-only `periodic:cleanup-acceptance`: named and default containers, subyard-dev cache removal and Go rebuild, interrupted hard cleanup and recreation preserving projects, registrations, and power intent | Cleanup policies, recovery, or cache declarations |
| Operator terminal ownership of sudo | `go:cli`: `TestRootStepPrivilegesAuthorizeBeforeBoundedAdapter` | `tests/runtime-privilege-reexec.sh`: dispatcher and elevated context | Optional Periodic-only `periodic:sudo-terminal-ownership`: real sudo password retry and caching, no-terminal refusal, cancellation, and Incus lifecycle on a disposable VM | Root authorization, PTY forwarding, or privileged lifecycle changes |
| Agent Git and sudo approval prompts | `config/agents/shared/tests/git-approval-hook-test.py`, run by `config/agents/claude/tests/provision.sh` against the installed hook | `config/agents/claude/tests/provision.sh` and `tests/codex-agent-provision.sh`: managed policy, hook registration and drift | Optional Periodic-only `profile:orca/codex-permissions`: native Claude and Codex prompts, plus a probe of Codex with the hook stubbed that records which shell forms Codex runs without asking; `profile:orca/managed-agents`: prompts in Orca terminal and chat | Hook or policy changes, and every Claude or Codex version bump: rerun `profile:orca/codex-permissions` and its probe, and update the hook's Codex word rule when the probe result changes |
| Zabbly signing-key download and repository convergence | — | `tests/zabbly-download.sh`: bounded download, atomic output, and repository caller contracts | Optional Periodic-only `periodic:zabbly-download-smoke`: both callers fetch the real HTTPS key and verify ownership, mode, and repeat convergence | Zabbly repository setup or the shared download helper |
| Retired source ingress refuses without mutation | `go:cli`: `TestReleaseTransitionIngressFactoryRefusesSourceByClass` | `go:cli`: `TestRetiredSourceHelpersDoNotWritePathsDuringRecovery` preserves filesystem/journal/entrypoints; `shell:engine-release`: pre-Go bootstrap refuses before publication; `go:migration`: `TestBootstrapRediscoversInterruptedSourceIngressFromRecovery` preserves recovery; `TestBootstrapSourceClassificationMatrix` enumerates marker, runtime, journal and path combinations before writes | None; retained source ingress has host-free bootstrap coverage only | Source ingress or bootstrap changes |
| Ordinary commands ignore retired V1 files | `go:cli`: `TestMutationGateIgnoresV1Files`, repair tests and V2 gate/RPC regressions; `go:migration`: source-history binding ignores V1 state and unsafe paths; `TestSourceHistoryBindsCommittedV1FilesAsAbsent` pins the candidate binding despite committed V1 history; `TestV2CompatibilityIngressSkipsLegacyLeafWithoutJournaledLegacyStep` preserves source-only inspection; `go:adapters/releaseruntime`: source policy ignores V1 files | `go:cli`: `TestFullUninstallDoesNotRequireMigration`; historical creator contracts remain migration-independent; `TestV2CompatibilityIngressRefusesUnfinishedImportWithoutWrites` refuses unfinished imports and replacement without writes, preserves completed V2 provenance; `TestMutationGateRefusesUnfinishedV1ImportBeforeRetainedOwner` prevents sealed-owner execution, including the progress version probe after activation, and replacement publication; V2 evidence/checkpoint tests stay | Required `p0:full-uninstall`, published anchor upgrade/rollback and `p0:newer-release` | V1 gate removal; supported installations are assumed to have no V1 journals, stray files are not read by ordinary admission |
| Source authorization, journal binding and recovery | `go:releasetransition`: journal codecs and in-memory source inspection normalization | `go:releasetransition`: source order/drift/recovery over filesystem fixtures; `go:cli`: candidate process/source ingress checks; `shell:engine-release`: installer/update/rollback | Published anchor update (`vm:published-upgrade/anchor`) and `p0:newer-release`: supported caller, retained data and unfinished-journal replacement | Transition/installer changes |
| Config precedence, safe diagnostics and drift | `go:config`: `TestResolveIntegrationSelection` | `go:config`: precedence over persisted layers; `go:cli`: config paths/show/status/apply tests use persisted config fixtures and native materialization; `shell:engine-release`: installed process checks | `p0:init-settings`: config apply/status plus actual guest file hash | Config resolver/materializer changes |
| Installed configuration sync over SSH/Git | `go:configsync`: source validation, convergence and recovery | Configuration-source Go tests and `tests/cli-contract.sh`: shared onboarding, confirmation and retained HostID | Periodic E2E `periodic:config-sync`, via `dev/e2e/config-sync-acceptance.sh --slot N`: installed pair, two-way convergence and dirty/diverged/conflicted refusal without live settings or remote ref mutation; outside release acceptance | Required for `internal/configsync`, configuration-source SSH/Git transport, or `config sync` / `config set --git` changes |
| Retained schema-1 Git settings on a supported release | `go:configsync`: `TestConfigSyncMigratesLegacySettingsWithoutLosingLocalChanges` | The same file-backed migration fixture checks unchanged, edited, mode-changed and missing inputs | `published:0.28.1`: synthetic retained settings survive candidate update; explicit `config sync` separates Git storage while yard identity, power and data stay intact | Legacy Git settings readers or the published-hop fixture |
| Status space cache freshness and selectors | `go:adapters/statusruntime`: `TestDescribeSpaceCache` uses fixed time and an injected refresh request | `go:adapters/statusruntime`: cache persistence and synchronous/asynchronous refresh contracts; `go:cli`: `TestStatusRoutesSummaryAndExplicitYardSelectors` and `TestSpace*` | `dev/e2e/status-fast-acceptance.sh`: real container refresh, separate `/srv` mount and verified same-inode storage allocation; `p0:real-incus` retains VM adapter compatibility | Status cache or space measurement changes |
| Generic resource consent, exact plans and retained data | `go:resource`: `TestStructuredPrepareContract`; `go:cli`: stream prompt logic | `go:resource`: registry/assessment over descriptor files; `go:cli`: resource command/prepared tests execute handlers; `TestResourceExactTerminalConsentAndRetainedData` adds real files and PTY; shipped profile host-free checks own handler semantics | `p0:profile-resource`: bound-resource profile; shipped profile E2E gates own external services | Resource contract or profile changes (`profile-generic` and owning profile sets) |
| Herdr runtime, spaces and client SSH | `config/profiles/herdr/tests/run.sh` checks release installation with command fakes, Unix RPC, document conflicts, layout retention and client key handling | The owning profile checks resource dispatch, auth-policy refusals and VM-yard refusal | Agents run `profile:herdr/lifecycle` on a leased VM. One installed container yard exercises systemd, linger, SSH proxy and client key lifecycle, reconnect over raw RPC with endpoint discovery from pairing, project spaces, handover from an older Core release and runtime refresh after `down`. Per-client focus is desktop-local and not observable over raw RPC, so it remains unproven. | Herdr pins, handler, document format or SSH ingress behavior |
| RPC framing, skew, cancellation, EOF and ordered streams | `go:rpc`: in-memory framing/stream checks in `session_test.go` | `go:cli`: typed owner queries over config fixtures; `shell:engine-release`: packaged version negotiation; `e2e:ssh-rpc`: real loopback SSH and selected-yard native settings | `p0:peer`: deployed selected-yard project query and reconnect snapshot; `p0:transport`: real disconnect | RPC/SSH changes |
| Exact owner plan binding, expiry and replay | `go:cli`: `TestExactOperationPlanExpiry` and `TestExactOperationPlanRejectsChangedBinding` use fixed time and in-memory plans | `go:cli`: `TestExactRPCPlanBindingAndSingleUse` retains handler refusal, no mutation and replay; `TestExactRPCRequiresCapabilityAndDiscardsOnDisconnect` uses `net.Pipe` | `dev/e2e/integration-remote.sh`: successful negotiated plan/execute over real SSH, remote enable/disable/status and retained data | Exact plan or owner session changes |
| Init-owned settings and `DEV_USER` rename | `go:adapters/reconcileruntime`: applied settings, developer identity plan and recovery; `go:cli`: admission, prepared init and stopped inspection; owner `identity-test.py`, `tests/developer-identity.sh` | Owner runners of `github`, `git-fetch`, `orca`, `telemetry` | `p0:init-settings`; Orca `identity-rename` lane; the GitHub acceptance controller's `--identity-rename` phase | Init-owned settings, release readiness or account transition changes |
| Project copies, owner inventory and offline recovery | `go:application`, `go:ownerinventory`: decisions with in-memory ports | `go:state`: file-backed persistence/recovery; `go:cli`: `TestOwnerProjectStateConvergesWithoutLosingLocalSource`, `TestReadOnlyProjectListHonorsFreshCacheAndLiveForce`, copy/admission/consent matrices; `tests/project-registry-convergence.sh` and `tests/remote-projects.sh`: native RPC/transfer and removal boundaries | `p0:peer`: real concurrent copies, remote shells, offline stale inventory and refresh/deletion | Project admission/routing changes |
| Owner inventory read freshness | `go:ownerinventory`: `TestSnapshotFresh` uses fixed time for default/custom TTL boundaries and clock skew | `go:ownerinventory`: service cache/fetch contracts; `go:cli`: trusted owner rename and stale inventory checks | `dev/e2e/entity-naming-lifecycle.sh`: real SSH rename, key rotation, offline refresh and removal; fixture timestamps expire caches without wall-clock sleeps | Owner inventory cache or refresh changes |
| Host registration consent and trust | `go:cli`: `TestSSHTrustUsesTypedConsentAndDoesNotConsumeCommandPayload` covers interactive and automated consent | `go:cli`: `TestHostAddPinsFingerprintAndRegistersInitialSnapshotTogether` checks one confirmation and atomic trust/inventory registration | `dev/e2e/entity-naming-lifecycle.sh`: direct, alias and proxy SSH registration uses explicit `--yes` | Host registration or SSH trust changes |
| Crypto and SSH adapter compatibility | Owning crypto, trust and transport packages | `e2e:adapter-contracts`, `e2e:credential-tools`, `e2e:ssh-credential-peer`: pinned real tools and loopback SSH | `e2e:ssh-unknown-host`, `p0:peer`: separate host identities, routing and failure/reconnect | Adapter/tool/trust changes |
| GitHub broker status, loaded identity and issuer | Owning `config/profiles/github/broker` tests; required `TestConnectionFixtureIssuerLocal` and runtime skew/refusal cases | `TestClientHandlerAndFakeGitHubContractOverUnixSocket`: production handler/client plus synthetic upstream; `config/profiles/github/tests/run.sh --preflight` runs required cases once and checks JSON pass/skip counts | Owning GitHub acceptance: installed broker and client identities | Broker protocol/consumer changes; local Go/Python prerequisites, T0/T1 |
| Shared RPC implementations | Go shared frame contracts and Rust golden/fault tests | Linux Veranda native Rust client builds the current Go engine, negotiates and queries it, then refuses version skew; existing Python subprocess owner retains the fault matrix | Existing Veranda owner/SSH lanes | RPC schema/client/server changes; stable Rust, Python and Go, T1; cold build separate |
| Installed credential timer ownership and intent | Owning credential/release adapter checks, including verified ancestor replacement | `tests/installed-runtime-refresh.sh`: production validator on exact-mode `/tmp` files; receipt/no-receipt, altered/foreign/symlink/mode refusal with no writes or manager calls; stopped/disabled intent | `config/profiles/github/tests/e2e/acceptance.sh --installed-worker --slot N`: real user manager, installed runtime admission, due worker and SSH/SOPS peer result | Keys unit/sandbox, installer/refresh or shared admission changes; local checks T0/T1, actual worker T3 |
| Periodic hosted VM image selection | `tests/workflow-real-adapter-gate.sh` checks the dispatch choices, all planned commands, pool boundary and Intel retry | Owning client and broker tests check lease request negotiation, grant validation and Incus alias selection | Periodic E2E on the selected OS, including both optional `periodic:nm-guard/fresh` and `periodic:nm-guard/upgrade` lanes | Changes to Periodic image routing, supported lease images or the NM guard fixture |
| Test-VM client handles and skill delivery | `config/profiles/test-vms-client/tests/run.sh`: host-access refusal and selection, skill/client delivery with Hermes links and deselection, session handles against a fake facade | `tests/agent-e2e.sh`: the runner built on the same client | `config/profiles/test-vms-client/tests/e2e/acceptance.sh --slot N`: installed candidate provisions the profile into a Hermes-preset yard, then deselection and init retire it; installed copy outside a checkout acquires, runs root commands in two calls of one lease, releases; runner `--verify-boundary` | Shared client, its profile hook or skill, or facade lease protocol changes |
| Broker slot quarantine, recovery and cross-slot isolation | `go:adapters/testvmsruntime` covers exact-slot identity, quarantine, automatic recovery, public status, release, pool resize and drain before broker recreation through `broker_pool_test.go` and the lease store and facade tests | `tests/test-vms.sh` checks the firewall rule. `config/profiles/test-vms-client/tests/client-handles.sh` checks busy wait and banner attribution | `vm:staging-reliability` checks installed quarantine, reaper recovery and lease context after guest reboot. Test-vms `memory-boundary` checks namespace TCP forwarding, held renewal, requester-death expiry and clean reuse | Broker release, recovery, slot identity, data access, firewall or lease-context changes |
| Profile runtime format and rollback hooks | `go:profile`: runtime `format` and `prepares_rollback_to` metadata; `go:adapters/reconcileruntime`: `rollback-observe` and `rollback-apply` transport | `go:adapters/releaseruntime`: `TestRollback*` and `TestRollbackPreparation*` with synthetic owners: two commands, every refusal, rechecks, resume and owner next steps; `config/profiles/telemetry/tests/rollback-test.py`: conversion, interruption after each effect, and acceptance by the v0.30.0 observer and apply | `vm:published-upgrade/0.30.0`: rollback, Ready, unchanged history and the update after it; `profile:telemetry/published-rollback`: the same lane with telemetry refused while enabled and prepared by the first command | Core rollback preparation, a declared runtime format, or a profile `rollback-observe`/`rollback-apply` hook |
| Broker bundled base recipe and supported upgrade | `config/profiles/test-vms`: `TestBaseRecipeInstallerContract`, both real archived environment recipes and installer with minimal context | Owning `tests/run.sh --preflight`: installer identity regression → bundled recipe contract; nonzero required cases and zero skips, validated collector PASS reuse. The full profile run repeats the installer check except under `make verify`, whose passed core suite sets `SUBYARD_CORE_INSTALLER_CHECKED=1` | `config/profiles/test-vms/tests/e2e/acceptance.sh --slot N --lane broker-cold-upgrade`: pinned actual 0.28.0 updater → frozen candidate → cold acquire, new base/boot/SSH/release/cleanup | Recipe or archive producer, installer/called libraries, broker activation or release migrations; T0/T1 before T3. Synthetic rollback and full-uninstall remain separate required coverage |
| Frozen historical producer ownership | `TestHistoricalUninstallSourceIgnoresMigrationReadiness`: frozen public bytes and independent production hash oracle; unknown/tampered/unsealed/rehashed negatives | Existing release/uninstall filesystem contracts preserve refusal and retained journals | Host-free historical producer contracts; current full-uninstall and v0.30.0 checkpoint-resume lane for native deletion | Historical recognition/migration changes; owning frozen fixtures with provenance, T0/T1 |
| Static skill inventory, transport and readiness | `go:adapters/reconcileruntime`: `TestAgentSkillSourcesParityWithCapturedReadiness`, unsafe-source refusal and captured-source guest readiness; `tests/agent-skills.sh`: delivery under three umasks | `tests/release-candidate.sh`: actual wrapper against a strict nonexecuting shim, independent synthetic inventory, all checkout/private/frozen layouts and umasks, exact member/command mutants; `go:internal/testkit/cmd/empty-incus`: `TestHistoryReadinessContract` connects public runtime readiness through the native Incus adapter to the migration-history responder | `config/profiles/android/tests/e2e/acceptance.sh --slot N --lane skills`: packaged delivery, repeat refresh, discovery repair and disable with unrelated content retained | Shared skill producer, helpers, source admission or transport changes; local contracts before T3 |
| Published evidence and resumable staging | `go:releasetransition`: existing pending/store recovery and unsafe-link/refusal cases; independent Python oracle negatives | `TestEvidenceOracleContract`: actual POSIX publication cuts, Python-owned snapshot/handshake, public completion and no-op retry; published mode/inode/mtime/digest remain fixed while allowed staging retains its inode/mode/prefix | Go history/checkpoint reader contracts and the published hop row (`published:0.28.1`) | Evidence writer, recovery or compatibility-oracle changes; prefix retention does not prove complete-record validity |
| Android toolchain selection and provision boundaries | Owning `toolchain-test.py`: explicit environment/architecture selection, pinned archives and retained-byte validation | Owning `tests/run.sh` executes the actual provision shell/Python CLI with ordinary and polluted caller environments/TMPDIR; shared fixture selection, native HTTP reset/resume and stall-retry tests and callable legacy-helper refresh preserve unrelated files | Existing owning Android acceptance lanes | Android selection, download, installation or helper-refresh changes; production architecture admission remains native x86_64-only |
| Android Gradle builds, ADB TCP relay and SDK additions | Owning `toolchain-test.py`: default package list, minor API levels, all manifest refusals, `package.xml` from the vendored repository entry for each pinned or retired archive, offline and marker-owned license staging | Owning `tests/run.sh`: `android-pool-test.py` runs the real client/pool over sockets for the uid-checked loopback relay, `host:version` preflight, `run`'s Gradle daemon guard, release closure and add-only `sdk install`/`sdk list` with aggregated refusals, partial-publication reporting, lock and license behavior; provision check covers license, metadata and default-package drift | `config/profiles/android/tests/e2e/acceptance.sh --slot N --lane gradle`: AGP 9 builds on defaults (37 minor 2, 35), broker-added `platforms;android-36.1`, `connectedDebugAndroidTest` through `run`, foreign-account relay refusal, repeat provisioning keeps the package | Client relay, SDK package list, license or broker install changes; host-free checks before the lane |
| Fixture preparation and retry | Core `tests/release-candidate.sh` owns archive/refusal/retry; GitHub `release-activation-test.py` owns metadata/version/architecture/checksum shape, synthetic successor, marker/init retry and partial cleanup | Existing `--preflight` includes the production Incus installer's package-only base-image call without installation identity; normal install still refuses missing/empty identity before writes. Release activation also executes its local probe and verifies frozen bundle checksum, engine identity and successor before allocation | Existing owning activation lane | Fixture/helper/artifact changes; Go/Python/jq/ShellCheck and exact artifact inputs, T0/T1 before T3 |
| Profile fixture lifecycle and private diagnostics | GitHub `release-activation-test.py` sources release/broker/connection operations and imports connection diagnostics; Hermes `e2e-capture-test.py` sources its operations and the existing SSH transport helper | Supported scoped disable retains connection bytes; failure/timeout/signal and cleanup exits stay distinct, including original exit 3. Hermes checks bounded runtime-only readiness, ordered scoped setters, capture-before-cleanup, private modes and native transport deadlines | GitHub broker acceptance, GitHub `--installed-worker`, and Hermes acceptance own their respective operations | Lifecycle/cleanup behavior changes require the owning lane; connection installer/refresh boundaries require installed-worker |
| Controller outcome and allocation ordering | `tests/agent-e2e.sh`, `tests/release-acceptance.sh` | Actual controller failure injection: missing/skipped/empty required cases stop before broker; native failure/signal/timeout and cleanup remain separate | Affected existing controller lane, preserving lease ownership and cleanup | Controller, phase, transport or cleanup changes; T1 before T3 |
| Fresh default coding integrations | `go:config`: `TestResolveIntegrationSelection` | `tests/agent-e2e.sh`: isolated selection fixture cleanup; `tests/release-acceptance.sh` and `tests/workflow-real-adapter-gate.sh`: release/periodic registry contracts | `dev/e2e/integration-selection.sh default`: all four default integrations, repeated init and native teardown, registered as `integration-selection/default` for release acceptance and Periodic E2E | Default selection or its fixture/cleanup changes; normal package download access |
| Newer release replacing an unfinished journal | `go:adapters/releaseruntime`: `TestNewerReleaseReplacementSelectsOnlyExplicitNewerVersion`, `TestPrepareTransitionRoutesNewerTargetToReplacement` | `go:releasetransition`: `TestNewerReleaseReplacesActivatedJournalAndKeepsItsBytes`, `TestNewerReleaseReplacesJournalBeforeActivation`, `TestNewerReleaseReportsEveryRefusal` | `dev/e2e/release-transition-newer-release-replacement.sh` in the P0 `newer-release` lane, also part of smoke and `full`: published v0.31.0 direct pin with the staged candidate's migration registry, named migration refusals without changes when the candidate carries migrations, withheld restart, replacement before and after activation, chained replacement, active launcher route | Journal replacement, its evidence or release selection |
| Guest temporary storage | `go:adapters/reconcileruntime`: `TestDiskTmpGuestPolicy`, `TestDiskTmpPolicyWaitsForGuestBoot`, `TestDiskTmpReadinessDistinguishesReboot` | `tests/create-subyard-docker-apparmor.sh`: ordinary creation, post-restart boot completion before VM network inspection, remote fixture image inputs in an isolated project, configured cloud-image user provisioning and existing-yard preservation; `tests/release-candidate.sh`: fresh native bootstrap context, failed inspection refusal and container/VM restart identities | `dev/e2e/disk-tmp-acceptance.sh`: installed minimal container/VM creation, safe active-tmpfs transition, native reboot and policy rollback | Common guest storage policy/readiness changes |
| Disk-/tmp migration restarts | `go:migration`: safety before stop, approved session-interrupting stop, stopped-yard no-op, marker completion and identity/cancellation guards; `go:cli`: pre-execution decline, marker-derived readiness and retry; `go:releasetransition`: interrupted per-yard rows | `tests/agent-e2e.sh`: single-history dispatch and owned cleanup; `go:adapters/incusclient`: sealed-engine CPU helper | `dev/e2e/p0-acceptance.sh --slot N --lane yard-maintenance`: published 0.28.0 → 0.28.1 hop, then explicitly synthetic pending-marker/foreign-policy drift on both running VM yards and the frozen candidate's zero-step update and its current-release retry: upfront disclosure and decline, approved live SSH session interruption and the VM start policy grant, isolated guest preflight failure, ready warnings, UUID/data preservation, read-only check and marker-only retry | Disk-/tmp migration, CPU helper, selected guest init admission or maintenance lane changes |
| Same-host network policy: links, isolation, spoofing and reboot | `go:yardnetwork`: planner, policy, ingress and service contracts including clean bounded stops | `tests/init-network-convergence.sh`, `tests/lib-power-network.sh`: shell network convergence and boot ordering | `dev/e2e/yard-network-policy.sh --slot N`: installed frozen candidate, three real yards, saved and isolated links, spoofing refusal, host reboot with resumed policy, isolated and final teardown | Network policy, ACL, NIC binding or network power changes |
| Host NetworkManager guard and admin uplink | `go:adapters/hostruntime`: the shared `tests/fixtures/nm-guard-vectors` rule, `nm-guard-formats`, `nm-guard-static` and `nm-guard-state` fixtures, the unread refusal, missing, foreign and unreadable records, the file identity binding, a linked guard read as `unbound`, the `unbound` init-then-reboot remedy, bridge state and route errors reported together, reboot reminder; `go:adapters/reconcileruntime`: the separate `network-guard` stage; `go:releasetransition`: the `nm-device-guard-v1` rollback boundary | `tests/lib-power-network.sh`: the same fixtures and `tests/fixtures/nm-list-selects.txt`, a failure on any NetworkManager reload or restart, the unread first write and stop with a missing or foreign record, the reboot that loads it, bridge names refreshing only the identity, hand restores and retargeted links read as `unbound`, a crash between publish and record, the early stop after an unread or truncated-guard migration and a failed record write, `managed=1` and edited-out repairs recorded unread, NetworkManager starting during the write, failed record and boot ID reads, failed link reads that void migration evidence, failed reads reported with independent refusals, Incus-only bridge names and pins, failed inventories, refusals together without changes, failed nmcli reads, inactive NetworkManager, teardown, provable and undecidable migration lists; `tests/install-incus-data-home.sh`: no reload or restart; `tests/init-network-convergence.sh`: planning drift | `dev/e2e/nm-admin-uplink.sh --slot N --lane upgrade|fresh`: taken uplink, refused then reload-free update with a `docker*` notice, refused rollback, init and start before the reboot, boot restore; first init stop, `eth9` veth, tap, guest restart, second bridge, boot restore, teardown | NetworkManager guard, its validators or route-device checks |
| Evidence reuse and gate completeness | `tests/release-acceptance.sh`: command/source/assets/helper/environment binding, tamper, incomplete and inherited-chain refusal | Existing `import`/`transfer`, validated by `load_bound`; execution prints direct/inherited reuse reason | Required fresh/continuous/full gates remain separate | Evidence/runner/dependency changes; T1; an equal executable alone is insufficient |

Go packages can contain both unit and local integration checks; the fixture boundary
determines their evidence, rather than the test language or package name.

Local adapter fixtures are T1 checks with local entrypoints; they require their
documented tools and never acquire VM leases.
A local native fixture does not establish systemd/cgroup, physical disk or Incus behavior.
Frozen release smoke and peer fixtures already reuse collector candidate assets when present;
source A/B, tagged owner histories and pinned predecessors retain distinct artifact identities.
Full/release-smoke, reboot observations, nested teardown and shipped-profile VM acceptance
remain separate physical checks, requested explicitly for release collection; local passes do not
claim that physical coverage.

T0 is the smallest named regression (aim for 60 seconds); T1 is the affected owning local
set (aim for about three minutes). T2 is the complete host-free suite, T3 the affected physical
lane, and T4 full P0. These are selection levels, not a sequence required for every edit.
Measure only necessary checks; report cold toolchain/download/build prerequisites separately.
Use existing Go `-run`, Cargo test filters, Python owning tests and profile `--preflight` modes.
Check the native summary has a nonzero case count and the intended case actually passed.
Unexpected required skips, missing cases and empty selections are incomplete, even with exit zero.

Changes to `config/systemd/subyard-keys-sync.*`, `scripts/install-keys-auto-sync.sh`, their
installer/refresh callers, `_keys-auto-sync`, credential consumer admission, or the shared
`internal/cli` and `internal/adapters/releaseruntime` admission validators **require** the owning
installed-worker mode above. Include transitive validator changes when selecting it. That mode
prints its reason and runs local prerequisites before allocating its disposable pair. It observes
the real worker's SSH/SOPS result. For a different unit, select its existing owning systemd lane
(for example `power-systemd`); the GitHub worker does not establish every core unit contract.
Selection is explicit by changed contract; automatic changed-file inference is not supported.
The ordinary profile/release inventory and mandatory smoke/full gates retain their existing meaning.

## Execution and reporting principles

Follow [test execution and delegation](../.agents/skills/subyard-dev-flow/SKILL.md#test-execution-and-delegation)
for roles and model settings. Preserve required coverage and candidate identity.

- One assigned polling owner reads active progress/logs; the developer and coordinator use
  its events and evidence instead of parallel polling. Interactive root-console work also has
  one owner. Process control, lease heartbeats and cleanup remain with the existing runner.
  While an observer owns the run, other roles do independent work or use the supported agent
  event wait within client limits. Do not add process/status/log polls or request updates merely
  because a wait returned unchanged. Check ownership once when its agreed checkpoint expires.
- Prefer completion events. For capacity shortages, use long adaptive waits and retry guarded
  acquisition when availability may reasonably have changed. Record the next retry time or
  resource-change trigger in the existing task. Before that time or trigger, an unchanged timed
  wakeup does not justify another capacity probe or allocation attempt. Use supported waits within
  client limits, without intermediate clock/status calls solely to recheck the wait.
  Necessary timed waits may still invoke the model.
  Send routine/material progress to one responsible canonical recipient named in the assignment
  (normally the coordinator while the developer phase has ended). Preserve operator updates.
  Do not send duplicate progress to both roles or acknowledge/follow up merely to say still running.
  Send failures, observation errors, blocked/interrupted execution and cleanup problems immediately
  to both the assigned developer and coordinator (canonical targets in the observer assignment).
  Escalate questions and approvals when observed;
  never answer approvals for the operator.
- Save the FULL native terminal tool object before any final artifact/projection read, even for
  empty stdout with a nonzero exit. Then read native summaries/receipts first. Return one final report:
  run/check identity, check and overall candidate statuses where applicable, original exit code, total duration and
  evidence paths. Verify every field; mark unavailable values explicitly. Use native duration
  or launch-to-completion time, never an individual wait's wall time. Preserve full logs;
  identify shortened output and include a bounded safe failure excerpt with its complete log path.
  Keep the original test exit distinct from capture failure/refusal and cleanup status; none of
  these can overwrite or turn that exit into a pass.
- Preserve failed, blocked, interrupted and incomplete outcomes. Missing expected final
  evidence is incomplete; heartbeats, process disappearance or one passing check do not
  prove complete acceptance.
- Reuse applicable evidence; do not duplicate logs/receipts in separate reports or run extra
  acceptance cycles solely to measure efficiency. Rerun reasons belong in the current task.

### Launch and observe long checks

Start the prescribed runner once; retain its process/session handle and evidence paths.
Wait about once a minute with the client's supported background wait, as described in
[Codex mechanics](#codex-mechanics) or [Claude Code mechanics](#claude-code-mechanics). On an
unchanged wait, continue the same wait without rereading plans, receipts or logs or sending an
acknowledgement. Each completed timed wait still returns to the model; this protocol does not
remove inference.

When the process ends, save the client's complete native completion result (exit code, output or
output path, and duration) to a unique unused file under `.build/` before any final artifact or
projection read. That result is authoritative for the process exit; a runner's JSON result cannot
replace it. If saving fails, keep the result in the conversation or task and report the capture
failure separately.

For release CI, a requested interval for GitHub API snapshots or routine progress reports does not
delay handling the existing release CLI process when it finishes. Keep awaiting its native completion
with the supported waits above, without extra API queries; save the terminal result and notify the
coordinator immediately. Resume comparison, review and pending gates on that event. Preserve the
available GitHub timestamp and the CLI's local observation/action timestamps when handing off;
an API update, local observation and the coordinator's next action are separate times.

Copy the prescribed command as an exact code block/argv, with no prose punctuation. Before launch,
verify its arguments, candidate, output directory, expected checks and capture paths. Keep one
observer and one runner owner; never restart a runner to repair observation.

[`dev/project-run-events.py`](../dev/project-run-events.py) reads only new complete JSONL records,
omits heartbeats and returns a bounded batch plus the next cursor. Accept the whole successful
projection before persisting its cursor outside the loop. It binds device/inode, observed size and
a bounded anchor at the processed newline; truncation/replacement or malformed complete records
produce explicit diagnostics, never an automatic offset reset. The journal stays untouched.
An incomplete tail is deferred. Drain `backlog` batches without another long wait so failures and
terminal events beyond the cap are reached. At native completion, an incomplete tail is incomplete
observation evidence. The anchor detects changes near the cursor, not arbitrary historical edits;
the existing journal remains append-only evidence, not a writable observation state.
A single record must fit the byte cap (8 KiB in the template); oversized lines are diagnostics.
If a valid native record needs a larger cap, adjust projection and output budgets explicitly.

For supplied GitHub run IDs, the same wake queries each `OWNER/REPO/RUN_ID` once through the
existing authorized read-only path, `subyard-github run -- gh api repos/OWNER/REPO/actions/runs/RUN_ID`.
Keep the full native query result and decoded response per run. API failure is an observation
error; only the API's `status` and `conclusion` describe the workflow. Completed IDs close
independently, so completion order does not cause serial watches.

A concise handoff retains the candidate's source/bundle identity; each stable source ID, handle,
journal, accepted cursor or cursor file, any pending EOF and native-result path or key; the operator's next wake time; and the
last material result with its sidecar/log/native pointers. For example: `candidate=<source>/<bundle>;
release: session=42, journal=.build/acceptance/events.jsonl, cursor=<accepted JSON>, pending=false;
GitHub: owner/repo/123=queued; next-wake=<operator-selected UTC>; last=selected passed, overall incomplete;
result=.build/acceptance/runs/<invocation>.json; native=.build/native-release.json`. Observation intervals
are assigned per session; these templates do not define a global interval.

For parallel acceptance, read new `check-result` events/current receipt as well as process
completion. Report failed/blocked checks when observed, without waiting for `run-result`;
keep observing the same controller through completion and cleanup unless the assigned developer
directs an owned interruption. Then return the final report and end the bounded assignment.
Make the native final summary/evidence available to both developer and coordinator; they need
not rewrite it. Stopping an observer does not establish that its runner or lease has drained.

#### Codex mechanics

Set wait intervals explicitly, about once a minute within tool limits and applicable rules:
`exec_command` initial `yield_time_ms=30000`, then `write_stdin` with empty `chars` and
`yield_time_ms=55000`; an enclosing `functions.exec` uses `yield_time_ms=60000` to cover
the inner wait and bounded reads. One enclosing call contains only one long process poll;
do not serialize several long polls in it. Resume a yielded script cell before another
process poll: its cell ID is distinct from the process session ID. Short default waits add
model returns; explicit outer timed yields can still cause model returns.

For tool-based observation, initialize `store("observer-sources", ids)` once with stable IDs.
Each `store("observer-source:" + id, state)` contains `session_id`, absolute `journal`, unique
unused `terminal_path` under `.build/`, `cursor: null`, `backlog: false`, `terminal: false` and
`closed: false`. Bind these states to the candidate/command. Native objects use separate
`observer-native:ID` keys; terminal objects use `observer-terminal:ID`. Never share a terminal path
or native key between sources. A launch that already terminated must save its full native object
and artifact first and initialize `terminal: true`; it then drains without polling.

The step below polls independent handles concurrently. Each worker stores its full native object
before waiting for another source or reading its final projection. Ready journals drain one page
per turn through a shared queue. Each active journal's EOF is fixed at its initial metadata read;
a newly terminal journal instead uses its final EOF. Appends cannot extend the current step.
`observer-max-pages` optionally bounds total projection work (default 100 pages); pending cursors
and snapshot EOFs survive the step. Resume pending drain without another native poll. Terminal
sources drain to final EOF; a trailing partial record is an observation diagnostic. Native completion
and journal completion are separate facts. Closed handles are never polled again.

```js
// @exec: {"yield_time_ms": 60000, "max_output_tokens": 24000}
// observer-poll-template
const quote = value => "'" + String(value).replaceAll("'", "'\\''") + "'";
const ids = load("observer-sources");
let pages = 0, draining = false;
const ready = [];
const limit = load("observer-max-pages") ?? 100;
const persist = source => store("observer-source:" + source.id, source.state);
const report = (source, fields) => notify(JSON.stringify({source: source.id, ...fields}));
async function drain() {
  draining = true;
  while (ready.length) {
    const job = ready.shift(), {source, resolve} = job, state = source.state;
    if (pages >= limit) {
      state.backlog = true;
      persist(source);
      report(source, {pending: true, cursor: state.cursor, eof: state.pending_eof});
      resolve();
      continue;
    }
    pages++;
    try {
      const command = "python3 dev/project-run-events.py --compact --max-bytes 8192 " + quote(state.journal)
        + (state.cursor === null ? "" : " --cursor " + quote(JSON.stringify(state.cursor)))
        + (state.pending_eof === undefined ? "" : " --until " + state.pending_eof);
      const native = await tools.exec_command({cmd: command, max_output_tokens: 24000});
      if (native.exit_code !== 0) throw new Error("projection failed: " + native.output.trim());
      const delta = JSON.parse(native.output);
      if (!delta.cursor || !Array.isArray(delta.events) || typeof delta.backlog !== "boolean")
        throw new Error("invalid projection response");
      state.cursor = delta.cursor; // Accept the whole page before advancing its cursor.
      state.backlog = delta.backlog;
      state.pending_eof = delta.eof;
      persist(source);
      if (delta.events.length) report(source, {events: delta.events});
      if (delta.backlog) ready.push(job); // Round-robin among all ready sources.
      else {
        delete state.pending_eof;
        if (state.terminal) {
          state.closed = true;
          if (delta.incomplete_bytes) report(source, {observation_error: "incomplete terminal journal tail",
            incomplete_bytes: delta.incomplete_bytes});
        }
        persist(source);
        resolve();
      }
    } catch (error) {
      state.projection_disabled = true;
      state.backlog = false;
      persist(source);
      report(source, {observation_error: error.message});
      resolve();
    }
  }
  draining = false;
}
function enqueue(source) {
  return new Promise(resolve => {
    ready.push({source, resolve});
    if (!draining) void drain();
  });
}
await Promise.allSettled(ids.map(id => (async () => {
  const source = {id, state: load("observer-source:" + id)}, state = source.state;
  if (state.closed) return;
  // Metadata only: freeze active EOF before polling; no final artifact/projection read yet.
  if (!state.terminal && !state.backlog && !state.projection_disabled) {
    try {
      const snapshot = await tools.exec_command({cmd: "python3 -c "
        + quote("import os,sys; print(os.stat(sys.argv[1]).st_size)") + " " + quote(state.journal), max_output_tokens: 1000});
      if (snapshot.exit_code !== 0 || !/^\d+\s*$/.test(snapshot.output)) throw new Error("journal snapshot unavailable");
      state.pending_eof = Number(snapshot.output.trim());
    } catch (error) {
      report(source, {observation_error: error.message});
      state.projection_disabled = true;
    }
  }
  if (!state.terminal && !state.backlog) {
    const native = await tools.write_stdin({session_id: state.session_id, chars: "", yield_time_ms: 55000});
    store("observer-native:" + id, native); // FULL object FIRST, before waiting for peers.
    if (native.exit_code !== undefined && native.exit_code !== null) {
      state.terminal = true;
      delete state.pending_eof; // Terminal drain captures final EOF after native completion.
      store("observer-terminal:" + id, native);
      persist(source);
      report(source, {terminal: true, native_exit_code: native.exit_code,
        native_terminal: state.terminal_path});
      const body = JSON.stringify(native, null, 2) + "\n";
      try {
        const saved = await tools.apply_patch("*** Begin Patch\n*** Add File: " + state.terminal_path + "\n"
          + body.trimEnd().split("\n").map(line => "+" + line).join("\n") + "\n*** End Patch");
        if (saved?.isError) throw new Error("native artifact save rejected");
      } catch (error) {
        report(source, {capture_error: error.message, native_key: "observer-terminal:" + id});
      }
    }
  }
  persist(source);
  if (!state.projection_disabled) await enqueue(source);
})().catch(error => notify(JSON.stringify({source: id, observation_error: error.message})))));
```

Unchanged steps emit no text/notify. Reports use immediate `notify` delivery because `text` is buffered
until the cell returns. Material native failures are reported as soon as received;
material journal failures are reported as each page is accepted, while other workers may still be
waiting. A rejected source does not discard another source's cursor or full native result. Artifact
save failure retains the terminal object in its own persistent key and reports capture separately.
Projection errors retain the accepted cursor and disable projection until explicitly repaired;
continue native polling through completion and cleanup. The full native `exec_command`/`write_stdin`
object remains authoritative for process exit; a producer JSON result cannot replace it. The template
is an observation step, not a runner, receipt writer or replacement controller. Its host-free checks
are `python3 -B dev/project-run-events-test.py` and `node dev/observer-poll-test.mjs`.

For GitHub runs, initialize `github-sources` with stable `REPOSITORY/RUN_ID` IDs and separate
`github-source:ID` states containing `repository`, `run_id` and `closed: false`. The full native query
and decoded API response have separate per-source keys. This example adds no network behavior to
the projector.

```js
// github-observer-template
const githubIds = load("github-sources");
await Promise.allSettled(githubIds.map(async id => {
  const state = load("github-source:" + id);
  if (state.closed) return;
  try {
    if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(state.repository) || !/^\d+$/.test(String(state.run_id)))
      throw new Error("invalid GitHub run identity");
    const native = await tools.exec_command({cmd: "subyard-github run -- gh api repos/" + state.repository
      + "/actions/runs/" + state.run_id, max_output_tokens: 24000});
    store("github-native:" + id, native);
    if (native.exit_code !== 0) throw new Error("GitHub status query failed");
    const response = JSON.parse(native.output);
    store("github-response:" + id, response);
    if (response.id !== Number(state.run_id) || typeof response.status !== "string"
        || !(response.conclusion === null || typeof response.conclusion === "string"))
      throw new Error("invalid GitHub run response");
    const changed = state.status !== response.status || state.conclusion !== response.conclusion;
    state.status = response.status;
    state.conclusion = response.conclusion;
    state.closed = response.status === "completed";
    store("github-source:" + id, state);
    if (changed) notify(JSON.stringify({source: id, status: response.status, conclusion: response.conclusion, closed: state.closed}));
  } catch (error) {
    notify(JSON.stringify({source: id, observation_error: error.message}));
  }
}));
```

#### Claude Code mechanics

Launch the prescribed command once with the Bash tool and `run_in_background: true`. The harness
notifies the session when the process exits, with its exit status and output file; that
notification is the native completion result. Do not poll the output file or sleep in the
foreground to detect completion.

Follow journal progress with one Monitor whose command runs the projector loop below. Each stdout
line becomes a notification, so only accepted material events and observation errors reach the
model. The cursor file is written only after a successful projection, before its events are
printed; a projection error keeps the accepted cursor, stops the loop and must be reported.
Monitors expire after at most 30 minutes; re-arm the same loop with the same cursor file.

```sh
journal=.build/acceptance/events.jsonl   # the runner's journal
state=.build/observe/release.cursor      # accepted cursor; absent before the first read
interval=60                              # 0 = final drain: stop when no backlog remains
mkdir -p "$(dirname "$state")"
while :; do
  args=(--compact --max-bytes 8192 "$journal")
  [ -s "$state" ] && args+=(--cursor "$(cat "$state")")
  if ! out=$(python3 dev/project-run-events.py "${args[@]}"); then
    printf 'observation_error %s\n' "$out"; exit 1
  fi
  jq -c .cursor <<<"$out" > "$state.tmp" && mv "$state.tmp" "$state"
  jq -c '.events[]' <<<"$out"
  jq -e .backlog <<<"$out" >/dev/null && continue
  [ "$interval" = 0 ] && { jq -c '{incomplete_bytes}' <<<"$out"; break; }
  sleep "$interval"
done
```

After the completion notification, save it as described above, stop the Monitor, and run the same
loop once in the foreground with `interval=0` to drain to the final EOF. A nonzero
`incomplete_bytes` there is an observation diagnostic. Native completion and journal completion
remain separate facts.

For GitHub runs, use a Monitor that queries each open run about every 60 seconds and prints one
line per status change, including every terminal `conclusion`; it exits when all runs are completed.

### Evidence handoff and cache cleanup

Before an expensive run, identify the smallest useful existing failure artifacts and which
will disappear with fixture/VM cleanup. Preserve their original bytes in the task's existing
private output directory before disposal, with bounded capture and restrictive access; hashes
and paths alone cannot recover deleted content. Keep secrets out of model output and tracked
files; return only safe bounded excerpts. Use the existing runner/fixture capture path rather
than another collector. A missing capture capability belongs to the owning fixture's repair.

On failure, save the native outcome first when available, then the minimal useful failure bytes
and safe excerpt with existing artifact paths, let runner cleanup/release finish, and return one
result. Capture from disposable storage must happen before it is deleted, even when the final
native outcome arrives only after cleanup. Preserve capture failure/refusal separately from the
original test exit; missing evidence stays incomplete and never authorizes a blind rerun.

Logs, receipts, native terminal objects and candidate bundles are temporary task artifacts.
Retain original records, launch bindings and complete logs while the task is active, including
failed, interrupted, blocked and incomplete outcomes and unavailable measurements. Keep sensitive
artifacts private; preserve safe redacted views for handoff. If retention would expire, copy the
needed bytes into the task's existing `.build` output directory and verify their hashes.
Never reconstruct missing artifacts. Copying records does not import a PASS onto another candidate.

After checks, review and runner cleanup, retain only concise key findings in the owning reference
or private overlay: the tested scope, source/candidate and baseline identities, outcomes, remaining
limits and any necessary reproduction steps. Do not commit task logs, receipts or bundles to Git.
Permanent log storage or copies outside the task cache are not required.

Then stop task-owned temporary helpers and remove only explicitly identified, completed task-owned
cache directories without asking for permission again. Confirm that their runners and leases have
finished and that no registered live
worktree or running application uses those paths. Preserve active sibling runs and shared build
outputs; never clear the whole `.build` as a handoff step. Keep original records until their review
and concise findings are complete. Cache cleanup does not require another VM acceptance run.

### Repair after a failure

Start with the observed native outcome and bounded failure excerpt linked to its full log.
Classify the cause as product, fixture or environment, then choose the smallest existing
diagnostic. State the hypothesis, owning files/check, expected difference and stop condition
before expanding reads. Fix the cause and run affected checks; batch independent repairs before
freezing a new candidate when needed. Before an expensive retry, verify the changed prerequisite
with its existing check and consult installed CLI help when invocation/compatibility is uncertain.
Record the concrete rerun reason in the current task, using existing evidence pointers.

Before cancelling an active check or repeating a passed one after a candidate change, identify
its affected runtime, fixture or shared inputs and check supported evidence import/transfer.
A new fingerprint alone is not a reason to cancel or repeat an unaffected check. If a required
gate cannot reuse that evidence, record that specific binding/coverage limit as the rerun reason.

Reassess a rebase or changed candidate by affected contracts. Reuse only supported, still-bound
results under the [candidate import/transfer limits](reference/release-publication.md#one-release-candidate); supporting diagnostics
do not replace required fresh, continuous or full end-to-end gates.

### Report deviations from the guide

If execution or reporting differs from this guide, promptly notify the operator before the
final task report: expected vs observed behavior, safe command/check and run identity,
evidence paths, and impact on coverage, result confidence or cleanup.
After reporting and investigating, correct code or outdated docs within the authorized scope
and verify; affected acceptance stays unconfirmed until valid evidence exists. Do not conceal
the discrepancy or weaken gates. A correctly reported failure/capacity refusal is an outcome,
not itself a guide deviation.

## Run the core checks

`make test` runs the basic Go tests and profile host-free checks. Core Go tests use
`-count=1` so repository-inventory checks execute even when their package code is unchanged;
the Go build cache still applies. To run only the core Go tests, use
`TMPDIR=/tmp go test -count=1 ./cmd/... ./internal/...`.

`./tests/run.sh` checks the current source files, including uncommitted edits. No Git history,
clean checkout, base commit or `.git` directory is required. Install the tools listed in
[the test guide](test-vms.md), including Git and ripgrep: tests of Git behavior create their own
temporary repositories. The release packaging test uses a disposable copy of the current public
files because it adds fixtures; that copy needs no Git metadata.

When the checkout is a Git work-tree root, `./tests/run.sh` ends with `checkout-unchanged`.
It fails when the run added or removed a path outside `.build/`, in the checkout or in an
optional `private/` clone. It compares path lists from `git ls-files`, with ignored directories
collapsed, so content edits and Git status changes of existing paths during the run pass. In
`private/` it ignores `tasks/`, `questions/`, `reference/`, `spec/` and top-level `*.md`. It also
fails when the run rewrote an untracked file of the retired `private/` config overlay
(`yards/`, `agents/`, `config.env`), which tests seed as checkout fixtures: a leak left by an
earlier run is otherwise part of the starting state. A run that fails earlier still prints these
changes. Outside a Git work-tree root the check is skipped.
The runner also sets `PYTHONDONTWRITEBYTECODE=1`.

`make test`, `./tests/run.sh`, the host-free profile runners and `dev/process-coverage.sh`
place disposable fixtures in `/tmp`, independently of a workspace-local `TMPDIR`, so Orca
cannot discover them in the working project. When invoking an individual test script or `go test` directly, use
`TMPDIR=/tmp`. Manual local reproductions must also create disposable Git repositories and
short-lived test worktrees under `/tmp`, outside managed workspace trees. `.gitignore` does
not prevent Orca from discovering nested Git roots, including those under `.build`.
Test worktrees must belong to fixture repositories.
Reserve `.build` for artifacts and logs; persistent development worktrees can remain there.
Representative local discovery tests share one Git directory across their checkout paths;
hook-observation tests use synthetic Git metadata. Ordinary Orca projects acceptance uses
small native fixtures; mass Git-root generation belongs only to the
[extended/manual diagnostic](#extendedmanual-diagnostics) on a leased disposable VM.

The runner prints one start/result line per check and a final `SUMMARY` with its
status, check count, elapsed seconds and original exit code. Successful check output
stays in separate logs under a unique `.build/test-runs/run.*/` directory. On the
first failure it stops, prints the full failed Go check output or the last 40 log lines
for other checks, and points to the complete log.
`RESULTS` and `SUMMARY` identify the run's `summary.tsv`, which has these columns:
`kind`, `suite`, `check`, `status`, `exit_code`, `duration_seconds`, `log`.
SIGINT and SIGTERM exit with 130 and 143 respectively; the active check and run are recorded as
failed with that status, rather than passed with partial output.
Log names are relative to that summary's directory. Each completed check has a
`check` row, and the final `run` row reports the whole invocation with suite `all`, `go` or
`shell`. A missing final row means the run did not finish reporting. Absent checks have no
result. Durations use Bash's elapsed seconds and may be zero for short checks. Separate
invocations never overwrite each other's results. Local core logs keep the newest 10 runs
under `.build/test-runs`.
On failure, CI uploads each affected job's verification console output and any core logs
and summaries as `ci-host-free-<go|shell|profiles>-<attempt>`. Release uploads its full
`make verify` output, core logs and summaries as `release-host-free-<attempt>`. Both retain
these artifacts for seven days. Read the summary first and open only the relevant log when
a check fails.

The two top-level host-free runners (`tests/run.sh`, `dev/test-profiles.sh`) and their
Makefile entrypoints give each invocation a private root under `/tmp`, with `TMPDIR`
and `GOTMPDIR` pointing into it. Nested runners reuse that root. The runner holds an
exclusive flock on the directory itself, inherited by children; the owner removes its root
only once unlocked, otherwise the next run does.
The next invocation removes abandoned sibling roots only after taking their lock and
validating an inode-bound receipt. Cleanup uses pinned directory fds and does not cross
mounts. Unknown directories, symlinks, incomplete or invalid receipts, and live locks
are preserved. Profile runners share the caller's process group, so group signals reach
their children directly. A PID-only INT or TERM stops dispatch without signalling running
profiles or other group members; the dispatcher waits for those profiles and exits with
130 or 143. The shell wrapper forwards these signals only to the dispatcher and waits.

A descendant that closes inherited fds and outlives its runner cannot retain the lock;
it may lose its scratch at the next run. SIGKILL before receipt publication leaves an
unknown directory for manual inspection. Direct individual tests still own their normal
trap or `t.Cleanup` cleanup; crash recovery belongs to the top-level runners.
`SUBYARD_KEEP_FAILED=1` explicitly preserves failed engine-release and source-constructor
fixtures outside the run root for diagnosis. Otherwise only failure logs are retained.

Host-free shell fixtures use a 30-second Incus agent deadline with margin for slow
CI scheduling instead of real VM boot deadlines. Tests of the deadline itself explicitly
select their intended timeout.
Keep fake responses current when shared lifecycle adapters gain new probes; include
captured fixture output and command logs when a fixture fails.

Profile-specific assertions live in `config/profiles/<name>/tests/`. Each shipped profile must own a
host-free `tests/run.sh` and, when it has live scenarios, `tests/e2e/acceptance.sh`. The shared
`dev/test-profiles.sh` runner enumerates every profile directory and checks all required entrypoints
before running any of them. A missing entrypoint is reported as incomplete and exits nonzero; a
profile can declare `tests/run.not-applicable` or `tests/e2e/acceptance.not-applicable` with a reason
only when that runner is mutually exclusive with the declaration. The reserved `default` profile is
the only built-in exception. A requested incomplete live runner keeps release acceptance incomplete.

Run all profile host-free checks with `bash dev/test-profiles.sh`, or invoke one profile's runner
directly. The shared host-free runner, including under `make verify`, runs up to three profiles
at once and reports every result. Each invocation keeps a combined stdout/stderr log per profile
under `.build/profile-test-runs/run.*/` and prints the log on failure. CI retains those logs for
seven days. Set `SUBYARD_PROFILE_TESTS_SERIAL=1` to run one profile at a time and stop at the first
failure. Core tests exercise generic APIs with synthetic fixtures; profile tests check their preset
and runtime through those APIs. Profile test directories are excluded from runtime release bundles.
For profile-owned live checks, use `bash dev/test-profiles.sh --e2e --slot N` or a profile's
`tests/e2e/acceptance.sh` directly, following the VM allocation guide below. The shared runner
forwards arguments; each profile owns its fixtures, assertions and lease cleanup. These profile
results supply physical evidence alongside core release smoke when explicitly requested. The
default release runs the host-free checks and compatibility matrix without allocating VMs.

Moving checks into profiles changes ownership: release acceptance inventories core and all shipped
profiles against the same candidate. Host-free checks remain required regardless of runtime profile
selection. Unrequested VM checks remain `not-run`; requested incomplete or failed checks block
acceptance. A passing default release does not claim full physical acceptance.

#### Periodic E2E on hosted runners

The Periodic E2E workflow (`.github/workflows/periodic-e2e.yml`) runs the long VM checks of release
acceptance on GitHub-hosted runners, so their failures surface between releases. It checks the latest
`master` commit at 00:23 and 12:23 UTC on `debian-13` and at 06:23 and 18:23 UTC on
`ubuntu-24.04`. The triggering cron entry determines the image even when GitHub starts the run
late. Each slot checks coverage for its own commit and image. A completed push or scheduled run
counts when its `pool` job succeeded and the run finished without cancellation. The workflow reads
the scheduled run title for its image and treats older unlabelled schedules and `ci-probe/**`
pushes as Debian runs. A failed check after pool success can be repeated with Re-run or a manual
dispatch.

On `master`, a new main run replaces an earlier run for the same image and, when it proceeds to
testing, stops its Intel retries. Main runs and retries on `master` show the image in their titles.
Other branches retain branch-wide replacement. A red check is a finding to fix early. Periodic E2E
is separate from [release acceptance](reference/release-publication.md#one-release-candidate).

`dev/ci/plan-periodic-checks.py` takes the checks from their existing owners: the release collector's
profile lanes, smoke check and core VM catalog (`dev/e2e/release-vm-checks.json`). Published upgrades
use the TSV's `direct`/`refusal` expectation (`anchor` and `hop` rows are direct) and the catalog's
exact agent selection; uncatalogued direct rows run agent-free and refusal rows run the refusal
scenario as `published:VERSION`. The catalog's full P0 check runs as its separate lanes,
except `boundary` and `cleanup`; a lane that the catalog also declares as its own check (the
`split-covered` line of `--list-lanes`) keeps this `p0:` run instead of a duplicate `vm:` check. The `pool` job first installs the candidate's `test-vms` service on a
runner and holds one real lease; each check then runs on its own fresh runner through
`dev/ci/periodic-e2e.sh`, which refuses to run anywhere but a disposable hosted runner. The runner
becomes an ordinary owner with the packaged candidate. A dispatch can select
`image=debian-13`, `image=ubuntu-24.04` or `image=ubuntu-26.04`. Manual dispatches default
to Debian 13, which is also the image used by `ci-probe/**` pushes. The planner passes
`--image` to the hosted wrapper for every lane, and the pool boundary check uses the same image.
The wrapper gives lane controllers a lease image default,
which the shared client sends to the hosted broker. The broker creates leases from the matching
Incus VM base. Intel retries retain the original image. Lane IDs and fixture images inside the
leased VMs are shared across the dispatch choices.
The `pool`, `check` and Intel `nested` jobs use the bounded runner/P0 snapshots and failed-log
tails [listed in hosted check diagnostics](reference/release-publication.md#exact-candidate-hosted-release-checks), uploaded on every outcome as `pool-diagnostics-ATTEMPT`,
`diagnostics-JOB_INDEX-ATTEMPT` and `nested-diagnostics-JOB_INDEX-ATTEMPT`. Their `runner/check.log`
contains check output, including native cleanup output, and their broker event records survive
interruption when final steps can run. Logs are staged under the runner temporary directory rather
than the managed candidate workspace; the upload path therefore matches the capture path. Jobs run
as root on the runner and hold no secrets, so the workflow never runs pull request code.

Published upgrade lanes use only v0.28.0 or newer. The retired schema-1 source-upgrade
and below-anchor published-refusal lanes are absent from the planner. Host-free
`compatibility` exercises the actual v0.28.0 updater against the packaged candidate,
including read-only inspection and retained settings/data. Config apply/status with a
guest file hash belongs to `p0:init-settings`. The `published:0.28.1` lane seeds an
explicitly synthetic retained schema-1 Git settings manifest on the supported hop,
checks that candidate update preserves it, then migrates it with `config sync`.

Checks that boot a VM inside the leased VM run only on Intel runners. Hosted runners are Azure
Hyper-V VMs on Intel or AMD CPUs, and the runner label cannot choose one. On AMD, virtualization
nested this deep is so slow that these checks cannot finish. On an AMD runner the job explains the
skip, and a `reschedule` job dispatches runs at the branch's latest commit with titles starting
with `Nested VM checks on Intel, attempt N, for run X`. A retry runs each check on three runners
at once and is repeated up to 30 times; checks that never reached Intel fail that last run as not run.
Only the `reschedule` job may
start runs, and the `supersede` job cancels replaced ones; neither checks out code.

Periodic-only `vm:incus-apparmor-probe` installs the packaged candidate on one standard VM and
checks failed AppArmor probes, both real Incus AppArmor transitions and nested Docker preservation.
The inner Incus must start with AppArmor enabled; the fixture refuses a disabled baseline and
restores its marked override before teardown. It runs in the ordinary `checks` matrix because it
creates a container, without booting a further VM. See the [focused probe recipe](test-vms.md#test-lanes-and-gates).

The Veranda GUI SHORT scenarios run here only, not in release acceptance, so they never gate a CLI or
engine release: `veranda:x11`, `veranda:wayland` and the manual `veranda:x11-remote-ui` and
`veranda:wayland-ime` lease one VM each through
`dev/e2e/veranda-short.sh`, as the [Veranda SHORT recipe](../veranda/desktop/README.md#disposable-x11-and-wayland-short)
does. A separate `veranda-bundle` job builds the GUI and a matching engine with the stable version
`VERANDA_VERSION` and adds them to a copy of the candidate bundle; other checks keep the unchanged
candidate bundle and still run if that build fails, while the Veranda checks then fail. These checks
measure no resource budgets.

The periodic-only `periodic:config-sync` check runs by default and can be selected with `checks`.
It leases a standard pair for installed configuration sync over SSH/Git, using the ordinary candidate
bundle. Its controller budget is 45 minutes plus 10 minutes for TERM cleanup; a hosted run took
17 minutes. It is outside the release catalog and release acceptance; changes listed in
[coverage ownership](#coverage-ownership) require this lane.

The optional `periodic:nm-guard/fresh` and `periodic:nm-guard/upgrade` checks use the selected
lease image and run by default. They are outside release acceptance.

A manual dispatch can name the check ids to run in `checks`. A check the planner marks `manual`, such
as `veranda:wayland-ime`, runs only when named there. The separate Stale runs workflow cancels
runs of any workflow whose latest attempt has been queued or in progress for over a day, because such
a run holds its concurrency group.

Standard VM checks for a pull request run here, not as a required status check. Dispatch the lanes
selected by the risk policy on the pushed branch (ids as printed by `dev/ci/plan-periodic-checks.py`)
and watch the run; its candidate identity is the branch head commit at dispatch. Intel retries
test the branch head at their start, so watch them too: a required check counts only when green on
the current branch head. A parent run that finished before a later push does not cover the new head.

```sh
subyard-github run -- gh workflow run periodic-e2e.yml --ref BRANCH -f image=ubuntu-24.04 -f checks='ID ...'
subyard-github run -- gh run list --workflow periodic-e2e.yml --branch BRANCH --commit HEAD_SHA \
  --json databaseId,displayTitle,status,conclusion
subyard-github run -- gh run watch RUN_ID --exit-status
```

Agent compatibility regressions run locally with
`go test ./internal/adapters/statusruntime` and `bash tests/codex-agent-provision.sh`.
They use temporary rules, fake CLI programs and release metadata; they do not start VMs,
contact model APIs or publish Git history. They cover the matcher invocation and failure
handling, latest-release installation and preservation of a working binary after a bad download.
Mocks do not establish compatibility with a real CLI release. Check its native `execpolicy check`
against the shipped rules for that evidence; real client approve/deny needs separate acceptance.

`bash tests/claude-agent-policy.sh` runs the Claude package's hermetic CLI-install and policy checks.
For a native Claude download, installation and repeat-provision smoke test, use
`dev/agent-e2e.sh --slot N --vm-count 1 --vm 1 --purpose claude-install -- bash config/agents/claude/tests/e2e/acceptance.sh --install-only`.
This mode needs no account or model API and does not run the separate SDK permission scenarios.

## Run Veranda host-free checks

Install Python 3 and the current stable Rust toolchain selected by
[`rust-toolchain.toml`](../rust-toolchain.toml), plus the module-selected Go toolchain on Linux
for the native owner process contract, then run from the repository root:

```sh
make verify-veranda
```

The target runs `python3 dev/check-veranda.py`; override the interpreter with
`make verify-veranda PYTHON=python` when needed. CI runs the same entrypoint with
`python` and current stable Rust for native tests. The native client has no npm/WebKit dependencies.
The entrypoint explicitly selects `+stable` for formatting, Clippy and tests,
including when `RUSTUP_TOOLCHAIN` is set.
The target and entrypoint do not install toolchains. `make verify` retains its
separate core and shipped-profile scope.

The runner temporary-path contract runs first, then on Linux the resource probe isolation,
memory probe accounting, bundle packaging and Cargo cache reuse tests;
a failed prerequisite stops the run. Next, Rust formatting, Clippy, and native client
tests with `--locked` run in that order, reporting every independent failure.
Python fixtures use the entrypoint's own interpreter.
`python3 dev/check-veranda.py --rust-only` skips the Python prerequisites and runs
formatting, Clippy, and native tests in the same order.
On Linux that suite runs once with a child-only `TMPDIR` alias to a private real
directory under an owned `/tmp` fixture. This exercises symlink temporary paths
without mutating the parent environment or parallel Rust tests' global state;
cleanup failure fails the check. Non-Linux runs explicitly skip the Linux probes.

A local Linux pass does not verify native ARM, macOS or Windows; those remain
separate CI matrix results. Desktop launches, real local/SSH owners, package
installation/upgrade/rollback and physical resource acceptance remain separate
[Veranda delivery gates](../veranda/README.md#delivery-gates).

Before a long Veranda VM scenario, use the same build in the target environment for
preparation, launch, one real product action, normal closure and cleanup. Verify actual
API responses and UI states. On the first failure, preserve the result, distinguish fixture
and product causes, repair the cause and repeat the short check before the long scenario.
Check nonzero executed test counts and calibrate the producer's actual offered, dispatched
and completed load. Reuse unaffected passes and retain the original acceptance criteria.

## Extended/manual diagnostics

The Orca load diagnostic is an explicit manual check, separate from `tests/run.sh`,
profile host-free runners, profile acceptance lanes and release acceptance. Ordinary
missing-record regression uses a small representative fixture. The large workload
of 1,000 or 1,400 Git roots and 100,001 flat files belongs only to the diagnostic's
leased disposable VM; never generate it in a live controller checkout or its `.build`.
Local `.build` directories may retain numeric results and sanitized logs.

Run the [dedicated controller](test-vms.md#extendedmanual-orca-load-diagnostic)
explicitly when measuring Orca catalog behavior. Passing ordinary tests does not prove
load capacity, and a diagnostic timeout or interrupted run is incomplete evidence.

## Choose the VM allocation

Android acceptance runs on one `android-test` VM (8 GiB RAM / 40 GiB disk). Ordinary full P0
uses the standard `subyard-pair` (two 4 GiB / 20 GiB guests) for its two-host scenarios.
Do not request two 16 GiB guests for Android or ordinary full P0.

`--vm 1` only selects where a command runs; it does not turn a pair allocation into a singleton.
Use `--vm-count 1` for one standard VM; `--type android-test` retains the larger Android
singleton. Commands and exact coverage are documented in
[the VM guide](test-vms.md#agent-workflow).

## Evidence tiers

| Tier | Evidence |
| --- | --- |
| T0 | An exact regression test for the defect or failure mode. The engineer or agent defines it from the observed failure. Target: at most 60 seconds. |
| T1 | Affected host-free package, race, shell, CLI, frontend, or Rust checks. Typical target: at most 3 minutes; broader owning checks may take longer. |
| T2 | The full host-free suite, `./tests/run.sh`. Veranda retains its separate host-free checks. |
| T3 | Existing targeted E2E lanes or real-host checks for affected physical boundaries. |
| T4 | A fresh full P0 of a frozen candidate: `dev/e2e/p0-acceptance.sh --slot N --lane full` with `SUBYARD_E2E_CANDIDATE_BUNDLE` and `SUBYARD_E2E_CANDIDATE_SHA256` from `dev/release-acceptance.py prepare`; release acceptance runs it as `vm:p0-full` with `--split-covered` plus the separate `vm:p0-power-systemd` and `vm:p0-legacy-continuation` checks. |

A fresh standalone full pass also satisfies its contained T3 lanes: smoke, boundary, transport, nested
teardown, real Incus, release, power/systemd (including reboot verification), legacy
continuation, peer and cleanup. In release acceptance, full P0 is a fresh `vm:p0-full --split-covered`
plus its split lane checks (`vm:p0-power-systemd`, `vm:p0-legacy-continuation`)
of the same frozen candidate. The catalog's `requires` field on `p0-full` declares them: once
`vm:p0-full` is requested or has any recorded result (run, imported or transferred), the aggregate is
not `passed` until each of them passes. Each runs the full pass's final home, cache, capacity and
boundary verification.
Cold dependencies, profile-resource's bind fixture and Orca acceptance are not covered by that matrix.
See the skill for when local, VM and publication checks apply; the tiers describe evidence scope,
not a ladder that every change must climb.

## Continue work across leases

Follow the skill's [lease evidence rules](../.agents/skills/subyard-dev-flow/SKILL.md#test-progress-across-leases)
and the [VM guide](test-vms.md) for disposable targets and lane commands.
