"use strict";
// Minimal, dependency-free Chrome DevTools Protocol driver for admin UI
// simulation. Uses Node's built-in WebSocket/fetch and an already installed
// Chromium (CHROME_PATH, or the Playwright browser cache). Nothing is
// downloaded. Pages are driven by evaluating JS, so actions go through the
// same DOM events a user's clicks and typing would fire.
const { spawn } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

function findChrome() {
  if (process.env.CHROME_PATH) return process.env.CHROME_PATH;
  const roots = [
    path.join(os.homedir(), "Library/Caches/ms-playwright"),
    path.join(os.homedir(), ".cache/ms-playwright"),
  ];
  const candidates = [];
  for (const root of roots) {
    if (!fs.existsSync(root)) continue;
    for (const dir of fs.readdirSync(root).sort().reverse()) {
      const base = path.join(root, dir);
      candidates.push(
        path.join(base, "chrome-headless-shell-mac-arm64/chrome-headless-shell"),
        path.join(base, "chrome-headless-shell-mac-x64/chrome-headless-shell"),
        path.join(base, "chrome-headless-shell-linux64/chrome-headless-shell"),
        path.join(base, "chrome-linux/chrome"),
      );
    }
  }
  candidates.push("/usr/bin/chromium", "/usr/bin/chromium-browser", "/usr/bin/google-chrome");
  return candidates.find((p) => fs.existsSync(p)) || null;
}

class Page {
  constructor(ws) {
    this.ws = ws;
    this.nextId = 1;
    this.pending = new Map();
    this.listeners = [];
    this.consoleErrors = [];
    this.dialogs = [];
    ws.addEventListener("message", (ev) => {
      const msg = JSON.parse(typeof ev.data === "string" ? ev.data : Buffer.from(ev.data).toString());
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id);
        this.pending.delete(msg.id);
        if (msg.error) reject(new Error(`${msg.error.message} (${msg.error.code})`));
        else resolve(msg.result);
        return;
      }
      if (msg.method === "Runtime.exceptionThrown") {
        const d = msg.params.exceptionDetails;
        this.consoleErrors.push(`exception: ${(d.exception && d.exception.description) || d.text}`);
      } else if (msg.method === "Runtime.consoleAPICalled" && msg.params.type === "error") {
        this.consoleErrors.push("console.error: " + msg.params.args.map((a) => a.value ?? a.description ?? "").join(" "));
      } else if (msg.method === "Page.javascriptDialogOpening") {
        // confirm()/alert() would block the page; accept and record them.
        this.dialogs.push(`${msg.params.type}: ${msg.params.message}`);
        this.send("Page.handleJavaScriptDialog", { accept: true, promptText: "" }).catch(() => {});
      }
      for (const l of this.listeners) l(msg);
    });
  }

  send(method, params = {}) {
    const id = this.nextId++;
    this.ws.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id);
          reject(new Error(`CDP ${method} timed out`));
        }
      }, 30000);
    });
  }

  waitEvent(method, timeoutMs = 15000) {
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.listeners = this.listeners.filter((l) => l !== fn);
        reject(new Error(`timeout waiting for ${method}`));
      }, timeoutMs);
      const fn = (msg) => {
        if (msg.method === method) {
          clearTimeout(timer);
          this.listeners = this.listeners.filter((l) => l !== fn);
          resolve(msg.params);
        }
      };
      this.listeners.push(fn);
    });
  }

  async navigate(url) {
    const loaded = this.waitEvent("Page.loadEventFired");
    await this.send("Page.navigate", { url });
    await loaded;
  }

  // eval runs an expression (may be async) in the page and returns its value.
  async eval(expression) {
    const res = await this.send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true, userGesture: true });
    if (res.exceptionDetails) {
      const d = res.exceptionDetails;
      throw new Error(`page eval failed: ${(d.exception && d.exception.description) || d.text}\n  in: ${expression.slice(0, 300)}`);
    }
    return res.result.value;
  }

  // waitFor polls a boolean expression until true.
  async waitFor(expression, { timeoutMs = 10000, what = expression } = {}) {
    const deadline = Date.now() + timeoutMs;
    let last;
    while (Date.now() < deadline) {
      try {
        last = await this.eval(`!!(${expression})`);
        if (last) return;
      } catch (err) {
        last = err.message;
      }
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error(`timeout waiting for: ${what} (last: ${last})`);
  }

  async screenshot(file) {
    const { data } = await this.send("Page.captureScreenshot", { format: "png" });
    fs.writeFileSync(file, Buffer.from(data, "base64"));
  }
}

async function launch({ width = 1440, height = 960 } = {}) {
  const chrome = findChrome();
  if (!chrome) throw new Error("no Chromium found: set CHROME_PATH");
  const userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), "cdp-profile-"));
  const proc = spawn(chrome, [
    "--headless=new", "--remote-debugging-port=0", `--user-data-dir=${userDataDir}`,
    "--no-first-run", "--no-default-browser-check", "--disable-gpu", "--disable-extensions",
    `--window-size=${width},${height}`, "about:blank",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  const browserWS = await new Promise((resolve, reject) => {
    let buf = "";
    const timer = setTimeout(() => reject(new Error("Chromium did not start: " + buf.slice(-500))), 20000);
    proc.stderr.on("data", (d) => {
      buf += d.toString();
      const m = buf.match(/DevTools listening on (ws:\/\/\S+)/);
      if (m) {
        clearTimeout(timer);
        resolve(m[1]);
      }
    });
    proc.on("exit", (code) => reject(new Error(`Chromium exited ${code}: ${buf.slice(-500)}`)));
  });
  const port = new URL(browserWS).port;
  const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
  const pageTarget = targets.find((t) => t.type === "page");
  const ws = new WebSocket(pageTarget.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => {
    ws.addEventListener("open", resolve, { once: true });
    ws.addEventListener("error", reject, { once: true });
  });
  const page = new Page(ws);
  await page.send("Page.enable");
  await page.send("Runtime.enable");
  await page.send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 1, mobile: false });
  const close = async () => {
    try { ws.close(); } catch {}
    proc.kill("SIGTERM");
    await new Promise((r) => setTimeout(r, 200));
    fs.rmSync(userDataDir, { recursive: true, force: true });
  };
  return { page, close, chrome };
}

module.exports = { launch, findChrome, Page };
