"use strict";
// Optional real-browser regression; uses the embedded page without a server.
const {test, before, after} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { assembleAdminHTML } = require("../../../scripts/testing/assemble_admin_html.cjs");
const playwright = require("playwright");

const html = assembleAdminHTML()
  .replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi, "");
const engine = process.env.UI_TEST_WEBKIT === "1" ? "webkit" : "chromium";
let browser;
before(async () => { browser = await playwright[engine].launch({headless: true}); });
after(async () => { if (browser) await browser.close(); });

for (const count of [1, 2, 4]) {
  test(`${engine}: ${count} long target model labels fit the drawer and preserve table rows`, async () => {
    const page = await browser.newPage();
    try {
      await page.setContent(html);
      for (const width of [320, 390, 1200]) {
        await page.setViewportSize({width, height: 844});
        let lightColors;
        for (const dark of [false, true]) {
          const layout = await page.evaluate(({count, dark}) => {
            document.documentElement.classList.toggle("dark", dark);
            document.getElementById("targetDetailDrawer").classList.add("open");
            document.querySelector(".target-detail-drawer").style.transition = "none";
            const panel = document.getElementById("targetDetailModels");
            panel.hidden = false;
            const labels = Array.from({length: count}, (_, i) => `模型${i}-VendorVeryLongModelNameWithoutBreaksABCDEF1234567890RepeatedLongModelNameABCDEF1234567890 (generic-model-alias)`);
            panel.innerHTML = `<div class="target-detail-models"><div class="target-model-list">${labels.map(label => `<span class="target-model-badge" title="${label}">${label}</span>`).join("")}</div></div><p class="target-detail-note">括号内为下游兜底别名；已暂停的模型不接收请求。</p>`;
            document.getElementById("layout-test-table")?.remove();
            const table = document.createElement("table");
            table.id = "layout-test-table";
            table.className = "targets-table";
            table.innerHTML = `<tbody><tr><td><div class="target-model-list">${labels.map(label => `<button type="button" class="target-model-badge">${label}</button>`).join("")}</div></td></tr></tbody>`;
            document.body.appendChild(table);
            const list = panel.querySelector(".target-model-list");
            const badges = [...list.children];
            const rect = element => {
              const r = element.getBoundingClientRect();
              return {left: r.left, right: r.right, top: r.top, bottom: r.bottom};
            };
            const tableList = table.querySelector(".target-model-list");
            return {
              labels,
              badges: badges.map(badge => ({rect: rect(badge), title: badge.title, text: badge.textContent, scrollWidth: badge.scrollWidth, clientWidth: badge.clientWidth})),
              list: {rect: rect(list), scrollWidth: list.scrollWidth, clientWidth: list.clientWidth},
              note: rect(panel.querySelector(".target-detail-note")),
              panelRole: panel.getAttribute("role"),
              panelLabel: panel.getAttribute("aria-labelledby"),
              colors: [getComputedStyle(badges[0]).color, getComputedStyle(badges[0]).backgroundColor],
              tableRows: getComputedStyle(tableList).gridTemplateRows,
              tableFlow: getComputedStyle(tableList).gridAutoFlow,
              tableWhiteSpace: getComputedStyle(tableList.firstElementChild).whiteSpace,
              tableButton: [tableList.firstElementChild.tagName, tableList.firstElementChild.getAttribute("type")],
            };
          }, {count, dark});
          const context = `${count} models at ${width}px ${dark ? "dark" : "light"}`;
          assert.equal(layout.panelRole, "tabpanel", context);
          assert.equal(layout.panelLabel, "targetDetailModelsTab", context);
          layout.badges.forEach((badge, i) => {
            assert.equal(badge.text, layout.labels[i], context);
            assert.equal(badge.title, layout.labels[i], context);
            assert.ok(badge.rect.bottom <= layout.list.rect.bottom + 1, `${context}: label ${i} fits its row`);
            if (i) assert.ok(badge.rect.top >= layout.badges[i - 1].rect.bottom - 1, `${context}: labels do not overlap`);
            assert.ok(badge.rect.bottom <= layout.note.top + 1, `${context}: label does not overlap the following note`);
            assert.ok(badge.scrollWidth <= badge.clientWidth + 1, `${context}: label ${i} does not clip`);
            assert.ok(badge.rect.left >= layout.list.rect.left - 1 && badge.rect.right <= layout.list.rect.right + 1, `${context}: label ${i} stays inside list`);
          });
          assert.ok(layout.list.scrollWidth <= layout.list.clientWidth + 1, `${context}: no horizontal overflow`);
          assert.equal(layout.tableRows, Array.from({length: Math.min(count, 3)}, () => "22px").join(" "), `${context}: table keeps fixed rows`);
          assert.equal(layout.tableFlow, "column", context);
          assert.equal(layout.tableWhiteSpace, "nowrap", context);
          assert.deepEqual(layout.tableButton, ["BUTTON", "button"], context);
          assert.notEqual(layout.colors[0], layout.colors[1], `${context}: badge text and background differ`);
          if (dark) assert.notDeepEqual(layout.colors, lightColors, `${context}: dark theme uses its own colors`);
          else lightColors = layout.colors;
        }
      }
    } finally {
      await page.close();
    }
  });
}
