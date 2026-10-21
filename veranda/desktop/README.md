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

Profiles show requested and effective `ENVIRONMENT_PROFILES`, selection presence,
owner reasons, source groups and layer provenance separately
from observed convergence: `current` is Applied, `changes-required` is Changes
required, `unknown` is Unavailable and `not-applicable` is N/A. Checkboxes edit a
nonsecret draft; Review selection prepares a config plan. Provision is a separate
reviewed operation. Ordered whole-list drafts preserve hidden names, duplicates and
explicit empty selections across refresh and restart. Profile descriptions are not
provided by the owner. Settings display typed
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

Use current stable Rust selected by [`rust-toolchain.toml`](../../rust-toolchain.toml),
`pkg-config`, and system GTK4 development files
(`libgtk-4-dev` on Debian/Ubuntu). The Rust bindings use GTK's baseline APIs,
including Ubuntu 24.04's GTK4.14. Build output stays outside the source directory:

```sh
CARGO_TARGET_DIR=.build/veranda-desktop cargo test --manifest-path veranda/desktop/Cargo.toml --locked
CARGO_TARGET_DIR=.build/veranda-desktop cargo build --manifest-path veranda/desktop/Cargo.toml --locked
```

Default tests run without a display. The navigator and opt-in paint adapter widget
regressions are explicitly ignored there and run separately on Linux with `xvfb`
and `xauth` installed:

```sh
fixture_root="$(mktemp -d /tmp/veranda-gtk-widgets.XXXXXX)"
chmod 0700 "$fixture_root"
trap 'rm -rf -- "$fixture_root"' EXIT
install -d -m 0700 "$fixture_root/runtime"
GDK_BACKEND=x11 GSK_RENDERER=cairo TMPDIR="$fixture_root" \
  XDG_RUNTIME_DIR="$fixture_root/runtime" xvfb-run -a -s '-screen 0 1280x800x24 -nolisten tcp' \
  cargo test --manifest-path veranda/desktop/Cargo.toml --locked --bin veranda \
  navigation::tests::rebuilt_selection_is_visible_without_stealing_or_stale_callbacks \
  -- --ignored --exact --test-threads=1
GDK_BACKEND=x11 GSK_RENDERER=cairo TMPDIR="$fixture_root" \
  XDG_RUNTIME_DIR="$fixture_root/runtime" xvfb-run -a -s '-screen 0 1280x800x24 -nolisten tcp' \
  cargo test --manifest-path veranda/desktop/Cargo.toml --locked --lib \
  paint::tests::adapter_tracks_real_child_frames_and_cancels_owned_lifetimes \
  -- --ignored --exact --test-threads=1
```

This private display and Cairo renderer belong only to the widget check; resource
measurements use their own renderer and lifecycle policy. CI runs both the default
headless tests and these explicit widget checks. The paint adapter accepts a
complete represented batch for each caller-authorized revision; callers must merge
still-covered original starts before applying a newer revision. It records actual
snapshot plus matching GTK after-paint, not compositor presentation. The application
does not enable this instrumentation or write measurement files by default.

For an explicitly observed run, set `SUBYARD_VERANDA_TRACE` to a new absolute
output filename in an existing directory owned by the current user with exact
mode `0700`. Every ancestor must be a real directory, with no symlinks or `..`
components. The Linux sink creates a new regular file with exact mode `0600`;
it never replaces an existing output. No parent directories are created.
`--version` returns before reading this variable or initializing observation,
GTK, the store or owner connections.

The run owns one observation context across activations, with distinct opaque
view ordinals. After `app.run` returns, the ledger seals atomically, retires all
pending starts as UNKNOWN, and writes only bounded scalar counts, receipt/part
cohorts and frame timings. Subsequent callbacks and drops cannot change that
snapshot. Actual matching Worker `Closed` updates are counted separately from
activations. They do not prove detached monitor/read/execute producers drained:
producer quiescence remains UNKNOWN. Zero, partial or unsuccessful run lifecycles
are UNKNOWN. The pending/phase/terminal bounds remain 2048/8192/8192; the encoded
file has a separate 8 MiB limit, not a completeness guarantee.

Missing, unsafe, incomplete or failed output is UNKNOWN for measurement consumers.
An opt-in write failure emits only `Veranda trace: UNKNOWN`, without the output
path or underlying error; consumers must retain that outcome with the file. This
instrumentation does not change the application exit status or owner cleanup.
Snapshot plus matching after-paint is measured, not compositor presentation.

For raw owning-child accounting, set `SUBYARD_VERANDA_OBSERVATION_CONFIG` with
the trace output to a private schema `3` configuration:
`{"schema":3,"child_usage":{"capacity":8192},"correlation":null}`.
The existing private-FD rules apply. This is mutually exclusive with
`SUBYARD_VERANDA_CORRELATION_CONFIG`; an optional `correlation` object contains
the existing offers, cohorts and ready destination without an inner schema.
The requested schema `3` trace retains bounded raw kernel rusage and owning
spawn/wait metadata. Checked interval starts round down and terminal endpoints
round up; missing, unsupported, dropped or unfinished observations remain
UNKNOWN. Rust-relative and Python monotonic clocks are separate domains.
No mixed-clock peak, descendant closure, resource budget or savings is implied.
The default unset path and plain schema `1`/correlation schema `2` stay separate.

For fixture event correlation, also set `SUBYARD_VERANDA_CORRELATION_CONFIG` to
a private immutable JSON file before starting the GUI. It uses schema `2` with
only `schema`, `offers`, `cohorts` and `ready`: offers contain `offer`, `action`
and the exact actual source URI; cohorts contain an opaque `ordinal`, expected
`owner`, optional `connection` and optional `yard`. The readiness destination is
an absent absolute leaf in an existing owned `0700` directory. Configuration is
bounded to 512 KiB, 2048 unique offers and 64 unique cohort targets, and must be
an owned single-link regular `0600` file with no symlink ancestors. Private
identities and URI mappings are never exported. The variable is ignored without
the trace opt-in; `--version` precedes both environment lookups.

Successful current typed reads bind actual context/source ordinals through the
existing View guards. Cache identifies a source without claiming an RPC or event
coverage. After every expected role is bound, one owned background thread freezes
the table and atomically publishes a scalar acknowledgement without replacing an
existing leaf. Stale, unsafe or failed publication remains UNKNOWN. After app.run,
the run finishes that thread, seals observation and writes schema `2`, including
the independent writer outcome, Cold/Warming/Frozen phase, scalar bindings and
the original same-clock freeze time. The plain trace remains schema `1`.
Owner contradictions and later source allocation make final qualification
UNKNOWN; source ordinals are not logical Owner identities. Readiness establishes
neither complete event coverage nor paint, resource or latency acceptance.

The selection/error model and worker can also compile without GTK development files:

```sh
CARGO_TARGET_DIR=.build/veranda-desktop cargo test --manifest-path veranda/desktop/Cargo.toml --locked --no-default-features
```

For a source-development launch, build the matching engine, then put the source
launcher on PATH. Both debug defaults identify the same development version:

```sh
make build
PATH="$PWD/bin:$PATH" .build/veranda-desktop/debug/veranda
```

An explicit stable `VERANDA_PRODUCT_VERSION` at GUI build time must match the
engine's `YARD_BUILD_VERSION`. Incompatible versions/capabilities remain errors;
the shell does not parse CLI output or provide a compatibility bypass. App data
uses `$XDG_DATA_HOME/subyard/veranda`, falling back to
`$HOME/.local/share/subyard/veranda`.

## Disposable X11 and Wayland SHORT

`wayland-short.py GUI_ELF ENGINE_ELF` runs only as the unprivileged user inside an
allocated disposable VM. It reuses public lease-fixture, typed owner-inventory,
process-identity and cleanup helpers. It installs VM-only dependencies, prepares
20 configured **NOT_CREATED** yards with 200 actual project records, then runs a
same-build GUI lifetime. X11 is the primary operating scenario: add `--x11` for a
private Xvfb display with Openbox and XTEST input through `xdotool`. The existing
default runs Wayland on labwc headless/Pixman with `wtype`; both backends require
their own native result. AT-SPI only reads accessibility. Physical Refresh,
Projects selection and close are checked. A new inventory
observation and ten visible project names are checked before natural GUI close,
observed child death and fixture cleanup. Configured records do not establish
container start/provision lifecycle. A captured screenshot is separate from any
latency or resource measurement.
The X11 driver verifies that its one visible window belongs to the exact owned
GUI PID before activation and each key or text event, and verifies the active
window again after activation. It does not use
accessibility actions or synthetic window-directed key events. Both X11 and Wayland support `--remote` and `--remote --ui`;
text uses their native keyboard injection tools. Each result identifies its backend, screenshot, original
GUI exit and observed process identities.
Add `--trace-observations` for a separate per-lifecycle sealed trace from the
actual GUI. The helper gives only that GUI a unique absent output filename in
its owned private directory; drivers, services and SSH fixtures remain unobserved.
Collection occurs after the original successful natural close, before fixture
state removal. The result retains traces separately from logs/screenshots and
checks the scalar schema, ownership/modes and the fixed UNKNOWN-output marker.
At least one original GUI request must have accepted whole coverage and an actual
snapshot/matching-after-paint terminal. Retired UNKNOWN starts remain visible;
producer quiescence UNKNOWN is not a cleanup failure. This opt-in establishes
neither event latency nor p95/resource acceptance. Missing, unsafe, malformed,
duplicate or unproven-paint output fails trace evidence independently of input
and GUI outcomes; the original workload and close deadlines do not change.
Add `--child-usage` with `--trace-observations` to request schema `3` and record
the actual GUI leader through the same owning Python capture/wait boundary.
Services, driver and compositor are outside that ledger. This is raw accounting,
not family/resource acceptance. The normal resource measurement loop is not
connected to this new trace mode yet.

With `--trace-observations`, `--correlation-config FILE` accepts a private schema2
template containing only `schema`, `offers` and `cohorts`. The helper authors a
separate private runtime configuration and absent ready/trace leaves for each GUI
lifetime; it does not forward the configuration to drivers or services. Its
bounded `wait_ready` handshake can gate a future producer, but this helper does
not launch a burst. A matching final context, freeze table and successful writer
outcome are still mandatory after natural GUI exit: a visible acknowledgement
alone cannot qualify a run, including a later publication/stop failure.
Connect requires the actual owned panel title and unique focused editable destination
before physical text and Return. Editor continuity compares the same visible typed
editor's text, caret and selection while Refresh has focus, before any return-focus
navigation: [GTK keyboard refocus can select all entry text](https://github.com/GNOME/gtk/blob/4.18.6/gtk/gtktext.c#L3229-L3249).

For a focused local diagnosis, use `wayland-short.py GUI ENGINE --x11
--profile-diagnostic --prepared-assets ASSETS` on an allocated disposable VM.
This diagnostic runs one configured-fleet GTK lifetime through the same physical
Profiles → Review profile selection → Discard plan sequence as `--remote --ui`,
then closes naturally. It excludes SSH onboarding and restoration and establishes
no acceptance or timing result. Its local workload budget stays 90 seconds.
The existing full remote UI workload and its 600-second per-lifetime budget are
unchanged. The input driver keeps at most 512 cached progress records on stdout
and atomically replaces one private checkpoint of at most 1024 bytes afterward.
The watchdog retains that last checkpoint before attempting a screenshot;
checkpoint, screenshot and primary timeout outcomes remain separate. Neither
progress writer performs accessibility queries or retains UI text or typed data.
X11 watchdog capture requires exactly one visible window of the owned GUI PID;
hidden auxiliary windows do not count toward that visible singleton.

Use the existing [candidate-bundle transport](../../docs/test-vms.md) to include
prebuilt ELFs with the current public source. This example uses synthetic product
version `0.99.1`, which satisfies the installed-source version floor; it does not
publish a release. Stage independent executable copies for the fixture:

```sh
VERANDA_PRODUCT_VERSION=0.99.1 CARGO_TARGET_DIR=.build/veranda-desktop \
  cargo +stable build --manifest-path veranda/desktop/Cargo.toml --locked
YARD_BUILD_VERSION=0.99.1 dev/build-engine.sh --force
install -D -m0755 .build/veranda-desktop/debug/veranda .build/veranda-short-inputs/veranda
install -D -m0755 .build/yard .build/veranda-short-inputs/yard
python3 - <<'PY'
import subprocess, tarfile
paths = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard']).decode().split('\0')
with tarfile.open('.build/veranda-gtk-short.tar.gz', 'w:gz') as archive:
    for path in sorted(set(paths) - {''}):
        archive.add(path, arcname=path, recursive=False)
    archive.add('.build/veranda-short-inputs/veranda', arcname='.subyard-acceptance/veranda', recursive=False)
    archive.add('.build/veranda-short-inputs/yard', arcname='.subyard-acceptance/yard', recursive=False)
PY
SUBYARD_E2E_CANDIDATE_BUNDLE="$PWD/.build/veranda-gtk-short.tar.gz" \
SUBYARD_E2E_CANDIDATE_SHA256="$(sha256sum .build/veranda-gtk-short.tar.gz | cut -d' ' -f1)" \
dev/agent-e2e.sh --slot 1 --vm-count 1 --purpose veranda-gtk-short -- \
  bash -euc '
root="$PWD"
operator="$(id -un)"
operator_home="$(getent passwd "$operator" | cut -d: -f6)"
bash dev/build-profiles.sh
. tests/helpers/test-context.sh
setup_test_context "$root/.build/incus-bootstrap"
export SUBYARD_USER="$operator" SUBYARD_OPERATOR_HOME="$operator_home"
export STORAGE_PATH="$operator_home/.cache/subyard-e2e-platform/incus/incus/storage"
SUBYARD_INSTALLATION_CONTEXT="$(test_installation_context default)"
export SUBYARD_INSTALLATION_CONTEXT
bash scripts/01-install-incus.sh --yes --zabbly
sudo -n runuser -u "$operator" -- env -i \
  HOME="$operator_home" USER="$operator" LOGNAME="$operator" PATH=/usr/local/bin:/usr/bin:/bin TMPDIR=/tmp \
  SUBYARD_E2E_VM=1 SUBYARD_E2E_RUN_ID="${SUBYARD_E2E_RUN_ID:-}" SUBYARD_E2E_SLOT="${SUBYARD_E2E_SLOT:-}" \
  python3 "$root/veranda/desktop/wayland-short.py" \
    "$root/.subyard-acceptance/veranda" "$root/.subyard-acceptance/yard" --x11
'
```

Choose an available owned slot using the VM guide; the example does not authorize
another holder's slot. Keep the original exit, full logs, cleanup and exact release
outcomes. In the disposable guest, the product Phase 1 installer prepares Incus
and enrolls the operator before the whole helper starts in a fresh group session.
This follows the [existing Phase 1 caller](../../dev/e2e/entity-naming-acceptance.sh)
and [VM guide](../../docs/test-vms.md); running the helper directly on the raw base
can fail with Incus socket permission denied. Remove `--x11` to run the separate
Wayland SHORT with the same input ELFs. Platform preparation packages the same
candidate Yard, installs it under
the disposable fixture state and runs its supported `migrate --yes` boundary before
source `init`; Go and the source packaging prerequisites must be available in the VM.
The installed engine must match the supplied engine bytes. The fixture removes its
isolated runtime with the rest of its owned state.
Add `--remote` to run three same-build lifetimes against an isolated real loopback
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
Each input lifetime has a fixed functional workload watchdog: 90 seconds locally,
360 seconds with `--remote`, and 600 seconds with `--remote --ui`. The expanded
SSH/UI scenarios include many guarded observations and physical steps. These are
test completion bounds, not product responsiveness limits; individual read, wait,
key and natural-close deadlines remain separate and unchanged.
The client does not retain each engine/SSH child's original wait status; observed
identity death after app-directed shutdown must not be reported as a child exit0.

For repeated fresh VM leases, `--prepared-assets DIRECTORY` accepts an explicitly
staged private directory instead of rebuilding the runtime and downloading the
base image for every lifetime. Its `prepared-assets.json` binds the runtime
bundle, checksum, manifest and provenance to the supplied engine version/SHA and
staged compiled helpers, plus the exported `images:debian/13` container's full
fingerprint, architecture and image parts. Every listed file must be an owned
independent regular file with mode `0600`; directories must be plain `0700`.
Missing, additional, symlinked or changed inputs fail before installation.
The default source-packaging path remains available when the option is absent.

Prepared assets still pass through the normal runtime installer, installed-engine
identity check, migration/readiness check and canonical yard init/teardown. The
fixture imports only a previously absent image into the allocated guest, serves
that one owned public image through the guest Incus daemon on HTTPS loopback,
and pins both primary and fallback image references to its full fingerprint.
The remote uses the daemon certificate read through the local API, an exclusive
client certificate pin and an independent TLS identity check. Existing listeners,
image/alias collisions and remote collisions are rejected. Exact owned remote,
listener, pin and image cleanup follows product teardown; unrelated client
settings and projects are preserved. Installed CLI help, actual import/export,
TLS routing through the canonical project and native cleanup require a leased
VM result; host-free artifact contracts do not establish those properties.

## Linux GTK ordinary resource baseline

The existing resource sampler has an explicit `--gtk` path for the current desktop.
Its owning VM helper keeps the authentic same-candidate installed release preparation
above, including installed-engine byte equality and migration readiness. Use a fresh
8 GiB `android-test` VM for each backend, canonical product Incus Phase1 preparation,
and a fresh unprivileged group session for the **whole helper**, as in the SHORT recipe.
First run the one-launch prerequisite (explicit staged package and current engine):

```sh
python3 dev/e2e/veranda-wayland-resources.py /absolute/candidate.deb /absolute/yard --gtk --x11 --short
```

After its review, omit `--short` for 30 fresh app starts, then 30 seconds of settling
and 120 seconds idle in each acknowledged visible/minimized state. For Wayland omit
`--x11`; use another fresh VM, rather than inheriting the first platform teardown.
The one-launch result is `incomplete` for resource acceptance, even when its native
prerequisite succeeds. Over-limit complete measurements fail after retaining metrics.
Targets and hard limits follow the [resource budgets](../../docs/reference/control-plane-architecture.md#veranda-resource-budgets).

Current GTK readiness observes exact PID/UID/start/executable and a complete bounded
owned AT-SPI tree: Connected, the actual default body heading and selected navigation
row, 20 configured yard rows, the default 10-project Overview summary and enabled Refresh.
Incomplete trees are discarded and retried under one startup deadline; foreign nodes,
ambiguous apps and tree limits fail. A healthy configured owner cannot pass with an error
screen. The 200 loaded projects are bound to real `owner.inventory` preflight and the
existing typed client conversion; this does **not** claim 200 simultaneously rendered
project widgets, or the ten project-name widgets hidden behind Projects. The physical
SHORT owns that separate names/navigation check.

Startup means the first controllable accessibility fleet boundary, **not paint**.
Read-only AT-SPI/proc observation and command latency add measured startup overhead and
sampling gaps; observer/compositor processes are outside app sums, while GUI work answering
accessibility requests is included. RSS/PSS/CPU retain simultaneous GUI/local-Go/owned-SSH
accounting; current mean CPU includes the final post-state-query sample before its matching
elapsed endpoint, and sampled idle maxima include baseline/final endpoints. Historical
receipts retain their original accounting. Peaks are sampled maxima, not continuous
peak proof. Missing/partial metrics
cannot PASS. The sampler closes its observed app tree through owned cleanup after each
launch; natural close remains a separate SHORT assertion.

Add `--child-usage` to request per-launch schema `3` app traces and the owning
Python GUI-child usage record. Optional `--correlation-config /absolute/private.json`
also performs the existing one-shot live readiness handshake after warmup, within
the original readiness deadline; it starts no event producer. The resource loop
uses the same private collector and strict validators as SHORT. Each launch gets
a separate initially absent trace destination. Collection follows an owned-window
natural close request and the original wait; refusal or timeout retains the
existing process cleanup fallback and reports trace `UNKNOWN`. Valid partial raw
records survive incomplete direct-child coverage, with the requested measurement
still `UNKNOWN` and the original GUI result recorded separately.

Schema `3` may also record the naturally mapped GTK native renderer and surface as
fixed Cairo/GL/Vulkan/Other/Unknown and Wayland/X11/Unknown enums. Missing observations
remain unknown. These class observations do not establish hardware acceleration.
Rust-relative and Python monotonic intervals remain separate coordinates; the raw
records establish no combined family peak or resource-budget result.

The fixture uses a private bus and owned Xvfb/Openbox at 1920×1080, or labwc/Pixman at
1920×1080@60 Hz, scale 1. X11 refresh rate is unmeasured. Resource children use the shipping GTK default/auto renderer with `GSK_RENDERER` unset,
recorded separately from the known compositor renderer and runtime packages. [`GSK_DEBUG=renderer`](https://sources.debian.org/src/gtk4/4.18.6%2Bds-2/gsk/gskrenderer.c/)
logs successful renderer/surface class pairs privately; per-launch
metrics retain distinct successful surfaces and report unknown when main-surface evidence
is missing/ambiguous/truncated. This instrumentation adds disclosed startup overhead and
does not prove hardware acceleration. Functional SHORT's explicit
Cairo selection remains separate. Window state is acknowledged
through exact-PID Openbox EWMH or singleton labwc foreign-toplevel state, not inferred from
sleep. No AT-SPI action/focus/text mutation is used. The historical explicit-binary
marker/WebKit research path remains available without `--gtk`, labelled separately.
Stress/event bursts, continuous refresh peaks, switch/snapshot/event-to-paint, recovery,
inactive/occluded states and package lifecycle are separate unmet gates; this ordinary
baseline does not promote them to PASS.

## Local GTK IBus composition probe

For a focused Wayland navigation diagnostic, the SHORT also accepts
`--remote --owner-focus` with the normal GUI, engine and `--prepared-assets` inputs.
It uses real SSH onboarding and product trust storage, saves Local/default/Projects,
closes naturally, then restores that context and navigates to the saved remote owner.
These two lifetimes are diagnostic only. After a strict focus rejection it records
at most one fresh observation within the same remaining observation deadline,
sends no further input, and retains the original failure even if focus converges.
Focused role/PID and actual parent relations are recorded as bounded opaque scalars;
missing relations remain unknown. The local `--owner-focus` mode is unchanged.

The SHORT's optional `--ime` mode is separate from `--remote --ui`: it runs two
local GUI lifetimes against the same genuine owner, without repeating SSH flows.
It adds `libglib2.0-bin` (the `gsettings` provider), `ibus`, `ibus-hangul`,
`ibus-gtk4` and `fonts-noto-cjk` only in this mode.
The fixture owns a foreground IBus daemon on a private address and bus, with
private HOME/XDG and keyfile settings; it records installed package/input hashes,
including the `gsettings` executable,
selects the `hangul` engine and verifies the two-set, initial-Latin, syllable-preedit
settings by readback. `GTK_IM_MODULE=ibus` explicitly tests the GTK module, including
on Wayland; it does not establish compositor-native input-method support.

In the actual local owner's Yard name editor, physical Shift+Space, `r`, `k`,
Shift+Space should compose ㄱ → 가 and commit U+AC00. No Hangul text injection,
clipboard, accessibility action or owner operation supplies that syllable.
Committed AT-SPI text must remain empty and the exact local-owner draft empty
or absent during both preedit capture stages; after commit both must equal U+AC00. The GUI
closes naturally and a second process must restore the same scoped draft/text.
The fixture captures `ime-preedit-consonant.png`, `ime-preedit-syllable.png` and
`ime-committed.png`; those are observations for separate native inspection.
GTK 4.18.6 keeps [preedit and committed text separate](https://github.com/GNOME/gtk/blob/4.18.6/gtk/gtktext.c#L4192-L4227); the [Entry AT-SPI getter](https://github.com/GNOME/gtk/blob/4.18.6/gtk/a11y/gtkatspitext.c#L486-L509) reads committed text.
Logical text/draft equality does not prove visible preedit, fonts or painted pixels.
Existing lifecycle ownership, physical-input guards and 90-second local input
budgets remain in force; actual package/schema/module and keyboard composition
compatibility still require native validation.

## Linux GTK package preparation

`python3 dev/package-veranda.py --version MAJOR.MINOR.PATCH` builds a separate
`subyard-veranda_<version>_<arch>.deb` with current stable Rust, locked dependencies and an
optimized, stripped native artifact. The required stable version is the shared
Yard/Veranda product version, passed to the native client through
`VERANDA_PRODUCT_VERSION`; internal Cargo package versions remain unchanged.
The builder explicitly selects `+stable` regardless of its working directory or
`RUSTUP_TOOLCHAIN` override and records the actual compiler version in package provenance.
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

For a separate fresh-VM Wayland package lifecycle, use the optional prefix
`dev/e2e/veranda-package.sh --wayland <baseline.deb> <candidate.deb>`; omitting it
retains X11. Both functional GTK paths force Cairo. The Wayland compositor is
owned headless Labwc/Pixman with Xwayland disabled, initially empty before each
launch; window attribution is subsequent singleton `org.subyard.Veranda` app-ID,
not exact PID identity (the protocol has no PID query). These checks do not supply
default-renderer/resource or connected-owner acceptance.
