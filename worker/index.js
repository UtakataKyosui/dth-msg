import "../build/wasm_exec.js";
import wasm from "../build/monitor.wasm";
import { readBody } from "./request-body.mjs";

let started = false;
function startGo() {
  if (!started) {
    const go = new globalThis.Go();
    const instance = new WebAssembly.Instance(wasm, go.importObject);
    go.run(instance).catch(() => { started = false; });
    started = true;
  }
}

function config(env) {
  return JSON.stringify({
    adminToken: env.ADMIN_TOKEN,
    clientId: env.GOOGLE_CLIENT_ID,
    clientSecret: env.GOOGLE_CLIENT_SECRET,
    refreshToken: env.GOOGLE_REFRESH_TOKEN,
    deviceName: env.FITBIT_DEVICE_NAME,
    missingHours: Number(env.MISSING_HOURS ?? 24),
    syncMinutes: Number(env.MAX_SYNC_AGE_MINUTES ?? 120),
    pollMinutes: Number(env.MAX_POLL_GAP_MINUTES ?? 15),
    targets: JSON.parse(env.SNS_TARGETS_JSON ?? "[]"),
    publicUrl: env.PUBLIC_URL,
    clerkPublishableKey: env.CLERK_PUBLISHABLE_KEY,
    clerkJwtKey: env.CLERK_JWT_KEY,
    clerkSecretKey: env.CLERK_SECRET_KEY,
    ownerUserId: env.OWNER_CLERK_USER_ID,
    discordClientId: env.DISCORD_CLIENT_ID,
    discordClientSecret: env.DISCORD_CLIENT_SECRET,
    enableX: env.ENABLE_X === "true",
  });
}

function monitor(env) {
  return env.MONITOR.get(env.MONITOR.idFromName("owner"));
}

const jsonHeaders = { "Content-Type": "application/json", "Cache-Control": "no-store" };
const routes = new Set(["/", "/console.js", "/auth/config", "/devices", "/integrations/x", "/integrations/discord", "/integrations/discord/callback", "/settings", "/status", "/messages", "/check"]);
const publicRoutes = new Set(["/", "/console.js", "/auth/config", "/integrations/discord/callback"]);

function goResponse(result) {
  const headers = new Headers();
  for (const [name, values] of Object.entries(result.headers ?? jsonHeaders)) {
    for (const value of Array.isArray(values) ? values : [values]) headers.append(name, value);
  }
  return new Response(result.body, { status: result.status, headers });
}

export default {
  async fetch(request, env) {
    // All authorization, including Clerk JWTs and the owner allowlist, is enforced in Go.
    return monitor(env).fetch(request);
  },
  async scheduled(_event, env, ctx) {
    ctx.waitUntil((async () => {
      const response = await monitor(env).fetch("https://internal/check", {
        method: "POST",
        headers: { Authorization: "Bearer " + env.ADMIN_TOKEN },
      });
      if (!response.ok) throw new Error("Scheduled monitor check failed");
    })());
  },
};

export class HealthMonitor {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.queue = Promise.resolve();
  }

  async fetch(request) {
    try {
      const path = new URL(request.url).pathname;
      if (!routes.has(path)) {
        return new Response('{"error":"not found"}', { status: 404, headers: jsonHeaders });
      }
      if (request.method !== "GET" || !publicRoutes.has(path)) {
        // Use the same Go verifier for Clerk and admin tokens without touching
        // storage, the request body, or the serialized monitoring queue.
        const authorization = await this.runGo(request, "{}", "", "/auth/verify");
        if (authorization.status !== 200) return goResponse(authorization);
      }
      let body = "";
      if (request.method === "PUT" && ["/messages", "/settings"].includes(path)) {
        body = await readBody(request, path === "/settings" ? 65536 : 32768);
      }
      // Serialize state access and the complete check, including outbound sends.
      const result = this.queue.then(() => this.handle(request, body));
      this.queue = result.catch(() => {});
      return await result;
    } catch (error) {
      const status = error.status === 408 || error.status === 413 ? error.status : 500;
      const message = status === 408 ? "body read timed out" : status === 413 ? "body too large" : "monitor unavailable";
      return new Response(JSON.stringify({ error: message }), { status, headers: jsonHeaders });
    }
  }

  async handle(request, body) {
    const state = await this.state.storage.get("state") ?? "{}";
    return goResponse(await this.runGo(request, state, body));
  }

  runGo(request, state, body, path) {
    startGo();
    const url = new URL(request.url);
    return globalThis.dthRequest(
      config(this.env), state, request.method, path ?? url.pathname + url.search, body,
      request.headers.get("Authorization") ?? "",
      path ? async () => { throw new Error("authorization must not write state"); }
        : async (value) => { await this.state.storage.put("state", value); },
      JSON.stringify({ cookie: request.headers.get("Cookie") ?? "", origin: request.headers.get("Origin") ?? "" }),
    );
  }
}
