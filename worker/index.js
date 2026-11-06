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
    targets: JSON.parse(env.SNS_TARGETS_JSON),
  });
}

function monitor(env) {
  return env.MONITOR.get(env.MONITOR.idFromName("owner"));
}

async function authorized(request, env) {
  const value = request.headers.get("Authorization") ?? "";
  if (!env.ADMIN_TOKEN || !value.startsWith("Bearer ")) return false;
  const encoder = new TextEncoder();
  const [got, want] = await Promise.all([
    crypto.subtle.digest("SHA-256", encoder.encode(value.slice(7))),
    crypto.subtle.digest("SHA-256", encoder.encode(env.ADMIN_TOKEN)),
  ]);
  const a = new Uint8Array(got);
  const b = new Uint8Array(want);
  let difference = 0;
  for (let i = 0; i < a.length; i++) difference |= a[i] ^ b[i];
  return difference === 0;
}

export default {
  async fetch(request, env) {
    if (!await authorized(request, env)) {
      return new Response('{"error":"unauthorized"}', {
        status: 401,
        headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
      });
    }
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
      const path = new URL(request.url).pathname;
      if (!["/status", "/messages", "/check"].includes(path)) {
        return new Response('{"error":"not found"}', { status: 404, headers });
      }
      // Bound the body before passing it across the Wasm boundary.
      let body = "";
      if (request.method === "PUT" && path === "/messages") {
        const reader = request.body?.getReader();
        const chunks = [];
        let length = 0;
        if (reader) {
          for (;;) {
            const { done, value } = await reader.read();
            if (done) break;
            length += value.byteLength;
            if (length > 32768) {
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
        config(this.env), state, request.method, path, body,
        request.headers.get("Authorization") ?? "",
        (value) => this.state.storage.put("state", value),
      );
      return new Response(result.body, { status: result.status, headers });
    } catch {
      return new Response('{"error":"monitor unavailable"}', { status: 500, headers });
    }
  }
}
