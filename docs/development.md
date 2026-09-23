# Development

Subyard's control-plane engine is a native Linux Go binary. Shell remains the system-adapter and
safety layer, so contributors need both the Go and shell toolchains.

## Toolchain

`go.mod` is authoritative: the module's minimum language baseline is Go 1.25.6 and its `toolchain`
directive selects Go 1.26.5 for development and CI. A recent Go bootstrap can fetch that toolchain
automatically. CI uses `actions/setup-go` with `go-version-file: go.mod`; it does not carry a second
version constant.

On Debian, install the normal build prerequisites with:

```sh
sudo apt-get update
sudo apt-get install -y golang-go gcc make shellcheck jq systemd
```

Inside a yard dedicated to Subyard development, enable the `subyard-dev` profile and run `yard
provision subyard-dev` from the owner host. The profile installs the Debian Go bootstrap and
ShellCheck in L1, while `go.mod` still selects the exact compiler and shared Go caches live under
`/srv/cache`.

Debian 13 currently ships an older bootstrap Go than the Incus client requires. That is acceptable
for source development because Go follows the module's `toolchain` directive. Confirm the selected
compiler with `go version` after the first build.

## Build and test

```sh
make build
./tests/run.sh
```

`make build` writes the ignored developer candidate `.build/yard` atomically. The source-tree
`bin/yard` launcher uses that explicit candidate and never compiles or downloads a toolchain at
runtime. Production does not use the source checkout:

```sh
curl -fsSL --proto '=https' --tlsv1.2 \
  https://github.com/Subyard/Subyard/releases/latest/download/subyard-install.sh | bash
exec "$SHELL" -l
```

This requires `curl`, `jq`, `sha256sum`, `tar`, and `gzip`; it asks once before changing the host,
links `~/.local/bin/{yard,sy}` to the verified runtime, and configures login PATH and completion.

`make package VERSION=<version>` writes amd64 or arm64 Linux engine artifacts and a complete
`subyard-<version>-linux-<arch>.tar.gz` runtime under `.build/release/`, each with a detached SHA-256,
compatibility manifest and provenance. Follow the
[dev-flow publication policy](../.agents/skills/subyard-dev-flow/SKILL.md#choose-checks-by-risk)
before pushing a `vMAJOR.MINOR.PATCH` tag; GitHub workflows do not have
access to that pool. The tag starts the independent Release workflow checks: host-free,
native Paseo, adapter and upgrade compatibility. The workflow publishes both architectures to a
tag-backed GitHub Release after they pass. Branch CI does not run for tag pushes. `yard update`
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

After activation or rollback and configuration refresh, `yard update` reinspects the exact selected
release without fetching another version. Its final human-readable summary reports readiness and
the active and previous releases, with blockers and next steps when needed. An unsuccessful final
check exits nonzero and records a failed update; `yard update --check` prints indented JSON; add `--json` for compact machine output.

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

This Linux check needs Python 3 and downloads checksum-pinned v0.9.1 and v0.11.2 runtimes for the
local architecture. It verifies the legacy standalone bridge as well as the frozen updater
contract, confines all state to a temporary directory and interrupts only its own update process
group. Use `--baseline-dir PATH` and `--legacy-baseline-dir PATH` to reuse downloaded official
assets (including the legacy runtime installer). Publication runs this check after building the
release assets.

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

## Delivery spike

The initial amd64 measurement on Debian 13 (2026-07-20, clean cache after compilation) produced a
3,031,202-byte statically linked core binary. `yard --list` cold start was 2–4 ms and an idle
`yard rpc --stdio` session reported about 4.1–4.3 MiB RSS with no observable background CPU activity. The
bounded official-Incus-client delivery spike linked to 14,074,018 bytes; that adapter is now the
production path for native status/inventory and typed RPC. These
figures are a regression baseline, not release limits; release evidence should repeat them on the
target host.

The switched engine was measured again in the same development class on 2026-07-21: the stripped
amd64 artifact was about 14.4 MB, 20 warm `yard --list` process samples had a 13.3 ms median
(7.2–22.5 ms range), and a negotiated idle stdio RPC process used about 11.9 MiB RSS with zero CPU
ticks over one second. The expected step from the core baseline is explained by the official Incus,
HTTP and WebSocket client graph now being linked into the sole production binary. RPC remains
request-driven with no background polling. Real snapshot latency is recorded separately on the
dedicated release host because it depends on a live container/VM and Incus socket.
