// Exercise the actual guide templates with deterministic native tools and supplied API responses.
import assert from 'node:assert/strict';
import {execSync, spawnSync} from 'node:child_process';
import {mkdtempSync, readFileSync, rmSync, writeFileSync, appendFileSync, existsSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import {join} from 'node:path';

const root = fileURLToPath(new URL('../', import.meta.url));
const guide = readFileSync(join(root, 'docs/testing.md'), 'utf8');
const compile = marker => new (Object.getPrototypeOf(async function () {}).constructor)
  ('tools', 'load', 'store', 'text', 'notify', [...guide.matchAll(/```js\n([^]*?)\n```/g)].find(match => match[1].includes(marker))[1]);
const step = compile('observer-poll-template'), githubStep = compile('github-observer-template');
const directory = mkdtempSync('/tmp/observer-poll-test-');
let fixtureNumber = 0, cases = 0;
const event = fields => JSON.stringify(fields) + '\n';
const native = exit_code => ({output: '', exit_code, chunk_id: 'native-chunk', wall_time_seconds: 0.001,
  nested: {evidence: ['preserve', 'every', 'field']}});
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done; }); return {promise, resolve}; };
const tick = () => new Promise(resolve => setImmediate(resolve));

function fixture(definitions) {
  const saved = new Map(), outputs = [], notifications = [], buffered = [], calls = [], states = new Map();
  for (const [id, records, polls] of definitions) {
    const journal = join(directory, `${++fixtureNumber}-${id}.jsonl`);
    writeFileSync(journal, records);
    const state = {session_id: fixtureNumber, journal, terminal_path: join(directory, `terminal-${fixtureNumber}.json`),
      cursor: null, backlog: false, terminal: false, closed: false};
    states.set(id, {state, polls});
    saved.set('observer-source:' + id, state);
  }
  saved.set('observer-sources', [...states.keys()]);
  const tools = {
    async write_stdin(args) {
      const [id, source] = [...states].find(([, item]) => item.state.session_id === args.session_id);
      assert.deepEqual(args, {session_id: source.state.session_id, chars: '', yield_time_ms: 55000});
      calls.push(id + ':poll');
      assert.ok(source.polls.length, 'no duplicate terminal poll');
      return await source.polls.shift()();
    },
    async apply_patch(patch) {
      const path = patch.split('\n')[1].slice('*** Add File: '.length);
      const [id] = [...states].find(([, item]) => item.state.terminal_path === path);
      calls.push(id + ':save');
      assert.ok(saved.has('observer-terminal:' + id), 'full native store precedes artifact save');
      const lines = patch.split('\n');
      writeFileSync(path, lines.slice(2, -1).map(line => line.slice(1)).join('\n'));
      return {};
    },
    async exec_command(args) {
      const [id, source] = [...states].find(([, item]) => args.cmd.includes(item.state.journal.replaceAll("'", "'\\''")));
      const projection = args.cmd.startsWith('python3 dev/');
      calls.push(id + (projection ? ':projection' : ':snapshot'));
      if (projection && source.state.terminal)
        assert.ok(saved.has('observer-terminal:' + id), 'full native store precedes final projection');
      try {
        return {exit_code: 0, output: execSync(args.cmd, {cwd: root, encoding: 'utf8', maxBuffer: 1048576,
          stdio: ['ignore', 'pipe', 'pipe']})};
      } catch (error) {
        return {exit_code: error.status, output: error.stdout};
      }
    },
  };
  return {saved, outputs, notifications, buffered, calls, tools, states, state: id => states.get(id).state,
    run: async () => {
      try {
        await step(tools, key => saved.get(key), (key, value) => saved.set(key, value),
          value => buffered.push(value), value => {
            const decoded = JSON.parse(value);
            notifications.push(decoded);
            outputs.push(decoded);
          });
      } finally {
        outputs.push(...buffered.splice(0)); // text() reaches the caller only when the cell ends.
      }
    }};
}

try {
  const failed = native(7);
  const empty = fixture([['empty', '', [async () => failed]]]);
  await empty.run();
  assert.deepEqual(empty.calls, ['empty:snapshot', 'empty:poll', 'empty:save', 'empty:projection']);
  assert.deepEqual(empty.saved.get('observer-terminal:empty'), failed);
  assert.deepEqual(JSON.parse(readFileSync(empty.state('empty').terminal_path)), failed);
  assert.equal(empty.outputs[0].native_exit_code, 7);
  await empty.run();
  assert.equal(empty.calls.filter(call => call.endsWith(':poll')).length, 1);
  cases++;

  const broken = fixture([['broken', '{broken}\n', [async () => native(23)]]]);
  await broken.run();
  assert.equal(broken.saved.get('observer-terminal:broken').exit_code, 23);
  assert.match(broken.outputs[1].observation_error, /malformed complete JSON/);
  assert.equal(broken.state('broken').cursor, null);
  assert.equal(broken.state('broken').projection_disabled, true);
  await broken.run();
  assert.equal(broken.calls.filter(call => call.endsWith(':poll')).length, 1);
  cases++;

  const saveError = fixture([['save', '', [async () => native(9)]]]);
  saveError.tools.apply_patch = async () => { throw new Error('artifact write unavailable'); };
  await saveError.run();
  assert.equal(saveError.saved.get('observer-terminal:save').exit_code, 9);
  assert.match(saveError.outputs[1].capture_error, /artifact write unavailable/);
  assert.equal(saveError.state('save').closed, true);
  cases++;

  const early = fixture([['early', event({event: 'check-result', status: 'failed', exit_code: 4}),
    [async () => ({session_id: 42, output: ''}), async () => native(4)]]]);
  await early.run();
  assert.equal(early.outputs[0].events[0].status, 'failed');
  const acceptedOffset = early.state('early').cursor.offset;
  appendFileSync(early.state('early').journal, event({event: 'run-result', status: 'failed'}));
  await early.run();
  assert.equal(early.outputs[2].events[0].event, 'run-result');
  assert.ok(early.state('early').cursor.offset > acceptedOffset);
  cases++;

  const first = deferred(), second = deferred();
  const waiting = fixture([['wait', event({event: 'heartbeat'}), [() => first.promise, () => second.promise]]]);
  const cell = waiting.run();
  await tick();
  assert.equal(waiting.outputs.length, 0);
  first.resolve({session_id: 42, output: ''});
  await cell;
  assert.equal(waiting.outputs.length, 0, 'unchanged heartbeat has no acknowledgement');
  const cursor = structuredClone(waiting.state('wait').cursor);
  const nextCell = waiting.run();
  await tick();
  assert.deepEqual(waiting.state('wait').cursor, cursor);
  second.resolve(native(0));
  await nextCell;
  assert.deepEqual(waiting.state('wait').cursor, cursor);
  assert.equal(waiting.outputs.length, 1);
  cases++;

  const backlog = fixture([['backlog', event({event: 'heartbeat'}).repeat(201)
    + event({event: 'check-result', status: 'failed'}), [async () => native(1)]]]);
  backlog.saved.set('observer-max-pages', 1);
  await backlog.run();
  assert.equal(backlog.state('backlog').backlog, true);
  assert.equal(backlog.state('backlog').closed, false);
  assert.ok(backlog.outputs.at(-1).pending);
  const boundary = backlog.state('backlog').pending_eof;
  appendFileSync(backlog.state('backlog').journal, event({event: 'heartbeat'}));
  backlog.saved.set('observer-max-pages', 10);
  await backlog.run();
  assert.equal(backlog.outputs.at(-1).events[0].status, 'failed');
  assert.equal(backlog.state('backlog').cursor.offset, boundary);
  assert.equal(backlog.calls.filter(call => call.endsWith(':poll')).length, 1);
  cases++;

  // A fast terminal object's failure is stored/reported before a slow peer or failed save finishes.
  const slow = deferred(), save = deferred();
  const parallel = fixture([['slow', '', [() => slow.promise]], ['fast', '', [async () => native(19)]]]);
  parallel.tools.apply_patch = () => save.promise;
  const concurrent = parallel.run();
  await tick();
  assert.equal(parallel.saved.get('observer-terminal:fast').exit_code, 19);
  assert.equal(parallel.outputs[0].source, 'fast');
  assert.equal(parallel.outputs[0].native_exit_code, 19);
  assert.equal(parallel.notifications[0].native_exit_code, 19, 'native failure is delivered before slow peer/save releases');
  assert.equal(parallel.buffered.length, 0, 'native failure must not rely on buffered text');
  save.resolve({isError: true});
  await tick();
  assert.ok(parallel.outputs.some(item => item.capture_error));
  slow.resolve({session_id: 1, output: ''});
  await concurrent;
  assert.ok(parallel.state('fast').cursor);
  cases++;

  // A journal failure notifies before a peer poll and the remainder of its own drain finish.
  const peer = deferred(), remainingDrain = deferred();
  const journalFailure = fixture([['peer', '', [() => peer.promise, async () => ({output: ''})]],
    ['material', event({event: 'check-result', status: 'failed', exit_code: 23})
      + event({event: 'heartbeat'}).repeat(101), [async () => ({output: ''}), async () => ({output: ''})]]]);
  const project = journalFailure.tools.exec_command;
  let materialPages = 0;
  journalFailure.tools.exec_command = async args => {
    if (args.cmd.startsWith('python3 dev/') && args.cmd.includes(journalFailure.state('material').journal)
        && ++materialPages === 2) await remainingDrain.promise;
    return project(args);
  };
  const materialCell = journalFailure.run();
  await tick();
  assert.equal(journalFailure.notifications[0].source, 'material');
  assert.equal(journalFailure.notifications[0].events[0].exit_code, 23);
  assert.equal(journalFailure.buffered.length, 0, 'material failure must not rely on buffered text');
  remainingDrain.resolve();
  await tick();
  assert.equal(journalFailure.notifications.length, 1);
  peer.resolve({output: ''});
  await materialCell;
  await journalFailure.run();
  assert.equal(journalFailure.outputs.length, 1, 'accepted failure is not replayed on an unchanged step');
  assert.equal(journalFailure.notifications.length, 1);
  cases++;

  // Second-source failure cannot lose first-source native evidence or cursor.
  const rejected = fixture([['one', event({event: 'run-result', status: 'passed'}), [async () => native(0)]],
    ['two', '', [async () => { throw new Error('native poll unavailable'); }]]]);
  await rejected.run();
  assert.equal(rejected.saved.get('observer-terminal:one').exit_code, 0);
  assert.ok(rejected.state('one').cursor.offset > 0);
  assert.ok(rejected.outputs.some(item => item.source === 'two' && item.observation_error));
  cases++;

  // Ready sources share the finite page budget fairly and retain independent pending cursors.
  const fair = fixture(['a', 'b'].map(id => [id, event({event: 'heartbeat'}).repeat(250),
    [async () => ({session_id: 1, output: ''})]]));
  fair.saved.set('observer-max-pages', 3);
  await fair.run();
  const turns = fair.calls.filter(call => call.endsWith(':projection'));
  assert.deepEqual(turns, ['a:projection', 'b:projection', 'a:projection']);
  assert.equal(fair.state('a').backlog, true);
  assert.equal(fair.state('b').backlog, true);
  cases++;

  // Records appended after the initial active EOF wait for a later step, without extending this drain.
  const growing = fixture([['growing', event({event: 'heartbeat'}).repeat(101),
    [async () => ({session_id: 1, output: ''}), async () => ({session_id: 1, output: ''})]]]);
  const execute = growing.tools.exec_command;
  growing.tools.exec_command = async args => {
    const result = await execute(args);
    if (args.cmd.startsWith('python3 dev/')) appendFileSync(growing.state('growing').journal,
      event({event: 'check-result', status: 'failed'}));
    return result;
  };
  await growing.run();
  assert.equal(growing.outputs.length, 0);
  growing.tools.exec_command = execute;
  await growing.run();
  assert.ok(growing.outputs[0].events.every(item => item.status === 'failed'));
  cases++;

  const tail = fixture([['tail', '{"event":', [async () => native(0)]]]);
  await tail.run();
  assert.match(tail.outputs.at(-1).observation_error, /incomplete terminal journal tail/);
  assert.equal(tail.state('tail').closed, true);
  cases++;

  const thrown = fixture([['throw', '', [async () => native(11)]]]);
  const exec = thrown.tools.exec_command;
  thrown.tools.exec_command = args => args.cmd.startsWith('python3 dev/')
    ? Promise.reject(new Error('projection transport unavailable')) : exec(args);
  await thrown.run();
  assert.equal(thrown.saved.get('observer-terminal:throw').exit_code, 11);
  assert.match(thrown.outputs.at(-1).observation_error, /projection transport unavailable/);
  cases++;

  const marker = join(directory, 'unexpected-command');
  const quoted = fixture([["quotes '\n$(printf ignored) `printf ignored`", '', [async () => native(0)]]]);
  const id = [...quoted.states.keys()][0];
  quoted.state(id).cursor = {stream: [1, 2], offset: 0, size: 0,
    anchor: "'\n$(touch " + marker + ") `touch " + marker + "`"};
  await quoted.run();
  assert.match(quoted.outputs.at(-1).observation_error, /journal replaced/);
  assert.equal(existsSync(marker), false);
  cases++;

  // Supplied GitHub responses: one query per active ID, independent completion and API errors.
  const saved = new Map([['github-sources', ['owner/repo/1', 'owner/repo/2', 'owner/repo/3']]]);
  for (const id of saved.get('github-sources')) saved.set('github-source:' + id,
    {repository: 'owner/repo', run_id: id.split('/').at(-1), closed: false});
  const responses = new Map([[1, [{id: 1, status: 'queued', conclusion: null}, {id: 1, status: 'in_progress', conclusion: null},
    {id: 1, status: 'completed', conclusion: 'success'}]], [2, [{id: 2, status: 'completed', conclusion: 'failure'}]],
    [3, ['api-failure', {id: 3, status: 'completed', conclusion: 'cancelled'}]]]);
  const githubCalls = [], outputs = [], api = {async exec_command(args) {
    assert.match(args.cmd, /^subyard-github run -- gh api repos\/owner\/repo\/actions\/runs\/\d+$/);
    const id = Number(args.cmd.split('/').at(-1));
    githubCalls.push(id);
    const response = responses.get(id).shift();
    return response === 'api-failure' ? {exit_code: 1, output: 'denied'} : {exit_code: 0, output: JSON.stringify(response), extra: 'native'};
  }};
  const githubRun = async () => {
    const buffered = [];
    await githubStep(api, key => saved.get(key), (key, value) => saved.set(key, value),
      value => buffered.push(value), value => outputs.push(JSON.parse(value)));
    outputs.push(...buffered);
  };
  await githubRun();
  assert.deepEqual(githubCalls, [1, 2, 3]);
  assert.equal(saved.get('github-source:owner/repo/2').closed, true);
  assert.equal(saved.get('github-response:owner/repo/2').conclusion, 'failure');
  assert.ok(outputs.find(item => item.source === 'owner/repo/3').observation_error);
  assert.equal(saved.get('github-source:owner/repo/3').closed, false);
  await githubRun();
  await githubRun();
  await githubRun();
  assert.deepEqual(githubCalls, [1, 2, 3, 1, 3, 1]);
  assert.equal(saved.get('github-native:owner/repo/1').extra, 'native');
  assert.equal(saved.get('github-source:owner/repo/1').conclusion, 'success');
  cases++;

  // Real producer -> saved sidecar -> compact journal -> observer, with legacy aggregate exit 1.
  const produced = join(directory, 'produced');
  const producer = spawnSync('python3', ['-B', '-c', `
import importlib.util, sys
from pathlib import Path
from unittest.mock import patch
spec = importlib.util.spec_from_file_location('acceptance', Path(sys.argv[1]) / 'dev/release-acceptance.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
output = Path(sys.argv[2]); output.mkdir()
checks = {'verify': ['true'], 'compatibility': ['true']}
receipt = {'version': 'test', 'profiles': {}, 'source_fingerprint': 'a'*64,
           'candidate_bundle_sha256': 'b'*64, 'external_obligations': {},
           'checks': {name: {'status': 'pending'} for name in checks}}
with patch.object(m, 'load_bound', return_value=(output, receipt)), patch.object(m, 'commands', return_value=checks), patch.object(m, 'source_fingerprint', return_value='a'*64), patch.object(m, 'execution_environment', return_value={}):
    sys.exit(m.run_checks(output, [], ['verify'], False))
`, root, produced], {encoding: 'utf8'});
  assert.equal(producer.status, 1, producer.stderr);
  const producerNative = {...native(producer.status), output: producer.stdout};
  const integrated = fixture([['producer', '', [async () => producerNative]]]);
  integrated.state('producer').journal = join(produced, 'events.jsonl');
  await integrated.run();
  const final = integrated.outputs.flatMap(item => item.events ?? []).find(item => item.event === 'run-result');
  assert.equal(final.selected_status, 'passed');
  assert.equal(final.overall_status, 'incomplete');
  const sidecar = JSON.parse(readFileSync(join(produced, final.result)));
  assert.equal(sidecar.invocation_id, final.invocation_id);
  assert.equal(sidecar.receipt_sha256, final.receipt_sha256);
  assert.equal(integrated.saved.get('observer-terminal:producer').exit_code, 1);
  cases++;
  console.log(`PASS: ${cases} observer contracts (multi-source native-FIRST, errors, fair bounded backlog, EOF, dedup, supplied GitHub statuses)`);
} finally {
  rmSync(directory, {recursive: true, force: true});
}
