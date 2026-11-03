import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { checkManualEvidence, sourceSnapshot } from './manual-evidence.mjs';

const helper = fileURLToPath(new URL('./manual-evidence.mjs', import.meta.url));
const sha256 = (bytes) => createHash('sha256').update(bytes).digest('hex');
const checkId = 'stage4a.human-readability';
function fixture() {
  const dir = mkdtempSync(join(tmpdir(), 'epocha-manual-test-'));
  const root = join(dir, 'source');
  mkdirSync(root);
  const git = (...args) => {
    const result = spawnSync('git', ['-C', root, ...args], { encoding: 'utf8' });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout;
  };
  git('init', '--quiet');
  writeFileSync(join(root, '.gitignore'), 'output/\n');
  writeFileSync(join(root, 'source.txt'), 'candidate source\n');
  git('add', '.');
  git('-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', '-c', 'commit.gpgsign=false', 'commit', '--quiet', '-m', 'fixture');
  const artifact = join(dir, 'raw-session.txt');
  writeFileSync(artifact, 'TEST FIXTURE ONLY: no real human acceptance.\n');
  const packet = { schema_version: 1, source: sourceSnapshot(root), checks: [{
    id: checkId, executor: 'Fixture Tester', performed_at: new Date().toISOString(), result: 'PASS',
    notes: 'Synthetic validator fixture, never evidence for a real gate.',
    artifacts: [{ path: 'raw-session.txt', sha256: sha256(readFileSync(artifact)) }],
  }] };
  const file = join(dir, 'evidence.json');
  function save() { writeFileSync(file, JSON.stringify(packet)); }
  save();
  return { dir, root, git, artifact, packet, file, save,
    check: () => checkManualEvidence({ root, checkId, evidenceFile: file }) };
}

// 每个变异独立建仓库，验证既包含 CLI 也包含实际 raw-byte 源码指纹。
test('missing explicit evidence file and missing check are NOT_RUN with real exit 3', () => {
  const f = fixture();
  for (const evidenceFile of ['', join(f.dir, 'absent.json')]) {
    const result = checkManualEvidence({ root: f.root, checkId, evidenceFile });
    assert.equal(result.status, 'NOT_RUN'); assert.equal(result.exit_code, 3);
  }
  f.packet.checks = []; f.save();
  assert.equal(f.check().status, 'NOT_RUN');
});

test('complete source fingerprint matches current raw bytes including untracked and deleted files', () => {
  const f = fixture();
  const files = f.git('ls-files', '--cached', '--others', '--exclude-standard', '-z').split('\0').filter(Boolean).sort();
  const manifest = files.map(path => ({ path, sha256: sha256(readFileSync(join(f.root, path))) }));
  assert.equal(f.packet.source.commit, f.git('rev-parse', 'HEAD').trim());
  assert.equal(f.packet.source.file_count, files.length);
  assert.equal(f.packet.source.snapshot_sha256, sha256(JSON.stringify(manifest)));
  const original = sourceSnapshot(f.root);
  writeFileSync(join(f.root, 'untracked.txt'), 'new file');
  assert.notEqual(sourceSnapshot(f.root).snapshot_sha256, original.snapshot_sha256);
  const failure = f.check(); assert.equal(failure.status, 'FAIL'); assert.equal(failure.exit_code, 1);
});

test('valid attestation and nonempty hashed raw evidence can be accepted only for its exact check', () => {
  const f = fixture(), result = f.check();
  assert.equal(result.status, 'PASS'); assert.equal(result.exit_code, 0);
  assert.equal(result.check_id, checkId); assert.equal(result.executor, 'Fixture Tester');
  assert.equal(result.artifacts.length, 1);
  assert.equal(checkManualEvidence({ root: f.root, checkId: 'stage4b.human-readability', evidenceFile: f.file }).status, 'NOT_RUN');
});

test('declared FAIL remains failure even with valid evidence', () => {
  const f = fixture(); f.packet.checks[0].result = 'FAIL'; f.save();
  assert.equal(f.check().status, 'FAIL'); assert.equal(f.check().exit_code, 1);
});

for (const [name, mutate] of [
  ['executor absent', p => { delete p.checks[0].executor; }],
  ['executor empty', p => { p.checks[0].executor = '   '; }],
  ['placeholder executor', p => { p.checks[0].executor = 'YOUR NAME'; }],
  ['date absent', p => { delete p.checks[0].performed_at; }],
  ['date without timezone', p => { p.checks[0].performed_at = '2026-11-02T12:00:00'; }],
  ['invalid calendar date', p => { p.checks[0].performed_at = '2026-02-30T12:00:00.000Z'; }],
  ['future execution', p => { p.checks[0].performed_at = '2999-01-01T00:00:00.000Z'; }],
  ['summary absent', p => { delete p.checks[0].notes; }],
  ['check id absent', p => { delete p.checks[0].id; }],
  ['duplicate check', p => { p.checks.push({ ...p.checks[0] }); }],
  ['unsupported result', p => { p.checks[0].result = 'force-pass'; }],
  ['missing artifacts', p => { p.checks[0].artifacts = []; }],
  ['missing raw hash', p => { delete p.checks[0].artifacts[0].sha256; }],
  ['wrong raw hash', p => { p.checks[0].artifacts[0].sha256 = 'a'.repeat(64); }],
  ['absolute evidence path', p => { p.checks[0].artifacts[0].path = '/tmp/evidence.txt'; }],
  ['Windows absolute evidence path', p => { p.checks[0].artifacts[0].path = 'C:/evidence.txt'; }],
  ['relative path traversal', p => { p.checks[0].artifacts[0].path = '../evidence.txt'; }],
  ['ambiguous path separators', p => { p.checks[0].artifacts[0].path = '..\\evidence.txt'; }],
  ['fingerprint absent', p => { delete p.source.snapshot_sha256; }],
  ['fingerprint wrong', p => { p.source.snapshot_sha256 = 'f'.repeat(64); }],
  ['abbreviated commit', p => { p.source.commit = p.source.commit.slice(0, 7); }],
  ['file count wrong', p => { p.source.file_count += 1; }],
  ['unknown schema', p => { p.schema_version = 999; }],
]) {
  test('rejects ' + name, () => {
    const f = fixture(); mutate(f.packet); f.save();
    const result = f.check(); assert.equal(result.status, 'FAIL'); assert.equal(result.exit_code, 1);
  });
}

test('missing raw evidence is NOT_RUN, empty or changed raw evidence never passes', () => {
  const f = fixture();
  f.packet.checks[0].artifacts[0].path = 'missing.txt'; f.save();
  assert.equal(f.check().status, 'NOT_RUN'); assert.equal(f.check().exit_code, 3);
  f.packet.checks[0].artifacts[0].path = 'raw-session.txt'; f.save();
  writeFileSync(f.artifact, '');
  assert.equal(f.check().status, 'FAIL');
  writeFileSync(f.artifact, 'tampered');
  assert.equal(f.check().status, 'FAIL');
});

test('malformed JSON is failure and no file contents appear in diagnostic output', () => {
  const f = fixture(); writeFileSync(f.file, '{SENTINEL_PRIVATE_EVIDENCE');
  const result = f.check(); assert.equal(result.status, 'FAIL');
  assert.equal(JSON.stringify(result).includes('SENTINEL_PRIVATE_EVIDENCE'), false);
});

test('CLI uses only explicit EPOCHA_MANUAL_EVIDENCE and preserves 0,1,3', () => {
  const f = fixture();
  function run(evidenceFile, args = ['--check', checkId]) {
    const env = { ...process.env }; delete env.EPOCHA_MANUAL_EVIDENCE;
    if (evidenceFile) env.EPOCHA_MANUAL_EVIDENCE = evidenceFile;
    const result = spawnSync(process.execPath, [helper, '--root', f.root, ...args], { env, encoding: 'utf8' });
    return { rc: result.status, json: JSON.parse(result.stdout) };
  }
  assert.equal(run('').rc, 3);
  assert.equal(run(f.file).rc, 0);
  f.packet.checks[0].artifacts[0].sha256 = '0'.repeat(64); f.save();
  assert.equal(run(f.file).rc, 1);
  assert.equal(run('', ['--force-pass', '--check', checkId]).rc, 1);
  const snapshot = run('', ['--snapshot']);
  assert.equal(snapshot.rc, 0); assert.deepEqual(snapshot.json.source, sourceSnapshot(f.root));
});

for (const name of ['stage4a.sh', 'stage4b.sh', 'admin-ui-check.sh']) {
  test(name + ' manual_result executes helper and preserves accepted, missing and invalid outcomes', () => {
    const script = readFileSync(new URL(name, import.meta.url), 'utf8');
    const match = script.match(/^manual_result\(\) \{\n[\s\S]*?^\}/m);
    assert.ok(match, 'gate must expose a narrow manual_result wrapper');
    const f = fixture();
    mkdirSync(join(f.root, 'docs/scripts'), { recursive: true });
    writeFileSync(join(f.root, 'docs/scripts/manual-evidence.mjs'), readFileSync(helper));
    f.packet.source = sourceSnapshot(f.root); f.save();
    const rootPosix = f.root.replaceAll('\\', '/');
    const env = { ...process.env, EPOCHA_MANUAL_EVIDENCE: f.file };
    const run = () => spawnSync('bash', ['-c', 'set -euo pipefail\nROOT=' + JSON.stringify(rootPosix) + '\npass(){ printf "CLASS=PASS\\n"; }\nnotrun(){ printf "CLASS=NOT_RUN\\n"; }\nfail(){ printf "CLASS=FAIL\\n"; }\n' + match[0] + '\nrc=0\nmanual_result stage4a.human-readability fixture || rc=$?\nprintf "RC=%s\\n" "$rc"\n'], { env, encoding: 'utf8' });
    let result = run(); assert.equal(result.status, 0, result.stderr); assert.match(result.stdout, /CLASS=PASS/); assert.match(result.stdout, /RC=0/);
    f.packet.checks = []; f.save();
    result = run(); assert.equal(result.status, 0, result.stderr); assert.match(result.stdout, /CLASS=NOT_RUN/); assert.match(result.stdout, /RC=3/);
    writeFileSync(f.file, '{bad-json');
    result = run(); assert.equal(result.status, 0, result.stderr); assert.match(result.stdout, /CLASS=FAIL/); assert.match(result.stdout, /RC=1/);
  });
}
