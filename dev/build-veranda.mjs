#!/usr/bin/env node
// Build an unsigned, version-matched desktop candidate without editing source manifests.
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { copyFileSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const repository = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const frontend = join(repository, 'veranda');
const cli = join(frontend, 'node_modules', '@tauri-apps', 'cli', 'tauri.js');
const output = join(repository, '.build', 'veranda');
const semver = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;
export function configuration(version, platform, icons = []) {
  if (!semver.test(version)) throw new Error('version must be stable MAJOR.MINOR.PATCH');
  const targets = { linux: ['deb'], darwin: ['app'], win32: ['nsis'] }[platform];
  if (!targets) throw new Error('build on Linux, macOS, or Windows');
  return { version, bundle: { active: true, targets, ...(icons.length ? { icon: icons } : {}) } };
}
function run(args, env) {
  const result = spawnSync(process.execPath, [cli, ...args], { cwd: frontend, env, stdio: 'inherit' });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`Tauri ${args[0]} failed (${result.status ?? result.signal})`);
}
function files(directory) {
  return readdirSync(directory, { withFileTypes: true }).flatMap(entry => {
    const path = join(directory, entry.name);
    return entry.isDirectory() ? files(path) : entry.isFile() ? [path] : [];
  });
}
export function main(args) {
  let version, check = false, noBundle = false;
  for (let index = 0; index < args.length; index++) {
    if (args[index] === '--version') version = args[++index];
    else if (args[index] === '--check') check = true;
    else if (args[index] === '--no-bundle') noBundle = true;
    else throw new Error('usage: node dev/build-veranda.mjs --version MAJOR.MINOR.PATCH [--check] [--no-bundle]');
  }
  const config = configuration(version, process.platform);
  const packageVersion = JSON.parse(readFileSync(join(frontend, 'package.json'), 'utf8')).version;
  const configVersion = JSON.parse(readFileSync(join(frontend, 'src-tauri', 'tauri.conf.json'), 'utf8')).version;
  const lockVersion = JSON.parse(readFileSync(join(frontend, 'package-lock.json'), 'utf8')).packages[''].version;
  const compatibility = JSON.parse(readFileSync(join(frontend, 'compatibility.json'), 'utf8'));
  const cargoVersion = readFileSync(join(frontend, 'src-tauri', 'Cargo.toml'), 'utf8').match(/^version\s*=\s*"([^"]+)"/m)?.[1];
  if (packageVersion !== lockVersion || packageVersion !== configVersion || packageVersion !== cargoVersion || packageVersion !== compatibility.productVersion) throw new Error('source package, Tauri, Cargo and compatibility versions differ');
  compatibility.productVersion = version;
  const candidateDirectory = join(output, version, `${process.platform}-${process.arch}`);
  const targetDirectory = process.env.CARGO_TARGET_DIR ? resolve(process.env.CARGO_TARGET_DIR) : join(output, 'target');
  const metadata = { product: 'Subyard Veranda', productVersion: version, sourcePackageVersion: packageVersion, platform: process.platform, arch: process.arch, unsigned: true, acceptance: 'pending', configuration: structuredClone(config), compatibility };
  if (check) { process.stdout.write(JSON.stringify(metadata, null, 2) + '\n'); return; }
  mkdirSync(candidateDirectory, { recursive: true });
  writeFileSync(join(candidateDirectory, 'compatibility.json'), JSON.stringify(compatibility, null, 2) + '\n');
  const env = { ...process.env, VERANDA_PRODUCT_VERSION: version, CARGO_TARGET_DIR: targetDirectory };
  const iconsDirectory = join(candidateDirectory, 'icons');
  run(['icon', join(frontend, 'src-tauri', 'icons', 'icon.png'), '--output', iconsDirectory], env);
  config.bundle.icon = ['32x32.png', '128x128.png', '128x128@2x.png', 'icon.icns', 'icon.ico'].map(name => join(iconsDirectory, name));
  run(['build', '--ci', '--no-sign', '--config', JSON.stringify(config), ...(noBundle ? ['--no-bundle'] : []), '--', '--locked'], env);
  const binary = join(targetDirectory, 'release', process.platform === 'win32' ? 'subyard-veranda.exe' : 'subyard-veranda');
  const bundle = join(targetDirectory, 'release', 'bundle');
  const packageDirectory = join(candidateDirectory, 'packages');
  mkdirSync(packageDirectory, {recursive: true});
  let artifacts;
  if (process.platform === 'darwin' && !noBundle) {
    const app = join(bundle, 'macos', 'Subyard Veranda.app');
    const archive = join(packageDirectory, `subyard-veranda_${version}_darwin-${process.arch}.tar.gz`);
    const result = spawnSync('tar', ['-czf', archive, '-C', dirname(app), 'Subyard Veranda.app'], {stdio: 'inherit'});
    if (result.error || result.status !== 0) throw new Error('could not archive the macOS application');
    artifacts = [archive];
  } else {
    const built = noBundle ? [binary] : files(bundle).filter(path => /\.(deb|exe)$/.test(path) && basename(path).includes(`_${version}_`));
    artifacts = built.map(path => { const destination = join(packageDirectory, noBundle ? `subyard-veranda_${version}_${process.platform}-${process.arch}${process.platform === 'win32' ? '.exe' : ''}` : basename(path)); copyFileSync(path, destination); return destination; });
  }
  if (!artifacts.length || !statSync(binary).isFile()) throw new Error('desktop build produced no artifacts');
  metadata.artifacts = artifacts.map(path => ({ path: path.slice(candidateDirectory.length + 1).replaceAll('\\', '/'), sha256: createHash('sha256').update(readFileSync(path)).digest('hex') }));
  const manifest = join(candidateDirectory, 'candidate.json');
  writeFileSync(manifest, JSON.stringify(metadata, null, 2) + '\n');
  process.stdout.write(`Unsigned Veranda ${version} candidate: ${manifest}\n`);
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { main(process.argv.slice(2)); } catch (error) { process.stderr.write(`build-veranda: ${error.message}\n`); process.exitCode = 1; }
}
