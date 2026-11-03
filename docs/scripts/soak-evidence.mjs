#!/usr/bin/env node
// Acceptance-only evidence checks. No runtime dependency, credentials or database contents are archived.
import { createHash } from 'node:crypto';
import { lstatSync, readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn, spawnSync } from 'node:child_process';

export const SOAK_LIMIT_MS = 300000;
const requireValue = (value, message) => { if (!value) throw new Error(message); };
const sha = (value) => createHash('sha256').update(value).digest('hex');
const positive = (value) => Number.isSafeInteger(value) && value > 0;
function durationMS(text) {
  let consumed = '';
  let ms = 0;
  for (const match of text.matchAll(/(\d+(?:\.\d+)?)(ms|µs|us|ns|h|m|s)/g)) {
    consumed += match[0];
    ms += Number(match[1]) * { h: 3600000, m: 60000, s: 1000, ms: 1, 'µs': 0.001, us: 0.001, ns: 0.000001 }[match[2]];
  }
  requireValue(consumed === text && ms >= 0 && Number.isFinite(ms), 'malformed Go duration');
  return ms;
}
export function checkLoad({ text, exit_code, elapsed_ms, timed_out }) {
  requireValue(exit_code === 0 && timed_out === false && Number.isFinite(elapsed_ms) && elapsed_ms > 0 && elapsed_ms <= SOAK_LIMIT_MS, 'load process failed or exceeded the independent 300s deadline');
  requireValue(typeof text === 'string' && text.trim(), 'empty load evidence');
  const plans = [...text.matchAll(/^负载形状 concurrency=(\d+) requests=(\d+) base=\S+（凭据不回显）\r?$/gm)];
  requireValue(plans.length === 1 && plans[0][1] === '1' && plans[0][2] === '450', 'load plan must be exactly concurrency=1 requests=450');
  const pace = [...text.matchAll(/^节流 pace=([^\s]+)（每 worker）[^\r\n]*$/gm)];
  requireValue(pace.length === 1 && durationMS(pace[0][1]) === 660, 'actual pacing must be 660ms');
  const summaries = [...text.matchAll(/^wall=(\S+) flows=(\d+) ok=(\d+) 吞吐=(\d+(?:\.\d+)?) 闭环\/秒\r?$/gm)];
  requireValue(summaries.length === 1 && summaries[0][2] === '450' && summaries[0][3] === '450', 'require exact flows=450 and ok=450 (no prefix matches)');
  const wall_ms = durationMS(summaries[0][1]);
  requireValue(wall_ms >= 449 * 660 && wall_ms <= SOAK_LIMIT_MS && wall_ms <= elapsed_ms + 1000, 'load wall time contradicts pacing or the actual deadline');
  requireValue(!/^失败类别：/m.test(text), 'load reports failed attempts');
  const stages = {};
  for (const name of ['challenge', 'redeem', 'siteverify', 'pow-tool']) {
    const matches = [...text.matchAll(new RegExp(`^${name}\\s+n=(\\d+) p50=(\\S+) p95=(\\S+) max=(\\S+)\\r?$`, 'gm'))];
    requireValue(matches.length === 1 && matches[0][1] === '450', `${name} must have 450 actual latency samples`);
    const [p50_ms, p95_ms, max_ms] = matches[0].slice(2, 5).map((value) => durationMS(value.trim()));
    requireValue(p50_ms <= p95_ms && p95_ms <= max_ms, `${name} malformed latency ordering`);
    stages[name] = { n: 450, p50_ms, p95_ms, max_ms };
  }
  return { requests: 450, flows: 450, ok: 450, failed: 0, concurrency: 1, pace_ms: 660, wall_ms, elapsed_ms, stages };
}

export function checkImageSite(exit_code, value) {
  const site = value?.site;
  requireValue(exit_code === 0 && site && typeof site.site_key === 'string' && /^pk_[A-Za-z0-9_-]+$/.test(site.site_key) &&
    site.verify_mode === 'pow_question' && site.generator_id === 'text-image-v1' && site.difficulty === 5, 'image site creation did not return the actual requested site');
  return site;
}
export function checkPool(value) {
  requireValue(Array.isArray(value?.buckets) && value.buckets.length > 0, 'pool buckets missing or empty');
  const buckets = value.buckets.filter((item) => item.generator_id === 'text-image-v1' && item.difficulty === 5);
  requireValue(buckets.length > 0, 'no actual difficulty-5 text-image bucket');
  for (const item of buckets) {
    requireValue(positive(item.ready) && positive(item.ready_asset_bytes) && positive(item.total_asset_bytes) && item.total_asset_bytes >= item.ready_asset_bytes, 'image ready count/bytes must be positive actual numbers');
  }
  requireValue(positive(value.image?.asset_bytes) && positive(value.image?.budget_bytes) && value.image.asset_bytes <= value.image.budget_bytes, 'image bytes/budget missing, zero, or inconsistent');
  const ready_asset_bytes = buckets.reduce((sum, item) => sum + item.ready_asset_bytes, 0);
  requireValue(ready_asset_bytes <= value.image.asset_bytes, 'bucket bytes exceed global asset bytes');
  return { ready: buckets.reduce((sum, item) => sum + item.ready, 0), ready_asset_bytes, asset_bytes: value.image.asset_bytes, budget_bytes: value.image.budget_bytes };
}
function memoryBytes(value) {
  const match = /^(\d+(?:\.\d+)?)(B|kB|KB|KiB|MB|MiB|GB|GiB)$/.exec(value);
  requireValue(match, 'container memory measurement malformed');
  return Number(match[1]) * { B: 1, kB: 1000, KB: 1000, KiB: 1024, MB: 1000000, MiB: 1048576, GB: 1000000000, GiB: 1073741824 }[match[2]];
}
export function checkSamples(text, elapsed_ms) {
  requireValue(typeof text === 'string' && text.trim() && Number.isFinite(elapsed_ms) && elapsed_ms > 0 && elapsed_ms <= SOAK_LIMIT_MS, 'missing sample window');
  const lines = text.trim().split(/\r?\n/);
  const samples = lines.map((line) => {
    const match = /^t=(\d+)s stats=(\d+(?:\.\d+)?)%\|([^ ]+) \/ ([^ ]+) db=(\d+) wal=(\d+) readyz=200$/.exec(line);
    requireValue(match, 'sample missing, unknown, malformed, or readyz not 200');
    const [t, cpu_percent, used, limit, db, wal] = match.slice(1);
    const sample = { elapsed_ms: Number(t) * 1000, cpu_percent: Number(cpu_percent), container_memory_bytes: memoryBytes(used), container_memory_limit_bytes: memoryBytes(limit), db_bytes: Number(db), wal_bytes: Number(wal), readyz: 200 };
    requireValue(positive(sample.db_bytes) && Number.isSafeInteger(sample.wal_bytes) && sample.wal_bytes >= 0 && sample.container_memory_bytes > 0 && sample.container_memory_bytes <= sample.container_memory_limit_bytes && sample.container_memory_limit_bytes === 128 * 1024 * 1024, 'sample resource values inconsistent with the 128MiB container');
    return sample;
  });
  requireValue(samples.length >= Math.floor(elapsed_ms / 45000) + 1 && samples[0].elapsed_ms <= 15000, 'sample start or count incomplete');
  samples.forEach((sample, index) => {
    requireValue(sample.elapsed_ms <= elapsed_ms + 1000, 'sample outside load window');
    if (index) requireValue(sample.elapsed_ms > samples[index - 1].elapsed_ms && sample.elapsed_ms - samples[index - 1].elapsed_ms <= 45000, 'sample gap or ordering invalid');
  });
  requireValue(elapsed_ms - samples.at(-1).elapsed_ms <= 45000, 'sample end incomplete');
  return samples;
}

export async function runBounded(command, args, { timeout_ms = SOAK_LIMIT_MS, env = process.env } = {}) {
  requireValue(Number.isSafeInteger(timeout_ms) && timeout_ms > 0 && timeout_ms <= SOAK_LIMIT_MS, 'deadline out of bounds');
  const start = performance.now();
  return await new Promise((resolvePromise) => {
    const child = spawn(command, args, { env, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = ''; let stderr = ''; let timed_out = false; let overflow = false; let spawn_error;
    const timer = setTimeout(() => { timed_out = true; child.kill('SIGKILL'); }, timeout_ms);
    const append = (target, chunk) => {
      if (stdout.length + stderr.length + chunk.length > 4 * 1024 * 1024) { overflow = true; child.kill('SIGKILL'); return; }
      if (target === 'out') stdout += chunk; else stderr += chunk;
    };
    child.stdout.on('data', (chunk) => append('out', chunk));
    child.stderr.on('data', (chunk) => append('err', chunk));
    child.on('error', (error) => { spawn_error = error.message; });
    const interrupt = () => { timed_out = true; child.kill('SIGKILL'); };
    process.once('SIGTERM', interrupt); process.once('SIGINT', interrupt);
    child.on('close', (code, signal) => {
      clearTimeout(timer); process.removeListener('SIGTERM', interrupt); process.removeListener('SIGINT', interrupt);
      resolvePromise({ command: [command, ...args], exit_code: timed_out ? 124 : overflow || spawn_error ? 1 : code, signal, timed_out, overflow, spawn_error,
        elapsed_ms: Math.round(performance.now() - start), limit_ms: timeout_ms, stdout, stderr });
    });
  });
}
function jsonFile(path) { return JSON.parse(readFileSync(path, 'utf8')); }
function writeJSON(path, value) { writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { flag: 'wx', mode: 0o600 }); }
function fileSize(path, missingOK = false) {
  try {
    const stat = lstatSync(path);
    requireValue(stat.isFile(), 'sample path is not a regular file');
    return stat.size;
  } catch (error) { if (missingOK && error.code === 'ENOENT') return 0; throw error; }
}
async function sample(options) {
  const stats = spawnSync('docker', ['stats', '--no-stream', '--format', '{{.CPUPerc}}|{{.MemUsage}}', options.container], { encoding: 'utf8', timeout: 10000, maxBuffer: 65536 });
  requireValue(!stats.error && !stats.signal && stats.status === 0 && stats.stdout.trim(), 'docker stats failed or empty');
  const response = await fetch(`${options.base}/readyz`, { signal: AbortSignal.timeout(5000), redirect: 'error' });
  // Fully consume within the same timeout; status alone is not enough when the body hangs.
  await response.text();
  requireValue(response.status === 200, 'sample readyz not 200');
  const line = `t=${options.elapsed}s stats=${stats.stdout.trim()} db=${fileSize(options.db)} wal=${fileSize(`${options.db}-wal`, true)} readyz=200`;
  // Structural checks use the same parser; coverage checks run once against the whole actual window.
  const padded = line.replace(/^t=\d+s/, 't=0s');
  checkSamples(`${padded}\n`, 1);
  console.log(line);
}
async function main(argv) {
  const [action, ...rest] = argv;
  const options = {}; let command;
  for (let i = 0; i < rest.length; i += 2) {
    if (rest[i] === '--') { command = rest.slice(i + 1); break; }
    requireValue(/^--[a-z-]+$/.test(rest[i]) && rest[i + 1] !== undefined && !Object.hasOwn(options, rest[i].slice(2)), 'invalid/duplicate argument');
    options[rest[i].slice(2)] = rest[i + 1];
  }
  if (action === 'run') {
    requireValue(command?.length && options.out && options.log, 'run requires --out --log -- actual-command');
    const result = await runBounded(command[0], command.slice(1));
    const text = `${result.stdout}${result.stderr}`;
    writeFileSync(options.log, text, { flag: 'wx', mode: 0o600 });
    const { stdout, stderr, ...record } = result;
    writeJSON(options.out, { ...record, log_sha256: sha(text) });
    return result.exit_code === 0 ? 0 : 1;
  }
  if (action === 'image-site') { checkImageSite(Number(options.rc), jsonFile(options.file)); return 0; }
  if (action === 'pool') { console.log(JSON.stringify(checkPool(jsonFile(options.file)))); return 0; }
  if (action === 'sample') { await sample(options); return 0; }
  if (action === 'verify') {
    const run = jsonFile(options.run);
    const log = readFileSync(options.log, 'utf8');
    requireValue(run.limit_ms === SOAK_LIMIT_MS && run.log_sha256 === sha(log), 'load log or deadline receipt mismatch');
    const load = checkLoad({ text: log, ...run });
    const samplesText = readFileSync(options.samples, 'utf8');
    const samples = checkSamples(samplesText, run.elapsed_ms);
    const before = checkPool(jsonFile(options['pool-before']));
    const after = checkPool(jsonFile(options['pool-after']));
    writeJSON(options.out, { schema_version: 1, load, samples, image_pool: { before, after },
      input_hashes: { load: sha(log), samples: sha(samplesText), run: sha(readFileSync(options.run)), pool_before: sha(readFileSync(options['pool-before'])), pool_after: sha(readFileSync(options['pool-after'])) },
      scope: 'pow_only flows with background text-image-v1 refill; container memory is NOT process RSS; p95 is NOT p99' });
    return 0;
  }
  throw new Error('usage: soak-evidence.mjs run|image-site|pool|sample|verify');
}
if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) {
  try { process.exitCode = await main(process.argv.slice(2)); } catch (error) { console.error(`[soak-evidence] FAIL: ${error.message}`); process.exitCode = 1; }
}
