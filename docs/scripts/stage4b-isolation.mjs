import { spawnSync } from 'node:child_process';
import { realpathSync, statSync } from 'node:fs';

const CHECKPOINT = 'ISOLATION_CHECKPOINT=mounts-verified';
const NETWORK_CHECKPOINT = 'NETWORK_CHECKPOINT=dns-blocked';
const SCRIPT = [
  'set -eu',
  'test -s /in/one.png',
  'test ! -e /in/ground-truth.jsonl',
  'test ! -e /answer.txt',
  `printf '%s\\n' '${CHECKPOINT}'`,
  'command -v getent >/dev/null 2>&1',
  'if getent hosts github.com >/dev/null 2>&1; then printf \'DNS_REACHABLE\\n\'; exit 1; fi',
  `printf '%s\\n' '${NETWORK_CHECKPOINT}'`,
].join('; ');

export function classifyIsolationRun(result) {
  if (!result || result.error || result.signal || result.status !== 0) {
    return { status: 'FAIL', reason: `container/test command did not complete (exit=${result?.status ?? 'unknown'}${result?.signal ? ` signal=${result.signal}` : ''}${result?.error ? ` error=${result.error.message}` : ''})` };
  }
  const output = `${result.stdout || ''}${result.stderr || ''}`;
  if (!output.split(/\r?\n/).includes(CHECKPOINT) || !output.split(/\r?\n/).includes(NETWORK_CHECKPOINT)) {
    return { status: 'FAIL', reason: 'expected image, hidden-answer, and network checkpoints are missing' };
  }
  if (/^(?:GROUNDTRUTH|ANSWER)_VISIBLE=/m.test(output) || /^DNS_REACHABLE$/m.test(output)) {
    return { status: 'FAIL', reason: 'isolation probe found visible answer material or reachable DNS' };
  }
  return { status: 'PASS', checkpoint: CHECKPOINT, network_checkpoint: NETWORK_CHECKPOINT, raw_exit_code: 0 };
}

export function runIsolationProbe({ image, stage, docker = 'docker', run = spawnSync }) {
  if (!image || !stage) return { status: 'FAIL', reason: 'image and stage paths are required', raw_exit_code: null };
  const source = realpathSync(stage);
  if (!statSync(source).isDirectory()) return { status: 'FAIL', reason: 'stage is not a directory', raw_exit_code: null };
  const result = run(docker, [
    'run', '--rm', '--network', 'none', '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
    '-v', `${source}:/in:ro`, '--entrypoint', '/bin/sh', image, '-ec', SCRIPT,
  ], { encoding: 'utf8', timeout: 60_000, maxBuffer: 1024 * 1024 });
  const verdict = classifyIsolationRun(result);
  return { ...verdict, raw_exit_code: result.status ?? (result.error ? null : 1), stdout: result.stdout || '', stderr: result.stderr || '' };
}

if (process.argv[1] && process.argv[1].endsWith('stage4b-isolation.mjs')) {
  const [image, stage] = process.argv.slice(2);
  const result = runIsolationProbe({ image, stage });
  console.log(`[stage4b-isolation] ${result.status}${result.raw_exit_code === null ? '' : ` rc=${result.raw_exit_code}`}: ${result.reason || result.checkpoint}`);
  if (result.stdout) process.stdout.write(result.stdout);
  if (result.stderr) process.stderr.write(result.stderr);
  process.exitCode = result.status === 'PASS' ? 0 : 1;
}
