import { createServer, request as httpRequest } from "node:http";
import { URL } from "node:url";

export async function createFailureFixture(upstream) {
  const target = new URL(upstream);
  if (target.protocol !== "http:" || target.hostname !== "127.0.0.1") {
    throw new Error("admin failure proxy requires a 127.0.0.1 HTTP upstream");
  }

  let apiUnavailable = false;
  let blockedRequests = 0;
  const sockets = new Set();
  const server = createServer((incoming, outgoing) => {
    const path = incoming.url || "/";
    if (apiUnavailable && path.startsWith("/api/admin/")) {
      blockedRequests++;
      incoming.socket.destroy();
      return;
    }

    const upstreamRequest = httpRequest({
      protocol: target.protocol,
      hostname: target.hostname,
      port: target.port,
      method: incoming.method,
      path,
      headers: { ...incoming.headers, host: target.host, connection: "close" },
      agent: false,
    }, (upstreamResponse) => {
      outgoing.writeHead(upstreamResponse.statusCode || 502, upstreamResponse.headers);
      upstreamResponse.pipe(outgoing);
    });
    upstreamRequest.on("error", () => {
      if (!outgoing.destroyed) outgoing.destroy();
    });
    incoming.on("error", () => upstreamRequest.destroy());
    incoming.pipe(upstreamRequest);
  });
  server.on("connection", (socket) => {
    sockets.add(socket);
    socket.on("close", () => sockets.delete(socket));
  });

  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      server.removeListener("error", reject);
      resolve();
    });
  });
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("failure proxy did not bind TCP");
  const baseURL = `http://127.0.0.1:${address.port}`;
  let closing;

  return {
    baseURL,
    get blockedRequests() { return blockedRequests; },
    setAPIUnavailable(value) { apiUnavailable = Boolean(value); },
    async close() {
      if (closing) return closing;
      closing = new Promise((resolve, reject) => {
        server.close((error) => error ? reject(error) : resolve());
        for (const socket of sockets) socket.destroy();
      });
      return closing;
    },
  };
}
