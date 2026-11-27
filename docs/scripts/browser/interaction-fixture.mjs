// Real Firefox profiles and HTTP delivery; no DOM or matchMedia replacement.
import { spawn } from "node:child_process";
import { writeFile, readFile } from "node:fs/promises";
import { join } from "node:path";
import { createServer } from "node:http";
import net from "node:net";
import { createHash } from "node:crypto";
import { BiDi } from "./bidi.mjs";
import { ResourceScope, waitFor } from "./widget_fixture.mjs";

export async function firefoxPreferences(parentDir, preferences) {
  const scope = new ResourceScope({ parentDir });
  let bidi;
  try {
    const profile = await scope.createWorkspace();
    await writeFile(join(profile, "user.js"), Object.entries(preferences).map(([k, v]) =>
      `user_pref(${JSON.stringify(k)}, ${JSON.stringify(v)});`).join("\n"));
    const reservation = net.createServer();
    await new Promise((ok, no) => reservation.once("error", no).listen(0, "127.0.0.1", ok));
    const port = reservation.address().port;
    await new Promise((ok, no) => reservation.close(e => e ? no(e) : ok()));
    const child = scope.add(spawn(process.env.FIREFOX_BIN || "firefox", [
      "--headless", "--no-remote", "--profile", profile, "--remote-debugging-port", String(port),
    ], { stdio: ["ignore", "pipe", "pipe"] }));
    let output = "", failure;
    child.on("error", e => { failure = e; });
    for (const stream of [child.stdout, child.stderr]) stream.on("data", b => { output = (output + b).slice(-16384); });
    await waitFor(() => {
      if (failure) throw failure;
      if (child.exitCode !== null) throw new Error(`Firefox exited: ${child.exitCode}`);
      return output.includes("WebDriver BiDi listening");
    }, { label: "isolated Firefox BiDi startup", timeoutMs: 20000 });
    bidi = await new BiDi(`ws://127.0.0.1:${port}/session`).connect();
    await bidi.startSession();
    return { bidi, async close() {
      try { await bidi.send("session.end"); } finally { bidi.close(); await scope.close(); }
    } };
  } catch (e) {
    bidi?.close();
    await scope.close();
    throw e;
  }
}

export async function sriFixture() {
  const script = await readFile(new URL("../../../web/widget/v1.js", import.meta.url));
  const integrity = "sha384-" + createHash("sha384").update(script).digest("base64");
  let apiRequests = 0;
  const server = createServer((req, res) => {
    if (req.url === "/widget/v1.js") {
      res.writeHead(200, { "Content-Type": "text/javascript" }); res.end(script); return;
    }
    if (req.url.startsWith("/api/")) { apiRequests++; res.writeHead(503); res.end(); return; }
    const hash = req.url === "/valid" ? integrity : "sha384-" + Buffer.alloc(48).toString("base64");
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
    res.end(`<!doctype html><meta charset="utf-8"><div id="widget"></div><p id="status">loading</p>
      <form><input name="epocha-response" value=""><button disabled>Submit</button></form>
      <script src="/widget/v1.js" integrity="${hash}" onerror="document.querySelector('#status').textContent='integrity-failed'"></script>
      <script>if(window.EpoCHA){document.querySelector('#status').textContent='loaded';
        EpoCHA.render('#widget',{apiBase:location.origin,siteKey:'pk_sri_fixture',fieldName:'',maxAutoRetry:0,
          onSuccess:function(token){document.querySelector('[name=epocha-response]').value=token;document.querySelector('button').disabled=false;}});}</script>`);
  });
  await new Promise((ok, no) => server.once("error", no).listen(0, "127.0.0.1", ok));
  return { url: `http://127.0.0.1:${server.address().port}`, get apiRequests() { return apiRequests; },
    close: () => new Promise((ok, no) => server.close(e => e ? no(e) : ok())) };
}
