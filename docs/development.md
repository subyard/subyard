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
The tooling program never changes which exact release owns a transition.

## Toolchain

`go.mod` is authoritative: the module's minimum language baseline is Go 1.26.0 and its `toolchain`
directive selects Go 1.26.10 for development and CI. A recent Go bootstrap can fetch that toolchain
automatically. CI uses `actions/setup-go` with `go-version-file: go.mod`; it does not carry a second
version constant.

`make lint` checks closed release-transition states and their consumers with golangci-lint
v2.15.0 and its `exhaustive` linter. `make verify` and the existing CI verification job
include this gate. The runner downloads checksum-pinned official Linux amd64/arm64
binaries into `.build/tools/`; Go build and module caches remain shared. A fail-closed
`default` branch does not satisfy exhaustiveness when a checkpoint state is added.

On Debian, install the normal build prerequisites with:

```sh
sudo apt-get update
sudo apt-get install -y golang-go gcc make shellcheck jq ripgrep binutils curl ca-certificates systemd
```

Inside a yard dedicated to Subyard development, enable the `subyard-dev` profile and run `yard
provision subyard-dev` from the owner host. The profile installs the Debian Go bootstrap,
ShellCheck and Go build prerequisites in L1. The direct provision hook's `--rust` option
(and `--check --rust`) explicitly adds Rust tooling for Veranda. Existing distro Rust is preserved. When Rustup is
missing, its official installer is downloaded and SHA-256 verified, then run as the developer.
Rustup uses Cargo and Rustup
homes beneath that developer's home; its readiness check reads installed toolchains without
downloading them. `go.mod` still selects the exact Go compiler and shared Go caches live under
`/srv/cache`.

The profile publishes absolute `GOCACHE` and `GOMODCACHE` through PAM's
`/etc/environment`, `/etc/profile.d/subyard-dev.sh`, and an `environment.d` file.
New SSH/login shells and IDE/agent command shells that source `/etc/profile` therefore
keep the same caches with a separate application `XDG_CONFIG_HOME`. Future user-manager
processes inherit the same defaults. Provisioning does not restart existing sessions or
services; a system service that does not launch such a shell must pass the cache environment
explicitly. The standard Go user configuration is still written to the developer's
`~/.config/go/env`, independently of the provision caller's XDG settings.

Rust runners use `dev/cargo_cache.py` before Cargo, supplying the owning manifest and a
run-owned final output directory. For example, from the checkout root:

```sh
cargo_environment="$(python3 dev/cargo_cache.py --manifest-path veranda/desktop/Cargo.toml --target-dir "$PWD/.build/my-check/target" --snapshot --run-target)"
eval "$cargo_environment"
```

`--run-target` allocates an invocation-owned final subdirectory under the given target
parent. This preserves explicit absolute cache/home/target overrides; a caller sharing an
explicit final directory must serialize its own build/read/publication boundary. Otherwise Cargo packages and
Rustup toolchains use the invoking developer's normal homes, and intermediates use
`$SUBYARD_CARGO_BUILD_ROOT/<resolved-compiler-group>` (`/srv/cache/cargo-build/subyard`
in the profile, or `~/.cache/subyard/cargo-build` outside it). Cargo 1.91 or newer is required.
The group records the resolved compiler and Cargo identity, plus explicit compiler wrappers;
Cargo fingerprints features, targets, profiles and flags within that group. Final images,
depfiles, logs and receipts remain in their owning output directories.

A checkout-local pass-through workspace compiler wrapper prevents Cargo from reusing
checkout-dependent built-in manifest paths for another checkout. Existing explicit workspace
wrappers are chained. Non-member local path dependencies additionally require checkout remap
flags; the helper adds these conservatively because Cargo's workspace wrapper does not cover
them. Absolute remap differences, including sealed packaging, can rebuild dependencies across
checkouts; a shared directory alone does not promise a cache hit for incompatible flags.
The helper explicitly exports its selected compiler (`RUSTC`, then `CARGO_BUILD_RUSTC`, then
PATH); a compiler selected only by Cargo config must be supplied through that environment.
Clippy retains its own driver and receives a checkout remap argument.

`--snapshot` defaults `CARGO_INCREMENTAL=0` for disposable source copies. Omit it for a
continually edited checkout, where normal Cargo incremental behavior remains available.
Ordinary dev/test debug information and assertions are preserved. For a measured space tradeoff,
opt into `CARGO_PROFILE_DEV_DEBUG=line-tables-only` and
`CARGO_PROFILE_TEST_DEBUG=line-tables-only`; set either to `2` for full debug information.
A small cache regression fixture does not justify changing all development profiles.

For new ad-hoc checks, inherit these shared roots even when isolating HOME or XDG. Do not
copy a previous attempt's Go/Cargo caches beside evidence or install a new toolchain per run.
Cold-cache capacity checks and fixtures with a separate UID keep their intentional isolation.
Old frozen runners retain their recorded settings. Cargo owns concurrent build locking;
remove only completed run-owned final directories during ordinary evidence cleanup.
`cargo clean` can also remove shared intermediates, so it is not a final-output cleanup command.
Cargo's normal package-cache automatic GC remains enabled; compiled intermediates have no
new size limit or automatic eviction policy.

Debian 13 currently ships an older bootstrap Go than the Incus client requires. That is acceptable
for source development because Go follows the module's `toolchain` directive. Confirm the selected
compiler with `go version` after the first build.

Profiles whose services start after the guest agent should declare the optional
[`startup_readiness` hook](control-plane.md#profile-extensions) in `profile.json`.
Its read-only response must distinguish transient startup from stable repairable drift;
keep its implementation and tests in the owning profile. See the
[profile startup contract](control-plane.md#profile-startup-readiness).

## Pull requests

PR conventions live in the [development workflow](../.agents/skills/subyard-dev-flow/SKILL.md#pull-requests).
`dev/pr-stats.py` prints the PR statistics block; its host-free test is `bash tests/pr-stats.sh`.

Repository settings the workflow expects:

- a branch ruleset on the default branch and `release/**`: pull request required (0 approvals),
  required status checks `verify`, `compatibility` and `native-engine-arm64` without "require
  branches to be up to date", force pushes and deletions blocked, no bypass for the GitHub App;
- merge options: squash merging only, squash commit message "Pull request title", auto-merge
  allowed, head branches deleted after merge;
- on `release/**`, if required status checks block the agent from creating the branch, allow
  creation without checks.

Without required checks, `gh pr merge --auto` merges immediately.

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
targeted source-repair tests require `make build VERSION=0.28.0-test` first. A delivery gate may instead supply its
once-built normal runtime through `SUBYARD_SOURCE_QUERY_TOOLING_ROOT`.
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
acceptance defaults to host-free/local checks, and publication verifies the immutable release.
Long core/profile VM checks run only on explicit request (`accept --with-vms --slots N` or a
collector `--only` selection); unrequested checks stay `not-run`.
[Live external checks](reference/release-publication.md#external-checks) are optional. `yard update`
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

Profile provisioning keeps downloads and temporary files in operation-owned disk scratch under
`/var/lib/subyard/profile-operation-v4`. A private temporary mount catches direct
`/tmp`, `/var/tmp` and `/dev/shm` writes. One operation runs per guest;
completion removes scratch, and the next invocation removes leftovers with a valid
durable ownership receipt after SIGKILL or reboot. Incomplete initialization or
ambiguous ownership is preserved and refused for inspection.
The operation needs Linux mount/PID namespace support and verified disk storage.

Profile extraction uses the same lifecycle in guest disk storage under
`/var/lib/subyard/profile-operation-v4`, with one operation per guest. Controller stderr
is drained with a 64 KiB capture limit. An optional profile `provision-tmp-paths`
file lists exact existing temporary directories needed by migration hooks; symlinks
are refused. Other temporary files remain private. The original guest `/proc` stays
visible for guards that inspect existing workloads.
Version 4 receipts bind opened filesystem IDs and inodes across reboot; live descriptor/path
checks still bind device IDs and inodes. Filesystems without a usable filesystem ID refuse.
The previous v3 directory remains a coordination bridge: both versions share its lock, and an
exactly authenticated v3 receipt mirrors the worker's durable active-process identity. Its data
is never cleared by v4. A valid v3 receipt whose device identity changed is preserved unchanged;
v4 uses its independently owned directory without adopting the old data. Old helpers remain
usable when their original v3 ownership checks pass. No receipt is reset to repair ambiguous
ownership, and incomplete initialization remains a refusal.
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

Build those assets from the current public worktree with `dev/package-engine.sh`, using the
same version and output directory supplied to the verifier. Its pinned historical inputs are
self-contained; earlier temporary fixture directories are unnecessary. A pass on a published
baseline does not establish acceptance of a dirty or untracked current candidate.

Small contracts precede that full gate. `tests/agent-skills.sh` and `tests/release-candidate.sh`
cover delivery and the actual wrapper's strict transport. Native source-inventory parity and
prepared-source tests remain in `internal/adapters/reconcileruntime`;
`TestHistoryReadinessContract` in `internal/testkit/cmd/empty-incus` connects the actual public
readiness producer to its nonexecuting migration-history consumer. The package-local
`config/skills/package.py` owns Python inventory and canonical transport modes: directories
`0700`, regular files `0600`, independent of checkout, private installed or frozen `0500`
source modes. Native Go admission keeps its independent inventory.

`TestEvidenceOracleContract` in `internal/releasetransition` connects real POSIX writer cuts
to the Python compatibility oracle through a bounded process handshake. Published evidence
retains its complete identity. A captured `.pending` file may complete only at its allowed
journal step/checkpoint destination with its inode, mode and prefix preserved; successful
retry removes staging. The normal transition validator still proves the full record's content.
These local contracts complement the complete packaged compatibility gate below. Native ARM
acceptance remains a CI result; cross-building does not establish it.

This Linux check needs Python 3 and Go for its empty-instance Incus API fixture.
It runs checksum-pinned released callers through update and `--check` refusal below v0.27.0,
requiring the exact anchor command and unchanged settings, runtime links and data. Sources before
v0.11.0 retain staged full-uninstall and separate clean-install coverage. Use
`--only-source-anchor` or `--only-legacy-retirement` for the corresponding focused check;
publication requires the complete default invocation. Named `--*-baseline-dir` options reuse
verified official assets. All mutable fixtures remain under `/tmp`.

The old-source migration, rollback, original-owner recovery and migration-history process corpora
are frozen in the v0.27.0 tag. Current Go tests retain V2 journal, checkpoint, frozen projection,
evidence and recovery readers. Published v0.27.0 direct upgrades and the first-boundary-release
chains from v0.11.0 and v0.26.0 cover the installed transition to the candidate on disposable VMs.
The host-free verifier does not claim those physical contracts.

Before preparing fixtures, the verifier compares available `/tmp` bytes and inodes with the
selected archives' known unpacked lower bound. Unknown downloads, build overhead and total peak
remain explicit; passing that floor neither reserves capacity nor proves enough space.

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

The [legacy migration support policy](reference/legacy-migration-support.md) requires installed
v0.27.0 or newer for direct update and retained rollback. Sources from v0.11.0 to below v0.27.0
first run `yard update --version 0.27.0`, then plain `yard update`. Earlier sources retain the
full-uninstall route. Exact unfinished transitions retain their original owner and bindings.

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
