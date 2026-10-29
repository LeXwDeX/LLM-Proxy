"use strict";
// internal/admin/AGENTS.md: other pages bind a split page only through its main
// module (js/pages/<page>/<page>.js), never through its internal parts.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

test("cross-page bindings go through the page's main module", () => {
  const root = path.join(__dirname, "js", "pages");
  const owner = {}; // module name -> {page, main}
  const files = [];
  for (const page of fs.readdirSync(root)) {
    for (const file of fs.readdirSync(path.join(root, page))) {
      if (!file.endsWith(".js")) continue;
      const src = fs.readFileSync(path.join(root, page, file), "utf8");
      files.push({page, file, src});
      for (const m of src.matchAll(/AdminApp\.register\("([^"]+)"/g)) owner[m[1]] = {page, main: file === page + ".js"};
    }
  }
  const violations = [];
  for (const {page, file, src} of files) {
    for (const m of src.matchAll(/app\.(?:bind|initializeModule)\("([^"]+)"/g)) {
      const o = owner[m[1]];
      if (o && o.page !== page && !o.main) violations.push(`${page}/${file} -> ${m[1]} (${o.page})`);
    }
  }
  assert.deepEqual(violations, []);
});
