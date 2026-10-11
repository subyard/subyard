// Exercise the actual guide template with short deterministic native-tool stubs.
import assert from 'node:assert/strict';
import {execSync} from 'node:child_process';
import {mkdtempSync, readFileSync, rmSync, writeFileSync, appendFileSync, existsSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import {join} from 'node:path';

const root = fileURLToPath(new URL('../', import.meta.url));
const guide = readFileSync(join(root, 'docs/testing.md'), 'utf8');
const source = guide.match(/```js\n([^]*?observer-poll-template[^]*?)\n```/)[1];
const step = new (Object.getPrototypeOf(async function () {}).constructor)('tools', 'load', 'store', 'text', source);
const directory = mkdtempSync('/tmp/observer-poll-test-');
let fixtureNumber = 0;
const event = fields => JSON.stringify(fields) + '\n';
const native = exit_code => ({output: '', exit_code, chunk_id: 'native-chunk', wall_time_seconds: 0.001,
  nested: {evidence: ['preserve', 'every', 'field']}});

function fixture(name, records, polls) {
  const journal = join(directory, name);
  writeFileSync(journal, records);
  const state = {session_id: 42, journal, terminal_path: join(directory, `terminal-${++fixtureNumber}.json`), cursor: null,
    backlog: false, terminal: false};
  const saved = new Map([['observer-run', state]]);
  const outputs = [];
  const calls = [];
  const tools = {
    async write_stdin(args) {
      assert.deepEqual(args, {session_id: 42, chars: '', yield_time_ms: 55000});
      calls.push('poll');
      assert.ok(polls.length, 'no duplicate terminal poll');
      return await polls.shift()();
    },
    async apply_patch(patch) {
      calls.push('save');
      assert.ok(saved.has('observer-native-terminal'), 'store precedes artifact save');
      const lines = patch.split('\n');
      writeFileSync(state.terminal_path, lines.slice(2, -1).map(line => line.slice(1)).join('\n'));
      return {};
    },
    async exec_command(args) {
      calls.push('projection');
      if (state.terminal) {
        assert.ok(saved.has('observer-native-terminal'), 'native object precedes final read');
        assert.deepEqual(JSON.parse(readFileSync(state.terminal_path)), saved.get('observer-native-terminal'));
      }
      try {
        return {exit_code: 0, output: execSync(args.cmd, {cwd: root, encoding: 'utf8', maxBuffer: 1048576,
          stdio: ['ignore', 'pipe', 'pipe']})};
      } catch (error) {
        return {exit_code: error.status, output: error.stdout};
      }
    },
  };
  return {state, saved, outputs, calls, tools,
    run: () => step(tools, key => saved.get(key), (key, value) => saved.set(key, value), value => outputs.push(value))};
}

try {
  // Empty stdout is still a terminal failure, with the full object persisted before reads.
  const failed = native(7);
  const empty = fixture('empty.jsonl', '', [async () => failed]);
  await empty.run();
  assert.deepEqual(empty.calls, ['poll', 'save', 'projection']);
  assert.deepEqual(empty.saved.get('observer-native-terminal'), failed);
  assert.equal(empty.outputs[0].terminal, true);

  // A projection error after terminal cannot overwrite native outcome or advance cursor.
  const broken = fixture('broken.jsonl', '{broken}\n', [async () => native(23)]);
  await broken.run();
  assert.equal(broken.saved.get('observer-native-terminal').exit_code, 23);
  assert.match(broken.outputs[0].observation_error, /malformed complete JSON/);
  assert.equal(broken.state.cursor, null);
  assert.equal(broken.state.projection_disabled, true);
  await broken.run();
  assert.equal(broken.calls.filter(call => call === 'poll').length, 1);

  // Native artifact-save errors also leave the full terminal object recoverable.
  const saveError = fixture('save-error.jsonl', '', [async () => native(9)]);
  saveError.tools.apply_patch = async () => { throw new Error('artifact write unavailable'); };
  await assert.rejects(saveError.run(), /artifact write unavailable/);
  assert.equal(saveError.saved.get('observer-native-terminal').exit_code, 9);
  assert.equal(saveError.state.terminal, true);
  assert.deepEqual(saveError.calls, ['poll']);

  // An early failed check returns while the same runner is active; next step has no replay.
  const early = fixture('early.jsonl', event({event: 'check-result', status: 'failed', exit_code: 4}),
    [async () => ({session_id: 42, output: ''}), async () => native(4)]);
  await early.run();
  assert.equal(early.outputs[0].terminal, false);
  assert.equal(early.outputs[0].events[0].status, 'failed');
  const acceptedOffset = early.state.cursor.offset;
  appendFileSync(early.state.journal, event({event: 'run-result', status: 'failed'}));
  await early.run();
  assert.equal(early.outputs[1].events.length, 1);
  assert.equal(early.outputs[1].events[0].event, 'run-result');
  assert.ok(early.state.cursor.offset > acceptedOffset);

  // A deferred poll models the same live cell across outer yields: no new text or handle.
  let resumeFirst, resumeSecond;
  const firstPoll = new Promise(resolve => { resumeFirst = resolve; });
  const secondPoll = new Promise(resolve => { resumeSecond = resolve; });
  const waiting = fixture('waiting.jsonl', event({event: 'heartbeat'}),
    [() => firstPoll, () => secondPoll]);
  const cell = waiting.run();
  assert.deepEqual(waiting.calls, ['poll']);
  assert.equal(waiting.outputs.length, 0);
  resumeFirst({session_id: 42, output: ''});
  await cell;
  assert.deepEqual(waiting.calls, ['poll', 'projection']);
  assert.equal(waiting.outputs.length, 0, 'unchanged heartbeat has no model-facing acknowledgement');
  const cursor = structuredClone(waiting.state.cursor);
  const nextCell = waiting.run();
  assert.deepEqual(waiting.calls, ['poll', 'projection', 'poll']);
  assert.deepEqual(waiting.state.cursor, cursor);
  resumeSecond(native(0));
  await nextCell;
  assert.deepEqual(waiting.state.cursor, cursor);
  assert.equal(waiting.outputs.length, 1);

  // Drain capped heartbeat backlog to a failure before the next process wait.
  const backlog = fixture('backlog.jsonl', event({event: 'heartbeat'}).repeat(101)
    + event({event: 'check-result', status: 'failed'}), [async () => ({session_id: 42, output: ''})]);
  await backlog.run();
  assert.deepEqual(backlog.calls, ['poll', 'projection', 'projection']);
  assert.equal(backlog.outputs[0].events[0].status, 'failed');

  const terminalBacklog = fixture('terminal-backlog.jsonl', event({event: 'heartbeat'}).repeat(101)
    + event({event: 'run-result', status: 'passed'}), [async () => native(0)]);
  await terminalBacklog.run();
  assert.deepEqual(terminalBacklog.calls, ['poll', 'save', 'projection', 'projection']);
  assert.equal(terminalBacklog.outputs[0].events[0].event, 'run-result');
  assert.equal(terminalBacklog.outputs[0].terminal, true);
  assert.equal(terminalBacklog.outputs[0].backlog, false);

  const thrown = fixture('throw.jsonl', '', [async () => native(11)]);
  thrown.tools.exec_command = async () => { throw new Error('projection transport unavailable'); };
  await thrown.run();
  assert.equal(thrown.saved.get('observer-native-terminal').exit_code, 11);
  assert.match(thrown.outputs[0].observation_error, /projection transport unavailable/);

  // Literal quotes, newlines and command substitutions must survive shell transport.
  const marker = join(directory, 'unexpected-command');
  const quoted = fixture("quotes '\n$(printf ignored) `printf ignored`.jsonl", '',
    [async () => native(0)]);
  // Embed a real marker path only in the JSON cursor; source strings remain safely quoted.
  quoted.state.cursor = {stream: [1, 2], offset: 0, size: 0, anchor: "'\n$(touch " + marker + ") `touch " + marker + "`"};
  await quoted.run();
  assert.match(quoted.outputs[0].observation_error, /journal replaced/);
  assert.equal(existsSync(marker), false);
  console.log('PASS: native terminal, projection errors, save errors, early failure, cell/session/cursor resume, failure/terminal backlog, shell quoting');
} finally {
  rmSync(directory, {recursive: true, force: true});
}
