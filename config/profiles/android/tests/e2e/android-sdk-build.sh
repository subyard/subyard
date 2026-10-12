#!/usr/bin/env bash
# One tiny APK built by the provisioned SDK, without Gradle or an emulator.
set -euo pipefail
[ "$#" -eq 0 ] || { printf 'usage: android-sdk-build.sh\n' >&2; exit 2; }
[ "$(id -u)" = 1000 ] || { printf 'android-sdk-build: requires the fixture developer\n' >&2; exit 1; }
export JAVA_HOME=/opt/jdk-17
export PATH="$JAVA_HOME/bin:$PATH"
sdk=/srv/cache/android-sdk
tools="$sdk/build-tools/36.0.0"
platform="$sdk/platforms/android-36/android.jar"
[ ! -w "$sdk" ] && [ -s "$platform" ]
[ "$(readlink -f "$sdk")" = "$sdk" ] && [ "$(findmnt -n -o TARGET -T "$sdk")" = /srv ]
for tool in aapt2 d8 dexdump zipalign apksigner; do test -x "$tools/$tool"; done
work="$(mktemp -d /tmp/subyard-android-sdk-build.XXXXXX)"
chmod 0700 "$work"
printf '%s\n' subyard-android-sdk-build-v1 > "$work/.marker"
stage=build
cleanup() {
  local result=$?
  trap - EXIT
  if [ "$result" -ne 0 ]; then
    printf 'android-sdk-build: stage=%s exit=%s\n' "$stage" "$result" >&2
    # These bounded files contain only this disposable app's public metadata.
    for evidence in badging.txt dex.txt contents.txt; do
      if [ -f "$work/$evidence" ]; then
        printf 'android-sdk-build: %s (first 4096 bytes)\n' "$evidence" >&2
        head -c 4096 "$work/$evidence" >&2 || true
      fi
    done
  fi
  if [ "$(cat "$work/.marker")" = subyard-android-sdk-build-v1 ]; then
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
mkdir -p res/values src/org/subyard/acceptance generated classes dex
cat > res/values/strings.xml <<'XML'
<resources><string name="app_name">Subyard SDK acceptance</string></resources>
XML
cat > AndroidManifest.xml <<'XML'
<manifest xmlns:android="http://schemas.android.com/apk/res/android"
    package="org.subyard.acceptance" android:versionCode="1" android:versionName="1.0">
  <application android:label="@string/app_name">
    <activity android:name=".MainActivity" android:exported="true">
      <intent-filter>
        <action android:name="android.intent.action.MAIN" />
        <category android:name="android.intent.category.LAUNCHER" />
      </intent-filter>
    </activity>
  </application>
</manifest>
XML
cat > src/org/subyard/acceptance/MainActivity.java <<'JAVA'
package org.subyard.acceptance;
public final class MainActivity extends android.app.Activity {
    @Override public void onCreate(android.os.Bundle state) {
        super.onCreate(state);
        android.widget.TextView text = new android.widget.TextView(this);
        text.setText(R.string.app_name);
        setContentView(text);
    }
}
JAVA
"$tools/aapt2" compile res/values/strings.xml -o .
"$tools/aapt2" link values_strings.arsc.flat -I "$platform" --manifest AndroidManifest.xml \
  --java generated --min-sdk-version 26 --target-sdk-version 36 -o unsigned.apk
javac --release 8 -classpath "$platform" -d classes \
  src/org/subyard/acceptance/MainActivity.java generated/org/subyard/acceptance/R.java
mapfile -t bytecode < <(find classes -name '*.class' -type f)
[ "${#bytecode[@]}" -gt 0 ]
"$tools/d8" --min-api 26 --lib "$platform" --output dex "${bytecode[@]}"
jar uf unsigned.apk -C dex classes.dex
"$tools/zipalign" -f 4 unsigned.apk aligned.apk
# Disposable test signing key; these public passwords protect no real credentials.
stage=sign
keytool -genkeypair -keystore test.keystore -storepass android -keypass android \
  -alias acceptance -dname CN=SubyardAcceptance -keyalg RSA -keysize 2048 -validity 2
"$tools/apksigner" sign --ks test.keystore --ks-pass pass:android --key-pass pass:android \
  --out app.apk aligned.apk
stage=signature
"$tools/apksigner" verify --verbose app.apk
stage=alignment
"$tools/zipalign" -c 4 app.apk
stage=manifest
"$tools/aapt2" dump badging app.apk > badging.txt
python3 - <<'PY'
from pathlib import Path
import shlex

records = {}
for line in Path('badging.txt').read_text().splitlines():
    kind, _, payload = line.partition(':')
    if kind in ('package', 'application', 'launchable-activity'):
        records[kind] = dict(part.split('=', 1) for part in shlex.split(payload) if '=' in part)
    elif kind in ('minSdkVersion', 'sdkVersion', 'targetSdkVersion'):
        records[kind] = shlex.split(payload)[0]
package = records.get('package', {})
for field, observed, expected in (
        ('package', package.get('name'), 'org.subyard.acceptance'),
        ('version code', int(package.get('versionCode', -1)), 1),
        ('version name', package.get('versionName'), '1.0'),
        ('minimum API', int(records.get('minSdkVersion', records.get('sdkVersion', -1))), 26),
        ('target API', int(records.get('targetSdkVersion', -1)), 36),
        ('resource label', records.get('application', {}).get('label'), 'Subyard SDK acceptance'),
        ('launcher Activity', records.get('launchable-activity', {}).get('name'),
         'org.subyard.acceptance.MainActivity')):
    if observed != expected:
        raise SystemExit(f'android-sdk-build: {field}: expected {expected!r}, observed {str(observed)[:200]!r}')
PY
stage=dex
mkdir verified
(cd verified && jar xf ../app.apk classes.dex)
"$tools/dexdump" verified/classes.dex > dex.txt
grep -Eq "^[[:space:]]*Class descriptor[[:space:]]*:[[:space:]]*'Lorg/subyard/acceptance/MainActivity;'" dex.txt
stage=contents
jar tf app.apk > contents.txt
for entry in AndroidManifest.xml resources.arsc classes.dex; do grep -Fxq "$entry" contents.txt; done
printf 'android tiny APK: resources, Activity, API levels, DEX, alignment and signature=PASS bytes=%s\n' "$(stat -c %s app.apk)"
