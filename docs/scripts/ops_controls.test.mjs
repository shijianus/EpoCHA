import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
const read = (file) => readFileSync(file, 'utf8');

test('production Compose has fixed service identity, required image and fail-closed profile', () => {
  const compose = read('deploy/docker/compose.yml');
  assert.match(compose, /user: "1000:1000"/);
  assert.match(compose, /image:.*EPOCHA_IMAGE:\?/);
  assert.match(compose, /EPOCHA_PRODUCTION_PROFILE:.*EPOCHA_PRODUCTION_PROFILE:-true/);
  assert.doesNotMatch(compose, /epocha:5a/);
  const env = read('deploy/docker/.env.example');
  assert.match(env, /^EPOCHA_PRODUCTION_PROFILE=true$/m);
  assert.match(env, /^EPOCHA_IMAGE=.*@sha256:/m);
});

test('systemd jobs use the service identity and alert without restarting or killing', () => {
  for (const name of ['backup', 'healthcheck']) {
    const service = read(`deploy/systemd/epocha-${name}.service`);
    const timer = read(`deploy/systemd/epocha-${name}.timer`);
    assert.match(service, /^User=1000$/m);
    assert.match(service, /^Group=1000$/m);
    assert.match(service, /^OnFailure=epocha-alert@%n.service$/m);
    assert.match(service, /^NoNewPrivileges=true$/m);
    assert.match(service, /^ExecStart=.*epocha-ops /m);
    assert.doesNotMatch(service, /(?:kill|restart|docker|prune|rm -)/);
    assert.match(timer, new RegExp(`^Unit=epocha-${name}\\.service$`, 'm'));
  }
  assert.match(read('deploy/systemd/epocha-backup.service'), /backup .*--dest/);
  assert.match(read('deploy/systemd/epocha-healthcheck.service'), /check .*--path \/readyz/);
});

for (const name of ['stage4a', 'stage4b', 'stage5a', 'stage5b', 'admin-ui-check']) {
  test(`${name} emits true RC on an early failure without running host tools`, (t) => {
    const dir = mkdtempSync(join(tmpdir(), 'epocha-exit-test-'));
    t.after(() => rmSync(dir, { recursive: true, force: true }));
    const mock = join(dir, 'mock.sh');
    writeFileSync(mock, 'mktemp() { return 73; }; uname() { echo Unsupported; }; docker() { echo UNEXPECTED_MUTATION >&2; return 99; }; export -f mktemp uname docker\n');
    const result = spawnSync('bash', [`docs/scripts/${name}.sh`], { env: { ...process.env, BASH_ENV: mock }, encoding: 'utf8', timeout: 10000 });
    assert.ifError(result.error);
    assert.notEqual(result.status, 0);
    const prefix = name === 'admin-ui-check' ? 'adminui' : name;
    const codes = [...result.stdout.matchAll(new RegExp(`^\\[${prefix}\\] SCRIPT_RC=(\\d+)$`, 'gm'))];
    assert.deepEqual(codes.map((m) => Number(m[1])), [result.status]);
    assert.doesNotMatch(result.stderr, /UNEXPECTED_MUTATION/);
  });
}

test('stage5b namespace rejects conflicts before cleanup, uses captured IDs only', () => {
  const source = read('docs/scripts/stage5b.sh');
  assert.match(source, /EPOCHA_5B_PROJECT/);
  assert.doesNotMatch(source, /PROJECT="epocha5b"|--name epocha5b-|network inspect epocha5b-recipe|\$RECIPE_COMPOSE/);
  assert.doesNotMatch(source, /down -v --remove-orphans/);
  assert.match(source, /cleanup_owned_resources/);
  for (const scenario of ['preflight', 'created', 'foreign-label', 'replaced-name']) {
    const result = spawnSync('bash', ['docs/scripts/stage5b.sh', '--selftest-resource-ownership', scenario], { encoding: 'utf8', timeout: 10000 });
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stderr + result.stdout);
    assert.match(result.stdout, new RegExp(`ownership selftest PASS: ${scenario}`));
    assert.match(result.stdout, /^\[stage5b\] SCRIPT_RC=0$/m);
  }
});

for (const prefix of ['', 'phase4-fixture']) test(`stage5b ${prefix || 'default'} namespace refuses existing container without mutations`, (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'epocha-namespace-test-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const mock = join(dir, 'mock.sh');
  writeFileSync(mock, `uname() { echo Linux; }; go() { echo go1.27.3; }; node() { echo 24; }
docker() { case "$*" in info) return 0;; 'container inspect '*) printf 'CONFLICT=%s\\n' "$3" >&2; return 0;; *) echo UNEXPECTED_MUTATION >&2; return 99;; esac; }
export -f uname go node docker
`);
  const result = spawnSync('bash', ['docs/scripts/stage5b.sh'], { env: { ...process.env, GO: 'go', BASH_ENV: mock, EPOCHA_5B_PROJECT: prefix }, encoding: 'utf8', timeout: 10000 });
  assert.ifError(result.error);
  assert.equal(result.status, 3, result.stdout + result.stderr);
  assert.match(result.stdout, prefix ? /phase4-fixture-second 已存在/ : /epocha5b-[a-z0-9-]+-second 已存在/);
  assert.doesNotMatch(result.stderr, /UNEXPECTED_MUTATION/);
});

test('stage5a custom project refuses existing assets before arming cleanup', (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'epocha-5a-conflict-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const mock = join(dir, 'mock.sh');
  writeFileSync(mock, `ss() { echo 'State Recv-Q Send-Q Local Address:Port Peer Address:Port'; }
docker() { case "$*" in info) return 0;; 'image inspect '*|'container inspect '*|'network inspect '*) return 1;; 'container ls '*) echo foreign-id;; *) echo UNEXPECTED_MUTATION >&2; return 99;; esac; }
make() { echo UNEXPECTED_MUTATION >&2; exit 99; }
export -f ss docker make
`);
  const result = spawnSync('bash', ['docs/scripts/stage5a.sh'], { env: { ...process.env, BASH_ENV: mock, EPOCHA_5A_PROJECT: 'phase4-fixture' }, encoding: 'utf8', timeout: 10000 });
  assert.ifError(result.error);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /已有资源/);
  assert.doesNotMatch(result.stdout + result.stderr, /UNEXPECTED_MUTATION/);
});

test('admin enable checks true and lifecycle consumers use the actual guarded path', () => {
  const script = read('docs/scripts/admin-ui-check.sh');
  assert.match(script, /grep -q '"enabled":true' "\$WORK\/http\/en.json"/);
  assert.match(read('docs/scripts/release-check.mjs'), /docs\/scripts\/browser\/adminui_lifecycle.test.mjs/);
  assert.match(read('Makefile'), /node docs\/scripts\/node-tests.mjs/);
});
