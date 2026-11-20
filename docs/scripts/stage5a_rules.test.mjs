import test from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { delimiter, join } from 'node:path';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { cleanupOwnedResources, registerOwnedResource } from './stage5a-resources.mjs';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const SCRIPT = join(ROOT, 'docs/scripts/stage5a.sh');

function runRules() {
  return spawnSync('bash', [SCRIPT, '--selftest-rules'], { cwd: ROOT, encoding: 'utf8', timeout: 30_000, maxBuffer: 2 * 1024 * 1024 });
}
function occurrence(text, pattern) {
  const flags = pattern.flags.includes('g') ? pattern.flags : `${pattern.flags}g`;
  const matches = [...text.matchAll(new RegExp(pattern.source, flags))];
  assert.equal(matches.length, 1, `expected one occurrence of ${pattern}, got ${matches.length}`);
  return matches[0].index;
}

function assertF08Contract(text) {
  const create = text.indexOf('admin_json POST /api/admin/admin-keys');
  const prove = text.indexOf('BACKUP_OLD_OK=1');
  const backup = text.indexOf('cexec epocha "$OPS" backup --db');
  assert.ok(create >= 0 && prove >= 0 && backup >= 0 && create < prove && prove < backup, 'dedicated admin key must be created, then proven by container whoami, before backup');
  const recovery = text.indexOf('"$BIN/epocha-ops" admin-recover');
  assert.ok(recovery >= 0 && create > recovery, 'dedicated backup key must be created after emergency recovery revokes all existing keys');
  assert.match(text, /BACKUP_OLD_OK=1/);
  assert.match(text, /BACKUP_OLD_OK -ne 1/);
  const oldRestore = text.indexOf('EPOCHA_ADMIN_KEY="$BACKUP_ADMIN"');
  const newRestore = text.indexOf('EPOCHA_ADMIN_KEY_FILE="$C_KEYS/admin"');
  assert.ok(oldRestore >= 0 && newRestore >= 0, 'restore must verify old key rejection and new key acceptance through container-only ops');
  assert.match(text, /OLD_ADMIN_FIRST=.*sed -n '1p'/);
  assert.match(text, /NEW_ADMIN_FIRST=.*sed -n '1p'/);
  assert.match(text, /OLD_ADMIN_FIRST.*status=401/);
  assert.match(text, /NEW_ADMIN_FIRST.*status=200/);
}

test('stage5a selftest exercises exact F08/F13 evidence rules with bash fixtures', () => {
  const result = runRules();
  assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
  assert.match(result.stdout, /rules selftest PASS/);
});

test('F08 source contract binds dedicated pre-backup key proof and exact restore statuses', () => {
  const text = readFileSync(SCRIPT, 'utf8');
  assert.doesNotThrow(() => assertF08Contract(text));
  for (const mutation of [
    text.replace(/admin_json POST \/api\/admin\/admin-keys[^\n]*backup-admin/, 'admin_json GET \/api\/admin\/whoami'),
    text.replace(/-e EPOCHA_ADMIN_KEY_FILE=/g, '-e EPOCHA_ADMIN_KEY_FILE=/etc/epocha/keys/admin'),
    text.replace(/OLD_ADMIN_FIRST.*\n/, ''),
  ]) assert.throws(() => assertF08Contract(mutation));
});

test('F13 source contract requires exact 150-flow plan, wall summary and four 150-sample stages without err field', () => {
  const text = readFileSync(SCRIPT, 'utf8');
  assert.match(text, /check_load_evidence/);
  assert.match(text, /concurrency=4 requests=150/);
  assert.match(text, /flows=150 ok=150/);
  for (const stage of ['challenge', 'redeem', 'siteverify', 'pow-tool']) assert.match(text, new RegExp(`${stage}.*n=150`));
  assert.match(text, /err=/);
  assert.match(text, /失败类别/);
});

test('stage5a fake Docker resource ownership preserves pre-existing and uncreated assets', () => {
  const calls = [];
  const fakeDocker = (_command, args) => {
    calls.push(args);
    return { status: 0, stdout: '', stderr: '' };
  };
  const empty = { armed: false, containers: [], volumes: [], networks: [], pulledImages: [] };
  assert.deepEqual(cleanupOwnedResources(empty, 'docker', fakeDocker), { status: 0, calls: [] });
  assert.deepEqual(calls, [], 'preflight failure must not invoke Docker cleanup');

  const owned = { armed: true, containers: [], volumes: [], networks: [], pulledImages: [] };
  for (const [kind, id] of [['containers', 'c-own'], ['volumes', 'v-own'], ['networks', 'n-own'], ['pulledImages', 'sha256:image-own']]) {
    registerOwnedResource(owned, kind, id);
    registerOwnedResource(owned, kind, id);
  }
  assert.throws(() => registerOwnedResource(owned, 'containers', ''));
  assert.throws(() => registerOwnedResource(owned, 'unknown', 'id'));
  const created = cleanupOwnedResources(owned, 'docker', fakeDocker);
  assert.equal(created.status, 0);
  assert.deepEqual(created.calls, [
    ['rm', '-f', 'c-own'], ['volume', 'rm', 'v-own'], ['network', 'rm', 'n-own'], ['image', 'rm', 'sha256:image-own'],
  ]);
  assert.deepEqual(calls, created.calls);
});


test('occupied-port preflight never invokes Docker cleanup or touches same-named assets', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'epocha-stage5a-fake-docker-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const bin = join(dir, 'bin');
  const record = join(dir, 'docker.log');
  const script = (name, body) => {
    const path = join(bin, name);
    writeFileSync(path, `#!/usr/bin/env bash\n${body}\n`);
    chmodSync(path, 0o700);
  };
  mkdirSync(bin);
  script('docker', `printf '%s\\n' "$*" >>"$EPOCHA_FAKE_DOCKER_LOG"\n[[ "$1 $2" == "info " ]]`);
  script('ss', 'printf "State Recv-Q Send-Q Local Address:Port Peer Address:Port\\nLISTEN 0 128 127.0.0.1:18080 0.0.0.0:*\\n"');
  for (const name of ['curl', 'jq', 'openssl']) script(name, 'exit 0');

  const result = spawnSync('bash', [SCRIPT], {
    cwd: ROOT,
    encoding: 'utf8',
    timeout: 30_000,
    env: {
      ...process.env,
      PATH: `${bin}${delimiter}${process.env.PATH}`,
      EPOCHA_FAKE_DOCKER_LOG: record,
      EPOCHA_5A_PORT: '18080',
    },
  });
  assert.equal(result.status, 1, `${result.stdout}\n${result.stderr}`);
  const calls = readFileSync(record, 'utf8').trim().split(/\r?\n/);
  assert.deepEqual(calls, ['info'], 'preflight must not run compose down, rm, network rm or image rmi');
  assert.match(result.stderr, /端口 18080 已被占用/);
});
