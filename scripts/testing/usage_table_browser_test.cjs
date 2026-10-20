"use strict";
// Local fixture only: real embedded HTML/CSS and usage module, no server or model calls.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const playwright = require("playwright");
const browserName = process.env.LLMS_UI_BROWSER || "chromium";
assert.ok(["chromium", "webkit"].includes(browserName), "supported browser");
const root = path.resolve(__dirname, "../..");
async function main() {
  const browser = await playwright[browserName].launch({ headless: true });
  try {
    const page = await browser.newPage({ viewport: { width: 1920, height: 1400 } });
    const errors = [];
    page.on("pageerror", err => errors.push(err.message));
    await page.setContent(fs.readFileSync(path.join(root, "internal/admin/ui/index.html"), "utf8").replace(/<script\b[^>]*>[\s\S]*?<\/script>/g, ""));
    await page.evaluate(() => {
      document.querySelectorAll(".page").forEach(p => p.classList.remove("active"));
      document.getElementById("page-usage").classList.add("active");
      document.getElementById("adminBootStatus").hidden = true;
      document.getElementById("topbarTitle").textContent = "消费统计";
      document.querySelectorAll(".sidebar-nav-item").forEach(item=>item.classList.toggle("active", item.dataset.page === "usage"));
      const rows = [
        { key: "有推理", model_group: "订阅", model: "glm-flash", output_tokens: 9420, reasoning_tokens: 8012 },
        { key: "只有主值", model_group: "API", model: "qwen-max", output_tokens: 8232, incomplete_cost_count: 1 },
        { key: "显式缓存", model_group: "订阅", model: "glm-max", output_tokens: 201, cached_tokens: 2147483647, explicit_cache_read_tokens: 147483647 },
        { key: "长名称-一个需要完整显示的客户端名称-Client-With-A-Long-Name", model_group: "自定义模型组", model: "Upstream-Model-With-A-Long-Name-And-Preserved-Case", output_tokens: 1234567890123, reasoning_tokens: 0 },
      ].map((r,i) => Object.assign({ requests: 10+i, input_tokens: 101, estimated_cost: 1.23, target: "fixture-target" }, r));
      window.fixtureRows = rows;
      const core = {
        esc: v => String(v).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/'/g,"&#39;").replace(/"/g,"&quot;"),
        fmtNum: v => Number(v || 0).toLocaleString("zh-CN"),
        fmtMoney: v => "$"+Number(v || 0).toFixed(2),
        totalTok: r => ["input_tokens","output_tokens","cached_tokens","cache_creation_tokens"].reduce((n,k)=>n+Number(r[k]||0),0),
        api: async () => ({by_client: rows, by_model_group_model: rows, by_target_model: rows}),
        toast: message => { throw Error(message); },
      };
      window.app = {state: {modelGroupsLoaded:true,modelGroups:[],usageModelGroupExpanded:{},usageModelTargetExpanded:{}}, bind: (module,key) => module === "quota" ? name => name === "company_general" ? "公司通用" : name : core[key]};
      window.AdminApp = {register: (name,factory) => { window.usage = factory(app); }};
    });
    await page.addScriptTag({ path: path.join(root, "internal/admin/ui/js/usage.js") });
    await page.evaluate(() => usage.loadUsage());
    // Before a user sorts, rows of each model group must be adjacent and groups alphabetic.
    const actualOrder = await page.locator("#usageByModelBody tr").evaluateAll(rows => rows.map(r=>[r.cells[0].textContent,r.cells[1].textContent]));
    const expectedOrder = await page.evaluate(() => fixtureRows.slice().sort((a,b)=>a.model_group.localeCompare(b.model_group) || app.bind("core","totalTok")(b)-app.bind("core","totalTok")(a)).map(r=>[r.model_group,r.model]));
    assert.deepEqual(actualOrder, expectedOrder, "default model ordering groups rows together");
    await page.locator('#usageByModelTable th[data-sort="total"]').click();
    assert.equal(await page.locator("#usageByModelBody tr").first().locator("td").nth(1).textContent(), "Upstream-Model-With-A-Long-Name-And-Preserved-Case");
    await page.locator('#usageByModelTable th[data-sort="total"]').click();
    assert.equal(await page.locator("#usageByModelBody tr").first().locator("td").nth(1).textContent(), "qwen-max");
    await page.locator('#usageByModelTable th[data-sort="model_group"]').click();
    assert.deepEqual(await page.locator("#usageByModelBody tr").evaluateAll(rows => rows.map(r=>[r.cells[0].textContent,r.cells[1].textContent])), expectedOrder);
    await page.locator('[data-usage-expand-kind="target"]').first().click();
    assert.ok(await page.locator(".usage-model-row").count(), "tree expansion still shows model rows");
    for (const [width, zoom] of [[1920, 1], [1280, 1], [390, 1], [1280, 2]]) {
      await page.setViewportSize({ width, height: 1400 });
      await page.evaluate(zoom => { document.body.style.zoom = zoom; }, zoom);
      const metrics = await page.evaluate(() => {
        return [...document.querySelectorAll('#page-usage table')].map(table => ({
          id:table.id,
          heights:[...table.tBodies[0].rows].map(row=>row.getBoundingClientRect().height),
          // Token line boxes are exact; text ranges measure glyphs. Allow border rounding and font metrics.
          centers:[...table.tBodies[0].rows].map(row => [...row.cells].map(td => {
            const rect = td.getBoundingClientRect();
            const style = getComputedStyle(td);
            const scale = rect.height / td.offsetHeight;
            const center = (rect.top + parseFloat(style.borderTopWidth)*scale + rect.bottom - parseFloat(style.borderBottomWidth)*scale) / 2;
            const token = td.querySelector('.usage-token-value');
            let content;
            if (token) {
              const visible = [...token.children].filter(el => getComputedStyle(el).display !== 'none' && el.textContent.trim()).map(el=>el.getBoundingClientRect());
              content = {top: Math.min(...visible.map(r=>r.top)), bottom: Math.max(...visible.map(r=>r.bottom))};
            } else {
              const range = document.createRange(); range.selectNodeContents(td);
              content = range.getBoundingClientRect();
            }
            return {text:td.textContent, delta:((content.top+content.bottom)/2-center)/scale, token:!!token};
          })),
          details:[...table.querySelectorAll('.usage-token-detail')].map(detail=>({height:detail.getBoundingClientRect().height,text:detail.textContent})),
          cells:[...table.tBodies[0].rows].flatMap(row=>[...row.cells].slice(table.id === 'usageByModelTable' ? 2 : 1).map(td=>({align:getComputedStyle(td).textAlign, nums:getComputedStyle(td).fontVariantNumeric}))),
          wrap: getComputedStyle(table.parentElement).overflowX,
          wrapperWidth: table.parentElement.clientWidth,
          scrollWidth:table.parentElement.scrollWidth,
        }));
      });
      const heights = metrics.flatMap(t=>t.heights);
      assert.ok((Math.max(...heights)-Math.min(...heights))/zoom<1, `all rows share height at ${width}px: ${JSON.stringify(metrics.map(t=>({id:t.id,heights:t.heights})))}`);
      for (const table of metrics) {
        assert.ok(table.cells.every(c=>c.align==='right' && c.nums.includes('tabular-nums')), `${table.id} numeric alignment`);
        assert.equal(table.wrap, 'auto');
        assert.ok(table.centers.flat().every(c=>Math.abs(c.delta) <= (c.token ? 1.1 : 2)), `${table.id} visible content centered at ${width}px/${zoom}: ${JSON.stringify(table.centers)}`);
        assert.ok(table.details.some(d=>!d.text) && table.details.some(d=>d.text), `${table.id} has both empty and populated detail slots`);
        assert.ok(table.details.every(d=>Math.abs(d.height-(d.text ? 16*zoom : 0))<0.6), `${table.id} only populated details occupy a line`);
        if (width === 390 || zoom === 2) assert.ok(table.scrollWidth>table.wrapperWidth, 'narrow tables scroll locally');
      }
      const overflow = await page.evaluate(() => ({width:innerWidth, document:document.documentElement.scrollWidth, content:document.querySelector('.content').clientWidth, scroll:document.querySelector('.content').scrollWidth, offenders:[...document.querySelectorAll('#page-usage > *')].filter(el=>el.getBoundingClientRect().right>innerWidth).map(el=>[el.className,el.getBoundingClientRect().right,el.outerHTML.slice(0,180)])}));
      assert.ok(overflow.document <= width && overflow.scroll <= overflow.content, `page has no horizontal overflow: ${JSON.stringify(overflow)}`);
      // Finish responsive CSS transitions before capturing; scroll the content pane to the tables.
      await page.evaluate(async () => {
        await Promise.all(document.getAnimations().map(animation=>animation.finished.catch(()=>{})));
        const content = document.querySelector('.content');
        const panel = document.getElementById('usageByClientTable').closest('.panel');
        content.scrollTop += panel.getBoundingClientRect().top-content.getBoundingClientRect().top-16;
      });
      await page.screenshot({ path: path.join(process.env.LLMS_UI_SCREENSHOT_DIR || os.tmpdir(), `usage-table-${browserName}-${width}-${zoom}x.png`), fullPage:true });
    }
    await page.evaluate(() => {
      document.body.style.zoom = 1;
      const rows = fixtureRows.map(row => ({...row, reasoning_tokens:0, explicit_cache_read_tokens:0}));
      usage.renderAggregateTable({tbody:"usageByClientBody", rows, sort:{key:"cost",dir:-1},columnCount:8});
      usage.renderAggregateTable({tbody:"usageByModelBody", rows, sort:{key:"model_group",dir:1},columnCount:9});
      usage.renderUsageModelGroups(rows);
    });
    const plainHeights = await page.locator('.usage-table tbody tr').evaluateAll(rows=>rows.map(row=>row.getBoundingClientRect().height));
    assert.ok(Math.max(...plainHeights)-Math.min(...plainHeights)<1, 'rows remain equal when all token details are absent');
    assert.ok(plainHeights.every(height=>height>=56), 'all-plain rows retain token slot height');
    await page.evaluate(() => {
      usage.renderAggregateTable({tbody:"usageByModelBody", rows:[],columnCount:9});
      usage.renderUsageModelGroups([]);
    });
    assert.equal(await page.locator('#usageByModelBody td').getAttribute('colspan'), '9');
    assert.match(await page.locator('#usageByModelGroupBody').textContent(), /当前时间段无数据/);
    assert.deepEqual(errors, []);
    console.log(browserName + ' PASS usage tables: grouped default, global sorting, tree expansion, equal row height, alignment, responsive/200% overflow and empty states');
  } finally { await browser.close(); }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
