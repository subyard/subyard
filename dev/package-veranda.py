#!/usr/bin/env python3
"""Build a separate Linux GTK DEB from current public inputs; never install it."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import stat
import subprocess
import tempfile
import tomllib

ROOT = Path(__file__).resolve().parents[1]
TOOLCHAIN = '1.88.0'
APP_ID = 'org.subyard.Veranda'


def stable_version(value):
    if not re.fullmatch(r'(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)', value) or len(value) > 64:
        raise ValueError('version must be explicit stable MAJOR.MINOR.PATCH')
    return value


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def encoded(value):
    return (json.dumps(value, sort_keys=True, indent=2) + '\n').encode()


def run(command, **kwargs):
    return subprocess.run(command, check=True, timeout=kwargs.pop('timeout', 120), **kwargs)


def no_symlink_parents(path):
    path = path.absolute()
    if '..' in path.parts:
        raise ValueError('input/output path must not contain parent traversal')
    for parent in [path, *path.parents]:
        if parent.is_symlink():
            raise ValueError('input/output path must not contain symlinks')
    return path


def plain(path):
    no_symlink_parents(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise ValueError('input must be a plain independent file')
    return path


def output_path(path):
    path = no_symlink_parents(path).resolve()
    root = no_symlink_parents(ROOT).resolve()
    if path == root or path in root.parents or path.is_relative_to(root) and not path.is_relative_to(root / '.build'):
        raise ValueError('checkout outputs must stay under .build')
    # Existing Cargo/output descendants must not redirect later generated writes.
    if path.is_dir():
        for directory, folders, files in os.walk(path, followlinks=False):
            if any((Path(directory)/name).is_symlink() for name in folders + files):
                raise ValueError('output tree must not contain symlinks')
    return path


def owning_tree(crate):
    """Reject source links even when rg omits them; do not follow an alias first."""
    no_symlink_parents(crate)
    if not crate.is_dir():
        raise ValueError('owning Rust crate directory is missing')
    inputs = []
    for directory, folders, files in os.walk(crate, followlinks=False):
        for name in folders + files:
            no_symlink_parents(Path(directory)/name)
        # Generated caches are not source. Referenced omitted inputs are checked
        # against Cargo's actual dep-info after compilation below.
        folders[:] = [name for name in folders if name not in {'target', '__pycache__', '.git'}]
        inputs.extend(Path(directory)/name for name in files if name.endswith('.rs') or name in {'Cargo.toml', 'Cargo.lock'})
    return inputs


def inventory():
    for name in ['client', 'desktop']:
        owning_tree(ROOT/'veranda'/name)
    paths = subprocess.check_output(['bash', str(ROOT/'tests/helpers/source-files.sh'), str(ROOT)], timeout=30).split(b'\0')
    result = []
    for raw in sorted(set(paths) - {b''}):
        name = raw.decode('utf8')
        path = Path(name)
        if path.is_absolute() or '..' in path.parts or any(part in {'.git', 'private', '.build'} for part in path.parts) or any(ord(c) < 32 for c in name):
            raise ValueError('public input path escaped its boundary')
        full = ROOT / path
        no_symlink_parents(full)
        info = full.lstat()
        if not stat.S_ISREG(info.st_mode):
            raise ValueError('public source inventory requires regular files')
        result.append({'path':name, 'sha256':sha(full), 'mode':oct(stat.S_IMODE(info.st_mode))})
    return result


def public_input(path, source):
    plain(path)
    if not path.is_relative_to(ROOT) or path.relative_to(ROOT).as_posix() not in source:
        raise ValueError('owning build input is absent from public inventory')


def workspace_inputs(metadata, source):
    """Path dependencies must be public, complete owning crates, not aliases."""
    paths = {row['path'] for row in source}
    for package in metadata['packages']:
        manifest = Path(package['manifest_path'])
        plain(manifest)
        if package['source'] is not None:
            continue
        public_input(manifest, paths)
        for path in owning_tree(manifest.parent):
            public_input(path, paths)
        for target in package['targets']:
            public_input(Path(target['src_path']), paths)
    for name in ['LICENSE', 'veranda/desktop/Cargo.lock', 'veranda/assets/favicon.svg',
                 f'veranda/desktop/packaging/{APP_ID}.desktop', f'veranda/desktop/packaging/{APP_ID}.metainfo.xml.in']:
        public_input(ROOT/name, paths)


def compiled_inputs(depfile, source, target_dir):
    """Bind the actual Rust module/include inputs, including ignored include files."""
    plain(depfile)
    if depfile.stat().st_size > 16*1024*1024:
        raise ValueError('Cargo source dep-info exceeds bound')
    line = depfile.read_text().replace('\\\n', '').splitlines()[0]
    target, separator, dependencies = line.partition(': ')
    if not target or not separator:
        raise ValueError('Cargo source dep-info is unavailable')
    public = {row['path'] for row in source}
    observed = set()
    for token in re.findall(r'(?:\\.|[^\s])+', dependencies):
        path = Path(re.sub(r'\\(.)', r'\1', token))
        if not path.is_absolute():
            path = ROOT/path
        no_symlink_parents(path)
        if path.is_relative_to(target_dir):
            # Build-script outputs have public owning scripts checked above.
            plain(path)
            continue
        public_input(path, public)
        observed.add(path.relative_to(ROOT).as_posix())
    if not observed:
        raise ValueError('Cargo source dep-info contains no public inputs')
    return sorted(observed)


def write(path, data, mode=0o644):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    path.chmod(mode)


def notices(metadata, destination):
    """Copy actual resolved crate notices, including build dependencies; no cache paths."""
    active = {node['id'] for node in metadata['resolve']['nodes']}
    report = []
    for package in sorted(metadata['packages'], key=lambda p:(p['name'],p['version'])):
        if package['id'] not in active:
            continue
        name, version = package['name'], package['version']
        if not re.fullmatch(r'[A-Za-z0-9_.-]+', name) or not re.fullmatch(r'[A-Za-z0-9_.+-]+', version):
            raise ValueError('unsafe dependency identity')
        source = Path(package['manifest_path']).parent
        no_symlink_parents(source)
        plain(Path(package['manifest_path']))
        if not package.get('license') and not package.get('license_file'):
            raise ValueError(f'no declared license for {name}')
        files = {path for path in source.iterdir() if path.is_file() and re.match(r'(?i)^(LICENSE|LICENCE|COPYING|COPYRIGHT|NOTICE)(?:$|[._-])',path.name)}
        if package.get('license_file'):
            license_file = source/package['license_file']
            no_symlink_parents(license_file)
            if not license_file.resolve().is_relative_to(source.resolve()):
                raise ValueError('dependency license path escaped crate')
            files.add(license_file)
        if package['source'] is None and source.is_relative_to(ROOT):
            files.add(ROOT/'LICENSE')
        if not files:
            raise ValueError(f'actual license/notice text unavailable for {name}')
        texts=[]
        for path in sorted(files):
            plain(path)
            if path.stat().st_size > 1024*1024:
                raise ValueError('dependency notice exceeds bounded size')
            relative=f'rust/{name}-{version}/{path.name}'
            write(destination/relative, path.read_bytes())
            texts.append({'path':relative,'sha256':sha(path)})
        report.append({'name':name,'version':version,'license':package.get('license'), 'source':'crates.io' if package['source'] and package['source'].startswith('registry+') else 'public workspace', 'notices':texts})
    write(destination/'rust-dependencies.json',encoded(report))
    return report


def shared_dependencies(elf, workspace, sdk_debs=None):
    """Use dpkg symbol/shlibs authority, including uninstalled published SDK DEBs."""
    write(workspace/'debian/control', b'Source: subyard-veranda\nSection: x11\nPriority: optional\nMaintainer: Subyard <subyard@users.noreply.github.com>\n\nPackage: subyard-veranda\nArchitecture: any\nDescription: Subyard graphical client\n')
    command=['dpkg-shlibdeps','-O']
    inputs=[]; seen=set()
    architecture=subprocess.check_output(['dpkg','--print-architecture'],text=True,timeout=5).strip()
    if sdk_debs:
        for archive in sorted(sdk_debs.glob('*.deb')):
            plain(archive)
            info = subprocess.check_output(['dpkg-deb','-f',str(archive),'Package','Version','Architecture'], text=True, timeout=20).splitlines()
            # dpkg-deb labels multiple requested fields. Keep structured public metadata.
            fields = dict(line.split(': ',1) for line in info)
            name = fields['Package']
            if not re.fullmatch(r'[a-z0-9][a-z0-9+.-]+',name):
                raise ValueError('unsafe SDK package name')
            if name in seen or fields['Architecture'] not in {architecture,'all'}:
                raise ValueError('duplicate or foreign-architecture SDK package')
            seen.add(name)
            directory=workspace/'sdk-packages'/name
            directory.mkdir(parents=True,exist_ok=False)
            run(['dpkg-deb','-e',str(archive),str(directory/'DEBIAN')],stdout=subprocess.DEVNULL)
            if (directory/'DEBIAN/shlibs').exists() or (directory/'DEBIAN/symbols').exists():
                run(['dpkg-deb','-x',str(archive),str(directory)],stdout=subprocess.DEVNULL)
                command.append('-S'+str(directory))
                inputs.append({'package':name,'version':fields['Version'],'architecture':fields['Architecture'],'archive_sha256':sha(archive)})
    command += ['-e'+str(elf)]
    result = run(command,cwd=workspace,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    # Preserve warnings as build evidence; do not suppress missing dependency information.
    print(result.stderr, end='', flush=True)
    lines=result.stdout.splitlines()
    if len(lines)!=1 or not lines[0].startswith('shlibs:Depends=') or '\n' in lines[0]:
        raise ValueError('dpkg-shlibdeps did not return one dependency projection')
    return lines[0].split('=',1)[1],inputs


def elf_metadata(elf):
    dynamic=subprocess.check_output(['readelf','--dynamic',str(elf)],text=True,timeout=20)
    if '(RPATH)' in dynamic or '(RUNPATH)' in dynamic:
        raise ValueError('package ELF must not retain build/library search paths')
    needed=re.findall(r'\(NEEDED\).*Shared library: \[([^\]]+)\]',dynamic)
    if not needed or len(needed)>64 or any('/' in name for name in needed):
        raise ValueError('invalid runtime shared-library projection')
    return needed


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version',required=True,type=stable_version)
    parser.add_argument('--output-dir',type=Path,default=ROOT/'.build/veranda-package/release')
    parser.add_argument('--target-dir',type=Path,default=ROOT/'.build/veranda-package/target')
    parser.add_argument('--sdk-root',type=Path,help='optional extracted published Debian GTK SDK')
    parser.add_argument('--sdk-debs',type=Path,help='matching original SDK DEBs for native dependency authority')
    args=parser.parse_args()
    if bool(args.sdk_root)!=bool(args.sdk_debs):
        parser.error('--sdk-root and --sdk-debs must be supplied together')
    if platform.system()!='Linux':
        parser.error('Linux native packaging only')
    machine=platform.machine()
    target,arch,triplet={'x86_64':('x86_64-unknown-linux-gnu','amd64','x86_64-linux-gnu'), 'aarch64':('aarch64-unknown-linux-gnu','arm64','aarch64-linux-gnu')}.get(machine,(None,None,None))
    if not arch:
        parser.error('unsupported native architecture')
    for tool in ['cargo','rustc','dpkg','dpkg-deb','dpkg-shlibdeps','readelf','rg','du','pkg-config','desktop-file-validate','appstreamcli']:
        if not shutil.which(tool): parser.error(tool+' is required')
    output=output_path(args.output_dir); target_dir=output_path(args.target_dir)
    source=inventory(); source_hash=hashlib.sha256(encoded(source)).hexdigest()
    output.mkdir(parents=True,exist_ok=True)
    artifact_name=f'subyard-veranda_{args.version}_{arch}.deb'
    for suffix in ['', '.sha256', '.manifest.json', '.provenance.json']:
        if (output/(artifact_name+suffix)).exists() or (output/(artifact_name+suffix)).is_symlink():
            parser.error('refusing to overwrite an existing candidate artifact')
    with tempfile.TemporaryDirectory(prefix='veranda-metadata-',dir='/tmp') as temporary:
        rendered=Path(temporary)/f'{APP_ID}.metainfo.xml'
        rendered.write_text((ROOT/f'veranda/desktop/packaging/{APP_ID}.metainfo.xml.in').read_text().replace('@VERSION@',args.version).replace('@DATE@',datetime.date.today().isoformat()))
        run(['desktop-file-validate',str(ROOT/f'veranda/desktop/packaging/{APP_ID}.desktop')])
        run(['appstreamcli','validate','--no-net',str(rendered)])
    env=dict(os.environ, VERANDA_PRODUCT_VERSION=args.version, CARGO_TARGET_DIR=str(target_dir), CARGO_BUILD_JOBS='2', TMPDIR='/tmp', CARGO_PROFILE_RELEASE_OPT_LEVEL='2', CARGO_PROFILE_RELEASE_DEBUG='0', CARGO_PROFILE_RELEASE_STRIP='symbols', CARGO_PROFILE_RELEASE_LTO='false', CARGO_PROFILE_RELEASE_CODEGEN_UNITS='16', CARGO_PROFILE_RELEASE_INCREMENTAL='false', CARGO_PROFILE_RELEASE_DEBUG_ASSERTIONS='false', CARGO_PROFILE_RELEASE_OVERFLOW_CHECKS='false')
    for key in ['CARGO_ENCODED_RUSTFLAGS','RUSTC','RUSTC_WRAPPER','RUSTC_WORKSPACE_WRAPPER','CARGO_BUILD_RUSTC']:
        env.pop(key,None)
    env['RUSTFLAGS']='--remap-path-prefix='+str(ROOT)+'=public-source'
    if args.sdk_root:
        sdk=args.sdk_root.resolve()/'usr'
        env.update(PKG_CONFIG_PATH=f'{sdk}/lib/{triplet}/pkgconfig:{sdk}/share/pkgconfig', LD_LIBRARY_PATH=f'{sdk}/lib/{triplet}')
    metadata=json.loads(subprocess.check_output(['cargo','+'+TOOLCHAIN,'metadata','--locked','--format-version','1','--filter-platform',target,'--manifest-path',str(ROOT/'veranda/desktop/Cargo.toml')],env=env,timeout=120))
    workspace_inputs(metadata, source)
    cache_roots={str(Path(p['manifest_path']).parent.parent) for p in metadata['packages'] if p['source'] and p['source'].startswith('registry+')}
    # Encoded flags preserve spaces in public source/cache locations.
    flags=['--remap-path-prefix='+str(ROOT)+'=public-source'] + ['--remap-path-prefix='+cache_root+'=public-cargo-registry' for cache_root in sorted(cache_roots)]
    env.pop('RUSTFLAGS',None)
    env['CARGO_ENCODED_RUSTFLAGS']='\x1f'.join(flags)
    with tempfile.TemporaryDirectory(prefix='veranda-license-preflight-',dir='/tmp') as temporary:
        notices(metadata,Path(temporary))
    run(['cargo','+'+TOOLCHAIN,'build','--release','--locked','--target',target,'--manifest-path',str(ROOT/'veranda/desktop/Cargo.toml')],env=env,timeout=1800)
    executable=target_dir/target/'release/veranda'
    rust_inputs=compiled_inputs(executable.with_suffix('.d'), source, target_dir)
    # Cargo legitimately hardlinks its generated executable to a hashed target.
    # Package staging below makes an independent copy; external inputs stay strict.
    if not stat.S_ISREG(executable.lstat().st_mode):
        raise ValueError('generated GUI must be a regular ELF')
    # A private, empty HOME without display proves the version path opens no store/GTK.
    with tempfile.TemporaryDirectory(prefix='veranda-package-',dir='/tmp') as temporary:
        work=Path(temporary)
        version_env=dict(env,HOME=str(work/'empty-home'),XDG_DATA_HOME=str(work/'empty-data'))
        for key in ['DISPLAY','WAYLAND_DISPLAY','DBUS_SESSION_BUS_ADDRESS']: version_env.pop(key,None)
        version=subprocess.check_output([str(executable),'--version'],env=version_env,text=True,timeout=5).strip()
        if version!='Subyard Veranda '+args.version or (work/'empty-home').exists() or (work/'empty-data').exists():
            raise ValueError('GUI/client product version or no-initialization verification failed')
        package=work/'debian/subyard-veranda'; binary=package/'usr/bin/subyard-veranda'
        write(binary,executable.read_bytes(),0o755)
        doc=package/'usr/share/doc/subyard-veranda'
        write(doc/'copyright',(ROOT/'LICENSE').read_bytes())
        licenses=notices(metadata,doc)
        dependencies,sdk_inputs=shared_dependencies(binary,work,args.sdk_debs.resolve() if args.sdk_debs else None)
        dependencies += ', openssh-client'
        needed=elf_metadata(binary)
        write(package/f'usr/share/applications/{APP_ID}.desktop',(ROOT/f'veranda/desktop/packaging/{APP_ID}.desktop').read_bytes())
        write(package/f'usr/share/icons/hicolor/scalable/apps/{APP_ID}.svg',(ROOT/'veranda/assets/favicon.svg').read_bytes())
        date=datetime.date.today().isoformat()
        appstream=(ROOT/f'veranda/desktop/packaging/{APP_ID}.metainfo.xml.in').read_text().replace('@VERSION@',args.version).replace('@DATE@',date)
        write(package/f'usr/share/metainfo/{APP_ID}.metainfo.xml',appstream.encode())
        build={'schemaVersion':1,'productVersion':args.version,'applicationId':APP_ID,'target':target,'rustToolchain':TOOLCHAIN,'profile':{'name':'release','optLevel':2,'strip':'symbols','lto':False,'codegenUnits':16,'incremental':False,'debugAssertions':False,'overflowChecks':False},'rustcVersion':subprocess.check_output(['rustc','+'+TOOLCHAIN,'--version'],env=env,text=True,timeout=10).strip(),'gtkBuildVersion':subprocess.check_output(['pkg-config','--modversion','gtk4'],env=env,text=True,timeout=10).strip(),'elfSha256':sha(binary),'sourceInputsSha256':source_hash,'rustSourceInputs':rust_inputs,'runtimeDepends':dependencies,'dtNeeded':needed,'cargoLockSha256':sha(ROOT/'veranda/desktop/Cargo.lock'),'rustDependencyNoticesSha256':sha(doc/'rust-dependencies.json'),'rootLicenseSha256':sha(doc/'copyright')}
        write(doc/'build-info.json',encoded(build))
        files=[path for path in package.rglob('*') if path.is_file()]
        installed_bytes=sum(path.stat().st_size for path in files)
        installed_kib=int(subprocess.check_output(['du','-sk',str(package)],text=True,timeout=10).split()[0])
        control=f'Package: subyard-veranda\nVersion: {args.version}\nArchitecture: {arch}\nMaintainer: Subyard <subyard@users.noreply.github.com>\nSection: x11\nPriority: optional\nInstalled-Size: {installed_kib}\nDepends: {dependencies}\nHomepage: https://github.com/Subyard/Subyard\nDescription: Native graphical client for Subyard\n GTK client for local and pinned SSH owners. Install the same product\n version of Yard separately; this package contains no engine or runtime.\n'
        write(package/'DEBIAN/control',control.encode())
        for path in [package,*package.rglob('*')]:
            if path.is_dir(): path.chmod(0o755)
        if inventory()!=source:
            raise ValueError('public source changed during build; package must be rebuilt')
        archive=output/artifact_name
        run(['dpkg-deb','--root-owner-group','-Zxz','-z9','--build',str(package),str(archive)])
        archive.chmod(0o644)
        if archive.stat().st_size>40*1024*1024 or installed_kib*1024>80*1024*1024:
            raise ValueError('GUI package exceeds compressed/installed hard budget')
        manifest=dict(build,artifact=artifact_name,sha256=sha(archive),compressedGuiBytes=archive.stat().st_size,installedGuiPayloadBytes=installed_bytes,installedSizeKiB=installed_kib,engineIncluded=False,systemDependenciesIncluded=False,rustDependencyCount=len(licenses))
        write(output/(artifact_name+'.manifest.json'),encoded(manifest))
        revision=subprocess.run(['git','-C',str(ROOT),'rev-parse','HEAD'],text=True,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=10)
        dirty=subprocess.run(['git','-C',str(ROOT),'status','--porcelain','--untracked-files=all'],text=True,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=10)
        provenance={'schemaVersion':1,'artifact':artifact_name,'sha256':sha(archive),'productVersion':args.version,'sourceRepository':'https://github.com/Subyard/Subyard','sourceRevision':revision.stdout.strip() if revision.returncode==0 else None,'sourceDirty':bool(dirty.stdout) if dirty.returncode==0 else None,'sourceInputsSha256':source_hash,'publicInputs':source,'sdkDependencyInputs':sdk_inputs,'generatedAt':datetime.datetime.now(datetime.timezone.utc).isoformat(),'build':build}
        write(output/(artifact_name+'.provenance.json'),encoded(provenance))
        write(output/(artifact_name+'.sha256'),(sha(archive)+'  '+artifact_name+'\n').encode())
        print(json.dumps(manifest,sort_keys=True),flush=True)


if __name__=='__main__':
    try:
        main()
    except (ValueError,OSError,subprocess.SubprocessError) as error:
        # No raw environment or private config. Called tools retain their original diagnostics.
        raise SystemExit('package-veranda: '+str(error))
