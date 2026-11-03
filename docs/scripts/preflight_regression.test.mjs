import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const PREFLIGHT = join(ROOT, 'deploy/preflight.sh').replaceAll('\\', '/');

// Source the actual script, but never contact Docker, a web server, or a remote host.
const HARNESS = String.raw`
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
find() { return 0; }
if [[ "$PF_DIRS" == readonly ]]; then
  locked_dir=''
  for d in /proc/sys /sys /proc; do
    if [[ -d "$d" && ! -w "$d" ]]; then locked_dir="$d"; break; fi
  done
  [[ -n "$locked_dir" ]] || { printf 'BLOCKED: no readonly pseudo-directory fixture\n' >&2; exit 125; }
  PF_DATA="$locked_dir"; PF_KEYS="$locked_dir"; PF_BACKUP="$locked_dir"
fi
source "$1" --data-dir "$PF_DATA" --keys-dir "$PF_KEYS" --backup-dir "$PF_BACKUP" --ports 8080 --"$PF_FORMAT"
`;

function runPreflight(t, { tool = 'ss', mode = 'free', dirs = 'writable', format = 'json' } = {}) {
  const temp = mkdtempSync(join(tmpdir(), 'epocha-preflight-regression-'));
  t.after(() => rmSync(temp, { recursive: true, force: true })); // Only this test's own new fixture.
  for (const name of ['data', 'keys', 'backups']) mkdirSync(join(temp, name));
  const env = {
    ...process.env,
    PF_TOOL: tool, PF_MODE: mode, PF_DIRS: dirs, PF_FORMAT: format,
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
  assert.equal(result.status, 0, result.stderr || result.stdout);
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
