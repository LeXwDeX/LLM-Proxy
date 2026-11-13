"use strict";
// Loads the real core/core.js into a throwaway vm context with a stub DOM and returns its exports.
// Unit tests of modules that run outside the full harness (their own fake document) use this to get the
// shared helpers, e.g. createTableSort, without re-implementing them. loadCoreEnv also returns the stub
// document, for tests that need to steer it (e.g. document.activeElement).
const vm = require("node:vm");
const { coreSource } = require("./admin_ui_scripts.cjs");

function stubNode(tag) {
  const listeners = {};
  return {
    tagName: tag, children: [], attrs: {}, listeners, style: {}, textContent: "",
    classList: { toggle() {}, contains() { return false; }, add() {}, remove() {} },
    addEventListener(type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
    setAttribute(key, value) { this.attrs[key] = value; },
    appendChild(child) { this.children.push(child); return child; },
    remove() {},
  };
}

function loadCoreEnv() {
  const body = stubNode("body");
  const byId = { toastContainer: stubNode("div"), confirmOkBtn: stubNode("button") };
  const document = {
    body,
    documentElement: stubNode("html"),
    hidden: false,
    getElementById: (id) => byId[id] || null,
    querySelectorAll: () => [],
    querySelector: () => null,
    createElement: stubNode,
    addEventListener() {},
  };
  const sandbox = {
    document,
    localStorage: { getItem: () => null, setItem() {} },
    setTimeout: () => 0,
    clearTimeout() {},
    setInterval: () => 1,
    clearInterval() {},
    addEventListener() {},
    AbortController,
    fetch() { throw new Error("load_core has no network"); },
    location: {},
    console: { log() {}, warn() {}, error() {}, info() {} },
  };
  sandbox.window = sandbox;
  vm.runInNewContext(coreSource("core/core.js"), sandbox);
  return { core: sandbox.AdminApp.modules.core, document };
}

function loadCore() {
  return loadCoreEnv().core;
}

module.exports = { loadCore, loadCoreEnv };
