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

const scriptMap = {
  core: "core/core", schema: "core/schema", navigation: "core/navigation", bootstrap: "core/bootstrap",
  overview: "pages/overview/overview", targets: "pages/targets/targets", pools: "pages/pools/pools",
  clients: "pages/clients/clients", pricing: "pages/pricing/pricing", usage: "pages/usage/usage",
  audit_trace: "pages/audit_trace/audit_trace", users: "pages/users/users",
  catalog: "pages/catalog/catalog", quota: "pages/quota/quota",
};

// Folds consecutive script tags of one page directory (a page split into
// several files) back to the single pages/<name>/<name>.js tag in the golden.
function collapsePageScripts(html) {
  const tag = /^(\s*)<script src="\/admin\/ui\/js\/pages\/([a-z_]+)\/[^"/]+\.js"><\/script>$/;
  let prev = "";
  const out = [];
  for (const line of html.split("\n")) {
    const m = tag.exec(line);
    if (m) {
      if (prev !== m[2]) out.push(`${m[1]}<script src="/admin/ui/js/pages/${m[2]}/${m[2]}.js"></script>`);
      prev = m[2];
      continue;
    }
    prev = "";
    out.push(line);
  }
  return out.join("\n");
}

test("Node assembly equals the golden index.html modulo script paths", () => {
  const golden = fs.readFileSync(path.join(__dirname, "..", "testdata", "index.golden.html"), "utf8")
    .replace(/\/admin\/ui\/js\/([a-z_]+)\.js"/g, (_, name) => {
      assert.ok(scriptMap[name], `unknown script ${name}`);
      return `/admin/ui/js/${scriptMap[name]}.js"`;
    });
  assert.equal(collapsePageScripts(assembleAdminHTML()), golden);
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
