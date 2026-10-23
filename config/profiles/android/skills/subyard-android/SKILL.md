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

Use bounded read-only checks before requesting an emulator:

```sh
timeout 30 android-broker catalog
timeout 30 android-broker status
```

The catalog lists supported device/API combinations and cached images; status reports
pool defaults and availability. Failed checks mean readiness is unknown. Skill presence
records successful provisioning, and does not prove that the pool is currently ready.

Run device tests with a broker-managed lease:

```sh
android-broker run --device phone --api 35 -- ./run-device-tests.sh
```

Use `--wait SECONDS` for a bounded wait when the pool is busy. The wrapper renews the
lease, preserves the command's exit code and releases on exit or interruption. It
passes both `ANDROID_SERIAL` and `ADB_SERVER_SOCKET` to the command: keep both, because
the serial alone does not identify an allocation. ADB uses the supplied socket.
For a multi-command session, run `android-broker run --device phone --api 35 -- bash`.

Use the broker for emulator lifecycle. Report unsupported requests or unavailable
provisioning to the operator; installing another SDK or launching an unmanaged emulator
bypasses the shared pool. Owner yard lifecycle, credentials and host permissions remain
operator responsibilities.
