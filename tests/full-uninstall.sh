#!/usr/bin/env bash
# Actual installed-runtime cold uninstall with Incus absent from the tool path.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d /tmp/subyard-full-uninstall.XXXXXX)"
trap '/bin/rm -rf -- "$fixture"' EXIT
python3 - "$ROOT" "$fixture" <<'PY_SIGNING_PTY'
import json,os,pathlib,subprocess,sys,time
root,temporary=map(pathlib.Path,sys.argv[1:])
driver=(root/'dev/e2e/full-uninstall.sh').read_text().split("<<'PY_AGENT'\n",1)[1].split('\nPY_AGENT',1)[0]
child='''import json,os,pathlib,select,sys,termios,time
state=pathlib.Path(sys.argv[0]).parent.parent
state.joinpath('child-pid').write_text(str(os.getpid()))
mode=state.joinpath('mode').read_text()
settings=termios.tcgetattr(0);settings[3]&=~termios.ECHO;termios.tcsetattr(0,termios.TCSANOW,settings)
if mode=='child-exit':sys.exit(42)
if mode=='no-prompt':sys.exit(0)
if mode=='timeout':time.sleep(60);sys.exit(45)
if mode=='misleading':
 print('Plan: key passphrase is read on the owner terminal after confirmation',flush=True)
 time.sleep(0.2);termios.tcflush(0,termios.TCIFLUSH)
prompt='Bad passphrase, try again for ' if mode=='bad-prompt' else 'Enter passphrase for '
os.write(1,(prompt+'synthetic-key: ').encode())
ready=select.select([0],[],[],3)[0]
accepted=bool(ready) and os.read(0,4096)==b'synthetic-fixture-passphrase\\n'
state.joinpath('accepted').write_text(json.dumps(accepted))
if mode=='repeat':
 os.write(1,b'Enter passphrase for synthetic-key: ')
 select.select([0],[],[],3)
 sys.exit(44)
sys.exit(0 if accepted else 42)
'''
sentinel=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)'])
try:
 for mode in ('misleading','normal','bad-prompt','repeat','child-exit','no-prompt','timeout'):
  state=temporary/('signing-'+mode);(state/'bin').mkdir(parents=True)
  (state/'mode').write_text(mode)
  executable=state/'bin/yard';executable.write_text('#!'+sys.executable+'\n'+child);executable.chmod(0o700)
  code=driver
  if mode=='timeout':
   # Accelerate only the deadline case; execute the actual unchanged 30s loop.
   code='import time\nclock=time.monotonic;start=clock()\ntime.monotonic=lambda:start+(clock()-start)*10\n'+driver
  result=subprocess.run([sys.executable,'-c',code,str(state)],capture_output=True,text=True,timeout=38)
  if mode in ('normal','misleading'):
   assert result.returncode==0 and (state/'accepted').read_text()=='true', mode+': real prompt was not answered'
  else:assert result.returncode!=0, mode+': refusal became success'
  assert 'synthetic-fixture-passphrase' not in result.stdout+result.stderr, mode+': private input escaped'
  facts=[json.loads(line.split('=',1)[1]) for line in result.stdout.splitlines() if line.startswith('FULL_UNINSTALL_SIGNING_PTY=')]
  assert len(facts)==1, mode+': missing bounded outcome'
  fact=facts[0]
  assert set(fact)=={'saw_prompt','sent_count','child_exit','deadline_reached','child_reaped','outcome'}, mode+': unexpected diagnostic fields'
  expected={'misleading':'unlocked','normal':'unlocked','bad-prompt':'bad_passphrase_prompt',
   'repeat':'repeated_prompt','child-exit':'child_failed','no-prompt':'missing_prompt','timeout':'deadline'}
  assert fact['outcome']==expected[mode], mode+': wrong outcome'
  assert fact['child_reaped'] and fact['sent_count'] in (0,1), mode+': child custody differs'
  if mode=='bad-prompt':assert fact['saw_prompt'] and fact['sent_count']==0, 'bad prompt received private input'
  if mode=='repeat':assert fact['sent_count']==1, 'repeated prompt received another response'
  if mode in ('normal','misleading'):assert fact['saw_prompt'] and fact['sent_count']==1 and fact['child_exit']==0, mode
  if mode=='child-exit':assert fact['child_exit']==42, 'native child exit was lost'
  if mode=='timeout':assert fact['deadline_reached'] and fact['child_exit']==-9, 'timeout did not reap owned child'
  pid=int((state/'child-pid').read_text())
  try:os.kill(pid,0)
  except ProcessLookupError:pass
  else:raise AssertionError(mode+': owned child survived')
  assert sentinel.poll() is None, mode+': unrelated child was signalled'
finally:
 sentinel.terminate();sentinel.wait(timeout=5)
print('ok: actual signing PTY waits for OpenSSH prompts, refuses repeats, retains exits and cleans only its child')
PY_SIGNING_PTY
version=0.17.0-test.full-uninstall
release="$fixture/release"
bash "$ROOT/dev/package-engine.sh" --output-dir "$release" --version "$version" --arch amd64 >/dev/null
bundle="$release/subyard-$version-linux-amd64.tar.gz"
export HOME="$fixture/home" SUBYARD_OPERATOR_HOME="$fixture/home"
# A sibling runtime/cache must not rely on audit logging to create DataHome.
export SUBYARD_NO_AUDIT=1
export SUBYARD_HOME="$fixture/data" SUBYARD_CONFIG_HOME="$fixture/config"
export YARD_RELEASE_CACHE="$fixture/cache"
export SUBYARD_POWER_UNIT_PATH="$fixture/units/subyard-power-reconcile.service"
export SUBYARD_POWER_RECONCILER_PATH="$fixture/helper"
export SUBYARD_KEYS_SYSTEMD_DIR="$fixture/user-units"
export SUBYARD_TEST_VMS_SINK_SERVICE_PATH="$fixture/units/subyard-test-vms-host-sink.service"
export SUBYARD_TEST_VMS_SINK_TIMER_PATH="$fixture/units/subyard-test-vms-host-sink.timer"
export SUBYARD_TEST_VMS_SINK_PATH="$fixture/sink"
mkdir -p "$HOME" "$fixture/tools"
runtime="$fixture/custom-runtime"
bash "$ROOT/scripts/install-runtime-release.sh" --runtime-root "$runtime" \
  --bundle "$bundle" --checksum "$bundle.sha256" --manifest "$bundle.manifest.json" \
  --provenance "$bundle.provenance.json" >/dev/null
# Keep only the two actual shell/Python entrypoint dependencies. No incus,
# systemctl, sudo or status binary is available to accidentally mask this case.
for command in bash dirname readlink python3 env; do
  ln -s "$(command -v "$command")" "$fixture/tools/$command"
done
unset YARD_ENGINE_PATH SUBYARD_INCUS_SOCKET
export PATH="$fixture/tools" INCUS_DIR="$fixture/never-created-incus" INCUS_SOCKET=''
if ! "$runtime/current/bin/yard" uninstall --full --yes > "$fixture/result.log" 2>&1; then
  /bin/cat "$fixture/result.log" >&2
  exit 1
fi
[ ! -e "$runtime" ] && [ ! -e "$SUBYARD_HOME" ] && [ ! -e "$SUBYARD_CONFIG_HOME" ] \
  && [ ! -e "$SUBYARD_HOME.uninstall" ] \
  || { /bin/cat "$fixture/result.log" >&2; exit 1; }
/bin/grep -Fq 'Fully removed this Subyard installation' "$fixture/result.log"
printf 'ok: current installed custom runtime fully uninstalls before Incus or host prerequisites exist\n'
# Supported bootstrap owns exact versioned cache artifacts. Foreign content
# beside them, including inside the same version directory, must survive.
for command in jq curl tar gzip stat uname sha256sum awk cut sed grep install mkdir mktemp mv rm ln chmod sync cp cmp sort tr cat getent readlink dirname basename flock find xargs touch date wc head id sleep; do
  for directory in /usr/bin /bin /usr/local/bin; do
    if [ -x "$directory/$command" ]; then /bin/ln -sf "$directory/$command" "$fixture/tools/$command"; break; fi
  done
done
export SUBYARD_HOME="$fixture/bootstrap-data" SUBYARD_CONFIG_HOME="$fixture/bootstrap-config"
export YARD_RUNTIME_ROOT="$fixture/bootstrap-runtime" YARD_RELEASE_CACHE="$fixture/bootstrap-cache"
export YARD_BIN_DIR="$fixture/bootstrap-bin" YARD_SHELL_RC="$fixture/bootstrap.rc" YARD_LOGIN_RC="$fixture/bootstrap.login"
export YARD_RELEASE_BASE_URL="file://$release"
# Existing unsafe targets must refuse before activating or creating inventory.
mkdir -m 0700 "$fixture/foreign-data-target"
ln -s "$fixture/foreign-data-target" "$SUBYARD_HOME"
if bash "$ROOT/dev/bootstrap-runtime.sh" --version "$version" --yes > "$fixture/unsafe-data.log" 2>&1; then exit 1; fi
grep -Fq 'unsafe installation data directory' "$fixture/unsafe-data.log"
/bin/unlink "$SUBYARD_HOME"
mkdir -m 0777 "$SUBYARD_HOME"
if bash "$ROOT/dev/bootstrap-runtime.sh" --version "$version" --yes > "$fixture/unsafe-data.log" 2>&1; then exit 1; fi
grep -Fq 'unsafe installation data directory' "$fixture/unsafe-data.log"
/bin/rmdir "$SUBYARD_HOME"
if ! bash "$ROOT/dev/bootstrap-runtime.sh" --version "$version" --yes > "$fixture/bootstrap.log" 2>&1; then
  /bin/cat "$fixture/bootstrap.log" >&2; exit 1
fi
[ "$(stat -c '%a' "$SUBYARD_HOME")" = 700 ]
printf 'foreign cache control\n' > "$YARD_RELEASE_CACHE/$version/foreign.control"
if ! "$YARD_BIN_DIR/yard" teardown --full --yes > "$fixture/bootstrap-full.log" 2>&1; then
  /bin/cat "$fixture/bootstrap-full.log" >&2; exit 1
fi
[ ! -e "$YARD_RUNTIME_ROOT" ] && [ ! -e "$SUBYARD_HOME" ] && [ ! -e "$SUBYARD_CONFIG_HOME" ] && [ ! -e "$YARD_BIN_DIR/yard" ] \
  || { /bin/cat "$fixture/bootstrap-full.log" >&2; exit 1; }
/bin/grep -Fxq 'foreign cache control' "$YARD_RELEASE_CACHE/$version/foreign.control"
[ "$(/usr/bin/find "$YARD_RELEASE_CACHE" -type f | /usr/bin/wc -l)" = 1 ] || { /bin/cat "$fixture/bootstrap-full.log" >&2; exit 1; }
printf 'ok: supported bootstrap cache artifacts removed; foreign file in the same version preserved\n'
