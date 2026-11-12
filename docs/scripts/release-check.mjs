#!/usr/bin/env node
// Development/release tooling only. No runtime imports, npm packages, or pass overrides.
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { join, resolve, basename, relative, isAbsolute } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { spawnSync } from 'node:child_process';
import { gzipSync } from 'node:zlib';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
export const GO_VERSION = 'go1.27.3';
export const MAX_BINARY_BYTES = 35 * 1024 * 1024;
export const LEGACY_GATES = Object.freeze([
  { id: 'stage4b', script: 'docs/scripts/stage4b.sh' },
  { id: 'stage5a', script: 'docs/scripts/stage5a.sh' },
  { id: 'stage5b', script: 'docs/scripts/stage5b.sh' },
  { id: 'admin-ui', script: 'docs/scripts/admin-ui-check.sh' },
]);
export const FUTURE_GO_PLACEHOLDERS = Object.freeze([
  {
    id: 'future.stats-sort-whitelist',
    package: 'github.com/shijianus/EpoCHA/internal/admin',
    test: 'TestStatsSortWhitelist',
    reason: '阶段 5 统计排序白名单尚未实现',
  },
  {
    id: 'future.weight-auto-demotion',
    package: 'github.com/shijianus/EpoCHA/internal/admin',
    test: 'TestWeightAutoDemotionRule',
    reason: '阶段 5 分级淘汰尚未实现',
  },
]);
const FUTURE_GO_BY_KEY = new Map(FUTURE_GO_PLACEHOLDERS.map((item) => [`${item.package}\0${item.test}`, item]));
const hash = (bytes) => createHash('sha256').update(bytes).digest('hex');
const fail = (reason) => ({ status: 'FAIL', reason });

export function checkBinaryBytes(size) {
  if (!Number.isSafeInteger(size) || size <= 0 || size > MAX_BINARY_BYTES) {
    throw new Error(`binary size ${size} B is outside 1..${MAX_BINARY_BYTES} B (35 MiB)`);
  }
  return size;
}

export function checkFormatting(files, run) {
  if (!Array.isArray(files) || files.length === 0) throw new Error('no Go source files found');
  for (let start = 0; start < files.length; start += 64) {
    const result = run(files.slice(start, start + 64));
    if (result.error || result.signal || result.status !== 0) throw new Error(`gofmt failed: ${result.stderr || result.error?.message || result.status}`);
    if (typeof result.stdout !== 'string') throw new Error('gofmt output missing');
    if (result.stdout.trim()) throw new Error(`unformatted Go files:\n${result.stdout.trim()}`);
  }
  return files.length;
}

export function cleanEnvironment(input) {
  const env = { ...input };
  for (const key of Object.keys(env)) {
    if (/^(EPOCHA_|BIDI_|ADMIN_|BAD_ADMIN_|EXAMPLE_)/i.test(key) ||
        /^(NODE_OPTIONS|NODE_TEST_CONTEXT|BASH_ENV|ENV|SHELLOPTS|MAKEFLAGS|MFLAGS|GOOS|GOARCH|CGO_ENABLED|GOFLAGS|GOTOOLCHAIN|GOWORK|GOENV)$/i.test(key)) delete env[key];
  }
  const manualEvidence = input.EPOCHA_MANUAL_EVIDENCE;
  if (typeof manualEvidence === 'string' && manualEvidence) env.EPOCHA_MANUAL_EVIDENCE = resolve(manualEvidence);
  return { ...env, GOTOOLCHAIN: 'local', GOFLAGS: '-mod=readonly', GOPROXY: 'off', GOSUMDB: 'off', GOWORK: 'off', GOENV: 'off', LC_ALL: 'C.UTF-8' };
}

function counts(match) {
  return Object.fromEntries(['pass', 'fail', 'blocked', 'not_run'].map((key, i) => [key, Number(match[i + 1])]));
}

export function classifyLegacyGate(id, rc, text) {
  const patterns = {
    stage4b: /^\[stage4b\] 工程项 (\d+) 项 PASS；FAIL (\d+)；BLOCKED (\d+)；NOT_RUN (\d+)$/gm,
    stage5a: /^\[stage5a\] 工程项 PASS=(\d+) FAIL=(\d+)；外部条件 BLOCKED=(\d+)；本轮未执行 NOT_RUN=(\d+)$/gm,
    stage5b: /^\[stage5b\] 必需项\(B 路径部署工程\) PASS=(\d+) FAIL=(\d+) BLOCKED=(\d+) NOT_RUN=(\d+)$/gm,
    'admin-ui': /^\[adminui\] RESULT=(?:PASS|FAIL|INCOMPLETE) pass=(\d+) fail=(\d+) blocked=(\d+) not_run=(\d+)$/gm,
  };
  if (!patterns[id]) return fail(`unknown gate ${id}`);
  const normalized = text.replaceAll('\r\n', '\n');
  const matches = [...normalized.matchAll(patterns[id])];
  if (matches.length !== 1) return { ...fail('missing or ambiguous nonempty gate summary'), raw_exit_code: rc };
  const required = counts(matches[0]);
  const result = { counts: required, raw_exit_code: rc };
  if (Object.values(required).some((n) => !Number.isSafeInteger(n)) || Object.values(required).reduce((a, b) => a + b, 0) === 0) {
    return { ...result, ...fail('zero or invalid gate evidence') };
  }
  if (id === 'stage5b') {
    const optional = [...normalized.matchAll(/^\[stage5b\] 可选项[^\n]* PASS=(\d+) FAIL=(\d+) BLOCKED=(\d+) NOT_RUN=(\d+)$/gm)];
    if (optional.length !== 1) return { ...result, ...fail('optional scope summary missing') };
    result.optional = counts(optional[0]);
    const codes = [...normalized.matchAll(/^\[stage5b\] SCRIPT_RC=(\d+)$/gm)];
    if (codes.length !== 1 || Number(codes[0][1]) !== rc) return { ...result, ...fail('SCRIPT_RC does not match actual process exit') };
  }
  if (id !== 'stage4b') {
    const prefix = id === 'admin-ui' ? 'adminui' : id;
    const verdicts = [...normalized.matchAll(new RegExp(`^\\[${prefix}\\] RESULT=(pass|fail|blocked|incomplete)(?=[^a-z]|$)`, 'gmi'))];
    if (verdicts.length !== 1) return { ...result, ...fail('RESULT missing or ambiguous') };
    if (verdicts[0][1].toLowerCase() === 'fail') return { ...result, ...fail('gate reports FAIL') };
    result.reported_result = verdicts[0][1].toUpperCase();
  }
  if (![0, 3].includes(rc) || required.fail > 0) return { ...result, ...fail('gate failed; nonzero exit is not discarded') };
  const prefix = id === 'admin-ui' ? 'adminui' : id;
  if (new RegExp(`^\\[${prefix}\\] FAIL(?: |:)`, 'm').test(normalized)) return { ...result, ...fail('failure record contradicts aggregate') };
  if (rc === 3 || required.blocked > 0 || required.not_run > 0 || ['BLOCKED', 'INCOMPLETE'].includes(result.reported_result)) {
    return { ...result, status: 'INCOMPLETE', reason: 'required external/manual or unexecuted conditions remain; raw exit retained' };
  }
  if (required.pass === 0) return { ...result, ...fail('no passing required evidence') };
  return { ...result, status: 'PASS' };
}

export function classifyTAP(rc, text) {
  const values = {};
  for (const key of ['tests', 'pass', 'fail', 'cancelled', 'skipped', 'todo']) {
    const match = [...text.matchAll(new RegExp(`^# ${key} (\\d+)\\r?$`, 'gm'))];
    if (match.length !== 1) return fail(`TAP ${key} count missing or ambiguous`);
    values[key] = Number(match[0][1]);
  }
  if (rc !== 0 || values.tests === 0 || values.fail > 0 || values.cancelled > 0) return { ...fail('Node suite failed or ran no tests'), counts: values };
  if (values.skipped > 0 || values.todo > 0) return { status: 'INCOMPLETE', counts: values, reason: 'skipped/todo tests are not PASS' };
  if (values.pass !== values.tests) return fail('TAP count mismatch');
  return { status: 'PASS', counts: values };
}

const ENGINEERING_CHECK_IDS = Object.freeze([
  'source-identity', 'go-package-inventory', 'node24', 'linux-host', 'key-permission-and-rerun',
  'go-toolchain', 'gnu-make', 'bash', 'race-compiler', 'engineering-rules', 'widget-regressions',
  'adminui-regressions', 'tooling-regressions', 'javascript-syntax', 'widget-size', 'shell-syntax',
  'fmt-check', 'diff-check', 'go-vet', 'go-test', 'go-race', 'static-build', 'binary-size',
  'secret-scan', 'source-stability',
]);
export const REQUIRED_CHECK_IDS = Object.freeze([
  ...ENGINEERING_CHECK_IDS, 'release-platform', 'candidate-clean', 'gate-tools', 'dedicated-docker',
  'ocr-container', 'stage4b', 'stage5a', 'stage5b', 'admin-ui',
  'manual-stage4a.human-readability', 'manual-stage4b.human-readability',
  'manual-adminui.http-errors', 'manual-adminui.clipboard-write', 'manual-adminui.real-mobile',
]);
export function requiredCheckIDs(profile) {
  if (profile === 'engineering') return ENGINEERING_CHECK_IDS;
  if (profile === 'full') return REQUIRED_CHECK_IDS;
  throw new Error('unknown check profile');
}

export function summarizeChecks(checks, requiredIDs = REQUIRED_CHECK_IDS) {
  const seen = new Set();
  if (!checks.length) return { result: 'FAIL', exit_code: 1, reason: 'empty check plan' };
  for (const item of checks) {
    if (!item.id || seen.has(item.id) || !['PASS', 'FAIL', 'BLOCKED', 'NOT_RUN', 'INCOMPLETE'].includes(item.status) ||
        (item.status === 'PASS' && (!item.executed || item.raw_exit_code !== 0))) {
      return { result: 'FAIL', exit_code: 1, reason: 'invalid, duplicate or unexecuted PASS evidence' };
    }
    seen.add(item.id);
  }
  const required = checks.filter((item) => item.required);
  const actualRequiredIDs = new Set(required.map((item) => item.id));
  const missingRequiredIDs = requiredIDs.filter((id) => !actualRequiredIDs.has(id));
  const unexpectedRequiredIDs = required.filter((item) => !requiredIDs.includes(item.id)).map((item) => item.id);
  if (missingRequiredIDs.length || unexpectedRequiredIDs.length) return { result: 'FAIL', exit_code: 1, reason: 'required check ID manifest mismatch', missing_required_ids: missingRequiredIDs, unexpected_required_ids: unexpectedRequiredIDs };
  if (!required.length || required.some((item) => item.status === 'FAIL')) return { result: 'FAIL', exit_code: 1 };
  if (required.some((item) => item.status !== 'PASS')) return { result: 'INCOMPLETE', exit_code: 3 };
  return { result: 'PASS', exit_code: 0 };
}

export function assertDisposableDocker(inventory) {
  for (const key of ['containers', 'images', 'volumes', 'networks']) {
    if (!Array.isArray(inventory[key])) throw new Error(`Docker ${key} inventory unavailable`);
  }
  if (inventory.containers.length || inventory.images.length || inventory.volumes.length ||
      inventory.networks.length !== 3 || ['bridge', 'host', 'none'].some((name) => !inventory.networks.includes(name))) {
    throw new Error('full acceptance needs an empty, dedicated local Docker daemon; existing assets will not be removed');
  }
}

function importedBrowserCases(manifestPath) {
  const script = `import { BROWSER_CASES } from ${JSON.stringify(pathToFileURL(resolve(manifestPath)).href)}; process.stdout.write(JSON.stringify(BROWSER_CASES));`;
  const result = spawnSync(process.execPath, ['--input-type=module', '-e', script], { cwd: ROOT, encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 });
  if (result.error || result.status !== 0) throw new Error(`browser manifest import failed: ${result.stderr || result.error?.message || result.status}`);
  let cases;
  try { cases = JSON.parse(result.stdout); } catch { throw new Error('browser manifest import returned invalid JSON'); }
  if (!Array.isArray(cases) || cases.length === 0) throw new Error('browser manifest has no cases');
  for (const item of cases) {
    if (!item || typeof item.id !== 'string' || typeof item.title !== 'string' || typeof item.file !== 'string' || typeof item.required !== 'boolean') {
      throw new Error('browser manifest contains an incomplete case record');
    }
  }
  return cases;
}

export function checkBrowserManifest(manifestPath, suitePath, tapText) {
  if (!manifestPath || !suitePath || typeof tapText !== 'string' || !tapText.trim()) throw new Error('browser manifest, suite and nonempty TAP are required');
  const cases = importedBrowserCases(manifestPath);
  const suite = readFileSync(resolve(suitePath), 'utf8');
  const ids = cases.filter((item) => item.required && item.file === basename(suitePath));
  if (!ids.length) throw new Error(`browser manifest has no required cases for ${basename(suitePath)}`);
  if (new Set(cases.map((item) => item.id)).size !== cases.length) throw new Error('browser manifest has duplicate IDs');
  if (new Set(ids.map((item) => item.title)).size !== ids.length) throw new Error('browser manifest has duplicate titles');
  const registeredIDs = [...suite.matchAll(/caseTitle\(["']([^"']+)["']\)/g)].map((m) => m[1]);
  for (const id of registeredIDs) {
    const entry = cases.find((item) => item.id === id);
    if (!entry || entry.file !== basename(suitePath)) throw new Error(`browser suite references unknown or wrong-file ID: ${id}`);
  }
  if (!registeredIDs.length && !/\btest\s*\(/.test(suite)) throw new Error('browser suite has no test registration');
  const normalized = tapText.replaceAll('\r\n', '\n');
  const counts = {};
  for (const name of ['tests', 'pass', 'fail', 'skipped', 'todo', 'cancelled']) {
    const matches = [...normalized.matchAll(new RegExp(`^# ${name} (\\d+)$`, 'gm'))];
    if (matches.length !== 1) throw new Error(`TAP ${name} summary missing or duplicated`);
    counts[name] = Number(matches[0][1]);
  }
  const entries = [...normalized.matchAll(/^(ok|not ok) (\d+) - (.+)$/gm)].map((m) => {
    const directive = /\s+#\s+(SKIP|TODO)(?:\s|$)/i.exec(m[3]);
    return { ok: m[1] === 'ok', number: Number(m[2]), title: (directive ? m[3].slice(0, directive.index) : m[3]).trim(), directive: directive?.[1].toUpperCase() };
  });
  const expectedTitles = ids.map((item) => item.title);
  const titles = entries.map((item) => item.title);
  const missing = expectedTitles.filter((title) => !titles.includes(title));
  const unknown = titles.filter((title) => !expectedTitles.includes(title));
  if (missing.length || unknown.length || new Set(titles).size !== titles.length || entries.length !== counts.tests ||
      entries.length !== expectedTitles.length || entries.some((entry, index) => entry.number !== index + 1) ||
      counts.tests !== counts.pass + counts.fail + counts.skipped + counts.todo + counts.cancelled) {
    throw new Error(`browser manifest mismatch: missing=${missing.length} unknown=${unknown.length} tests=${counts.tests} manifest=${ids.length}`);
  }
  const skipped = entries.filter((entry) => entry.directive === 'SKIP').length;
  const todo = entries.filter((entry) => entry.directive === 'TODO').length;
  const passed = entries.filter((entry) => entry.ok && !entry.directive).length;
  if (skipped !== counts.skipped || todo !== counts.todo || passed !== counts.pass || counts.fail > 0 ||
      entries.some((entry) => !entry.ok && !entry.directive) && counts.cancelled === 0) throw new Error('TAP results contradict counts or contain failed cases');
  const incomplete = counts.skipped + counts.todo + counts.cancelled > 0;
  return { status: incomplete ? 'INCOMPLETE' : 'PASS', manifest_cases: ids.length, ...counts, missing, unknown };
}
function filesBelow(relative) {
  const dir = join(ROOT, relative);
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = `${relative}/${entry.name}`;
    if (entry.isSymbolicLink()) return [];
    return entry.isDirectory() ? filesBelow(path) : [path];
  });
}

function command(cmd, args, options = {}) {
  return spawnSync(cmd, args, { cwd: ROOT, env: cleanEnvironment(process.env), encoding: 'utf8', maxBuffer: 64 * 1024 * 1024, timeout: 30 * 60 * 1000, ...options });
}
function must(cmd, args, options) {
  const result = command(cmd, args, options);
  if (result.error || result.signal || result.status !== 0) throw new Error(`${cmd} failed: ${result.stderr || result.error?.message || result.status}`);
  return result.stdout;
}
function fmt() {
  const files = ['cmd', 'internal', 'web'].flatMap(filesBelow).filter((name) => name.endsWith('.go'));
  const n = checkFormatting(files, (batch) => command('gofmt', ['-l', ...batch]));
  return `gofmt checked ${n} actual Go files`;
}
function binarySize(path) {
  const stat = statSync(path);
  if (!stat.isFile()) throw new Error('binary is not a regular file');
  checkBinaryBytes(stat.size);
  return `${path}: ${stat.size} B <= ${MAX_BINARY_BYTES} B (35 MiB)`;
}
function widgetSize() {
  const js = readFileSync(join(ROOT, 'web/widget/v1.js'));
  const worker = readFileSync(join(ROOT, 'web/widget/v1.worker.js'));
  const interactive = readFileSync(join(ROOT, 'web/widget/v1.interactive.js'));
  if (!js.length || !worker.length || !interactive.length) throw new Error('empty widget asset');
  const size = gzipSync(js, { level: 9 }).length;
  const isize = gzipSync(interactive, { level: 9 }).length;
  if (size >= 30 * 1024) throw new Error(`widget gzip ${size} B must be < 30720 B`);
  // ADR-025 第 5 条：交互模块按需加载，核心与模块各自守体积；交互题型一次加载两文件。
  if (isize >= 30 * 1024) throw new Error(`interactive module gzip ${isize} B must be < 30720 B`);
  if (size + isize >= 40 * 1024) throw new Error(`core+interactive gzip ${size + isize} B must be < 40960 B`);
  return `widget ${js.length} B; gzip ${size} B < 30720 B; interactive ${interactive.length} B; gzip ${isize} B < 30720 B; ` +
    `core+interactive gzip ${size + isize} B < 40960 B; main+worker gzip ${gzipSync(Buffer.concat([js, worker]), { level: 9 }).length} B`;
}
function sourceIdentity() {
  const commit = must('git', ['rev-parse', 'HEAD']).trim();
  if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error('full source OID unavailable');
  const status = must('git', ['status', '--porcelain=v1', '-z']);
  const files = [...new Set(must('git', ['ls-files', '--cached', '--others', '--exclude-standard', '-z']).split('\0').filter(Boolean))].sort();
  if (!files.length) throw new Error('empty source inventory');
  const manifest = files.map((path) => ({ path, sha256: existsSync(join(ROOT, path)) ? hash(readFileSync(join(ROOT, path))) : null }));
  return { commit, dirty: status.length !== 0, file_count: manifest.length, snapshot_sha256: hash(JSON.stringify(manifest)), files: manifest };
}

function parseJSONObjects(text) {
  const objects = [];
  let start = -1;
  let depth = 0;
  let quoted = false;
  let escaped = false;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (start < 0) {
      if (/\s/.test(ch)) continue;
      if (ch !== '{') throw new Error('go list emitted a non-object record');
      start = i;
      depth = 1;
      continue;
    }
    if (quoted) {
      if (escaped) escaped = false;
      else if (ch === '\\') escaped = true;
      else if (ch === '"') quoted = false;
      continue;
    }
    if (ch === '"') quoted = true;
    else if (ch === '{') depth++;
    else if (ch === '}') {
      depth--;
      if (depth === 0) {
        objects.push(JSON.parse(text.slice(start, i + 1)));
        start = -1;
      }
    }
  }
  if (start >= 0 || quoted || depth !== 0) throw new Error('go list emitted truncated JSON');
  return objects;
}

export function checkGoInventory(text, source, root = ROOT) {
  const packages = parseJSONObjects(text);
  if (!packages.length) throw new Error('empty Go package inventory');
  if (!Array.isArray(source?.files) || !source.files.length) throw new Error('empty Git source inventory');
  const tracked = new Set(source.files.map((item) => item.path));
  const missing = [];
  let checked = 0;
  for (const pkg of packages) {
    if (typeof pkg.Dir !== 'string' || !pkg.ImportPath) throw new Error('incomplete Go package record');
    for (const key of ['GoFiles', 'CgoFiles', 'IgnoredGoFiles', 'EmbedFiles', 'TestGoFiles', 'XTestGoFiles', 'TestEmbedFiles', 'XTestEmbedFiles']) {
      for (const name of pkg[key] || []) {
        const path = relative(resolve(root), resolve(pkg.Dir, name)).replaceAll('\\', '/');
        if (isAbsolute(path) || path === '..' || path.startsWith('../')) throw new Error('Go package source escaped workspace');
        checked++;
        if (!tracked.has(path)) missing.push({ path, package: pkg.ImportPath, kind: key });
      }
    }
  }
  if (!checked) throw new Error('empty Go source input inventory');
  if (missing.length) throw new Error(`Go package inventory contains source outside Git inventory; use a clean isolated worktree: ${missing.slice(0, 8).map((item) => item.path).join(', ')}`);
  return { packages: packages.length, source_files_checked: checked };
}
function verifyGoInventory(source) {
  const result = command('go', ['list', '-json', './...']);
  if (result.error || result.status !== 0) throw new Error(`go list -json ./... failed: ${result.stderr || result.error?.message || result.status}`);
  return checkGoInventory(result.stdout, source);
}
export function goTests(rc, text) {
  let passed = 0;
  let skipped = 0;
  let failed = false;
  const future = [];
  try {
    for (const line of text.trim().split('\n')) {
      const event = JSON.parse(line);
      const key = event.Package && event.Test ? `${event.Package}\0${event.Test}` : '';
      const placeholder = key ? FUTURE_GO_BY_KEY.get(key) : undefined;
      if (event.Action === 'fail') failed = true;
      if (event.Test && event.Action === 'pass') passed++;
      if (event.Test && event.Action === 'skip') {
        skipped++;
        if (placeholder) future.push({ ...placeholder, package: event.Package });
        else future.push({ package: event.Package, test: event.Test, reason: event.Test && /ReadOnly|Readonly|Permission|NoWrite|权限|只读/i.test(event.Test) ? '环境权限条件不足' : '未登记的 skip' });
      }
    }
  } catch { return fail('Go JSON test evidence missing or malformed'); }
  const unknownSkip = future.filter((item) => item.reason === '未登记的 skip');
  const environmentalSkip = future.filter((item) => item.reason && /环境|权限|平台|unsupported|无法/i.test(item.reason));
  if (rc !== 0 || failed || passed === 0) return { ...fail('Go tests failed or no actual tests passed'), counts: { passed, skipped }, future, unknown_skip: unknownSkip, environmental_skip: environmentalSkip };
  if (unknownSkip.length) return { status: 'FAIL', counts: { passed, skipped }, future, unknown_skip: unknownSkip, environmental_skip: environmentalSkip, reason: 'Go tests contain an unregistered skip' };
  if (environmentalSkip.length) return { status: 'INCOMPLETE', counts: { passed, skipped }, future_requirements: environmentalSkip.map(({ package: pkg, test, reason }) => ({ package: pkg, test, status: 'BLOCKED', reason })), unknown_skip: [], environmental_skip: environmentalSkip, reason: 'environmental Go skips remain blocked' };
  return {
    status: 'PASS',
    counts: { passed, skipped },
    future_requirements: future.map(({ id, test, reason }) => ({ id, test, status: 'NOT_IMPLEMENTED/NOT_RUN', reason })),
    unknown_skip: [],
    ...(future.length ? { reason: 'registered future Go placeholders are recorded separately and do not count as PASS' } : {}),
  };
}

function runProfile(profile, output) {
  const started = new Date().toISOString();
  if (!['full', 'engineering'].includes(profile)) throw new Error('profile must be full or engineering');
  const parent = join(ROOT, 'release-artifacts');
  if (!existsSync(parent)) mkdirSync(parent, { mode: 0o700 });
  const out = output ? resolve(output) : mkdtempSync(join(parent, `${profile}-`));
  if (output) mkdirSync(out, { mode: 0o700 }); // Never reuse, overwrite, or delete another run's artifacts.
  const checks = [];
  const full = profile === 'full';
  let source;
  const record = (item) => {
    checks.push(item);
    console.log(`[release-check] ${item.status} ${item.id}${item.reason ? `: ${item.reason}` : ''}`);
    return item.status === 'PASS';
  };
  const missing = (id, reason, required = true, status = 'BLOCKED') => record({ id, required, status, executed: false, raw_exit_code: null, reason });
  const local = (id, fn, required = true, prerequisite = false) => {
    try {
      const evidence = fn();
      return record({ id, required, status: 'PASS', executed: true, raw_exit_code: 0, evidence });
    } catch (error) {
      return record({ id, required, status: prerequisite ? 'BLOCKED' : 'FAIL', executed: true, raw_exit_code: null, reason: error.message });
    }
  };
  const run = (id, cmd, args, classify, required = true, extraEnv = {}) => {
    const begin = Date.now();
    const result = command(cmd, args, { env: { ...cleanEnvironment(process.env), ...extraEnv } });
    const log = `${result.stdout || ''}${result.stderr || ''}`;
    writeFileSync(join(out, `${id}.log`), log, { flag: 'wx', mode: 0o600 });
    const outcome = result.error || result.signal ? fail(result.error?.message || `signal ${result.signal}`) :
      classify ? classify(result.status, log) : (result.status === 0 ? { status: 'PASS' } : fail(`process exited ${result.status}`));
    return record({ id, required, ...outcome, executed: true, raw_exit_code: result.status, command: [cmd, ...args], duration_ms: Date.now() - begin, log: `${id}.log`, log_sha256: hash(log) });
  };
  try {
    const sourceOK = local('source-identity', () => { source = sourceIdentity(); return { commit: source.commit, dirty: source.dirty, snapshot_sha256: source.snapshot_sha256 }; });
    const nodeOK = local('node24', () => { if (Number(process.versions.node.split('.')[0]) !== 24) throw new Error(`Node 24 required; got ${process.version}`); return process.version; }, true, true);
    const linux = process.platform === 'linux';
    const linuxOK = local('linux-host', () => { if (!linux) throw new Error(`Linux required for flock/race/deployment; actual ${process.platform}/${process.arch}`); return `${process.platform}/${process.arch}`; }, true, true);
    const keyIdentityOK = linuxOK && process.getuid() !== 0;
    if (!keyIdentityOK || !nodeOK) missing('key-permission-and-rerun', 'needs Node 24 and actual Linux under the non-root service identity; Windows/root refusal selftest is not a permission test');
    const goOK = local('go-toolchain', () => {
      const v = must('go', ['env', 'GOVERSION']).trim();
      if (v !== GO_VERSION) throw new Error(`${GO_VERSION} required with GOTOOLCHAIN=local; actual ${v}`);
      return v;
    }, true, true);
    if (sourceOK && goOK) local('go-package-inventory', () => verifyGoInventory(source));
    else missing('go-package-inventory', 'source inventory or pinned Go toolchain unavailable');
    const makeOK = local('gnu-make', () => must('make', ['--version']).split('\n')[0], true, true);
    const bashOK = local('bash', () => must('bash', ['--version']).split('\n')[0], true, true);
    const compilerOK = linuxOK && goOK ? local('race-compiler', () => must('gcc', ['--version']).split('\n')[0], true, true) : missing('race-compiler', 'Linux and the pinned Go toolchain required');

    if (nodeOK) {
      run('engineering-rules', process.execPath, ['--test', '--test-reporter=tap', 'docs/scripts/release-check.test.mjs', 'docs/scripts/release_integration_regression.test.mjs'], classifyTAP);
      run('widget-regressions', process.execPath, ['--test', '--test-reporter=tap', 'docs/scripts/browser/pow_vectors.test.mjs', 'docs/scripts/browser/widget_lifecycle.test.mjs'], classifyTAP);
      run('adminui-regressions', process.execPath, ['--test', '--test-reporter=tap', 'docs/scripts/browser/adminui_lifecycle.test.mjs'], classifyTAP);
      run('tooling-regressions', process.execPath, ['--test', '--test-reporter=tap', 'docs/scripts/build-receipt.test.mjs', 'docs/scripts/soak-evidence.test.mjs', 'docs/scripts/preflight_regression.test.mjs', 'docs/scripts/manual-evidence.test.mjs', 'docs/scripts/stage5a_rules.test.mjs'], classifyTAP);
      if (keyIdentityOK) run('key-permission-and-rerun', process.execPath, ['--test', '--test-reporter=tap', 'docs/scripts/release-check.test.mjs'], classifyTAP);
      local('javascript-syntax', () => {
        const files = ['web', 'docs/scripts'].flatMap(filesBelow).filter((name) => /\.(mjs|js)$/.test(name));
        if (!files.length) throw new Error('no JavaScript sources');
        for (const file of files) must(process.execPath, ['--check', file]);
        return `${files.length} JavaScript files checked`;
      });
      local('widget-size', widgetSize);
    } else for (const id of ['engineering-rules', 'widget-regressions', 'adminui-regressions', 'tooling-regressions', 'javascript-syntax', 'widget-size']) missing(id, 'Node 24 unavailable');
    if (bashOK) local('shell-syntax', () => {
      const files = ['deploy', 'docs/scripts'].flatMap(filesBelow).filter((name) => name.endsWith('.sh'));
      if (!files.length) throw new Error('no shell sources');
      for (const file of files) must('bash', ['-n', file]);
      return `${files.length} actual shell scripts checked`;
    }); else missing('shell-syntax', 'bash unavailable');
    if (goOK) local('fmt-check', fmt); else missing('fmt-check', `${GO_VERSION} unavailable`);
    run('diff-check', 'git', ['diff', '--check']);
    if (linuxOK && goOK) {
      run('go-vet', 'go', ['vet', './...']);
      run('go-test', 'go', ['test', '-json', '-count=1', './...'], goTests);
      if (compilerOK) run('go-race', 'go', ['test', '-race', '-json', '-count=1', './...'], goTests, true, { CGO_ENABLED: '1' });
      else missing('go-race', 'race needs a working C compiler; production binaries remain CGO_ENABLED=0');
      if (makeOK) {
        if (run('static-build', 'make', ['build', 'build-ops'], undefined, true, { CGO_ENABLED: '0' })) local('binary-size', () => [binarySize(join(ROOT, 'bin/epocha')), binarySize(join(ROOT, 'bin/epocha-ops'))]);
        else missing('binary-size', 'static build did not complete');
        run('secret-scan', 'make', ['secret-scan']);
      } else for (const id of ['static-build', 'binary-size', 'secret-scan']) missing(id, 'GNU Make unavailable');
    } else for (const id of ['go-vet', 'go-test', 'go-race', 'static-build', 'binary-size', 'secret-scan']) missing(id, `legitimate Linux + ${GO_VERSION} required; not attempted on native Windows`);

    let fullPlatform = false;
    if (full) fullPlatform = local('release-platform', () => {
      if (!linux || process.arch !== 'arm64' || process.getuid() !== 1000 || process.getgid() !== 1000) throw new Error('formal scope remains B / linux/arm64 / UID:GID 1000:1000; other hosts are test-only');
      return 'B / linux/arm64 / 1000:1000';
    }, true, true);
    else missing('release-platform', 'limited engineering profile; no release/platform certification', false, 'NOT_RUN');
    const clean = sourceOK && !source.dirty;
    if (full && !clean) missing('candidate-clean', 'a formal candidate requires a clean committed source OID; this tool will not commit for you');
    else if (full) local('candidate-clean', () => source.commit);

    let gateHost = false;
    let dockerOK = false;
    if (full && fullPlatform && goOK && makeOK && bashOK && compilerOK) {
      gateHost = local('gate-tools', () => {
        for (const tool of ['curl', 'jq', 'openssl', 'readelf', 'ss', 'python3', 'firefox', 'docker']) must('bash', ['-c', 'command -v "$1"', 'release-check', tool]);
        return 'stage tools present; actual stage commands still decide results';
      }, true, true);
      if (gateHost) dockerOK = local('dedicated-docker', () => {
        const endpoint = process.env.DOCKER_CONTEXT ? must('docker', ['context', 'inspect', process.env.DOCKER_CONTEXT, '--format', '{{.Endpoints.docker.Host}}']).trim() :
          process.env.DOCKER_HOST || must('docker', ['context', 'inspect', '--format', '{{.Endpoints.docker.Host}}']).trim();
        if (!endpoint.startsWith('unix://')) throw new Error('only a dedicated local Unix-socket Docker daemon is allowed');
        must('docker', ['info'], { timeout: 30000 });
        must('docker', ['compose', 'version'], { timeout: 30000 });
        const list = (args) => must('docker', args, { timeout: 30000 }).trim().split('\n').filter(Boolean);
        const inventory = { containers: list(['container', 'ls', '-aq']), images: list(['image', 'ls', '-aq']), volumes: list(['volume', 'ls', '-q']), networks: list(['network', 'ls', '--format', '{{.Name}}']) };
        assertDisposableDocker(inventory);
        return inventory;
      }, true, true);
    }
    if (full && !gateHost && !checks.some((item) => item.id === 'gate-tools')) missing('gate-tools', 'full Linux/arm64 toolchain, UID and gate tools unavailable');
    if (full && !dockerOK && !checks.some((item) => item.id === 'dedicated-docker')) missing('dedicated-docker', 'dedicated Docker inventory cannot be checked before platform prerequisites');
    if (full && !dockerOK) missing('ocr-container', 'isolated Docker prerequisites unavailable');
    if (dockerOK) run('ocr-container', 'make', ['ocr-container']);
    for (const gate of LEGACY_GATES) {
      if (!full) missing(gate.id, 'not selected in engineering-only profile', false, 'NOT_RUN');
      else if (!gateHost || (gate.id !== 'admin-ui' && !dockerOK) || (gate.id === 'stage5b' && !clean)) missing(gate.id, gate.id === 'stage5b' && !clean ? 'clean committed candidate required by stage5b' : 'required Linux/arm64 or isolated Docker prerequisites unavailable');
      else run(gate.id, 'bash', [gate.script], (rc, log) => classifyLegacyGate(gate.id, rc, log));
    }
    if (nodeOK) for (const id of ['stage4a.human-readability', 'stage4b.human-readability', 'adminui.http-errors', 'adminui.clipboard-write', 'adminui.real-mobile']) {
      run(`manual-${id}`, process.execPath, ['docs/scripts/manual-evidence.mjs', '--check', id, '--root', ROOT], (rc, text) => {
        const evidence = JSON.parse(text);
        if (evidence.exit_code !== rc || !['PASS', 'FAIL', 'NOT_RUN'].includes(evidence.status)) return fail('manual evidence receipt mismatch');
        return evidence;
      }, full);
    }
    else for (const id of ['stage4a.human-readability', 'stage4b.human-readability', 'adminui.http-errors', 'adminui.clipboard-write', 'adminui.real-mobile']) missing(`manual-${id}`, 'Node unavailable', full);
    for (const [id, reason] of [
      ['assistive-technology-and-real-mobile', 'real devices and assistive technology not exercised by this command'],
      ['public-deployment', 'no authorization to push, tag, publish ports, change DNS or issue public certificates'],
      ['captcha-production-readiness', 'engineering evidence does not change experimental:true / production_ready:false'],
    ]) missing(id, reason, false, 'NOT_RUN');
    if (sourceOK) local('source-stability', () => {
      const end = sourceIdentity();
      if (end.commit !== source.commit || end.snapshot_sha256 !== source.snapshot_sha256) throw new Error('source changed during checks; rerun against a stable snapshot');
      return end.snapshot_sha256;
    }, true, true);
    else missing('source-stability', 'initial source inventory unavailable');
  } catch (error) {
    record({ id: 'runner-error', required: true, status: 'FAIL', executed: true, raw_exit_code: null, reason: error.message });
  }
  const verdict = summarizeChecks(checks, requiredCheckIDs(profile));
  const summary = {
    schema_version: 1, profile, started_at: started, finished_at: new Date().toISOString(),
    ...verdict, release_ready: full && verdict.exit_code === 0, deployed: false,
    supported_scope: 'B / linux/arm64 / UID:GID 1000:1000',
    host: { platform: process.platform, arch: process.arch, node: process.version },
    source, checks,
    limitations: ['Checksums are traceability, not signatures or provenance authentication.', 'Engineering profile PASS is not full release acceptance.', 'Configuration written locally does not mean remote CI has run.'],
  };
  writeFileSync(join(out, 'summary.json'), `${JSON.stringify(summary, null, 2)}\n`, { flag: 'wx', mode: 0o600 });
  console.log(`[release-check] RESULT=${verdict.result} SCRIPT_RC=${verdict.exit_code} PROFILE=${profile}`);
  console.log(`[release-check] summary=${join(out, 'summary.json')}`);
  return verdict.exit_code;
}

function main(argv) {
  if (argv.length === 4 && argv[0] === '--check-browser-manifest') {
    const [, manifestPath, suitePath, tapPath] = argv;
    try {
      const result = checkBrowserManifest(manifestPath, suitePath, readFileSync(resolve(tapPath), 'utf8'));
      console.log(`MANIFEST_${result.status}=${result.manifest_cases} tests=${result.tests} pass=${result.pass} skipped=${result.skipped} todo=${result.todo} cancelled=${result.cancelled}`);
      return result.status === 'PASS' ? 0 : 3;
    } catch (error) {
      console.error(`[release-check] FAIL: ${error.message}`);
      return 1;
    }
  }
  if (argv.length === 1 && argv[0] === '--fmt-check') { console.log(fmt()); return 0; }
  if (argv.length === 2 && argv[0] === '--binary-size') { console.log(binarySize(argv[1])); return 0; }
  if (argv.length === 1 && argv[0] === '--widget-size') { console.log(widgetSize()); return 0; }
  if (argv.length === 1 && argv[0] === '--help') {
    console.log('node docs/scripts/release-check.mjs [--profile full|engineering] [--out NEW_DIRECTORY]\n0=requested engineering scope passed; 1=failure; 3=incomplete/blocked. Full scope retains stage4b/5a/5b/admin-ui results. No override grants PASS.');
    return 0;
  }
  let profile = 'full';
  let output;
  const seen = new Set();
  for (let i = 0; i < argv.length; i += 2) {
    if (!['--profile', '--out'].includes(argv[i]) || !argv[i + 1] || seen.has(argv[i])) throw new Error(`unknown, duplicate or incomplete argument: ${argv[i]}`);
    seen.add(argv[i]);
    if (argv[i] === '--profile') profile = argv[i + 1];
    else output = argv[i + 1];
  }
  return runProfile(profile, output);
}

if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) {
  try { process.exitCode = main(process.argv.slice(2)); }
  catch (error) { console.error(`[release-check] FAIL: ${error.message}`); process.exitCode = 1; }
}
