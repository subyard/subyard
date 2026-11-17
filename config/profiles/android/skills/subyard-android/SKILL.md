---
name: subyard-android
description: Build or test Android apps, find the installed Android SDK or JDK, or request a phone or tablet emulator inside a Subyard yard.
---

Actual toolchain exports live in `/etc/profile.d/subyard-android.sh`; use a login shell
if the profile PATH is missing. L1 defaults: SDK `/srv/cache/android-sdk`, JDK `/opt/jdk-17`,
Gradle cache `/srv/cache/gradle`; L2 Gradle cache: `/home/dev/.gradle`. Exports take precedence.

The shared SDK is read-only; use `android-broker sdk list` and `android-broker sdk install`
for missing packages, rather than a private SDK. Installs are add-only;
Google SDK licenses are accepted automatically and logged. Package grammar/defaults: [Android guide](https://github.com/Subyard/Subyard/blob/master/docs/android.md).

`timeout 30 android-broker catalog` lists device/API choices;
`timeout 30 android-broker status` checks availability. Failed checks mean readiness is unknown;
skill presence records provisioning, not current pool readiness.

Run tests inside a broker-managed lease:

```sh
android-broker run --device phone --api 35 -- ./gradlew :app:connectedDebugAndroidTest
```

`run` renews/releases the lease and adds `-Dorg.gradle.daemon=false` to `GRADLE_OPTS`
to avoid a daemon retaining a previous lease's ADB port. With `acquire`, use `--no-daemon`.
Keep `ANDROID_SERIAL`, `ADB_SERVER_SOCKET` (adb CLI) and `ANDROID_ADB_SERVER_PORT`
(AGP/ddmlib loopback port): the serial alone does not identify an allocation.
`adb forward`/`adb reverse` ports stay inside the emulator's private network.
Use the broker for SDK additions and emulator lifecycle; owner yard lifecycle,
credentials and host permissions belong to the operator.
