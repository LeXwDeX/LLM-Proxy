"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const { createHarness } = require("../../../scripts/testing/admin_ui_test.cjs");

const targets = [
  {
    name: "official",
    pool: "ds",
    model_mappings: [
      { upstream: "DeepSeek-V4.1-Flash", fallback: "ds-v4-flash" },
      { upstream: "Official-Second-Model", fallback: "alpha-1, beta-2" },
    ],
  },
  {
    name: "agg",
    pool: "ds",
    model_mappings: [{ upstream: "deepseek-chat", fallback: "ds-v4-flash" }],
  },
  {
    name: "legacy",
    pool: "ds",
    model_mappings: [
      { upstream: "Legacy-Model", fallback: "legacy-alias", paused: true },
    ],
  },
  { name: "unmapped", pool: "ds" },
];

async function render(strategy) {
  const harness = createHarness();
  const app = await harness.run();
  app.state.pools = [{ name: "ds", routing_strategy: strategy }];
  app.state.targets = targets;
  app.modules.pools.renderPools();
  return harness.ids.get("poolsTableBody").innerHTML;
}

// Issue #179: a cost pool's runtime view names the client alias first and the
// member's upstream model second, so operators can see which members compete
// for the same client-facing model. Comma-separated fallbacks are split.
test("cost pool member tooltips show aliases before upstream models", async () => {
  const rows = await render("cost");
  assert.match(rows, /ds-v4-flash（上游 DeepSeek-V4\.1-Flash）/);
  assert.match(
    rows,
    /alpha-1（上游 Official-Second-Model）、beta-2（上游 Official-Second-Model）/,
    "comma-separated aliases are split into independent labels",
  );
  assert.match(rows, /ds-v4-flash（上游 deepseek-chat）/);
  assert.match(
    rows,
    /已暂停 legacy-alias（上游 Legacy-Model）/,
    "a paused mapping is still labelled and marked",
  );
  assert.match(rows, /模型 不限/, "an unrestricted member is explicit");
  assert.doesNotMatch(
    rows,
    /DeepSeek-V4\.1-Flash（上游 ds-v4-flash）/,
    "the upstream model must not be rendered as the primary label",
  );
});

// The grouping itself is visible without hovering: fallback alias groups come
// first, and client-addressable upstream names appear as secondary groups.
// Upstream names stay secondary in each badge's title.
test("cost pool shows alias groups with upstreams as secondary info", async () => {
  const rows = await render("cost");
  assert.match(rows, /<div class='pool-alias-groups'>/);
  assert.match(rows, />ds-v4-flash · official、agg</);
  assert.match(rows, />alpha-1 · official</);
  assert.match(rows, />beta-2 · official</);
  assert.match(
    rows,
    /title='上游：official → DeepSeek-V4\.1-Flash；agg → deepseek-chat'/,
    "the group title maps each member to its upstream spelling",
  );
  assert.match(
    rows,
    />DeepSeek-V4\.1-Flash · official</,
    "an upstream name that clients may request is a secondary group",
  );
  assert.match(rows, />deepseek-chat · agg</);
  assert.doesNotMatch(
    rows,
    />legacy-alias · /,
    "a paused mapping does not compete and is not grouped",
  );
});

// A request name can be one member's upstream spelling and another member's
// fallback. Both compete for that name, so the group must include both.
test("upstream request names group every competing member", async () => {
  const harness = createHarness();
  const app = await harness.run();
  app.state.pools = [{ name: "ds", routing_strategy: "cost" }];
  app.state.targets = [
    {
      name: "a",
      pool: "ds",
      model_mappings: [{ upstream: "deepseek-chat", fallback: "ds-flash" }],
    },
    {
      name: "b",
      pool: "ds",
      model_mappings: [{ upstream: "deepseek-chat" }],
    },
  ];
  app.modules.pools.renderPools();
  const rows = harness.ids.get("poolsTableBody").innerHTML;
  assert.match(rows, />deepseek-chat · a、b</);
  assert.match(rows, />ds-flash · a</);
});

// Alias and upstream are operator-controlled text; both label paths must stay
// escaped in the tooltip and in the visible group badge.
test("alias groups and tooltips escape configured names", async () => {
  const harness = createHarness();
  const app = await harness.run();
  app.state.pools = [{ name: "ds", routing_strategy: "cost" }];
  app.state.targets = [
    {
      name: "t",
      pool: "ds",
      model_mappings: [
        { upstream: "<img src=x>", fallback: "'><img src=y>" },
      ],
    },
  ];
  app.modules.pools.renderPools();
  const rows = harness.ids.get("poolsTableBody").innerHTML;
  assert.doesNotMatch(rows, /<img /);
  assert.match(rows, /&lt;img src=x&gt;/);
  assert.match(rows, /&#39;&gt;&lt;img src=y&gt;/);
});

// Paused targets and paused pools must not look like active cost candidates.
test("paused targets and paused pools do not look active", async () => {
  const harness = createHarness();
  const app = await harness.run();
  app.state.pools = [{ name: "ds", routing_strategy: "cost" }];
  app.state.targets = [
    {
      name: "active",
      pool: "ds",
      model_mappings: [{ upstream: "Active-Upstream", fallback: "active-alias" }],
    },
    {
      name: "paused-target",
      pool: "ds",
      paused: true,
      model_mappings: [{ upstream: "Paused-Upstream", fallback: "paused-alias" }],
    },
  ];
  app.modules.pools.renderPools();
  let rows = harness.ids.get("poolsTableBody").innerHTML;
  assert.match(
    rows,
    /模型 目标已暂停 paused-alias（上游 Paused-Upstream）/,
    "a paused target is labelled in its member tooltip",
  );
  assert.match(rows, />active-alias · active</);
  assert.doesNotMatch(
    rows,
    />paused-alias · /,
    "a paused target does not join the active alias groups",
  );

  app.state.pools[0].paused = true;
  app.modules.pools.renderPools();
  rows = harness.ids.get("poolsTableBody").innerHTML;
  assert.doesNotMatch(
    rows,
    /pool-alias-groups/,
    "a paused pool does not show active alias groups",
  );
});

// The visible alias badge must carry explicit theme colors, not inherit an
// invisible bare class (the #178 failure mode).
test("alias group badge style defines theme colors", () => {
  const css = fs.readFileSync(path.join(__dirname, "css/admin.css"), "utf8");
  const rule = css.match(/\.pool-alias-group\s*\{([^}]*)\}/);
  assert.ok(rule, ".pool-alias-group rule is missing");
  assert.match(rule[1], /color\s*:\s*var\(--accent\)/);
  assert.match(rule[1], /background\s*:\s*var\(--accent-soft\)/);
  const light = css.match(/\.app\s*\{([^}]*)\}/);
  const dark = css.match(/\.app\.dark\s*\{([^}]*)\}/);
  assert.ok(light && dark, "theme token blocks are missing");
  for (const block of [light[1], dark[1]]) {
    assert.match(block, /--accent\s*:/);
    assert.match(block, /--accent-soft\s*:/);
  }
});

// Load pools have no alias comparison semantics; their runtime view stays as
// before, matching the target editor's upstream-first wording.
test("load pools keep the chips-only runtime view", async () => {
  const rows = await render("load");
  assert.match(rows, /pool-member-name'>official/);
  assert.doesNotMatch(rows, /（上游 /);
  assert.doesNotMatch(rows, /pool-alias-groups/);
});