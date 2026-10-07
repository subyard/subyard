#!/usr/bin/python3
import math,os,re,signal,stat,subprocess as sp,sys,time
from pathlib import Path
class AmbiguousSelection(Exception): pass
def terminal_label(value):
    # Pinned Code replaces its initial input label with the terminal ID/title when attaching.
    return isinstance(value,str) and len(value)<=8192 and '\x00' not in value and (value=='Terminal input' or
        re.fullmatch(r'Terminal [1-9][0-9]*(?:, [^\x00]+)?'
            r'(?:\nRun the command: Toggle Screen Reader Accessibility Mode for an optimized screen reader experience)?'
            r'(?:\nUse [^\x00]+ for terminal accessibility help)?',value) is not None)
def private(p,folder=False):
    info = p.lstat()
    assert info.st_uid == os.geteuid()
    assert stat.S_ISDIR(info.st_mode) if folder else stat.S_ISREG(info.st_mode) and info.st_nlink == 1
    assert stat.S_IMODE(info.st_mode) == (0o700 if folder else 0o600)
def fixture(root):
    private(root,True);private(root/'.marker');private(root/'editor',True)
    private(root/'editor/.marker')
    assert (root/'.marker').read_text() == 'subyard-veranda-native-v1\n'
    assert (root/'editor/.marker').read_text() == 'subyard-veranda-editor-v1\n'
def activate():
    print('native-stage: editor.activation-start',flush=True)
    def expired(*_): raise TimeoutError()
    signal.signal(signal.SIGALRM,expired)
    signal.alarm(5)
    try:
        root=Path(sys.argv[2]);fixture(root)
        assert sys.argv[3] == '/usr/bin/env'
        for var,name in [('HOME','home'),('XDG_CONFIG_HOME','config'),
                               ('XDG_CACHE_HOME','cache'),('XDG_RUNTIME_DIR','runtime')]:
            p = root/'editor' / name;private(p,True)
            assert os.environ.get(var) == str(p)
        assert not any(name in os.environ for name in ['DCONF_PROFILE','GSETTINGS_BACKEND',
            'GSETTINGS_SCHEMA_DIR','GIO_EXTRA_MODULES','GIO_MODULE_DIR'])
        addr = os.environ.get('DBUS_SESSION_BUS_ADDRESS','')
        assert re.fullmatch(r'unix:(?:path|abstract)=[A-Za-z0-9_/.-]{1,256}(?:,guid=[0-9a-fA-F]{32})?',addr)
        import gi
        gi.require_version('Gio','2.0')
        from gi.repository import Gio,GLib
        bus = Gio.DBusConnection.new_for_address_sync(addr,
            Gio.DBusConnectionFlags.AUTHENTICATION_CLIENT | Gio.DBusConnectionFlags.MESSAGE_BUS_CONNECTION,
            None,None)
        def call(service,p,iface,method,params,out):
            return bus.call_sync(service,p,iface,method,params,
                GLib.VariantType.new(out),Gio.DBusCallFlags.NONE,1000,None)
        service='org.freedesktop.DBus'
        def bus_id(method):
            return call(service,'/org/freedesktop/DBus',service,method,GLib.Variant('(s)',(service,)),'(u)').unpack()[0]
        assert bus_id('GetConnectionUnixUser') == os.geteuid()
        proc=Path('/proc',str(bus_id('GetConnectionUnixProcessID')))
        start=(proc/'stat').read_text().rsplit(')',1)[1].split()[19]
        def owned_bus():
            assert proc.stat().st_uid == os.geteuid()
            fields = (proc/'stat').read_text().rsplit(')',1)[1].split()
            assert int(fields[1]) == os.getppid() and fields[19] == start
            assert (proc/'exe').resolve() == Path('/usr/bin/dbus-daemon').resolve()
        owned_bus()
        call('org.a11y.Bus','/org/a11y/bus','org.freedesktop.DBus.Properties','Set',
            GLib.Variant('(ssv)',('org.a11y.Status','IsEnabled',GLib.Variant('b',True))),'()')
        out=call('org.a11y.Bus','/org/a11y/bus','org.freedesktop.DBus.Properties','Get',
            GLib.Variant('(ss)',('org.a11y.Status','IsEnabled')),'(v)')
        assert out.unpack() == (True,)
        owned_bus()
        print('native-stage: editor.activation-ready',flush=True)
        signal.alarm(0)
        os.execv(sys.argv[3],sys.argv[3:])
    except BaseException:
        print('native-stage: editor.activation-missing', flush=True)
        return 1
    finally:
        signal.alarm(0)
def main():
    step='accessibility'
    pt='arguments'
    progress_at=0
    alarm_error=None
    def emit(val):
        print('native-stage: editor.' + val, flush=True)
    def check(val):
        nonlocal pt,progress_at
        pt=val
        if step=='choice':
            now=time.monotonic()
            if now>=progress_at:
                progress_at=now+0.5
                emit('checkpoint.' + val)
    def expired(*_):
        nonlocal alarm_error
        alarm_error=TimeoutError()
        raise alarm_error
    signal.signal(signal.SIGALRM, expired)
    try:
        root=Path(sys.argv[1]);wid=sys.argv[2];ms=int(sys.argv[3])
        assert wid.isdecimal() and len(wid) <= 20 and 0 < ms <= 30000
        deadline = time.monotonic() + ms/1000
        signal.alarm(math.ceil(ms/1000))
        fixture(root)
        assert os.environ.get('DBUS_SESSION_BUS_ADDRESS')
        token=root.name.removeprefix('native.')
        assert token.isascii() and token.isalnum() and 0<len(token)<=32
        val=sys.stdin.read(1025)
        assert val==f'umask 077; set -C; printf \'%s\\n\' "$PWD" > /tmp/veranda-editor.{token}/pwd; chmod 0600 /tmp/veranda-editor.{token}/pwd; exit'
        step='accessibility-import';emit(step)
        import gi
        gi.require_version('Atspi', '2.0')
        from gi.repository import Atspi, GLib
        Role=Atspi.Role;State=Atspi.StateType;Action=Atspi.Action
        step='accessibility-init';emit(step)
        Atspi.set_timeout(250, 500);Atspi.init()
        def xdo(args,read=False):
            left=deadline-time.monotonic();assert left>0
            return sp.run(['xdotool',*args],check=True,stdout=sp.PIPE if read else sp.DEVNULL,
                stderr=sp.DEVNULL,timeout=left).stdout
        step='window-binding';emit(step)
        pid=int(xdo(['getwindowpid', wid], True))
        proc=Path('/proc', str(pid));start=(proc/'stat').read_text().rsplit(')', 1)[1].split()[19]
        def owned():
            check('owned-uid')
            assert proc.stat().st_uid == os.geteuid()
            check('owned-executable')
            assert (proc/'exe').resolve() == root/'editor/VSCode-linux-x64/code'
            check('owned-start')
            assert (proc/'stat').read_text().rsplit(')', 1)[1].split()[19] == start
        owned();emit('window-bound');step='application'
        appseen=False
        dup = set()
        def find_app():
            nonlocal appseen, step
            owned();check('application-desktop');desktop = Atspi.get_desktop(0)
            check('application-count');cnt=desktop.get_child_count();found=[]
            for i in range(cnt):
                check('application-child');child = desktop.get_child_at_index(i)
                if child is None: continue
                check('application-pid')
                if child.get_process_id() == pid: found.append(child)
            check('application-unique')
            assert len(found) <= 1
            if found and not appseen:
                appseen=True
                emit('application-seen');step='document'
            return found[0] if found else None
        def walk(node):
            out=[];seen=set();queue=[(node,0,())]
            while queue:
                item, depth, p = queue.pop()
                if item is None: continue
                check('walk-identity')
                if item in seen:
                    kind='self' if p and item == p[-1] else 'ancestor' if item in p else 'duplicate'
                    if kind not in dup:
                        dup.add(kind);emit('walk-repeat-' + kind)
                    continue
                check('walk-node-limit');assert len(out) < 2048
                check('walk-depth-limit');assert depth <= 64
                try:
                    check('walk-state')
                    if item.get_state_set().contains(State.DEFUNCT): continue
                    out.append(item);seen.add(item)
                    check('walk-children')
                    queue.extend((item.get_child_at_index(i), depth + 1, p + (item,))
                                 for i in range(item.get_child_count()))
                except GLib.Error: continue
            return out
        pending_read=None
        def wait(test):
            nonlocal pending_read
            pending_read=None
            loop=GLib.MainLoop();out=[];err=[];first=None
            def observe():
                nonlocal pending_read,first
                try:
                    pending_read=None
                    if time.monotonic() >= deadline:
                        if first: check(first[1]);raise first[0]
                        raise TimeoutError()
                    app=find_app()
                    active=test(walk(app)) if app else None
                    if active: out.append(active);loop.quit();return False
                except BaseException as error:
                    if error is alarm_error and first and time.monotonic() >= deadline:
                        check(first[1]);error=first[0]
                    if pending_read and error is pending_read[0]:
                        if time.monotonic() < deadline:
                            if first is None: first=pending_read
                            return True  # Discard this entire observation; re-read on the next tick.
                        if first: check(first[1]);error=first[0]
                    err.append(error);loop.quit();return False
                return True
            source=GLib.timeout_add(50, observe)
            try: loop.run()
            except TimeoutError as error:
                if error is alarm_error and first and time.monotonic() >= deadline: check(first[1]);raise first[0]
                raise
            finally:
                pending_read=None
                if not out and not err: GLib.source_remove(source)
            if err: raise err[0]
            assert out
            return out[0]
        def api_read(node, field):
            nonlocal pending_read
            pending_read=None
            assert field in {'role', 'name'}
            check('predicate-' + field)
            try: return node.get_role() if field == 'role' else node.get_name()
            except GLib.Error as error:
                kind='other-glib'
                if error.domain == 'atspi_error':
                    if error.code == 0: kind='application-gone'
                    elif error.code == 1: kind='ipc'
                status='recheck-failed'
                if deadline-time.monotonic() >= 0.5:
                    try: status='defunct' if node.get_state_set().contains(State.DEFUNCT) else 'live'
                    except BaseException: pass
                check('predicate-' + field + '-' + kind + '-' + status)
                if kind == 'ipc' and status == 'defunct': pending_read=(error,pt)
                raise
        def role(node): return api_read(node, 'role')
        def name(node): return api_read(node, 'name')
        def state(node): check('predicate-state');return node.get_state_set()
        def focused(node):
            flags=state(node)
            return flags.contains(State.FOCUSED) and flags.contains(State.ENABLED)
        def unique(values):
            check('predicate-unique')
            if len(values) > 1: raise AmbiguousSelection()
            return values[0] if values else None
        seen = set()
        agreed=False
        def trust_button(nodes):
            node=unique([n for n in nodes if role(n) == Role.PUSH_BUTTON and name(n) == 'Trust Folder & Continue'])
            if node: assert state(node).contains(State.ENABLED)
            return node
        def consent(node):
            nonlocal agreed, step
            assert not agreed
            step='trust';emit('trust-choice');owned()
            act=node.get_action_iface()
            assert act
            cnt=Action.get_n_actions(act);assert 0<cnt<=8
            ids=[i for i in range(cnt) if Action.get_action_name(act,i) in {'press','click'}]
            assert len(ids)==1 and Action.do_action(act,ids[0])
            wait(lambda nodes: not trust_button(nodes))
            agreed=True;emit('trust-accepted')
        def frame():
            fixture(root);owned()
            try:
                fd=os.open(root/'editor.failure.request',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
                with os.fdopen(fd,'w') as output:
                    os.fchmod(output.fileno(),0o600);output.write('editor-palette-readiness-v1\n')
            except OSError: pass
        def mark(val):
            if val not in seen:
                seen.add(val);emit('palette-state.' + val)
                if val=='input-absent': frame()
        def palette_ready(nodes):
            trust=trust_button(nodes)
            if trust: return trust
            mark('input-scan')
            proc=unique([n for n in nodes if role(n) in {Role.ENTRY, Role.COMBO_BOX}
                and name(n)=='Type the name of a command to run.'
                and state(n).contains(State.ENABLED)])
            if proc is None:
                mark('input-absent');return None
            mark('input-present')
            if focused(proc):
                mark('direct-focus');return proc
            ownset=set(nodes);boxes=set();rows=set()
            mark('relation-scan')
            check('predicate-relations');rels=proc.get_relation_set() or []
            assert len(rels) <= 2048
            for rel in rels:
                check('predicate-relations')
                if rel.get_relation_type() != Atspi.RelationType.CONTROLLER_FOR: continue
                cnt=rel.get_n_targets();assert 0 <= cnt <= 2048
                for i in range(cnt):
                    item=rel.get_target(i)
                    if item in ownset and role(item)==Role.LIST_BOX: boxes.add(item)
            mark('owned-listbox-present' if boxes else 'owned-listbox-absent')
            for item in boxes:
                rows.update(n for n in walk(item) if n in ownset and role(n)==Role.LIST_ITEM
                    and focused(n) and state(n).contains(State.SELECTED))
            row=unique(list(rows))
            mark('active-descendant-ready' if row else 'focused-selected-row-absent')
            return proc if row else None
        def open_palette():
            nonlocal step
            step='palette-binding';emit('palette-open')
            owned();emit('palette-window-bound')
            step='palette-focus';check('gesture-focus');xdo(['windowfocus', wid]);emit('palette-window-focused')
            step='palette-key';check('gesture-key');xdo(['key', 'ctrl+shift+p']);emit('palette-key-sent')
            step='palette'
            seen.clear();mark('observe-start')
        def palette(cmd,phase):
            nonlocal step
            open_palette()
            ready=wait(palette_ready)
            if role(ready)==Role.PUSH_BUTTON:
                assert phase=='create-choice'
                consent(ready);open_palette();ready=wait(palette_ready)
                assert role(ready) != Role.PUSH_BUTTON
            emit('palette-ready');step='palette-binding';owned()
            step='palette-type';check('gesture-type')
            xdo(['type', '--clearmodifiers', '--delay', '1', cmd])
            step='choice';emit(step)
            choice={'create-choice':'choice-create','focus-choice':'choice-focus'}[phase]
            mark(choice)
            def exact(nodes, active=True):
                hits=[n for n in nodes if role(n)==Role.LIST_ITEM and (not active or state(n).contains(State.SELECTED))
                    and re.fullmatch(re.escape(cmd)+r'(?:, Control\+'+(r'Shift\+`' if phase=='create-choice' else 'DownArrow')+r')?(?:, (?:recently used|similar commands|commonly used|other commands))?',name(n) or '')]
                if not hits and 'choice' not in seen: seen.add('choice');frame()
                return unique(hits)
            def choice_ready(nodes):
                item=exact(nodes,False)
                if item is None:
                    mark(choice + '-target-absent');return None
                owned();par=item.get_parent();box=par;trail=set()
                for _ in range(64):
                    check('application-pid');assert box and box not in trail and box.get_process_id()==pid
                    if role(box)==Role.LIST_BOX: break
                    trail.add(box);box=box.get_parent()
                else: raise AssertionError()
                rows=[n for n in walk(box) if n.get_parent()==par and role(n)==Role.LIST_ITEM]
                row=unique([n for n in rows if state(n).contains(State.SELECTED)])
                check('predicate-unique');assert row
                for n in [item,row]:
                    check('application-pid');assert n.get_process_id()==pid
                    assert state(n).contains(State.ENABLED)
                check('choice-indices');ids=[n.get_index_in_parent() for n in rows]
                check('choice-order');assert len(set(ids))==len(ids) and all(0<=i<2048 for i in ids)
                rows=[n for _,n in sorted(zip(ids,rows),key=lambda p:p[0])]
                a=rows.index(item);b=rows.index(row)
                check('choice-adjacent')
                if item!=row and abs(a-b)>1: mark(choice + '-nonadjacent')
                return (item,row,a,b)
            item,row,a,b=wait(choice_ready)
            if a!=b:
                owned();assert int(xdo(['getwindowfocus'],True))==int(wid)
                xdo(['key','--repeat',str(abs(a-b)),'--delay','1','Down' if a>b else 'Up'])
            mark(choice + '-selection-check')
            assert wait(exact)==item
            emit(phase);owned();check('gesture-key');xdo(['key', 'Return'])
        initial = wait(lambda nodes: nodes if any(role(n) == Role.DOCUMENT_WEB for n in nodes) else None)
        emit('document-ready');emit('accessibility-ready')
        trust=trust_button(initial)
        if trust: consent(trust)
        palette('Terminal: Create New Terminal', 'create-choice')
        step='trust'
        if not agreed: consent(wait(trust_button))
        assert agreed
        palette('Terminal: Focus Terminal', 'focus-choice')
        step='terminal'
        wait(lambda nodes: unique([n for n in nodes if role(n) in {Role.ENTRY, Role.TEXT}
             and terminal_label(name(n)) and focused(n)]))
        emit('terminal-focused');step='input';owned()
        check('gesture-type');xdo(['type', '--clearmodifiers', '--delay', '1', val])
        check('gesture-key');xdo(['key', 'Return'])
        emit('input-sent')
        return 0
    except BaseException as error:
        emit('checkpoint.' + pt)
        classes=[TimeoutError,sp.TimeoutExpired,sp.CalledProcessError,AmbiguousSelection,
            AssertionError,AttributeError,TypeError,ValueError,OSError]
        kind=next((cls.__name__ for cls in classes if isinstance(error,cls)),'Other')
        if 'GLib' in locals() and isinstance(error,GLib.Error): kind='GLibError'
        emit('exception.' + kind)
        if step.startswith('palette'):
            if isinstance(error, TimeoutError): err='palette-missing'
            elif isinstance(error, AmbiguousSelection): err='palette-ambiguous'
            elif isinstance(error, GLib.Error): err='palette-rpc-exception'
            elif step in {'palette-binding', 'palette-focus', 'palette-key', 'palette-type'}: err=step + '-failed'
            else: err='palette-observation-failed'
            emit(err)
        else: emit(step + '-missing')
        return 1
    finally:
        signal.alarm(0)
if __name__ == '__main__':
    sys.exit(activate() if sys.argv[1:2] == ['--activate'] else main())
