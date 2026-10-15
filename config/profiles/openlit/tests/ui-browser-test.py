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
import subprocess
import tempfile
import time
import unittest

spec = importlib.util.spec_from_file_location('browser', Path(__file__).parent/'e2e/ui-browser.py')
browser = importlib.util.module_from_spec(spec)
spec.loader.exec_module(browser)
BINARY = shutil.which('chromium') or shutil.which('chromium-browser') or shutil.which('google-chrome')


@unittest.skipUnless(BINARY, 'rendered DOM prerequisite: Chromium')
class BrowserDOMTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix='openlit-dom-', dir='/tmp')
        cls.root = Path(cls.temporary.name)
        cls.root.chmod(0o700)
        cls.profile = cls.root/'profile'
        cls.profile.mkdir(mode=0o700)
        cls.log = (cls.root/'chromium.log').open('wb')
        os.chmod(cls.root/'chromium.log', 0o600)
        argv = [BINARY, '--headless=new', '--remote-debugging-address=127.0.0.1', '--remote-debugging-port=0', '--user-data-dir='+str(cls.profile), '--disable-background-networking', '--disable-dev-shm-usage', '--no-first-run', 'about:blank']
        if os.getuid() == 0:
            argv.insert(1, '--no-sandbox')
        cls.process = subprocess.Popen(argv, stdout=cls.log, stderr=cls.log, start_new_session=True)
        try:
            cls.cdp = browser.CDP(browser.discover(cls.profile, time.monotonic()+15), time.monotonic()+90)
            cls.cdp.call('Page.enable')
        except Exception:
            cls.tearDownClass()
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


if __name__ == '__main__':
    unittest.main()
