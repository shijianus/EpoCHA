#!/usr/bin/env node
import { readFileSync, writeFileSync, realpathSync, statSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { resolve, relative, isAbsolute, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const sha256 = (bytes) => createHash('sha256').update(bytes).digest('hex');

export function buildReport(directory, tests) {
  const root = realpathSync(directory);
  if (!Array.isArray(tests) || !tests.length) throw new Error('nonempty test evidence is required');
  const result = tests.map((item) => {
    if (!item || typeof item.test !== 'string' || !item.test || !/^[a-f0-9]{40}$/.test(item.commit) ||
        !item.environment?.os || !item.environment?.arch || !Array.isArray(item.command) || !item.command.length ||
        !item.command.every((part) => typeof part === 'string' && part.length) ||
        !['PASS', 'FAIL', 'BLOCKED', 'NOT_RUN'].includes(item.status) ||
        !Array.isArray(item.defects) || !Array.isArray(item.fixes)) throw new Error('incomplete test receipt');
    if (item.status === 'PASS' && item.exit_code !== 0) throw new Error('PASS requires process exit 0');
    if (item.status === 'FAIL' && (!Number.isInteger(item.exit_code) || item.exit_code === 0)) throw new Error('FAIL requires a failed command');
    const unexecuted = item.exit_code === null;
    if (unexecuted && !['BLOCKED', 'NOT_RUN'].includes(item.status)) throw new Error('unexecuted test cannot pass');
    if (unexecuted && (!item.reason || item.log !== null)) throw new Error('missing unexecuted reason or contradictory log');
    if (!unexecuted && !Number.isInteger(item.exit_code)) throw new Error('raw exit code unavailable');
    let logHash = null;
    if (!unexecuted) {
      if (typeof item.log !== 'string' || !item.log || isAbsolute(item.log)) throw new Error('relative raw log required');
      const file = realpathSync(resolve(root, item.log));
      const rel = relative(root, file);
      if (isAbsolute(rel) || rel === '..' || rel.startsWith('..\\') || rel.startsWith('../') || !statSync(file).isFile()) throw new Error('log escaped evidence directory');
      const log = readFileSync(file);
      if (!log.length) throw new Error('empty log is not evidence');
      logHash = sha256(log);
    }
    return { ...item, log_sha256: logHash };
  });
  return { schema_version: 1, generated_at_utc: new Date().toISOString(), release_approved: false,
    purpose: 'Traceable baseline evidence; formal release-check remains authoritative.',
    tests: result };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    if (process.argv.length !== 4) throw new Error('usage: node baseline-evidence.mjs EVIDENCE_DIR INPUT_JSON');
    const root = resolve(process.argv[2]);
    const report = buildReport(root, JSON.parse(readFileSync(process.argv[3], 'utf8')));
    const bytes = Buffer.from(JSON.stringify(report, null, 2) + '\n');
    writeFileSync(join(root, 'report.json'), bytes, { flag: 'wx' });
    writeFileSync(join(root, 'report.sha256'), sha256(bytes) + '  report.json\n', { flag: 'wx' });
    console.log(`BASELINE_EVIDENCE_TESTS=${report.tests.length} RELEASE_APPROVED=false`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
