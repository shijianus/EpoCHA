import { spawn as nodeSpawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdir, mkdtemp, rm, readFile, writeFile, rename } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import net from "node:net";

export class ResourceScope {
  constructor({ parentDir = tmpdir(), termTimeoutMs = 2000, killTimeoutMs = 1000 } = {}) {
    this.parentDir = parentDir;
    this.termTimeoutMs = termTimeoutMs;
    this.killTimeoutMs = killTimeoutMs;
    this.children = [];
    this.workspace = null;
    this.closePromise = null;
  }

  async createWorkspace() {
    if (this.workspace) throw new Error("case workspace already created");
    await mkdir(this.parentDir, { recursive: true });
    this.workspace = await mkdtemp(join(this.parentDir, "widget-case-"));
    return this.workspace;
  }

  add(child) {
    if (!child || typeof child.kill !== "function") throw new TypeError("invalid child process");
    this.children.push(child);
    return child;
  }

  close() {
    if (!this.closePromise) this.closePromise = this.#close();
    return this.closePromise;
  }

  async #close() {
    const errors = [];
    let childFailed = false;
    for (const child of [...this.children].reverse()) {
      try {
        await stopChild(child, this.termTimeoutMs, this.killTimeoutMs);
      } catch (error) {
        childFailed = true;
        errors.push(error);
      }
    }
    this.children.length = 0;
    if (this.workspace && !childFailed) {
      try {
        await rm(this.workspace, { recursive: true, force: true, maxRetries: 3, retryDelay: 50 });
      } catch (error) {
        errors.push(error);
      }
      this.workspace = null;
    }
    if (errors.length) throw new AggregateError(errors, "widget case resource cleanup failed");
  }
}

function exitWithin(child, timeoutMs) {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve(true);
  return new Promise((resolve) => {
    let timer;
    const done = () => {
      clearTimeout(timer);
      child.removeListener("exit", done);
      child.removeListener("close", done);
      resolve(true);
    };
    child.once("exit", done);
    child.once("close", done);
    timer = setTimeout(() => {
      child.removeListener("exit", done);
      child.removeListener("close", done);
      resolve(false);
    }, timeoutMs);
    timer.unref?.();
  });
}

async function waitExit(child, timeoutMs) {
    if (child.exitCode !== null || child.signalCode !== null) return child.exitCode === 0;

  return new Promise((resolve) => {
    let timer;
    const done = () => {
      clearTimeout(timer);
      child.removeListener("exit", done);
      child.removeListener("close", done);
      resolve(child.exitCode !== null && child.exitCode === 0);
    };
    child.once("exit", done);
    child.once("close", done);
    timer = setTimeout(() => {
      child.removeListener("exit", done);
      child.removeListener("close", done);
      resolve(false);
    }, timeoutMs);
    timer.unref?.();
  });
}

async function stopChild(child, termTimeoutMs, killTimeoutMs) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  child.kill("SIGTERM");
  if (await exitWithin(child, termTimeoutMs)) return;
  child.kill("SIGKILL");
  if (await exitWithin(child, killTimeoutMs)) return;
  throw new Error("child did not exit after bounded TERM/KILL cleanup");
}

export async function waitFor(check, { timeoutMs = 15000, intervalMs = 50, signal, label = "condition" } = {}) {
  const started = Date.now();
  for (;;) {
    if (signal?.aborted) throw signal.reason instanceof Error ? signal.reason : new Error("wait aborted");
    const value = await check();
    if (value) return value;
    const remaining = timeoutMs - (Date.now() - started);
    if (remaining <= 0) throw new Error(`timed out waiting for ${label}`);
    await new Promise((resolve, reject) => {
      const timer = setTimeout(done, Math.min(intervalMs, remaining));
      const onAbort = () => {
        clearTimeout(timer);
        signal.removeEventListener("abort", onAbort);
        reject(signal.reason instanceof Error ? signal.reason : new Error("wait aborted"));
      };
      function done() {
        signal?.removeEventListener("abort", onAbort);
        resolve();
      }
      if (signal) {
        signal.addEventListener("abort", onAbort, { once: true });
        if (signal.aborted) onAbort();
      }
    });
  }
}

function randomKey(prefix) {
  return `${prefix}_${randomBytes(12).toString("hex")}`;
}

function secret() {
  return randomBytes(32).toString("base64");
}

async function freeLoopbackPort() {
  const server = net.createServer();
  await new Promise((resolve, reject) => server.listen(0, "127.0.0.1", resolve).once("error", reject));
  const { port } = server.address();
  await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  return port;
}

function command(scope, executable, args, env) {
  const child = nodeSpawn(executable, args, { env, stdio: ["ignore", "pipe", "pipe"] });
  child.output = "";
  const append = (chunk) => {
    child.output = (child.output + chunk.toString("utf8")).slice(-256 * 1024);
  };
  child.stdout.on("data", append);
  child.stderr.on("data", append);
  child.once("error", (error) => { child.spawnError = error; });
  scope.add(child);
  return child;
}

function listenPort(output, label) {
  const escaped = label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = output.match(new RegExp(`msg=(?:"${escaped}"|${escaped}) addr=127\\.0\\.0\\.1:(\\d+)`));
  return match ? Number(match[1]) : 0;
}

async function readyChild(child, check, label, signal) {
  return waitFor(async () => {
    if (child.spawnError) throw child.spawnError;
    if (child.exitCode !== null || child.signalCode !== null) {
      throw new Error(`${label} exited during startup (${child.exitCode ?? child.signalCode})`);
    }
    return await check();
  }, { timeoutMs: 20000, intervalMs: 50, signal, label });
}

async function createSite(admin, adminKey, { name, origin, powBits = 12, responseTTL = 900, generator = "text-image-v1" }) {
  const siteKey = randomKey("pk_case");
  const response = await fetch(`${admin}/api/admin/sites`, {
    signal: AbortSignal.timeout(5000),
    method: "POST",
    headers: { Authorization: `Bearer ${adminKey}`, "Content-Type": "application/json" },
    body: JSON.stringify({
      name, site_key: siteKey, pow_bits: powBits, verify_mode: "pow_question",
      generator_id: generator, difficulty: 1, response_ttl_sec: responseTTL,
      origins: [origin], allow_missing_origin: false,
    }),
  });
  if (response.status !== 201) throw new Error(`case site creation failed (${response.status})`);
  const body = await response.json();
  if (body.site?.site_key !== siteKey || typeof body.secret !== "string" || !body.secret) {
    throw new Error("case site creation response is incomplete");
  }
  return { siteKey, siteSecret: body.secret };
}

async function startApp(scope, { binary, api, site, csp, widgetOrigin, parent, env, signal, port }) {
  port ??= await freeLoopbackPort();
  const address = `127.0.0.1:${port}`;
  const child = command(scope, binary, [
    "--listen", address, "--api", api, "--site-key", site.siteKey,
    "--site-secret", site.siteSecret, "--csp", csp,
    "--widget-origin", widgetOrigin, "--db", join(parent, `guestbook-${port}.db`),
  ], env);
  const base = `http://${address}`;
  await readyChild(child, async () => {
    try {
      const response = await fetch(`${base}/healthz`, { signal: AbortSignal.timeout(500) });
      return response.ok;
    } catch {
      return false;
    }
  }, "exampleapp", signal);
  return `${base}/`;
}

export async function createWidgetCaseFixture({ testName, parentDir, serverBin, devseedBin, exampleappBin, signal, generator = "text-image-v1" } = {}) {
  for (const [name, value] of Object.entries({ testName, parentDir, serverBin, devseedBin, exampleappBin })) {
    if (typeof value !== "string" || value.length === 0) throw new Error(`missing widget case fixture option: ${name}`);
  }
  const scope = new ResourceScope({ parentDir });
  try {
    const workspace = await scope.createWorkspace();
    const masterKey = secret();
    const pepper = secret();
    const adminKey = `ak_${randomBytes(32).toString("hex")}`;
    const serverEnv = {
      ...process.env,
      EPOCHA_MASTER_KEY: masterKey,
      EPOCHA_PEPPER: pepper,
      EPOCHA_ADMIN_KEY: adminKey,
      EPOCHA_POOL_ENABLED: "0",
      EPOCHA_RATE_IP_RPM: "6000",
      EPOCHA_RATE_SITE_RPM: "6000",
      EPOCHA_MAX_ACTIVE_CHALLENGES: "2000",
      EPOCHA_MAX_ACTIVE_CHALLENGES_PER_SITE: "2000",
    };
    const server = command(scope, serverBin, ["--db", join(workspace, "epocha.db"), "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0"], serverEnv);
    const publicPort = await readyChild(server, () => listenPort(server.output, "已监听"), "epocha public listener", signal);
    const adminPort = await readyChild(server, () => listenPort(server.output, "管理面已挂载"), "epocha admin listener", signal);
    const api = `http://127.0.0.1:${publicPort}`;
    const admin = `http://127.0.0.1:${adminPort}`;
    await readyChild(server, async () => {
      try {
        const response = await fetch(`${api}/healthz`, { signal: AbortSignal.timeout(500) });
        return response.ok;
      } catch {
        return false;
      }
    }, "epocha health endpoint", signal);

    const mainPort = await freeLoopbackPort();
    const mainSite = await createSite(admin, adminKey, { name: "widget case main", origin: `http://127.0.0.1:${mainPort}`, generator });
    const mainURL = await startApp(scope, {
      binary: exampleappBin, api, site: mainSite, csp: "blob", widgetOrigin: "api", parent: workspace, env: serverEnv, signal, port: mainPort,
    });

    const seedDir = join(workspace, "seed");
    await mkdir(seedDir);
    const records = [];
    // 刷新会按生产策略抬高难度；每档均提供受控题，不能靠禁用风险策略维持测试。
    for (let difficulty = 1; difficulty <= 5; difficulty++) {
      const tierDir = join(workspace, `tier-${difficulty}`);
      const seeder = command(scope, devseedBin, [
        "--db", join(workspace, "epocha.db"), "--out-dir", tierDir,
        "--generator", generator, "--difficulty", String(difficulty), "--count", "12",
      ], serverEnv);
      const completed = await waitExit(seeder, 20000);
      if (!completed || seeder.exitCode !== 0) throw new Error(`devseed failed (${seeder.exitCode ?? "timeout"})`);
      const tier = JSON.parse(await readFile(join(tierDir, "seeded-answers.json"), "utf8"));
      if (!Array.isArray(tier) || tier.length !== 12) throw new Error("incomplete per-tier question fixture");
      for (const record of tier) {
        const name = `d${difficulty}-${record.png}`;
        await rename(join(tierDir, record.png), join(seedDir, name));
        records.push({ ...record, png: name });
      }
    }
    await writeFile(join(seedDir, "seeded-answers.json"), JSON.stringify(records), { flag: "wx", mode: 0o600 });

    const urls = {
      EPOCHA_API: api,
      EXAMPLE_URL: mainURL,
      EXAMPLE_NOWORKER_URL: "",
      EXAMPLE_HARD_URL: "",
      EXAMPLE_WORKERFILE_URL: "",
      EXAMPLE_TTL_URL: "",
      SEED_JSON: join(seedDir, "seeded-answers.json"),
      SEED_DIR: seedDir,
      SITE_KEY: mainSite.siteKey,
      HARD_SITE_KEY: "",
      SLOW_SITE_KEY: "",
      TTL_SITE_KEY: "",
    };

    const wantsNoWorker = testName.includes("CSP 禁止 Worker") || testName.includes("错误文案不撑破布局");
    const wantsHard = testName.includes("计算超时是真实约束") || testName.includes("高难度站点：取消");
    const wantsWorkerFile = testName.includes("方案 B：同源 worker");
    const wantsTTL = testName.includes("过期") || testName.includes("TTL");

    if (wantsNoWorker) {
      const port = await freeLoopbackPort();
      const site = await createSite(admin, adminKey, { name: "widget case no-worker", origin: `http://127.0.0.1:${port}` });
      urls.EXAMPLE_NOWORKER_URL = await startApp(scope, {
        binary: exampleappBin, api, site, csp: "no-worker", widgetOrigin: "api", parent: workspace, env: serverEnv, signal, port,
      });
    }
    if (wantsHard) {
      const port = await freeLoopbackPort();
      const origin = `http://127.0.0.1:${port}`;
      const hardSite = await createSite(admin, adminKey, { name: "widget case hard", origin, powBits: 20 });
      const slowSite = await createSite(admin, adminKey, { name: "widget case slow", origin, powBits: 24 });
      urls.HARD_SITE_KEY = hardSite.siteKey;
      urls.SLOW_SITE_KEY = slowSite.siteKey;
      urls.EXAMPLE_HARD_URL = await startApp(scope, {
        binary: exampleappBin, api, site: hardSite, csp: "blob", widgetOrigin: "api", parent: workspace, env: serverEnv, signal, port,
      });
    }
    if (wantsWorkerFile) {
      const port = await freeLoopbackPort();
      const site = await createSite(admin, adminKey, { name: "widget case worker-file", origin: `http://127.0.0.1:${port}` });
      urls.EXAMPLE_WORKERFILE_URL = await startApp(scope, {
        binary: exampleappBin, api, site, csp: "worker-file", widgetOrigin: "self", parent: workspace, env: serverEnv, signal, port,
      });
    }
    if (wantsTTL) {
      const port = await freeLoopbackPort();
      const site = await createSite(admin, adminKey, {
        name: "widget case short-ttl", origin: `http://127.0.0.1:${port}`, responseTTL: 2,
      });
      urls.TTL_SITE_KEY = site.siteKey;
      urls.EXAMPLE_TTL_URL = await startApp(scope, {
        binary: exampleappBin, api, site, csp: "blob", widgetOrigin: "api", parent: workspace, env: serverEnv, signal, port,
      });
    }

    return { ...urls, workspace, close: () => scope.close() };
  } catch (error) {
    await scope.close().catch(() => {});
    throw error;
  }
}

export { stopChild };
