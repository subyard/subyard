#!/usr/bin/env python3
"""Exercise local-account auth through the actual published origin, without external accounts."""
import argparse
import http.cookiejar
import json
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
    args=parser.parse_args()
    origin=args.origin.rstrip('/')
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
        result=json_request('/api/auth/callback/register',{'csrfToken':csrf,'email':'openlit-'+args.identity+'@example.invalid',
            'password':'Synthetic-fixture-7-'+args.identity,'callbackUrl':origin+'/dashboard','json':'true'})
        target=result.get('url','')
        assert target and urllib.parse.urlsplit(target).netloc==urllib.parse.urlsplit(origin).netloc, 'registration callback escaped published origin'
    session=json_request('/api/auth/session')
    assert session.get('user',{}).get('email')=='openlit-'+args.identity+'@example.invalid', 'account session is missing or changed'
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
        assert session.get('user',{}).get('email')=='openlit-'+args.identity+'@example.invalid', 'onboarding changed the authenticated account'
        code,_,_=request('/dashboard')
    assert code==200, 'authenticated dashboard did not open (HTTP '+str(code)+')'
    jar.save(ignore_discard=True,ignore_expires=True)
    os.chmod(args.state,0o600)
    print('ok: published-origin account, session and dashboard'+(' retained after re-enable' if args.resume else ''))


if __name__=='__main__': main()
