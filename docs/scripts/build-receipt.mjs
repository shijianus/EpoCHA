#!/usr/bin/env node
// Offline development tooling. Receipts bind bytes and commands, not signatures or release approval.
import { createHash } from 'node:crypto';
import { copyFileSync, existsSync, lstatSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const MODULE = 'github.com/shijianus/EpoCHA';
export const GO_VERSION = 'go1.27.3';
const NAMES = ['epocha', 'epocha-ops'];
const PACKAGES = ['./cmd/server', './cmd/ops'];
const sha = (bytes) => createHash('sha256').update(bytes).digest('hex');
const same = (a, b, label) => { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error(`${label} mismatch`); };
const requireValue = (ok, message) => { if (!ok) throw new Error(message); };
const writeNew = (path, bytes) => writeFileSync(path, bytes, { flag: 'wx', mode: 0o600 });

export function validateInputs(value) {
  requireValue(value && /^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$/.test(value.version), 'version must be a nonempty safe token');
  requireValue(/^[a-f0-9]{40}$/.test(value.commit), 'commit must be the full source OID');
  requireValue(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/.test(value.build_date) &&
    Number.isFinite(Date.parse(value.build_date)) && new Date(value.build_date).toISOString() === value.build_date.replace('Z', '.000Z'), 'build_date must be a fixed valid UTC second');
  requireValue(value.goos === 'linux' && ['arm64', 'amd64'].includes(value.goarch), 'target must be explicit linux/arm64 or linux/amd64 (amd64 is not formal release support)');
  requireValue(value.cgo_enabled === undefined || value.cgo_enabled === '0', 'production CGO_ENABLED must be 0');
  return { version: value.version, commit: value.commit, build_date: value.build_date, goos: value.goos, goarch: value.goarch, cgo_enabled: '0' };
}

export function checkVersionOutput(name, text, inputs) {
  requireValue(NAMES.includes(name), 'unknown binary');
  same(text.trim(), `${name} ${inputs.version} (commit ${inputs.commit}, built ${inputs.build_date})`, `${name} runtime version`);
}

function environment(inputs) {
  const env = { ...process.env };
  for (const key of Object.keys(env)) {
    if (/^(EPOCHA_|BIDI_|ADMIN_|BAD_ADMIN_|EXAMPLE_)/i.test(key) ||
        /^(GOOS|GOARCH|GOAMD64|GOARM64|CGO_ENABLED|GOFLAGS|GOTOOLCHAIN|GOENV|GOWORK|GOPROXY|GOSUMDB|GOPRIVATE|GONOPROXY|GONOSUMDB|GOTOOLDIR|GOROOT|GOEXPERIMENT|NODE_OPTIONS|NODE_TEST_CONTEXT|BASH_ENV|ENV|MAKEFLAGS|MFLAGS)$/i.test(key)) delete env[key];
  }
  return { ...env, ...buildEnvironment(inputs) };
}
function buildEnvironment(inputs) {
  return { GOTOOLCHAIN: 'local', GOENV: 'off', GOWORK: 'off', GOPROXY: 'off', GOSUMDB: 'off',
    GOFLAGS: '-mod=readonly', CGO_ENABLED: '0', GOOS: inputs.goos, GOARCH: inputs.goarch };
}
function run(command, args, options = {}) {
  const started_at = new Date().toISOString();
  const result = spawnSync(command, args, { encoding: 'utf8', timeout: 180000, maxBuffer: 16 * 1024 * 1024, windowsHide: true, ...options });
  const record = { command: [command, ...args], started_at, finished_at: new Date().toISOString(), exit_code: result.status };
  requireValue(!result.error && !result.signal && result.status === 0, `${command} failed (${result.status ?? result.signal ?? 'spawn'}): ${result.error?.message || result.stderr || ''}`);
  return { text: result.stdout, stderr: result.stderr, record };
}
function goVersion(go, env, root) {
  const value = run(go, ['env', 'GOVERSION'], { cwd: root, env }).text.trim();
  same(value, GO_VERSION, 'pinned Go toolchain');
  return value;
}
function sourceIdentity(root, env) {
  const commit = run('git', ['rev-parse', 'HEAD'], { cwd: root, env }).text.trim();
  requireValue(/^[a-f0-9]{40}$/.test(commit), 'full source OID unavailable');
  const dirty = run('git', ['status', '--porcelain=v1', '-z'], { cwd: root, env }).text.length !== 0;
  const paths = [...new Set(run('git', ['ls-files', '--cached', '--others', '--exclude-standard', '-z'], { cwd: root, env }).text.split('\0').filter(Boolean))].sort();
  requireValue(paths.length > 0, 'empty source inventory');
  const files = paths.map((path) => {
    const absolute = join(root, path);
    if (!existsSync(absolute)) return { path, sha256: null };
    requireValue(lstatSync(absolute).isFile(), `source is not a regular file: ${path}`);
    return { path, sha256: sha(readFileSync(absolute)) };
  });
  return { commit, dirty, file_count: files.length, snapshot_sha256: sha(JSON.stringify(files)) };
}
function ldflags(inputs) {
  return `-s -w -X ${MODULE}/internal/version.Version=${inputs.version} -X ${MODULE}/internal/version.Commit=${inputs.commit} -X ${MODULE}/internal/version.BuildDate=${inputs.build_date}`;
}
function buildArgs(index, inputs, destination) {
  return ['build', '-trimpath', '-buildvcs=false', '-ldflags', ldflags(inputs), '-o', destination, PACKAGES[index]];
}
function fileBytes(path) {
  requireValue(lstatSync(path).isFile(), `not a regular file: ${path}`);
  const data = readFileSync(path);
  requireValue(data.length > 0, `empty artifact: ${path}`);
  return data;
}
function staticELF(bytes, arch) {
  requireValue(bytes.length >= 64 && bytes.subarray(0, 4).equals(Buffer.from([127, 69, 76, 70])) && bytes[4] === 2 && bytes[5] === 1, 'expected a little-endian ELF64 binary');
  same(bytes.readUInt16LE(18), arch === 'arm64' ? 183 : 62, 'ELF architecture');
  const offset = Number(bytes.readBigUInt64LE(32));
  const size = bytes.readUInt16LE(54);
  const count = bytes.readUInt16LE(56);
  requireValue(Number.isSafeInteger(offset) && size >= 56 && count > 0 && offset >= 64 && offset + size * count <= bytes.length, 'invalid ELF program header table');
  for (let i = 0; i < count; i++) requireValue(bytes.readUInt32LE(offset + i * size) !== 3, 'PT_INTERP is forbidden in the scratch runtime');
  return { format: 'ELF64', architecture: arch, pt_interp: false };
}
function inspectBinary(go, path, index, inputs, env, root) {
  const bytes = fileBytes(path);
  const elf = staticELF(bytes, inputs.goarch);
  const info = JSON.parse(run(go, ['version', '-m', '-json', path], { cwd: root, env }).text);
  same(info.GoVersion, GO_VERSION, 'embedded Go version');
  same(info.Path, `${MODULE}/${PACKAGES[index].slice(2)}`, 'embedded main package');
  requireValue(Array.isArray(info.Settings) && info.Settings.length > 0, 'missing embedded build settings');
  const settings = Object.fromEntries(info.Settings.map(({ Key, Value }) => [Key, Value]));
  for (const [key, value] of Object.entries({ GOOS: inputs.goos, GOARCH: inputs.goarch, CGO_ENABLED: '0', '-trimpath': 'true' })) same(settings[key], value, `embedded ${key}`);
  // Go deliberately omits -ldflags under -trimpath. The command and receipt checksum bind the tuple;
  // these byte checks are not a substitute for the separate actual image --version invocation.
  for (const value of [inputs.version, inputs.commit, inputs.build_date]) requireValue(bytes.includes(Buffer.from(`${value}\0`)), 'injected version tuple absent from actual binary bytes');
  return { sha256: sha(bytes), size_bytes: bytes.length, go_version: info.GoVersion, module: info.Path, settings, elf };
}
function seal(out, name, value) {
  const bytes = `${JSON.stringify(value, null, 2)}\n`;
  writeNew(join(out, `${name}.json`), bytes);
  writeNew(join(out, `${name}.sha256`), `${sha(bytes)}  ${name}.json\n`);
  return sha(bytes);
}
function unseal(out, name) {
  const bytes = fileBytes(join(out, `${name}.json`));
  same(fileBytes(join(out, `${name}.sha256`)).toString(), `${sha(bytes)}  ${name}.json\n`, `${name} receipt checksum`);
  return { value: JSON.parse(bytes), sha256: sha(bytes) };
}
function checkInventory(out) {
  const allowed = new Set(['out/epocha', 'out/epocha-ops', 'manifest.json', 'manifest.sha256',
    'build-epocha.log', 'build-epocha-ops.log', 'image.json', 'image.sha256', 'image-build.log',
    'image-out/epocha', 'image-out/epocha-ops', 'candidate.json', 'candidate.sha256', 'candidate-manifest.txt']);
  const visit = (relative = '') => {
    for (const entry of readdirSync(join(out, relative), { withFileTypes: true })) {
      const name = relative ? `${relative}/${entry.name}` : entry.name;
      requireValue(!entry.isSymbolicLink(), `symlink in archive: ${name}`);
      if (entry.isDirectory()) {
        requireValue(['out', 'image-out'].includes(name), `unexpected archive directory: ${name}`);
        visit(name);
      } else requireValue(entry.isFile() && allowed.has(name), `unexpected archive file (not public build evidence): ${name}`);
    }
  };
  requireValue(lstatSync(out).isDirectory() && !lstatSync(out).isSymbolicLink(), 'archive must be a regular directory');
  visit();
}

export function buildReceipt({ root = ROOT, out, go = 'go', ...values }) {
  root = resolve(root); out = resolve(out);
  const inputs = validateInputs(values);
  const env = environment(inputs);
  requireValue(!existsSync(out), 'output already exists; choose a new directory, previous bytes are never overwritten');
  const toolchain = goVersion(go, env, root);
  const source = sourceIdentity(root, env);
  same(inputs.commit, source.commit, 'requested commit versus actual source HEAD');
  mkdirSync(out, { mode: 0o700 });
  mkdirSync(join(out, 'out'), { mode: 0o700 });
  const artifacts = NAMES.map((name, index) => {
    const path = `out/${name}`;
    const args = buildArgs(index, inputs, join(out, path));
    const started_at = new Date().toISOString();
    const result = spawnSync(go, args, { cwd: root, env, encoding: 'utf8', timeout: 180000, maxBuffer: 16 * 1024 * 1024, windowsHide: true });
    const log = `${result.stdout || ''}${result.stderr || ''}${result.error ? `\n${result.error.message}\n` : ''}`;
    const logPath = `build-${name}.log`;
    writeNew(join(out, logPath), log);
    requireValue(!result.error && !result.signal && result.status === 0, `${name} compile failed; retained ${join(out, logPath)}`);
    const inspection = inspectBinary(go, join(out, path), index, inputs, env, root);
    return { name, path, ...inspection, build: { command: [go, ...args], cwd: root, environment: buildEnvironment(inputs), started_at, finished_at: new Date().toISOString(), exit_code: result.status, log: logPath, log_sha256: sha(log) } };
  });
  same(sourceIdentity(root, env), source, 'source changed during build');
  const manifest = { schema_version: 1, kind: 'epocha-build-receipt', inputs, toolchain, source, artifacts,
    runtime: { status: 'NOT_RUN', reason: 'Compilation and static inspection only; no target binary was executed.' },
    limitations: ['Checksums are not signed provenance.', 'Dirty source receipts are not formal release candidates.', 'The actual build argv is recorded; reproducible builds are not promised.'] };
  seal(out, 'manifest', manifest);
  verifyReceipt({ out, go });
  return manifest;
}

export function verifyReceipt({ out, go = 'go' }) {
  out = resolve(out);
  checkInventory(out);
  const sealed = unseal(out, 'manifest');
  const manifest = sealed.value;
  same(manifest.schema_version, 1, 'receipt schema');
  same(manifest.kind, 'epocha-build-receipt', 'receipt kind');
  const inputs = validateInputs(manifest.inputs);
  same(manifest.inputs, inputs, 'input schema');
  same(manifest.toolchain, GO_VERSION, 'receipt toolchain');
  same(manifest.source.commit, inputs.commit, 'source commit');
  requireValue(typeof manifest.source.dirty === 'boolean' && manifest.source.file_count > 0 && /^[a-f0-9]{64}$/.test(manifest.source.snapshot_sha256), 'source identity missing');
  same(manifest.runtime.status, 'NOT_RUN', 'build-only runtime status');
  requireValue(manifest.artifacts?.length === 2, 'both original binary artifacts are required');
  const env = environment(inputs);
  goVersion(go, env, out);
  manifest.artifacts.forEach((artifact, index) => {
    same(artifact.name, NAMES[index], 'artifact name');
    same(artifact.path, `out/${NAMES[index]}`, 'artifact path');
    const inspection = inspectBinary(go, join(out, artifact.path), index, inputs, env, out);
    for (const key of Object.keys(inspection)) same(artifact[key], inspection[key], `${artifact.name} ${key}`);
    same(artifact.build.exit_code, 0, 'actual build exit');
    same(artifact.build.environment, buildEnvironment(inputs), 'actual build environment');
    requireValue(typeof artifact.build.cwd === 'string' && artifact.build.cwd.length > 0 && Date.parse(artifact.build.started_at) <= Date.parse(artifact.build.finished_at), 'build location/timestamps missing');
    const argv = artifact.build.command;
    requireValue(Array.isArray(argv) && argv.length === 9 && typeof argv[0] === 'string' && argv[0].length > 0 && typeof argv[7] === 'string', 'actual build command missing');
    same(argv.slice(1), buildArgs(index, inputs, argv[7]), 'actual build argv');
    same(artifact.build.log, `build-${artifact.name}.log`, 'build log path');
    same(artifact.build.log_sha256, sha(readFileSync(join(out, artifact.build.log))), 'build log hash');
  });
  if (existsSync(join(out, 'image.json'))) verifyImage(out, sealed);
  else requireValue(!existsSync(join(out, 'image.sha256')), 'orphan image checksum');
  if (existsSync(join(out, 'candidate.json'))) {
    const candidate = unseal(out, 'candidate').value;
    same(candidate.build_manifest_sha256, sealed.sha256, 'candidate build binding');
    same(candidate.image_receipt_sha256, unseal(out, 'image').sha256, 'candidate image binding');
    same(candidate.source, manifest.source, 'candidate source binding');
    requireValue(!candidate.source.dirty, 'candidate source must be clean');
    checkBackup(candidate.backup, inputs);
    same(fileBytes(join(out, 'candidate-manifest.txt')).toString(), candidateText(candidate), 'candidate text');
  }
  return manifest;
}

function verifyImage(out, sealed) {
  const image = unseal(out, 'image').value;
  same(image.build_manifest_sha256, sealed.sha256, 'image build binding');
  requireValue(/^sha256:[a-f0-9]{64}$/.test(image.image_id), 'local image ID missing');
  requireValue(typeof image.tag === 'string' && image.tag.length > 0 && image.identity_kind === 'local-image-id', 'image tag/identity kind missing');
  same(image.platform, `${sealed.value.inputs.goos}/${sealed.value.inputs.goarch}`, 'image platform');
  requireValue(Number.isSafeInteger(image.size_bytes) && image.size_bytes > 0 && image.commands?.length > 0, 'image size/actual commands missing');
  same(image.build_log_sha256, sha(readFileSync(join(out, 'image-build.log'))), 'image build log');
  requireValue(image.artifacts?.length === 2, 'image binary evidence missing');
  image.artifacts.forEach((artifact, index) => {
    const name = NAMES[index];
    same(artifact.name, name, 'image binary name');
    const hash = sha(fileBytes(join(out, `image-out/${name}`)));
    same(hash, sealed.value.artifacts[index].sha256, 'copied image bytes versus original build');
    same(artifact.sha256, hash, 'image copied byte receipt');
    same(artifact.runtime.exit_code, 0, 'actual image version exit');
    checkVersionOutput(name, artifact.runtime.stdout, sealed.value.inputs);
  });
  return image;
}

export function imageReceipt({ root = ROOT, out, go = 'go', tag, docker = 'docker' }) {
  root = resolve(root); out = resolve(out);
  const manifest = verifyReceipt({ out, go });
  requireValue(!existsSync(join(out, 'image.json')) && !existsSync(join(out, 'image-build.log')) && !existsSync(join(out, 'image-out')), 'image evidence exists; build into a new output directory');
  requireValue(typeof tag === 'string' && /^[A-Za-z0-9][A-Za-z0-9._/:+-]{0,255}$/.test(tag), 'invalid image tag');
  const env = environment(manifest.inputs);
  const commands = [];
  const dockerRun = (args, timeout = 60000) => {
    const result = run(docker, args, { cwd: root, env, timeout });
    commands.push(result.record);
    return result;
  };
  const existing = spawnSync(docker, ['image', 'inspect', tag], { cwd: root, env, encoding: 'utf8', timeout: 30000, maxBuffer: 1024 * 1024, windowsHide: true });
  commands.push({ command: [docker, 'image', 'inspect', tag], cwd: root, exit_code: existing.status, started_at: new Date().toISOString(), finished_at: new Date().toISOString() });
  requireValue(!existing.error && !existing.signal && existing.status !== 0, `image tag already exists or cannot be probed: ${tag}; refusing overwrite`);
  const build = dockerRun(['build', '--network=none', '--pull=false', '--platform', `${manifest.inputs.goos}/${manifest.inputs.goarch}`, '-f', 'deploy/docker/Dockerfile', '--target', 'runtime', '-t', tag, '--build-context', `toolchain=${out}`, '.'], 600000);
  const buildLog = `${build.text}${build.stderr}`;
  writeNew(join(out, 'image-build.log'), buildLog);
  const inspect = JSON.parse(dockerRun(['image', 'inspect', tag]).text);
  requireValue(Array.isArray(inspect) && inspect.length === 1, 'ambiguous image identity');
  const { Id: image_id, Os: os, Architecture: arch, Size: size_bytes } = inspect[0];
  requireValue(/^sha256:[a-f0-9]{64}$/.test(image_id), 'Docker did not return an immutable local image ID');
  same(`${os}/${arch}`, `${manifest.inputs.goos}/${manifest.inputs.goarch}`, 'actual image platform');
  mkdirSync(join(out, 'image-out'), { mode: 0o700 });
  const confinement = ['--network', 'none', '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--memory', '128m', '--pids-limit', '64'];
  let cid;
  let serverVersion;
  try {
    cid = dockerRun(['create', ...confinement, '--entrypoint', '/usr/local/bin/epocha', image_id, '--version']).text.trim();
    requireValue(/^[a-f0-9]{64}$/.test(cid), 'created container ID missing');
    for (const name of NAMES) dockerRun(['cp', `${cid}:/usr/local/bin/${name}`, join(out, `image-out/${name}`)]);
    serverVersion = dockerRun(['start', '-a', cid]);
    same(dockerRun(['inspect', '--format', '{{.State.ExitCode}}', cid]).text.trim(), '0', 'server version container exit');
  } finally {
    if (cid && /^[a-f0-9]{64}$/.test(cid)) dockerRun(['rm', '-f', cid]);
  }
  const opsVersion = dockerRun(['run', '--rm', ...confinement, '--entrypoint', '/usr/local/bin/epocha-ops', image_id, 'version']);
  const versions = [serverVersion, opsVersion];
  const artifacts = NAMES.map((name, index) => {
    const hash = sha(fileBytes(join(out, `image-out/${name}`)));
    same(hash, manifest.artifacts[index].sha256, `${name} actual image bytes`);
    checkVersionOutput(name, versions[index].text, manifest.inputs);
    return { name, sha256: hash, runtime: { command: versions[index].record.command, exit_code: versions[index].record.exit_code, stdout: versions[index].text } };
  });
  const image = { schema_version: 1, build_manifest_sha256: unseal(out, 'manifest').sha256, identity_kind: 'local-image-id', image_id, tag,
    platform: `${os}/${arch}`, size_bytes, commands, artifacts, build_log_sha256: sha(buildLog),
    limitations: ['A mutable tag is not a digest. The local image ID is not a registry digest.', 'Only binary bytes and --version were checked here, not deployment acceptance.'] };
  seal(out, 'image', image);
  verifyReceipt({ out, go });
  return image;
}

export function checkBackup(backup, inputs) {
  requireValue(backup && Number.isSafeInteger(backup.schema_version) && backup.schema_version > 0, 'backup schema missing');
  same(backup.app_version, inputs.version, 'backup app_version');
  same(backup.commit, inputs.commit, 'backup commit');
}
function candidateText(candidate) {
  return `Build receipt: manifest.json (sha256 ${candidate.build_manifest_sha256})\nImage receipt: image.json (sha256 ${candidate.image_receipt_sha256})\nSource: ${candidate.source.commit} clean\nPlatform: ${candidate.platform}\nRuntime UID:GID: ${candidate.uid}:${candidate.gid}\nBackup schema: ${candidate.backup.schema_version}\nImage ID (local, NOT registry digest): ${candidate.image_id}\nTag (mutable, NOT digest): ${candidate.tag}\nThis candidate identity does not assert that all deployment gates passed.\n`;
}
export function candidateReceipt({ root = ROOT, out, go = 'go', backup_manifest, uid, gid }) {
  root = resolve(root); out = resolve(out);
  const manifest = verifyReceipt({ out, go });
  const imageSeal = unseal(out, 'image');
  requireValue(!manifest.source.dirty, 'formal candidate requires clean source at build time');
  same(sourceIdentity(root, environment(manifest.inputs)), manifest.source, 'source changed since actual build');
  same(`${manifest.inputs.goos}/${manifest.inputs.goarch}`, 'linux/arm64', 'formal support platform');
  requireValue(String(uid) === '1000' && String(gid) === '1000', 'formal support requires actual UID:GID 1000:1000');
  const backupBytes = fileBytes(backup_manifest);
  const fullBackup = JSON.parse(backupBytes);
  const backup = { schema_version: fullBackup.schema_version, app_version: fullBackup.app_version, commit: fullBackup.commit };
  checkBackup(backup, manifest.inputs);
  const candidate = { schema_version: 1, build_manifest_sha256: unseal(out, 'manifest').sha256, image_receipt_sha256: imageSeal.sha256,
    source: manifest.source, platform: 'linux/arm64', uid: Number(uid), gid: Number(gid), backup, backup_manifest_sha256: sha(backupBytes),
    image_id: imageSeal.value.image_id, tag: imageSeal.value.tag };
  seal(out, 'candidate', candidate);
  writeNew(join(out, 'candidate-manifest.txt'), candidateText(candidate));
  verifyReceipt({ out, go });
  return candidate;
}

function main(argv) {
  const [action, ...rest] = argv;
  const allowed = {
    build: ['root', 'out', 'go', 'version', 'commit', 'build-date', 'goos', 'goarch'],
    verify: ['out', 'go'], image: ['root', 'out', 'go', 'tag'],
    candidate: ['root', 'out', 'go', 'backup-manifest', 'uid', 'gid'],
  };
  requireValue(Object.hasOwn(allowed, action), 'usage: build-receipt.mjs build|verify|image|candidate --out NEW_DIRECTORY [fixed inputs]');
  const options = {};
  for (let i = 0; i < rest.length; i += 2) {
    const key = rest[i]?.slice(2);
    requireValue(rest[i]?.startsWith('--') && allowed[action].includes(key) && rest[i + 1] && !Object.hasOwn(options, key.replaceAll('-', '_')), `unknown, incomplete or repeated argument ${rest[i]}`);
    options[key.replaceAll('-', '_')] = rest[i + 1];
  }
  requireValue(options.out, '--out is required');
  const handlers = { build: buildReceipt, verify: verifyReceipt, image: imageReceipt, candidate: candidateReceipt };
  handlers[action](options);
  console.log(`[build-receipt] ${action} verified: ${resolve(options.out)} (no release approval implied)`);
}
if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) {
  try { main(process.argv.slice(2)); } catch (error) { console.error(`[build-receipt] FAIL: ${error.message}`); process.exitCode = 1; }
}
