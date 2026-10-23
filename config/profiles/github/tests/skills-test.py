#!/usr/bin/env python3
"""The GitHub package retains its single skill through discovery migration."""
import importlib.util
import os
import subprocess
import shutil
import tarfile
import pwd
from pathlib import Path
import tempfile
import unittest

PROFILE = Path(__file__).resolve().parents[1]
ROOT = PROFILE.parents[2]
spec = importlib.util.spec_from_file_location('deliver', ROOT/'config/skills/deliver.py')
deliver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deliver)


class GitHubSkill(unittest.TestCase):
    def test_first_registration_waits_for_successful_owner_activation(self):
        owner = (PROFILE/'owner.sh').read_text().split('# Static skill repair',1)[1]
        provision = (PROFILE/'provision.sh').read_text()
        parser = provision.split('mode=apply\n',1)[1].split('\nDEV_USER=',1)[0]
        special = provision.split('# Static refresh cannot register',1)[1].split('\nchanged=0',1)[0]
        ordinary = provision.rsplit('\nskill_delivery apply',1)[1]
        ordinary = 'skill_delivery apply'+ordinary
        with tempfile.TemporaryDirectory(prefix='subyard-github-first-registration-',dir='/tmp') as temporary:
            for scenario in ('manager-failure','start-failure','handshake-failure','success'):
                with self.subTest(scenario=scenario):
                    root = Path(temporary)/scenario
                    home = root/'home'
                    home.mkdir(parents=True)
                    engine = root/'source-engine'
                    engine.write_text('native engine fixture')
                    engine.chmod(0o700)
                    profile = root/'profile'
                    profile.mkdir()
                    (profile/'owner.sh').write_text('#!/bin/sh\nexit 1\n')
                    (profile/'owner.sh').chmod(0o700)
                    environment = dict(os.environ,SCENARIO=scenario,FIXTURE_ROOT=str(root),DEV_HOME=str(home),
                        DELIVERY_SOURCE=str(ROOT/'config/skills/deliver.py'),PROFILE_SOURCE=str(PROFILE),
                        PROFILE_DIR=str(profile),engine=str(engine),state_dir=str(root/'host-state'),
                        unit_dir=str(root/'units'),broker_engine=str(root/'host-state/engine'),
                        runtime_file=str(root/'host-state/runtime.json'),unit_file=str(root/'units/broker.service'),
                        unit='fixture.service',mode='apply',YARD_INSTANCE_NAME='fixture',INCUS_PROJECT='fixture',
                        guest_engine='/usr/local/libexec/subyard/github-client',DEV_USER=pwd.getpwuid(os.getuid()).pw_name)
                    setup = r'''
skill_delivery() { python3 "$DELIVERY_SOURCE" "$@" --root "$FIXTURE_ROOT" --provider profile-github --source "$PROFILE_SOURCE/skills.json" --home "$DEV_HOME" --uid "$(id -u)" --gid "$(id -g)" --extra; }
render_runtime() { printf '{}\n'; }
render_unit() { printf '# fixture unit\n'; }
incus() { cat >/dev/null; }
loginctl() { printf 'yes\n'; }
user_systemctl() {
  case "$1:$SCENARIO" in enable:manager-failure) return 8 ;; restart:start-failure) return 9 ;; esac
}
guest_runtime_current() { [ "$SCENARIO" != handshake-failure ]; }
sleep() { :; }
die() { exit 2; }
'''
                    guest = 'guest_hook() ( mode=apply\n'+parser+'\n# Static refresh cannot register'+special+'\n'+ordinary+'\n)\n'
                    result = subprocess.run(['bash','-euc',setup+guest+'\n# Static skill repair'+owner],
                        env=environment,capture_output=True,text=True,timeout=10)
                    receipt = root/'var/lib/subyard/agent-skills/profile-github.json'
                    if scenario == 'success':
                        self.assertEqual(result.returncode,0,result.stderr)
                        self.assertTrue(receipt.is_file())
                    else:
                        self.assertNotEqual(result.returncode,0)
                        self.assertFalse(receipt.exists(),'failed first owner advertised a skill')
                    for adapter in deliver.ADAPTERS+deliver.EXTRA:
                        self.assertEqual(os.path.lexists(home/adapter/'subyard-github'),scenario == 'success')

    def test_profile_activation_transports_helper_before_core_install(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary)
            commands = root/'commands'
            commands.mkdir()
            archive = root/'bundle.tar'
            incus = commands/'incus'
            incus.write_text('#!/bin/sh\ncat > "$CAPTURE_ARCHIVE"\n')
            incus.chmod(0o755)
            body = (PROFILE/'owner.sh').read_text().split('guest_hook() {',1)[1].split('\n}\n',1)[0]
            environment = dict(os.environ, PATH=str(commands)+':'+os.environ['PATH'], CAPTURE_ARCHIVE=str(archive))
            script = 'guest_hook() {'+body+'\n}\nguest_hook'
            environment.update(profile_dir=str(PROFILE),root=str(ROOT),DEV_USER=pwd.getpwuid(os.getuid()).pw_name,YARD_INSTANCE_NAME='fixture',INCUS_PROJECT='fixture')
            result = subprocess.run(['bash','-euc',script],env=environment,capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr)
            guest = root/'guest'
            guest.mkdir()
            with tarfile.open(archive) as bundle:
                bundle.extractall(guest,filter='data')
            self.assertEqual((guest/'agent-skills.py').read_bytes(),(ROOT/'config/skills/deliver.py').read_bytes())
            # Invoke the actual owning skill-delivery boundary in a guest that has
            # no installed core helper. Only redirect its filesystem root.
            python = commands/'python3'
            python.write_text('#!/bin/sh\nexec /usr/bin/python3 "$@" --root "$FIXTURE_ROOT"\n')
            python.chmod(0o755)
            home = root/'home'
            home.mkdir()
            boundary = (guest/'provision.sh').read_text().split('skill_delivery() {',1)[1].split('\n}',1)[0]
            environment.update(DEV_HOME=str(home),FIXTURE_ROOT=str(root/'installed'))
            result = subprocess.run(['bash','-euc','skill_delivery() {'+boundary+'\n}\nskill_delivery apply',str(guest/'provision.sh')],env=environment,capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertTrue((home/'.agents/skills/subyard-github/SKILL.md').is_file())
            canonical = root/'installed/usr/local/share/subyard/skills/subyard-github/SKILL.md'
            canonical.write_text('stale static guidance')
            owner_profile = root/'owner-profile'
            owner_profile.mkdir()
            check = owner_profile/'owner.sh'
            check.write_text('#!/bin/sh\n[ "$1" = --check ]\ncmp -s "$CANONICAL_SKILL" "$EXPECTED_SKILL"\n')
            check.chmod(0o755)
            manager, engine = root/'manager',root/'engine'
            manager.write_text('unchanged manager')
            engine.write_text('unchanged executable')
            environment.update(PROFILE_DIR=str(owner_profile),mode='apply',CANONICAL_SKILL=str(canonical),EXPECTED_SKILL=str(guest/'SKILL.md'),MANAGER=str(manager),ENGINE=str(engine))
            fast_path = (PROFILE/'owner.sh').read_text().split('# Static skill repair',1)[1].split('\ninstall -d -m 0700',1)[0]
            script = 'skill_delivery() {'+boundary+'\n}\nguest_hook() { skill_delivery apply --registered; }\n# Static skill repair'+fast_path+'\nprintf mutated > "$MANAGER"\nprintf mutated > "$ENGINE"\n'
            result = subprocess.run(['bash','-euc',script,str(guest/'provision.sh')],env=environment,capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertEqual(canonical.read_bytes(),(PROFILE/'SKILL.md').read_bytes())
            self.assertEqual(manager.read_text(),'unchanged manager')
            self.assertEqual(engine.read_text(),'unchanged executable')


    def test_receiptless_legacy_residue_and_foreign_artifacts(self):
        source = (PROFILE/'provision.sh').read_text()
        def function(name):
            return name+'() {'+source.split(name+'() {',1)[1].split('\n}',1)[0]+'\n}\n'
        check = 'changed=0\n'+source.split('changed=0\n',1)[1].split('\nif [ "$mode" = check ]',1)[0]
        links = 'skill_links=('+source.split('skill_links=(',1)[1].split('\n)',1)[0]+'\n)\n'
        cases = (('canonical-only',True,False,False,10),
                 ('legacy-link-only',False,True,False,10),
                 ('both',True,True,False,10),
                 ('foreign-canonical',True,False,True,0),
                 ('foreign-link',False,True,True,0),
                 ('foreign-both',True,True,True,0))
        with tempfile.TemporaryDirectory(prefix='subyard-github-removal-',dir='/tmp') as temporary:
            for name, has_canonical, has_link, foreign, expected in cases:
                with self.subTest(name=name):
                    root = Path(temporary)/name
                    home = root/'home'
                    home.mkdir(parents=True)
                    canonical = root/'usr/local/share/subyard/skills/subyard-github'
                    canonical.parent.mkdir(parents=True)
                    canonical.parent.chmod(0o755)
                    content = b'user-owned skill contents' if foreign else (PROFILE/'SKILL.md').read_bytes()
                    if has_canonical:
                        canonical.mkdir()
                        canonical.chmod(0o755)
                        (canonical/'SKILL.md').write_bytes(content)
                        (canonical/'SKILL.md').chmod(0o644)
                    link = home/'.codex/skills/subyard-github'
                    target = root/'foreign-skill' if foreign else canonical
                    if has_link:
                        link.parent.mkdir(parents=True)
                        link.symlink_to(target)
                    environment = dict(os.environ,FIXTURE_ROOT=str(root),DEV_HOME=str(home),DEV_USER=pwd.getpwuid(os.getuid()).pw_name,PROFILE_SOURCE=str(PROFILE),DELIVERY_SOURCE=str(ROOT/'config/skills/deliver.py'))
                    setup = r'''mode=check-removed
skill_dir="$FIXTURE_ROOT/usr/local/share/subyard/skills/subyard-github"
skill="$skill_dir/SKILL.md"
remote="$FIXTURE_ROOT/usr/local/bin/git-remote-subyard-github"
wrapper="$FIXTURE_ROOT/usr/local/bin/subyard-github"
socket_dir="$DEV_HOME/.local/share/subyard"
socket="$socket_dir/github.sock"
wiring_source="$PROFILE_SOURCE/git_wiring.py"
skill_source="$PROFILE_SOURCE/SKILL.md"
wrapper_source="$PROFILE_SOURCE/bin/subyard-github"
remote_source="$PROFILE_SOURCE/bin/git-remote-subyard-github"
wrapper_body() { cat "$wrapper_source"; }
remote_body() { cat "$remote_source"; }
skill_body() { cat "$skill_source"; }
skill_delivery() { python3 "$DELIVERY_SOURCE" "$@" --root "$FIXTURE_ROOT" --provider profile-github --home "$DEV_HOME" --uid "$(id -u "$DEV_USER")" --gid "$(id -g "$DEV_USER")" --extra; }
'''
                    script = setup+links+function('owned_file')+function('link_matches')+function('socket_path_safe')+check
                    result = subprocess.run(['bash','-euc',script],env=environment,capture_output=True,text=True)
                    self.assertEqual(result.returncode,expected,result.stderr)
                    self.assertFalse((root/'var/lib/subyard/agent-skills/profile-github.json').exists())
                    if has_canonical:
                        self.assertEqual((canonical/'SKILL.md').read_bytes(),content)
                    if has_link:
                        self.assertEqual(os.readlink(link),str(target))

    def test_adopt_legacy_canonical_without_duplicate_codex_registration(self):
        with tempfile.TemporaryDirectory(dir='/tmp') as temporary:
            root = Path(temporary)
            home = root/'home'
            home.mkdir()
            canonical = root/'usr/local/share/subyard/skills/subyard-github'
            canonical.mkdir(parents=True)
            canonical.chmod(0o755)
            canonical.parent.chmod(0o755)
            (canonical/'SKILL.md').write_bytes((PROFILE/'SKILL.md').read_bytes())
            for adapter in ('.codex/skills','.claude/skills','.config/opencode/skills','.pi/agent/skills','.hermes/skills'):
                directory = home/adapter
                directory.mkdir(parents=True)
                (directory/'subyard-github').symlink_to(canonical)
            args = type('Args', (), dict(root=root, home=home, uid=os.getuid(), gid=os.getgid(), provider='profile-github', mode='apply', source=PROFILE/'skills.json', registered=False, extra=True))()
            deliver.delivery(args)
            args.mode='check'
            self.assertEqual(deliver.delivery(args),0)
            self.assertFalse(os.path.lexists(home/'.codex/skills/subyard-github'))
            for adapter in deliver.ADAPTERS+deliver.EXTRA:
                self.assertEqual((home/adapter/'subyard-github/SKILL.md').read_bytes(),(PROFILE/'SKILL.md').read_bytes())
            args.mode='remove'
            deliver.delivery(args)
            args.mode='check-removed'
            self.assertEqual(deliver.delivery(args),0)


if __name__ == '__main__':
    unittest.main()
