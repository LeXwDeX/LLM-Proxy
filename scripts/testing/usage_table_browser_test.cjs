"use strict";
// Local fixture only: real embedded HTML/CSS and usage module, no server or model calls.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { assembleAdminHTML } = require("./assemble_admin_html.cjs");
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
    await page.setContent(assembleAdminHTML().replace(/<script\b[^>]*>[\s\S]*?<\/script>/g, ""));
    await page.evaluate(() => {
      document.querySelectorAll(".page").forEach(p => p.classList.remove("active"));
      document.getElementById("page-usage").classList.add("active");
      document.getElementById("adminBootStatus").hidden = true;
      document.getElementById("topbarTitle").textContent = "消费统计";
      document.querySelectorAll(".sidebar-nav-item").forEach(item=>item.classList.toggle("active", item.dataset.page === "usage"));
      const rows = [
        { key: "有推理", model_group: "订阅", model: "glm-flash", input_tokens: 101, output_tokens: 9420, reasoning_tokens: 8012, requests: 10, estimated_cost: 1.23, target: "fixture-target" },
        { key: "只有主值", model_group: "API", model: "qwen-max", input_tokens: 101, output_tokens: 8232, requests: 11, estimated_cost: 2.5, incomplete_cost_count: 1, target: "fixture-target" },
        { key: "显式缓存", model_group: "订阅", model: "glm-max", input_tokens: 101, output_tokens: 201, cached_tokens: 2147483647, explicit_cache_read_tokens: 147483647, requests: 12, estimated_cost: 0, incomplete_usage_count: 1, incomplete_cost_count: 2, target: "fixture-target" },
        { key: "长名称-一个需要完整显示的客户端名称-Client-With-A-Long-Name", model_group: "自定义模型组", model: "Upstream-Model-With-A-Long-Name-And-Preserved-Case", input_tokens: 101, output_tokens: 1234567890123, reasoning_tokens: 0, requests: 13, target: "fixture-target" },
        { key: "用量行", model_group: "API", model: "usage-model", input_tokens: 101, output_tokens: 9440, requests: 0, estimated_cost: 0, incomplete_usage_count: 2, target: "fixture-target" },
      ];
      window.fixtureRows = rows;
      window.fixtureAggregate = {
        group_by: "hour",
        totals: { requests: 62, input_tokens: 404, output_tokens: 1234577027, cached_tokens: 2147483647, cache_creation_tokens: 0, tool_use_prompt_tokens: 5, estimated_cost: 10, incomplete_usage_count: 0, incomplete_cost_count: 0 },
        buckets: [
          { bucket_start: "2026-10-25T01:00:00Z", requests: 2, input_tokens: 10, output_tokens: 5, estimated_cost: 0 },
          { bucket_start: "2026-10-25T02:00:00Z", requests: 3, input_tokens: 20, output_tokens: 5 },
        ],
        by_client: rows.map((row, i) => ({ key: ["fixture-client", "tenant-b", "tenant-c", "__proto__", "usage-client"][i], requests: row.requests + (i === 4 ? 0 : 4), input_tokens: row.input_tokens, output_tokens: row.output_tokens, cached_tokens: row.cached_tokens, explicit_cache_read_tokens: row.explicit_cache_read_tokens, estimated_cost: row.estimated_cost, incomplete_usage_count: row.incomplete_usage_count, incomplete_cost_count: row.incomplete_cost_count })),
        by_model_group_model: rows,
        by_model: rows.map((row, i) => ({...row, requests: row.requests + (i === 4 ? 0 : 4)})),
        by_target_model: rows,
      };
      window.primaryFixtureAggregate = window.fixtureAggregate;
      window.apiCalls = [];
      const core = {
        esc: v => String(v).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/'/g,"&#39;").replace(/"/g,"&quot;"),
        fmtNum: v => Number(v || 0).toLocaleString("zh-CN"),
        fmtMoney: v => "$"+Number(v || 0).toFixed(2),
        totalTok: r => ["input_tokens","output_tokens","cached_tokens","cache_creation_tokens","tool_use_prompt_tokens"].reduce((n,k)=>n+Number(r[k]||0),0),
        api: async url => { window.apiCalls.push(url); return fixtureAggregate; },
        toast: message => { throw Error(message); },
      };
      window.app = {state: {modelGroupsLoaded:true,modelGroups:["API","订阅","自定义模型组"],usageModelGroupExpanded:{},usageModelTargetExpanded:{}}, bind: (module,key) => module === "quota" ? name => name === "company_general" ? "公司通用" : name : core[key]};
      window.AdminApp = {register: (name,factory) => { window.usage = factory(app); }};
    });
    await page.addScriptTag({ path: path.join(root, "internal/admin/ui/js/pages/usage/usage.js") });
    await page.evaluate(() => usage.loadUsage());
    assert.deepEqual(await page.evaluate(() => apiCalls.length), 1, "tables and trend share one aggregate request");
    assert.match(await page.evaluate(() => apiCalls[0]), /group_by=hour/, "today follows the hour bucket contract");

    // The three tabs are keyboard operable without issuing aggregate requests.
    await page.locator("#usageTabGroup").focus();
    await page.keyboard.press("ArrowRight");
    assert.equal(await page.evaluate(() => document.activeElement?.id), "usageTabClient");
    assert.equal(await page.locator("#usageTabClient").getAttribute("aria-selected"), "true");
    assert.equal(await page.locator("#usagePanelClient").isVisible(), true);
    await page.keyboard.press("End");
    assert.equal(await page.evaluate(() => document.activeElement?.id), "usageTabModel");
    assert.equal(await page.locator("#usagePanelModel").isVisible(), true);
    await page.keyboard.press("Home");
    assert.equal(await page.evaluate(() => document.activeElement?.id), "usageTabGroup");
    assert.equal(await page.evaluate(() => apiCalls.length), 1, "tab changes do not refetch");

    // A real 1h click and client filter keep one request per selection and share it with the trend.
    await page.locator('.time-range-btn[data-range="1h"]').click();
    await page.waitForFunction(() => apiCalls.length === 2);
    assert.match(await page.evaluate(() => apiCalls[1]), /group_by=hour/);
    await page.locator("#usageClientFilter").selectOption("fixture-client");
    await page.waitForFunction(() => apiCalls.length === 3);
    assert.match(await page.evaluate(() => apiCalls[2]), /client_name=fixture-client/);
    assert.equal(await page.locator("#usReqs").textContent(), "62");
    assert.match(await page.locator("#usCost").textContent(), /\$10\.00/);
    assert.match(await page.locator("#usTotal").textContent(), /3,382,061,083/, "total includes cached and tool input");

    // Model-group selections restate the aggregate with repeated model_group params; the menu opens and closes predictably.
    const usageModelGroupQuery = () => page.evaluate(() => {
      const url = apiCalls[apiCalls.length - 1];
      const params = new URLSearchParams(url.split("?")[1] || "");
      return {
        groups: params.getAll("model_group"),
        occurrences: (url.match(/[?&]model_group=/g) || []).length,
        client: params.get("client_name"),
      };
    });
    const waitForUsageCallAfter = async calls => {
      await page.waitForFunction(expected => apiCalls.length === expected, calls + 1);
    };
    const modelGroupButton = page.locator("#usageModelGroupFilterButton");
    const modelGroupMenu = page.locator("#usageModelGroupFilterMenu");
    await modelGroupButton.click();
    assert.equal(await modelGroupMenu.isVisible(), true, "model-group menu opens");
    assert.equal(await modelGroupButton.getAttribute("aria-expanded"), "true");
    assert.equal(await page.evaluate(() => document.activeElement?.value), "", "opening focuses the all option");
    await page.keyboard.press("Escape");
    assert.equal(await modelGroupMenu.isVisible(), false, "Escape closes the model-group menu");
    assert.equal(await modelGroupButton.getAttribute("aria-expanded"), "false");
    assert.equal(await page.evaluate(() => document.activeElement?.id), "usageModelGroupFilterButton", "Escape returns focus to the trigger");

    await modelGroupButton.click();
    let usageCalls = await page.evaluate(() => apiCalls.length);
    await page.getByRole("checkbox", { name: "API", exact: true }).check();
    await waitForUsageCallAfter(usageCalls);
    let modelGroupQuery = await usageModelGroupQuery();
    assert.deepEqual(modelGroupQuery.groups, ["API"], "single selection sends one model_group param");
    assert.equal(modelGroupQuery.occurrences, 1);
    assert.equal(modelGroupQuery.client, "fixture-client", "model-group selection keeps the client filter");
    assert.equal(await modelGroupButton.textContent(), "API");

    usageCalls = await page.evaluate(() => apiCalls.length);
    await page.getByRole("checkbox", { name: "订阅", exact: true }).check();
    await waitForUsageCallAfter(usageCalls);
    modelGroupQuery = await usageModelGroupQuery();
    assert.equal(modelGroupQuery.occurrences, 2, "multiple selections repeat the model_group param");
    assert.deepEqual(modelGroupQuery.groups.slice().sort(), ["API", "订阅"].sort());
    assert.equal(await modelGroupButton.textContent(), "已选 2 个模型组");

    usageCalls = await page.evaluate(() => apiCalls.length);
    await page.getByRole("checkbox", { name: "全部模型组", exact: true }).check();
    await waitForUsageCallAfter(usageCalls);
    modelGroupQuery = await usageModelGroupQuery();
    assert.equal(modelGroupQuery.occurrences, 0, "all clears model_group params");
    assert.deepEqual(modelGroupQuery.groups, []);
    assert.equal(await modelGroupButton.textContent(), "全部模型组");
    assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll("#usageModelGroupFilterMenu input[type=checkbox]")].filter(input => input.checked).map(input => input.value)), [""], "all is the only checked option");
    assert.equal(await modelGroupMenu.isVisible(), true);

    await page.locator("#topbarTitle").click();
    assert.equal(await modelGroupMenu.isVisible(), false, "outside click closes the model-group menu");
    assert.equal(await modelGroupButton.getAttribute("aria-expanded"), "false");

    await page.setViewportSize({ width: 390, height: 844 });
    await modelGroupButton.click();
    assert.equal(await modelGroupMenu.isVisible(), true, "model-group menu stays open at 390px");
    const narrowMenu = await page.evaluate(() => {
      const filter = document.getElementById("usageModelGroupFilter").getBoundingClientRect();
      const menu = document.getElementById("usageModelGroupFilterMenu").getBoundingClientRect();
      return {
        filterWidth: filter.width,
        menuLeft: menu.left,
        menuRight: menu.right,
        menuWidth: menu.width,
        viewport: innerWidth,
        documentWidth: document.documentElement.scrollWidth,
      };
    });
    assert.ok(narrowMenu.menuLeft >= 0 && narrowMenu.menuRight <= narrowMenu.viewport, `open menu fits the 390px viewport: ${JSON.stringify(narrowMenu)}`);
    assert.ok(Math.abs(narrowMenu.menuWidth - narrowMenu.filterWidth) < 1, "open menu spans the filter width");
    assert.ok(narrowMenu.documentWidth <= narrowMenu.viewport, `menu causes no page overflow at 390px: ${JSON.stringify(narrowMenu)}`);
    await page.keyboard.press("Escape");
    await page.setViewportSize({ width: 1920, height: 1400 });

    // Cost switch redraws from the already loaded aggregate.
    const beforeToggle = await page.evaluate(() => apiCalls.length);
    await page.locator("#usageChartMetricCost").click();
    assert.equal(await page.locator("#usageChartMetricCost").getAttribute("aria-pressed"), "true");
    assert.match(await page.locator("#dailyChart").textContent(), /\$0\.00/);
    assert.match(await page.locator("#dailyChart").textContent(), /不可用/);
    assert.equal(await page.evaluate(() => apiCalls.length), beforeToggle, "cost switch does not refetch");
    await page.locator("#usageChartMetricTokens").click();
    assert.equal(await page.locator("#usageChartMetricTokens").getAttribute("aria-pressed"), "true");

    // Costs are already discounted; the front end only shows them and the share denominator.
    await page.locator("#usageTabClient").click();
    assert.match(await page.locator("#usageByClientBody").textContent(), /占比 12\.3%/);
    assert.match(await page.locator("#usageByClientBody").textContent(), /\$0\.00/);
    assert.match(await page.locator("#usageByClientBody").textContent(), /不可用/);

    // Before a user sorts, rows of each model group must be adjacent and groups alphabetic.
    await page.locator("#usageTabModel").click();
    await page.locator("#usagePanelModel").waitFor({ state: "visible" });
    const actualOrder = await page.locator("#usageByModelBody tr").evaluateAll(rows => rows.map(r=>[r.cells[0].textContent,r.cells[1].textContent]));
    const expectedOrder = await page.evaluate(() => fixtureRows.slice().sort((a,b)=>a.model_group.localeCompare(b.model_group) || app.bind("core","totalTok")(b)-app.bind("core","totalTok")(a)).map(r=>[r.model_group,r.model]));
    assert.deepEqual(actualOrder, expectedOrder, "default model ordering groups rows together");
    await page.locator('#usageByModelTable th[data-sort="total"]').click();
    assert.equal(await page.locator("#usageByModelBody tr").first().locator("td").nth(1).textContent(), "Upstream-Model-With-A-Long-Name-And-Preserved-Case");
    await page.locator('#usageByModelTable th[data-sort="total"]').click();
    assert.equal(await page.locator("#usageByModelBody tr").first().locator("td").nth(1).textContent(), "qwen-max");
    await page.locator('#usageByModelTable th[data-sort="model_group"]').click();
    assert.deepEqual(await page.locator("#usageByModelBody tr").evaluateAll(rows => rows.map(r=>[r.cells[0].textContent,r.cells[1].textContent])), expectedOrder);

    // The primary model-group source is success-weighted and must not read by_model.
    const requestSum = (selector, cellIndex) => page.locator(selector.includes(" ") ? selector : `${selector} tbody tr`).evaluateAll((rows, index) => rows.reduce((sum, row) => sum + Number((row.cells[index].textContent || "").replace(/,/g, "") || 0), 0), cellIndex);
    assert.equal(await page.locator("#usageModelRequestHeader").textContent(), "请求（成功）");
    assert.match(await page.locator("#usageModelRequestHeader").getAttribute("title"), /成功请求权重（状态码 1–499）/);
    assert.match(await page.locator("#usageRequestNote").textContent(), /回退到按模型汇总/);
    assert.doesNotMatch(await page.locator("#usageRequestNote").textContent(), /目标与模型明细的请求数始终/);
    assert.equal(await requestSum("#usageByModelTable", 2), 46, "primary model requests are success-weighted, not by_model all requests");
    assert.equal(await requestSum("#usageByClientTable", 1), 62, "unfiltered client rows keep all requests");
    assert.equal((await page.locator("#usReqs").textContent()).replace(/,/g, ""), "62", "unfiltered summary keeps all requests");
    const partialTitles = await page.locator("#usageByModelBody span[title]").evaluateAll(nodes => nodes.map(node => node.getAttribute("title")));
    assert.ok(partialTitles.some(title => title.includes("用量不完整 2 条记录") && !title.includes("费用不完整")), "usage-only rows name only usage incompleteness");
    assert.ok(partialTitles.some(title => title.includes("费用不完整 1 条记录") && !title.includes("用量不完整")), "cost-only rows name only cost incompleteness");
    assert.ok(partialTitles.some(title => title.includes("用量不完整 1 条记录；费用不完整 2 条记录")), "mixed rows show both counts");
    assert.ok(partialTitles.every(title => !title.includes("0 条记录")), "absent categories are not printed as zero records");
    assert.ok((await page.locator("#usageByModelBody").textContent()).includes("（部分）"));

    // Without model-group rows the model tab falls back to by_model; the header must say so.
    await page.evaluate(async () => {
      const original = window.primaryFixtureAggregate;
      window.fixtureAggregate = {
        ...original,
        by_model_group_model: [],
      };
      await usage.loadUsage();
    });
    assert.equal(await page.locator("#usageModelRequestHeader").textContent(), "请求权重");
    assert.match(await page.locator("#usageModelRequestHeader").getAttribute("title"), /全部请求/);
    assert.equal(await requestSum("#usageByModelTable", 2), 62, "fallback model rows carry all requests");
    assert.equal(await requestSum("#usageByClientTable", 1), 62);
    assert.equal((await page.locator("#usReqs").textContent()).replace(/,/g, ""), "62");
    await page.locator("#usageTabGroup").click();
    await page.evaluate(() => {
      document.querySelectorAll('#usageByModelGroupBody [data-usage-expand-kind="target"]').forEach(button => {
        app.state.usageModelTargetExpanded[decodeURIComponent(button.dataset.usageExpandKey)] = true;
      });
      usage.renderUsageModelGroups(app.state.usageModelGroupRows);
    });
    assert.equal(await requestSum("#usageByModelGroupTable .usage-model-row", 1), 46, "the tree stays success-weighted while the model tab falls back");
    await page.evaluate(async () => {
      window.fixtureAggregate = window.primaryFixtureAggregate;
      await usage.loadUsage();
    });
    assert.equal(await page.locator("#usageModelRequestHeader").textContent(), "请求（成功）");
    assert.equal(await requestSum("#usageByModelTable", 2), 46);
    await page.locator("#usageTabGroup").click();
    await page.locator('[data-usage-expand-kind="target"]').first().click();
    assert.ok(await page.locator(".usage-model-row").count(), "tree expansion still shows model rows");

    for (const [width, zoom] of [[1920, 1], [1280, 1], [390, 1], [1280, 2]]) {
      await page.setViewportSize({ width, height: 1400 });
      await page.evaluate(zoom => { document.body.style.zoom = zoom; }, zoom);
      for (const tab of ["client", "model", "group"]) {
        await page.evaluate(tab => usage.selectUsageTab(tab, false), tab);
        const tableID = {client: "usageByClientTable", model: "usageByModelTable", group: "usageByModelGroupTable"}[tab];
        const metrics = await page.evaluate((tableID) => {
          const table = document.getElementById(tableID);
          return {
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
          };
        }, tableID);
        const heights = metrics.heights;
        assert.ok((Math.max(...heights)-Math.min(...heights))/zoom<1, `${tableID} rows share height at ${width}px: ${JSON.stringify(metrics.heights)}`);
        assert.ok(metrics.cells.every(c=>c.align==='right' && c.nums.includes('tabular-nums')), `${tableID} numeric alignment`);
        assert.equal(metrics.wrap, 'auto');
        assert.ok(metrics.centers.flat().every(c=>Math.abs(c.delta) <= (c.token ? 1.1 : 2)), `${tableID} visible content centered at ${width}px/${zoom}: ${JSON.stringify(metrics.centers)}`);
        assert.ok(metrics.details.some(d=>!d.text) && metrics.details.some(d=>d.text), `${tableID} has both empty and populated detail slots`);
        assert.ok(metrics.details.every(d=>Math.abs(d.height-(d.text ? 16*zoom : 0))<0.6), `${tableID} only populated details occupy a line`);
        if (width === 390 || zoom === 2) assert.ok(metrics.scrollWidth>metrics.wrapperWidth, `${tableID} scrolls locally`);
      }
      const overflow = await page.evaluate(() => ({width:innerWidth, document:document.documentElement.scrollWidth, content:document.querySelector('.content').clientWidth, scroll:document.querySelector('.content').scrollWidth, offenders:[...document.querySelectorAll('#page-usage > *')].filter(el=>el.getBoundingClientRect().right>innerWidth).map(el=>[el.className,el.getBoundingClientRect().right,el.outerHTML.slice(0,180)])}));
      assert.ok(overflow.document <= width && overflow.scroll <= overflow.content, `page has no horizontal overflow: ${JSON.stringify(overflow)}`);
    }

    // Keep the plain rows equal after all token detail slots disappear.
    await page.evaluate(() => {
      document.body.style.zoom = 1;
      const rows = fixtureRows.map(row => ({...row, reasoning_tokens:0, explicit_cache_read_tokens:0}));
      usage.selectUsageTab("client", false);
      usage.renderAggregateTable({tbody:"usageByClientBody", rows, sort:{key:"cost",dir:-1},columnCount:8});
      usage.selectUsageTab("model", false);
      usage.renderAggregateTable({tbody:"usageByModelBody", rows, sort:{key:"model_group",dir:1},columnCount:9});
      usage.selectUsageTab("group", false);
      usage.renderUsageModelGroups(rows);
    });
    for (const tableID of ["usageByClientTable", "usageByModelTable", "usageByModelGroupTable"]) {
      await page.evaluate((tableID) => usage.selectUsageTab({usageByClientTable:"client",usageByModelTable:"model",usageByModelGroupTable:"group"}[tableID], false), tableID);
      const plainHeights = await page.locator(`#${tableID} tbody tr`).evaluateAll(rows=>rows.map(row=>row.getBoundingClientRect().height));
      assert.ok(Math.max(...plainHeights)-Math.min(...plainHeights)<1, `${tableID} rows remain equal when all token details are absent`);
      assert.ok(plainHeights.every(height=>height>=56), `${tableID} all-plain rows retain token slot height`);
    }

    await page.evaluate(() => {
      usage.renderAggregateTable({tbody:"usageByClientBody", rows:[],columnCount:8});
      usage.selectUsageTab("model", false);
      usage.renderAggregateTable({tbody:"usageByModelBody", rows:[],columnCount:9});
      usage.selectUsageTab("group", false);
      usage.renderUsageModelGroups([]);
    });
    await page.evaluate(() => usage.selectUsageTab("model", false));
    assert.equal(await page.locator('#usageByModelBody td').getAttribute('colspan'), '9');
    await page.evaluate(() => usage.selectUsageTab("group", false));
    assert.match(await page.locator('#usageByModelGroupBody').textContent(), /当前时间段无数据/);
    await page.evaluate(() => usage.selectUsageTab("client", false));
    assert.match(await page.locator('#usageByClientBody').textContent(), /当前时间段无数据/);

    // Capture the complete usage surface with no stale scroll or hover tooltip.
    const screenshotDir = process.env.LLMS_UI_SCREENSHOT_DIR || os.tmpdir();
    fs.mkdirSync(screenshotDir, { recursive: true });
    await page.evaluate(() => {
      usage.selectUsageTab("group", false);
      const rows = fixtureRows.map(row => ({...row, reasoning_tokens:0, explicit_cache_read_tokens:0}));
      usage.renderAggregateTable({tbody:"usageByClientBody", rows, sort:{key:"cost",dir:-1},columnCount:8});
      usage.renderAggregateTable({tbody:"usageByModelBody", rows, sort:{key:"model_group",dir:1},columnCount:9});
      usage.renderUsageModelGroups(rows);
    });
    for (const [width, height, theme] of [[1440, 1200, "light"], [390, 844, "light"], [1440, 1200, "dark"], [390, 844, "dark"]]) {
      await page.setViewportSize({ width, height });
      await page.mouse.move(8, 300);
      await page.evaluate(theme => {
        document.documentElement.classList.toggle("dark", theme === "dark");
        document.body.style.zoom = 1;
        document.activeElement?.blur();
        document.querySelectorAll(".content, .table-wrap").forEach(el => { el.scrollTop = 0; el.scrollLeft = 0; });
      }, theme);
      const hint = await page.evaluate(() => document.getElementById("targetModelHint"));
      assert.equal(hint, null, "no tooltip can remain in the usage fixture");
      const contentSize = await page.evaluate(() => {
        const content = document.querySelector(".content");
        return { width: content.clientWidth, scrollWidth: content.scrollWidth, height: content.scrollHeight, top: content.getBoundingClientRect().top };
      });
      assert.ok(contentSize.scrollWidth <= contentSize.width, `no page overflow at ${width}px/${theme}`);
      // The app scrolls inside .content; an element screenshot otherwise clips below the viewport.
      await page.setViewportSize({ width, height: Math.ceil(contentSize.height + contentSize.top) });
      await page.screenshot({ path: path.join(screenshotDir, `usage-page-${width === 390 ? "mobile" : "desktop"}-${theme}.png`), animations: "disabled", fullPage: true });
    }
    await page.setViewportSize({ width: 1920, height: 1400 });
    await page.evaluate(() => document.documentElement.classList.remove("dark"));
    assert.deepEqual(errors, []);
    console.log(browserName + ' PASS usage tables: one range/filter request, model-group menu single/multiple/all URL params with Escape/outside close and 390px fit, hour buckets, source-labelled request weights and fallback, partial usage/cost states, keyboard tabs, tree expansion, tool-token totals, discounted costs and shares, zero/missing states, equal row height, responsive/200% overflow, empty states and screenshots');
  } finally { await browser.close(); }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
