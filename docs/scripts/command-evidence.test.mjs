import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';

test('command receipts preserve separate streams, UTC, raw failure and spawn failure', async (t) => {
  const { createCommandRecorder } = await import('./command-evidence.mjs');
  const dir = mkdtempSync(join(tmpdir(), 'epocha-command-evidence-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const recorder = createCommandRecorder(dir);
  const result = recorder.run(process.execPath, ['-e', 'process.stdout.write("OUT");process.stderr.write("ERR");process.exitCode=7'], { encoding: 'utf8' });
  assert.equal(result.status, 7);
  recorder.run('epocha-no-such-executable-382912', [], { encoding: 'utf8' });
  assert.equal(recorder.commands.length, 2);
  for (const receipt of recorder.commands) {
    assert.match(receipt.started_at, /Z$/);
    assert.match(receipt.finished_at, /Z$/);
    assert.ok(Date.parse(receipt.finished_at) >= Date.parse(receipt.started_at));
    assert.ok(receipt.duration_ms >= 0);
    assert.notEqual(receipt.stdout, receipt.stderr);
    for (const key of ['stdout', 'stderr']) assert.match(receipt[`${key}_sha256`], /^[a-f0-9]{64}$/);
  }
  const first = recorder.commands[0];
  assert.equal(first.raw_exit_code, 7);
  assert.equal(readFileSync(join(dir, first.stdout), 'utf8'), 'OUT');
  assert.equal(readFileSync(join(dir, first.stderr), 'utf8'), 'ERR');
  assert.equal(recorder.commands[1].raw_exit_code, null);
  assert.match(recorder.commands[1].error, /ENOENT/);
});

test('Node entrypoint rejects nonexistent files even alongside a valid passing test', () => {
  const result = spawnSync(process.execPath, ['docs/scripts/node-tests.mjs', '--test-reporter=tap', 'docs/scripts/browser/pow_vectors.test.mjs', 'docs/scripts/missing-phase4.test.mjs'], { encoding: 'utf8' });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /missing test file:.*missing-phase4/);
  assert.doesNotMatch(result.stdout, /# pass/);
});

test('release CLI emits its actual SCRIPT_RC on usage error and success', () => {
  for (const [args, rc] of [[['--invalid'], 1], [['--widget-size'], 0]]) {
    const result = spawnSync(process.execPath, ['docs/scripts/release-check.mjs', ...args], { encoding: 'utf8' });
    assert.equal(result.status, rc);
    assert.match(result.stdout, new RegExp(`^\\[release-check\\] SCRIPT_RC=${rc}$`, 'm'));
  }
});
