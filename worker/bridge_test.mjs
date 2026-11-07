// Verify the Worker/Wasm boundary without external APIs or deployment.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { readBody } from "./request-body.mjs";

const source = (await readFile(new URL("./index.js", import.meta.url), "utf8"))
  .replace(/^import .*;\n/gm, "");
const emptyWasm = "const wasm = new WebAssembly.Module(new Uint8Array([0,97,115,109,1,0,0,0]));\n" + `import { readBody } from ${JSON.stringify(new URL("./request-body.mjs", import.meta.url).href)};\n`;
globalThis.Go = class { importObject = {}; async run() {} };
const { HealthMonitor, default: worker } = await import("data:text/javascript;base64," + Buffer.from(emptyWasm + source).toString("base64"));

test("Clerk bearer, browser cookie, origin, query and all response headers cross the boundary", async () => {
  const storage = new Map();
  const state = { storage: { get: async key => storage.get(key), put: async (key, value) => storage.set(key, value) } };
  const monitor = new HealthMonitor(state, { SNS_TARGETS_JSON: "[]", CLERK_PUBLISHABLE_KEY: "public-fixture", ENABLE_X: "false" });
  let calls = 0;
  globalThis.dthRequest = async (config, stored, method, path, body, auth, save, headers) => {
    calls++;
    assert.equal(JSON.parse(config).clerkPublishableKey, "public-fixture");
    assert.equal(JSON.parse(config).enableX, false);
    assert.equal(stored, "{}");
    assert.equal(method, "GET");
    assert.equal(path, "/integrations/discord/callback?code=test&state=random");
    assert.equal(auth, "Bearer clerk-jwt");
    assert.equal(body, "");
    assert.deepEqual(JSON.parse(headers), { cookie: "dth_discord_oauth=browser", origin: "https://console.example.com" });
    await save('{"saved":true}');
    return { status: 303, body: "", headers: { Location: ["/?integration=connected"], "Set-Cookie": ["first=1; HttpOnly", "second=2; HttpOnly"], "Cache-Control": ["no-store"] } };
  };
  const response = await monitor.fetch(new Request("https://console.example.com/integrations/discord/callback?code=test&state=random", {
    headers: { Authorization: "Bearer clerk-jwt", Cookie: "dth_discord_oauth=browser", Origin: "https://console.example.com" },
  }));
  assert.equal(response.status, 303);
  assert.equal(response.headers.get("Location"), "/?integration=connected");
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(response.headers.getSetCookie().length, 2);
  assert.equal(storage.get("state"), '{"saved":true}');
  assert.equal(calls, 1);
});

test("settings bodies stay bounded after authorization and before state access", async () => {
  const monitor = new HealthMonitor({ storage: { get: async () => "{}" } }, {});
  const paths = [];
  globalThis.dthRequest = async (_config, _state, _method, path) => { paths.push(path); return { status: 200, body: "{}" }; };
  const result = await monitor.fetch(new Request("https://example.com/settings", { method: "PUT", body: "x".repeat(65537) }));
  assert.equal(result.status, 413);
  assert.deepEqual(paths, ["/auth/verify"]);
});

function streamingRequest(path, authorization = "") {
  let controller;
  const body = new ReadableStream({ start(value) { controller = value; } }, { highWaterMark: 0 });
  const request = new Request("https://example.com" + path, { method: "PUT", body, duplex: "half", headers: { Authorization: authorization } });
  return { request, controller };
}

function deadline(promise, ms = 1000) {
  let timer;
  return Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(Error("monitor check blocked by request body")), ms); })]).finally(() => clearTimeout(timer));
}

for (const path of ["/settings", "/messages"]) {
  test("unauthenticated unfinished " + path + " never reads its body or blocks a check", async () => {
    const monitor = new HealthMonitor({ storage: { get: async () => "{}" } }, {});
    const paths = [];
    globalThis.dthRequest = async (_config, _state, _method, path, _body, auth) => {
      paths.push(path);
      return { status: auth === "Bearer admin" ? 200 : 401, body: "{}" };
    };
    const { request, controller } = streamingRequest(path);
    try {
      const rejected = monitor.fetch(request);
      const check = monitor.fetch(new Request("https://example.com/check", { method: "POST", headers: { Authorization: "Bearer admin" } }));
      assert.equal((await deadline(rejected)).status, 401);
      assert.equal(request.bodyUsed, false);
      assert.equal((await deadline(check)).status, 200);
      assert.deepEqual(paths, ["/auth/verify", "/auth/verify", "/check"]);
    } finally { controller.close(); }
  });
}

test("an authorized unfinished upload stays outside the monitoring queue", async () => {
  const monitor = new HealthMonitor({ storage: { get: async () => "{}" } }, {});
  const paths = [];
  let authorized;
  const preflight = new Promise(resolve => { authorized = resolve; });
  globalThis.dthRequest = async (_config, _state, method, path) => {
    paths.push(path);
    if (path === "/auth/verify" && method === "PUT") authorized();
    return { status: 200, body: "{}" };
  };
  const { request, controller } = streamingRequest("/settings", "Bearer clerk-jwt");
  const upload = monitor.fetch(request);
  try {
    await preflight;
    const check = await deadline(monitor.fetch(new Request("https://example.com/check", { method: "POST", headers: { Authorization: "Bearer admin" } })));
    assert.equal(check.status, 200);
    assert.equal(paths.includes("/settings"), false);
    controller.enqueue(new TextEncoder().encode("{}")); controller.close();
    assert.equal((await upload).status, 200);
    assert.ok(paths.indexOf("/check") < paths.indexOf("/settings"));
  } finally { if (request.body?.locked) { try { controller.close(); } catch {} } }
});

test("a whole-body deadline cancels a stalled reader even when cancellation never settles", async () => {
  let cancelled = false;
  const body = new ReadableStream({ cancel() { cancelled = true; return new Promise(() => {}); } });
  const request = new Request("https://example.com/settings", { method: "PUT", body, duplex: "half" });
  await assert.rejects(deadline(readBody(request, 65536, 5)), error => error.status === 408);
  assert.equal(cancelled, true);
});

test("readBody preserves UTF-8 split between chunks and accepts the exact byte limit", async () => {
  const bytes = new TextEncoder().encode("あ");
  const body = new ReadableStream({ start(controller) { controller.enqueue(bytes.subarray(0, 1)); controller.enqueue(bytes.subarray(1)); controller.close(); } });
  assert.equal(await readBody(new Request("https://example.com/settings", { method: "PUT", body, duplex: "half" }), 3), "あ");
});

test("checks and state mutations remain serialized after their body is complete", async () => {
  const monitor = new HealthMonitor({ storage: { get: async () => "{}" } }, {});
  let release;
  let started;
  const checkStarted = new Promise(resolve => { started = resolve; });
  const checkDone = new Promise(resolve => { release = resolve; });
  const paths = [];
  globalThis.dthRequest = async (_config, _state, _method, path) => {
    if (path !== "/auth/verify") paths.push(path);
    if (path === "/check") { started(); await checkDone; }
    return { status: 200, body: "{}" };
  };
  const check = monitor.fetch(new Request("https://example.com/check", { method: "POST" }));
  await checkStarted;
  const upload = monitor.fetch(new Request("https://example.com/settings", { method: "PUT", body: "{}" }));
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(paths, ["/check"]);
  release(); await check; await upload;
  assert.deepEqual(paths, ["/check", "/settings"]);
});

test("outer Worker forwards session authorization to Go and cron uses only the admin token", async () => {
  const requests = [];
  const env = { ADMIN_TOKEN: "test-admin", MONITOR: { idFromName: name => name, get: () => ({ fetch: async (...args) => { requests.push(args); return new Response("ok"); } }) } };
  await worker.fetch(new Request("https://example.com/devices", { headers: { Authorization: "Bearer clerk-jwt" } }), env);
  assert.equal(requests[0][0].headers.get("Authorization"), "Bearer clerk-jwt");
  let scheduled;
  await worker.scheduled({}, env, { waitUntil: promise => { scheduled = promise; } });
  await scheduled;
  assert.equal(requests[1][1].headers.Authorization, "Bearer test-admin");
});
