#!/usr/bin/env python3
"""Explicit inner loopback OTLP bridge to a privately mounted collector socket."""
import argparse
import http.client
import http.server
import os
from pathlib import Path
import stat
import threading

import records

LIMIT = 4*1024*1024


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 16

    def __init__(self, address, path):
        self.path = path
        self.slots = threading.BoundedSemaphore(16)
        super().__init__(address, Handler)

    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self.slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.slots.release()


class Handler(http.server.BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.connection.settimeout(5)

    def log_message(self, *args):
        pass

    def do_POST(self):
        if self.path not in ('/v1/logs', '/v1/metrics', '/v1/traces') or self.headers.get('Transfer-Encoding'):
            self.send_error(400)
            return
        try:
            size = int(self.headers.get('Content-Length', '-1'))
            if not 0 <= size <= LIMIT:
                self.send_error(413)
                return
            info = self.server.path.lstat()
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                raise ValueError('private socket required')
            body = self.rfile.read(size)
            if len(body) != size:
                raise ValueError('short body')
            headers = {name: self.headers[name] for name in ('Content-Type', 'Content-Encoding', 'x-subyard-agent-run-id') if name in self.headers}
            with records.LocalConnection(self.server.path) as connection:
                connection.request('POST', self.path, body, headers)
                response = connection.getresponse()
                content = response.read(LIMIT+1)
                if len(content) > LIMIT:
                    raise ValueError('oversized response')
                self.send_response(response.status)
                self.send_header('Content-Type', response.getheader('Content-Type', 'application/x-protobuf'))
                self.send_header('Content-Length', str(len(content)))
                self.end_headers()
                self.wfile.write(content)
        except (OSError, ValueError, http.client.HTTPException):
            self.send_error(503)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--socket', type=Path, required=True)
    parser.add_argument('--listen', default='127.0.0.1:14318')
    args = parser.parse_args()
    host, port = args.listen.rsplit(':', 1)
    if host != '127.0.0.1' or not 1024 <= int(port) <= 65535:
        parser.error('only unprivileged IPv4 loopback is supported')
    with Server((host, int(port)), args.socket) as server:
        server.serve_forever()


if __name__ == '__main__':
    main()
