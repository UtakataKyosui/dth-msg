import "../build/wasm_exec.js";
import wasm from "../build/monitor.wasm";

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

  fetch(request) {
    // Serialize the complete check, including outbound sends, across awaits.
    const result = this.queue.then(() => this.handle(request));
    this.queue = result.catch(() => {});
    return result;
  }

  async handle(request) {
    const headers = { "Content-Type": "application/json", "Cache-Control": "no-store" };
    try {
      const url = new URL(request.url);
      const path = url.pathname;
      if (!["/", "/console.js", "/auth/config", "/devices", "/integrations/x", "/integrations/discord", "/integrations/discord/callback", "/settings", "/status", "/messages", "/check"].includes(path)) {
        return new Response('{"error":"not found"}', { status: 404, headers });
      }
      // Bound the body before passing it across the Wasm boundary.
      let body = "";
      if (request.method === "PUT" && ["/messages", "/settings"].includes(path)) {
        const reader = request.body?.getReader();
        const chunks = [];
        let length = 0;
        if (reader) {
          for (;;) {
            const { done, value } = await reader.read();
            if (done) break;
            length += value.byteLength;
            if (length > (path === "/settings" ? 65536 : 32768)) {
              await reader.cancel();
              return new Response('{"error":"body too large"}', { status: 413, headers });
            }
            chunks.push(value);
          }
        }
        const bytes = new Uint8Array(length);
        let offset = 0;
        for (const chunk of chunks) {
          bytes.set(chunk, offset);
          offset += chunk.byteLength;
        }
        body = new TextDecoder().decode(bytes);
      }
      startGo();
      const state = await this.state.storage.get("state") ?? "{}";
      const result = await globalThis.dthRequest(
        config(this.env), state, request.method, path + url.search, body,
        request.headers.get("Authorization") ?? "",
        async (value) => { await this.state.storage.put("state", value); },
        JSON.stringify({ cookie: request.headers.get("Cookie") ?? "", origin: request.headers.get("Origin") ?? "" }),
      );
      const responseHeaders = new Headers();
      for (const [name, values] of Object.entries(result.headers ?? headers)) {
        for (const value of Array.isArray(values) ? values : [values]) responseHeaders.append(name, value);
      }
      return new Response(result.body, { status: result.status, headers: responseHeaders });
    } catch {
      return new Response('{"error":"monitor unavailable"}', { status: 500, headers });
    }
  }
}
