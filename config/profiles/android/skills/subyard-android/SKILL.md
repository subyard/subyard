---
name: subyard-android
description: Build or test Android apps, find the installed Android SDK or JDK, or request a phone or tablet emulator inside a Subyard yard.
---

Check the installed environment in a login shell when your current shell lacks the
profile PATH. The profile exports its actual paths in `/etc/profile.d/subyard-android.sh`.
Standard L1 paths are SDK `/srv/cache/android-sdk`, JDK `/opt/jdk-17`, and Gradle cache
`/srv/cache/gradle`; provisioning supports SDK, JDK version and yard Gradle overrides.
Inspect `JAVA_HOME`, `ANDROID_SDK_ROOT`, `ANDROID_HOME`, `GRADLE_USER_HOME`, and
`command -v java adb android-broker` for the actual toolchain. L2 project environments
have a separate writable Gradle cache at `/home/dev/.gradle`; use that environment's
exports. Keep the provisioned SDK and JDK as the build inputs.

The shared SDK is read-only. Defaults: platforms `android-35` and `android-37.2`,
build-tools `36.0.0` and `37.0.0`, platform-tools, cmdline-tools and emulator.
`android-broker sdk list` shows default, previous-default and added packages.
When Gradle reports `SDK directory is not writable`, `Failed to find target` or missing
build tools, add the package (for example compileSdk 36 minor 1) and rerun:

```sh
android-broker sdk install 'platforms;android-36.1' 'build-tools;35.0.0'
```

Allowed IDs: `platforms;android-N[.M]`, `platforms;android-N-extM`,
`build-tools;X.Y.Z[-rcN]`, `ndk;X.Y.Z`, `cmake;X.Y.Z[.W]`; no previews. Installs are
add-only. Google SDK licenses are accepted automatically and logged.

Use bounded read-only checks before requesting an emulator:

```sh
timeout 30 android-broker catalog
timeout 30 android-broker status
```

The catalog lists supported device/API combinations and cached images; status reports
pool defaults (phone, API 36) and availability. Failed checks mean readiness is unknown.
Skill presence records successful provisioning, and does not prove that the pool is
currently ready.

Run device tests with a broker-managed lease:

```sh
android-broker run --device phone --api 35 -- ./run-device-tests.sh
android-broker run --device phone --api 35 -- ./gradlew :app:connectedDebugAndroidTest
```

`run` adds `-Dorg.gradle.daemon=false` to `GRADLE_OPTS`: a daemon would outlive the lease
and keep its old ADB port. With `acquire`, pass `--no-daemon` yourself. Use `--wait SECONDS` for a bounded wait when the pool is busy. The wrapper
renews the lease, preserves the command's exit code and releases on exit or interruption.
It passes `ANDROID_SERIAL`, `ADB_SERVER_SOCKET` (adb CLI) and `ANDROID_ADB_SERVER_PORT`
(loopback port for AGP/ddmlib, accepting only your user): keep them, because the serial
alone does not identify an allocation. `adb forward`/`adb reverse` ports live in the
emulator's private network and are not reachable from your command.
For a multi-command session, run `android-broker run --device phone --api 35 -- bash`.

Use the broker for emulator lifecycle and SDK additions. Report unsupported requests
or unavailable provisioning to the operator; a private SDK or an unmanaged emulator
bypasses the shared pool. Owner yard lifecycle, credentials and host permissions remain
operator responsibilities.
