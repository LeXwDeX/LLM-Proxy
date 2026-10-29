"use strict";
// The Node include helper must produce the same HTML as the Go assembly
// (ui_assemble.go). Go pins its output to testdata/index.golden.html; this test
// pins the Node output to the same golden with the same script-path mapping.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { assembleAdminHTML } = require("../../../scripts/testing/assemble_admin_html.cjs");

// A page split into several files lists them in load order.
const scriptMap = {
  core: ["core/core"], schema: ["core/schema"], navigation: ["core/navigation"], bootstrap: ["core/bootstrap"],
  overview: ["pages/overview/overview"], pools: ["pages/pools/pools"],
  targets: ["targets", "health", "detail", "key-list", "model-mappings", "edit-modal", "save", "list"].map((n) => `pages/targets/${n}`),
  clients: ["pages/clients/clients"], usage: ["pages/usage/usage"],
  pricing: ["pages/pricing/pricing-shared", "pages/pricing/pricing-rates", "pages/pricing/pricing-periods",
    "pages/pricing/pricing-list", "pages/pricing/pricing-modal", "pages/pricing/pricing"],
  audit_trace: ["pages/audit_trace/audit_trace"], users: ["pages/users/users"],
  catalog: ["pages/catalog/catalog"],
  quota: ["pages/quota/quota-model", "pages/quota/quota-data", "pages/quota/quota-groups", "pages/quota/quota-levels",
    "pages/quota/quota-summary", "pages/quota/quota-drawer", "pages/quota/quota-sync", "pages/quota/quota"],
};

test("Node assembly equals the golden index.html modulo script paths", () => {
  const golden = fs.readFileSync(path.join(__dirname, "..", "testdata", "index.golden.html"), "utf8")
    .replace(/<script src="\/admin\/ui\/js\/([a-z_]+)\.js"><\/script>/g, (_, name) => {
      assert.ok(scriptMap[name], `unknown script ${name}`);
      return scriptMap[name].map((rel) => `<script src="/admin/ui/js/${rel}.js"></script>`).join("\n  ");
    });
  assert.equal(assembleAdminHTML(), golden);
});

test("include errors fail loudly", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "asm-"));
  const write = (rel, text) => { fs.mkdirSync(path.dirname(path.join(dir, rel)), { recursive: true }); fs.writeFileSync(path.join(dir, rel), text); };
  write("index.html", "a\n  <!-- @include p/x.html -->\nb\n");
  write("p/x.html", "  X\n");
  assert.equal(assembleAdminHTML(dir), "a\n  X\nb\n");
  write("index.html", "<!-- @include nope.html -->\n");
  assert.throws(() => assembleAdminHTML(dir));
  write("index.html", "<!-- @include a.html -->\n"); write("a.html", "<!-- @include b.html -->\n"); write("b.html", "<!-- @include a.html -->\n");
  assert.throws(() => assembleAdminHTML(dir), /cycle/);
  write("index.html", "x <!-- @include a.html -->\n"); write("a.html", "A\n");
  assert.throws(() => assembleAdminHTML(dir), /leftover/);
  write("index.html", "<!-- @include ../x.html -->\n");
  assert.throws(() => assembleAdminHTML(dir), /invalid/);
  write("index.html", "<!-- @include a.html -->\n"); write("a.html", "A");
  assert.throws(() => assembleAdminHTML(dir), /newline/);
  write("index.html", "<!-- @include a.html -->\n<!-- @include a.html -->\n"); write("a.html", "A\n");
  assert.throws(() => assembleAdminHTML(dir), /more than once/);
  write("index.html", "<!-- @include a.html -->\n"); write("a.html", "\uFEFFA\n");
  assert.throws(() => assembleAdminHTML(dir), /BOM/);
});
