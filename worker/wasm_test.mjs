// Usage: node worker/wasm_test.mjs /path/to/monitor.wasm /path/to/wasm_exec.js
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { generateKeyPairSync, sign } from "node:crypto";
import { createRequire } from "node:module";
import { resolve } from "node:path";

const [wasmPath, runtimePath] = process.argv.slice(2);
if (!wasmPath || !runtimePath) throw Error("Provide the compiled Wasm and matching Go wasm_exec.js paths");
createRequire(import.meta.url)(resolve(runtimePath));
const go = new Go();
const { instance } = await WebAssembly.instantiate(await readFile(wasmPath), go.importObject);
go.run(instance);

const { privateKey, publicKey } = generateKeyPairSync("rsa", { modulusLength: 2048 });
const config = {
  adminToken: "fixture-admin-".repeat(4),
  publicUrl: "https://console.example.com", ownerUserId: "user_owner",
  clerkPublishableKey: "pk_test_" + Buffer.from("test.clerk.accounts.dev$").toString("base64").replace(/=+$/, ""),
  clerkJwtKey: publicKey.export({ type: "spki", format: "pem" }), clerkSecretKey: "sk_test_not_real",
  missingHours: 24, syncMinutes: 120, pollMinutes: 15, targets: [],
};
function jwt(user = "user_owner") {
  const header = Buffer.from(JSON.stringify({ alg: "RS256", typ: "JWT" })).toString("base64url");
  const now = Math.floor(Date.now() / 1000);
  const body = Buffer.from(JSON.stringify({ iss: "https://test.clerk.accounts.dev", azp: config.publicUrl, sub: user, sid: "sess_fixture", exp: now + 60, nbf: now - 60, iat: now, v: 2 })).toString("base64url");
  const message = header + "." + body;
  return message + "." + sign("RSA-SHA256", Buffer.from(message), privateKey).toString("base64url");
}
let state = "{}";
const run = (method, path, body = "", bearer = "", origin = "") => globalThis.dthRequest(
  JSON.stringify(config), state, method, path, body, bearer ? "Bearer " + bearer : "",
  async value => { state = value; }, JSON.stringify({ origin, cookie: "" }),
);
try {
  const page = await run("GET", "/");
  assert.equal(page.status, 200);
  assert.deepEqual(page.headers["Content-Type"], ["text/html; charset=utf-8"]);
  assert.ok(page.headers["Content-Security-Policy"][0].includes("test.clerk.accounts.dev"));
  assert.equal((await run("GET", "/settings")).status, 401);
  assert.equal((await run("GET", "/settings", "", jwt("user_other"))).status, 401);
  const owner = jwt();
  const settings = JSON.stringify({ deviceName: "users/me/pairedDevices/test", targets: [{ id: "slack", kind: "slack", url: "https://hooks.slack.com/services/test" }], messages: { slack: "fixture" } });
  assert.equal((await run("PUT", "/settings", settings, owner, "https://evil.example.com")).status, 403);
  assert.equal((await run("PUT", "/settings", settings, owner, config.publicUrl)).status, 200);
  const stored = JSON.parse(state);
  assert.equal(stored.ownerUserId, "user_owner");
  assert.equal(stored.messages.slack, "fixture");
  assert.equal(JSON.parse((await run("GET", "/settings", "", owner)).body).messages.slack, "fixture");
  assert.ok(!state.includes("sk_test_not_real"));
  const publicConfig = (await run("GET", "/auth/config")).body;
  assert.ok(!publicConfig.includes("sk_test_not_real"));
  console.log("Wasm: Clerk SDK RSA verification, owner restriction, Origin validation, settings persistence, public response headers passed");
  process.exit(0);
} catch (error) {
  console.error(error);
  process.exit(1);
}
