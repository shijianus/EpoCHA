import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const PREFLIGHT = join(ROOT, 'deploy/preflight.sh').replaceAll('\\', '/');

// A fully isolated supported-host fixture: all Docker calls are read-only mocks.
const DEPLOY_HARNESS = String.raw`
id() { case "$1" in -u|-g) echo 1000;; -un) echo fixture;; esac; }
uname() { case "$1" in -m) echo aarch64;; *) echo Linux;; esac; }
awk() {
  case "$*" in
    *MemTotal*|*MemAvailable*) echo 2000000;; *SwapTotal*) echo 524288;;
    *) builtin command awk "$@";;
  esac
}
cat() { if [[ "$1" == /proc/sys/vm/swappiness ]]; then echo 10; else builtin command cat "$@"; fi; }
stat() { case "$2" in '%u:%g') echo 1000:1000;; '%u') echo 1000;; '%a') echo 600;; *) builtin command stat "$@";; esac; }
nginx() { echo fixture; }
systemctl() { echo active; }
ss() { return 0; }
command() { if [[ "$1" == -v && "$2" == caddy ]]; then return 1; fi; builtin command "$@"; }
docker() {
  printf '%s\n' "$*" >>"$PF_CALLS"
  case "$*" in
    '--version') echo 'Docker version 29.8.2';;
    'info') return 0;;
    'info --format '*) echo fixture;;
    'compose version --short') printf '%s\n' "$PF_VERSION";;
    'ps -q') echo fixture-id;;
    'compose '*config*) [[ "$PF_BAD_CONFIG" != 1 ]] || return 9; cat "$PF_CONFIG";;
    'container inspect '*) [[ "$PF_NO_CONTAINER" != 1 ]] || return 1; cat "$PF_CONTAINER";;
    'image inspect '*) echo sha256:fixture-image;;
    *) echo UNEXPECTED_DOCKER_CALL >&2; return 99;;
  esac
}
script="$1"; shift
source "$script" "$@"
`;

function deploymentFixture(t, { version = 'v5.6.0', strict = true, image, user, production, container = {}, noContainer = false, missingCompose = false, missingEnv = false, badConfig = false } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'epocha-deployment-preflight-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  for (const name of ['data', 'keys', 'backup']) mkdirSync(join(dir, name));
  for (const name of ['master', 'pepper']) writeFileSync(join(dir, 'keys', name), 'NOT-A-REAL-KEY', { mode: 0o600 });
  const digest = `example/epocha@sha256:${'a'.repeat(64)}`;
  const service = { image: image ?? digest, user: user ?? '1000:1000', environment: { EPOCHA_PRODUCTION_PROFILE: production ?? 'true' } };
  writeFileSync(join(dir, 'config.json'), JSON.stringify({ name: 'fixture', services: { epocha: service } }));
  writeFileSync(join(dir, 'container.json'), JSON.stringify({ image: digest, image_id: 'sha256:fixture-image', user: '1000:1000', production: 'true', running: true, project: 'fixture', service: 'epocha', ...container }));
  writeFileSync(join(dir, 'compose.yml'), 'services: {}\n');
  writeFileSync(join(dir, '.env'), '# fixture\n');
  const args = [PREFLIGHT, '--json', '--data-dir', join(dir, 'data'), '--keys-dir', join(dir, 'keys'), '--backup-dir', join(dir, 'backup'), '--compose-file', join(dir, missingCompose ? 'missing.yml' : 'compose.yml'), '--env-file', join(dir, missingEnv ? 'missing.env' : '.env'), '--require-container', 'fixture-container'];
  if (strict) args.push('--strict');
  const result = spawnSync('bash', ['--noprofile', '--norc', '-c', DEPLOY_HARNESS, 'deployment-preflight', ...args], {
    cwd: ROOT, encoding: 'utf8', timeout: 15000,
    env: { ...process.env, BASH_ENV: '', ENV: '', PF_VERSION: version, PF_CONFIG: join(dir, 'config.json'), PF_CONTAINER: join(dir, 'container.json'), PF_CALLS: join(dir, 'calls'), PF_NO_CONTAINER: noContainer ? '1' : '', PF_BAD_CONFIG: badConfig ? '1' : '' },
  });
  assert.ifError(result.error);
  assert.doesNotMatch(result.stderr, /UNEXPECTED_DOCKER_CALL/);
  assert.doesNotMatch(result.stdout + result.stderr, /NOT-A-REAL-KEY/);
  assert.ok([0, 3].includes(result.status), result.stderr);
  return { ...result, report: JSON.parse(result.stdout) };
}

for (const version of ['v2.0.0', '2.39.1', 'v5.6.0']) test(`strict deployment accepts Compose ${version}`, (t) => {
  const { status, report } = deploymentFixture(t, { version });
  assert.equal(status, 0, JSON.stringify(report.strict_failures));
  assert.equal(report.compose_supported, true);
  assert.deepEqual(report.strict_failures, []);
  assert.equal(report.deployment_valid, true);
});
for (const version of ['', 'v1.29.2', 'garbage']) test(`strict deployment rejects Compose ${JSON.stringify(version)}`, (t) => {
  const { status, report } = deploymentFixture(t, { version });
  assert.equal(status, 3);
  assert.equal(report.compose_supported, false);
  assert.ok(report.strict_failures.some((s) => s.includes('Compose')));
});
for (const [name, options] of [
  ['mutable image', { image: 'epocha:latest' }], ['wrong UID', { user: '65534:65534' }],
  ['missing production', { production: '' }], ['false production', { production: 'false' }],
  ['missing container', { noContainer: true }], ['container image', { container: { image: 'epocha:old' } }],
  ['container image ID', { container: { image_id: 'sha256:other' } }],
  ['container user', { container: { user: '65534:65534' } }], ['container production', { container: { production: 'false' } }],
  ['container stopped', { container: { running: false } }], ['container project', { container: { project: 'other' } }],
  ['container service', { container: { service: 'other' } }],
  ['missing compose', { missingCompose: true }], ['missing env', { missingEnv: true }], ['invalid config', { badConfig: true }],
]) test(`strict deployment refuses ${name}`, (t) => {
  const { status, report } = deploymentFixture(t, options);
  assert.equal(status, 3);
  assert.equal(report.deployment_valid, false);
  assert.ok(report.strict_failures.some((s) => s.includes('[deployment]')));
});
test('ordinary preflight reports bad deployment without claiming strict approval', (t) => {
  const { status, report } = deploymentFixture(t, { strict: false, image: 'epocha:latest' });
  assert.equal(status, 0);
  assert.equal(report.deployment_valid, false);
  assert.deepEqual(report.strict_failures, []);
  assert.ok(report.warnings.some((s) => s.includes('[deployment]')));
});

// Source the actual script, but never contact Docker, a web server, or a remote host.
const HARNESS = String.raw`
unset SQLITE_TMPDIR TMPDIR
command() {
  if [[ "$1" == -v ]]; then
    case "$2" in
      ss|netstat) [[ "$2" == "$PF_TOOL" ]]; return ;;
      docker|nginx|caddy|systemctl) return 1 ;;
    esac
  fi
  builtin command "$@"
}
docker() { printf 'UNEXPECTED_DOCKER_CALL\n' >&2; return 99; }
nginx() { printf 'UNEXPECTED_NGINX_CALL\n' >&2; return 99; }
caddy() { printf 'UNEXPECTED_CADDY_CALL\n' >&2; return 99; }
systemctl() { printf 'UNEXPECTED_SYSTEMCTL_CALL\n' >&2; return 99; }
ss() {
  [[ "$PF_MODE" != fail ]] || return 42
  if [[ " $* " != *' -H '* ]]; then
    printf 'State Recv-Q Send-Q Local Address:Port Peer Address:Port\n'
  fi
  if [[ "$PF_MODE" == occupied ]]; then
    printf 'LISTEN 0 128 127.0.0.1:8080 0.0.0.0:*\n'
  fi
}
netstat() {
  printf 'Active Internet connections (only servers)\nProto Recv-Q Send-Q Local Address Foreign Address State\n'
  [[ "$PF_MODE" != fail ]] || return 42
  if [[ "$PF_MODE" == occupied ]]; then
    printf 'tcp 0 0 127.0.0.1:8080 0.0.0.0:* LISTEN\n'
  fi
}
id() {
  case "$1" in -u|-g) printf '1000\n' ;; -un) printf 'fixture\n' ;; *) return 2 ;; esac
}
stat() {
  [[ "\${PF_SCAN_KEYS:-}" == 1 ]] && { builtin command stat "$@"; return; }
  case "$2" in
    '%u:%g') printf '424242:424242\n' ;;
    '%u') printf '424242\n' ;;
    '%a') printf '700\n' ;;
    *) return 2 ;;
  esac
}
findmnt() {
  case "$*" in *FSTYPE) printf 'fixture-fs\n' ;; *TARGET) printf '/fixture\n' ;; *) return 2 ;; esac
}
df() {
  printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\nfixture 100000 1000 99000 1%% /\n'
}
find() { builtin command find "$@"; }
if [[ "$PF_DIRS" == readonly ]]; then
  locked_dir=''
  for d in /proc/sys /sys /proc; do
    if [[ -d "$d" && ! -w "$d" ]]; then locked_dir="$d"; break; fi
  done
  [[ -n "$locked_dir" ]] || { printf 'BLOCKED: no readonly pseudo-directory fixture\n' >&2; exit 125; }
  PF_DATA="$locked_dir"; PF_KEYS="$locked_dir"; PF_BACKUP="$locked_dir"
fi
if [[ "$PF_STRICT" == 1 ]]; then
  source "$1" --data-dir "$PF_DATA" --keys-dir "$PF_KEYS" --backup-dir "$PF_BACKUP" --ports 8080 --"$PF_FORMAT" --strict
else
  source "$1" --data-dir "$PF_DATA" --keys-dir "$PF_KEYS" --backup-dir "$PF_BACKUP" --ports 8080 --"$PF_FORMAT"
fi
`;

function runPreflight(t, { tool = 'ss', mode = 'free', dirs = 'writable', format = 'json', strict = false, expectedStatus = 0, scanKeys = false } = {}) {
  const temp = mkdtempSync(join(tmpdir(), 'epocha-preflight-regression-'));
  t.after(() => rmSync(temp, { recursive: true, force: true })); // Only this test's own new fixture.
  for (const name of ['data', 'keys', 'backups']) mkdirSync(join(temp, name));
  if (scanKeys) {
    writeFileSync(join(temp, 'keys', 'master'), 'master-fixture');
    writeFileSync(join(temp, 'keys', 'pepper'), 'pepper-fixture');
    symlinkSync('master', join(temp, 'keys', 'key-link'));
  }
  const env = {
    ...process.env,
    PF_TOOL: tool, PF_MODE: mode, PF_DIRS: dirs, PF_FORMAT: format, PF_STRICT: strict ? '1' : '', PF_SCAN_KEYS: scanKeys ? '1' : '',
    SQLITE_TMPDIR: '', TMPDIR: '',
    PF_DATA: join(temp, 'data').replaceAll('\\', '/'),
    PF_KEYS: join(temp, 'keys').replaceAll('\\', '/'),
    PF_BACKUP: join(temp, 'backups').replaceAll('\\', '/'),
  };
  delete env.BASH_ENV;
  delete env.ENV;
  const result = spawnSync('bash', ['--noprofile', '--norc', '-c', HARNESS, 'preflight-regression', PREFLIGHT], {
    cwd: ROOT, env, encoding: 'utf8', timeout: 15000,
  });
  assert.ifError(result.error);
  assert.equal(result.status, expectedStatus, result.stderr || result.stdout);
  assert.doesNotMatch(result.stderr, /UNEXPECTED_.*_CALL/);
  if (format === 'text') return result;
  const report = JSON.parse(result.stdout);
  const count = result.stderr.match(/warn=(\d+)/);
  assert.ok(count, 'the final warning count must be present');
  assert.equal(Number(count[1]), report.warnings.length);
  return { ...result, report };
}

for (const tool of ['ss', 'netstat']) {
  test(`${tool} failure stays unknown and warns instead of reporting free`, (t) => {
    const { report } = runPreflight(t, { tool, mode: 'fail' });
    assert.equal(report.port_probe_tool, tool);
    assert.deepEqual(report.ports, [{ port: 8080, state: 'unknown' }]);
    assert.ok(report.warnings.some((w) => w.includes('8080') && w.includes('unknown')));
  });
  for (const mode of ['free', 'occupied']) {
    test(`${tool} successful ${mode} probe preserves the observed state`, (t) => {
      const { report } = runPreflight(t, { tool, mode });
      assert.deepEqual(report.ports, [{ port: 8080, state: mode }]);
    });
  }
}

test('directory warnings survive into JSON and the final warning count', (t) => {
  const { report } = runPreflight(t, { dirs: 'readonly' });
  for (const field of ['data_dir', 'keys_dir', 'backup_dir']) {
    assert.equal(report[field].exists, true);
    assert.equal(report[field].writable_by_runner, false);
    assert.match(report[field].note, /ErrLockAccess/);
  }
  assert.equal(report.warnings.filter((w) => w.includes('ErrLockAccess')).length, 3);
});

test('directory warnings are also retained in the human-readable report', (t) => {
  const { stdout, stderr } = runPreflight(t, { dirs: 'readonly', format: 'text' });
  assert.equal((stdout.match(/^  - .*ErrLockAccess/gm) || []).length, 3);
  const count = stdout.match(/需要人判断的 (\d+) 项/);
  assert.ok(count);
  assert.equal(Number(count[1]), Number(stderr.match(/warn=(\d+)/)[1]));
});

test('SQLite temporary storage is reported independently of the database filesystem', (t) => {
  const { report } = runPreflight(t);
  assert.ok(Array.isArray(report.sqlite_temp_candidates));
  assert.deepEqual(report.sqlite_temp_candidates.map((item) => item.path), ['/var/tmp', '/usr/tmp', '/tmp']);
  assert.ok(report.sqlite_temp_candidates.every((item) => item.avail_kb === 99000));
  assert.ok(report.warnings.some((item) => item.includes('SQLite') && item.includes('临时')));
});

test('strict mode returns exit 3 outside formal support', (t) => {
  const { report } = runPreflight(t, { strict: true, expectedStatus: 3 });
  assert.equal(report.strict, true);
  assert.ok(report.strict_failures.length > 0);
  assert.ok(report.warnings.some((w) => w.includes('严格预检阻断')));
});

test('strict mode rejects key symlinks and reports the required key names', (t) => {
  const { report } = runPreflight(t, { strict: true, expectedStatus: 3, scanKeys: true });
  assert.equal(report.key_master_present, true);
  assert.equal(report.key_pepper_present, true);
  assert.equal(report.key_files_that_are_symlinks, 1);
  assert.ok(report.strict_failures.some((item) => item.includes('符号链接')));
});

function serverDirectives(source) {
  const text = source.replace(/#.*$/gm, '');
  const blocks = [];
  for (const match of text.matchAll(/\bserver\s*\{/g)) {
    let depth = 1;
    let direct = '';
    for (let i = match.index + match[0].length; i < text.length && depth > 0; i++) {
      const char = text[i];
      if (char === '{') depth++;
      else if (char === '}') depth--;
      else if (depth === 1) direct += char;
    }
    assert.equal(depth, 0, 'template server block must close');
    blocks.push(direct);
  }
  return { text, blocks };
}

test('HTTP and HTTPS explicitly select query-free access logs and discard unsafe error logs', () => {
  const { text, blocks } = serverDirectives(readFileSync(join(ROOT, 'deploy/docker/nginx.conf.example'), 'utf8'));
  assert.equal(blocks.length, 2, 'both server blocks must be inspected');
  for (const port of [80, 443]) {
    const matches = blocks.filter((block) => new RegExp(`\\blisten ${port}(?:[ ;])`).test(block));
    assert.equal(matches.length, 1);
    assert.match(matches[0], /access_log\s+\/var\/log\/nginx\/epocha\.access\.log\s+epocha_access\s*;/);
    assert.match(matches[0], /error_log\s+\/dev\/null\s+crit\s*;/);
  }
  const format = text.match(/\blog_format\s+epocha_access\s+([^;]+);/);
  assert.ok(format, 'safe log format must exist');
  assert.deepEqual([...new Set(format[1].match(/\$[a-z_]+/g))].sort(), [
    '$body_bytes_sent', '$remote_addr', '$request_method', '$request_time', '$status', '$time_local', '$uri',
  ].sort());
});

test('restore runbook switches complete directories instead of copying into an old WAL directory', () => {
  const doc = readFileSync(join(ROOT, 'docs/BACKUP-RESTORE.md'), 'utf8');
  const section = doc.match(/## 5\. 恢复流程[\s\S]*?(?=### `admin-recover`)/)?.[0];
  assert.ok(section, 'the handover runbook must exist');
  assert.doesNotMatch(section, /^\s*(?:cp|mv)\s+[^\n]*epocha\.db/m);
  assert.match(section, /EPOCHA_DATA_DIR=\$NEW_DATA/);
  assert.match(section, /EPOCHA_KEY_DIR=\$NEW_KEYS/);
  assert.match(section, /EPOCHA_ADMIN_KEY_FILE=\/etc\/epocha\/keys\/admin/);
  assert.match(section, /EPOCHA_MASTER_KEY_PREVIOUS_FILE/);
  assert.match(section, /\/api\/admin\/whoami/);
  assert.match(section, /ErrLockHeld/);
  assert.match(section, /ErrLockAccess/);
  const secret = section.indexOf('/secrets');
  const enable = section.indexOf('/enable');
  assert.ok(secret >= 0 && enable > secret, 'issue a new secret before enabling the site');
});

test('deployment indexes keep Cloudflare and fallback unimplemented without a false SQLite claim', () => {
  const docker = readFileSync(join(ROOT, 'deploy/docker/README.md'), 'utf8');
  const cloudflare = readFileSync(join(ROOT, 'deploy/cloudflare/README.md'), 'utf8');
  assert.doesNotMatch(docker, /副本\s*>\s*1\s*直接破坏一次性消费/);
  assert.match(docker, /ADR-004/);
  assert.match(cloudflare, /未实现/);
  assert.match(cloudflare, /fallback_base/);
  assert.match(cloudflare, /免费计划.*SQLite-backed Durable Objects/);
  assert.doesNotMatch(cloudflare, /Durable Objects 需要付费计划/);
});
