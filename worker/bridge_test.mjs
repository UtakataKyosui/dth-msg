// Verify the Worker/Wasm boundary without external APIs or deployment.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const source = (await readFile(new URL("./index.js", import.meta.url), "utf8"))
  .replace(/^import .*;\n/gm, "");
const emptyWasm = "const wasm = new WebAssembly.Module(new Uint8Array([0,97,115,109,1,0,0,0]));\n";
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

test("settings bodies stay bounded before reaching Go", async () => {
  const monitor = new HealthMonitor({ storage: { get: async () => "{}" } }, {});
  let called = false;
  globalThis.dthRequest = async () => { called = true; throw Error("unexpected"); };
  const result = await monitor.fetch(new Request("https://example.com/settings", { method: "PUT", body: "x".repeat(65537) }));
  assert.equal(result.status, 413);
  assert.equal(called, false);
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
