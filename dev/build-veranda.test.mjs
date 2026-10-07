import assert from 'node:assert/strict';
import { test } from 'node:test';
import { configuration } from './build-veranda.mjs';

test('stamps one exact stable product version across native candidate configurations', () => {
  for (const [platform, target] of [['linux', 'deb'], ['darwin', 'app'], ['win32', 'nsis']]) {
    const config = configuration('3.4.5', platform);
    assert.equal(config.version, '3.4.5');
    assert.deepEqual(config.bundle.targets, [target]);
    assert.equal(config.bundle.active, true);
  }
  for (const invalid of [undefined, 'v1.2.3', '1.2.3-dev', '1.2', '01.2.3', '1.2.3;echo']) {
    assert.throws(() => configuration(invalid, 'linux'), /stable/);
  }
});

import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
const root = fileURLToPath(new URL('../', import.meta.url));

test('checks a release override without rewriting any source version', () => {
  const manifests = ['veranda/package.json', 'veranda/package-lock.json', 'veranda/src-tauri/Cargo.toml', 'veranda/src-tauri/tauri.conf.json', 'veranda/compatibility.json'];
  const before = manifests.map(path => readFileSync(new URL('../' + path, import.meta.url)));
  const result = spawnSync(process.execPath, ['dev/build-veranda.mjs', '--version', '7.8.9', '--check'], {cwd: root, encoding: 'utf8'});
  assert.equal(result.status, 0, result.stderr);
  const candidate = JSON.parse(result.stdout);
  assert.equal(candidate.productVersion, '7.8.9');
  assert.equal(candidate.configuration.version, '7.8.9');
  assert.equal(candidate.compatibility.productVersion, '7.8.9');
  assert.equal(candidate.compatibility.productVersionPolicy, 'exact-match');
  assert.deepEqual(candidate.compatibility.rpc, {minimum: 1, maximum: 1});
  assert.deepEqual(candidate.compatibility.requiredCapabilities, ['owner-inventory-v1', 'ordered-events']);
  assert.deepEqual(candidate.compatibility.featureCapabilities.operations, ['operation-exact-plan-v1', 'operation-steps-v1']);
  assert.equal(candidate.unsigned, true);
  assert.equal(candidate.acceptance, 'pending');
  manifests.forEach((path, index) => assert.deepEqual(readFileSync(new URL('../' + path, import.meta.url)), before[index]));
});
