#!/usr/bin/env node
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, statfsSync, writeFileSync } from 'node:fs';
import { resolve, join } from 'node:path';
import { createHash } from 'node:crypto';

const [directory, id, ...command] = process.argv.slice(2);
if (!directory || !/^[a-z0-9-]+$/.test(id || '') || !command.length) throw Error('usage: baseline-run.mjs NEW_EVIDENCE_DIR ID COMMAND [ARGS...]');
const root = resolve(directory);
if (!existsSync(root)) mkdirSync(root, { recursive: true, mode: 0o700 });
const receiptPath = join(root, `${id}.receipt.json`);
if (existsSync(receiptPath) || existsSync(join(root, `${id}.log`))) throw Error('evidence id already exists');
const git = (args) => {
  const r = spawnSync('git', args, { encoding: 'utf8' });
  if (r.status !== 0) throw Error('source identity unavailable');
  return r.stdout.trim();
};
const commit = git(['rev-parse', 'HEAD']);
const dirty = git(['status', '--porcelain=v1']);
if (!/^[a-f0-9]{40}$/.test(commit) || dirty) throw Error('clean exact source commit required');
const start = new Date().toISOString();
const temp = process.env.SQLITE_TMPDIR;
let blocked;
try {
  if (!temp) throw Error('explicit isolated SQLITE_TMPDIR required');
  const info = statfsSync(temp);
  if (info.bavail * info.bsize < 256 * 1024 * 1024) throw Error('SQLite temporary filesystem has less than 256MiB available');
} catch (error) { blocked = error.message; }
let result;
if (!blocked) result = spawnSync(command[0], command.slice(1), { encoding: 'utf8', env: { ...process.env, GOTOOLCHAIN: 'local', GOPROXY: 'off', GOSUMDB: 'off', GOWORK: 'off', GOENV: 'off' }, timeout: 30 * 60 * 1000, maxBuffer: 64 * 1024 * 1024 });
const log = blocked ? `BLOCKED: ${blocked}\n` : `${result.stdout || ''}${result.stderr || ''}${result.error ? `\n${result.error.message}\n` : ''}`;
const code = blocked ? null : result.status;
const status = blocked || code === 3 ? 'BLOCKED' : code === 0 ? 'PASS' : 'FAIL';
const receipt = { schema_version: 1, test: id, commit, dirty: false, environment: { os: process.platform, arch: process.arch, node: process.version }, command, started_at_utc: start, finished_at_utc: new Date().toISOString(), exit_code: code, status, reason: blocked || (code === 3 ? 'command reports incomplete conditions; inspect raw log' : undefined), signal: result?.signal || null, log: `${id}.log`, log_sha256: createHash('sha256').update(log).digest('hex') };
writeFileSync(join(root, `${id}.log`), log, { flag: 'wx', mode: 0o600 });
writeFileSync(receiptPath, JSON.stringify(receipt, null, 2) + '\n', { flag: 'wx', mode: 0o600 });
console.log(`${id} STATUS=${status} RAW_EXIT=${code} COMMIT=${commit}`);
process.exitCode = blocked ? 3 : code ?? 1;
