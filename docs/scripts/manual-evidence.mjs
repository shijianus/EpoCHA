#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { existsSync, readFileSync, realpathSync, statSync } from 'node:fs';
import { dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const MAX_PACKET_BYTES = 1024 * 1024;
const MAX_ARTIFACT_BYTES = 128 * 1024 * 1024;
const SHA256 = /^[a-f0-9]{64}$/;
const CHECK_ID = /^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)+$/;
const hash = bytes => createHash('sha256').update(bytes).digest('hex');

function git(root, args) {
  const result = spawnSync('git', ['-C', root, ...args], { encoding: 'utf8', maxBuffer: 32 * 1024 * 1024, timeout: 30000 });
  if (result.error || result.status !== 0) throw new Error('source-inventory-unavailable');
  return result.stdout;
}

// 与 release-check 的 raw-byte manifest 一致；未跟踪源文件也影响快照，已删除路径记录 null。
export function sourceSnapshot(root = ROOT) {
  root = resolve(root);
  const commit = git(root, ['rev-parse', 'HEAD']).trim();
  if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error('source-commit-unavailable');
  const files = [...new Set(git(root, ['ls-files', '--cached', '--others', '--exclude-standard', '-z']).split('\0').filter(Boolean))].sort();
  if (!files.length) throw new Error('source-inventory-empty');
  const manifest = files.map(path => ({ path, sha256: existsSync(join(root, path)) ? hash(readFileSync(join(root, path))) : null }));
  return { commit, snapshot_sha256: hash(JSON.stringify(manifest)), file_count: manifest.length };
}

function text(value, max = 2048) {
  return typeof value === 'string' && value.length > 0 && value.length <= max && value.trim() === value && !/[\u0000-\u001f\u007f]/.test(value);
}
function performer(value) {
  return text(value, 128) && !/^(?:your name|your-name|executor|name|todo|tbd|unknown|example|placeholder|agent|assistant|chatgpt|codex|zcode|待填|姓名|执行者|執行者)$/i.test(value);
}
function timestamp(value) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{3})?Z$/.test(value)) return false;
  const n = Date.parse(value);
  if (!Number.isFinite(n) || n > Date.now()) return false;
  return new Date(n).toISOString() === (value.includes('.') ? value : value.slice(0, -1) + '.000Z');
}
function artifactPath(value) {
  return text(value, 1024) && !isAbsolute(value) && !/^[a-zA-Z]:/.test(value) && !value.includes('\\') &&
    value.split('/').every(part => part && part !== '.' && part !== '..');
}
function result(status, checkId, reason, extra = {}) {
  return { status, exit_code: status === 'PASS' ? 0 : status === 'NOT_RUN' ? 3 : 1, check_id: checkId, reason, ...extra };
}
function invalid(condition, reason) { if (!condition) throw new Error(reason); }

export function checkManualEvidence({ root = ROOT, checkId, evidenceFile = process.env.EPOCHA_MANUAL_EVIDENCE } = {}) {
  if (typeof checkId !== 'string' || !CHECK_ID.test(checkId)) return result('FAIL', checkId || '', 'invalid-check-id');
  if (!evidenceFile) return result('NOT_RUN', checkId, 'manual-evidence-not-provided');
  try {
    const packetPath = resolve(evidenceFile);
    if (!existsSync(packetPath)) return result('NOT_RUN', checkId, 'manual-evidence-file-missing');
    const packetStat = statSync(packetPath);
    invalid(packetStat.isFile() && packetStat.size > 0 && packetStat.size <= MAX_PACKET_BYTES, 'invalid-evidence-file');
    let packet;
    try { packet = JSON.parse(readFileSync(packetPath, 'utf8')); }
    catch { throw new Error('invalid-evidence-json'); }
    invalid(packet && packet.schema_version === 1 && Array.isArray(packet.checks) && packet.checks.length <= 128, 'invalid-evidence-schema');
    const source = packet.source;
    invalid(source && /^[a-f0-9]{40}$/.test(source.commit) && SHA256.test(source.snapshot_sha256) &&
      Number.isSafeInteger(source.file_count) && source.file_count > 0, 'invalid-source-fingerprint');
    const ids = new Set();
    for (const check of packet.checks) {
      invalid(check && CHECK_ID.test(check.id) && !ids.has(check.id), 'invalid-or-duplicate-check-id');
      ids.add(check.id);
      invalid(performer(check.executor), 'invalid-executor');
      invalid(timestamp(check.performed_at), 'invalid-execution-date');
      invalid(check.result === 'PASS' || check.result === 'FAIL', 'invalid-recorded-result');
      invalid(text(check.notes), 'invalid-execution-notes');
      invalid(Array.isArray(check.artifacts) && check.artifacts.length > 0 && check.artifacts.length <= 32, 'invalid-artifact-list');
      const paths = new Set();
      for (const artifact of check.artifacts) {
        invalid(artifact && artifactPath(artifact.path) && !paths.has(artifact.path) && SHA256.test(artifact.sha256), 'invalid-artifact-record');
        paths.add(artifact.path);
      }
    }
    const check = packet.checks.find(item => item.id === checkId);
    if (!check) return result('NOT_RUN', checkId, 'manual-check-not-recorded');
    const current = sourceSnapshot(root);
    invalid(source.commit === current.commit && source.file_count === current.file_count && source.snapshot_sha256 === current.snapshot_sha256,
      'source-fingerprint-mismatch');
    const evidenceRoot = realpathSync(dirname(packetPath));
    const artifacts = [];
    for (const artifact of check.artifacts) {
      const path = join(evidenceRoot, artifact.path);
      if (!existsSync(path)) return result('NOT_RUN', checkId, 'raw-evidence-missing');
      const target = realpathSync(path);
      const within = relative(evidenceRoot, target);
      invalid(within && !isAbsolute(within) && within !== '..' && !within.startsWith('..' + sep), 'raw-evidence-outside-bundle');
      invalid(target !== realpathSync(packetPath), 'self-referential-evidence');
      const info = statSync(target);
      invalid(info.isFile() && info.size > 0 && info.size <= MAX_ARTIFACT_BYTES, 'invalid-raw-evidence-file');
      const bytes = readFileSync(target);
      invalid(bytes.length > 0 && hash(bytes) === artifact.sha256, 'raw-evidence-hash-mismatch');
      artifacts.push({ path: artifact.path, sha256: artifact.sha256, bytes: bytes.length });
    }
    invalid(JSON.stringify(sourceSnapshot(root)) === JSON.stringify(current), 'source-changed-during-validation');
    return result(check.result, checkId, check.result === 'PASS' ? 'manual-record-and-artifacts-validated' : 'executor-recorded-failure', {
      executor: check.executor, performed_at: check.performed_at, source: current, artifacts,
    });
  } catch (error) {
    // 只回固定诊断，JSON 原文、证据内容与操作系统报错不进入门禁日志。
    const known = /^(invalid-|source-|raw-evidence-|self-referential-)/.test(error.message);
    return result('FAIL', checkId, known ? error.message : 'evidence-validation-failed');
  }
}

function cli(args) {
  let root = ROOT, checkId, snapshot = false;
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--root' && args[i + 1]) root = args[++i];
    else if (args[i] === '--check' && args[i + 1] && !checkId) checkId = args[++i];
    else if (args[i] === '--snapshot' && !snapshot) snapshot = true;
    else return result('FAIL', checkId || '', 'invalid-cli-arguments');
  }
  if (snapshot && !checkId) {
    try { return { status: 'PASS', exit_code: 0, source: sourceSnapshot(root) }; }
    catch { return result('FAIL', '', 'source-inventory-unavailable'); }
  }
  if (snapshot || !checkId) return result('FAIL', checkId || '', 'invalid-cli-arguments');
  return checkManualEvidence({ root, checkId });
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const output = cli(process.argv.slice(2));
  process.stdout.write(JSON.stringify(output) + '\n');
  process.exitCode = output.exit_code;
}
