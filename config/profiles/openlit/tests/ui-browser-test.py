#!/usr/bin/env python3
"""Render pinned signal-row markup in Chromium; emit only closed observations."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('browser', Path(__file__).parent/'e2e/ui-browser.py')
browser = importlib.util.module_from_spec(spec)
spec.loader.exec_module(browser)
BINARY = shutil.which('chromium') or shutil.which('chromium-browser') or shutil.which('google-chrome')
CAPTURE_ROOT = Path(__file__).resolve().parents[4]/'.build/test-runs'


def preserve_startup_failure(process, log):
    status = process.poll()
    info = os.fstat(log.fileno())
    if not stat.S_ISREG(info.st_mode):
        raise ValueError('startup-log-not-regular')
    limit = 256*1024
    payload = os.pread(log.fileno(), limit, 0)
    CAPTURE_ROOT.mkdir(parents=True, exist_ok=True)
    output = Path(tempfile.mkdtemp(prefix='openlit-chromium.', dir=CAPTURE_ROOT))
    output.chmod(0o700)
    facts = {'category': 'chromium-startup', 'process_poll': status,
             'process_running': status is None, 'log_bytes': len(payload),
             'log_truncated': info.st_size > len(payload)}
    for name, data in (('chromium.log', payload),
                       ('startup.json', (json.dumps(facts)+'\n').encode())):
        with (output/name).open('xb') as stream:
            os.fchmod(stream.fileno(), 0o600)
            stream.write(data)
    return output, status


@unittest.skipUnless(BINARY, 'rendered DOM prerequisite: Chromium')
class BrowserDOMTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix='openlit-dom-', dir='/tmp')
        cls.root = Path(cls.temporary.name)
        cls.root.chmod(0o700)
        cls.profile = cls.root/'profile'
        cls.profile.mkdir(mode=0o700)
        cls.log = (cls.root/'chromium.log').open('w+b')
        os.chmod(cls.root/'chromium.log', 0o600)
        argv = [BINARY, '--headless=new', '--remote-debugging-address=127.0.0.1', '--remote-debugging-port=0', '--user-data-dir='+str(cls.profile), '--disable-background-networking', '--disable-dev-shm-usage', '--no-first-run', 'about:blank']
        if os.getuid() == 0:
            argv.insert(1, '--no-sandbox')
        cls.process = subprocess.Popen(argv, stdout=cls.log, stderr=cls.log, start_new_session=True)
        try:
            cls.cdp = browser.CDP(browser.discover(cls.profile, time.monotonic()+15), time.monotonic()+90)
            cls.cdp.call('Page.enable')
        except Exception:
            try:
                output, status = preserve_startup_failure(cls.process, cls.log)
                print(f'openlit_chromium_startup capture=saved process_poll={status} path={output}', file=sys.stderr)
            except Exception as failure:
                print('openlit_chromium_startup capture=failed category='+type(failure).__name__, file=sys.stderr)
            try:
                cls.tearDownClass()
                print('openlit_chromium_startup cleanup=complete', file=sys.stderr)
            except Exception as failure:
                print('openlit_chromium_startup cleanup=failed category='+type(failure).__name__, file=sys.stderr)
            raise

    @classmethod
    def tearDownClass(cls):
        if hasattr(cls, 'cdp'):
            cls.cdp.socket.close()
        if cls.process.poll() is None:
            os.killpg(cls.process.pid, signal.SIGTERM)
            try:
                cls.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(cls.process.pid, signal.SIGKILL)
                cls.process.wait(timeout=5)
        cls.log.close()
        cls.temporary.cleanup()

    def render(self, service='codex_cli_rs', hidden=False, tab='logs'):
        # LogRecord's actual service field and body are distinct elements in
        # pinned OpenLIT 2.1.0. Unknown service/body values must never emit.
        style = 'display:none' if hidden else ''
        if tab == 'metrics':
            field = '<div class="mt-1 flex flex-wrap"><span class="rounded font-medium">gauge</span><span class="rounded" style="'+style+'">'+service+'</span></div>'
        elif tab == 'traces':
            field = '<div class="mt-1 flex flex-wrap gap-x-3"><span>2026-10-14</span><span class="font-mono">trace</span><span style="'+style+'">'+service+'</span></div>'
        else:
            field = '<div class="grid min-w-0 grow"><span class="max-w-36 truncate" style="'+style+'">'+service+'</span></div>'
        type(self).generation = getattr(type(self), 'generation', 0)+1
        generation = type(self).generation
        html = '<title>OpenLIT</title><body data-fixture="'+str(generation)+'"><button>Logs</button><button>Metrics</button><button>Traces</button><button role="tab" aria-selected="true">7 days</button><section><div class="grid gap-2"><button class="w-full text-left border">'+field+'<p>PRIVATE_BODY_SENTINEL codex_cli_rs</p></button></div></section></body>'
        path = self.root/'fixture.html'
        path.write_text(html)
        path.chmod(0o600)
        self.cdp.call('Page.navigate', {'url': path.as_uri()+'?tab='+tab+'&tr=7D'})
        browser.wait(self.cdp, 'document.readyState==="complete"&&document.body.dataset.fixture==="'+str(generation)+'"', time.monotonic()+5, 'fixture-navigation')
        return self.cdp.evaluate(browser.OBSERVE)

    def test_rendered_service_field_and_range_have_closed_facts(self):
        for service in ('codex_cli_rs', 'codex_exec', 'codex-app-server'):
            for tab in ('logs', 'metrics', 'traces'):
                with self.subTest(service=service, tab=tab):
                    value = self.render(service=service, tab=tab)
                    self.assertEqual(value['rows'], 1)
                    self.assertEqual(value['codexRows'], 1)
                    self.assertEqual(value['rowElements']['visibleCategories']['CODEX'], 1)
                    self.assertEqual(value['range'], {'url': '7D', 'selected7D': True})
                    self.assertNotIn('PRIVATE_BODY_SENTINEL', json.dumps(value))
                    self.assertNotIn(str(self.root), json.dumps(value))

    def test_hidden_service_is_diagnosed_and_never_counted_as_visible_codex(self):
        for service in ('codex_cli_rs', 'codex_exec', 'codex-app-server'):
            with self.subTest(service=service):
                value = self.render(service=service, hidden=True)
                self.assertEqual(value['rowElements']['serviceFields'], 1)
                self.assertEqual(value['rowElements']['visibleServiceFields'], 0)
                self.assertEqual(value['codexRows'], 0)

    def test_unknown_service_cannot_be_replaced_by_body_identity(self):
        value = self.render(service='PRIVATE_SERVICE_SENTINEL')
        self.assertEqual(value['codexRows'], 0)
        self.assertEqual(value['rowElements']['categories']['OTHER'], 1)
        output = io.StringIO()
        summary = {'pages': []}
        with contextlib.redirect_stdout(output), self.assertRaises(browser.Failure):
            browser.record_page(summary, value, 'logs')
        self.assertEqual(summary['pages'], [value])
        self.assertIn('openlit_browser_observation=', output.getvalue())
        self.assertNotIn('PRIVATE_SERVICE_SENTINEL', output.getvalue())
        self.assertNotIn('PRIVATE_BODY_SENTINEL', output.getvalue())

    def test_inherited_property_names_remain_closed_service_categories(self):
        expected = {'CODEX', 'CLAUDE', 'UNKNOWN_SERVICE', 'ALL_SERVICES', 'EMPTY', 'OTHER'}
        for service in ('constructor', '__proto__', 'toString', 'codex_exec-other'):
            with self.subTest(service=service):
                value = self.render(service=service)
                self.assertEqual(value['codexRows'], 0)
                for name in ('categories', 'visibleCategories'):
                    categories = value['rowElements'][name]
                    self.assertEqual(set(categories), expected)
                    self.assertTrue(all(type(count) is int and count >= 0 for count in categories.values()))
                    self.assertEqual(categories['OTHER'], 1)
                    self.assertEqual(sum(categories.values()), 1)

    def test_database_acknowledgment_never_returns_connection_fields(self):
        self.render()
        self.cdp.evaluate('window.fetch=async()=>({ok:true,json:async()=>[{isCurrent:true,name:"Default DB",database:"openlit",password:"PRIVATE_PASSWORD_SENTINEL",host:"private.invalid"}]})')
        self.assertEqual(self.cdp.evaluate(browser.DATABASE), 'CURRENT_DEFAULT')
        for name, database in (('Default DB', 'PRIVATE_DATABASE_SENTINEL'), ('PRIVATE_NAME_SENTINEL', 'openlit')):
            with self.subTest(name=name, database=database):
                values = [{'isCurrent': True, 'name': name, 'database': database}]
                self.cdp.evaluate('window.fetch=async()=>({ok:true,json:async()=>'+json.dumps(values)+'})')
                self.assertEqual(self.cdp.evaluate(browser.DATABASE), 'CURRENT_OTHER')

    def test_clear_filters_acknowledges_actual_control_click(self):
        self.render()
        self.assertEqual(self.cdp.evaluate(browser.CLEAR_FILTERS), 'NOT_PRESENT')
        self.cdp.evaluate('(()=>{const b=document.createElement("button");b.textContent="Clear Filters";b.onclick=()=>b.remove();document.body.append(b);})()')
        self.assertTrue(self.cdp.evaluate(browser.OBSERVE)['clearControlPresent'])
        self.assertEqual(self.cdp.evaluate(browser.CLEAR_FILTERS), 'CLICKED')
        self.assertFalse(self.cdp.evaluate(browser.OBSERVE)['clearControlPresent'])


class StartupCaptureTest(unittest.TestCase):
    def test_failed_child_evidence_survives_cleanup_and_failure_is_preserved(self):
        # Exercise the real setup failure boundary without launching a browser.
        class FailedBrowser(BrowserDOMTest):
            pass
        native_popen = subprocess.Popen
        sentinel = b'controlled Chromium startup stderr\n'
        failure = browser.Failure('chromium-startup')
        def launch(argv, **options):
            return native_popen([sys.executable, '-c',
                                 'import sys; sys.stderr.buffer.write('+repr(sentinel)+'); sys.exit(23)'], **options)
        def discover(*args):
            FailedBrowser.process.wait(timeout=5)
            raise failure
        with tempfile.TemporaryDirectory(prefix='openlit-startup-capture-', dir='/tmp') as name:
            output = Path(name)/'captures'
            diagnostics = io.StringIO()
            with patch(__name__+'.CAPTURE_ROOT', output), patch(__name__+'.BINARY', sys.executable), \
                 patch.object(subprocess, 'Popen', side_effect=launch), \
                 patch.object(browser, 'discover', side_effect=discover), contextlib.redirect_stderr(diagnostics):
                with self.assertRaises(browser.Failure) as raised:
                    FailedBrowser.setUpClass()
            self.assertIs(raised.exception, failure)
            self.assertFalse(FailedBrowser.root.exists())
            self.assertEqual(FailedBrowser.process.poll(), 23)
            capture, = output.iterdir()
            self.assertEqual((capture/'chromium.log').read_bytes(), sentinel)
            facts = json.loads((capture/'startup.json').read_text())
            self.assertEqual(facts, {'category': 'chromium-startup', 'process_poll': 23,
                                    'process_running': False, 'log_bytes': len(sentinel), 'log_truncated': False})
            self.assertEqual(stat.S_IMODE(capture.stat().st_mode), 0o700)
            for path in capture.iterdir():
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertNotIn(sentinel.decode().strip(), diagnostics.getvalue())
            cleanup = FailedBrowser.tearDownClass
            def failed_cleanup():
                cleanup()
                raise RuntimeError('PRIVATE_CLEANUP_SENTINEL')
            diagnostics = io.StringIO()
            with patch(__name__+'.BINARY', sys.executable), \
                 patch.object(subprocess, 'Popen', side_effect=launch), \
                 patch.object(browser, 'discover', side_effect=discover), \
                 patch(__name__+'.preserve_startup_failure', side_effect=OSError('PRIVATE_CAPTURE_SENTINEL')), \
                 patch.object(FailedBrowser, 'tearDownClass', side_effect=failed_cleanup), \
                 contextlib.redirect_stderr(diagnostics):
                with self.assertRaises(browser.Failure) as raised:
                    FailedBrowser.setUpClass()
            self.assertIs(raised.exception, failure)
            self.assertFalse(FailedBrowser.root.exists())
            self.assertIn('capture=failed category=OSError', diagnostics.getvalue())
            self.assertIn('cleanup=failed category=RuntimeError', diagnostics.getvalue())
            self.assertNotIn('PRIVATE_', diagnostics.getvalue())


if __name__ == '__main__':
    unittest.main()
