# scripts/testing — 本机测试工具

本目录存放管理前端的 Node 测试与可选浏览器 QA 脚本。这些脚本只使用本地 fixture 或进程内虚假上游，不调用真实模型，也不需要真实凭据。代理转发、计费与厂商 Patch 的端到端回归见下文 `make e2e`。

## 管理前端模块行为门禁（admin_ui_test.cjs）

```sh
node scripts/testing/admin_ui_test.cjs
```

仅使用 Node 内置模块，按 `internal/admin/ui/index.html` 中的脚本顺序在 VM 中执行内嵌 JS 模块，检查启动流程、Schema 驱动的控件和跨模块行为。`internal/admin/ui_runtime_test.go` 的 `TestUIRuntimeBehavior` 会调用该脚本，因此 `go test ./internal/admin/` 在本机有 `node` 时一并运行它；没有 `node` 时该用例 SKIP（设置 `LLMS_REQUIRE_NODE_TESTS=1` 则改为失败）。`internal/admin/ui/` 下的 `*.test.cjs` 单元用例基于 `node:test` 并复用本脚本导出的 harness；其中一部分由 `internal/admin` 的 Go 测试调用，全部用例可用 `node --test internal/admin/ui/` 直接运行。

## 管理后台网页全流程模拟（admin_ui_sim.cjs / admin_ui_sim_flows.cjs）

```sh
make ui-sim
# 或
node scripts/testing/admin_ui_sim.cjs [--keep] [--shots DIR]
```

`admin_ui_sim.cjs` 编译 `cmd/proxy` 与 `cmd/fakellm`，启动两台 fakellm 和一个使用临时配置与 bbolt 的独立代理实例，再通过无头 Chromium 驱动真实管理页面。`admin_ui_sim_flows.cjs` 定义具体流程：所有创建和编辑都经可见表单与保存按钮完成，客户端流量经代理发往虚假上游。覆盖登录、创建部署池/职级/目标/客户端、用量与总览核对、key 屏蔽与解除、目标暂停与恢复、Trace 与额度页面、删除客户端以及浏览器控制台无报错。

- `--keep`：结束后保留临时目录（配置与 bbolt）；`--shots DIR`：为每个流程截图，失败时另存 `fail-*.png`。
- 退出码：全部通过为 0，有失败为 1，启动失败为 2。
- 需要本机已安装的 Chromium（`CHROME_PATH`，或 Playwright 的浏览器缓存）。不进入 CI。

## 浏览器驱动（cdp_browser.cjs）

无第三方依赖的 Chrome DevTools Protocol 驱动，使用 Node 内置的 WebSocket/fetch 与本机已安装的 Chromium（`CHROME_PATH` 或 Playwright 浏览器缓存），不下载任何内容。页面通过执行 JS 驱动，触发与用户点击、输入相同的 DOM 事件。供 `admin_ui_sim.cjs` 使用。

## 可选浏览器 QA（admin_ui_browser_test.cjs、usage_table_browser_test.cjs）

```sh
node scripts/testing/admin_ui_browser_test.cjs
node scripts/testing/usage_table_browser_test.cjs
```

两者都依赖本机已安装的 `playwright` 包与浏览器，不进入 CI，CI 基线是上面的 Node VM 门禁。

- `admin_ui_browser_test.cjs`：用 Playwright/Chromium 访问本地 mock server 上的真实管理页面，复用 `admin_ui_test.cjs` 的 Schema fixture；不使用凭据，也不调用上游模型。可用 `LLMS_UI_SCREENSHOT_DIR` 指定截图目录。
- `usage_table_browser_test.cjs`：仅加载真实内嵌 HTML/CSS 与消费统计模块的本地 fixture，不启动服务。可用 `LLMS_UI_BROWSER` 选择 `chromium`（默认）或 `webkit`。

## 虚假上游端到端（make e2e）

```sh
make e2e   # go test -tags=e2e -count=1 -v ./test/e2e/
```

套件编译真实代理二进制，在进程内启动确定性虚假上游 `internal/fakellm`，经管理 API 创建 target、池、客户端与职级后以客户端身份发请求，核对上游实际收到的内容（路径、认证、模型名大小写）、客户端响应和 usage 事件的计费回读。覆盖原厂与 Patch 厂商矩阵（含 `standard` 反例）、Responses WebSocket、key 池限流轮换与耗尽、失败切换与取消不重试，以及 JSON/SSE 响应压缩。`TestKnownGaps` 以 SKIP 列出已知兼容缺口及观测值，`-v` 输出即当前清单。

fakellm 的回复与 token 用量由请求字节确定性推导，行为可通过请求内 `[[fake k=v ...]]` 指令、`X-Fake-Scenario` 头或 `POST /_control` 控制；preset 只代表按官方文档建模的合同，不能替代真实厂商验收。独立进程入口为 `cmd/fakellm`（`go run ./cmd/fakellm -h` 查看参数），镜像为 `Dockerfile.fakellm`。

`test/e2e/live_codex_test.go` 需要额外的 `live` 构建标签和操作者自行注入的环境变量，会调用真实模型，不属于 `make e2e`，也不进入 CI。

## bundle 账本跨版本合同（make integration）

`make integration` 包含 [`test/integration/usage_bundle_rollback_test.go`](../../test/integration/usage_bundle_rollback_test.go)，使用 [`test/integration/testdata/usage_bundle_probe/main.go`](../../test/integration/testdata/usage_bundle_probe/main.go) 分别链接当前工作区和两个固定历史提交的真实存储、journal 与计价实现，在临时目录核对 DB 已落库、仅 journal、已落库但日志未压缩、免费 root 混合旧记录，以及重复重放和继续写入。检查稳定价、精确价、条件价、记录/请求/token 计数、非计费子集和日志状态，并保留 v1.0.1 的不兼容反例。

需要 `git`、`tar` 和含测试常量所固定两个提交对象的完整 Git 历史；缺少对象会使门禁失败，不得 SKIP。CI 的 `integration` checkout 使用 `fetch-depth: 0`。固定提交仅用于数据合同验证，不是生产安全回退制品；操作限制见 [README 的账本兼容说明](../../README.zh-CN.md#未发布-bundle-账本兼容与回退限制)。这些测试不调用真实模型，也不代表生产部署验收。
