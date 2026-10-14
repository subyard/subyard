#!/usr/bin/env python3
"""Owning fixture checks for real local-account and onboarding request contracts."""
import http.server
import json
from pathlib import Path
import re
import runpy
import subprocess
import tempfile
import threading
import unittest
import urllib.parse

SCRIPT=Path(__file__).with_name('e2e')/'ui-origin.py'

class UIOriginTest(unittest.TestCase):
    def test_remote_native_landing_url_keeps_api_origin_and_rendered_tunnel(self):
        parser=runpy.run_path(str(SCRIPT.with_name('remote-owner.py')))['dashboard_instruction']
        origin,command=parser('openlit dashboard up (open http://127.0.0.1:22222/telemetry?tab=logs after: ssh -N -L 22222:127.0.0.1:3000 yard-demo)')
        self.assertEqual(origin,'http://127.0.0.1:22222/')
        self.assertEqual(command,['ssh','-N','-L','22222:127.0.0.1:3000','yard-demo'])

    def exercise(self, reject=False, telemetry=None, llm_count=0, metric_service='codex_cli_rs', metric_key='serviceName'):
        class Handler(http.server.BaseHTTPRequestHandler):
            authenticated=False
            onboarded=False
            paths=[]
            def log_message(self,*args): pass
            def reply(self,status,body=None,location=None):
                self.send_response(status)
                self.send_header('Content-Type','application/json')
                if location: self.send_header('Location',location)
                self.end_headers()
                self.wfile.write(json.dumps(body or {}).encode())
            def do_GET(self):
                self.paths.append(self.path)
                if self.path=='/api/auth/csrf': self.reply(200,{'csrfToken':'synthetic'})
                elif self.path=='/api/auth/session': self.reply(200,{'user':{'email':'user@openlit.io' if telemetry else 'openlit-fixture@example.invalid'}} if self.authenticated else {})
                elif self.path=='/api/db-config': self.reply(200,[{'id':'default-db'}])
                elif self.path=='/telemetry?tab=logs': self.reply(200)
                elif self.path=='/dashboard':
                    if not self.authenticated: self.reply(307,location='/login')
                    elif not self.onboarded: self.reply(307,location='/onboarding')
                    else: self.reply(200)
                else: self.reply(404)
            def do_POST(self):
                self.paths.append(self.path)
                raw=self.rfile.read(int(self.headers['Content-Length']))
                if self.headers['Content-Type']=='application/json': payload=json.loads(raw)
                else: payload={k:v[0] for k,v in urllib.parse.parse_qs(raw.decode()).items()}
                assert self.headers['Origin']=='http://'+self.headers['Host']
                if self.path.startswith('/api/auth/callback/'):
                    password=payload['password']
                    assert all(re.search(pattern,password) for pattern in ('[A-Z]','[a-z]','[0-9]'))
                    if reject:
                        self.reply(401,{'url':'http://'+self.headers['Host']+'/login?error=Password%20policy%20rejected%20sensitive-sentinel'})
                    else:
                        Handler.authenticated=True
                        self.reply(200,{'url':payload['callbackUrl']})
                elif self.path=='/api/organisation': self.reply(200,{'id':'fixture-org'})
                elif self.path=='/api/organisation/current/fixture-org': self.reply(200)
                elif self.path=='/api/user/complete-onboarding': Handler.onboarded=True; self.reply(200,{'success':True})
                elif self.path.startswith('/api/telemetry/'):
                    assert payload['environment']=='' and payload['selectedConfig']=={}, 'native attributes must not be hidden by SDK filters'
                    # OpenLIT 2.1.0 returns grouped Metrics series, rather than
                    # the ClickHouse-shaped Logs/Traces records.
                    rows=[{'metricName':'codex.api_request','metricType':'gauge',metric_key:metric_service,
                           'latestValue':1,'pointCount':1}] if self.path=='/api/telemetry/metrics' else [{'ServiceName':'codex_cli_rs'}]
                    self.reply(200,{'err':None,'records':[] if telemetry=='empty' else rows,'total':0 if telemetry=='empty' else 1})
                elif self.path=='/api/metrics/request/total': self.reply(200,{'err':None,'data':[{'total_requests':llm_count}]})
                else: self.reply(404)
        with http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler) as server, tempfile.TemporaryDirectory(dir='/tmp') as root:
            thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
            try:
                args=['python3',str(SCRIPT),'--origin','http://127.0.0.1:'+str(server.server_port),'--state',str(Path(root)/'cookies'),'--identity','fixture']
                if telemetry:
                    path=Path(root)/'credentials.json'
                    path.write_text(json.dumps({'email':'user@openlit.io','password':'Synthetic-fixture-7-local'}))
                    path.chmod(0o600)
                    args+=['--credentials',str(path),'--telemetry',telemetry]
                result=subprocess.run(args,capture_output=True,text=True,timeout=10)
            finally: server.shutdown();thread.join()
        return result,Handler.paths
    def test_credentials_and_onboarding(self):
        result,paths=self.exercise()
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('/api/user/complete-onboarding',paths)
        self.assertEqual(paths[-1],'/dashboard')
    def test_failure_summary_preserves_safe_evidence(self):
        result,_=self.exercise(reject=True)
        self.assertNotEqual(result.returncode,0)
        self.assertIn('"status": 401',result.stderr)
        self.assertIn('"error_category": "password-policy"',result.stderr)
        self.assertNotIn('sensitive-sentinel',result.stderr)

    def test_operator_login_and_native_read_paths_without_sdk_filters(self):
        for telemetry in ('empty','codex'):
            with self.subTest(telemetry=telemetry):
                result,paths=self.exercise(telemetry=telemetry)
                self.assertEqual(result.returncode,0,result.stderr)
                self.assertIn('/api/auth/callback/login',paths)
                self.assertIn('/api/telemetry/trace',paths)
                self.assertIn('/api/metrics/request/total',paths)
                self.assertNotIn('Synthetic-fixture-7-local',result.stdout+result.stderr)

    def test_empty_phase_rejects_nonzero_dashboard_count_and_native_phase_reports_it(self):
        result,_=self.exercise(telemetry='empty',llm_count='0')
        self.assertEqual(result.returncode,0,result.stderr)
        result,_=self.exercise(telemetry='empty',llm_count=42)
        self.assertNotEqual(result.returncode,0)
        self.assertIn('pre-event LLM dashboard was not empty',result.stderr)
        result,_=self.exercise(telemetry='codex',llm_count=42)
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('upstream_llm_request_count=42',result.stdout)

    def test_metrics_require_native_service_in_pinned_grouped_row(self):
        for overrides in ({'metric_service':'unrelated_service'}, {'metric_key':'ServiceName'}):
            with self.subTest(overrides=overrides):
                result,_=self.exercise(telemetry='codex',**overrides)
                self.assertNotEqual(result.returncode,0)
                self.assertIn('native Codex metrics missing from UI',result.stderr)

if __name__=='__main__': unittest.main()
