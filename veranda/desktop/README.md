# Veranda GTK desktop

This Linux shell links gtk4-rs and the existing native Rust client in one
GUI process. It displays the local owner and saved remote connections, host/yard
navigation, separate Host/Yard pages and a names-only project list with Code and
Shell actions. Selection preferences use the
client's private app-local store. Missing or unavailable saved selections fall back
to the local current yard. Saved remotes load on selection.

`Connect remote host` in the fleet navigator opens an inline workspace. Enter a
host or `user@host`, optionally `:port` (brackets for IPv6), and review the public
SSH fingerprint and native consequences. Explicit acceptance precedes SSH agent
authentication, exact product/RPC/capability negotiation, owner discovery and
registration. A changed key requires the separate selected-owner `Repair host
key` action and another public-fingerprint review. Failed compatibility, changed
owner identity or stale metadata cannot replace a registration. The desktop never
reads or writes CLI registrations or handles private keys, passwords or tokens.

Cancel revokes an unconsumed review or the provisional SSH child and prevents
registration when cancellation wins the native publication lock. If registration
has already committed, its confirmed result is displayed. Public-key assessment
uses the client's bounded SSH tools; cancellation waits for that assessment to
finish. One trust flow is active at a time; reply IDs and selection generations
fence late results. Native storage continues to enforce permissions/ACLs, atomic
writes, single-use consent, pin binding and stale baselines.

Refresh and native event hints trigger authoritative inventory reads. Event
payloads never replace inventory: a read invalidated while in flight is discarded
and refreshed, and returning to an owner refreshes its cached view. A failed read
remains explicit until Refresh or a native recovery hint. Navigation stays disabled
until initial selection restoration finishes. Reads and
preference writes and trust operations run on a bounded worker outside the GTK thread. Closing the
window calls native shutdown and waits off-thread for the worker and owned
engine/SSH cleanup before allowing GTK to close. No timer polls the owner while idle.

## Owner pages and operation review

Host tabs are Overview, Yards, Sync, Diagnostics and Settings. Yard tabs are
Overview, Projects, Profiles, Diagnostics and Settings. Shell, copied shell
commands, project Code/Shell and host htop use native typed session descriptors.
Missing htop returns an owner error; the ordinary host Shell remains available.
Start, Stop, Reconcile and reported-preset named-yard creation prepare owner plans.
Creation targets an existing owner session; selecting an unknown yard never creates it.

Profiles show effective `ENVIRONMENT_PROFILES` and layer provenance separately
from observed convergence: `current` is Applied, `changes-required` is Changes
required, `unknown` is Unavailable and `not-applicable` is N/A. Checkboxes edit a
nonsecret draft; Review selection prepares a config plan. Provision is a separate
reviewed operation. The current RPC projection has no profile descriptions or
separate desired-selection DTO, so those fields are absent. Settings display typed
availability, effective/default values, allowed scopes and provenance. Public
editable scalar values have scoped Set/Unset review; protected path/file/executable
values and credentials are excluded. Profile selection has its own surface.

Host Sync has separate cached configuration Git and REDACTED credential sections.
Credential-free repository URLs prepare Connect Git plans; Pull, Push and Sync now
use the owner's existing config/keys exact plans. Reads never fetch Git. Credential
add/import/rotate and secret payloads are outside the desktop interface.

An operation review displays the owner's summary, confirmation policy, expiry,
consequences and exact steps including dependencies and verification. Execute
consumes the native plan once; Discard releases it. Cancellation waits for an owner
outcome. Connection loss or interrupted execution remains Unknown, never success.
Definite pre-admission execution rejection is typed as Not submitted and releases
its interrupted-intent record; admitted work with transport loss remains Unknown.
The operation panel survives navigation. Review plans and trust capabilities are
never persisted or reused after restart.

Scoped nonsecret drafts and interrupted operation identifiers use a separate
bounded private `desktop.json`. Native permission/ACL checks, atomic replacement
and a stale baseline prevent concurrent overwrite. Storage failure disables new
mutations until reopen. Unknown operation history is retained; restart requires an
authoritative refresh and a fresh plan. Draft text uses GTK text widgets, including
multiline and Unicode values, and survives tab/scope changes, Refresh and restart.
Same-selection background reads retain editor widgets and buffers while updating
authoritative labels and mutation gates in place. Deliberate navigation may rebuild
forms. A removed focused setting stays visibly Unavailable until focus leaves.
Reads, preparation, execution, cancellation, launch and draft writes run off GTK;
pending read/detail/state requests coalesce and owner commands have bounded admission.

## Build and focused checks

Use Rust 1.88 or newer, `pkg-config`, and system GTK4 development files
(`libgtk-4-dev` on Debian/Ubuntu). The Rust bindings use GTK's baseline APIs,
including Ubuntu 24.04's GTK4.14. Build output stays outside the source directory:

```sh
CARGO_TARGET_DIR=.build/veranda-desktop cargo test --manifest-path veranda/desktop/Cargo.toml --locked
CARGO_TARGET_DIR=.build/veranda-desktop cargo build --manifest-path veranda/desktop/Cargo.toml --locked
```

The selection/error model and worker can also compile without GTK development files:

```sh
CARGO_TARGET_DIR=.build/veranda-desktop cargo test --manifest-path veranda/desktop/Cargo.toml --locked --no-default-features
```

For a source-development launch, build the matching engine, then put the source
launcher on PATH. Both debug defaults identify the same development version:

```sh
dev/build-engine.sh
PATH="$PWD/bin:$PATH" .build/veranda-desktop/debug/veranda
```

An explicit stable `VERANDA_PRODUCT_VERSION` at GUI build time must match the
engine's `YARD_BUILD_VERSION`. Incompatible versions/capabilities remain errors;
the shell does not parse CLI output or provide a compatibility bypass. App data
uses `$XDG_DATA_HOME/subyard/veranda`, falling back to
`$HOME/.local/share/subyard/veranda`.

## Disposable Wayland SHORT

`wayland-short.py GUI_ELF ENGINE_ELF` runs only as the unprivileged user inside an
allocated disposable VM. It reuses public lease-fixture, typed owner-inventory,
process-identity and cleanup helpers. It installs VM-only dependencies, prepares
20 configured **NOT_CREATED** yards with 200 actual project records, then runs a
same-build GUI lifetime on labwc headless/Pixman. AT-SPI only reads accessibility;
`wtype` delivers physical Refresh, Projects selection and close. A new inventory
observation and ten visible project names are checked before natural GUI close,
observed child death and fixture cleanup. Configured records do not establish
container start/provision lifecycle. A captured screenshot is separate from any
latency or resource measurement.

Use the existing [candidate-bundle transport](../../docs/test-vms.md) to include
prebuilt ELFs with the current public source. For example, after the builds above:

```sh
python3 - <<'PY'
import subprocess, tarfile
paths = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard']).decode().split('\0')
with tarfile.open('.build/veranda-gtk-short.tar.gz', 'w:gz') as archive:
    for path in sorted(set(paths) - {''}):
        archive.add(path, arcname=path, recursive=False)
    archive.add('.build/veranda-desktop/debug/veranda', arcname='.subyard-acceptance/veranda', recursive=False)
    archive.add('.build/yard', arcname='.subyard-acceptance/yard', recursive=False)
PY
SUBYARD_E2E_CANDIDATE_BUNDLE="$PWD/.build/veranda-gtk-short.tar.gz" \
SUBYARD_E2E_CANDIDATE_SHA256="$(sha256sum .build/veranda-gtk-short.tar.gz | cut -d' ' -f1)" \
dev/agent-e2e.sh --slot 1 --vm-count 1 --purpose veranda-gtk-short -- \
  python3 veranda/desktop/wayland-short.py .subyard-acceptance/veranda .subyard-acceptance/yard
```

Choose an available owned slot using the VM guide; the example does not authorize
another holder's slot. Keep the original exit, full logs, cleanup and exact release
outcomes. Add `--remote` to run three same-build lifetimes against an isolated real loopback
SSH server and system SSH agent with synthetic keys on the disposable VM. This
checks first-key decline/accept, compatible owner discovery, cancellation during
an authenticated but blocked RPC negotiation, remote Refresh, saved owner/yard/tab
restoration, local selection restoration and a separate replacement-key Repair.
It checks private metadata and natural death of observed GUI/engine/SSH identities.
The SSH fixture routes to a separate configured owner environment on the same VM;
it establishes actual SSH/RPC use, not inter-host network recovery. Every dependent
stage stops on its first failure. Trust and connected-fleet screenshots are UI
evidence only. Add `--ui` for focused keyboard-driven typed Profiles plan/discard, Host Sync
metadata, Unicode named-yard draft and draft restoration checks on the same
remote candidate. These remain tests to run, not native proof from unit checks.
The helper does not measure resource/latency budgets, create running
yards or establish full UX/IME/platform/package behavior.
The client does not retain each engine/SSH child's original wait status; observed
identity death after app-directed shutdown must not be reported as a child exit0.

## Linux GTK package preparation

`python3 dev/package-veranda.py --version MAJOR.MINOR.PATCH` builds a separate
`subyard-veranda_<version>_<arch>.deb` with Rust 1.88.0, locked dependencies and an
optimized, stripped native artifact. The required stable version is the shared
Yard/Veranda product version, passed to the native client through
`VERANDA_PRODUCT_VERSION`; internal Cargo package versions remain unchanged.
This builder leaves core bootstrap, runtime packaging and CLI updates independent.

Native Linux amd64/arm64 preparation requires the Rust toolchain, GTK4 development
SDK, pkg-config, Debian packaging tools, desktop-file-utils and AppStream. Optional
`--sdk-root` and `--sdk-debs` take an extracted published Debian SDK and its original
DEBs; dependency derivation uses their real symbol/shlibs metadata without installing
packages. Missing metadata stops preparation. No private patched dependencies are used.
Outputs and Cargo target cache default to `.build/veranda-package/`.
Output paths are checked canonically before writes; parent traversal and symlink
aliases are rejected. Owning Rust source/manifest/notice paths must have ordinary
parents, and actual Cargo dep-info must refer to inventoried public inputs or
generated outputs inside the selected target cache. Ignored source inputs cannot
silently escape the provenance inventory.

The DEB contains only the GUI, `org.subyard.Veranda` desktop/icon/AppStream assets,
root MIT license, actual resolved Rust dependency notices and sanitized build metadata.
Files/directories have fixed modes and root package ownership. System shared-library
dependencies come from dpkg-shlibdeps; openssh-client supplies the system SSH transport
and agent tools. GTK/GLib and their transitive OS packages are system dependencies,
excluded from the GUI payload. Yard/Incus/runtime are not bundled. Local owners need
Yard installed separately; remote owners need the exact matching product release and
an existing system SSH agent. VS Code, supported external terminals and host htop are
optional external session tools; native availability checks report missing tools.

Detached SHA256, manifest and provenance bind the DEB/ELF, version, dependencies,
locked build metadata and actual public source paths/bytes/modes, including dirty and
untracked inputs. They contain no environment dump, private files or cache absolute
paths. `python3 dev/check-veranda-package.py <deb> --runtime-version` validates the
real package without installing, including headless `--version` without GTK/owner/store
initialization; native shared libraries must be available for that optional check.
Manifest metrics separate compressed GUI bytes, regular-file payload bytes and Debian
Installed-Size from engine/system dependencies. Clean-machine dependency download/disk
costs require actual install evidence; a package build supplies no such measurement.

`dev/e2e/veranda-package.sh <baseline.deb> <candidate.deb>` is an allocated-VM-only
install/upgrade/rollback helper. It accepts validated ascending stable versions, checks
CLI bytes and `$XDG_DATA_HOME/subyard/veranda` preservation, accounts for incremental
OS dependencies and observes actual GTK accessibility readiness plus natural close and
observed owned-child cleanup. Its readiness outcome may be a fleet or actionable owner
error; it does not establish complete owner/session/IME/resource acceptance. Native
clean-machine lifecycle, crash/recovery, platform, signing and release gates remain open.
The normal apt install/upgrade/rollback sequence and dependency cost snapshots finish
before any GUI test tools are installed, so GTK libraries also used by Openbox/GI/AT-SPI
remain ordinary GUI costs. Metrics distinguish cumulative dependencies since that clean
baseline, newly added packages per phase and the phase's OS installed-size change;
rollback may retain upgrade dependencies. Fixture-only tools are then installed for a
second install/upgrade/rollback sequence that checks physical GTK launch/close. Its
warm-cache apt logs are separate and cannot stand in for clean-machine download costs.
