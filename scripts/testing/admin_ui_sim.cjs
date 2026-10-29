#!/usr/bin/env node
"use strict";
// Full admin web-UI simulation against a REAL proxy binary and fake upstreams.
//
//   node scripts/testing/admin_ui_sim.cjs [--keep] [--shots DIR]
//
// It builds cmd/proxy and cmd/fakellm, starts two fake providers and an
// isolated proxy (temp config + bbolt), then drives the real admin pages in a
// headless Chromium through the Chrome DevTools Protocol: login, create a
// pool, job level, targets and a client through the forms, send client
// traffic (fake upstream, no real model), and verify what the overview, usage,
// target key status and trace pages display. Nothing touches a deployed instance.
// Requires an installed Chromium (CHROME_PATH or the Playwright cache).
const assert = require("node:assert/strict");
const { spawn, execFileSync } = require("node:child_process");
const fs = require("node:fs");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const { launch } = require("./cdp_browser.cjs");

// ADMIN_SIM_ROOT lets a regression run build an older source tree (e.g. pre-fix).
const root = process.env.ADMIN_SIM_ROOT || path.resolve(__dirname, "../..");
const args = process.argv.slice(2);
const keep = args.includes("--keep");
const shotsDir = args.includes("--shots") ? path.resolve(args[args.indexOf("--shots") + 1]) : null;
const ADMIN_PASSWORD = "ui-sim-password-not-secret";

const results = [];
function record(status, name, detail = "") {
  results.push({ status, name, detail });
  console.log(`${status.padEnd(4)} ${name}${detail ? " — " + detail : ""}`);
}

function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
    srv.on("error", reject);
  });
}

async function waitHTTP(url, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(url);
      if (r.ok) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 150));
  }
  throw new Error("not ready: " + url);
}

async function startStack(dir) {
  const bin = (name) => path.join(dir, name);
  for (const [out, pkg] of [["llms-proxy", "./cmd/proxy"], ["fakellm", "./cmd/fakellm"]]) {
    execFileSync("go", ["build", "-o", bin(out), pkg], { cwd: root, stdio: "inherit" });
  }
  const procs = [];
  const fakes = [];
  for (const name of ["fake-a", "fake-b"]) {
    const port = await freePort();
    const p = spawn(bin("fakellm"), ["-addr", `127.0.0.1:${port}`], {
      env: { ...process.env, MOCK_NAME: name }, stdio: "ignore",
    });
    procs.push(p);
    fakes.push(`http://127.0.0.1:${port}`);
    await waitHTTP(`http://127.0.0.1:${port}/healthz`);
  }
  const port = await freePort();
  const cfg = {
    server: { bind: `127.0.0.1:${port}`, request_timeout_seconds: 30 },
    data_store: { db_path: path.join(dir, "proxy.db") },
    admin_session: { cookie_name: "admin_sid", secret: "ui-sim-session-secret", ttl_seconds: 3600 },
    logging: { level: "warn", access_log: path.join(dir, "access.log"), error_log: path.join(dir, "error.log") },
  };
  fs.writeFileSync(path.join(dir, "config.json"), JSON.stringify(cfg), { mode: 0o600 });
  const out = fs.openSync(path.join(dir, "proxy.out"), "w");
  const proxy = spawn(bin("llms-proxy"), ["-config", path.join(dir, "config.json")], {
    cwd: dir,
    env: { ...process.env, ADMIN_PASSWORD, LOG_LEVEL: "warn", TRACE_STORE_ENABLED: "true",
      UPSTREAM_ERROR_LOG_PATH: path.join(dir, "upstream-error.log") },
    stdio: ["ignore", out, out],
  });
  procs.push(proxy);
  const base = `http://127.0.0.1:${port}`;
  await waitHTTP(`${base}/healthz`);
  return { base, fakes, stop: () => procs.forEach((p) => p.kill("SIGTERM")) };
}

// clientCall sends a request as an API client (not through the browser).
async function clientCall(base, key, pathname, body) {
  const r = await fetch(base + pathname, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${key}` },
    body: JSON.stringify(body),
  });
  return { status: r.status, text: await r.text() };
}

module.exports = { record, results, clientCall, startStack, ADMIN_PASSWORD };

async function main() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "llms-ui-sim-"));
  let stack, browser;
  try {
    stack = await startStack(dir);
    browser = await launch();
    const { page } = browser;
    const flows = require("./admin_ui_sim_flows.cjs");
    for (const flow of flows.list) {
      try {
        await flow.run({ page, stack, ADMIN_PASSWORD, record, clientCall });
        record("PASS", flow.name);
      } catch (err) {
        record("FAIL", flow.name, err.message.split("\n")[0]);
        if (shotsDir) {
          fs.mkdirSync(shotsDir, { recursive: true });
          await page.screenshot(path.join(shotsDir, `fail-${flow.name}.png`)).catch(() => {});
        }
        if (flow.critical) break;
      }
      if (shotsDir) {
        fs.mkdirSync(shotsDir, { recursive: true });
        await page.screenshot(path.join(shotsDir, `${flow.name}.png`)).catch(() => {});
      }
    }
    if (page.consoleErrors.length) record("FAIL", "browser-console-clean", page.consoleErrors.slice(0, 5).join(" | "));
    else record("PASS", "browser-console-clean");
  } finally {
    if (browser) await browser.close();
    if (stack) stack.stop();
    if (!keep) fs.rmSync(dir, { recursive: true, force: true });
    else console.log("kept", dir);
  }
  const failed = results.filter((r) => r.status === "FAIL").length;
  console.log(`RESULT total=${results.length} fail=${failed}`);
  process.exitCode = failed ? 1 : 0;
}

if (require.main === module) {
  main().catch((err) => {
    console.error(err);
    process.exitCode = 2;
  });
}
