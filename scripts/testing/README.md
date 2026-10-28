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

## 用量收据、重试身份与最终快照（本机单测）

以下用例在 `go test ./internal/...` 中运行，不调用真实模型：

- `internal/nosql/usage_receipts_test.go`：`TestPrunedBundleRetryDoesNotResurrectOrDoubleCount`、`TestLegacyPrunedDatabaseRejectsUnverifiedIdentity`、`TestJournalReplayAfterPruneDoesNotDoubleCount`、`TestReceiptWatermarkMarksMissingAggregateCell`、`TestReceiptBucketRecreationRequiresNewWatermark`、`TestPruneFailsClosedWithoutReceiptBucket`。
- 同文件的 `TestLegacyDelimiterClientAggregateKeyMigration` 与 `TestAsyncJournalContinuesAfterLegacyDelimiterMigration` 验证旧聚合维度含 `|` 时迁移、重开、异步 journal 写入、回放与清理；保留或已清理 raw 均覆盖，仍按原始键核对计数并保守拒绝无法证明的历史身份。
- `internal/nosql/usage_identity_test.go`：`TestRecordOnceRejectsExplicitIdentityWithoutTimestamp`、`TestRecordOnceRetryIgnoresOnlyDerivedPricingRuleMetadata`、`TestUsageJournalPendingRetryIgnoresDerivedPricingMetadata`。
- `internal/proxy/usage_final_snapshot_test.go`：`TestClaudeConfirmedFinalSnapshotSurvivesUntrustedTail`、`TestClaudeInvalidLaterFinalDoesNotOverwriteConfirmed`；`internal/fakellm/anthropic_final_tail_test.go`：`TestAnthropicFinalSnapshotTailModes`。
- `internal/quota/accounting_identity_test.go`：`TestRecordUsageRejectsExplicitIdentityWithoutTimestampWithoutSideEffects`、`TestRecordUsageRetryIgnoresPricingRuleVersion`。
- `internal/nosql/usage_price_coverage_test.go`：`TestStableAggregateReportsMissingSupplementaryIterationPrice`、`TestStableAggregateIterationPriceScope`、`TestStableAggregatePriceRecoveryDoesNotPersistDerivedIncomplete`、`TestMixedLegacyAndNewCellIsNotAssumedCovered`、`TestStablePriceCoverageSurvivesClearedRawHistory`，以及 Summary、ReadCostUnits、模型组消费者的缺价计数回归。
- `internal/nosql/usage_watermark_v2_test.go`、`internal/nosql/usage_identity_strict_issue72_test.go`：覆盖收据水位 v2 标记、当前与未来小时的不可信边界、无法识别小时的未知范围标记、重启后边界保持，以及生成 ID、显式身份和批量写入的拒绝边界。
- `internal/nosql/usage_unresolved_persistence_issue72_test.go`、`internal/nosql/usage_integrity_issue72_test.go`、`internal/nosql/usage_blocking_issue72_test.go`、`internal/quota/accounting_integrity_issue72_test.go`：验证被拒绝或无效记录在移出 journal 前登记为未解决证据、跨重启保留，并阻断受影响客户端的费用与额度校准；只有已提交且账务完整性得到证明的摘要冲突和 bundle 不阻断，仅身份验证不够。
- `internal/nosql/usage_backfill_evidence_issue72_test.go`、`internal/nosql/usage_backfill_accounting_issue72_test.go`：回填快照保留原始聚合字节，失败回滚，重启后仍可核对。
- `internal/nosql/usage_recheck_issue72_test.go`、`internal/nosql/usage_review_recheck_p1_test.go`、`internal/nosql/usage_review_resolution_p1_test.go`：复核在身份与账务证明同时成立时把单条证据置为已解决并报告 `accounting_verified`/`accounting_restored`，包括仅剩收据与保留聚合时的证明；账务证明不足时只报告身份已验证（`identity_verified_accounting_unresolved`）并保留未解决状态，身份或证据不足时报告 `insufficient_evidence`，重启后会重新检查此前仅凭身份解决的证据，审计写入失败回滚。
- `internal/admin/usage_recheck_denials_issue72_test.go`、`internal/admin/usage_recheck_accounting_p1_test.go`、`internal/quota/usage_recheck_accounting_p1_test.go`：覆盖复核 API 的拒绝与审计、证明不足的复核不恢复账务、额度校准仍被阻断。
- `internal/nosql/usage_journal_window_issue72_test.go`、`internal/nosql/usage_source_activation_issue72_test.go`、`internal/nosql/usage_unresolved_entry_issue72_test.go`：覆盖范围 drain 跳过起点之前的已知外来行、未来或归属/时间未知的行仍使校准失败，以及范围 drain 不证明完整性，以及聚合来源缺失标记跨重启保留并在缺失额度聚合时阻断费用读取。
- `internal/nosql/usage_client_index_v2_stage1_test.go`、`usage_client_index_v2_requirements_test.go`、`usage_client_index_v2_review_test.go`、`usage_client_index_source_loss_test.go`：覆盖 v2 批次进度与 proof 的原子提交和回滚、重启续建、v1 部分进度拒绝、healthy prune 保留 proof、prune 返回此前批次计数、损坏证据阻断精确费用读取、全部锚点缺失不能认证、reindex 不修补缺失 proof 也不重新认证，以及来源丢失重建仍保留阻断标记。

以上用例覆盖工作区当前合同，不代表部署或真实上游验收。

`internal/nosql/usage_agg_v2_test.go` 覆盖版本化聚合键、旧／新格式混合读取、receipt 数量核对、回放与 backfill 边界；`internal/admin/overview_usage_count_test.go` 回读管理总览，确保含歧义分隔符的旧 cell 不丢失总请求计数。旧歧义维度不恢复，需要维度的读取明确失败。跨版本用例只核对其列出的数据合同，不能证明旧二进制支持新聚合键或安全降级。

## 虚假上游端到端（make e2e）

```sh
make e2e   # go test -tags=e2e -count=1 -v ./test/e2e/
```

套件编译真实代理二进制，在进程内启动确定性虚假上游 `internal/fakellm`，经管理 API 创建 target、池、客户端与职级后以客户端身份发请求，核对上游实际收到的内容（路径、认证、模型名大小写）、客户端响应和 usage 事件的计费回读。覆盖原厂与 Patch 厂商矩阵（含 `standard` 反例）、Responses WebSocket、key 池限流轮换与耗尽、失败切换与取消不重试，以及 JSON/SSE 响应压缩。`TestKnownGaps` 以 SKIP 列出已知兼容缺口及观测值，`-v` 输出即当前清单。

当前新增的最终快照合同用例为 `TestClaudeConfirmedFinalSnapshotE2E`（[`test/e2e/claude_final_snapshot_test.go`](../../test/e2e/claude_final_snapshot_test.go)），子用例 `advisor_tail_nonfinal`、`advisor_tail_empty_stop`、`advisor_tail_error_empty_stop`、`advisor_refusal_free_tail_nonfinal`、`replace_empty_iterations`、`fallback_output_tail_nonfinal`。它核对 root/附属记录、模型、token、诊断、失败状态和聚合费用；尾部帧是合成反例，不是官方 Claude 序列。

同一表还覆盖 `advisor_tail_invalid_iteration_retry`、`advisor_tail_omitted_iterations_retry`、`advisor_tail_message_mismatch_retry`、`advisor_tail_error_retry_final`、`advisor_tail_retry_final` 和 `advisor_tail_cost_incomplete_retry`。这些合成指令验证非法或无法证明的 iteration 候选不能丢弃已确认组件，合法替换仍更新费用，error 诊断跨替换保留，并核对费用不完整计数；不表示真实上游曾返回这些序列。

同文件的 `TestClaudeInvalidFinalTailRecoveryE2E` 验证无效终态不能污染后续合法终态；[`TestChatFinalSnapshotTailE2E`](../../test/e2e/chat_final_snapshot_test.go) 验证无 usage 的 Chat 尾帧不能改写已确认档位和费用。对应合成指令为 `iterations=advisor_tail_invalid_final_retry` 和 `chat_tail=reprice_nonfinal`，不是原厂正常输出合同。

fakellm 的回复与 token 用量由请求字节确定性推导，行为可通过请求内 `[[fake k=v ...]]` 指令、`X-Fake-Scenario` 头或 `POST /_control` 控制；preset 只代表按官方文档建模的合同，不能替代真实厂商验收。独立进程入口为 `cmd/fakellm`（`go run ./cmd/fakellm -h` 查看参数），镜像为 `Dockerfile.fakellm`。

`test/e2e/live_codex_test.go` 需要额外的 `live` 构建标签和操作者自行注入的环境变量，会调用真实模型，不属于 `make e2e`，也不进入 CI。

## bundle 账本跨版本合同（make integration）

`make integration` 包含 [`test/integration/usage_bundle_rollback_test.go`](../../test/integration/usage_bundle_rollback_test.go)，使用 [`test/integration/testdata/usage_bundle_probe/main.go`](../../test/integration/testdata/usage_bundle_probe/main.go) 分别链接当前工作区和两个固定历史提交的真实存储、journal 与计价实现，在临时目录核对 DB 已落库、仅 journal、已落库但日志未压缩、免费 root 混合旧记录，以及重复重放和继续写入。检查稳定价、精确价、条件价、记录/请求/token 计数、非计费子集和日志状态，并保留 v1.0.1 的不兼容反例。

需要 `git`、`tar` 和含测试常量所固定两个提交对象的完整 Git 历史；缺少对象会使门禁失败，不得 SKIP。CI 的 `integration` checkout 使用 `fetch-depth: 0`。固定提交仅用于数据合同验证，不是生产安全回退制品；操作限制见 [README 的账本兼容说明](../../README.zh-CN.md#未发布-bundle-账本兼容与回退限制)。这些测试不调用真实模型，也不代表生产部署验收。
