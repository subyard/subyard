#!/usr/bin/env bash
# The reported scenario: AGP 9 builds on default SDK packages, a missing minor-API platform is added
# through the broker, the project builds against it and runs connected tests through a lease.
set -euo pipefail
[ "$#" -eq 0 ] || { printf 'usage: android-gradle.sh\n' >&2; exit 2; }
[ "$(id -u)" = 1000 ] || { printf 'android-gradle: requires the fixture developer\n' >&2; exit 1; }
# shellcheck source=/dev/null
. /etc/profile.d/subyard-android.sh
sdk="$ANDROID_SDK_ROOT"
added='platforms;android-36.1'
# Separate commands: set -e ignores failures before the last command of an && list.
test ! -w "$sdk"
for platform in android-35 android-37.2; do
  test -s "$sdk/platforms/$platform/android.jar"
  # AGP sees archive-installed defaults on the read-only SDK only through package.xml.
  grep -Fq "<localPackage path=\"platforms;$platform\"" "$sdk/platforms/$platform/package.xml"
done
test -x "$sdk/build-tools/36.0.0/aapt2"
test ! -e "$sdk/platforms/android-36.1"
# Pinned distribution; AGP 9.2.1 requires Gradle 9.4.1 or newer.
gradle_version=9.4.1
gradle_sha256=2ab2958f2a1e51120c326cad6f385153bb11ee93b3c216c5fccebfdfbb7ec6cb
work="$(mktemp -d /tmp/subyard-android-gradle.XXXXXX)"
chmod 0700 "$work"
printf '%s\n' subyard-android-gradle-v1 > "$work/.marker"
stage='setup'
cleanup() {
  local result=$?
  trap - EXIT
  if [ "$result" -ne 0 ]; then
    printf 'android-gradle: stage=%s exit=%s\n' "$stage" "$result" >&2
    for log in assemble-37.2.log assemble-35.log assemble-missing.log sdk-install.log assemble-36.1.log connected.log; do
      if [ -f "$work/$log" ]; then
        printf 'android-gradle: %s (last 80 lines)\n' "$log" >&2
        tail -n 80 "$work/$log" >&2 || true
      fi
    done
  fi
  if [ "$(cat "$work/.marker")" = subyard-android-gradle-v1 ]; then
    rm -rf -- "$work" || { [ "$result" -ne 0 ] || result=3; }
  else
    [ "$result" -ne 0 ] || result=3
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "$work"
stage='gradle-download'
# The script itself arrives on stdin; no command below may consume it.
curl -fLsS --connect-timeout 15 --max-time 600 -o gradle.zip \
  "https://services.gradle.org/distributions/gradle-$gradle_version-bin.zip" < /dev/null
printf '%s  gradle.zip\n' "$gradle_sha256" | sha256sum -c - >/dev/null
unzip -q gradle.zip
rm gradle.zip
# Outside a lease these builds disable the daemon explicitly; inside run the wrapper does.
gradle=("$work/gradle-$gradle_version/bin/gradle" --console=plain)
stage='project'
mkdir -p project/app/src/main/java/org/subyard/gradle project/app/src/androidTest/java/org/subyard/gradle
cd project
cat > settings.gradle.kts <<'KTS'
pluginManagement {
    repositories { google(); mavenCentral(); gradlePluginPortal() }
}
dependencyResolutionManagement {
    repositories { google(); mavenCentral() }
}
rootProject.name = "subyard-gradle-acceptance"
include(":app")
KTS
cat > build.gradle.kts <<'KTS'
plugins { id("com.android.application") version "9.2.1" apply false }
KTS
cat > gradle.properties <<'PROPERTIES'
org.gradle.jvmargs=-Xmx1536m
android.useAndroidX=true
kotlin.compiler.execution.strategy=in-process
PROPERTIES
cat > app/build.gradle.kts <<'KTS'
plugins { id("com.android.application") }
android {
    namespace = "org.subyard.gradle"
    // Each step selects its compile platform; a minor API level is optional.
    compileSdk = providers.gradleProperty("subyardCompileSdk").get().toInt()
    providers.gradleProperty("subyardCompileSdkMinor").orNull?.let { compileSdkMinor = it.toInt() }
    defaultConfig {
        applicationId = "org.subyard.gradle"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "1.0"
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }
}
dependencies {
    androidTestImplementation("androidx.test:runner:1.7.0")
    androidTestImplementation("androidx.test.ext:junit:1.3.0")
}
KTS
cat > app/src/main/AndroidManifest.xml <<'XML'
<manifest xmlns:android="http://schemas.android.com/apk/res/android">
  <application android:label="Subyard Gradle acceptance">
    <activity android:name=".MainActivity" android:exported="true">
      <intent-filter>
        <action android:name="android.intent.action.MAIN" />
        <category android:name="android.intent.category.LAUNCHER" />
      </intent-filter>
    </activity>
  </application>
</manifest>
XML
cat > app/src/main/java/org/subyard/gradle/MainActivity.java <<'JAVA'
package org.subyard.gradle;
public final class MainActivity extends android.app.Activity {
    @Override public void onCreate(android.os.Bundle state) {
        super.onCreate(state);
        android.widget.TextView text = new android.widget.TextView(this);
        text.setText("Subyard Gradle acceptance");
        setContentView(text);
    }
}
JAVA
cat > app/src/androidTest/java/org/subyard/gradle/DeviceTest.java <<'JAVA'
package org.subyard.gradle;
import static org.junit.Assert.assertEquals;
import androidx.test.ext.junit.runners.AndroidJUnit4;
import androidx.test.platform.app.InstrumentationRegistry;
import org.junit.Test;
import org.junit.runner.RunWith;
@RunWith(AndroidJUnit4.class)
public final class DeviceTest {
    @Test public void targetPackageIsInstalled() {
        assertEquals("org.subyard.gradle",
            InstrumentationRegistry.getInstrumentation().getTargetContext().getPackageName());
    }
}
JAVA
not_writable() {
  if grep -Eq 'SDK directory is not writable|Failed to find (target|Build Tools)' "$1"; then
    printf 'android-gradle: AGP attempted to change the shared SDK\n' >&2
    return 1
  fi
}
assemble() {
  local label="$1" log="$work/assemble-$1.log"
  shift
  "${gradle[@]}" --no-daemon "$@" :app:assembleDebug > "$log" 2>&1 < /dev/null
  not_writable "$log"
  if grep -Fq 'Exception while marshalling' "$log"; then
    printf 'android-gradle: AGP could not record package metadata for an SDK component\n' >&2
    return 1
  fi
  test -s app/build/outputs/apk/debug/app-debug.apk
  "$sdk/build-tools/36.0.0/aapt2" dump badging app/build/outputs/apk/debug/app-debug.apk > "$work/badging.txt"
  grep -Fq "package: name='org.subyard.gradle'" "$work/badging.txt"
  printf 'android gradle: AGP 9.2.1 assembleDebug compileSdk=%s=PASS\n' "$label"
}
stage='assemble-defaults'
assemble 37.2 -PsubyardCompileSdk=37 -PsubyardCompileSdkMinor=2
assemble 35 -PsubyardCompileSdk=35
stage='assemble-missing'
# Before the broker adds it, the missing platform fails without changing the shared SDK.
if "${gradle[@]}" --no-daemon -PsubyardCompileSdk=36 -PsubyardCompileSdkMinor=1 :app:assembleDebug \
    > "$work/assemble-missing.log" 2>&1 < /dev/null; then
  printf 'android-gradle: compileSdk 36.1 built before the platform was added\n' >&2
  exit 1
fi
test ! -e "$sdk/platforms/android-36.1"
stage='sdk-install'
android-broker sdk install "$added" > "$work/sdk-install.json" 2> "$work/sdk-install.log" < /dev/null
grep -Fxq "Android: accepted Google Android SDK licenses for $added" "$work/sdk-install.log"
python3 - "$work/sdk-install.json" "$added" <<'PY'
import json, sys
result = json.load(open(sys.argv[1]))
assert result['installed'] == [sys.argv[2]] and not result['already_installed'], result
PY
test -s "$sdk/platforms/android-36.1/android.jar"
test ! -w "$sdk/platforms/android-36.1"
android-broker sdk install "$added" > "$work/sdk-repeat.json" < /dev/null
android-broker sdk list > "$work/sdk-list.json" < /dev/null
python3 - "$work/sdk-repeat.json" "$work/sdk-list.json" "$added" <<'PY'
import json, sys
repeat, listed = (json.load(open(path)) for path in sys.argv[1:3])
added = sys.argv[3]
assert repeat['installed'] == [] and repeat['already_installed'] == [added], repeat
sources = {item['package']: item['source'] for item in listed['packages']}
assert sources[added] == 'added' and sources['platforms;android-37.2'] == 'default', sources
PY
printf 'android gradle: sdk install outside defaults, repeat and list=PASS\n'
assemble 36.1 -PsubyardCompileSdk=36 -PsubyardCompileSdkMinor=1
stage='connected'
# No --no-daemon: run's GRADLE_OPTS guard keeps Gradle inside the lease.
android-broker run --device phone --api 36 --wait 900 --purpose android-gradle -- \
  "${gradle[@]}" -PsubyardCompileSdk=36 -PsubyardCompileSdkMinor=1 :app:connectedDebugAndroidTest \
  > "$work/connected.log" 2>&1 < /dev/null
not_writable "$work/connected.log"
if pgrep -u "$(id -u)" -f 'org.gradle.launcher.daemon.bootstrap.GradleDaemon' >/dev/null; then
  printf 'android-gradle: a Gradle daemon outlived the lease\n' >&2
  exit 1
fi
python3 - <<'PY'
from pathlib import Path
import xml.etree.ElementTree as ET
reports = list(Path('app/build/outputs/androidTest-results/connected').rglob('TEST-*.xml'))
assert reports, 'no connected test report'
suites = [ET.parse(report).getroot() for report in reports]
assert sum(int(suite.get('tests', 0)) for suite in suites) == 1
assert all(int(suite.get('failures', 0)) == 0 and int(suite.get('errors', 0)) == 0 for suite in suites)
PY
printf 'android gradle: connectedDebugAndroidTest through a lease=PASS\n'
