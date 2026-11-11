# 统一失败出队（Unified Failover）设计

- **状态**：评审稿
- **日期**：2026-11-08
- **范围**：`internal/proxy` 的目标选择、转发失败分类与响应交付；不含管理端 UI 与计费规则变更。
- **引用约定**：未写目录的 `*.go` 文件均位于 `internal/proxy/`；其他文件按完整路径标注。

**结论摘要**：新增单一分类器，把“未提交且未处理、且不属于 401/402/403/408/429 的上游 4xx”转为出队；5xx/408、403、已提交响应与本地请求错误均不进入出队面。队列不物化，复用现有成本选路与 `attempted` 集合；4xx 响应按 attempt 缓存，队列耗尽时原样回放第一个真实错误。

## 1. 问题与目标

LLMS Proxy 在 `cost` 部署池中按估算价格从低到高排序候选（A 最便宜，其后 B/C/D/E）。当前同一次客户端请求内，跨 target 的 failover 只由 `forwardAttemptError.retryable=true` 驱动（service.go:1382-1413、1454-1480）；上游返回非 2xx 时，响应在 `writeResponse` 原样透传，5xx/408/429 仅 `MarkFailure` 供后续请求避开，普通 4xx 反而走 `MarkSuccess`（forward.go:864-910、1835-1843）。

结果是：当 A 对某类请求稳定返回 4xx 时，每个请求都在 A 上失败，B/C/D/E 永远不会被尝试。已知场景是百炼 Claude 兼容端点对 Claude Code 的 `tool_addition` 返回 400（该现象来自任务背景，本次未在仓库内复现，见第 14 节）。这不是单次抖动，而是选路语义把“上游拒绝”误当成“客户端问题”。

设计目标：

- 把“本次候选没有服务成”统一为出队语义：未处理的失败继续下一个候选，已处理的失败立即终止。
- 客户端无感：不因内部换 target 改写请求语义；成功路径的协议、用量与响应字节不变。
- 全部候选失败时交付真实上游错误，而不是笼统 503。

非目标：

- 不改变 keypool 对 401/402/429 的既有收敛规则，不扩大同 key 重试；403 不跨候选出队，key 级 403 沿用既有分类，耗尽后终止。
- 不重放已向客户端提交的响应，不重放可能已推理或计费的 5xx/408/已发送超时。
- 不引入 alias 级健康、熔断或第二套重试状态机。

## 2. 已确认的代码现状

- 跨 target failover 只认 `forwardAttemptError.retryable`（service.go:1382-1413）；`isUpstreamFailureStatus` 只用于健康判定，不产生重试（forward.go:1835-1843）。
- 5xx/408/429 在 `recordOutcome` 中 `MarkFailure`，但响应原样透传；其他 4xx `MarkSuccess`（forward.go:864-910）。
- keypool 第一层已处理：401/402 由 `targetKeyExhausted` 按响应体分类（forward.go:655、provider_error.go:49-108）；429 有同 key 重试、换 key、`reqTotalFails>=4` 安全阀（service.go:1150-1275）；全 key 耗尽才以 `retryable=true, skipMute=true` 升级到 target failover（forward.go:158-168）。GLM 200+SSE 与 MiniMax 200 JSON 的 in-band 限流也在 keypool 处理（forward.go:660-734）。
- 预发送判定 `isPreSendTransportError` 只允许拨号/TLS/WS 握手超时重试；已发送的超时/取消不重试（forward.go:1728-1750）。
- SSE 在拷贝 body 前就 `WriteHeader(200)` 并立即 Flush（forward.go:862、925-926）；WS 升级前 400/404/405/426/501 是同 target 的 HTTP 回退（responses_ws.go:952-957、responses_websocket.go:901-915），升级后绝不重放（responses_ws.go:1088-1105）。
- 健康状态按 target 代际隔离（target.go:19-60、target_identity.go:35-78），`breakerThreshold` 默认 1（target.go:112-140）：一次 5xx 即可 mute 60s。
- 候选过滤一直以客户端请求名（trim+lower）为单位（service.go:706-707、sanitize.go:354-397、cost_routing.go:25-35、98-139）；IP 粘连 Set 侧写上游原始拼写，Get 侧先 alias 再枚举成员 upstream 拼写（service.go:1464-1470、target.go:391-430、534-575）；会话绑定用客户端 alias（affinity.go:64-80）。本设计不改粘连存储身份。
- 用量 `Event.Model` 必须是实际服务 target 的 upstream（forward.go:1424-1512、internal/usage/store.go:50-62）；非 2xx 不写用量（已发送生成的 WS 终态沿用既有 `responseObservesUsage` 规则）；一个请求至多一个 bundle；已发送未响应走 `pendingNoUsage`（no_official_usage.go:97-146、314-330）。
- 既有红线：`internal/proxy/AGENTS.md:38-47` 要求普通上游 4xx/5xx 默认透传、不做通用重试；`test/e2e/resilience_test.go:103-107` 明确 5xx 不重放，因为上游可能已计算。

## 3. 核心模型：一次 attempt 的结果

### 3.1 判定维度

判定对象是“一次尝试的结果”，不是错误码本身。四个维度：

| 维度 | 定义 | 判定要点 |
| --- | --- | --- |
| Committed | 是否已向客户端写出状态/头/首字节 | SSE 200 头已 Flush（forward.go:925-926）、WS 101、普通 HTTP 已 `WriteHeader`（forward.go:862）均为 true |
| MayHaveProcessed | 上游是否可能已处理/计费 | 请求已完整写出但无响应、5xx/408、已发送超时均为 true；上游 4xx 在 headers 阶段、预发送传输错误、本地请求错误为 false |
| Scope | 失败归属 | Key / Target / Request / Client |
| Action | 本 attempt 的处置 | SwitchKey / Dequeue / Terminate |

### 3.2 决策规则（按优先级）

| 优先级 | 条件 | 动作 |
| --- | --- | --- |
| 1 | `Committed=true` 或 `Scope=Client` | Terminate |
| 2 | `Scope=Request` | Terminate，不进入队列 |
| 3 | `Scope=Key` | 先走现有 keypool 状态机；401/402/429 全 key 耗尽后 Dequeue（显式目标除外；429 安全阀命中则 Terminate），403 耗尽后仍 Terminate |
| 4 | `Scope=Target` 且 status=403（账户/权限拒绝） | Terminate：跨候选重放会放大客户端授权错误并可能触发账号锁定 |
| 5 | `Scope=Target` 且 `MayHaveProcessed=false` | Dequeue（含既有预发送传输错误路径） |
| 6 | `Scope=Target` 且 `MayHaveProcessed=true` | Terminate |

规则 1 的“提交即终局”与传输无关：SSE 已 Flush 的 200、WS 已 101、普通 HTTP 已写头，都不能再换 target 重放，否则客户端会看到两段响应或两次生成。

规则 3 的 Key 作用域包括：上游 401/402（凭据/计费分类）、429（限流）、GLM/MiniMax 的 200+in-band 限流。它们必须先经过 `targetKeyExhausted`、同 key 重试、换 key 与全局安全阀；只有全 key 耗尽、且当前请求允许 fallback 时，才由 `retryable` 升级为 Dequeue。429 命中 `reqTotalFails>=4` 安全阀时按现状直接交付 429，不再 Dequeue（service.go:1171-1186）。Key 作用域的 `MayHaveProcessed` 不参与决策；429/402 不能绝对证明上游未推理或未扣费，因此不据此重放生成，只按既有 key 规则做有限同 key 重试或换 key。单 key 目标的 401/402 同样按凭据/账户失败处理，不能因为“只有一个 key”就绕过 key 状态直接按普通 4xx 出队。403 即使被厂商 Patch 归为 key 级（如额度类），也只按既有 key 规则处理，全 key 耗尽后仍 Terminate，不跨候选重放；通用账户/权限 403 按规则 4 直接终止。原因是账户/权限拒绝跨候选重放会放大客户端授权错误并可能触发账号锁定。显式 `X-Proxy-Target` 时 `allowFallback=false`（service.go:621），Dequeue 降级为 Terminate，只允许同 target 切 key。

规则 5 是本次唯一新增的出队面（预发送传输错误沿用既有 `retryable` 路径，不属于新增 4xx 语义）。非 401/402/403/408/429 的 4xx 表示上游明确拒绝且未进入生成；代理本地的 JSON 解析、model 缺失、Content-Type/body 大小与歧义等请求级错误已在代理侧拦截（service.go:620-712），能到达上游的 4xx 主要是该 provider 特有的拒绝，换一个候选有实际意义。

规则 6 是与“所有错误都出队”直觉的另一例外，必须保留：5xx/408 可能已推理、已生成或已扣费，尤其是图片生成；重放会重复计费。已发送超时/断流同理。既有 e2e（`test/e2e/resilience_test.go:103-107`）与 `internal/proxy/AGENTS.md:38-47` 红线也要求 5xx 不重放。

### 3.3 单一分类器

只新增一个分类入口 `classifyNon2xxAttempt`：输入 `status`、keypool `reason`、path/transport、`Committed`、`MayHaveProcessed`，输出 `SwitchKey` / `Dequeue` / `Terminate`。不新建第二套状态机，不按状态码写散落的分支。200+in-band 错误不进入该分类器，继续由 keypool 处理。

## 4. 失败矩阵

| 场景 | Committed | MayHaveProcessed | Scope | 动作 | 健康/用量 |
| --- | --- | --- | --- | --- | --- |
| 拨号/DNS/TLS/WS 握手超时（未发送） | false | false | Target | Dequeue（既有行为） | 现有 `isPreSendTransportError` 语义；可降级 target |
| 已发送超时/断流 | false | true | Target | Terminate | 504/502；MarkFailure；pendingNoUsage |
| 客户端断开/取消 | false | n/a | Client | Terminate | 499；不 mute；按现有 0 token 结算 |
| 上游 401/402 | false | n/a | Key | SwitchKey，耗尽后 Dequeue | key 冷却；target 不因账户失败刷新粘连 |
| 上游 429 | false | n/a | Key | 同 key 重试/换 key/安全阀；耗尽后 Dequeue，安全阀命中则 Terminate | key 冷却；`skipMute` 不 mute target |
| 上游 403（厂商 Patch 识别为 key 级，如额度类） | false | n/a | Key | 走既有 key 循环；耗尽后 Terminate，不出队 | 沿用 keypool/健康既有规则；不跨候选重放 |
| 上游 403（通用账户/权限拒绝） | false | false | Target | Terminate | 不缓存、不出队；按既有 4xx 结算（账户级失败沿用 keypool 判定），不写用量；跨候选重放会放大客户端授权错误并可能触发账号锁定 |
| 其他上游 4xx（headers 阶段，403/408 除外） | false | false | Target | Dequeue | 缓存并在丢弃点 MarkSuccess |
| 5xx/408（headers 阶段） | false | true | Target | Terminate | 原样透传；MarkFailure；mute 一个 target |
| SSE 4xx（写头前，403/408 除外） | false | false | Target | Dequeue | 同普通可出队 4xx；403/408 Terminate |
| SSE 200 已 Flush 后失败 | true | true | — | Terminate | 不重放；按现有中断结算 |
| WS 升级前 400/404/405/426/501 | false | false | Target | 同 target HTTP 回退 | 不记为 target 失败 |
| WS 升级前 401/402/429 | false | n/a | Key | 走既有 key 循环 | 沿用 keypool/健康既有规则 |
| WS 升级前其他完整拒绝 | false | 4xx false / 5xx true | Target | 非 403/408 的 4xx Dequeue；403/408/5xx Terminate | 按普通 HTTP 失败矩阵处理，不触发同 target 的再次回退 |
| WS 握手不完整/超限 | false | true | Target | Terminate | 不重放 |
| WS 升级后（101） | true | true | — | Terminate | 绝不重放 |
| Responses 已转发任一 event | true | true | — | Terminate | `generationSent` 是权威信号 |
| 图片生成 4xx | false | false | Target | Dequeue（403/408 除外） | 上游拒绝、未生成；403/408 Terminate |
| 图片生成 5xx/已发送超时 | false | true | Target | Terminate | 可能已生成计费 |
| count_tokens 4xx | false | false | Target | Dequeue（403/408 除外） | 只读幂等；不写用量；403/408 Terminate |
| 本地 quota/admission/accounting/参数错误 | false | n/a | Request | Terminate | 不选 target、不进入队列 |

## 5. 队列语义与候选身份

队列不物化。复用 `selectTarget` 与 `attempted` 集合：每次 Dequeue 结算当前 attempt，当前 target 已在 `attempted` 中（service.go:853、target.go:374），然后重新进入 `selectTarget`；复用同一 cost ranker 的排序规则（同一客户端 alias、同一请求时点），不预生成候选列表，也不维护下标游标。`attempted` 本身保证不会回到已试过的 target；成本排序、健康、容量、探针与准入仍在每次选择时重新求值。

灾备身份（DR identity）是 trim+lower 的客户端请求名（alias）。候选是所有通过 ACL、未 `paused`、`modelAllowed`、`SupportsPath` 的 target；upstream 名只用于该 target 的 wire 请求、计价与用量，不参与“哪些 target 属于同一模型”的判断（cost_routing.go:25-35、98-139）。`model_mappings`、`model_prefix` 仍按目标各自解析。

显式 `X-Proxy-Target`（或 query `target`）时 `allowFallback=false`（service.go:621）：Dequeue 降级为 Terminate，只允许同 target 切 key；不因 4xx 跳到其他 target。

健康状态仍按 target/key 维护，不新增 alias 级健康或熔断。alias 只用于候选过滤、队列顺序与诊断。理由是单 provider 故障若扩散为 alias 级不可用，会把其他仍可服务的 provider 一起排除；IP 粘连的 Set/Get 两代 key 兼容也不动（target.go:391-430、534-575）。所有策略都不主动打断粘连（已删除 `cost` 池的 `selectionCostRebind` 让出路径）：粘连目标无法承接时临时改选，但不迁移原绑定。

## 6. 响应缓存与回放

只为“可出队的 4xx”缓存响应：`(status, 原始 headers, 有界原始 body)`。约束：

- 缓存 per-request，绑定产生该响应的 `origin targetState` 与 `upstreamModel`；请求结束即丢弃，不做跨请求复用。
- body 上限 64 KiB（原始 wire 字节）。超过上限时不缓存，终止出队并交付该响应；不能为了凑缓存截断 body 或改写 `Content-Length`。
- 不缓存 2xx body，也不缓存 403 与 keypool 已消费的 401/402/429 拒绝；403 不进入出队面，后者由既有 key 状态机与终态交付负责。
- 回放必须原样：`Content-Type`、`Content-Encoding`、`Content-Length`、request-id 等头与 body 成对回放；gzip/br 等编码保持原始字节，不解压后重压，不把压缩体当明文。hop-by-hop 头仍按现有 `hopHeaders` 规则过滤（forward.go:1612-1621）。
- 绝不混用不同 attempt 的 header/body；每次回放只读取同一缓存项。
- 队列耗尽时回放第一个已缓存的可出队 4xx（按尝试顺序），保留 `X-Proxy-Target` 与 `X-Upstream-Request-Id`（forward.go:848、855-856）以及上游 `request_id`。
- 没有任何真实响应可回放时，按最后一次失败合成 502/503/504：传输/上游读失败用 502，超时用 504，无候选或全被准入拒绝用 503。仅在没有任何缓存 4xx 时，keypool 已有的终态（如全 key 耗尽的 429 限额响应）优先按现状交付，不被合成错误覆盖。

错误体读取失败、编码不可原样回放或 body 超限时，终止出队：body 超限交付该 attempt 的原始响应；读取失败或编码不可原样回放时按该 attempt 的失败交付 502/504。不得用部分 body 或后续 attempt 的头补齐。该边界在 P0 用测试固定。

## 7. 健康、探针与结算

每个 attempt 必须恰好一次 `releaseProbe` 并完成结算。半开探测租约属于选中它的请求；同 target 的 key 重试保留租约，Dequeue 到下一 target 前释放旧租约（service.go:781-792）；缓存回放与终止路径同样释放，避免 half-open 永久卡死。禁止重复释放或把旧租约释放到新选择上。

健康语义：

- 4xx 出队在丢弃点按现有 `MarkSuccess` 语义记可达（至少释放 probe）；取舍是“4xx 仍算 target 可达”，因为上游确实响应了，且熔断不应因请求内容问题而隐藏一个可服务节点。代价是粘连或成本首跳可能反复命中同一 4xx target；P2 再评估短期冷却。
- 5xx 保持 `MarkFailure`；`skipMute` 的 key/容量失败不 mute。由于 5xx 不 Dequeue，一次请求不会因 5xx 连续 mute 后续所有候选；传输层 failover 可能已降级前一个 target，但总量有限，不会出现“每个 target 都 5xx 出队导致全站 mute 60s”。
- 账户级失败收敛后不刷新/建立 IP 粘连，沿用 `accountLevelFailure` 现有处理（service.go:1458-1470）。

用量：

- 成功响应拥有唯一 usage；4xx 出队不写 usage。
- 已发送但未取得响应的 attempt 走现有 `pendingNoUsage`，请求结束时至多记一条 0 token 不完整事件（no_official_usage.go:97-146）。
- `Event.Model` 始终保持实际服务 target 的 upstream，不写 alias；一个请求最多一个 bundle（`internal/proxy/AGENTS.md:38-47` 红线）。

## 8. 特殊路径

- SSE：可出队的 4xx（403/408 除外）必须在 `WriteHeader` 之前拦截并缓存；200 头一旦 Flush 即 `Committed=true`，后续失败只能 Terminate，不重放。
- WebSocket：升级前 400/404/405/426/501 保持同 target 的 HTTP 回退，不记为 target 失败，回退后的 HTTP 结果按普通 HTTP 失败矩阵处理；其他完整拒绝中，401/402/429 走 keypool；非 403/408 的 4xx 可 Dequeue；403/408/5xx Terminate；不完整或超限握手 Terminate；升级后（101）一律 Terminate。
- Responses：`generationSent` 是“已输出”的权威信号（responses_ws.go:995-1004、forward.go:1601-1610）；已转发任一 event 即 Terminate，不新造平行信号。
- 图片生成：非 403/408 的 4xx 可 Dequeue（上游拒绝、未生成）；403/408/5xx/已发送超时 Terminate（可能已生成并计费）。
- count_tokens：只读幂等，非 403/408 的 4xx 可 Dequeue；403/408 与 5xx 仍按统一规则 Terminate，不因只读扩大 5xx 重放面。若评审希望只读请求的 5xx 也出队，应作为显式扩展单列。

## 9. 可观测性

只加聚合计数器，不加 alias/target 标签（高基数）：

| 计数器 | 含义 |
| --- | --- |
| `dequeued_total` | attempt 被判为 Dequeue 的次数 |
| `dequeue_exhausted_total` | 队列耗尽（含缓存回放或合成错误）的次数 |
| `cached_error_replayed_total` | 缓存 4xx 被回放的次数 |
| `dequeue_by_status_class` | 按状态类（4xx/5xx/transport/local）聚合的分类次数；其中只有非 401/402/403/408/429 的 4xx 应产生 Dequeue |

暴露到 `/admin/metrics` 的 JSON 字段需与 `ServiceMetrics` 精确对应（service.go:170-197；参照 `internal/admin/handlers_status.go:73-74、246-247` 的既有字段）。日志记录 from/to target、status、origin request-id，不含 body、凭据或上游 URL。

## 10. 分阶段

**P0：最小可用**

- `classifyNon2xxAttempt` + 可出队 4xx 出队 + 缓存回放 + probe settle。
- 不改 5xx、403、SSE、WS 的既有语义；可出队 4xx 只在写头前拦截。
- 更新 `TestPoolFailover` 中 4xx 的期望，新增 4xx 出队与缓存回放用例。

**P1：收敛与观测**

- 复核 key 耗尽升级：全 key 耗尽后的 401/402/429 终态交付是否应保留最后一次真实响应。
- 落地第 9 节计数器与 admin JSON 字段测试。
- 同步文档：在 `internal/proxy/AGENTS.md:38-47` 的失败矩阵红线中明确“仅非 401/402/403/408/429 的 4xx 可出队、5xx 仍不重放”；更新 `README.md:211-213` 与 `README.zh-CN.md:211-213` 的失败/重试章节；在 `internal/admin/AGENTS.md` 补充指标契约，并在 admin 测试中断言新增 JSON 字段的精确名称与类型。

**P2（可选）**

- 4xx 首跳短期冷却，避免粘连反复命中同一坏 target。
- admission 容量与出队统一：Dequeue 后候选的配额/并发检查与首次选择一致。

## 11. 测试清单

- 分类矩阵：每种 `(Committed, MayHaveProcessed, Scope)` 组合只产生一个动作；403 与 5xx/408 永不 Dequeue；Scope=Request 永不 Dequeue。
- 成本队列：A 返回可出队 4xx（如 400），B 返回 200；断言 A→B 顺序、客户端只收到 B 的响应、用量 `Event.Model` 为 B 的 upstream。
- 队列耗尽：所有候选返回可出队 4xx 时回放第一个已缓存的可出队 4xx；断言 status、原始 headers、body、`X-Proxy-Target`、`X-Upstream-Request-Id` 与上游 request-id 均来自同一 attempt。
- 5xx：不重放、不尝试 B、原样透传、只 mute 一个 target、`totalTargetRetries` 不增加。
- keypool：401/402/429 先同 target 切 key；单 key 401/402 走凭据/账户失败后再升级；403 不跨候选出队（key 级 403 仍按既有 key 规则处理，耗尽后终止；通用账户/权限 403 直接终止）；显式 target 不 Dequeue。
- SSE：可出队 4xx 写头前出队，403/408 Terminate；200 Flush 后失败 Terminate，客户端只收到一段流。
- WS：升级前 400/404/405/426/501 仍同 target HTTP 回退；其他完整拒绝按矩阵；101 后失败 Terminate。
- Responses：任一 event 转发后失败 Terminate，不重放。
- 图片：非 403/408 的 4xx Dequeue、403/408/5xx Terminate。
- count_tokens：非 403/408 的 4xx Dequeue。
- probe：每条 Dequeue/缓存回放/终止路径恰好释放一次；half-open 不卡死。
- 缓存边界：body 恰好 64 KiB、超过 64 KiB、gzip/br、读取失败、`Content-Length` 不匹配。
- 用量：4xx 出队不写 usage；已发送未响应产生至多一条 0 token 事件；成功路径一个 bundle。
- 指标：四个计数器按预期递增，admin JSON 字段名与类型固定，无 alias/target 标签。
- e2e：更新 `test/e2e/resilience_test.go` 的 4xx 期望；保留 5xx 不重放用例。

## 12. 评审反例与约束

- 不要把 5xx/408 纳入出队：可能重复推理/扣费，尤其图片生成。也不要把 403 纳入出队：账户/权限拒绝跨候选重放会放大客户端授权错误并可能触发账号锁定。
- 不要按状态码宽泛重试；判定必须经过 `classifyNon2xxAttempt`。
- 不要把 Scope=Request 映射为 Dequeue；本地 quota/admission/accounting/参数错误直接终止。
- 不要新建第二套状态机或预生成队列；只加一个分类器，复用 `selectTarget` + `attempted`。
- 4xx 出队不要绕过 keypool：401/402/429 必须先走 key 循环；单 key 目标的 401/402 也按凭据/账户失败处理。403 不进入出队面：通用账户/权限 403 直接终止，厂商 Patch 对 403 的既有 key 级分类不变，耗尽后终止。
- 一次请求最多 mute 有限 target；5xx 不出队已经避免“一次请求熔断全站”。
- probe lease 在每条出队/缓存回放路径都要释放，避免 half-open 卡死。
- alias 不做跨 target 健康身份，否则单 provider 故障会扩散；IP 粘连两代 key 兼容不动。
- 不要缓存 2xx body 或 403；错误体超限、gzip/br、`Content-Length`/`Content-Encoding` 必须原样回放，不得混用 attempt。
- 200+in-band 错误（GLM/MiniMax）仍归 keypool，不进入本次分类器。

## 13. 待决策项

| 决策 | 推荐 | 理由与影响 |
| --- | --- | --- |
| 5xx 是否纳入出队 | 不纳入 | 可能已推理/扣费；纳入必须改 e2e、AGENTS、README，并显式接受重复计费 |
| 4xx 范围 | 非 Key 作用域且非 403/408 的上游 4xx | 本地错误已在代理侧拦截，上游 4xx 多为 provider 特有拒绝；401/402/429 仍走 keypool。403 是账户/权限拒绝，跨候选重放会放大客户端授权错误并可能触发账号锁定，列为终止；408 可能在请求已完整发送后返回，同样终止。如评审要求白名单，400/404/422 可保留，413/415 因 body 级且跨 target 大概率确定性失败，可排除 |
| 4xx 健康语义 | 沿用 `MarkSuccess`（可达） | 不新增 `MarkNeutral`；避免粘连反复首跳可在 P2 加短期冷却 |

## 14. 未验证/待确认

- 百炼 Claude 兼容端点 `tool_addition` 400 的生产现象来自任务背景，本次未在仓库或真实上游复现；需用真实请求确认状态码、错误体编码与是否在 headers 阶段返回。
- “可出队的上游 4xx 一定未进入生成/未计费”是设计假设；个别厂商可能在 4xx 前已产生部分计费，需按 provider 抽样验证。
- 64 KiB 缓存上限是工程取值，未对真实错误体分布做统计；gzip/br 错误体超限的比例未知。
- WS 不完整/超限握手的现有分类只做了代码核对，未做真实上游矩阵测试。
- `count_tokens` 的 5xx 是否应例外，需评审确认。
- 新增计数器与 admin JSON 字段的最终命名需与现有指标契约对齐；`internal/admin/AGENTS.md` 是否已逐字段描述这些字段需在 P1 确认，如无则补充。
- keypool 全 key 耗尽后的最后一次 401/402/429 是否应作为真实响应交付，留待 P1 复核。
## 15. 模型级 429 冷却、代理内有界等待与粘连终态仲裁

- **触发事故**：同一客户端同时用 qwen-max 与 DeepSeek 别名，两者落到同一百炼目标（上游 `qwen4-max-preview` 与 `deepseek-v4.1-flash`），DeepSeek 在百炼不可用时另有官方渠道。百炼对 `qwen4-max-preview` 按模型订阅限流返回 429，按目标熔断整个百炼 60 秒，DeepSeek 会话被挤到官方渠道；官方因思考模式缺历史思考块返回 400，Claude Code 对 400 直接停止。
- **原则**：客户端尽量少感知错误，平台替客户端能做的都做；红线不变（已发送请求不重放、5xx/408/发送后超时不重试、客户端取消立即停止、不为确定性客户端错误无限重试）。

### 15.1 B：429 冷却按（target, 上游模型）

- `targetHealthState.modelLimits`（键为目标实际转发的上游模型原始拼写；目标无 `model_mappings` 时键为小写请求名，上限 32 项，溢出退回目标级冷却）保存 `until/streak/status/reason`。`markTargetFailure` 新增 `upstreamModel` 参数：类别为 429 且模型非空时走模型级（`Retry-After` 精确不翻倍，否则按该模型连续次数以 429 类基数翻倍、封顶 8 倍；冷却期内迟到失败只计数），其余类别（账户拒绝、网络、5xx、408、504）仍为目标级。多 key 目标的 429 见 §15.4（按（key，模型）记录，全部 key 对该模型被限流时进入模型级冷却）。
- 模型级 429（计数的真实应答）把目标按“已被一次 4xx 证明可达”处理：目标未处于冷却时重置其连续失败（半开探针抽到 429 即恢复目标），运行中的目标级冷却仍由自己的探针结束。
- 选路在判断目标可用前，用目标对本请求别名解析出的上游模型（`resolveUpstreamModel`）检查模型级冷却：粘连命中、`findAvailableRankedDetail`、显式目标都先于准入（不占探针租约）；被冷却的目标对本请求视为冷却中、计入 `blockage.earliestRecovery`，对其他模型照常可用，不作为提前探测候选。成本分层的准入过滤在分层之前，整层不可用才溢出。
- 清除：同一模型的真实成功（`writeResponse` 成功分支）或持有模型探针的请求得到任何非 429 的上游应答，清该模型条目；其他模型的成功、请求类 4xx、`markReachable` 都不清。`ClearMute` 清空全部；身份分叉的新健康对象从空开始。
- 可观测：`TargetStatus.ModelCooldowns` 与 `/admin/metrics` 的 `targets_runtime[].model_cooldowns[]`（`model/until/status/reason`）；`phase` 在仅有模型冷却时保持 `ready`；成本预览按别名解析出的上游模型把成员相位标为 `muted` 并带 `model_cooldown_until`；管理端目标列表/详情显示“部分模型限流”。

### 15.2 C：代理内有界等待

- 选路失败且尚未写出任何字节时，满足全部条件才等待一次：(1) 不是显式目标、不是 MiniMax 终态；(2) 请求被粘连目标持有且该目标可等（冷却或探针在途，本请求未向它发送过），或阻塞全部短暂（`selectionError.Transient`：模型级 429 冷却、非账户拒绝的目标级冷却，且最早恢复时间已知）、无缓存的 4xx、本请求未收到 429；(3) 最早恢复时间距今 ≤ `inProxyWaitMax`（30 秒）；(4) 全局等待名额未满（64，原子计数）。**满载不等待**（没有恢复时间）。等待 = 恢复时间 + 0-500 ms 抖动，只受父上下文约束（不计入 `request_timeout`；取消按 499、父 deadline 到期按 504）；选路失败分支位于 `releaseSelected` 之后，等待期间不持有目标并发槽、key、探针租约（目标级与模型级）、准入令牌。
- 唤醒后 `attempt--` 并重新选路。粘连持有的请求先单独对未尝试过的粘连目标准入（`selectionTrace.retrySticky`），恢复了就回到它。**每请求每目标至多发送一次**：已在 `attempted` 中的粘连目标（发送前失败、已作答）既不等待也不重发，直接 503 + Retry-After；每请求至多等待一次。
- **半开门**：目标级冷却与模型级冷却结束后都只放一个探针请求（模型级为 `modelProbe` 租约，随请求持有，成功清条目、429 再冷却、其他结果释放），其余请求视为“探针在途”。多个等待者同时醒来时，一个充当探针，其余在一次最长 3 秒（`inProxyProbeWaitMax`，每 200 ms 重选一次）的短轮询里等探针结算：要么全部成功，要么 3 秒内得到 503 + Retry-After。
- 延迟上界：原先立即 503 的请求现在可能延迟最多约 30.5 秒（30 秒等待 + 0-500 ms 抖动），遇探针在途时另加至多 3 秒轮询（凡探针在途，没等过的请求也轮询，并把它算作唯一的一次等待），合计约 33.5 秒。生产入口 nginx 的 `proxy_read_timeout` 为默认 60 秒（只读确认），仍有余量；`inProxyWaitMax` 因此保持 30 秒，不要调大到接近该值。
- 指标：`in_proxy_wait_total`、`in_proxy_wait_success_total`（等待后最终成功）、`in_proxy_wait_rejected_total`（无名额）、`in_proxy_waiting`（当前等待者）；探针轮询不单独计数。

### 15.3 A：等不到时的终态仲裁

- 粘连持有 `stickyVerdictPending`（只读，不抢探针、不改状态）：本请求首轮选路命中粘连（会话或 IP，`selectionTrace.bound`）；粘连目标仍是合法候选（存在、未暂停、ACL、模型、路径）；本请求内没有任何绑定目标返回过 4xx（`stickyAnswered`，429 亦然；有则以作答者的应答为结论）；不处于账户拒绝冷却；它暂时不可用——被本请求模型的 429 冷却、目标级冷却、半开探针阻塞，或满载/IP 软上限——或已在本请求发送前失败。已知恢复时间超出绑定的上限则不持有（会话绑定 10 分钟 `retryAfterMax`，IP 亲和 120 秒 `ipStickyHoldMax`），回放替代目标的 4xx。
- 持有成立时终态一律 `503` + `Retry-After`（向上取整、≥1；取粘连目标恢复时间与其余候选最早恢复时间中较早者；满载与探针在途为 1）+ 代理生成的 `upstream_temporarily_unavailable` 错误体（`writeStickyRetry`），优先于 429、`upstream_rejected` 与裸 503 分支（显式目标、MiniMax 终态除外）。有缓存的替代 4xx 时带 `X-Upstream-Status` 与 `error.substitute_error_code`（替代目标的 `error.code`，缺省取 `type`，至多 64 字符，仅供排障；不点名目标、不回放正文），没有缓存错误时两者省略。计 `sticky_retry_hinted_total`，有缓存错误时另计 `dequeue_exhausted_total`，不计 `cached_error_replayed_total`；绑定不动，目标恢复后客户端自动回到它。原生 WS 轮次得到同形错误事件并带 `retry_after`。
- 粘连目标自己的 4xx（含 429）出队时整体替换 `cachedFailoverError`（状态、头、体、目标、request-id 成组，并重新记 trace）。
- 普通“全部冷却/满载/恢复中”的 503 补 `Retry-After`（最早恢复时间，≥1 秒）。
- 保持原样：粘连目标自己的 4xx、无粘连请求的首个真实 4xx（不改 503，避免坏请求无限重试）、确定性错误白名单、审核类 403、客户端凭据 401/403、显式 `X-Proxy-Target`、5xx/408、发送后超时、已提交响应、本地 quota/参数/415/499、粘连目标处于账户拒绝冷却时按原逻辑回放（它已长期不可用）。

### 15.4 多 key 目标：按（key，上游模型）的 429

- keypool 原先的 `rate_limited` 不分模型，同一 key 上一个模型的 429 会让它的其他模型也被换 key 或耗尽。现在通用 429 只记 `keyRuntime.modelLimits`（5s 起翻倍、封顶 60s，每 key 至多 32 项，溢出回退 key 级），key 保持可用，选 key（`selectKeyForClientModel` 等）按本请求上游模型跳过被限流的 key；同 key 有限重试不扩大：每个 key 在一次请求内至多同 key 重试一次（请求内本地计数 `sameKeyRetries`，不依赖会被其他模型成功清零的按 key 共享计数），全局四次上限不变。账户类（401/402/403、额度耗尽）与 GLM 选择目标的 429 仍是 key 级。
- 所有未被封的 key 对该模型都被限流时，`forwardRequest` 先走原有唤醒路径（尚有可唤醒的 exhausted key 时），仍无 key 可用才返回带 `modelLimitedUntil` 的 `skipMute` 错误（不当作耗尽），`service.go` 对该（目标，模型）`setModelCooldown` 到最早 key 恢复（不计失败；上游没有应答，不恢复半开中的目标、不清失败/超时计数），之后与单 key 目标走同一条模型级冷却 / hold / 等待路径。模型限流导致的溢出选 key 只服务本次请求，不创建或移动客户端的 key 亲和绑定。该次 429 已作答，目标在 `attempted` 中不重发，也不再用 `lastForwardStatus=429` 抑制粘连 hold 与等待。`KeyStatus.ModelLimits` 供管理端展示。

### 15.5 取舍与已知限制

- 多个等待者同时醒来时一个充当探针（目标级或模型级），其余在 ≤3 秒的短轮询里等它结算，探针更慢则这些请求 503 + `Retry-After`。
- 粘连等待不检查其余候选的账户拒绝冷却：等待的目的是粘连目标本身的恢复。
- 本设计基于代码核对与 fakellm 的确定性场景，未经真实上游 API 验证。
