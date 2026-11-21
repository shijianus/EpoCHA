// adminlife.generator-default：管理台创建表单不得有任何隐式默认题型（ADR-034 §7）。
// Registry.IDs() 按字母排序，accessible-choice-v1（无障碍专用）是首项——旧实现把它
// 预选成默认值，切到 pow_question 就会提交一个普通通道永远 503 的站点。
// 这条测试用**真实 19 型目录与排序**（镜像 Registry.IDs() + dispatch.Catalog 的
// accessible/motion 事实），在 Node DOM 仿真里走创建表单的真实路径。

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { BROWSER_CASE_TITLES } from "./cases.mjs";
import { flush, harness } from "./adminui_lifecycle_harness.mjs";

function caseTitle(id) {
  const title = BROWSER_CASE_TITLES[id];
  if (!title) throw new Error(`browser manifest missing ${id}`);
  return title;
}

// 真实 19 型目录（与生产 Registry 注册集一致）+ dispatch.Catalog 的通道事实。
// 排序必须与 Registry.IDs() 的 sort.Strings 输出一致：accessible-choice-v1 是首项。
const REAL_CATALOG = [
  { id: "accessible-choice-v1", media: "text", version: "v1", experimental: true, production_ready: false, accessible: true, motion: false },
  { id: "arithmetic-baseline-v1", media: "text", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "bank-shot-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "crowd-find-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "energy-window-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: true },
  { id: "intersection-point-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "parallax-code-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: true },
  { id: "pipe-connect-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "progressive-target-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "shape-count-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "shape-grid-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "shell-track-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: true },
  { id: "silhouette-assembly-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "symbol-path-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "text-image-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "text-image-v2", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "text-image-v3", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "tile-rotation-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
  { id: "visual-rank-v1", media: "image", version: "v1", experimental: true, production_ready: false, accessible: false, motion: false },
];
assert.equal(REAL_CATALOG.length, 19, "catalog must mirror the real 19 registered generators");
const sorted = [...REAL_CATALOG].map((item) => item.id).sort();
assert.deepEqual(REAL_CATALOG.map((item) => item.id), sorted, "catalog order must match Registry.IDs() alphabetical output");

test(caseTitle("adminlife.generator-default"), async (t) => {
  const h = harness(t);
  // 注入真实 19 型目录（一次性 responder 优先于默认夹具）：
  h.queue("GET", "/api/admin/generators", () => Promise.resolve(h.response({ max_difficulty: 5, generators: REAL_CATALOG })));
  await h.login();

  const gen = h.find('[id^="new-gen"]');
  assert.equal(gen.children[0].value, "", "first option must be the explicit placeholder");
  assert.equal(gen.children[0].selected, true, "no implicit generator default");
  assert.match(gen.children[0].textContent, /明确选择题型/);
  const values = gen.children.map((option) => option.value).filter((value) => value !== "");
  assert.deepEqual(values, sorted, "options must list the real 19 generators in registry order; actual=" + JSON.stringify(values));
  const accOption = gen.children.find((option) => option.value === "accessible-choice-v1");
  assert.match(accOption.textContent, /无障碍专用/, "accessible-only generator must be labeled");

  // 切到 pow_question 且未选题：提交被拦，无 POST，状态行给出明确指引。
  await h.input('[id^="new-name"]', "no-default");
  const mode = h.find('[id^="new-mode"]');
  await h.input(mode, "pow_question");
  const writesBefore = h.requests.filter((item) => item.options.method === "POST").length;
  await h.click("创建站点");
  assert.match(h.text(), /必须明确选择题型/);
  assert.equal(h.requests.filter((item) => item.options.method === "POST").length, writesBefore, "blocked submit must not POST");

  // 选中无障碍专用题型且池为空：同样拦截，指明通道限制与出路。
  await h.input(gen, "accessible-choice-v1");
  await h.click("创建站点");
  assert.match(h.text(), /无障碍专用题型/);
  assert.equal(h.requests.filter((item) => item.options.method === "POST").length, writesBefore, "channel-mismatch submit must not POST");

  // 显式配置：普通题型 + 无障碍题型入池 ⇒ 合法，POST 真实发出且 payload 含 pool。
  await h.input(gen, "arithmetic-baseline-v1");
  const accBox = h.document.querySelectorAll('[id^="new-pool"] input[type="checkbox"]')
    .find((box) => box.value === "accessible-choice-v1");
  assert.ok(accBox, "pool picker must list accessible-choice-v1");
  // 仿真 DOM 不复刻浏览器对 checkbox 的原生切换：直接置位等价于用户点击后的状态。
  accBox.checked = true;
  await h.click("创建站点");
  await flush();
  const post = h.requests.filter((item) => item.options.method === "POST").pop();
  assert.ok(post, "explicit mixed-channel config must POST");
  const body = JSON.parse(post.options.body);
  assert.equal(body.generator_id, "arithmetic-baseline-v1");
  assert.deepEqual(body.generator_pool, ["accessible-choice-v1"]);
  assert.equal(body.verify_mode, "pow_question");
});
