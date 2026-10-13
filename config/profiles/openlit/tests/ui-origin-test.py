#!/usr/bin/env python3
"""Owning fixture checks for real local-account and onboarding request contracts."""
import http.server
import json
from pathlib import Path
import re
import subprocess
import tempfile
import threading
import unittest
import urllib.parse

SCRIPT=Path(__file__).with_name('e2e')/'ui-origin.py'

class UIOriginTest(unittest.TestCase):
    def exercise(self, reject=False):
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
                elif self.path=='/api/auth/session': self.reply(200,{'user':{'email':'openlit-fixture@example.invalid'}} if self.authenticated else {})
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
                else: self.reply(404)
        with http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler) as server, tempfile.TemporaryDirectory(dir='/tmp') as root:
            thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
            try:
                result=subprocess.run(['python3',str(SCRIPT),'--origin','http://127.0.0.1:'+str(server.server_port),'--state',str(Path(root)/'cookies'),'--identity','fixture'],capture_output=True,text=True,timeout=10)
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

if __name__=='__main__': unittest.main()
