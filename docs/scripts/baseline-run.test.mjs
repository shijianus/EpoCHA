import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, mkdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const script = fileURLToPath(new URL('./baseline-run.mjs', import.meta.url));

test('baseline command runner records actual exit codes and preserves every run', (t) => {
  const root = mkdtempSync(join(tmpdir(), 'epocha-receipt-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const repo = join(root, 'repo');
  mkdirSync(repo);
  for (const args of [['init', '--quiet'], ['-c', 'user.name=fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '--allow-empty', '--quiet', '-m', 'fixture']]) {
    const git = spawnSync('git', args, { cwd: repo, encoding: 'utf8' });
    assert.equal(git.status, 0, git.stderr);
  }
  const output = join(root, 'receipts');
  const env = { ...process.env, SQLITE_TMPDIR: root };
  const command = [script, output, 'actual-failure', process.execPath, '-e', 'console.log("real output");process.exit(7)'];
  const run = spawnSync(process.execPath, command, { cwd: repo, env, encoding: 'utf8' });
  assert.equal(run.status, 7, run.stderr);
  const receipt = JSON.parse(readFileSync(join(output, 'actual-failure.receipt.json'), 'utf8'));
  assert.equal(receipt.exit_code, 7);
  assert.equal(receipt.status, 'FAIL');
  assert.match(receipt.commit, /^[a-f0-9]{40}$/);
  assert.match(readFileSync(join(output, 'actual-failure.log'), 'utf8'), /real output/);
  const repeat = spawnSync(process.execPath, command, { cwd: repo, env, encoding: 'utf8' });
  assert.notEqual(repeat.status, 0);
  assert.equal(readFileSync(join(output, 'actual-failure.receipt.json'), 'utf8'), JSON.stringify(receipt, null, 2) + '\n');
});
