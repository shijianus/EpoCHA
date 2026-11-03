import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

const ROOT = fileURLToPath(new URL('../..', import.meta.url));
const rules = () => import('./soak-evidence.mjs');
const good = '负载形状 concurrency=1 requests=450 base=http://127.0.0.1:18182（凭据不回显）\n节流 pace=660ms（每 worker）→ 目标速率≈1.52 闭环/秒（并发 1）\nwall=4m58s flows=450 ok=450 吞吐=1.51 闭环/秒\nchallenge  n=450 p50=1ms p95=2ms max=3ms\nredeem     n=450 p50=1ms p95=2ms max=3ms\nsiteverify n=450 p50=1ms p95=2ms max=3ms\npow-tool   n=450 p50=1ms p95=2ms max=3ms\n';
const pool = { buckets: [{ generator_id: 'text-image-v1', difficulty: 5, ready: 20, ready_asset_bytes: 1024, total_asset_bytes: 1024 }], image: { asset_bytes: 1024, budget_bytes: 4194304 } };

test('450/450 requires exact counts, all stage samples and actual zero exit inside 300 seconds', async () => {
  const { checkLoad } = await rules();
  const input = { text: good, exit_code: 0, elapsed_ms: 298000, timed_out: false };
  assert.equal(checkLoad(input).ok, 450);
  for (const patch of [
    { text: '' }, { text: good.replace('ok=450', 'ok=4500') }, { text: good.replace('flows=450', 'flows=449') },
    { text: good.replace('requests=450', 'requests=500') }, { text: good.replace('ok=450', 'ok=449') },
    { text: good.replace('siteverify n=450', 'siteverify n=449') }, { text: good + '失败类别：challenge=1\n' },
    { text: good + good }, { exit_code: 1 }, { exit_code: 124 }, { elapsed_ms: 300001 }, { timed_out: true },
    { text: good.replace('pace=660ms', 'pace=0s') }, { text: good.replace('wall=4m58s', 'wall=9m0s') },
  ]) assert.throws(() => checkLoad({ ...input, ...patch }), undefined, JSON.stringify(patch));
});

test('image creation and pool evidence cannot be empty, inferred from other buckets or numeric strings', async () => {
  const { checkImageSite, checkPool } = await rules();
  const site = { site: { site_key: 'pk_test', verify_mode: 'pow_question', generator_id: 'text-image-v1', difficulty: 5 } };
  assert.equal(checkImageSite(0, site).site_key, 'pk_test');
  for (const [rc, value] of [[1, site], [0, {}], [0, { site: { ...site.site, site_key: '' } }], [0, { site: { ...site.site, generator_id: 'arithmetic-baseline-v1' } }]]) {
    assert.throws(() => checkImageSite(rc, value));
  }
  assert.equal(checkPool(pool).ready, 20);
  for (const value of [{}, { ...pool, buckets: [] }, { ...pool, image: { asset_bytes: 0 } },
    { ...pool, buckets: [{ ...pool.buckets[0], generator_id: 'arithmetic-baseline-v1' }] },
    { ...pool, buckets: [{ ...pool.buckets[0], ready: 0 }] },
    { ...pool, buckets: [{ ...pool.buckets[0], ready_asset_bytes: 0 }] },
    { ...pool, buckets: [{ ...pool.buckets[0], ready: '20' }] },
  ]) assert.throws(() => checkPool(value));
});

test('resource sampling rejects unknowns, gaps, bad readyz, malformed values and no samples', async () => {
  const { checkSamples } = await rules();
  const samples = Array.from({ length: 11 }, (_, i) => `t=${i * 28}s stats=1.25%|20MiB / 128MiB db=4096 wal=0 readyz=200`).join('\n') + '\n';
  assert.equal(checkSamples(samples, 298000).length, 11);
  for (const bad of ['', '?', samples.replace('1.25%', '?'), samples.replace('readyz=200', 'readyz=503'),
    samples.replace('db=4096', 'db=?'), samples.replace('wal=0', 'wal=-1'), samples.replace('t=28s', 't=80s'),
    samples.replace('128MiB', '0MiB'), samples.replace('20MiB', '200MiB'), samples.split('\n').slice(5).join('\n'),
  ]) assert.throws(() => checkSamples(bad, 298000));
});

test('deadline stops a real hung subprocess instead of relying on request count', { timeout: 5000 }, async () => {
  const { runBounded } = await rules();
  const start = performance.now();
  const result = await runBounded(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { timeout_ms: 100 });
  assert.equal(result.timed_out, true);
  assert.notEqual(result.exit_code, 0);
  assert.ok(performance.now() - start < 4000);
  const quick = await runBounded(process.execPath, ['-e', 'process.stdout.write("observed")'], { timeout_ms: 2000 });
  assert.equal(quick.exit_code, 0);
  assert.equal(quick.timed_out, false);
  assert.equal(quick.stdout, 'observed');
});

test('stage5b requires site, load, pool and sample evidence and uses an independent hard watchdog', () => {
  const script = readFileSync(join(ROOT, 'docs/scripts/stage5b.sh'), 'utf8');
  assert.match(script, /soak-evidence\.mjs run/);
  assert.match(script, /soak-evidence\.mjs image-site/);
  assert.match(script, /soak-evidence\.mjs pool/);
  assert.match(script, /soak-evidence\.mjs verify/);
  assert.doesNotMatch(script, /wait "\$SOAK_PID" \|\| true/);
  assert.doesNotMatch(script, /grep -q "ok=450"/);
  assert.doesNotMatch(script, /stats=.*\|\| echo '\?'/);
});
