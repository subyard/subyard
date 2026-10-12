#!/usr/bin/env python3
"""Actual GTK accessibility readiness and natural close inside the package VM fixture."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


def main():
    if os.environ.get('SUBYARD_E2E_VM') != '1' or os.getuid() == 0:
        raise SystemExit('requires an allocated unprivileged VM')
    pid = int(sys.argv[1])
    process = Path('/proc')/str(pid)
    start = process.joinpath('stat').read_text().rsplit(')',1)[1].split()[19]
    executable = process.joinpath('exe').stat()
    identity = (executable.st_dev, executable.st_ino)
    def owned():
        assert process.stat().st_uid == os.getuid()
        fields = process.joinpath('stat').read_text().rsplit(')',1)[1].split()
        assert fields[19] == start and fields[0] not in ('Z','X')
        current = process.joinpath('exe').stat()
        assert (current.st_dev,current.st_ino) == identity
    import gi
    gi.require_version('Atspi','2.0')
    from gi.repository import Atspi, GLib
    Atspi.set_timeout(250,500)
    Atspi.init()
    def nodes():
        owned()
        context = GLib.MainContext.default()
        for _ in range(64):
            if not context.pending(): break
            context.iteration(False)
        desktop = Atspi.get_desktop(0)
        count = desktop.get_child_count()
        assert 0 <= count <= 128
        apps = [desktop.get_child_at_index(i) for i in range(count)]
        apps = [app for app in apps if app and app.get_process_id() == pid]
        assert len(apps) <= 1
        if not apps: return []
        apps[0].clear_cache()
        result=[]; queue=[(apps[0],0)]; seen=set()
        while queue:
            node,depth=queue.pop()
            if not node or node in seen: continue
            seen.add(node)
            assert depth<=32 and len(result)<2048
            if node.get_state_set().contains(Atspi.StateType.DEFUNCT): continue
            result.append(node)
            count=node.get_child_count(); assert 0<=count<=512
            queue.extend((node.get_child_at_index(i),depth+1) for i in range(count))
        return result
    deadline=time.monotonic()+20
    kind=None
    while time.monotonic()<deadline:
        items=nodes()
        names={node.get_name() for node in items if node.get_state_set().contains(Atspi.StateType.SHOWING)}
        buttons=[node for node in items if node.get_name() in {'Refresh','Connect remote host'} and node.get_state_set().contains(Atspi.StateType.ENABLED)]
        if len(buttons)==2:
            if 'Connected' in names and any('Observed:' in name for name in names): kind='fleet'
            elif any(re.match(r'^[a-z][a-z0-9_]*: \S',name or '') for name in names): kind='actionable-owner-error'
        if kind: break
        time.sleep(.05)
    if not kind: raise RuntimeError('GTK readiness timeout')
    owned()
    windows=subprocess.check_output(['xdotool','search','--onlyvisible','--pid',str(pid)],timeout=3).splitlines()
    assert len(windows)==1 and windows[0].isdigit()
    window=windows[0].decode()
    assert int(subprocess.check_output(['xdotool','getwindowpid',window],timeout=3))==pid
    subprocess.run(['xdotool','windowactivate','--sync',window],check=True,timeout=3)
    subprocess.run(['xdotool','key','--clearmodifiers','alt+F4'],check=True,timeout=3)
    print(json.dumps({'gtk_readiness':kind,'natural_close_requested':True}),flush=True)


if __name__=='__main__': main()
