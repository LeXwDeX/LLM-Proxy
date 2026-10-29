"use strict";
// The Node include helper must produce the same HTML as the Go assembly
// (ui_assemble.go); Go compares its output against this helper directly.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { assembleAdminHTML } = require("../../../scripts/testing/assemble_admin_html.cjs");

test("Node assembly leaves no include marker and references every script exactly once", () => {
  const html = assembleAdminHTML();
  assert.ok(!html.includes("@include"), "leftover include marker");
  const jsRoot = path.join(__dirname, "js");
  const files = [];
  for (const dir of ["core", "pages"]) {
    const walk = (d) => fs.readdirSync(d, { withFileTypes: true }).forEach((e) => {
      const full = path.join(d, e.name);
      if (e.isDirectory()) walk(full);
      else if (e.name.endsWith(".js") && !e.name.endsWith(".test.js")) files.push(path.relative(jsRoot, full).split(path.sep).join("/"));
    });
    walk(path.join(jsRoot, dir));
  }
  const tags = [...html.matchAll(/<script src="\/admin\/ui\/js\/([^"]+)"><\/script>/g)].map((m) => m[1]);
  assert.deepEqual([...tags].sort(), files.sort());
  assert.equal(new Set(tags).size, tags.length, "duplicate script tag");
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
