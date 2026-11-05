"""Rebind only owned OpenLit identity receipts; retain containers and volumes."""
import grp
import json
import os
from pathlib import Path
import pwd
import sys
import authority
import native
import runtime
import trace_cutover

TIMER='subyard-openlit-trace-cutover.timer'
SERVICE='subyard-openlit-trace-cutover.service'


def trace_projection(state,original,target):
    if state.is_symlink():raise ValueError('unsafe OpenLit identity state')
    if not state.exists():return None,None
    if trace_cutover.admission(state) is not None:
        raise ValueError('pending trace admission must finish before identity transition')
    record=trace_cutover.record(state)
    if record is None:return None,None
    if record['phase']!='retired' or record['home'] not in (original['home'],target['home']) or record['uid']!=original['uid']:
        raise ValueError('pending or foreign trace cutover must finish before identity transition')
    before=dict(record,home=original['home'])
    after=dict(record,home=target['home'])
    return runtime.digest(before),runtime.digest(after)


def projection(state,source,unit,home,selected,agents,user):
    observation,facts=runtime.capture(state,source,unit,home,selected,agents,user)
    if facts.get('probe_failed'):
        raise ValueError('OpenLit identity native observation failed')
    return observation,facts


def execute(mode,selected,user,agents,arguments,source=None,state=Path('/var/lib/subyard/openlit'),unit=Path('/etc/systemd/system/subyard-openlit.service')):
    source=source or Path(__file__).resolve().parent
    binding=arguments[-1]
    try:
        journal,recipe=authority.load(binding,'openlit')
        phase=journal['phase']
        restarted=authority.rebooted(journal)
        original,target=recipe['original'],recipe['target']
    except FileNotFoundError:
        if mode!='identity-observe':raise
        account=pwd.getpwnam(user)
        original=dict(user=user,uid=account.pw_uid,gid=account.pw_gid,home=account.pw_dir,group=grp.getgrgid(account.pw_gid).gr_name)
        target=dict(original,user=arguments[0],home='/home/'+arguments[0],group=arguments[0])
        phase=None
        restarted=False
    home=Path(target['home'] if phase in ('home','account','account-ready') else original['home'])
    traces=trace_projection(state,original,target)
    observed,facts=projection(state,source,unit,home,selected,agents,original['user'])
    if not state.exists() and any(path.exists() or path.is_symlink() for path in (unit,unit.with_name(TIMER),unit.with_name(SERVICE))):
        raise ValueError('unowned OpenLit identity units')
    if phase is None:
        if observed['state'] not in ('current','absent'):
            raise ValueError('OpenLit identity requires verified installed ownership')
        installed=state.exists()
        receipt=json.loads((state/'receipt.json').read_text()) if installed and runtime.owned_file(state/'receipt.json',0o600) else None
        if installed and (not runtime.valid_receipt(receipt) or receipt!={'schema':1,'desired':observed['desired'],'containers':facts['containers']}):
            raise ValueError('OpenLit identity receipt changed')
        recipe=dict(schema=1,source=binding,original=original,target=target,installed=installed,facts=facts,trace=list(traces),receipt=receipt,
                    desired=runtime.recipe(source,selected,agents,target['user']))
    else:
        if state.exists()!=recipe['installed'] or list(traces)!=recipe['trace']:
            raise ValueError('OpenLit identity state changed')
        expected=recipe['facts']
        if recipe['installed']:
            current=facts.get('timer',{})
            if not authority.active_allowed(current.get('ActiveState'),expected['timer']['ActiveState'],expected['timer']['UnitFileState'],restarted) or current.get('UnitFileState')!=expected['timer']['UnitFileState']:
                raise ValueError('OpenLit trace timer intent changed')
            facts['timer']=expected['timer']
        if facts!=expected or runtime.recipe(source,selected,agents,target['user'])!=recipe['desired']:
            raise ValueError('OpenLit identity native scope changed')
    if mode=='identity-observe':return recipe
    if not recipe['installed']:return None
    receipt_path=state/'receipt.json'
    receipt=json.loads(receipt_path.read_text()) if runtime.owned_file(receipt_path,0o600) else None
    desired_receipt=dict(recipe['receipt'],desired=recipe['desired'])
    if receipt not in (recipe['receipt'],desired_receipt):raise ValueError('OpenLit identity receipt changed')
    if phase in ('prepared','quiesced','home','account'):
        for name in (TIMER,SERVICE):runtime.run(['systemctl','stop',name])
        return None
    if phase!='account-ready':raise ValueError('invalid OpenLit identity phase')
    if traces[0] is not None:
        record=trace_cutover.record(state)
        if record['home']==original['home']:
            if runtime.digest(record)!=traces[0]:raise ValueError('OpenLit trace identity changed')
            trace_cutover.save(state,dict(record,home=target['home']))
    if recipe['facts']['timer']['ActiveState']=='active':runtime.run(['systemctl','start',TIMER])
    else:runtime.run(['systemctl','stop',TIMER])
    current,current_facts=projection(state,source,unit,Path(target['home']),selected,agents,target['user'])
    if current['state']!='current' or current_facts['containers']!=recipe['receipt']['containers']:
        raise ValueError('OpenLit identity verification failed')
    if receipt!=desired_receipt:native.write(receipt_path,json.dumps(desired_receipt)+'\n')
    return None


def main():
    mode,selected,user,agents,*arguments=sys.argv[1:]
    if mode=='identity-observe':
        result=execute(mode,selected=='1',user,agents.split(),arguments)
    else:
        descriptor=trace_cutover.acquire()
        try:result=execute(mode,selected=='1',user,agents.split(),arguments)
        finally:os.close(descriptor)
    if result is not None:print(json.dumps(result))


if __name__=='__main__':
    try:main()
    except (ValueError,OSError,KeyError,TypeError) as error:
        print(authority.refusal('OpenLit',error),file=sys.stderr)
        raise SystemExit(1)
