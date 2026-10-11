#!/usr/bin/env python3
"""Exercise real native exporters with local synthetic model providers, then query OpenLIT."""
import http.server
import json
from pathlib import Path
import subprocess
import threading
import time

HOME = Path('/home/dev')
STATE = Path('/var/lib/subyard/openlit')
# Test customization after enable must survive ordinary refresh and disable.
claude = json.loads((HOME / '.claude/settings.json').read_text())
claude.setdefault('env', {})['OPENLIT_E2E_CUSTOM'] = 'preserved'
(HOME / '.claude/settings.json').write_text(json.dumps(claude))
# Customize an unmanaged field without importing another TOML dependency.
config = HOME / '.codex/config.toml'
text = config.read_text()
if 'model =' in text:
    import re
    text = re.sub(r'^model\s*=.*$', 'model = "fixture-custom-preserved"', text, flags=re.M)
else:
    text = 'model = "fixture-custom-preserved"\n' + text
config.write_text(text)


class Provider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers.get('content-length', '0'))))
        if 'count_tokens' in self.path:
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(b'{"input_tokens": 10}')
            return
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Connection', 'close')
        self.end_headers()
        if 'messages' in self.path:
            message = {'id': 'msg_fixture', 'type': 'message', 'role': 'assistant',
                       'content': [], 'model': data.get('model', 'fixture'), 'stop_reason': None,
                       'stop_sequence': None, 'usage': {'input_tokens': 10, 'output_tokens': 0}}
            events = [
                ('message_start', {'type': 'message_start', 'message': message}),
                ('content_block_start', {'type': 'content_block_start', 'index': 0, 'content_block': {'type': 'text', 'text': ''}}),
                ('content_block_delta', {'type': 'content_block_delta', 'index': 0, 'delta': {'type': 'text_delta', 'text': 'fixture'}}),
                ('content_block_stop', {'type': 'content_block_stop', 'index': 0}),
                ('message_delta', {'type': 'message_delta', 'delta': {'stop_reason': 'end_turn', 'stop_sequence': None}, 'usage': {'output_tokens': 1}}),
                ('message_stop', {'type': 'message_stop'})]
        else:
            item = {'id': 'msg_fixture', 'type': 'message', 'status': 'completed', 'role': 'assistant',
                    'content': [{'type': 'output_text', 'text': 'fixture', 'annotations': []}]}
            response = {'id': 'resp_fixture', 'object': 'response', 'status': 'completed', 'model': 'fixture',
                        'output': [item], 'usage': {'input_tokens': 10, 'output_tokens': 1, 'total_tokens': 11,
                        'input_tokens_details': {'cached_tokens': 0}, 'output_tokens_details': {'reasoning_tokens': 0}}}
            events = [
                ('response.created', {'type': 'response.created', 'response': {'id': 'resp_fixture', 'status': 'in_progress', 'output': []}}),
                ('response.output_item.added', {'type': 'response.output_item.added', 'output_index': 0, 'item': item}),
                ('response.output_text.delta', {'type': 'response.output_text.delta', 'item_id': 'msg_fixture', 'output_index': 0, 'content_index': 0, 'delta': 'fixture'}),
                ('response.output_item.done', {'type': 'response.output_item.done', 'output_index': 0, 'item': item}),
                ('response.completed', {'type': 'response.completed', 'response': response})]
        for name, event in events:
            self.wfile.write(('event: '+name+'\ndata: '+json.dumps(event)+'\n\n').encode())
        self.wfile.flush()


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Provider)
threading.Thread(target=server.serve_forever, daemon=True).start()
base = 'http://127.0.0.1:'+str(server.server_port)
env = {'HOME': str(HOME), 'PATH': '/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
       'ANTHROPIC_API_KEY': 'synthetic-local-fixture', 'ANTHROPIC_BASE_URL': base,
       'OTEL_METRIC_EXPORT_INTERVAL': '1000', 'OTEL_LOGS_EXPORT_INTERVAL': '1000',
       'DISABLE_AUTOUPDATER': '1'}
commands = {
    'claude': ['claude', '--print', '--model', 'fixture', '--dangerously-skip-permissions', 'Say fixture'],
    'codex': ['codex', '-c', 'model_provider="fixture"', '-c', 'model="fixture"',
              '-c', 'model_providers.fixture.name="Local fixture"',
              '-c', 'model_providers.fixture.base_url="'+base+'/v1"',
              '-c', 'model_providers.fixture.wire_api="responses"',
              '-c', 'model_providers.fixture.requires_openai_auth=false',
              'exec', '--skip-git-repo-check', 'Say fixture']}
for agent, command in commands.items():
    version = subprocess.check_output(['runuser', '-u', 'dev', '--', command[0], '--version'], cwd='/tmp', env=env, timeout=15, text=True).strip()
    print('native_version='+version, flush=True)
    result = subprocess.run(['runuser', '-u', 'dev', '--', *command], cwd='/tmp', env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=90, text=True)
    if result.returncode != 0:
        print('native_failure='+agent+' exit='+str(result.returncode), flush=True)
        # Fixture has only synthetic local API state; bounded error helps distinguish API/config failure.
        print(result.stdout[-4000:], flush=True)
        raise SystemExit(1)
server.shutdown()

# Query the actual OpenLIT ClickHouse tables from inside its private network; never expose DB credentials.
compose = ['docker', 'compose', '--env-file', str(STATE/'database.env'), '-f', str(STATE/'compose.yaml')]
def query(sql):
    return subprocess.check_output([*compose, 'exec', '-T', 'clickhouse', 'bash', '-c',
        'exec clickhouse-client --user openlit --password "$CLICKHOUSE_PASSWORD" --database openlit --query "$1"',
        'fixture', sql], timeout=10, text=True).strip()

deadline = time.monotonic()+90
found = {}
while time.monotonic() < deadline:
    logs = query("SELECT DISTINCT ServiceName FROM otel_logs ORDER BY ServiceName FORMAT JSONEachRow")
    metrics = query("SELECT DISTINCT ServiceName FROM otel_metrics_sum UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_metrics_histogram FORMAT JSONEachRow")
    found = {'logs': logs, 'metrics': metrics}
    if all(any(agent in signal.lower() for agent in (name,)) for name in ('claude', 'codex') for signal in (logs, metrics)):
        break
    time.sleep(2)
else:
    print('native_ingestion_missing='+json.dumps(found), flush=True)
    raise SystemExit(1)
print('native_openlit_tables='+json.dumps(found), flush=True)
print('ok: genuine Claude Code and Codex native logs and metrics reached OpenLIT ClickHouse', flush=True)
