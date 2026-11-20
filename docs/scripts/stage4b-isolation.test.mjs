import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { classifyIsolationRun, runIsolationProbe } from './stage4b-isolation.mjs';

const pass = (stdout) => ({ status: 0, stdout, stderr: '' });
const goodOutput = 'ISOLATION_CHECKPOINT=mounts-verified\nNETWORK_CHECKPOINT=dns-blocked\n';

test('isolation classifier rejects failed startup, empty output, missing entrypoint and each failed checkpoint', () => {
  for (const result of [
    { status: 125, stdout: '', stderr: 'container runtime error' },
    { status: 0, stdout: '', stderr: '' },
    { status: 127, stdout: '', stderr: '/bin/sh: not found' },
    pass('ISOLATION_CHECKPOINT=mounts-verified\n'),
    pass('NETWORK_CHECKPOINT=dns-blocked\n'),
    pass('GROUNDTRUTH_VISIBLE=1\n' + goodOutput),
    pass('ANSWER_VISIBLE=1\n' + goodOutput),
    pass('DNS_REACHABLE\n' + goodOutput),
  ]) assert.equal(classifyIsolationRun(result).status, 'FAIL');
  assert.equal(classifyIsolationRun(pass(goodOutput)).status, 'PASS');
});

test('isolation runner preserves docker exit code and marks missing tools as failure', (t) => {
  const root = mkdtempSync(join(tmpdir(), 'epocha-stage4b-isolation-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const stage = join(root, 'stage');
  mkdirSync(stage);
  writeFileSync(join(stage, 'one.png'), 'fixture');
  const fake = join(root, 'docker-fail');
  writeFileSync(fake, '#!/bin/sh\nprintf "docker exit 125\\n" >&2\nexit 125\n', { mode: 0o700 });
  const result = runIsolationProbe({ image: 'example/image', stage, docker: fake, run: () => ({ status: 125, stdout: '', stderr: 'docker exit 125' }) });
  assert.equal(result.status, 'FAIL');
  assert.equal(result.raw_exit_code, 125);
  const missing = runIsolationProbe({ image: 'example/image', stage, docker: join(root, 'missing-docker'), run: () => ({ error: new Error('missing docker'), status: null }) });
  assert.equal(missing.status, 'FAIL');
  assert.equal(missing.raw_exit_code, null);
});
