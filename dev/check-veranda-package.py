#!/usr/bin/env python3
"""Validate a real Veranda DEB and its source/ELF/notice bindings without installing."""
import argparse
import hashlib
import io
import json
import math
from pathlib import Path
import subprocess
import tarfile
import tempfile


def digest(data): return hashlib.sha256(data).hexdigest()


def checked_archive(archive,kind):
    data=subprocess.check_output(['dpkg-deb','--'+kind+'-tarfile',str(archive)],timeout=30)
    if len(data)>100*1024*1024: raise ValueError('unpacked archive exceeds bound')
    result={}
    with tarfile.open(fileobj=io.BytesIO(data),mode='r:') as source:
        members=source.getmembers()
        if len(members)>2048: raise ValueError('too many package members')
        for member in members:
            name=member.name.removeprefix('./')
            if name in {'','.'}: continue
            path=Path(name)
            if path.is_absolute() or '..' in path.parts or name in result or not (member.isfile() or member.isdir()):
                raise ValueError('package path/type escaped its boundary')
            if member.uid!=0 or member.gid!=0: raise ValueError('package owner must be root:root')
            expected=0o755 if member.isdir() or name=='usr/bin/subyard-veranda' else 0o644
            if member.mode!=expected: raise ValueError('package mode differs for '+name)
            result[name]=source.extractfile(member).read() if member.isfile() else None
    return result


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('deb',type=Path)
    parser.add_argument('--runtime-version',action='store_true',help='also run the ELF headless --version; native GTK shared libraries must be available')
    args=parser.parse_args(); archive=args.deb.resolve()
    manifest=json.loads(Path(str(archive)+'.manifest.json').read_text())
    provenance=json.loads(Path(str(archive)+'.provenance.json').read_text())
    assert digest(archive.read_bytes())==manifest['sha256']==provenance['sha256']
    assert Path(str(archive)+'.sha256').read_text()==manifest['sha256']+'  '+archive.name+'\n'
    inputs=provenance['publicInputs']
    assert digest((json.dumps(inputs,sort_keys=True,indent=2)+'\n').encode())==manifest['sourceInputsSha256']==provenance['sourceInputsSha256']
    assert len({row['path'] for row in inputs})==len(inputs)
    assert all(not Path(row['path']).is_absolute() and '..' not in Path(row['path']).parts and not any(part in {'private','.build','.git'} for part in Path(row['path']).parts) for row in inputs)
    if 'rustSourceInputs' in manifest:
        rust_inputs=manifest['rustSourceInputs']
        assert isinstance(rust_inputs,list) and rust_inputs and len(set(rust_inputs))==len(rust_inputs)
        assert set(rust_inputs)<={row['path'] for row in inputs}
    control=checked_archive(archive,'ctrl'); assert set(control)=={'control'}
    fields=dict(line.split(': ',1) for line in control['control'].decode().splitlines() if line and not line.startswith(' '))
    assert fields['Package']=='subyard-veranda' and fields['Version']==manifest['productVersion']
    assert fields['Architecture']=={'x86_64-unknown-linux-gnu':'amd64','aarch64-unknown-linux-gnu':'arm64'}[manifest['target']]
    assert fields['Depends']==manifest['runtimeDepends'] and 'openssh-client' in fields['Depends'].split(', ')
    payload=checked_archive(archive,'fsys')
    files={name:data for name,data in payload.items() if data is not None}
    assert all(name=='usr/bin/subyard-veranda' or name.startswith(('usr/share/doc/subyard-veranda/','usr/share/icons/hicolor/scalable/apps/','usr/share/applications/','usr/share/metainfo/')) for name in files)
    assert {name for name in files if name.startswith('usr/bin/')}=={'usr/bin/subyard-veranda'}
    assert digest(files['usr/bin/subyard-veranda'])==manifest['elfSha256']
    prefix='usr/share/doc/subyard-veranda/'
    build=json.loads(files[prefix+'build-info.json']); assert build==provenance['build']
    assert digest(files[prefix+'copyright'])==build['rootLicenseSha256']==next(row['sha256'] for row in inputs if row['path']=='LICENSE')
    assert digest(files[prefix+'rust-dependencies.json'])==build['rustDependencyNoticesSha256']
    licenses=json.loads(files[prefix+'rust-dependencies.json'])
    assert len(licenses)==manifest['rustDependencyCount'] and licenses
    for package in licenses:
        assert package['notices']
        for notice in package['notices']:
            assert notice['path'].startswith('rust/'+package['name']+'-'+package['version']+'/')
            assert digest(files[prefix+notice['path']])==notice['sha256']
    actual_bytes=sum(len(data) for data in files.values())
    assert actual_bytes==manifest['installedGuiPayloadBytes']
    assert int(fields['Installed-Size'])==manifest['installedSizeKiB']>=math.ceil(actual_bytes/1024)
    assert archive.stat().st_size==manifest['compressedGuiBytes']<=40*1024*1024
    assert manifest['installedSizeKiB']*1024<=80*1024*1024
    assert manifest['engineIncluded'] is False and manifest['systemDependenciesIncluded'] is False
    with tempfile.TemporaryDirectory(prefix='veranda-package-check-',dir='/tmp') as temporary:
        root=Path(temporary)
        for name in ['usr/share/applications/org.subyard.Veranda.desktop','usr/share/metainfo/org.subyard.Veranda.metainfo.xml']:
            path=root/Path(name).name; path.write_bytes(files[name]);path.chmod(0o644)
        subprocess.run(['desktop-file-validate',str(root/'org.subyard.Veranda.desktop')],check=True,timeout=10)
        subprocess.run(['appstreamcli','validate','--no-net',str(root/'org.subyard.Veranda.metainfo.xml')],check=True,timeout=20)
        assert b'Icon=org.subyard.Veranda\n' in files['usr/share/applications/org.subyard.Veranda.desktop']
        assert b'Exec=subyard-veranda\n' in files['usr/share/applications/org.subyard.Veranda.desktop']
        assert files['usr/share/icons/hicolor/scalable/apps/org.subyard.Veranda.svg'].startswith(b'<svg ')
        binary=root/'subyard-veranda'; binary.write_bytes(files['usr/bin/subyard-veranda']);binary.chmod(0o755)
        dynamic=subprocess.check_output(['readelf','--dynamic',str(binary)],text=True,timeout=10)
        import re
        assert re.findall(r'\(NEEDED\).*Shared library: \[([^\]]+)\]',dynamic)==manifest['dtNeeded']==build['dtNeeded']
        assert '(RPATH)' not in dynamic and '(RUNPATH)' not in dynamic
        if args.runtime_version:
            import os
            env=dict(os.environ,HOME=str(root/'home'),XDG_DATA_HOME=str(root/'data'))
            for name in ['DISPLAY','WAYLAND_DISPLAY','DBUS_SESSION_BUS_ADDRESS']: env.pop(name,None)
            assert subprocess.check_output([str(binary),'--version'],env=env,text=True,timeout=5).strip()=='Subyard Veranda '+manifest['productVersion']
            assert not (root/'home').exists() and not (root/'data').exists()
    print(json.dumps({'originalExitCode':0,'productVersion':manifest['productVersion'],'compressedGuiBytes':manifest['compressedGuiBytes'],'installedGuiPayloadBytes':actual_bytes,'installedSizeKiB':manifest['installedSizeKiB'],'rustDependencyCount':len(licenses),'runtimeVersionChecked':args.runtime_version,'nativeLifecycle':'NOT_RUN'},sort_keys=True))


if __name__=='__main__': main()
