# Android emulator leases

The Android profile supplies a shared pool with two slots by default. A slot starts only for a
lease and returns to the pool after its emulator, compositor, ADB server and connections have
stopped. Each allocation starts with fresh Android userdata and no snapshots. Existing personal
AVDs are neither imported into the pool nor removed.

Run `yard provision android` on the owner host (use `yard -Y NAME provision android` for a
named yard). This adds Android to that yard's local profiles while preserving existing selections,
including when a configuration sync source is registered. To save the selection in Git instead,
use the [explicit Git workflow](configuration.md#enabling-a-profile-with-a-registered-source).
An already selected profile can be provisioned directly. Provisioning reconciles
yard prerequisites and installs the toolchain under one confirmation. It installs the shared SDK
at `/srv/cache/android-sdk`, JDK at `/opt/jdk-17`, the in-yard client and
the pool service. It also writes `/etc/profile.d/subyard-android.sh`; open a new login shell after
installation to load the SDK environment. A selected profile alone does not prove installation.
The yard needs x86_64 KVM. The shipped `config/profiles/android/profile.conf` explicitly selects
`EMULATOR_GPU=software-gles`: headless SwiftShader OpenGL ES without a GPU render node or
compositor. Guest Vulkan is explicitly disabled in this mode to reduce startup work on small
VMs. Verbose guest-to-host log forwarding is silenced; guest log buffers remain available through
`adb logcat`. Select `EMULATOR_GPU=software` for SwiftShader with Vulkan support; it can require more
startup time and CPU capacity. Neither mode substitutes hardware rendering automatically.
For hardware graphics, set `EMULATOR_GPU=host` and `YARD_DEVICES="kvm gpu"` in that profile,
then converge the yard with idle pool slots. Host mode requires a working hardware render node
and never falls back to software. KVM remains required in every mode. See the upstream
[graphics modes](https://developer.android.com/studio/run/emulator-acceleration#accel-graphics) and
[slow-emulator troubleshooting](https://developer.android.com/studio/run/emulator-troubleshooting#emulator_runs_slow_after_an_update).
These are profile settings,
not flags to `yard init`. Setup never changes the outer yard's trust lane or enables host
privileges to work around missing devices. Both container and VM yards are supported.

Provision adequate capacity before use: two devices reserve 7.5 GiB including the pool's
yard allowance, so an 8 GiB yard has little remaining capacity for builds. Both presets use
two guest vCPUs and 2560 MiB guest RAM plus 1024 MiB per emulator for the runtime and renderer. Phone is
1080×1920 at 420 dpi; tablet is 800×1280 at 160 dpi (an 800×1280 dp tablet layout).
These are fixed presets, not a fallback chosen under memory pressure. The tablet stays below
the emulator's three-million-pixel threshold, which otherwise forces 4 GiB guest RAM in
[current emulator sources](https://android.googlesource.com/platform/external/qemu/+/refs/heads/emu-main-dev/android/android-emu/android/main-common.c).
Admission also checks available memory. Initial image installation requires at least
8 GiB free disk space for staging, potentially more for a larger image and its temporary SDK dependencies,
plus 2 GiB per configured slot and 1 GiB for yard headroom (13 GiB total for the default pool).
The installer preserves that slot/headroom allowance and removes failed staging data. Runtime starts
also check this minimum free-space allowance. It is not a userdata quota: an SDK image may
require a larger virtual partition, and writes can consume additional disk space. Size the yard
for the expected workload. Capacity errors never shrink the configured pool or stop another consumer's lease.

Once provisioned, agents run these commands **inside the yard**. The local client connects
directly to the pool's Unix socket; it does not require host command execution or Incus access.

From the owner host, use a login shell to load the SDK profile's PATH (select a named yard with
`-Y NAME`):

```sh
yard shell -- bash -lc 'android-broker status'
```

Inside the yard:

```sh
android-broker catalog
android-broker status
android-broker run --device phone --api 35 -- ./run-device-tests.sh
android-broker run --device tablet --api 36 --wait 120 -- bash
```

The defaults are `EMULATOR_SHARING=shared`, `EMULATOR_POOL_SIZE=2`, `EMULATOR_DEVICE=phone`,
`ANDROID_API=36`, `ANDROID_VARIANT=google_apis` and `ANDROID_ABI=x86_64` in the Android profile.
`status` reports the configured defaults. `catalog` reports phone/tablet presets, Android 14/15/16
(API 34/35/36), available Google APIs/Play Store image revisions and local cache presence. Unsupported
requests fail without substituting a device or API. A full pool reports busy; `--wait SECONDS`
waits at most the requested duration (maximum 3600). Provisioning has its own download and boot
checks; cold boot is bounded to 20 minutes, allowing software rendering on modest nested VMs.
A successful allocation is returned only after Android reports `sys.boot_completed=1` and
configures a guest IPv4 address. Wi-Fi initialization can finish after the boot-complete property;
the pool enables guest Wi-Fi if it is still off. If IPv4 is absent, it requests a connection to
the emulator's default open `AndroidWifi` network once, then waits within the same boot deadline.
Readiness does not depend on a public probe service.
Android's DNS service can settle after IPv4 configuration; commands that need Internet access
should allow a bounded connection retry.

`run` provides the command with:

| Variable | Meaning |
| --- | --- |
| `ANDROID_SERIAL` | Serial of this allocation (`emulator-5554` inside its private ADB server) |
| `ADB_SERVER_SOCKET` | Private local Unix socket for this allocation's ADB server |
| `SUBYARD_EMU_SLOT`, `SUBYARD_EMU_GENERATION` | Public slot and generation identifiers |
| `SUBYARD_EMU_DEVICE`, `SUBYARD_EMU_API`, `SUBYARD_EMU_ANDROID_VERSION` | Resolved device and Android version |
| `SUBYARD_EMU_VARIANT`, `SUBYARD_EMU_ABI` | Resolved image variant and ABI |

Use both `ANDROID_SERIAL` and `ADB_SERVER_SOCKET`; the serial alone is not an allocation identity.
ADB uses the supplied socket automatically. Every connection authenticates to the pool with the
lease capability; neither shared TCP ports nor a host Incus ADB proxy grant access. Release closes
existing connections, and an old socket/capability cannot reach a later generation. The runtime's
network namespace contains only its own ADB server and emulator. A per-allocation `slirp4netns`
service supplies outbound networking without publishing inbound ports or changing yard firewall
rules. It blocks access through the host loopback gateway and uses the yard's configured
non-loopback IPv4 DNS servers. Release verifies that both runtime and network helper have stopped.

The wrapper renews every 60 seconds, preserves the command's exit code and releases on exit,
interrupt, hangup or termination. The pool owner checks expiry independently of the wrapper. After
600 seconds without a successful renewal, it closes access and stops the runtime. Inactivity in
ADB or CPU usage does not release a live lease. A failed stop quarantines the slot and keeps its
image protected until an owner retry confirms cleanup. Restarting the pool owner revokes previous
leases and cleans recorded runtimes before accepting requests; stopping the yard also stops its
runtime services.

For explicit multi-step use, choose a new private lease-file path:

```sh
android-broker acquire --device phone --api 35 --lease-file /tmp/my-android-lease.json
android-broker renew --lease-file /tmp/my-android-lease.json
android-broker release --lease-file /tmp/my-android-lease.json
```

Acquire prints JSON containing allocation metadata and the connection environment. The secret
credential stays in the mode-0600 lease file; keep it private. Explicit acquire does **not** renew
automatically: call `renew` at least once per minute while working, or prefer `run -- bash`.
Repeated release is safe. Public status contains owner yard/project/run/purpose and timestamps,
without capabilities or connection endpoints. `--yard`, `--project` and `--purpose` supply display
attribution when the client is outside a managed workspace; these labels do not authorize access.
Project environments receive their yard name and project ID from the normal environment launcher.

The provisioned `android-broker` client is available inside the yard and Android project environments.
L2 mounts the SDK, JDK and client code read-only and needs no KVM, Incus or sudo access. On the
owner host, `yard emu ...` uses the profile handler; `yard -Y OWNER/YARD emu ...` follows the
standard owner routing.
The Android profile does not install a `yard` binary inside the yard.
`run` executes work on the selected owner. Its ADB Unix socket belongs to that execution
environment and is not a controller-local endpoint. Remote `view` instead runs the viewer on
the controller and carries broker requests, ADB and media over SSH to the selected yard.

L1 agents share the yard's `/srv/cache/gradle`. Each L2 environment keeps its own writable Gradle
home at `/home/dev/.gradle` in that container; it survives container stops and is discarded when
the environment is recreated. A writable Gradle cache is never shared across container boundaries.

## View from a Linux laptop

Run `yard emu view` on the computer where the window should appear. Install Subyard,
`scrcpy` and ADB on that computer. The Android profile and emulator hardware belong to
the server's yard; the laptop does not need an Android profile or KVM.

Register the owner once over SSH, then use its authoritative HostID and yard name:

```sh
# On the laptop, once per owner:
yard host add me@my-server
yard yards

# On the laptop, for each viewing session:
yard -Y owner-host/default emu view
yard -Y owner-host/default emu view --control --device tablet --api 35
```

Replace `owner-host/default` with the `<HostID>/<yard>` reported by registration and
`yard yards`. The selector determines which server and yard supply the emulator.
Without a selector, `view` uses the default yard; it does not select a remote server
automatically. A short yard name is usable only when it identifies one known yard.
See [owner registration and yard selection](workflows.md#select-local-and-remote-yards).

For a remote yard, this one command opens local `scrcpy` and uses the registered SSH
connection to reach the pool. It needs no manually opened tunnel or graphical session
on the server. Keep the command running while viewing. Both machines must have a
Subyard runtime that supports controller viewer sessions, and the Android profile must
be provisioned on the selected owner. After updating Subyard, refresh the in-yard client
through `yard -Y owner-host/default provision android` on the laptop, or
`yard -Y default provision android` on the server.

For a local yard, `yard -Y NAME emu view` opens the viewer on that yard's owner host.
The in-yard client also provides `android-broker view`, but it requires a graphical
environment where it executes; an ordinary headless agent shell does not provide one.

## Show an agent's leased device

A standalone `view` acquires a new emulator. To show the same device an agent is using,
the agent acquires an explicit lease inside the yard. These handoff instructions are also
available to agents through the installed `android-broker --help`:

```sh
android-broker acquire --device phone --api 36 --lease-file /tmp/my-android-lease.json
android-broker renew --lease-file /tmp/my-android-lease.json
```

Choose a new private lease-file path, use the ADB environment returned by acquire, and
renew at least once per minute throughout the work. Tell the operator the selected yard
and the absolute lease-file path; do not send the file contents or put them in a repository.
The operator runs this on the laptop:

```sh
yard -Y owner-host/default emu view --lease-file /tmp/my-android-lease.json
```

For a remote yard, `--lease-file` is a path inside that selected yard. A lease created in an
Android project environment can be attached only when its file is also accessible from the yard.
The client reads it through the authenticated SSH connection; no manual copy to the laptop is needed. For a
local owner invocation or `android-broker view`, the path is local to the invoking environment.
Add `--control` when operator input is wanted. The agent remains responsible for renewing
and releasing its lease after the attached viewer closes.

```sh
android-broker release --lease-file /tmp/my-android-lease.json
```

The viewer requires `scrcpy` and ADB in the invoking environment. It is view-only unless
`--control` is specified. A standalone viewer owns a new lease and releases it when closed. An
attached viewer requires its owner's lease file, does not renew that lease and never releases it.
The client carries scrcpy's media streams through the same authenticated lease channel as ADB.
While opening the first media stream, it retries an explicit ADB service-unavailable response
within a 60-second readiness window, backing off to one attempt per second. Each attempt opens a
fresh authenticated channel. Transport errors and failures on later streams are not retried.
If scrcpy fails, the client reports the first relay failure's stage and elapsed time when available,
without including lease credentials or connection addresses.
Its temporary TCP listener binds only to the invoking environment's loopback address and closes
with the viewer; it does not publish the emulator's private ports. As with normal scrcpy forwarding,
the invoking environment must be trusted while its local viewer is open.
GUI sockets are not mounted into agent environments.

```sh
android-broker cache prepare --api 35
android-broker cache prepare --api 36
android-broker cache prune --dry-run
android-broker cache prune
```

Prepare images before starting emulators when the pool is idle. It installs one requested API,
variant and ABI into the verified pool cache without creating a lease, changing a slot generation
or starting a runtime. It takes the SDK maintenance lock exclusively: an active lease makes
prepare fail busy, and allocations fail busy while preparation is running. Prepare API 35 and API
36 first when a phone and tablet will be leased concurrently. An installation can take up to 15
minutes; a disconnected client does not cancel its bounded cache work, while pool shutdown cancels
it and removes its private staging directory.

Prune only removes verified, reproducible images owned by the pool under
`/var/lib/subyard-android/images`. It skips images referenced by provisioning, held, draining or
quarantined slots. Pinning, download and pruning are coordinated; concurrent requests download a
missing revision once. Dry-run lists candidates without deleting anything. Results list removed
and skipped images and byte counts; an empty result succeeds. A later acquire downloads a removed
image again. Shared SDK build tools, Gradle caches and personal AVDs are outside this cleanup scope.
The installed SDK manager handles download integrity and licensing in a private staging SDK;
the pool verifies the requested API, variant, ABI and exact revision before publishing the image.

The shared build SDK is read-only to agents. Setup serializes changes and waits for emulator
leases to release their SDK read locks; repeated setup installs missing components without
upgrading binaries under a running emulator. Changing the pool service/configuration requires idle
slots. The legacy manual launch/controller entrypoints reject new starts. An existing legacy
emulator must be stopped explicitly before activating the new pool; setup does not claim unknown
processes or erase existing Android data.

For an existing Android L2 environment, stop it with `yard down PROJECT` before upgrading the
yard, then run `yard up --rebuild PROJECT` to apply the new read-only mounts, runtime user and
private Gradle home. Preserve any needed files from the old container's writable layer first;
rebuild replaces that layer while retaining the mounted project workspace. Starting an existing
environment does not update its mounts, and the normal profile manifest check requires rebuild
when the profile changes.

Owners can use `yard emu revoke --slot 001` or `yard emu down` to drain leases through the normal
confirmation policy. These operations use a separate owner-only socket, never the socket mounted
into project environments. Service diagnostics are available through the yard's system journal.
