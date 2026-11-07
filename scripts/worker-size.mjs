// Checked 2026-11-07: both Free and Paid limit the entire uncompressed Worker
// to 64 MiB. gzip size is informational, not a platform limit.
// https://developers.cloudflare.com/workers/platform/limits/#worker-size
export const PLATFORM_MAX_BYTES = 64 * 1024 * 1024;

export function workerBudget(value = String(PLATFORM_MAX_BYTES)) {
  if (!/^\d+$/.test(value)) throw new Error("WORKER_MAX_BYTES must be a positive integer");
  const bytes = Number(value);
  if (!Number.isSafeInteger(bytes) || bytes <= 0 || bytes > PLATFORM_MAX_BYTES) {
    throw new Error("WORKER_MAX_BYTES must be between 1 and " + PLATFORM_MAX_BYTES);
  }
  return bytes;
}

export function measureWorker(files, output) {
  if (!files.some(file => file.name.endsWith(".wasm")) || !files.some(file => file.name.endsWith(".js"))) {
    throw new Error("Wrangler output must include JavaScript and Wasm modules");
  }
  const totalBytes = files.reduce((sum, file) => {
    if (!Number.isSafeInteger(file.bytes) || file.bytes < 0) throw new Error("Invalid module size");
    return sum + file.bytes;
  }, 0);
  const matches = [...output.matchAll(/Total Upload:\s*(\d+\.\d+) KiB\s*\/\s*gzip:\s*(\d+\.\d+) KiB/g)];
  if (matches.length !== 1 || (totalBytes / 1024).toFixed(2) !== matches[0][1]) {
    throw new Error("Module byte sizes do not match Wrangler Total Upload; review the size checker before deploying");
  }
  return { totalBytes, wasmBytes: files.filter(file => file.name.endsWith(".wasm")).reduce((sum, file) => sum + file.bytes, 0), gzipKiB: Number(matches[0][2]), files };
}

export function enforceBudget(report, maxBytes) {
  if (report.totalBytes > maxBytes) {
    throw new Error(`Worker size ${report.totalBytes} bytes exceeds the ${maxBytes}-byte budget`);
  }
}
