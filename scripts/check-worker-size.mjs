import { spawn } from "node:child_process";
import { appendFile, mkdir, mkdtemp, readdir, rm, stat, writeFile } from "node:fs/promises";
import { resolve, relative, join } from "node:path";
import { fileURLToPath } from "node:url";
import { enforceBudget, measureWorker, PLATFORM_MAX_BYTES, workerBudget } from "./worker-size.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const maxBytes = workerBudget(process.env.WORKER_MAX_BYTES);
const build = join(root, "build");
await mkdir(build, { recursive: true });
const outdir = await mkdtemp(join(build, "worker-size-"));

async function modules(directory, prefix = "") {
  const files = [];
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const name = prefix + entry.name;
    if (entry.isSymbolicLink()) throw new Error("Unexpected symlink in Wrangler output");
    if (entry.isDirectory()) files.push(...await modules(join(directory, entry.name), name + "/"));
    else if (name !== "README.md" && !name.endsWith(".map")) {
      files.push({ name, bytes: (await stat(join(directory, entry.name))).size });
    }
  }
  return files.sort((a, b) => a.name.localeCompare(b.name));
}

try {
  // Use the lockfile-pinned local CLI. dry-run never uploads or provisions resources.
  const args = [resolve(root, "node_modules/wrangler/bin/wrangler.js"), "deploy", "--dry-run", "--autoconfig=false", "--outdir", relative(root, outdir)];
  const output = await new Promise((resolveOutput, reject) => {
    const child = spawn(process.execPath, args, { cwd: root, env: { ...process.env, WRANGLER_SEND_METRICS: "false", NO_COLOR: "1" }, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    child.stdout.on("data", chunk => { stdout += chunk; process.stdout.write(chunk); });
    child.stderr.on("data", chunk => process.stderr.write(chunk));
    child.on("error", reject);
    child.on("close", code => code === 0 ? resolveOutput(stdout) : reject(new Error("Wrangler dry-run failed with exit code " + code)));
  });
  const report = { ...measureWorker(await modules(outdir), output), maxBytes, platformMaxBytes: PLATFORM_MAX_BYTES };
  await writeFile(join(build, "worker-size.json"), JSON.stringify(report, null, 2) + "\n");
  const summary = `Worker: ${report.totalBytes} bytes (${(report.totalBytes / 1024 / 1024).toFixed(2)} MiB) / ${maxBytes} bytes. Wasm: ${report.wasmBytes} bytes. gzip (reference): ${report.gzipKiB} KiB.`;
  console.log(summary);
  if (process.env.GITHUB_STEP_SUMMARY) await appendFile(process.env.GITHUB_STEP_SUMMARY, summary + "\n");
  enforceBudget(report, maxBytes);
} finally {
  await rm(outdir, { recursive: true, force: true });
}
