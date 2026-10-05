#!/usr/bin/env bash
# Shared resources own hooks in projects-changed.d; selected agent hooks live in the list.
set -euo pipefail
observe_scope() {
  python3 - <<'SUBYARD_HOOK_OBSERVATION'
import hashlib,json,os,stat
from pathlib import Path
limit=4096
# Traversal and hashing share one path budget; observing a path never skips hashing.
observed_paths=set()
def observe_path(p,discovery=False):
 observed_paths.add(p)
 if len(observed_paths)>limit:
  raise RuntimeError('project discovery exceeds observation limit' if discovery else 'project hook scope exceeds observation limit')
def digest(paths,read=False):
 h=hashlib.sha256()
 for p in sorted(set(paths),key=str):
  observe_path(p)
  s=p.lstat()
  h.update(str(p).encode()+b'\0')
  h.update(str((s.st_mode,s.st_uid,s.st_gid,s.st_dev,s.st_ino,s.st_size,s.st_mtime_ns,s.st_ctime_ns)).encode()+b'\0')
  if stat.S_ISLNK(s.st_mode): h.update(os.readlink(p).encode())
  elif read and stat.S_ISREG(s.st_mode):
   if s.st_size>1048576: raise RuntimeError('project hook input exceeds observation limit')
   h.update(p.read_bytes())
 return h.hexdigest()
root=Path('/srv/workspaces')
projects=[]
roots=[]
if root.exists():
 for workspace in root.iterdir():
  projects.append(workspace)
  roots.append(str(workspace))
  if workspace.is_dir():
   for p in workspace.iterdir():
    projects.append(p)
    if p.name=='src' and p.is_dir():
     # Hook discovery inputs include nested repository roots, not working-tree contents.
     for base,dirs,files in os.walk(p,followlinks=False):
      observe_path(Path(base),discovery=True)
      if '.git' in dirs:
       projects.append(Path(base))
       projects.append(Path(base)/'.git')
       config=Path(base)/'.git/config'
       if config.exists(): projects.append(config)
       dirs.remove('.git')
      if '.git' in files: projects.append(Path(base)/'.git')
      if len(projects)>limit: raise RuntimeError('project hook scope exceeds observation limit')
wiring=[]
hookpaths=[]
resolved={}
def resolve_hook(hook):
 p=Path(hook)
 if p.is_absolute(): return str(p)
 if not hook or any(c not in 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-' for c in hook):
  raise RuntimeError('invalid installed hook command')
 for directory in ['/usr/local/bin','/usr/bin','/bin']:
  candidate=Path(directory)/hook
  if candidate.is_file() and os.access(candidate,os.X_OK): return str(candidate)
 return ''
def hook_sources(path):
 p=Path(path)
 paths=[p]
 if p.is_symlink(): paths.append(p.resolve(strict=True))
 return paths
for p in [Path('/usr/local/libexec/subyard/projects-changed'),Path('/etc/subyard/agent-project-hooks')]:
 if p.exists() or p.is_symlink(): wiring.append(p)
hookdir=Path('/usr/local/libexec/subyard/projects-changed.d')
if hookdir.exists():
 for p in hookdir.iterdir():
  if p.is_symlink() or not p.is_file(): raise RuntimeError('invalid installed hook')
  if os.access(p,os.X_OK): hookpaths.append(str(p));resolved[str(p)]=str(p)
  wiring.append(p)
listing=Path('/etc/subyard/agent-project-hooks')
if listing.exists():
 for line in listing.read_text().splitlines():
  if not line: continue
  path=resolve_hook(line)
  resolved[line]=path
  if path: wiring.extend(hook_sources(path))
  hookpaths.append(line)
print(json.dumps(dict(projects=digest(projects),wiring=digest(wiring,read=True),facts={hook:digest(hook_sources(path),read=True) for hook,path in sorted(resolved.items()) if path and (Path(path).exists() or Path(path).is_symlink())},resolved=resolved,hooks=sorted(set(hookpaths)),roots=sorted(roots)),separators=(',',':')))
SUBYARD_HOOK_OBSERVATION
}
if [ "${1:-}" = --observe ]; then
  observe_scope
  exit 0
fi
if [ -n "${SUBYARD_PROJECT_HOOK_SCOPE:-}" ]; then
  observed="$(observe_scope)"
  # Validate before invoking any hook. Keep approved paths as data, never shell code.
  approved="$(python3 - "$observed" <<'SUBYARD_HOOK_APPROVAL'
import json,os,sys
scope=json.loads(os.environ['SUBYARD_PROJECT_HOOK_SCOPE'])
observed=json.loads(sys.argv[1])
if observed['projects']!=scope['projects'] or observed['roots']!=scope['roots']:
 raise SystemExit('project hook project scope changed')
if any(observed['facts'].get(p)!=v for p,v in scope.get('facts',{}).items() if p in observed['facts']):
 raise SystemExit('project hook source changed')
if any(observed['resolved'].get(hook)!=path for hook,path in scope.get('resolved',{}).items() if observed['resolved'].get(hook)):
 raise SystemExit('project hook command identity changed')
if set(observed['hooks'])-set(scope['hooks']):
 raise SystemExit('project hook installed scope expanded')
for path in scope['hooks']:
 if (not path.startswith('/') and any(c not in 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-' for c in path)) or any(c in path for c in '\r\n\x00'):
  raise SystemExit('invalid approved project hook path')
for path in scope['hooks']:
 binding=scope.get('bindings',{}).get(path,'')
 if binding and binding!='conditional' and (len(binding)!=64 or any(c not in '0123456789abcdef' for c in binding)):
  raise SystemExit('invalid native project hook binding')
 resolved=observed['resolved'].get(path,'')
 if resolved: print(resolved+'\t'+binding)
SUBYARD_HOOK_APPROVAL
)"
  status=0
  while IFS=$'\t' read -r hook binding; do
    [ -n "$hook" ] && [ -x "$hook" ] || continue
    SUBYARD_PROJECT_HOOK_BINDING="$binding" "$hook" || status=1
  done <<< "$approved"
  exit "$status"
fi
status=0
for hook in /usr/local/libexec/subyard/projects-changed.d/*; do
  [ -x "$hook" ] || continue
  "$hook" || status=1
done
while IFS= read -r hook; do
  [ -n "$hook" ] || continue
  "$hook" || status=1
done < /etc/subyard/agent-project-hooks
exit "$status"
