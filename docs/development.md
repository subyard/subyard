# Development

Subyard's control-plane engine is a native Linux Go binary. Shell remains the system-adapter and
safety layer, so contributors need both the Go and shell toolchains.

`make build` also builds package-owned native executables declared by profiles
and coding integrations. Outputs remain separate under `.build/profiles/` and
`.build/agents/`; runtime packaging includes both for the selected architecture.
Plain source copies need no Git metadata. Native integration helper bytes bind
assessments: rebuild before planning and obtain a fresh plan after rebuilding.
Observation does not build a missing helper.

The same build also stages normal Go tooling under `.build/runtime/`.
It reuses the runtime packager's complete asset inventory and sealed-image
verification; `.build/runtime/current` selects an immutable versioned stage. The stage's
Go helper must match `.build/yard` exactly. Ordinary source admission uses the Go read-only implementation and needs no Rust executable.
The explicit experimental stage also supports Rust queries over historical Go data.
The tooling program never changes which exact release owns a transition.

## Toolchain

`go.mod` is authoritative: the module's minimum language baseline is Go 1.25.6 and its `toolchain`
directive selects Go 1.26.6 for development and CI. A recent Go bootstrap can fetch that toolchain
automatically. CI uses `actions/setup-go` with `go-version-file: go.mod`; it does not carry a second
version constant.

On Debian, install the normal build prerequisites with:

```sh
sudo apt-get update
sudo apt-get install -y golang-go gcc make shellcheck jq ripgrep binutils curl ca-certificates systemd
```

Inside a yard dedicated to Subyard development, enable the `subyard-dev` profile and run `yard
provision subyard-dev` from the owner host. The profile installs the Debian Go bootstrap,
ShellCheck and Go build prerequisites in L1. The direct provision hook's `--rust` option
(and `--check --rust`) explicitly adds experimental Rust/musl tooling. Existing distro Rust is preserved. When Rustup is
missing, its official installer is downloaded and SHA-256 verified, then run as the developer.
Rustup uses Cargo and Rustup
homes beneath that developer's home; its readiness check reads installed toolchains without
downloading them. `go.mod` still selects the exact Go compiler and shared Go caches live under
`/srv/cache`.

Debian 13 currently ships an older bootstrap Go than the Incus client requires. That is acceptable
for source development because Go follows the module's `toolchain` directive. Confirm the selected
compiler with `go version` after the first build.

## Experimental Rust release owner

Ordinary runtime packages remain Go-owned and exclude Rust assets.
`dev/package-engine.sh --experimental-release-owner` explicitly includes `bin/subyard-release-owner`, its exact Go native helper
`bin/yard-engine`, and the manifest-bound declaration `config/release-owner.json`.
The installed host needs neither a Go nor a Rust toolchain. Only experimental Rust packaging requires
Go from `go.mod`, the root stable Rust toolchain, Cargo, musl target standard libraries,
and `readelf` from binutils. Dependencies build with `Cargo.lock` and `--locked`;
fixture-only Cargo features are excluded from shipped binaries.

For manual setup, install Rustup as your developer user following its
[installation guide](https://rust-lang.github.io/rustup/installation/), then install the
musl standard libraries with `rustup target add x86_64-unknown-linux-musl
aarch64-unknown-linux-musl`. Normal runtime and tooling builds reject Go build tags;
test-only oracle or fixture images cannot serve as development tooling.

`make build-experimental-release-owner VERSION=0.22.0-test` stages the experimental runtime.
It does not enable production routing. `dev/build-release-owner.sh` builds the owner for Linux amd64 or arm64 musl.
`dev/package-engine.sh` uses it when assembling a runtime. The manifest-listed build
sidecar records the actual compiler, Cargo target, lockfile and source input hashes,
features, executable digest and static ELF checks. The stable toolchain channel does
not itself pin a compiler version; use the recorded resolved version when reproducing
an artifact. Cross-compiling arm64 verifies a build, while native ARM process behavior
is checked separately in CI.

`make verify-release-owner-foundation`, `make verify-release-owner-recovery` and
`make verify-release-owner-legacy` provide focused domain, protected-storage and frozen
codec checks. `release-owner/check-runtime.sh` adds actual owner/native process gates.
Its owning Go trace export runs before the Rust consumers; full-entry replay then binds exact
native plan, grant and durable journal bytes. Scripted component peers establish protocol and
reducer comparison, while normal sealed packages and real component effects have separate gates.
`dev/check-release-owner-delivery.sh` packages both architectures and checks inventory,
sealing, the retained session, artifact tampering and execution without toolchains.
Standalone Go domain tests remain usable without building Rust; package and process gates
require both toolchains. Preserve absolute `CARGO_HOME` and `RUSTUP_HOME` when a test
isolates `HOME`.

The owning Rust checks compare production history directly against 70 generated cases
from Go's independent bitset oracle. No separate history or native-owner pilot is built.
The current owner/native process checks retain consent, source-CAS, custody and process-loss
coverage. These checks do not replace normal packaged-owner, released-caller, component
or VM acceptance. Production routing remains gated until the complete candidate passes.

## Build and test

`make cli-docs` builds the current CLI and regenerates [the command reference](cli-reference.md)
from its actual public command help. `make cli-docs-check` verifies that the checked-in reference
is current, and CI runs this check. Generation uses a temporary home and public configuration,
without operator settings.

```sh
make build
make verify
```

`make build` writes the ignored developer candidate `.build/yard` atomically and builds declared
profile binaries under `.build/profiles/<name>/`. It also publishes the complete Go tooling
runtime under `.build/runtime/current`. `make test` and `make verify` stage a source-compatible test version before Go consumers;
targeted source-repair tests require `make build VERSION=0.22.0-test` first. A delivery gate may instead supply its
once-built normal runtime through `SUBYARD_RUST_SOURCE_QUERY_TOOLING_ROOT`.
`make verify` runs core and profile host-free
checks together. Packaging builds the same declared binaries for the target architecture, excluding
profile Go sources and test directories from runtime bundles. Packaging enumerates current public
files using the repository ignore rules, including uncommitted and untracked public inputs; it also
works from a plain source copy without a Git index. The source-tree
`bin/yard` launcher uses that explicit candidate and never compiles or downloads a toolchain at
runtime. Production does not use the source checkout:

```sh
curl -fsSL --proto '=https' --tlsv1.2 \
  https://github.com/Subyard/Subyard/releases/latest/download/subyard-install.sh | bash
exec "$SHELL" -l
```

This requires `curl`, `jq`, `sha256sum`, `tar`, `gzip`, Python 3, and `flock`; it asks once before changing the host,
links `~/.local/bin/{yard,sy}` to the verified runtime, and configures login PATH and completion.
The standalone runtime installer also uses Python 3 and `flock` to retain installation ownership
and serialize publication with setup, updates, and full uninstall.

`make package VERSION=<version>` writes amd64 or arm64 Linux engine artifacts and a complete
`subyard-<version>-linux-<arch>.tar.gz` runtime under `.build/release/`, each with a detached SHA-256,
compatibility manifest and provenance. Runtime releases use the
[prepare → accept → publish flow](reference/release-publication.md): CI builds exact assets,
acceptance checks core and every shipped profile on disposable VMs, and publication verifies the
immutable release. [Live external checks](reference/release-publication.md#external-checks) are
optional; required VM checks remain mandatory. `yard update`
verifies all release inputs, applies the candidate's migration registry, publishes an immutable
release directory and atomically rotates
`current`/`previous`. See [release migrations](control-plane.md#release-migrations) for the runtime
contract and [real-host acceptance](real-host-acceptance.md) for its test lane. First install and
runtime execution require no Go or source checkout; interrupted or incompatible releases cannot
replace the working runtime. Recognized pre-0.1 source installs enter the same assessed release
transition. Its bounded [`scripts/migrate-source-install.sh`](../scripts/migrate-source-install.sh)
leaf publishes recovery facts before importing config and later switches shell entrypoints; it does
not authorize, activate or roll back a release. An interruption resumes from the protected outer
journal and observed facts.

Native Paseo builds keep downloads, npm cache and logs in operation-owned disk
scratch under `.build/paseo-operation-<uid>`. A private temporary mount catches direct
`/tmp`, `/var/tmp` and `/dev/shm` writes. One build runs per checkout and user;
completion removes scratch, and the next invocation removes leftovers with a valid
durable ownership receipt after SIGKILL or reboot. Incomplete initialization or
ambiguous ownership is preserved and refused for inspection. Old `.build/paseo-npm-cache` and diagnostic directories are left
untouched. Safe npm records go to stderr, capped at 8 KiB per attempt. Network
retries remain bounded; version, integrity and native-module failures stop the build.
The build needs Linux user/mount/PID namespace support and verified disk storage.

Profile extraction uses the same lifecycle in guest disk storage under
`/var/lib/subyard/profile-operation`, with one operation per guest. Controller stderr
is drained with a 64 KiB capture limit. An optional profile `provision-tmp-paths`
file lists exact existing temporary directories needed by migration hooks; symlinks
are refused. Other temporary files remain private. The original guest `/proc` stays
visible for guards that inspect existing workloads.
Hooks that call `systemctl` need the guest's existing system bus; the worker sets
the [documented `SYSTEMCTL_FORCE_BUS=1` route](https://github.com/systemd/systemd/blob/v257.13/docs/ENVIRONMENT.md)
across the PID namespace, preserving
existing bus addresses. This does not install or start a bus.
Descendant cleanup covers spawned processes, including detached sessions; intentional work requested through
external daemons retains those daemons' lifecycle. No per-operation RAM, byte or
inode quota is imposed; the existing yard RAM boundary still applies.

After activation or rollback and configuration refresh, `yard update` reinspects the exact selected
release without fetching another version. Its final human-readable summary reports readiness and
the active and previous releases, with blockers and next steps when needed. An unsuccessful final
check exits nonzero and records a failed update; `yard update --check` prints indented JSON; add `--json` for compact machine output.

If the protected transition requires repair of the active release, `yard update` first repairs that
release and ends the invocation before checking for a newer version. Its preview and final result
identify the repair explicitly; after a successful repair, run `yard update` again to check for and
install a newer release.

Structured update history is durable outside the installed runtime. Each committed activation or rollback,
plus direct preparation failures and declined confirmations, records a structured attempt under
`$SUBYARD_HOME/logs/updates`; the newest 30 attempts are retained. `yard logs --updates [-n N]`
shows their phase progression, verified source and target versions when available, and safe terminal
status codes. It deliberately excludes installer output, error text, environment values, and recovery
journal JSON; detailed diagnostics remain on the invoking console. `yard logs --audit [-n N]` reads
the host command audit log across the current file and five 1 MiB rotations. Both local viewers work
without a usable yard configuration or Incus, while an explicit `-Y` selector preserves ordinary
owner routing. `yard logs` without either selector continues to show the selected yard's runtime log.

Verify a built candidate with the unmodified supported updater before release:

```sh
python3 dev/verify-release-upgrades.py --release-dir .build/release --version <candidate-version>
```

This Linux check needs Python 3 and Go to build the local empty-instance Incus API fixture. It
downloads checksum-pinned v0.4.1, v0.4.2, v0.9.1, v0.10.0, v0.11.0, v0.11.2, v0.17.3, v0.18.1, v0.18.6 and v0.19.0 runtimes for the local architecture
and verifies retired-source refusal/full-uninstall, the inclusive v0.11.0 source boundary, frozen and
published updater contracts, and activation-only recovery. It confines all state to a temporary directory and interrupts only its own update process
group. Use `--baseline-dir PATH`, `--legacy-baseline-dir PATH`, `--activation-baseline-dir PATH`
`--published-baseline-dir PATH` and `--source-baseline-dir PATH` to reuse downloaded official assets (including the legacy runtime
installer). For a focused legacy-retirement regression, use `--only-legacy-retirement`.
For a focused v0.17.3 regression, add `--only-activation-recovery`; publication requires
the complete check after building the release assets.
The fresh recovery fixture preserves stopped-yard deferred evidence: equal migration epochs do
not make its installed component contracts compatible with v0.11.2, which has no preview owner
payload or runtime-owner advertisement. That branch requires typed rollback refusal with unchanged
history; a separate compatible fixture proves successful matching-epoch rollback and forward update.

For producer/delivery changes, `--only-runtime-recovery` runs actual v0.18.6
caller continuation and eligible rollback plus exact sealed v0.17.3
activation-only owner recovery. It retains the same published pins and does not
replace the complete publication compatibility gate.

The actual v0.18.6 cases retain completed history and authentically interrupted activation-only
history, reproduce the released alias writer defect, stage verified repair tooling, require fresh source
consent, and require separate original-owner recovery consent before ordinary upgrade. Use
`--only-source-repair` for this focused check. It does not replace the complete released-caller matrix.

The ordinary released-caller compatibility gate includes the unmodified, checksum-pinned
v0.19.0 updater on both architectures: direct upgrade to the fixed candidate, a controlled
activation observation failure after verified migration effects, preservation of the owning
diagnostic during reread, and fresh-process resume without replaying completed migrations.
It verifies retained config/data, checkpoint/projection and a normal readiness-gated command.
`--only-direct-019` selects this vector alone; supplying the authentic custom predecessor flags
adds that incident-specific history without making public CI depend on a private artifact.

For the exact migration-ledger owner defect, `--only-migration-owner-replacement` additionally
requires `--migration-owner-baseline-dir` containing the checksum-pinned official v0.21.0 Linux
amd64 bundle and `--migration-owner-source-dir` / `--migration-owner-source-version` identifying
an authentic retained predecessor. The fixture preserves both update generations' transaction
evidence, reproduces the released reader failure, checks fresh consent, interrupts successor
publication and resumes the ordinary V2 candidate journal. This local fixture uses observation-only
Incus responses and does not establish installed Incus/systemd activation behavior.

The default compatibility invocation also runs the migration-history process corpus.
Checkpoint fixtures must pass supported source admission before and after repair; the legacy
fixture must retain its exact known unsupported-history refusal. The controlled apply failure
must preserve the public materialized-config diagnostic and retry, with one recorded exit37
for the exact seeded command and stdin. Use
`--only-migration-history` to isolate it and `--migration-history-baselines PATH` to reuse
checksum-pinned `baseline-VERSION` directories. Add `--migration-history-case checkpoint`, `legacy`, or `cuts`
with the focused flag to isolate a diagnostic; the default gate runs all cases. It retains authentic completed transactions
from v0.18.6 → v0.19.0 and v0.11.2 → v0.18.1, then exercises the same packaged candidate's
independent power and SSH-settings migrations, activation failure, fresh-process retry,
ledger/checkpoint interruption, and refusal of changed evidence or stale consent. Runtime
readiness must come from the ordinary update gate after migration completion.

The synthetic integration uses a regular text source and the generic materialized-config
contract. Its Incus API fixture accepts exact core command grammars and captured stdin;
it never executes guest commands. Known simulated config drift is armed once only after the
candidate is current, the journal is reconciling, and the migration steps are verified; this
keeps the released predecessor ready before ordinary update. Profile observations remain in each profile's existing
fixture provider. These process checks establish packaged caller, history and reducer
behavior with simulated guest effects; physical file durability, Incus and systemd behavior
still require the owning VM acceptance lanes. Evidence and pin mutants exercise unfinished
verified recovery before further effects, including the journal's embedded evidence. Completed
runtime readiness alone does not require every retained sidecar. The existing read-only
`config repair-alias ... --check` source-admission route validates those separate artifact triples;
unsupported admission is recorded explicitly and grants no repair authority.
Run this corpus when migration history,
source admission, recovery authorization or the activation readiness boundary changes.

The packaged bootstrap supports pinned staging without loading broken configuration:

```sh
bash subyard-install.sh --publish-only --version <chosen-version>
```

It uses the normal download/checksum/manifest/provenance verification and reports the exact immutable
candidate identity and path, then stops before activation, stable links, shell setup or source changes.
Invoke that candidate's `bin/yard` for explicit supported source repair. Staging is preparation, not
installation or release readiness; it cannot retroactively upgrade retained caller capabilities.
Publishing or operator installation requires the separate delivery authorization.

Future persistent schema changes must retain a compatible reader or provide a typed migration and
capability boundary. Keep generalized alias authoring safety and actual released-caller regressions
for both completed history and retained unfinished history; rebuilding an old binary is insufficient.

The complete check also uses the actual v0.11.2 and v0.18.1 callers to verify checkpoint-reader bridge
ordering and rollback refusal after authoritative epochs exceed the frozen projection. It packages a
synthetic epoch-3 owner from the invoking bound source with an explicit test version and unchanged published
baseline pins. Add `--only-checkpoint-source` for this focused regression; it does not replace the
complete release compatibility gate or publish the reader bridge.

The [legacy migration support policy](reference/legacy-migration-support.md) sets the approved
minimum source version to v0.11.0, inclusive. Earlier V1 sources are refused and use the existing
full-uninstall route, followed by a separate clean installation. No V1 upgrade bridge or notice
window is provided. Supported V2 caller,
producer, recovery and checkpoint-reader contracts remain covered, including actual acceptance
of the v0.11.0 lower bound.

Choose local checks with [Subyard dev-flow](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk).
`./tests/run.sh` is the full unprivileged suite. It runs formatting, vet, race-enabled Go tests, a
short parser fuzz smoke, the static binary build, and all Bash unit/contract/integration tests. It
requires the `systemd-analyze` binary for unprivileged unit parsing, but does not require root, a
running systemd manager/PID 1, the host Incus socket, real credentials, SSH peers, or external
services.

CI additionally installs `openssh-server`, downloads the pinned age/SOPS artifacts through the
checksum-verifying project installer, and runs the temporary loopback contracts under
`tests/real-host/`. Those tests use synthetic payloads and an ephemeral non-system sshd; dedicated
container/VM and two-owner-host acceptance run separately. The commands are
`dev/e2e/p0-acceptance.sh --slot N` for release smoke and `--lane full` for the full matrix;
their selection criteria live in the skill.

Live platform and release acceptance runs only on operator-allocated E2E VMs; see
[`real-host-acceptance.md`](real-host-acceptance.md).

On a trusted KVM-capable host, the two-instance portion can run in an opt-in container yard through
[`yard test-vms`](test-vms.md). Its lifecycle, lease fencing and heartbeat expiry are covered host-free by
`tests/test-vms.sh`; actual VM boot and SSH remain an E2E VM gate.
