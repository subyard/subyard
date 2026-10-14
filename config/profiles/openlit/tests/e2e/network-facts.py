#!/usr/bin/env python3
"""Bounded network observations; completion is not registry/blob connectivity."""
import argparse
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import subprocess
import sys

REGISTRY = 'registry-1.docker.io'
WRITE_OUT = '%{http_code} %{time_namelookup} %{time_connect} %{time_appconnect} %{time_total} %{remote_ip}'


def public_ipv4(value):
    address = ipaddress.ip_address(value)
    if address.version != 4 or not address.is_global:
        raise ValueError('public IPv4 required')
    return str(address)


def registry_addresses():
    try:
        result = subprocess.run(['getent', 'ahostsv4', REGISTRY], capture_output=True, timeout=3)
        addresses = set()
        for line in result.stdout.decode(errors='replace').splitlines():
            try:
                addresses.add(public_ipv4(line.split()[0]))
            except (ValueError, IndexError):
                pass
        return sorted(addresses, key=lambda value: int(ipaddress.ip_address(value)))[:4]
    except (OSError, subprocess.TimeoutExpired):
        return []


def route_facts(addresses):
    if not addresses:
        return {'observed': False}
    try:
        result = subprocess.run(['ip', '-j', 'route', 'get', addresses[0]], capture_output=True, timeout=2)
        value = json.loads(result.stdout)
        row = value[0]
        device = row.get('dev', '')
        if not re.fullmatch('[a-zA-Z0-9_.-]{1,32}', device):
            raise ValueError()
        mtu = int((Path('/sys/class/net')/device/'mtu').read_text())
        if not 68 <= mtu <= 65536:
            raise ValueError()
        return {'observed': result.returncode == 0, 'interface_mtu': mtu,
                'gateway_present': bool(row.get('gateway'))}
    except (OSError, ValueError, TypeError, IndexError, KeyError, subprocess.TimeoutExpired):
        return {'observed': False}


def curl_facts(url, options):
    argv = ['curl', '-q', '--silent', '--show-error', '--connect-timeout', '5',
            '--max-time', '10', '--output', '/dev/null', '--write-out', WRITE_OUT,
            *options, url]
    try:
        result = subprocess.run(argv, capture_output=True, timeout=11)
        fields = result.stdout.decode(errors='replace').strip().split()
        value = {'exit_code': result.returncode, 'http_status': None,
                 'tls_stage': 'unknown', 'timings': {}}
        if len(fields) not in (5, 6) or not re.fullmatch('[0-9]{3}', fields[0]):
            value['category'] = 'invalid-curl-projection'
            return value
        value['http_status'] = int(fields[0])
        for key, field in zip(('dns', 'connect', 'tls', 'total'), fields[1:5]):
            if not re.fullmatch(r'[0-9]+(?:\.[0-9]+)?', field) or not 0 <= float(field) <= 11:
                value['category'] = 'invalid-curl-projection'
                return value
            value['timings'][key] = float(field)
        times = value['timings']
        value['tls_stage'] = ('http-response' if value['http_status'] else
                              'tls-complete' if times['tls'] else
                              'tcp-connected' if times['connect'] else
                              'dns-complete' if times['dns'] else 'not-connected')
        if len(fields) == 6:
            try:
                address = public_ipv4(fields[5])
                value['remote_public_ipv4'] = True
                # Default/IPv4 requests may inherit a private proxy setting.
                # Only direct connections may expose the public peer address.
                if '--noproxy' in options:
                    value['remote_ipv4'] = address
            except ValueError:
                value['remote_public_ipv4'] = False
        return value
    except (OSError, subprocess.TimeoutExpired) as error:
        return {'category': type(error).__name__, 'tls_stage': 'unknown'}


def probe(location, shared_addresses):
    own_addresses = registry_addresses()
    encoded = '\n'.join(own_addresses).encode()
    print('openlit_e2e_network='+json.dumps({
        'location': location, 'mode': 'dns-route', 'dns_ipv4': own_addresses,
        'dns_ipv4_count': len(own_addresses), 'dns_sha256': hashlib.sha256(encoded).hexdigest(),
        'shared_ipv4_count': len(shared_addresses),
        'shared_dns_matches': len(set(shared_addresses)&set(own_addresses)),
        'route': route_facts(shared_addresses or own_addresses)}, sort_keys=True), flush=True)
    url = 'https://registry-1.docker.io/v2/'
    cases = [('registry-default', url, []), ('registry-ipv4', url, ['-4']),
             ('registry-direct-ipv4', url, ['-4', '--noproxy', '*'])]
    for address in shared_addresses:
        cases.append(('registry-direct-resolved-ipv4', url,
                      ['-4', '--noproxy', '*', '--resolve', REGISTRY+':443:'+address]))
    cases.extend([
        ('dockerhub-auth', 'https://auth.docker.io/token?service=registry.docker.io&scope=repository:clickhouse/clickhouse-server:pull', []),
        ('ghcr', 'https://ghcr.io/v2/', []),
        ('ghcr-auth', 'https://ghcr.io/token?service=ghcr.io&scope=repository:openlit/openlit:pull', []),
    ])
    for mode, target, options in cases:
        facts = {'location': location, 'mode': mode, **curl_facts(target, options)}
        if '--resolve' in options:
            facts['resolved_ipv4'] = options[-1].rsplit(':', 1)[1]
        print('openlit_e2e_network='+json.dumps(facts, sort_keys=True), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('addresses', 'probe'))
    parser.add_argument('--location', choices=('owner', 'guest'))
    parser.add_argument('--registry-ip', action='append', default=[])
    args = parser.parse_args()
    try:
        if len(args.registry_ip) > 4:
            raise ValueError()
        addresses = [public_ipv4(value) for value in args.registry_ip]
        if len(set(addresses)) != len(addresses):
            raise ValueError()
        if args.mode == 'addresses':
            if args.location or addresses:
                raise ValueError()
            print(json.dumps(registry_addresses()))
        elif args.location:
            probe(args.location, addresses)
        else:
            raise ValueError()
    except ValueError:
        print('openlit_e2e_network_error=invalid-probe-arguments', file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
