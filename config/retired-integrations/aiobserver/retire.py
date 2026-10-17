#!/usr/bin/env python3
"""Retire only proven predecessor service controls and routes; keep all user data."""
import hashlib,ipaddress,json,os,re,subprocess,sys
ENV=os.environ
PROJECT=ENV['INCUS_PROJECT']
INSTANCE=ENV['YARD_INSTANCE_NAME']
PROVISION='user.subyard.ai_observer_provision'
PROXY='user.subyard.ai_observer_proxy'
DEFERRED='user.subyard.deferred_publication.ai-observer'
CONTROL_ASSESSMENT = r'''
wrapper=/usr/local/bin/ai-observer
marker=/etc/subyard/ai-observer/managed
unit=/etc/systemd/system/subyard-ai-observer.service
check=/usr/local/bin/ai-observer-check
installed=/usr/local/bin/ai-observer-installed
health=/usr/local/bin/ai-observer-health
managed='# Managed by Subyard AI Observer provision.'
present() { [ -e "$1" ] || [ -L "$1" ]; }
verify() {
  [ -f "$1" ] && [ ! -L "$1" ]
  [ "$(stat -c '%u:%g' "$1")" = '0:0' ]
  [ "$(stat -c '%a' "$1")" = "$2" ]
  grep -Fxq "$3" "$1"
  sha256sum "$1"
}
parent=/var/lib/subyard
if present "$parent"; then
  [ -d "$parent" ] && [ ! -L "$parent" ] && [ "$(stat -c '%u:%g' "$parent")" = 0:0 ]
  [ "$(stat -c '%a' "$parent")" = 755 ] || [ "$(stat -c '%a' "$parent")" = 700 ]
fi
receipt=/var/lib/subyard/ai-observer-retirement
if present "$receipt"; then
  [ -f "$receipt" ] && [ ! -L "$receipt" ] && [ "$(stat -c '%u:%g:%a' "$receipt")" = 0:0:600 ]
  [ "$(head -n 1 "$receipt")" = '# subyard-ai-observer-retirement-v1' ]
  tail -n +2 "$receipt" | awk '
    $0 !~ /^[0-9a-f]+  \/(usr\/local\/bin\/ai-observer(-check|-installed|-health)?|etc\/systemd\/system\/subyard-ai-observer.service|etc\/subyard\/ai-observer\/managed)$/ || length($1)!=64 {exit 1}
    END {if (NR!=4 && NR!=6) exit 1}'
  for path in "$wrapper" "$marker" "$unit" "$check" "$installed" "$health"; do
    if present "$path"; then
      [ -f "$path" ] && [ ! -L "$path" ] && [ "$(stat -c '%u:%g' "$path")" = 0:0 ]
      expected=$(awk -v path="$path" '$2==path {print $1}' "$receipt")
      [ -n "$expected" ] && [ "$(sha256sum "$path" | cut -d' ' -f1)" = "$expected" ]
    fi
  done
  sha256sum "$receipt"
elif present "$wrapper" || present "$marker" || present "$unit" || present "$check" || present "$installed" || present "$health"; then
  verify "$wrapper" 755 "$managed"
  verify "$marker" 644 subyard-ai-observer-v1
  verify "$unit" 644 "$managed"
  verify "$check" 755 "$managed"
  # Current wrappers declare both additional controls. A partial current set
  # must not be mistaken for a valid legacy installation.
  if present "$installed" || present "$health" || grep -q '^installed_path=' "$wrapper" || grep -q '^health_path=' "$wrapper"; then
    verify "$installed" 755 "$managed"
    verify "$health" 755 "$managed"
  fi
  if systemctl is-active --quiet subyard-ai-observer.service; then echo active; fi
  if systemctl is-enabled --quiet subyard-ai-observer.service; then echo enabled; fi
fi
'''

def run(*args, input=None):
    result=subprocess.run(args,input=input,capture_output=True,timeout=30)
    if result.returncode: raise ValueError('native-unavailable')
    return result.stdout

def guest(*args):
    return run('incus','exec',INSTANCE,'--project',PROJECT,'--',*args)

def incus(*args):
    return run('incus',*args,'--project',PROJECT)

def digest(value):
    return hashlib.sha256(json.dumps(value,sort_keys=True,separators=(',',':')).encode()).hexdigest()

def observe():
    snapshot=json.loads(run('incus','query',f'/1.0/instances/{INSTANCE}?project={PROJECT}'))
    config,devices=snapshot['config'],snapshot['devices']
    marker,device=config.get(PROXY,''),devices.get('ai-observer')
    for key in (PROXY,PROVISION,DEFERRED):
        if snapshot.get('expanded_config',config).get(key,'')!=config.get(key,''): raise ValueError('ambiguous-ownership')
    if snapshot.get('expanded_devices',devices).get('ai-observer')!=device: raise ValueError('ambiguous-ownership')
    provision=config.get(PROVISION,'')
    if provision and provision != 'pending' and not re.fullmatch('[a-f0-9]{64}',provision): raise ValueError('ambiguous-ownership')
    if marker:
        version,value=marker.split(':',1); value=value.removeprefix('pending:')
        host,port=('127.0.0.1',value) if version=='v1' else value.rsplit(':',1)
        if version not in ('v1','v2') or (version=='v2' and ipaddress.ip_address(host) not in ipaddress.ip_network('100.64.0.0/10')) or not port.isdecimal() or str(int(port))!=port or not 1024<=int(port)<=65535: raise ValueError('ambiguous-ownership')
        expected={'type':'proxy','bind':'host','listen':f'tcp:{host}:{port}','connect':'tcp:127.0.0.1:8080'}
        if device is not None and device!=expected: raise ValueError('foreign-ownership')
    elif device is not None: raise ValueError('foreign-ownership')
    saved=config.get(DEFERRED,'')
    if saved:
        def unique(pairs):
            result={}
            for key,value in pairs:
                if key in result: raise ValueError('ambiguous-ownership')
                result[key]=value
            return result
        deferred=json.loads(saved,object_pairs_hook=unique)
        if (not marker or version!='v2' or not isinstance(deferred,dict)
            or set(deferred)!={'version','ownership','device'} or type(deferred['version']) is not int
            or deferred['version']!=1 or deferred['ownership']!=marker or deferred['device']!=expected): raise ValueError('ambiguous-ownership')
    service=guest('sh','-eu','-c',CONTROL_ASSESSMENT).decode()
    names=guest('timeout','10','docker','container','ls','-a','--format','{{.Names}}').decode().splitlines()
    container=None
    if 'subyard-ai-observer' in names:
        rows=json.loads(guest('timeout','10','docker','container','inspect','subyard-ai-observer'))
        if len(rows)!=1: raise ValueError('ambiguous-ownership')
        row=rows[0]
        if not service or row['Name']!='/subyard-ai-observer' or row['Config']['Labels'].get('org.subyard.managed')!='ai-observer-v1': raise ValueError('foreign-ownership')
        # Do not hash credentials from native Env. Bind complete config locally via a
        # private digest; only the opaque outer CAS value crosses the lifecycle port.
        identity={k:row.get(k) for k in ('Id','Image','Config','HostConfig','Mounts')}
        # Docker mount enumeration order carries no ownership meaning. Keep every
        # complete record, including duplicates; all other arrays remain exact.
        if isinstance(identity['Mounts'],list):
            identity['Mounts']=sorted(identity['Mounts'],key=lambda mount:json.dumps(mount,sort_keys=True,separators=(',',':')))
        container={'id':row['Id'],'running':row['State']['Running'],'identity':digest(identity)}
    facts={'service':service,'container':container,'marker':marker,'device':device,'provision':config.get(PROVISION,''),'deferred':saved}
    current=not marker and device is None and not facts['provision'] and not service and not container and not saved
    if current: return {'state':'absent','actual':'','desired':''},facts
    desired=digest({'retired':True,'source':ENV.get('SUBYARD_INTEGRATION_SOURCE_BINDING','')})
    return {'state':'stale','actual':digest(facts),'desired':desired},facts

def main():
    observed,facts=observe()
    if sys.argv[1:]==['observe']: print(json.dumps(observed)); return
    if len(sys.argv)!=5 or sys.argv[1]!='apply' or [observed['actual'],observed['desired']]!=sys.argv[3:] or observed['state']!='stale': raise ValueError('observation-changed')
    # A second ownership read binds every control before removal, including native ID.
    checked,_=observe()
    if checked!=observed: raise ValueError('observation-changed')
    if facts['deferred']:
        incus('config','unset',INSTANCE,DEFERRED)
        # Revoke restoration before touching controls, then validate any route
        # that won the preceding native ETag race. Never remove a foreign route.
        _,fresh=observe()
        if (fresh['deferred'] or fresh['marker']!=facts['marker']
            or any(fresh[key]!=facts[key] for key in ('service','container','provision'))): raise ValueError('observation-changed')
        facts=fresh
    if facts['service']:
        guest('sh','-eu','-c','if [ -f /etc/systemd/system/subyard-ai-observer.service ]; then systemctl disable --now subyard-ai-observer.service; fi')
    if facts['container']:
        if facts['container']['running']: guest('timeout','20','docker','stop','--time','10',facts['container']['id'])
        guest('timeout','10','docker','rm',facts['container']['id'])
    if facts['service']:
        # Persist the exact control hashes before deletion so interrupted retirement
        # can validate the remaining files without requiring a complete legacy set.
        cleanup=CONTROL_ASSESSMENT+"""
receipt=/var/lib/subyard/ai-observer-retirement
if [ ! -e "$receipt" ]; then
  install -d -m 0755 /var/lib/subyard
  temporary=$(mktemp /var/lib/subyard/.ai-observer-retirement.XXXXXX)
  { printf '# subyard-ai-observer-retirement-v1\n';
    for path in "$wrapper" "$marker" "$unit" "$check" "$installed" "$health"; do
      [ ! -f "$path" ] || sha256sum "$path"
    done
  } > "$temporary"
  chmod 0600 "$temporary"
  mv -T "$temporary" "$receipt"
fi
rm -f -- "$wrapper" "$marker" "$unit" "$check" "$installed" "$health"
systemctl daemon-reload
rm -- "$receipt"
"""
        guest('sh','-eu','-c',cleanup)
    if facts['device'] is not None: incus('config','device','remove',INSTANCE,'ai-observer')
    for key in (PROXY,PROVISION):
        if facts['marker' if key==PROXY else 'provision']: incus('config','unset',INSTANCE,key)
    verified,_=observe()
    if verified['state']!='absent': raise ValueError('convergence-failed')
    print(json.dumps(verified))

if __name__=='__main__':
    try: main()
    except (ValueError,KeyError,OSError,subprocess.SubprocessError) as error:
        reason=str(error) if str(error) in ('native-unavailable','ambiguous-ownership','foreign-ownership','observation-changed','convergence-failed') else 'ambiguous-ownership'
        print('subyard-lifecycle-diagnostic-v1 '+reason,file=sys.stderr); sys.exit(1)
