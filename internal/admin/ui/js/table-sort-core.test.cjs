"use strict";
// core.createTableSort: the one sorting component behind every admin table.
const assert = require("node:assert/strict");
const test = require("node:test");
const { loadCoreEnv } = require("../../../../scripts/testing/load_core.cjs");

const { core, document: stubDocument } = loadCoreEnv();
const { createTableSort, compareTableSortValues } = core;

// A <th> and a <thead> just rich enough for the sorter: attributes, classes and a data-sort query.
function fakeTh(key) {
  const attrs = key === null ? {} : { "data-sort": key };
  const classes = new Set();
  return {
    attrs,
    classes,
    focused: 0,
    focus() { this.focused++; stubDocument.activeElement = this; },
    getAttribute: (name) => (name in attrs ? attrs[name] : null),
    setAttribute: (name, value) => { attrs[name] = String(value); },
    classList: {
      add: (name) => classes.add(name),
      remove: (...names) => names.forEach((name) => classes.delete(name)),
    },
  };
}
function fakeHead(keys) {
  const cells = keys.map(fakeTh);
  const listeners = {};
  return {
    cells,
    listeners,
    addEventListener: (type, fn) => { (listeners[type] = listeners[type] || []).push(fn); },
    querySelectorAll: (selector) => {
      assert.equal(selector, "th[data-sort]");
      return cells.filter((cell) => cell.getAttribute("data-sort") !== null);
    },
    // The event target stands in for the clicked element: closest() resolves to its sortable <th>, or null.
    click: (cell, extra = {}) => listeners.click.forEach((fn) => fn({ target: { closest: () => (cell && cell.getAttribute("data-sort") !== null ? cell : null) }, ...extra })),
    key: (cell, key) => {
      const event = { key, target: { closest: () => (cell.getAttribute("data-sort") !== null ? cell : null) }, prevented: false, preventDefault() { this.prevented = true; } };
      listeners.keydown.forEach((fn) => fn(event));
      return event;
    },
  };
}
// The sorter lives in a vm context, so arrays and objects it returns have another realm's prototypes;
// copy them into this realm before deep-comparing.
const values = (rows) => Array.from(rows, (row) => row.v);
const names = (rows) => Array.from(rows, (row) => row.n);
const stateOf = (sort) => ({ ...sort.state() });

test("a header cycles ascending, descending, then the page default order", () => {
  const changes = [];
  const sort = createTableSort({ columns: { v: (row) => row.v }, onChange: (state) => changes.push(state) });
  const rows = [{ v: 2, n: "b" }, { v: 1, n: "a" }, { v: 3, n: "c" }];
  const byName = (a, b) => a.n.localeCompare(b.n);
  assert.deepEqual(stateOf(sort), { key: "", dir: 0 });
  assert.deepEqual(values(sort.apply(rows, byName)), [1, 2, 3], "no sort active: the default order");
  sort.toggle("v");
  assert.deepEqual(stateOf(sort), { key: "v", dir: 1 });
  assert.deepEqual(values(sort.apply(rows, byName)), [1, 2, 3]);
  sort.toggle("v");
  assert.deepEqual(stateOf(sort), { key: "v", dir: -1 });
  assert.deepEqual(values(sort.apply(rows, byName)), [3, 2, 1]);
  sort.toggle("v");
  assert.deepEqual(stateOf(sort), { key: "", dir: 0 }, "the third click clears the sort");
  assert.deepEqual(values(sort.apply(rows, byName)), [1, 2, 3]);
  assert.deepEqual(changes.map((state) => ({ ...state })), [{ key: "v", dir: 1 }, { key: "v", dir: -1 }, { key: "", dir: 0 }], "onChange receives each new state");
});

test("choosing another column starts ascending", () => {
  const sort = createTableSort({ columns: { a: (r) => r.a, b: (r) => r.b } });
  sort.toggle("a");
  sort.toggle("a");
  sort.toggle("b");
  assert.deepEqual(stateOf(sort), { key: "b", dir: 1 });
});

test("an unknown key is ignored and does not notify", () => {
  const changes = [];
  const sort = createTableSort({ columns: { a: (r) => r.a }, onChange: (state) => changes.push(state) });
  sort.toggle("nope");
  sort.toggle("");
  sort.toggle("toString"); // inherited names are not columns
  assert.deepEqual(stateOf(sort), { key: "", dir: 0 });
  assert.deepEqual(changes, []);
});

test("empty values always sort last, ascending and descending", () => {
  const sort = createTableSort({ columns: { v: (r) => r.v } });
  const rows = [{ v: null }, { v: 5 }, { v: undefined }, { v: 1 }, { v: "" }, { v: NaN }, { v: 3 }];
  sort.toggle("v");
  assert.deepEqual(values(sort.apply(rows)).slice(0, 3), [1, 3, 5]);
  assert.ok(values(sort.apply(rows)).slice(3).every((v) => v === null || v === undefined || v === "" || Number.isNaN(v)));
  sort.toggle("v");
  assert.deepEqual(values(sort.apply(rows)).slice(0, 3), [5, 3, 1], "descending keeps the empties at the end");
  assert.ok(values(sort.apply(rows)).slice(3).every((v) => v === null || v === undefined || v === "" || Number.isNaN(v)));
  assert.equal(values(sort.apply(rows)).length, 7);
  // 0 and false-like numbers are real values, not empties.
  const zero = sort.apply([{ v: null }, { v: 0 }, { v: -1 }]);
  assert.deepEqual(values(zero), [0, -1, null]);
});

test("rows that compare equal keep the page default order in both directions", () => {
  const sort = createTableSort({ columns: { g: (r) => r.g } });
  const rows = [{ g: 1, n: "d" }, { g: 2, n: "c" }, { g: 1, n: "b" }, { g: 2, n: "a" }];
  const byName = (a, b) => a.n.localeCompare(b.n);
  sort.toggle("g");
  assert.deepEqual(names(sort.apply(rows, byName)), ["b", "d", "a", "c"]);
  sort.toggle("g");
  assert.deepEqual(names(sort.apply(rows, byName)), ["a", "c", "b", "d"], "descending still orders ties by the default");
  // Without a default compare, ties keep the input order.
  assert.deepEqual(names(sort.apply(rows)), ["c", "a", "d", "b"]);
});

test("numbers compare numerically, strings with zh-CN numeric collation", () => {
  const sort = createTableSort({ columns: { n: (r) => r.v } });
  sort.toggle("n");
  assert.deepEqual(values(sort.apply([{ v: 10 }, { v: 9 }, { v: 100 }, { v: 2 }])), [2, 9, 10, 100], "10 is greater than 9");
  assert.deepEqual(values(sort.apply([{ v: "node10" }, { v: "node9" }, { v: "node2" }])), ["node2", "node9", "node10"], "digit runs compare as numbers");
  assert.deepEqual(values(sort.apply([{ v: "波" }, { v: "阿" }, { v: "才" }])), ["阿", "波", "才"], "Chinese follows pinyin order");
  assert.deepEqual(values(sort.apply([{ v: "b" }, { v: "A" }, { v: "a" }])), ["A", "a", "b"], "letters that differ only by case tie and keep their order");
  assert.ok(compareTableSortValues(1, 2) < 0 && compareTableSortValues(2, 1) > 0 && compareTableSortValues(2, 2) === 0);
  assert.ok(compareTableSortValues(null, 1) > 0 && compareTableSortValues(1, null) < 0 && compareTableSortValues(null, undefined) === 0);
  // Infinity is an ordinary (largest) number, e.g. an unlimited quota.
  assert.deepEqual(values(sort.apply([{ v: Infinity }, { v: 3 }, { v: Infinity }, { v: 0 }])), [0, 3, Infinity, Infinity]);
});

test("apply copies its input and tolerates a non-array", () => {
  const sort = createTableSort({ columns: { v: (r) => r.v } });
  const rows = [{ v: 2 }, { v: 1 }];
  sort.toggle("v");
  const sorted = sort.apply(rows);
  assert.notEqual(sorted, rows);
  assert.deepEqual(values(rows), [2, 1], "the input order is untouched");
  assert.deepEqual(Array.from(sort.apply(undefined)), []);
  assert.deepEqual(Array.from(sort.apply(null, () => 0)), []);
});

test("bind: header clicks and Enter/Space toggle, repeat binds are ignored, paint reflects the state", () => {
  const changes = [];
  const sort = createTableSort({ columns: { a: (r) => r.a, b: (r) => r.b }, onChange: (state) => changes.push(state) });
  const head = fakeHead(["a", "b", null]); // the third header is an actions column without data-sort
  sort.bind(head);
  sort.bind(head);
  assert.equal(head.listeners.click.length, 1, "binding the same head twice registers one listener");
  assert.ok(head.cells.slice(0, 2).every((cell) => cell.getAttribute("tabindex") === "0" && cell.getAttribute("aria-sort") === "none"), "sortable headers are focusable and announce no sort");
  assert.equal(head.cells[2].getAttribute("tabindex"), null, "a non-sortable header stays out of the tab order");
  head.click(head.cells[0]);
  assert.deepEqual(stateOf(sort), { key: "a", dir: 1 });
  assert.equal(head.cells[0].getAttribute("aria-sort"), "ascending");
  assert.ok(head.cells[0].classes.has("sorted-asc") && !head.cells[0].classes.has("sorted-desc"));
  assert.equal(head.cells[1].getAttribute("aria-sort"), "none");
  head.click(head.cells[0]);
  assert.equal(head.cells[0].getAttribute("aria-sort"), "descending");
  assert.ok(head.cells[0].classes.has("sorted-desc") && !head.cells[0].classes.has("sorted-asc"));
  head.click(head.cells[2]); // not sortable: nothing happens
  head.click(null);
  assert.equal(changes.length, 2);
  const enter = head.key(head.cells[1], "Enter");
  assert.equal(enter.prevented, true);
  assert.deepEqual(stateOf(sort), { key: "b", dir: 1 });
  head.key(head.cells[1], " ");
  assert.deepEqual(stateOf(sort), { key: "b", dir: -1 });
  const other = head.key(head.cells[1], "a");
  assert.equal(other.prevented, false, "other keys keep their default behaviour");
  assert.deepEqual(stateOf(sort), { key: "b", dir: -1 });
  sort.toggle("b");
  assert.ok(head.cells.every((cell) => !cell.classes.has("sorted-asc") && !cell.classes.has("sorted-desc")), "the default order shows no arrow");
});

test("paint repaints headers that were regenerated after binding", () => {
  const sort = createTableSort({ columns: { a: (r) => r.a } });
  const head = fakeHead(["a"]);
  sort.bind(head);
  sort.toggle("a");
  head.cells = null; // header markup replaced
  const regenerated = [fakeTh("a")];
  head.querySelectorAll = () => regenerated;
  sort.paint();
  assert.equal(regenerated[0].getAttribute("aria-sort"), "ascending");
  assert.equal(regenerated[0].getAttribute("tabindex"), "0");
  assert.ok(regenerated[0].classes.has("sorted-asc"));
});

test("bind and paint tolerate a missing or minimal element", () => {
  const sort = createTableSort({ columns: { a: (r) => r.a } });
  sort.bind(null);
  sort.bind(undefined);
  const minimal = { addEventListener() {} }; // no querySelectorAll, e.g. a stub document
  sort.bind(minimal);
  sort.paint();
  sort.toggle("a");
  assert.deepEqual(stateOf(sort), { key: "a", dir: 1 });
});

test("every sorter registers itself for the coverage gate", () => {
  const before = core.listTableSorts().length;
  const sort = createTableSort({ columns: { a: (r) => r.a, b: (r) => r.b } });
  const head = fakeHead(["a", "b"]);
  sort.bind(head);
  assert.equal(core.listTableSorts().length, before + 1);
  assert.ok(core.listTableSorts().includes(sort));
  assert.deepEqual(Array.from(sort.keys()), ["a", "b"]);
  assert.equal(sort.heads().length, 1);
  assert.equal(sort.heads()[0], head);
});

test("focus stays on the header the user sorted by, even when the header is regenerated with the rows", () => {
  // Two detail tables share one sorter through their container; the header of each is rebuilt on every sort.
  const container = { tables: [[fakeTh("a"), fakeTh("b")], [fakeTh("a"), fakeTh("b")]], listeners: {} };
  container.addEventListener = (type, fn) => { (container.listeners[type] = container.listeners[type] || []).push(fn); };
  container.querySelectorAll = () => container.tables.flat();
  const sort = createTableSort({
    columns: { a: (r) => r.a, b: (r) => r.b },
    onChange: () => { container.tables = container.tables.map(() => [fakeTh("a"), fakeTh("b")]); },
  });
  sort.bind(container);
  const secondTableB = container.tables[1][1];
  stubDocument.activeElement = secondTableB;
  sort.toggle("b");
  const rebuilt = container.tables[1][1];
  assert.notEqual(rebuilt, secondTableB, "the header was replaced");
  assert.equal(stubDocument.activeElement, rebuilt, "focus moved to the same column of the same table");
  assert.equal(rebuilt.focused, 1);
  assert.equal(container.tables[0][1].focused, 0, "the other table's header is left alone");
  // A header that is not focused is not stolen focus for; nor is another column's.
  stubDocument.activeElement = null;
  sort.toggle("b");
  assert.ok(container.tables.flat().every((th) => th.focused === 0));
  stubDocument.activeElement = container.tables[0][0];
  sort.toggle("b");
  assert.ok(container.tables.flat().every((th) => th.focused === 0), "focus on another column is not moved");
  // A header that is not replaced keeps focus without being refocused.
  const head = fakeHead(["a"]);
  const stable = createTableSort({ columns: { a: (r) => r.a } });
  stable.bind(head);
  stubDocument.activeElement = head.cells[0];
  stable.toggle("a");
  assert.equal(head.cells[0].focused, 0);
});
