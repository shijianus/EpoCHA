import assert from "node:assert/strict";

export function assertImageDimensions({ natural, attributes, rendered }) {
  assert.ok(natural.every((n) => Number.isSafeInteger(n) && n > 0),
    "浏览器没有解码出有效图片尺寸");
  assert.ok(attributes.every((n) => Number.isSafeInteger(n) && n > 0),
    "图片声明宽高必须是正整数");
  assert.deepEqual(natural, attributes,
    `PNG 固有尺寸 ${natural} 与 HTML 声明 ${attributes} 不一致`);
  assert.ok(rendered.every((n) => Number.isFinite(n) && n > 0), "图片显示尺寸无效");
  const originalRatio = natural[0] / natural[1];
  const renderedRatio = rendered[0] / rendered[1];
  assert.ok(Math.abs(renderedRatio / originalRatio - 1) < 0.02,
    `图片缩放后比例失真：固有 ${natural}，显示 ${rendered}`);
}
