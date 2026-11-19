import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { buildReport } from './baseline-evidence.mjs';

function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'epocha-baseline-report-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  writeFileSync(join(dir, 'test.log'), 'test output\n');
  const item = { test: 'unit', commit: 'a'.repeat(40), environment: { os: 'linux', arch: 'x64', node: 'v24' }, command: ['go', 'test', './...'], exit_code: 0, status: 'PASS', log: 'test.log', defects: ['A'], fixes: ['b'.repeat(40)] };
  return { dir, item };
}

test('baseline evidence binds commands and raw logs to exact candidate and environment', (t) => {
  const { dir, item } = fixture(t);
  const report = buildReport(dir, [item]);
  assert.equal(report.release_approved, false);
  assert.equal(report.tests[0].log_sha256, createHash('sha256').update('test output\n').digest('hex'));
  assert.deepEqual(report.tests[0].command, item.command);
  assert.equal(report.tests[0].commit, item.commit);
});

test('baseline evidence fails closed on empty, missing or contradictory evidence', (t) => {
  const { dir, item } = fixture(t);
  for (const tests of [[], [{ ...item, commit: 'HEAD' }], [{ ...item, log: '../other.log' }], [{ ...item, log: 'missing.log' }], [{ ...item, exit_code: 3 }], [{ ...item, command: [] }], [{ ...item, environment: {} }]]) {
    assert.throws(() => buildReport(dir, tests));
  }
});

test('BLOCKED and NOT_RUN preserve absent execution instead of manufacturing an exit code', (t) => {
  const { dir, item } = fixture(t);
  for (const status of ['BLOCKED', 'NOT_RUN']) {
    const report = buildReport(dir, [{ ...item, status, exit_code: null, log: null, reason: 'formal arm64 environment unavailable' }]);
    assert.equal(report.tests[0].exit_code, null);
    assert.equal(report.tests[0].log_sha256, null);
    assert.equal(report.release_approved, false);
  }
});
