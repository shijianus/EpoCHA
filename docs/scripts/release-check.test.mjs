import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync, statSync, chmodSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const read = (path) => readFileSync(join(ROOT, path), 'utf8');
const rules = () => import('./release-check.mjs');
const temporary = (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'epocha-engineering-test-'));
  t.after(() => rmSync(dir, { recursive: true, force: true })); // This invocation created the entire directory.
  return dir;
};

// Repository policy tests precede implementation changes. They do not claim to run Docker or a browser.
test('deployment recipes never overwrite keys or return to the obsolete stage-5b candidate', () => {
  const deployment = read('docs/DEPLOYMENT.md');
  const trial = read('docs/TRIAL-DEPLOYMENT.md');
  assert.doesNotMatch(deployment, /sudo tee/);
  assert.match(deployment, /bash deploy\/prepare-keys\.sh/);
  assert.doesNotMatch(trial, /git checkout stage-5b-done/);
  assert.match(trial, /APPROVED_OID/);
  assert.match(trial, /APPROVED_MANIFEST/);
  assert.match(trial, /尚未生成|尚无.*候选/);
});

test('README separates historical OCR measurements from runtime assets', () => {
  const text = read('README.md');
  assert.doesNotMatch(text, /新机会在启动时\s*自行装载/);
  assert.doesNotMatch(text, /本机没有离线 OCR/);
  assert.match(text, /历史/);
  assert.match(text, /96%.*31%.*3%/);
  assert.match(text, /production_ready:false/);
});

test('Docker context excludes development archives, workloads and release output', () => {
  const entries = read('.dockerignore').split(/\r?\n/).map((s) => s.trim());
  for (const required of ['epocha-offline-assets.tar.gz', '.zcode/', 'release-artifacts/', 'cmd/loadtest/', '.github/']) {
    assert.ok(entries.includes(required), `missing Docker exclusion: ${required}`);
  }
  assert.doesNotMatch(read('.gitignore'), /^epocha-offline-assets\.tar\.gz$/m, 'retain the tracked historical archive');
  for (const required of ['.zcode/', '/release-artifacts/']) {
    assert.ok(read('.gitignore').split(/\r?\n/).includes(required), `missing Git exclusion: ${required}`);
  }
});

test('Makefile uses fail-closed formatting, byte-exact size and lifecycle regressions', () => {
  const text = read('Makefile');
  assert.match(text, /node docs\/scripts\/release-check\.mjs --fmt-check/);
  assert.match(text, /node docs\/scripts\/release-check\.mjs --binary-size/);
  assert.doesNotMatch(text, /gofmt -l \. \| grep/);
  assert.doesNotMatch(text, /mb=\$\$\(\( size \/ 1024 \/ 1024 \)\)/);
  assert.match(text, /node --test[^\n]*docs\/scripts\/browser\/widget_lifecycle\.test\.mjs/);
});

test('CI pins the authorized Go release, Node 24 and does not swallow incomplete gates', () => {
  const text = read('.github/workflows/ci.yml');
  assert.match(text, /go-version:\s*['"]1\.27\.3['"]/);
  assert.match(text, /node-version:\s*['"]24(?:\.\d+\.\d+)?['"]/);
  assert.match(text, /ubuntu-24\.04-arm/);
  assert.match(text, /amd64/);
  assert.match(text, /--profile full/);
  assert.doesNotMatch(text, /continue-on-error:\s*true|\|\|\s*(?:true|exit 0)/);
});

test('admin enable acceptance uses POST rather than a GET-only helper', () => {
  const text = read('docs/scripts/admin-ui-check.sh');
  assert.match(text, /-X POST[\s\S]{0,240}\/enable/,
    'enable must be exercised with its write method');
  assert.doesNotMatch(text, /en="\$\(acode_of[\s\S]{0,180}\/enable/,
    'GET-only acode_of must not be used for enable');
});


test('Go skip classification keeps named future placeholders separately recorded while unknown skips fail', async () => {
  const { goTests, FUTURE_GO_PLACEHOLDERS } = await rules();
  assert.equal(FUTURE_GO_PLACEHOLDERS.length, 2);
  const future = JSON.stringify({ Package: FUTURE_GO_PLACEHOLDERS[0].package, Test: FUTURE_GO_PLACEHOLDERS[0].test, Action: 'skip' });
  const pass = JSON.stringify({ Package: FUTURE_GO_PLACEHOLDERS[0].package, Test: 'Implemented', Action: 'pass' });
  const named = goTests(0, `${future}\n${pass}\n`);
  assert.equal(named.status, 'PASS');
  assert.equal(named.unknown_skip.length, 0);
  assert.equal(named.future_requirements[0].status, 'NOT_IMPLEMENTED/NOT_RUN');
  const environmental = goTests(0, `${JSON.stringify({ Package: 'example/pkg', Test: 'ReadonlyDirectory', Action: 'skip' })}\n${pass}\n`);
  assert.equal(environmental.status, 'INCOMPLETE');
  assert.equal(environmental.environmental_skip.length, 1);
  const unknown = goTests(0, `${JSON.stringify({ Package: 'example/pkg', Test: 'UnexpectedSkip', Action: 'skip' })}\n${pass}\n`);
  assert.equal(unknown.status, 'FAIL');
  assert.equal(unknown.unknown_skip.length, 1);
});
test('stage5a gates restore authentication, strict load counts, image bytes, exact image files and 413', () => {
  const text = read('docs/scripts/stage5a.sh');
  assert.match(text, /EPOCHA_ADMIN_KEY="\$BACKUP_ADMIN"[\s\S]{0,220}whoami/,
    'restore gate must exercise the old key through the management HTTP path');
  assert.match(text, /--requests 150[\s\S]{0,260}(flows=|ok=|loadtest)/,
    'load gate must require all planned flows to succeed');
  assert.match(text, /POOL_IMG[^\n]*> 0|POOL_IMG[^\n]*-gt 0|asset_bytes[^\n]*[1-9]/,
    'image pool gate must require nonzero image bytes');
  assert.match(text, /BIGCODE.*413[\s\S]{0,260}fail/,
    'a proxy body-limit miss must be a failure');
  assert.match(text, /EXPECTED_(?:IMAGE_)?FILES|exact.*(?:file|path)|预期.*文件清单/i,
    'image inventory must use an exact whitelist');
});
test('browser manifest helper imports the repository manifest and rejects missing/unknown cases', async () => {
  const { checkBrowserManifest } = await rules();
  const manifest = 'docs/scripts/browser/cases.mjs';
  const suite = 'docs/scripts/browser/adminui_browser.test.mjs';
  const okTitles = [
    '错误 key 登录失败：显示凭据无效、留在登录面、输入框不留原文',
    '正确 key 登录：显示非敏感 key_id，凭据不进任何浏览器存储',
    '创建站点：真实 201、一次性 secret 出现在结果面板、验证模式与题型按选择生效',
    '进入详情不再取回 secret：页面与只读 API 两侧都扫过',
    '修改策略与 Origin：保存、重新读取一致，越界值被服务端拒',
    '停用与启用：确认框展示名称与 site_key，状态与版本是服务端事实',
    '题型面板显示实验性与生产就绪的真实结论，页面不请求任何外部源',
    '退出：页面回到登录面且无法再读管理数据（凭据是页面丢的，不是服务端失效）',
    '375×667：视口真的设成 375，登录、创建与保存控件都能命中',
    '连续错误 key 触发 429 时不说成密码错误；服务真不可达时说明是连接问题',
  ];
  const tap = `TAP version 13\n${okTitles.map((title, i) => `ok ${i + 1} - ${title}`).join('\n')}\n1..10\n# tests 10\n# pass 10\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n`;
  assert.equal(checkBrowserManifest(manifest, suite, tap).status, 'PASS');
  assert.throws(() => checkBrowserManifest(manifest, suite, tap.replace(okTitles[0], '删除后的用例')));
  assert.throws(() => checkBrowserManifest(manifest, suite, tap.replace(okTitles[0], `${okTitles[0]}\nok 11 - 未登记用例`)));
});
test('repository attributes force LF even under core.autocrlf=true and preserve binary bytes', (t) => {
  const attributes = read('.gitattributes');
  assert.match(attributes, /^\*\s+text=auto\s+eol=lf$/m);
  const dir = temporary(t);
  const git = (...args) => {
    const result = spawnSync('git', ['-c', 'core.autocrlf=true', '-C', dir, ...args], { encoding: 'utf8' });
    assert.equal(result.status, 0, result.stderr || result.error?.message);
    return result.stdout;
  };
  git('init', '--quiet');
  writeFileSync(join(dir, '.gitattributes'), attributes);
  const source = Buffer.from('line one\nline two\n');
  const binary = Buffer.from([0, 13, 10, 255, 13, 10, 1]);
  for (const name of ['a.go', 'a.mjs', 'a.sh', 'Makefile']) writeFileSync(join(dir, name), source);
  for (const name of ['a.png', 'a.ttf', 'a.tar.gz', 'a.zip']) writeFileSync(join(dir, name), binary);
  git('add', '.');
  const out = join(dir, 'checkout');
  mkdirSync(out);
  git('checkout-index', '--all', `--prefix=${out.replaceAll('\\', '/')}/`);
  for (const name of ['a.go', 'a.mjs', 'a.sh', 'Makefile']) assert.deepEqual(readFileSync(join(out, name)), source);
  for (const name of ['a.png', 'a.ttf', 'a.tar.gz', 'a.zip']) assert.deepEqual(readFileSync(join(out, name)), binary);
});

test('binary budget compares bytes, including the first byte above 35 MiB', async () => {
  const { checkBinaryBytes, MAX_BINARY_BYTES } = await rules();
  assert.equal(MAX_BINARY_BYTES, 35 * 1024 * 1024);
  assert.doesNotThrow(() => checkBinaryBytes(MAX_BINARY_BYTES));
  for (const value of [MAX_BINARY_BYTES + 1, MAX_BINARY_BYTES + 1024 * 1024 - 1, 0, -1, NaN, '1', 1.5]) {
    assert.throws(() => checkBinaryBytes(value), undefined, `must reject ${value}`);
  }
});

test('formatter cannot pass on empty input, execution failure or syntax-error empty stdout', async () => {
  const { checkFormatting } = await rules();
  assert.throws(() => checkFormatting([], () => ({ status: 0, stdout: '' })));
  for (const result of [
    { status: 2, stdout: '', stderr: 'syntax error' },
    { status: null, stdout: '', error: new Error('missing gofmt') },
    { status: 0, stdout: 'internal/example.go\n' },
  ]) assert.throws(() => checkFormatting(['internal/example.go'], () => result));
  assert.equal(checkFormatting(['internal/example.go'], () => ({ status: 0, stdout: '' })), 1);
});

test('a zero-exit stage4b with missing OCR is INCOMPLETE rather than PASS', async () => {
  const { classifyLegacyGate } = await rules();
  const result = classifyLegacyGate('stage4b', 0, '[stage4b] 工程项 8 项 PASS；FAIL 0；BLOCKED 2；NOT_RUN 0\n');
  assert.equal(result.status, 'INCOMPLETE');
  assert.equal(result.counts.blocked, 2);
});

test('admin-ui exit 3 for manual gaps stays incomplete even with all engineering tests passing', async () => {
  const { classifyLegacyGate } = await rules();
  const result = classifyLegacyGate('admin-ui', 3, '[adminui] RESULT=INCOMPLETE pass=12 fail=0 blocked=0 not_run=3\n');
  assert.equal(result.status, 'INCOMPLETE');
  assert.equal(result.raw_exit_code, 3);
});

test('stage5b optional/out-of-scope results never change required counts', async () => {
  const { classifyLegacyGate } = await rules();
  const log = '[stage5b] 必需项(B 路径部署工程) PASS=37 FAIL=0 BLOCKED=0 NOT_RUN=0\n' +
    '[stage5b] 可选项(A 路径/未验证平台) PASS=0 FAIL=1 BLOCKED=2 NOT_RUN=3\n' +
    '[stage5b] 超范围项(产品效果，本门禁不判定) 登记=3\n[stage5b] RESULT=pass（必需项全部通过）\n[stage5b] SCRIPT_RC=0\n';
  const result = classifyLegacyGate('stage5b', 0, log);
  assert.equal(result.status, 'PASS');
  assert.equal(result.optional.fail, 1);
  assert.equal(result.optional.not_run, 3);
});

test('legacy parser rejects missing/empty summaries, contradictory exit codes and hidden failures', async () => {
  const { classifyLegacyGate } = await rules();
  for (const [gate, rc, log] of [
    ['stage4b', 0, ''],
    ['stage4b', 0, '[stage4b] 工程项 0 项 PASS；FAIL 0；BLOCKED 0；NOT_RUN 0'],
    ['stage4b', 0, '[stage4b] 工程项 3 项 PASS；FAIL 1；BLOCKED 0；NOT_RUN 0'],
    ['admin-ui', 1, '[adminui] RESULT=PASS pass=4 fail=0 blocked=0 not_run=0'],
    ['stage5a', 0, '[stage5a] 工程项 PASS=2 FAIL=0；外部条件 BLOCKED=0；本轮未执行 NOT_RUN=0\n[stage5a] RESULT=fail'],
    ['stage5b', 0, '[stage5b] 必需项(B 路径部署工程) PASS=1 FAIL=0 BLOCKED=0 NOT_RUN=0\n[stage5b] SCRIPT_RC=3'],
  ]) assert.equal(classifyLegacyGate(gate, rc, log).status, 'FAIL', `${gate}: ${log}`);
  const patterns = {
    stage4b: '[stage4b] 工程项 4 项 PASS；FAIL 0；BLOCKED 0；NOT_RUN 0\n',
    stage5a: '[stage5a] 工程项 PASS=4 FAIL=0；外部条件 BLOCKED=0；本轮未执行 NOT_RUN=0\n[stage5a] RESULT=pass（4 项全部通过）\n',
    'admin-ui': '[adminui] RESULT=PASS pass=4 fail=0 blocked=0 not_run=0\n',
  };
  for (const [gate, text] of Object.entries(patterns)) assert.equal(classifyLegacyGate(gate, 0, text).status, 'PASS');
  assert.equal(classifyLegacyGate('stage5a', 3, '[stage5a] 工程项 PASS=4 FAIL=0；外部条件 BLOCKED=1；本轮未执行 NOT_RUN=0\n[stage5a] RESULT=blocked\n').status, 'INCOMPLETE');
  assert.equal(classifyLegacyGate('admin-ui', 3, '[adminui] RESULT=PASS pass=4 fail=0 blocked=0 not_run=0').status, 'INCOMPLETE');
});

test('required missing checks yield exit 3, failures dominate, and no evidence can never yield PASS', async () => {
  const { summarizeChecks } = await rules();
  const pass = { id: 'test', required: true, status: 'PASS', executed: true, raw_exit_code: 0 };
  const required = ['test', 'missing', 'failure'];
  assert.equal(summarizeChecks([pass], ['test']).exit_code, 0);
  for (const status of ['BLOCKED', 'NOT_RUN', 'INCOMPLETE']) {
    assert.equal(summarizeChecks([pass, { id: 'missing', required: true, status, executed: false }], ['test', 'missing']).exit_code, 3);
  }
  assert.equal(summarizeChecks([pass, { id: 'failure', required: true, status: 'FAIL' }, { id: 'missing', required: true, status: 'BLOCKED' }], required).exit_code, 1);
  assert.equal(summarizeChecks([], ['test']).exit_code, 1);
  assert.equal(summarizeChecks([{ ...pass, executed: false }], ['test']).exit_code, 1);
  assert.equal(summarizeChecks([{ ...pass, raw_exit_code: 3 }], ['test']).exit_code, 1);
  assert.equal(summarizeChecks([pass, { ...pass }], ['test']).exit_code, 1);
  assert.equal(summarizeChecks([pass, { id: 'effect', required: false, status: 'NOT_RUN' }], ['test']).exit_code, 0);
});

test('required check manifest rejects a single PASS or missing IDs', async () => {
  const { summarizeChecks } = await rules();
  const pass = { id: 'source-identity', required: true, status: 'PASS', executed: true, raw_exit_code: 0 };
  assert.equal(summarizeChecks([pass], ['source-identity']).exit_code, 0);
  const missing = summarizeChecks([pass], ['source-identity', 'go-test']);
  assert.equal(missing.exit_code, 1);
  assert.match(missing.reason, /manifest mismatch/);
});

test('production approval is independent from green engineering evidence and binds candidate evidence', async () => {
  const { summarizeChecks, evaluateProductionApproval, productionAdmissionRequirements } = await rules();
  const required = ['engineering-a', 'engineering-b'];
  const checks = required.map((id) => ({ id, required: true, status: 'PASS', executed: true, raw_exit_code: 0 }));
  const engineering = summarizeChecks(checks, required);
  assert.equal(engineering.result, 'PASS');
  assert.equal(engineering.exit_code, 0);

  const candidate = {
    supported_scope: 'B / linux/arm64 / UID:GID 1000:1000',
    source_oid: 'a'.repeat(40),
    source_manifest_sha256: 'b'.repeat(64),
    artifacts: { binary_sha256: 'c'.repeat(64), image_digest: `sha256:${'d'.repeat(64)}` },
  };
  const binding = evaluateProductionApproval(candidate, []).candidate_fingerprint_sha256;
  const admissions = productionAdmissionRequirements.map((id) => ({
    id,
    status: 'PASS',
    source_oid: candidate.source_oid,
    candidate_fingerprint_sha256: binding,
    evidence_sha256: 'e'.repeat(64),
  }));
  admissions.find((item) => item.id === 'captcha-effectiveness').status = 'FAIL';
  admissions.find((item) => item.id === 'assistive-technology').status = 'NOT_RUN';
  admissions.find((item) => item.id === 'persistence-policy').status = 'BLOCKED';

  const approval = evaluateProductionApproval(candidate, admissions);
  assert.equal(approval.approved, false);
  assert.notEqual(approval.status, 'PASS');
  assert.equal(approval.admissions.find((item) => item.id === 'captcha-effectiveness').status, 'FAIL');
  assert.equal(approval.admissions.find((item) => item.id === 'assistive-technology').status, 'NOT_RUN');
  assert.equal(approval.admissions.find((item) => item.id === 'persistence-policy').status, 'BLOCKED');
  assert.equal(evaluateProductionApproval({ ...candidate, artifacts: {} }, admissions).approved, false);
  assert.equal(evaluateProductionApproval(candidate, admissions.map((item) => ({ ...item, source_oid: 'f'.repeat(40) })).filter((item) => item.id !== 'real-device')).approved, false);
});

test('stage4b isolation verdict rejects nonzero execution, missing checkpoint and incomplete mount assertions', async () => {
  const { classifyIsolationRun } = await import('./stage4b-isolation.mjs');
  const complete = {
    status: 0,
    stdout: 'ISOLATION_CHECKPOINT=mounts-verified\nNETWORK_CHECKPOINT=dns-blocked\n',
    stderr: '',
  };
  assert.equal(classifyIsolationRun(complete).status, 'PASS');
  for (const evidence of [
    { ...complete, status: 125 },
    { ...complete, stdout: '' },
    { ...complete, stdout: 'ISOLATION_CHECKPOINT=mounts-verified\n' },
    { ...complete, stdout: complete.stdout + 'GROUNDTRUTH_VISIBLE=1\n' },
    { ...complete, stdout: complete.stdout + 'ANSWER_VISIBLE=1\n' },
    { ...complete, stdout: complete.stdout + 'DNS_REACHABLE\n' },
  ]) assert.equal(classifyIsolationRun(evidence).status, 'FAIL');
});

test('Node TAP verification rejects empty output, zero tests and skipped tests', async () => {
  const { classifyTAP } = await rules();
  assert.equal(classifyTAP(0, '').status, 'FAIL');
  assert.equal(classifyTAP(0, '# tests 0\n# pass 0\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n').status, 'FAIL');
  assert.equal(classifyTAP(0, '# tests 2\n# pass 1\n# fail 0\n# cancelled 0\n# skipped 1\n# todo 0\n').status, 'INCOMPLETE');
  assert.equal(classifyTAP(0, '# tests 1\n# pass 1\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n').status, 'PASS');
});

test('full plan retains all four strongest existing gates, without a force-pass switch', async () => {
  const { LEGACY_GATES, cleanEnvironment, LINUX_CROSS_VET_ENV } = await rules();
  assert.deepEqual(LEGACY_GATES.map((gate) => gate.id), ['stage4b', 'stage5a', 'stage5b', 'admin-ui']);
  assert.deepEqual(LEGACY_GATES.map((gate) => gate.script), [
    'docs/scripts/stage4b.sh', 'docs/scripts/stage5a.sh', 'docs/scripts/stage5b.sh', 'docs/scripts/admin-ui-check.sh',
  ]);
  const env = cleanEnvironment({ PATH: '/tools', DOCKER_HOST: 'unix:///test.sock', GOTOOLCHAIN: 'auto', NODE_OPTIONS: '--require fake', EPOCHA_STAGE4B_SKIP_OCR: '1', EPOCHA_ASSUME_PASS: '1', GOFLAGS: '-race' });
  assert.equal(env.GOTOOLCHAIN, 'local');
  assert.equal(env.GOFLAGS, '-mod=readonly');
  assert.equal(env.DOCKER_HOST, 'unix:///test.sock');
  assert.equal(env.PATH, '/tools');
  assert.equal(env.NODE_OPTIONS, undefined);
  assert.equal(Object.keys(env).some((key) => key.startsWith('EPOCHA_')), false);
  assert.deepEqual(LINUX_CROSS_VET_ENV, { GOOS: 'linux', GOARCH: 'amd64', CGO_ENABLED: '0' });
});

test('Docker acceptance refuses an unmeasured or nonempty daemon instead of deleting existing assets', async () => {
  const { assertDisposableDocker } = await rules();
  const empty = { containers: [], images: [], volumes: [], networks: ['bridge', 'host', 'none'] };
  assert.doesNotThrow(() => assertDisposableDocker(empty));
  for (const kind of ['containers', 'images', 'volumes', 'networks']) {
    assert.throws(() => assertDisposableDocker({ ...empty, [kind]: [...empty[kind], 'pre-existing'] }));
  }
  assert.throws(() => assertDisposableDocker({ containers: [], images: [] }));
  assert.throws(() => assertDisposableDocker({ ...empty, networks: [] }));
});

if (process.platform === 'linux' && process.getuid() !== 0) {
  test('Linux key provisioning creates private same-UID files and preserves bytes on rerun', (t) => {
    const dir = join(temporary(t), 'keys');
    mkdirSync(dir, { mode: 0o700 });
    const run = () => spawnSync('bash', [join(ROOT, 'deploy/prepare-keys.sh'), dir], { encoding: 'utf8' });
    const first = run();
    assert.equal(first.status, 0, first.stderr);
    const before = ['master', 'pepper', 'admin'].map((name) => readFileSync(join(dir, name)));
    for (const name of ['master', 'pepper', 'admin']) {
      const stat = statSync(join(dir, name));
      assert.equal(stat.uid, process.getuid());
      assert.equal(stat.gid, process.getgid());
      assert.equal(stat.mode & 0o777, 0o600);
    }
    assert.equal(run().status, 0);
    assert.deepEqual(['master', 'pepper', 'admin'].map((name) => readFileSync(join(dir, name))), before);
    for (const secret of before) assert.equal(first.stdout.includes(secret.toString()), false);
    chmodSync(join(dir, 'master'), 0o644);
    assert.equal(run().status, 1, 'wide permissions must fail without changing the key');
    assert.deepEqual(readFileSync(join(dir, 'master')), before[0]);
    chmodSync(join(dir, 'master'), 0o600);
    const target = join(temporary(t), 'unchanged-secret');
    writeFileSync(target, 'private-value', { mode: 0o600 });
    const linked = join(temporary(t), 'keys');
    mkdirSync(linked, { mode: 0o700 });
    symlinkSync(target, join(linked, 'master'));
    const rejected = spawnSync('bash', [join(ROOT, 'deploy/prepare-keys.sh'), linked], { encoding: 'utf8' });
    assert.equal(rejected.status, 1, 'provisioning must reject a symlink rather than overwrite its target');
    assert.equal(readFileSync(target, 'utf8'), 'private-value');
  });
} else {
  test('key provisioning explicitly blocks unsupported native Windows or root rather than claiming permission verification', () => {
    const result = spawnSync('bash', [join(ROOT, 'deploy/prepare-keys.sh'), '/unused'], { encoding: 'utf8' });
    assert.equal(result.status, 3, result.stderr || result.error?.message);
    assert.match(result.stderr, /BLOCKED/);
  });
}
