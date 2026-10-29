"use strict";
// Script manifest helpers for Node tests. The list of scripts comes from the
// assembled HTML (never hard-coded), and per-page loaders return every file of
// a page directory so checks stay valid when a page is split into several files.
// Missing, empty or unlisted files throw instead of letting a check pass vacuously.
const fs = require("fs");
const path = require("path");
const { assembleAdminHTML, uiDir } = require("./assemble_admin_html.cjs");

const jsDir = path.join(uiDir, "js");
const tagRe = /<script\s+src="\/admin\/ui\/js\/([^"?]+\.js)(?:\?[^"]*)?"[^>]*><\/script>/g;

function walk(dir, base = dir) {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const full = path.join(dir, e.name);
    if (e.isDirectory()) return walk(full, base);
    return e.name.endsWith(".js") ? [path.relative(base, full).split(path.sep).join("/")] : [];
  });
}

// Scripts in HTML load order: [{rel, name (basename), file, source}].
function manifest(html = assembleAdminHTML()) {
  const loose = (html.match(/<script\s+src="\/admin\/ui\/js\/[^"?]+/g) || []).length;
  const out = [...html.matchAll(tagRe)].map((m) => {
    const rel = m[1];
    if (rel.includes("..") || rel.includes("\\") || path.isAbsolute(rel)) throw new Error(`unclean script path ${rel}`);
    const file = path.join(jsDir, rel);
    if (!fs.existsSync(file)) throw new Error(`script tag ${rel} does not resolve to a file`);
    return { rel, name: path.basename(rel), file, source: fs.readFileSync(file, "utf8") };
  });
  if (out.length !== loose) throw new Error(`${loose} admin script tags but ${out.length} are valid .js tags`);
  const seen = new Set();
  for (const s of out) {
    if (seen.has(s.rel)) throw new Error(`script ${s.rel} loaded twice`);
    seen.add(s.rel);
  }
  for (const rel of walk(jsDir)) if (!seen.has(rel)) throw new Error(`script ${rel} is not loaded by the HTML`);
  return out;
}

// Concatenated source of every script under js/pages/<page>/ in manifest order.
function pageSource(page) {
  const prefix = `pages/${page}/`;
  const parts = manifest().filter((s) => s.rel.startsWith(prefix));
  if (parts.length === 0) throw new Error(`no scripts for page ${page}`);
  const src = parts.map((s) => s.source).join("\n;\n");
  if (src.replace(/[;\s]/g, "") === "") throw new Error(`scripts for page ${page} are empty`);
  return src;
}

// Source of one core script (core/core.js, core/schema.js, ...).
function coreSource(rel) {
  const s = manifest().find((x) => x.rel === rel);
  if (!s || s.source.trim() === "") throw new Error(`script ${rel} missing or empty`);
  return s.source;
}

module.exports = { manifest, pageSource, coreSource, jsDir };
