import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const rules = () => import('./build-receipt.mjs');
const inputs = { version: 'freeze-test', commit: '0123456789abcdef0123456789abcdef01234567', build_date: '2026-11-02T01:02:03Z', goos: 'linux', goarch: 'arm64' };

test('build inputs require a full commit and fixed safe version/date/target before compilation', async () => {
  const { validateInputs } = await rules();
  assert.doesNotThrow(() => validateInputs(inputs));
  for (const patch of [
    { commit: '0123456' }, { version: '' }, { version: 'x -X injected=true' },
    { build_date: 'unknown' }, { build_date: '2026-02-31T00:00:00Z' },
    { goos: 'windows' }, { goarch: '' }, { cgo_enabled: '1' },
  ]) assert.throws(() => validateInputs({ ...inputs, ...patch }), undefined, JSON.stringify(patch));
});

test('version checks require the actual exact tuple, not a matching prefix or mutable tag', async () => {
  const { checkVersionOutput } = await rules();
  const output = `epocha ${inputs.version} (commit ${inputs.commit}, built ${inputs.build_date})\n`;
  assert.doesNotThrow(() => checkVersionOutput('epocha', output, inputs));
  for (const bad of ['', output.replace(inputs.commit, '0123456'), output.replace(inputs.build_date, '2026-11-02T01:02:04Z'), output + output]) {
    assert.throws(() => checkVersionOutput('epocha', bad, inputs));
  }
});

test('stage5b captures parameters before the build and never reconstructs a later dry run', () => {
  const script = readFileSync(join(ROOT, 'docs/scripts/stage5b.sh'), 'utf8');
  assert.doesNotMatch(script, /make -n build/);
  assert.match(script, /make docker-image-local[^\n]*VERSION="\$VERSION"[^\n]*COMMIT="\$COMMIT"[^\n]*BUILDDATE="\$BUILDDATE"/);
  assert.match(script, /build-receipt\.mjs candidate/);
  assert.doesNotMatch(script, /if \[\[ \$DOCKER_OK -eq 1 \]\]; then\s*# B 路径[\s\S]{0,220}\s+pass/);
});

test('Makefile builds and packages one recorded prebuilt directory rather than rebuilding on verification', () => {
  const text = readFileSync(join(ROOT, 'Makefile'), 'utf8');
  assert.match(text, /COMMIT\s*:=.*git rev-parse HEAD/);
  assert.match(text, /build-receipt\.mjs build/);
  assert.match(text, /build-receipt\.mjs image/);
  assert.match(text, /--version "\$\(VERSION\)" --commit "\$\(COMMIT\)" --build-date "\$\(BUILDDATE\)"/);
});

test('CI archives original prebuilt bytes and receipts, including the hidden directory', () => {
  const text = readFileSync(join(ROOT, '.github/workflows/ci.yml'), 'utf8');
  assert.match(text, /include-hidden-files:\s*true/);
  assert.match(text, /deploy\/docker\/\.prebuilt\//);
  assert.match(text, /build-receipt\.mjs build/);
  assert.doesNotMatch(text, /path:\s*(?:\.|deploy\/docker|\*\*)\s*$/m);
});
