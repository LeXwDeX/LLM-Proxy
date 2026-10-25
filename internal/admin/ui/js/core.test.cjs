"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");

function loadCore() {
  function node() {
    return {
      classList: {toggle() {}, contains() { return false; }},
      addEventListener() {},
      setAttribute() {},
      appendChild() {},
      remove() {},
    };
  }
  const document = {
    documentElement: node(),
    getElementById: id => (id === "toastContainer" || id === "confirmOkBtn" ? node() : null),
    querySelectorAll: () => [],
    createElement: () => node(),
  };
  const sandbox = {
    document,
    localStorage: {getItem: () => null, setItem() {}},
    setTimeout() {},
    clearTimeout() {},
    console,
  };
  sandbox.window = sandbox;
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, "core.js"), "utf8"), sandbox);
  return sandbox.AdminApp.modules.core;
}

test("totalTok matches the backend activity contract and counts tool-use prompt tokens", () => {
  const core = loadCore();
  // internal/admin/handlers_activity.go: input = input + cached + cache_creation + tool_use_prompt.
  assert.equal(core.totalTok({
    input_tokens: 2,
    output_tokens: 3,
    cached_tokens: 5,
    cache_creation_tokens: 7,
    tool_use_prompt_tokens: 11,
  }), 28);
  assert.equal(core.totalTok({input_tokens: 2, output_tokens: 3, tool_use_prompt_tokens: 4}), 9);
  assert.equal(core.totalTok({}), 0);
});
