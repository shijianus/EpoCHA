import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { assertImageDimensions } from "./browser/image-dimensions.mjs";

const valid = { natural: [280, 104], attributes: [280, 104], rendered: [318, 318 * 104 / 280] };

test("image contract accepts proportional CSS scaling while checking intrinsic and declared dimensions", () => {
  assert.doesNotThrow(() => assertImageDimensions(valid));
  assert.doesNotThrow(() => assertImageDimensions({ ...valid, rendered: [140, 52] }));
});

test("image contract rejects mismatched declared dimensions and stretched rendering", () => {
  for (const image of [
    { ...valid, attributes: [318, 118] },
    { ...valid, natural: [280, 0] },
    { ...valid, attributes: [280.5, 104] },
    { ...valid, rendered: [318, 104] },
    { ...valid, rendered: [0, 0] },
  ]) assert.throws(() => assertImageDimensions(image));
});

test("real browser suite reads HTML attributes rather than CSS-dependent img.width getters", () => {
  const source = readFileSync(new URL("./browser/widget_browser.test.mjs", import.meta.url), "utf8");
  assert.match(source, /getAttribute\(['"]width['"]\)/);
  assert.match(source, /getAttribute\(['"]height['"]\)/);
  assert.match(source, /assertImageDimensions\(dims\)/);
  assert.doesNotMatch(source, /attr:\s*\[img\.width\s*,\s*img\.height\]/);
});
