import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { cpSync, copyFileSync, existsSync, mkdtempSync, openSync, closeSync, readSync, readFileSync, rmSync, writeFileSync, writeSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const TOOL = join(ROOT, 'docs/scripts/build-receipt.mjs');
const GO = process.env.RECEIPT_GO || 'go';
const env = { ...process.env, GOTOOLCHAIN: 'local', GOPROXY: 'off', GOSUMDB: 'off', GOWORK: 'off', GOENV: 'off', GOFLAGS: '', CGO_ENABLED: '0', GOOS: 'linux', GOARCH: 'arm64' };
const version = 'receipt-integration';
const date = '2026-11-02T01:02:03Z';
const invoke = (...args) => spawnSync(process.execPath, [TOOL, ...args], { cwd: ROOT, env, encoding: 'utf8', timeout: 180000, maxBuffer: 8 * 1024 * 1024 });
const ok = (result) => assert.equal(result.status, 0, result.stderr || result.error?.message || result.stdout);
const hashFile = (path) => {
  const hash = createHash('sha256');
  hash.update(readFileSync(path));
  return hash.digest('hex');
};

test('real offline linux/arm64 builds produce immutable verifiable original-byte receipts; never a runtime PASS', { timeout: 550000 }, async (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'epocha-receipt-build-'));
  // Only remove this test's own directory, never a previous build or the repository .prebuilt tree.
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const commitResult = spawnSync('git', ['rev-parse', 'HEAD'], { cwd: ROOT, encoding: 'utf8' });
  ok(commitResult);
  const commit = commitResult.stdout.trim();
  const ldflags = `-s -w -X github.com/shijianus/EpoCHA/internal/version.Version=${version} -X github.com/shijianus/EpoCHA/internal/version.Commit=${commit} -X github.com/shijianus/EpoCHA/internal/version.BuildDate=${date}`;
  // This real compile runs even in the initial red test, so a missing helper cannot disguise a broken/offline toolchain.
  const original = join(dir, 'original-epocha');
  const compiled = spawnSync(GO, ['build', '-trimpath', '-buildvcs=false', '-ldflags', ldflags, '-o', original, './cmd/server'], { cwd: ROOT, env, encoding: 'utf8', timeout: 180000, maxBuffer: 8 * 1024 * 1024 });
  ok(compiled);
  assert.ok(readFileSync(original).length > 0);
  t.diagnostic('actual offline Go compile completed before receipt checks; linux/arm64 binary was not executed');
  const out = join(dir, 'recorded');
  const args = ['build', '--root', ROOT, '--out', out, '--go', GO, '--version', version, '--commit', commit, '--build-date', date, '--goos', 'linux', '--goarch', 'arm64'];
  ok(invoke(...args));
  const manifestBytes = readFileSync(join(out, 'manifest.json'));
  const manifest = JSON.parse(manifestBytes);
  assert.equal(manifest.inputs.commit, commit);
  assert.equal(manifest.inputs.build_date, date);
  assert.equal(manifest.inputs.cgo_enabled, '0');
  assert.equal(manifest.inputs.goos, 'linux');
  assert.equal(manifest.inputs.goarch, 'arm64');
  assert.equal(manifest.toolchain, 'go1.27.3');
  assert.equal(manifest.runtime.status, 'NOT_RUN');
  assert.equal(manifest.artifacts.length, 2);
  assert.ok(manifest.artifacts.every((item) => item.build.command.includes(ldflags)));
  assert.equal(hashFile(join(out, 'out/epocha')), hashFile(original));
  ok(invoke('verify', '--out', out, '--go', GO));
  const originalOpsHash = hashFile(join(out, 'out/epocha-ops'));
  assert.notEqual(invoke(...args).status, 0, 'an existing output directory must not be overwritten');
  assert.deepEqual(readFileSync(join(out, 'manifest.json')), manifestBytes);
  assert.equal(hashFile(join(out, 'out/epocha-ops')), originalOpsHash);

  const archive = join(dir, 'independent-archive');
  cpSync(out, archive, { recursive: true });
  ok(invoke('verify', '--out', archive, '--go', GO));
  const corruptPath = join(archive, 'out/epocha');
  const beforeHash = hashFile(corruptPath);
  const corrupt = Buffer.allocUnsafe(1);
  const reader = openSync(corruptPath, 'r');
  // A one-byte mutation avoids another full-size Buffer allocation.
  readSync(reader, corrupt, 0, 1, 1);
  closeSync(reader);
  const handle = openSync(corruptPath, 'r+');
  corrupt[0] ^= 1;
  writeSync(handle, corrupt, 0, 1, 1);
  closeSync(handle);
  assert.notEqual(invoke('verify', '--out', archive, '--go', GO).status, 0, 'changed original bytes must fail');
  copyFileSync(join(out, 'out/epocha'), corruptPath);
  assert.equal(hashFile(corruptPath), beforeHash);
  for (const field of ['version', 'commit', 'build_date', 'goos', 'goarch', 'cgo_enabled']) {
    const changed = structuredClone(manifest);
    changed.inputs[field] = 'changed';
    writeFileSync(join(archive, 'manifest.json'), JSON.stringify(changed));
    assert.notEqual(invoke('verify', '--out', archive, '--go', GO).status, 0, `tampered ${field} must fail`);
  }
  writeFileSync(join(archive, 'manifest.json'), manifestBytes);
  writeFileSync(join(archive, 'unexpected.db'), 'must not be archived');
  assert.notEqual(invoke('verify', '--out', archive, '--go', GO).status, 0, 'unexpected data/credentials in an archive must fail');
  assert.equal(existsSync(join(out, 'image.json')), false, 'a cross-build cannot claim image or runtime evidence');
  assert.equal(dirname(join(out, 'out/epocha')), join(out, 'out'));
});
