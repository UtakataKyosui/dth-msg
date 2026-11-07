import assert from "node:assert/strict";
import { test } from "node:test";
import { enforceBudget, measureWorker, PLATFORM_MAX_BYTES, workerBudget } from "./worker-size.mjs";

test("size covers JavaScript plus all Wasm modules using exact uncompressed bytes", () => {
  const files = [{ name: "index.js", bytes: 1024 }, { name: "one.wasm", bytes: 2048 }, { name: "two.wasm", bytes: 3072 }];
  const report = measureWorker(files, "Total Upload: 6.00 KiB / gzip: 1.23 KiB\n");
  assert.equal(report.totalBytes, 6144);
  assert.equal(report.wasmBytes, 5120);
  assert.equal(report.gzipKiB, 1.23);
});

test("exact budget is accepted but a single byte above it fails regardless of gzip size", () => {
  const report = { totalBytes: PLATFORM_MAX_BYTES, gzipKiB: 0.01 };
  assert.doesNotThrow(() => enforceBudget(report, PLATFORM_MAX_BYTES));
  assert.throws(() => enforceBudget({ ...report, totalBytes: PLATFORM_MAX_BYTES + 1 }, PLATFORM_MAX_BYTES), /exceeds/);
  assert.throws(() => enforceBudget(report, 1024), /exceeds/);
});

test("missing modules, missing size output and accounting changes fail closed", () => {
  const files = [{ name: "index.js", bytes: 1024 }, { name: "app.wasm", bytes: 1024 }];
  assert.throws(() => measureWorker(files, ""), /do not match/);
  assert.throws(() => measureWorker(files, "Total Upload: 1.00 KiB / gzip: 0.50 KiB"), /do not match/);
  assert.throws(() => measureWorker(files.slice(0, 1), "Total Upload: 1.00 KiB / gzip: 0.50 KiB"), /include/);
  assert.throws(() => measureWorker([{ name: "app.wasm", bytes: NaN }, files[0]], ""), /Invalid/);
});

test("a local budget can be stricter but cannot exceed the documented platform limit", () => {
  assert.equal(workerBudget(), 67108864);
  assert.equal(workerBudget("1048576"), 1048576);
  for (const value of ["0", "-1", "1.5", "NaN", "67108865"]) assert.throws(() => workerBudget(value));
});
