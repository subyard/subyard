#!/usr/bin/env python3
"""Exercise local-account auth through the actual published origin, without external accounts."""
import argparse
import datetime
import http.cookiejar
import json
import math
import os
from pathlib import Path
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        return None


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--origin',required=True)
    parser.add_argument('--state',type=Path,required=True)
    parser.add_argument('--identity',required=True)
    parser.add_argument('--resume',action='store_true')
    parser.add_argument('--credentials',type=Path)
    parser.add_argument('--telemetry',choices=['empty','codex'])
    args=parser.parse_args()
    origin=args.origin.rstrip('/')
    credentials=None
    if args.credentials:
        assert not args.credentials.is_symlink() and args.credentials.stat().st_mode & 0o777==0o600, 'unsafe fixture credential file'
        credentials=json.loads(args.credentials.read_text())
    email=credentials['email'] if credentials else 'openlit-'+args.identity+'@example.invalid'
    jar=http.cookiejar.LWPCookieJar(str(args.state))
    if args.resume: jar.load(ignore_discard=True,ignore_expires=True)
    opener=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar),NoRedirect())
    def request(path,payload=None,json_body=False):
        data=(json.dumps(payload).encode() if json_body else urllib.parse.urlencode(payload).encode()) if payload is not None else None
        headers={'Origin':origin}
        if payload is not None: headers['Content-Type']='application/json' if json_body else 'application/x-www-form-urlencoded'
        call=urllib.request.Request(origin+path,data=data,headers=headers)
        try:
            response=opener.open(call,timeout=15)
        except urllib.error.HTTPError as error:
            response=error
        return response.code,response.headers,response.read()
    def json_request(path,payload=None,json_body=False):
        code,headers,data=request(path,payload,json_body)
        if code!=200:
            # Never print response bodies, cookies or raw error query values.
            summary={'path':path,'status':code,'content_type':headers.get_content_type()}
            try:
                result=json.loads(data)
                target=urllib.parse.urlsplit(result.get('url','')) if isinstance(result,dict) else None
                if target:
                    summary.update(redirect_origin=target.scheme+'://'+target.netloc,redirect_path=target.path)
                    error=urllib.parse.parse_qs(target.query).get('error',[''])[0].lower()
                    summary['error_category']='password-policy' if 'password' in error else 'credentials' if 'credentials' in error else 'other' if error else 'none'
            except (ValueError,TypeError): pass
            raise AssertionError('API request failed: '+json.dumps(summary,sort_keys=True))
        return json.loads(data)
    if not args.resume:
        code,headers,_=request('/dashboard')
        assert code in (301,302,303,307,308), 'anonymous dashboard was not redirected'
        target=urllib.parse.urljoin(origin,headers['Location'])
        assert urllib.parse.urlsplit(target).netloc==urllib.parse.urlsplit(origin).netloc, 'dashboard redirect escaped published origin'
        assert urllib.parse.urlsplit(target).path=='/login', 'anonymous dashboard did not reach login'
        csrf=json_request('/api/auth/csrf')['csrfToken']
        result=json_request('/api/auth/callback/'+('login' if credentials else 'register'),{'csrfToken':csrf,'email':email,
            'password':credentials['password'] if credentials else 'Synthetic-fixture-7-'+args.identity,'callbackUrl':origin+'/dashboard','json':'true'})
        target=result.get('url','')
        assert target and urllib.parse.urlsplit(target).netloc==urllib.parse.urlsplit(origin).netloc, 'registration callback escaped published origin'
    session=json_request('/api/auth/session')
    assert session.get('user',{}).get('email')==email, 'account session is missing or changed'
    code,headers,_=request('/dashboard')
    if not args.resume and code in (301,302,303,307,308):
        target=urllib.parse.urlsplit(urllib.parse.urljoin(origin,headers['Location']))
        assert target.netloc==urllib.parse.urlsplit(origin).netloc and target.path=='/onboarding', 'authenticated dashboard redirected unexpectedly'
        # Follow the pinned UI's normal "skip setup" flow through its authenticated APIs.
        organisation=json_request('/api/organisation',{'name':'Personal Organisation'},json_body=True)
        assert organisation.get('id'), 'onboarding organisation was not created'
        json_request('/api/organisation/current/'+organisation['id'],{},json_body=True)
        assert json_request('/api/user/complete-onboarding',{},json_body=True).get('success'), 'onboarding did not complete'
        session=json_request('/api/auth/session')
        assert session.get('user',{}).get('email')==email, 'onboarding changed the authenticated account'
        code,_,_=request('/dashboard')
    assert code==200, 'authenticated dashboard did not open (HTTP '+str(code)+')'
    if args.telemetry:
        assert credentials, 'native UI reads require the provisioned account/default DB'
        assert json_request('/api/db-config'), 'provisioned account has no database link'
        now=datetime.datetime.now(datetime.timezone.utc)
        payload={'timeLimit':{'start':(now-datetime.timedelta(days=1)).isoformat(), 'end':now.isoformat(), 'type':'custom'},
                 'limit':25,'offset':0,'selectedConfig':{},'sorting':{},'environment':''}
        for signal,path in (('logs','/api/telemetry/logs'),('metrics','/api/telemetry/metrics'),('traces','/api/telemetry/trace')):
            result=json_request(path,payload,json_body=True)
            assert not result.get('err'), 'native '+signal+' UI query failed'
            records=result.get('records',[])
            if args.telemetry=='empty':
                assert signal!='traces' or not records, 'trace table was not empty before native events'
            else:
                # The pinned Metrics list groups series into camel-case fields;
                # Logs and Traces retain the ClickHouse ServiceName field.
                service_key='serviceName' if signal=='metrics' else 'ServiceName'
                assert any('codex' in str(row.get(service_key,'')).lower() for row in records), 'native Codex '+signal+' missing from UI'
            print('native_ui_signal='+signal+' records='+str(len(records)))
        # Before any events this dashboard must be empty. After native traces,
        # upstream's LLM count can include generic spans; report it without
        # interpreting it as actual model calls or token/cost evidence.
        result=json_request('/api/metrics/request/total',{**payload,'operationType':'llm'},json_body=True)
        assert not result.get('err'), 'LLM empty-state query failed'
        raw_count=result['data'][0]['total_requests']
        assert isinstance(raw_count,(int,float,str)) and not isinstance(raw_count,bool), 'invalid LLM request count'
        try: count=float(raw_count)
        except (ValueError,TypeError): raise AssertionError('invalid LLM request count') from None
        assert math.isfinite(count) and count>=0 and count.is_integer(), 'invalid LLM request count'
        count=int(count)
        if args.telemetry=='empty': assert count==0, 'pre-event LLM dashboard was not empty'
        print('upstream_llm_request_count='+str(count))
        code,_,_=request('/telemetry?tab=logs')
        assert code==200, 'native telemetry landing page did not open'
    jar.save(ignore_discard=True,ignore_expires=True)
    os.chmod(args.state,0o600)
    print('ok: published-origin account, session and dashboard'+(' retained after re-enable' if args.resume else ''))


if __name__=='__main__': main()
