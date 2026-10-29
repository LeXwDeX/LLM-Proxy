"use strict";
// Assembles internal/admin/ui/index.html by expanding `<!-- @include path -->`
// lines. Mirrors assembleAdminHTML in internal/admin/ui_assemble.go: the whole
// line (with its newline) is replaced by the included file verbatim; paths are
// relative to the ui directory; missing files, cycles, files without a final
// newline and leftover markers are errors.
const fs = require("fs");
const path = require("path");

const uiDir = path.join(__dirname, "..", "..", "internal", "admin", "ui");
const includeLine = /^[ \t]*<!-- @include (\S+) -->[ \t]*$/;

function expand(rel, stack, root) {
  if (path.isAbsolute(rel) || rel.split("/").some((seg) => seg === ".." || seg === "." || seg === "") || rel.includes("\\")) {
    throw new Error(`invalid include path ${JSON.stringify(rel)}`);
  }
  if (stack.includes(rel)) throw new Error(`include cycle: ${[...stack, rel].join(" -> ")}`);
  const src = fs.readFileSync(path.join(root, rel), "utf8");
  if (stack.length > 0 && !src.endsWith("\n")) throw new Error(`included file ${rel} must end with a newline`);
  let out = "";
  for (const line of src.split(/(?<=\n)/)) {
    const m = includeLine.exec(line.replace(/[\r\n]+$/, ""));
    if (m) {
      out += expand(m[1], [...stack, rel], root);
    } else if (line.includes("@include")) {
      throw new Error(`${rel}: malformed or leftover include marker: ${line.trim()}`);
    } else {
      out += line;
    }
  }
  return out;
}

function assembleAdminHTML(root = uiDir) {
  return expand("index.html", [], root);
}

module.exports = { assembleAdminHTML, uiDir };
