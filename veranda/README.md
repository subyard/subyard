# Subyard Veranda

Veranda is Subyard's Tauri 2 desktop client. Its fleet contains owner hosts and yards;
projects appear inside the selected yard. A thin Rust boundary owns local/SSH transport,
app-local connections and pinned host keys. The Svelte frontend receives bounded typed
facts, redacted credential metadata and exact owner plans. It has no arbitrary shell or
filesystem plugin.

Linux supports local and remote owner hosts. Windows and macOS are remote clients for a
Linux owner host: the client machine does not need Go, Incus, the Yard engine or a privileged
helper. Remote sessions use the system SSH client and an existing system SSH agent; private
keys, passphrases and tokens do not enter frontend state.

Node.js and npm are development/build tools only. The installed application contains the
compiled frontend and native client; it uses the platform WebView.

A frontend rendering failure shows a fixed recovery message with a `Reload fleet` action.
Exception details are not displayed or sent through IPC.

## Develop and check

Host-free checks require Python 3, Rust 1.88 or newer, Node.js 22 and npm. Install the
locked frontend dependencies first; the check target does not install them. From the
repository root:

```sh
npm --prefix veranda ci
make verify-veranda
```

`make verify-veranda` runs [the shared local/CI entrypoint](../dev/check-veranda.py):
the runner's temporary-path contract, Linux resource probe isolation, frontend type
checks and tests, packaging contracts, the frontend build and native Rust tests
without desktop features. CI uses
`python dev/check-veranda.py` with Rust 1.88.0 after `npm ci`.
Set `PYTHON` for another interpreter, such as `make verify-veranda PYTHON=python`.
The entrypoint passes its Python executable to native test fixtures. On Linux, the
native suite runs once with child-only `TMPDIR` pointing through an owned temporary
symlink, checking the alias behavior also found on macOS; other platforms explicitly
skip that probe. `python3 dev/check-veranda.py --rust-only` runs just the native suite.
These checks do not establish native ARM/macOS/Windows results on Linux or physical
desktop, SSH, package installation and resource acceptance.

Install the [Tauri 2 prerequisites](https://v2.tauri.app/start/prerequisites/) for desktop builds.
The committed npm policy disables dependency lifecycle scripts. Start the desktop app with
`npm --prefix veranda run tauri dev`. For Linux local mode, install the matching `yard` on
`PATH`; from a source checkout use `make build` and the repository's `bin/` directory.
Browser-only layout inspection uses `npm --prefix veranda run dev:fixture`; the fixture is
restricted to development builds.

The product and interaction contract lives in [the UX documentation](../docs/veranda/README.md).

## Build a matched desktop candidate

Yard and Veranda use one product tag, `vMAJOR.MINOR.PATCH`, and separate installable artifacts.
Build on the destination platform with its native prerequisites:

```sh
node dev/build-veranda.mjs --version 0.1.0 --check
node dev/build-veranda.mjs --version 0.1.0
```

Replace `0.1.0` with the exact Yard product release. The helper validates the checked-in npm,
Cargo and Tauri baseline versions, passes the product version through an inline Tauri
configuration and `VERANDA_PRODUCT_VERSION`, and leaves source manifests unchanged. It keeps
generated platform icons, packages and a SHA-256 candidate manifest under
`.build/veranda/<version>/<platform>-<arch>/`. Cargo targets default to `.build/veranda/target`;
set `CARGO_TARGET_DIR` to reuse an existing native compilation cache. Linux produces a Debian package, Windows an
NSIS installer, and macOS an application archived with its executable permissions intact.
The Debian package adds the system SSH client to Tauri's desktop dependencies. CI Linux
packages use Ubuntu 24.04; compatibility with older distributions requires separate verification.
`--no-bundle` builds only the native executable for a focused compilation smoke.

The [desktop candidate workflow](../.github/workflows/veranda.yml) performs frontend and
native transport checks plus native builds on Linux amd64/arm64, Windows and macOS. A product
tag stamps its exact version into all candidates. These jobs upload unsigned CI artifacts;
they do not publish a release or install/update the Yard engine. Existing core and shipped
profile acceptance jobs remain independent and required.

## Delivery gates

A successful desktop build is a candidate, not release acceptance. Before publication,
complete real local/remote compatibility, host-key/reconnect/cancellation, package
installation/upgrade/rollback and resource measurements on each claimed platform, together
with the existing Yard and shipped-profile release gates. Both products must have the same
version; a mismatch produces actionable incompatibility rather than a compatibility promise.

Windows code signing and macOS signing/notarization require separately configured protected
CI credentials and platform acceptance. They are not configured here. See Tauri's
[Windows signing](https://v2.tauri.app/distribute/sign/windows/) and
[macOS signing](https://v2.tauri.app/distribute/sign/macos/) guides. Unsigned candidate builds
explicitly disable signing, are marked `unsigned` with acceptance `pending` in their manifests,
and are not wired into the existing release publisher. CLI installation/update continues to
operate without Veranda; desktop installation and updates remain separate.

## Focused E2E checks

Native transport tests require Python 3: the synthetic owner fixture defaults to `python3`
and accepts an executable through `VERANDA_TEST_PYTHON`. The shared check entrypoint
selects its own Python executable; CI explicitly provisions Python. Python is a test/probe
requirement, not an installed Veranda runtime dependency; a Windows `python3` alias must
not be assumed to exist.

The [owner RPC harness](../dev/e2e/veranda-owner.sh) requires a disposable allocated VM,
Incus and the fixture's prerequisites. Run it through the allocation workflow in the
[testing guide](../docs/testing.md) and [VM guide](../docs/test-vms.md). The
[native SSH harness](../dev/e2e/veranda-native.sh) runs inside that marked owner fixture
with its native test binary, matching owner binary, yard name and fixture root. It is not
a standalone check against a personal owner host. Focused passes do not replace package,
platform, shipped-profile or release acceptance.
Its isolated loopback server requires OpenSSH 9.8 or newer and bounds unauthenticated
connections at 16. The fixture disables `PerSourcePenalties` for its 100 repeated assessments,
which each scan three host-key algorithms. Private root/key ownership and mode checks remain
explicit; failure diagnostics emit only bounded stages, iteration counts and fixed codes.

The native SSH scenario also pauses an owned loopback TCP relay while its sockets remain
open. It requires bounded read failure, actual transport loss and failed reconnect before
resuming transmission, followed by an autonomous fresh subscribed session with unchanged
pins, connection records and owner settings. The relay retains bounded pending data and
closes its owned sockets on exit. This checks SSH byte-flow stalling; it does not establish
sleep/wake, interface-change or subsequent Incus event-delivery acceptance.
The separate IPv4 loss phase uses the same recovery assertions against the direct SSH endpoint.
Inside the allocated guest, a guarded nftables table drops only traffic between loopback
addresses and the exact owned listener port. Positive kernel counters are required before
restoring traffic. Cleanup checks the table's ownership marker and removes it before releasing
the port; failed cleanup keeps the reservation until the disposable lease ends and fails the check.
Run `bash dev/e2e/veranda-native.sh --packet-loss-self-test` for the bounded ownership/counter
checks; actual packet loss and recovery require the allocated guest.

Set `SUBYARD_E2E_VERANDA_NATIVE=1` to include the staged native client checks.
`SUBYARD_E2E_VERANDA_NATIVE_ONLY=1` selects their independent scenario after real owner
setup and skips the separate framed owner/bootstrap suite. The opt-in
`SUBYARD_E2E_VERANDA_TERMINAL=1` installs fixture-only Xvfb, xterm and xdotool in the
allocated VM and checks real host, yard and project shell input, working directories and
window cleanup, with a 30-second bound per session. It does not establish VS Code acceptance.
The separate opt-in `SUBYARD_E2E_VERANDA_EDITOR=1` prepares pinned official VS Code
1.140.0 and Remote SSH 0.128.0 inside the marked allocated VM. It requires official
download access and a working normal Electron sandbox with unprivileged user namespaces.
It checks production remote-editor launch, the selected project's real integrated-terminal
working directory and window cleanup, with a 300-second GUI segment bound after bounded
tool preparation. Only the synthetic
agent's public key is added to the disposable guest; existing authorized keys are preserved.
Code data, extensions and settings remain isolated, workspace trust stays enabled, and the
owned owner listener allows local forwarding only to the selected guest port and preview
port 8765. This check does not establish other editor versions or desktop platforms.

The [Debian package harness](../dev/e2e/veranda-package.sh) runs only on allocated VM1
with Veranda initially absent. Supply two explicitly staged `.deb` paths: baseline 0.1.0
and candidate 0.1.1. It installs the normal package dependencies plus the Xvfb and
desktop-file-utils fixture tools, verifies the desktop entry and installed executable,
then checks install, upgrade and rollback launches with preserved isolated app data.
GUI readiness has a 20-second bound and can represent either a fleet or an actionable
owner error; it does not prove owner RPC compatibility. Package files and installed CLI
hashes are checked to ensure that GUI delivery does not bundle or alter the owner CLI.
Cleanup removes only the fixture-installed GUI package and marker-owned app state.

On an allocated disposable owner VM, run `SUBYARD_E2E_VERANDA_RELEASE_GUI_ONLY=1 bash dev/e2e/veranda-owner.sh`
for the separate 300-second release GUI matrix using staged
`.build/veranda-package-noop-base.deb` (0.1.0) and `-next.deb` (0.1.1), with real
version-stamped owner engines. Set `SUBYARD_E2E_VERANDA_GUI_BASE_DEB` and
`SUBYARD_E2E_VERANDA_GUI_NEXT_DEB` to absolute paths to check another staged pair
without replacing previous artifacts. It requires Xvfb, private D-Bus, Python GI/Atspi,
OpenSSH server and the normal GUI runtime libraries. It prepares tools and the actual
DEB dependencies with apt, requires Veranda to be absent initially, and removes only
its fixture-owned Veranda package on exit. Preparation timing is reported separately.
The fixture checks default,
named and UI-consented pinned SSH contexts using accessibility assertions and private
screenshots. This mode cannot be combined with native, editor or bootstrap-only
acceptance; it does not establish those gates or other platforms. Missing prerequisites
or ambiguous accessibility controls fail the check. The default case proves owner RPC
negotiation without `-Y`, using a private config-only default inventory row; it does not
initialize or establish a live default yard lifecycle. The named case uses the running
disposable fixture yard.

Add `SUBYARD_E2E_VERANDA_GUI_GROWTH=1` to that independent GUI mode for a focused
connection growth check using only the current 0.1.1 DEB from
`SUBYARD_E2E_VERANDA_GUI_NEXT_DEB`. It keeps one GUI process alive for ten warmup and
100 measured onboarding/removal cycles. Each removal must close the captured SSH and
owner-session process identities. The first and last ten measured cycles each settle
for 30 seconds before sampling aggregate native, WebKit and local owner RPC RSS;
retained growth between their medians must stay within 10 MiB. The GUI segment has an
1800-second bound, with separate preparation and cleanup. Its receipt reports completed
cycles, all twenty RSS samples and the budget result; interrupted checks stay incomplete.
This uses the owned Xvfb input fixture and a small real owner inventory. Canonical
Wayland fleet, peak, latency and stress checks remain separate. Run
`python3 dev/e2e/veranda-release-gui.py --growth-self-test` to check median and receipt handling.

Failed GUI checks retain a separate bounded diagnostic receipt with accessibility failure
details, owned process-role counts and fixed stderr signature flags. With the existing
`VERANDA_RESOURCE_PROBE=1` opt-in, the application emits each fixed frontend fault or
Linux WebKit termination/responsiveness code at most once. These records contain no error
messages, stacks, URLs or connection data. Responsiveness records describe observed changes;
process liveness alone does not prove the frontend is responsive. An AT-SPI child-count
failure sentinel discards the whole observation and uses the existing bounded retry,
without accepting a partial tree or increasing its size limits.

## Reproduce the Linux resource probe

The [probe](../dev/measure-veranda.py) uses Python's standard library, Linux `/proc`, Xvfb,
the release binary and a matching Yard engine. X11 screenshot capture additionally uses
the installed `libX11`; WebKit version metadata uses `pkg-config` or the Debian runtime package.
Metadata is checked before launching the application. From the repository root,
with the native build prerequisites installed:

```sh
CARGO_TARGET_DIR="$PWD/veranda/src-tauri/target" node dev/build-veranda.mjs --version 0.1.0
YARD_BUILD_VERSION=0.1.0 dev/build-engine.sh --output "$PWD/.build/veranda-probe-engine"
python3 dev/measure-veranda.py
```

Keep both binaries on the same exact product version. Defaults are 30 fresh-process starts
and 120 seconds idle after 30 seconds of settling the final launch. `--binary`, `--engine`,
`--starts`, `--idle-seconds`, `--idle-settle-seconds`, `--timeout`,
`--output` and `--screenshot` override their corresponding inputs. The diagnostic
`--disable-compositing`, `--disable-dmabuf` and `--disable-gdk-gl` options affect only the isolated child and
must remain off for the normal candidate baseline.

Use `--wayland` from an existing Wayland session to measure rendering on that display
instead of starting Xvfb. The compositor socket and its private runtime directory must
belong to the current user. The child still gets isolated app and owner state, an absolute
compositor socket reference, and no X11 fallback. This mode captures no screenshots of the
real desktop. Missing display prerequisites fail before the application starts.
It does not prepare the benchmark fleet or verify the required hardware, compositor,
screen geometry or refresh rate; record those separately before claiming acceptance.

Add `--configured-fleet` for one local owner with 20 configured, uncreated yards and
200 project names. Each fresh launch gets isolated registrations and current-schema
records. The matching engine validates them through its real inventory read before
timing starts; the first readiness marker must then report the same loaded counts from
the frontend after two animation frames. An error screen cannot pass this workload.
The seeded fleet has no Incus instances. This verifies loaded data and selected-screen readiness,
not every project's pixels, initialization, imported workspaces or running-yard behavior.
The preparation and engine preflight are outside startup timing; OS/file caches remain warm.

The [allocated-VM wrapper](../dev/e2e/veranda-wayland-resources.py) prepares minimal Weston
and the staged 0.1.1 amd64 package inside an owned `android-test` singleton. Pass it the
staged package and matching engine paths in the same frozen public candidate bundle.
It selects headless GL composition explicitly, validates the current 1920×1080/60 Hz output,
and records actual CPU/RAM, package versions and the compositor's GL renderer. The app keeps
its normal rendering flags. The result reaches the controller after owned compositor,
platform and temporary-file cleanup; the runner then disposes of the lease. This is an empty-owner Wayland control;
the prepared fleet and application hardware acceleration remain separate acceptance checks.
The wrapper's `--configured-fleet` selects the configured/uncreated data workload above.
It prepares native Incus access through product `yard init` in a separate isolated yard,
removes that instance before measurement and tears down retained data afterward. Preparation
and its bounded privileged-child and root-file cleanup checks are outside startup timing. The GUI stays
unprivileged. This mode cannot be combined with the short paired GDK diagnostic.

Fresh temporary state lives under `/var/tmp`, independent of `TMPDIR`, so owner keys remain
outside the checkout. Privileged cleanup validates the exact directory identity and private
marker, then deletes only that fixture without following symlinks or crossing filesystems.
Cleanup failures leave a successful check failed and preserve an existing primary failure.

The probe creates fresh app/owner configuration, cache and data directories, prevents the
private configuration fallback, and launches no remote SSH connections or mutations.
It measures the first usable fleet/error marker after rendering, then keeps sampling for
two seconds to include asynchronous details and renderer initialization. OS/library caches
remain warm. Aggregate RSS includes the native shell, WebKit and all discovered Yard/helper
processes, including children in independent process groups. Shared pages are counted in
each process's RSS; they are not subtracted. PSS is reported separately. CPU is normalized
to one core. Sampled peaks can miss short-lived processes between samples. Xvfb and the
sampling process are outside the application aggregate.

Outputs default to `.build/veranda-resources.json` and `.build/veranda-resources.png`.
The JSON records binary hashes, process attribution, software/CPU metadata and scope.
The latest measured 0.1.1 Xvfb candidate used Debian 13, WebKitGTK 2.52.5, Mesa 25.0.7
and eight logical CPUs. The executable SHA-256 was
`9f97a4a7f41ef2c71a041e4cb144db45d5ef157c900546a12045f55085635b68`.
It rendered an isolated local owner with unknown yard states and no registered projects.
Thirty starts measured median 1.39 seconds and p95 1.97 seconds; peak aggregate RSS was
531 MiB and idle peak RSS was 525 MiB. Idle CPU averaged 0.28% of one core over 120 seconds.
These measurements exceeded the original 256 MiB idle memory limit. This is neither physical
fleet/mutation coverage nor acceptance of the 8 GiB Wayland baseline, other Linux hardware,
Windows or macOS. Those gates remain pending.

The same GUI executable was also measured on a disposable Debian 13 Wayland VM with
four CPUs, 8 GiB nominal RAM, WebKitGTK 2.54.0, Mesa 25.0.7 and Weston 14.0.2 headless GL
at 1920×1080/60 Hz. The compositor reported llvmpipe software rendering; application
hardware acceleration was unverified. The matching 0.1.1 engine was rebuilt from the
rebased source. Thirty starts measured median 0.94 seconds and p95 2.96 seconds; startup
peak aggregate RSS was 576 MiB. After 30 seconds of settling, the 120-second idle window
measured peak RSS 591 MiB and CPU 0.025% of one core. Compositor/probe processes were
excluded; owned app/compositor cleanup and lease release passed. Startup and memory
limits were unmet under the original 256/384 MiB RSS budgets. This empty-owner control differs from the Xvfb environment and
does not establish a backend's causal effect, a leak, or prepared-fleet acceptance.

A short paired diagnostic on the same Wayland VM ran three fresh starts and ten seconds
of idle per condition. Normal idle peak RSS was 564 MiB; app-only `GDK_GL=disable`
reduced it to 471 MiB. The web process no longer had Mesa mappings in that condition,
while the main application retained them. Both measurements exceeded the original idle memory limit.
The normal-first order, warm caches and short windows limit this diagnostic; its startup
times do not replace the thirty-launch baseline. The GTK environment override remains
diagnostic-only, and application rendering settings are unchanged.

The current 0.1.1 DEB was then measured with one local owner, 20 configured uncreated yards
and 200 project names on the same Debian 13 Wayland software-rendering setup. The installed
executable SHA-256 was `440927b3a881045d22ec9ca5cb1a9bfcad51d965016951d7ba2f06247b26783d`,
verified against the exact package payload. Thirty starts measured p95 1.895 seconds and
580 MiB sampled peak RSS. After 30 seconds of settling, the 120-second idle window measured
587 MiB peak RSS, 393 MiB PSS and CPU 0.042% of one core. All four owned cleanup checks and
lease release passed. Startup and idle CPU meet their limits. Idle RSS and the sampled startup
peak are below the Linux software-rendering limit revised to 640 MiB on 2026-10-06; the lower
128/256 MiB targets remain unmet. Memory investigation and alternative-runtime research remain
planned work. This establishes the configured/uncreated data workload, while initialized fleets,
switch/event peaks, stress, cycle growth and hardware acceleration remain separate checks.

An earlier candidate, SHA-256
`3650aab56c4a2876490ddba359525837e388dfb67718a373497fa8e1ec930e41`, measured median
1.28 seconds, p95 1.37 seconds, 531 MiB peak RSS and 523 MiB idle peak RSS. One earlier
launch exited before readiness with status 101; a diagnostic reproduction and thirty
subsequent starts succeeded, with the original cause unestablished. A separate three-start,
ten-second diagnostic of that earlier binary enabled both early rendering flags and reached
the same screen with 450 MiB idle peak RSS, exceeding the original budget. These runs do not
establish a performance regression or either flag's individual effect; production retains
its normal rendering behavior.


## Dependency audit limits

Devalue is patched to 5.9.4 in the lockfile. The reviewed npm audit still reports the
Vitest/mocker path-traversal advisory and the cookie/SvelteKit/adapter dependency chain;
their suggested fixes require separate major-tooling migrations. Veranda ships static
assets without a Node web server, and development tooling is not exposed by default.
The zero-result `npm audit --omit=dev` is not proof that compiled browser code has no
dependency exposure: build dependencies can contribute shipped assets. Keep full npm and
native dependency audits, their maintenance warnings and platform acceptance under review.

The locked Linux graph includes GLib 0.18.5, affected by
[RUSTSEC-2024-0429](https://rustsec.org/advisories/RUSTSEC-2024-0429.html).
Source searches found no production caller of its affected string-variant iterator in the
application or inspected locked dependencies. Expanded macros, the final binary and native
libraries were not analyzed; this does not establish exhaustive unreachability. Removing the
affected copy requires a compatible upstream fix or an aligned GTK/WebKit dependency update.

The Debian fixture prints `PACKAGE_METRICS` for each install/upgrade/rollback:
archive bytes, the application's dpkg `Installed-Size` in KiB, and cumulative
installed size/count of newly added OS dependencies relative to the state after
Xvfb and desktop-file-utils were installed. Fixture tools are excluded. These
package metadata sizes are separate from the application's budget and are not a
filesystem measurement; dependencies already present on the VM contribute no
incremental size. `APT_DOWNLOAD_SUMMARY` preserves apt's bounded English archive
summary before fixture logs are removed. Download totals may reflect a warm apt
cache; a missing summary is reported as unavailable rather than zero.
