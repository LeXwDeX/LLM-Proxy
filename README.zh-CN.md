# LLMs Proxy

[English](README.md) | 简体中文

LLMs Proxy 是一个 Go HTTP 服务，为客户端提供统一的模型 API 入口，并将请求转发到配置的上游 provider。它支持客户端鉴权、模型别名映射、目标路由、用量记录、价格核算和 Web 管理界面。

源码仓库：<https://github.com/LeXwDeX/LLM-Proxy>。开发约束见 [`AGENTS.md`](AGENTS.md)。

## 能力概览

- `azure_openai`、`openai`、`claude`、`gemini`、`openai_image` 和 `dual_protocol` 上游类型。
- DeepSeek、阿里百炼及旧类型别名映射到 `dual_protocol`；规范类型列表以 [`internal/config/endpoint_type.go`](internal/config/endpoint_type.go) 为准。
- OpenAI 风格 `/v1/*` 代理入口、本地模型列表、管理 API 与 Web UI。
- 基于 bbolt 的内嵌持久化，无需单独安装数据库。
- 按配置的官方 API 价率、模型组系数和 usage 记录核算用量。

### 额度方案同步

额度管理的「批量操作」支持同步到指定客户端、整个项目组或整个职级。每次同步在同一个数据库事务中读取源客户端的方案、确定目标名单并完成整批覆盖；任一校验或写入失败，整批回滚。组合选择条件取并集，排除源客户端自身，成功后返回实际同步名单并刷新这些客户端的额度状态。

覆盖内容是完整的模型组选择及每组个人可用额度倍率，包括留空、显式 `0` 和正数。目标原有的额外模型组会被移除，源方案为空时清空目标方案。目标保留自己的职级绑定，日/周/月可用额度仍按「模型组基数 × 目标职级倍率 × 个人倍率」计算；身份、密钥、授权和历史用量保持原值。同步完成后的独立编辑属于后续操作，不建立持续跟随源客户端的关系。

### 原始用量客户端索引

启动后会在后台分批建立派生的 `usage_events_by_client_v1` 索引，不等待迁移完成才启动 HTTP 服务。索引覆盖所有保留的原始用量事件，不改变原始事件键、小时聚合、价格公式或现有报表/额度结果。新事件（含恢复日志重放）与索引、两个小时聚合在同一事务中更新；保留期清理同步删除索引，长期小时聚合仍保留。

迁移逐批保存游标，中断后继续已提交的批次。完成前分批核对原始记录与索引的双向对应，再按客户端/小时分段核对修订号、计数和校验值。每段在同一个读事务内核对，不再要求整个扫描期间全局修订号不变；正常持续写入不会迫使全量校验重来。段修订冲突最多尝试三次。核对成功才写完成标记；该标记中的记录数是各段核验时的合计，不表示同一时刻的全库快照。后台迁移失败会记录错误，并以 1 秒起步的指数退避重试，最长间隔 1 分钟，不必等下次启动。关闭进程时取消并等待迁移退出，再关闭数据库。

保留期清理遇到无法解码的原始事件时，将原键、原字节、隔离时间与诊断原因移入 `usage_events_quarantine_v1`，再从原始事件 bucket 移除；其他过期记录继续清理，永久聚合不变。隔离与移除在同一事务中完成，并记录不含事件正文的 WARN。无法确认归属时采用全局保守失败：记录未解决隔离事实、使索引完成标记失效，所有索引查询均返回明确错误，迁移不能重新发布完成标记。隔离证据不会自动删除；需核实并处理损坏历史，不能将其当作零用量。隔离原键的恢复日志重放也会返回错误并留待处理，避免重复累计永久聚合。

存储接口 `ReadClientEvents` 按客户端、UTC 小时读取 `[from, to)` 内的保留事件。每个事件读取事务有批次上限，并使用事务内维护的小时修订号复核，小时发生变化最多尝试三次；索引不完整或无法稳定时返回明确错误，不返回部分结果。不同小时可以来自不同快照。迁移的单段校验读取该客户端整个小时，事务长度取决于该段记录量。该接口不是已清理历史的完整账目证明，计价调用仍须与永久小时聚合核对原始历史。

隔离证据的人工核对顺序：先保存含数据库、恢复日志及迁移快照的一致性备份，在离线副本上核对隔离的原键/原字节、两个永久聚合和 journal 中同一事件的状态；确认归属、原始事件恢复方式以及是否已累计费用，避免把已聚合事件再次 `Record`。**当前没有受支持的管理员解除接口或通用离线修复工具**，因此本版本不提供“删隔离记录即可解除”的操作。解除需要专门受审查的维护程序，在同一写事务中修复原始记录、索引和聚合，逐条处理隔离证据；全部核对完后才能清除 `usage_events_quarantine_count_v1` 未解决标记，失效旧完成标记及迁移游标，并重新执行完整迁移验证。仅清除完成标记、仅删隔离 bucket 或仅重放 journal 都不能证明账目一致。尚未完成核对时保留证据与标记，使用下述回退路径，不将隔离事件当作零消费。

### 精确历史费用与额度校准

`ReadCostUnits` 将小时内价格稳定的 target/model 保持为小时单元；价格模板覆盖、分钟时段或时区偏移在 UTC 小时内切价，或存在请求条件价时，使用事件的 target、model、timestamp 和计价上下文选择 `pricing.Resolver` 价率，费用公式仍为 `usage.CostRates.CostFor`。目标模板、全局模板、目录价格和 provider 隔离规则不变。存储层使用调用方传入的 resolver；管理报表与额度各自只折算一次模型组系数。

单客户端额度校准只读取日/周/月窗口并集一次（ISO 周可早于月初），再将每个已计价单元分配到三个窗口。分钟模板使用客户端索引读取，不解码其他客户端事件；每个小时的最后一个读事务同时复核小时修订状态与永久聚合的请求数。索引未完成、有未解决隔离或索引不一致时，记录诊断并丢弃全部局部结果，回退到原有全量原始事件扫描路径。全量路径仍跳过无法解码的事件，并通过双向 raw/rollup 请求计数核对使受影响客户端明确失败。需要精确计价的原始事件已被保留期清理时返回错误，不能猜测小时价或成功返回偏低金额；价格稳定的历史仍可使用永久聚合。

多客户端报表在一个读事务中扫描原始历史一次，将同一批费用结果复用于总计、时间桶、客户端、模型、模型组及 target/model 明细。Summary 的精确 last_hour 与其他窗口共用这一扫描；全为稳定价格时仅扫描 last_hour 原始事件。报表保留既有小时窗口归属和各入口的请求计数口径（全部请求或成功请求），只将选价时刻改为实际事件时间。冷客户端的准入与 Status 校准按客户端合并并发；首次失败同样进入 10 秒退避，期间不重复 Drain/Evaluate，并保留原计数、拒绝准入，直到可靠校准成功。

索引、小时修订状态、隔离记录与迁移标记都在同一个 bbolt 文件中，随数据库的一致性备份保存。恢复已有标记和游标的备份后，迁移跳过已完成构建或继续未完成批次；旧备份没有索引时从原始记录构建。用量恢复日志是独立文件，仍须纳入同一备份集；恢复后通过 `Record` 的幂等路径重放。不得用旧备份覆盖新实例已产生的数据。

## 环境要求

- Go 版本以 [`go.mod`](go.mod) 为准（当前声明 Go 1.27.2）。
- Docker/Compose 仅在使用容器部署时需要。
- 上游 API 凭据和客户端访问密钥应通过受保护的本地配置或环境注入；不要提交到 Git。

## 本地构建和运行

```sh
make build
./bin/llms-proxy -config config/config.json
```

`make build` 会尝试从 models.dev 更新内置模型目录；网络不可用时保留已有目录数据。程序默认读取 `config/config.json`，也可用 `-config` 指定其他文件。若配置中未设置监听地址，默认监听 `0.0.0.0:8000`；`SERVER_BIND` 环境变量可覆盖监听地址。

启动后可检查健康状态：

```sh
curl -fsS http://127.0.0.1:8000/healthz
```

客户端通过 `/v1/*` 调用代理。模型映射中的别名匹配规则与发给上游的模型名不同：上游模型名按配置原样发送，包括大小写。API 代理以透传为主；客户端应使用上游支持的协议、路径和请求格式。

### 模型前缀与共享价格

目标编辑页的「模型前缀」对应 `model_prefix`。出站完整模型名为前缀与每行上游模型值直接拼接，例如 `Vendor/` + `fable5` → `Vendor/fable5`；保留大小写，不自动添加分隔符，留空保持原有行为。客户端仍使用原别名，允许范围与计费模型身份不变。HTTP JSON、multipart、模型位于路径中的请求、Responses WS 和模型检测使用同一出站规则；切换目标时从原请求重新组装。上游响应中的模型名仍原样透传。

模型计费支持同时多选 Target 与模型，例如两条渠道的 `fable5`、`fable5-1` 共用一份基础价格、缓存价格和分时段规则。保存时校验所选 Target × 模型的全部组合，每个具体 Target 必须已配置该模型；默认全局兜底独立选择。相交组合的重复兜底价或重叠生效时间会整单拒绝。费用仍按实际目标、模型和事件时间分别核算，系数与额度规则不变。

管理 API 的 `/admin/data/pricing-templates` 新增 `targets`、`models` 数组，一条共享规则只有一个 ID。旧 `target`、`model` 字段和既有数据继续兼容；返回的旧字段表示首项，完整范围以数组为准。编辑规则时绑定集合固定，修改价格会作用于整条规则；更换绑定需复制并安排生效时间，原规则保留。历史费用继续按原事件时间选价；直接编辑旧生效窗口内的价格仍会影响该窗口的重新计算。旧单模型接口不能覆盖或删除多绑定规则。

部署池的 `name` 仍是小写内部标识；新增 `display_name` 保留用户输入的大小写。管理页可修正显示大小写，成员 Target 与客户端授权引用保持不变。旧数据未保存原大小写，显示时回退到 `name`，可在编辑页重新填写；仅大小写不同的名称不能创建为两个池。

### 日期例外与请求条件价

模型计费的生效窗口仍按事件时间选择，日期例外和条件价是模板的可选字段；未配置时保持原有价格行为，不自动给现有目标加规则。所有费用继续通过 `usage.CostRates.CostFor` 计算，模型组系数只折算一次。

- `date_exceptions` 用于分时段模板：每项 `start_date` / `end_date` 为模板时区内的日历日期，区间左闭右开。例如覆盖 10 月 1–7 日应填写 `2026-10-01` 至 `2026-10-08`。先选模板生效窗口，再按“日期例外 → 星期时段 → 基础价”取一组价率；日期例外不能重叠。
- `conditions` 可分别配置在基础价、每个星期时段及每个日期例外中。先选时间范围，再在该范围匹配条件；三个范围的条件互不继承。输入下限 `min_input_tokens` 包含，下一个上限 `max_input_tokens` 不包含；`service_tier` 与 `speed` 为独立维度。条件需有唯一 ID，同一范围内的条件不能交叠，留空维度代表任意值。命中时替换整组 `rates`，不将多条规则或倍率相乘；可选缓存/图像价仍保留缺值回退与显式 0 的差别。
- “超过 272,000 输入”对应下限 `272001`，高档适用整请求。OpenAI Chat/Responses 使用上游原始总输入（已含缓存）；Claude 使用普通输入 + 缓存读 + 缓存写，TTL、图像和思考等子项不再叠加。输入缺失、冲突或溢出不能当成已知 0。
- 服务档位以响应实际值为准。OpenAI `default` 规范为 `standard`、适用协议中的 `priority` 规范为 `fast`；Claude `usage.speed` 与 `service_tier` 分开记录。请求字段只保留证据，不决定实际价。SSE 后续省略不抹去已观察字段，明确新值更新旧值；WS 同样按每轮响应记录。可选值由 `/admin/data/schema` 从领域声明下发。
- 只对配置了相应条件维度的范围要求计价上下文。缺少实际档位、非法总输入或未配置的非标准组合，保留该时间范围的基础估算，并标记 `cost_incomplete` 与诊断；不拒绝转发、不丢记账。没有条件的普通目标保持原行为。已知标准档且未命中任何条件时使用基础价。

用量事件保存 `pricing_context`、所选 `pricing_rule_id` / `pricing_rule_version`。上下文包含版本、总输入存在性/有效性、请求和响应的原始档位及观察状态；未出现、null、空字符串、未知值与有效值分开。模板 `version` 是维护者填写的非负版本号，旧模板默认 0；ID/版本是选价证据，不是不可修改的历史账单快照。修改旧模板或删除旧价格仍会影响重新计算；需要保留历史口径时应新增未来生效的模板并保留旧记录，不能仅修改版本号就宣称历史已固定。

条件价按逐事件计算，不能先合并同一小时的长短请求或不同档位再选价。报表与额度增量、恢复和校准共用同一选价入口。条件价所需原始事件已被保留期裁剪时，历史查询返回 `ErrUsageHistoryIncomplete`；永久小时聚合不足以还原请求档位，不能将估算当成完整账单。没有条件且整小时内价格稳定的范围仍可使用永久聚合。日期边界遇到非整小时偏移时区也走精确事件路径。

日期维护由配置显式控制，没有内置厂商识别或自动年度日历。维护 DeepSeek 规则时，应核对原厂当期规则与官方节假日通知，在现有规则到期前补齐下一年的日期；缺少某日例外会沿用星期规则，不会自动识别为假日。2026-10-14 核对的[DeepSeek 价格规则](https://api-docs.deepseek.com/zh-cn/quick_start/pricing/)为北京时间周一至五排除法定节假日，09:00–12:00、14:00–18:00峰价；周末及假日全天谷价，所以调休补班的周末不提升为峰价。[2026 年国务院通知](https://big5.www.gov.cn/gate/big5/www.gov.cn/zhengce/zhengceku/202511/content_7047091.htm)可用于配置当年日期，但不证明原厂历史价格条款的首次生效日。

长输入与服务档位示例的依据核对日为 2026-10-14：[GPT-6.1 Sol](https://developers.openai.com/api/docs/models/gpt-6.1-sol)、[OpenAI Fast](https://developers.openai.com/api/docs/guides/fast-mode)、[Claude 长上下文](https://platform.claude.com/docs/en/about-claude/pricing#long-context-pricing)、[Claude Fast](https://platform.claude.com/docs/en/build-with-claude/fast-mode)。这些只支持有据型号及组合；不能按模型系列盲推倍率，也不扩展可用协议或自动启用暂停目标。fakellm 回归代表协议合同验证，不能替代国外真实 API 验收。上线本能力不会修改生产价表、补造旧事件档位或自动重算历史费用。

### 首页实时活动

首页展示全站和逐客户端的当前在途请求、当前请求客户端数、Responses WebSocket 已连接会话，以及最近五分钟已记录的输入、输出 Token。HTTP/SSE 从已鉴权且路径有效的请求进入到处理结束计数，WebSocket 按有效 `response.create` 轮次计数；空闲但未关闭的 WebSocket 只计入会话数，不计入当前请求客户端数。它们不表示 HTTP keep-alive 的 TCP 连接数。

只读接口 `/admin/api/activity` 经现有管理会话访问。用量按事件时间精确读取 `[now - 5m, now)`，全站、客户端和十秒时间桶来自同一个读取事务，不设事件数量截断。输入合计包含非缓存输入、缓存读、缓存写及独立工具输入；输出直接使用已有输出总量，推理、图片和缓存期限等子集不重复相加。进行中尚未上报的用量不估算，不完整用量保留已知数字并单独提示。近期有活动或用量的已删除客户端仍保留在明细中。

并发曲线记录每十秒的峰值，服务器在请求开始、结束和读取快照时推进历史，无需打开浏览器才能采集。全站峰值独立记录，不能累加各客户端各自的峰值。进程启动前、历史断档和最老裁剪桶的未知峰值以 `null` 返回；当前未结束桶标为部分采样。进程重启或墙钟回拨会重新开始历史采样，当前在途计数按实际请求生命周期维护。当前在途的全站与客户端值来自同次快照，WebSocket 会话、配置和用量查询存在正常采样时差，不表示跨来源的全局线性快照。

页面每五秒刷新实时数据，离开首页或隐藏页面时暂停，返回立即刷新；活动和历史读取各设十二秒超时，失败保留最后一次数据并显示更新时间与失败提示。选择客户端仅切换曲线范围，全站汇总继续显示；搜索、排序和键盘读数可用。首页移除了近期审计日志，独立审计功能仍保留。原有七十二小时请求完成统计与历史费用位于辅助区，沿用下述口径。

### 请求完成统计

管理总览的请求总量与成功率使用独立的 `request_stats_72h`，不是用量事件数或服务可用率。每次进入代理 Service 的 HTTP 请求、每个有效且通过鉴权复核的 WebSocket `response.create` 轮次在结束时计数一次；换 key、切 target 不另计。成功需最终 2xx、完整回写且没有明确生命周期失败；支持的 SSE 还需协议终态。缺少 usage 本身不表示请求失败。失败和取消均进入总量，成功率为成功数 / 总量；没有样本时成功率为 `null`，页面显示 `--`。

样本不含 Service 前的鉴权/IP 拒绝、健康/管理/ping、WebSocket 升级和会话控制、无效或鉴权复核拒绝消息。统计区间为 `[from, to)`，`to` 是当前 UTC 分钟起点，`from = to - 72h`；当前未结束分钟不计入。`since` 是新统计 bucket 首次初始化时间，不从历史用量推算。分钟计数独立保留 7 天，启动及每 12 小时限量清理，不受用量保留开关影响。

OpenAI 兼容请求的 JSON 或 SSE 数据帧中，顶层 `error` 为含有效 `message`、`type` 或 `code` 的非空对象时，即使 HTTP 为 200 也记为业务失败；后续 usage 或终态不会清除该失败。已报告用量保留，HTTP 状态和响应体原样透传，不因此新增重试、切 target 或封 key。`error:null`、空对象和生成内容中的嵌套 error 不触发此规则。通用规则不读 OpenRouter `error.metadata`、不识别 MiniMax `base_resp`；只有显式选择对应 Patch 时由厂商规则读取。

目标的 `usage_adapter` 显式选项为 `standard`、`qwen`、`glm`、`deepseek`、`kimi`、`openrouter`、`minimax`，不按 host 或模型名推断。OpenRouter 与 MiniMax 只增加错误 Patch，用量仍按实际协议使用原厂解析器。OpenRouter 通过结构化预算来源区分临时限流、额度和余额，普通 403 文本不触发；MiniMax 仅在非流式 Chat Completions HTTP 200 JSON 中读取顶层 `base_resp` 数字业务码，已知认证/额度/限流码直接换 key，未知非零码透传并记 usage failed。MiniMax 全 key 耗尽时允许 failover，否则返回映射错误而非原始 200 体。2056 在代理侧按 `quota_exceeded` 处理：未配置有效未来重置时间时记为 `quota_exceeded_api`，需人工唤醒，或配置有效重置时间按既有 keypool 规则恢复（与 Kimi 一致）；不会自行推算 M Plan 窗口。流式、非 2xx、Anthropic Messages 和 Responses 均走原厂规则，不以 `base_resp` 触发 Patch。依据 [MiniMax Chat OpenAPI](https://platform.minimaxi.com/docs/api-reference/text-chat-openai) 与 [官方错误码页](https://platform.minimaxi.com/docs/api-reference/errorcode)，核对日期 2026-10-13；文档核对不等于真实 API 验收。完整判据与不做项见 [代理层约束](internal/proxy/AGENTS.md)。

计费只依据上游官方返回的 token 用量字段，不估算、不补算，也不重放请求。客户端中途断开或取消时，代理照常立即取消上游，不重试、不重放；若此时尚未取得该协议的官方最终用量字段，且上游请求可能已发出，则记一条 0 token、状态为不完整的用量事件，诊断为 `client_disconnected_no_usage`（管理端口径说明中显示为“连接断开，未获取到计费字段”），已观察到的部分 token 也不计入；同时写一条 Warn 日志并累计计数 `usage_client_gone_no_usage`。可证明尚未向上游发送任何字节的取消不写用量事件。该规则与协议和传输方式（HTTP、SSE、WebSocket）无关；因此统计会少于厂商官方账单中这部分中断请求的用量，属于预期。上游原因（出错、断流、超时、解码失败、缺少终态或官方最终用量）造成的中断同样记 0 token，诊断为 `upstream_interrupted_no_usage`，计数 `usage_upstream_interrupted_no_usage`；两类计数均可在 `/admin/metrics` 与总览中查看。一个客户端请求至多对应一条用量事件，期间会重试或切换目标的失败尝试只写日志与计数。

统计不保存身份、请求正文或密钥，也不参与计费、额度、重试或健康结算。写入失败会记录日志，并在总览通过本进程 `record_failures` 提示样本可能不完整；没有恢复日志，写入失败和进程崩溃期间不保证计数完整。读取失败显示不可用，不回退到旧数据。兼容字段 `success_rate`、`requests_72h`、`success_72h` 仍以用量事件为样本，并通过 `legacy_sample_basis: "usage_events"` 标明口径。

### 响应压缩与用量

HTTP 响应的用量观察支持未压缩、`gzip`、标准 `deflate`（zlib）、`zstd` 与 `br`（Brotli），用于解析 JSON/SSE 用量。客户端显式指定 `Accept-Encoding` 时，响应字节及编码头保持透传；未指定且没有 Range、并非 HEAD 请求时，保留代理原有的自动 gzip 协商与单 gzip 透明解压行为。所有编码头字段一并检查，多值、未知或叠加编码不能因首个头可识别而被当成完整用量。压缩处理适用于所有目标，不由厂商 Patch 开关控制。`br` 与 `zstd` 的观察解码共用全局内存预算（默认 512 MiB，可用环境变量 `LLMS_OBSERVATION_DECODE_BUDGET_MB` 调整，单位 MiB），预算不足时本次用量按观察失败记录、转发字节不变，管理端 `/admin/metrics` 的 `observation_budget` 可查看占用与耗尽次数。`zstd` 按 [RFC 9659](https://www.rfc-editor.org/rfc/rfc9659.html#section-3) 限制解码窗口为 8 MiB，这不是响应总长度上限；首帧为单段帧且声明大小在 8–64 MiB 之间时按声明大小放宽，超过 64 MiB 在帧头阶段拒绝。需要观察用量的请求转发给上游时会从 `Accept-Encoding` 中去掉 `br`（保留其余编码和 q 值；含 `*` 时追加 `br;q=0`；去掉后为空时设为 `identity`），避免客户端借 br 解码预算规避计费；上游仍返回 br 时照常观察。`br` 只用于用量与 trace 观察，非 2xx 错误分类、厂商探测和原生 WebSocket 回退的编码范围不变。未知编码、多层编码及损坏响应不推算 token，用量保留为不完整。

fakellm 支持生成 `identity`、`gzip`、`deflate`、`zstd` 的 JSON/SSE 响应，以及编码头覆盖、截断和损坏故障。SSE 每帧同步刷新压缩流；控制面和 WebSocket 升级不套用 HTTP 正文压缩。对应本机 e2e（`make e2e`）检查编码头、客户端解码、持久化用量和计费，详见 [测试说明](scripts/testing/README.md)。

Trace 使用同一解码规则，并保留既有正文长度上限；截断的 trace 不代表用量观察也被截断。旧版本将未支持的压缩字节作为 UTF-8 文本保存时可能已损坏原始数据，不能据此恢复精确 token 或补算费用。新增支持只影响之后的请求，不重写历史用量。

### 请求成员规范化

请求中影响路由、权限、计费或流式行为的顶层字段（如 `model`、`stream`、`service_tier`、`input`、`messages`、`tools` 等）若重复出现或以大小写变体出现，代理在入口只保留最后一个拼写完全一致的值，删除其余副本与变体后再做权限判断、计费与转发；只有变体时全部删除。JSON、multipart 表单、`application/x-www-form-urlencoded` 表单与 WebSocket 请求一致处理，上游无论按何种方式解析都只会读到代理授权并计费的那个值。没有此类冲突的请求原样转发。`custom_body` 不允许配置这些字段的大小写变体。

嵌套的计价成员按同一规则处理：`stream_options` 中的 `include_usage`、`include_obfuscation`，`prompt_cache_options` 中的 `ttl`，以及顶层、`system[*]`、`messages[*]`、`messages[*].content[*]`、`tools[*]` 中的 `cache_control`（含其内部的 `type`、`ttl`）和 `messages[*]` 的 `content`。更深层的位置（如 `tool_result` 内嵌内容）不在当前范围。

请求体类型按 `Content-Type` 的精确媒体类型判定（JSON、urlencoded、multipart 三类），取模型、规范化与表单转换共用同一判定。声明为 urlencoded、正文（去掉前导空白后）以 `{` 开头的请求（如未带 `-H` 的 `curl -d`）：正文是合法 JSON（不带 BOM）时按 JSON 处理，转发给上游时 `Content-Type` 改为 `application/json`（保留 charset）；非法或近似 JSON（如含 NaN、尾逗号、BOM）返回 `400 ambiguous_request_body`。声明为 multipart 但存在解析歧义的请求返回 `400 ambiguous_request_body` 且不转发，包括：正文是 JSON、`boundary` 为空或含首尾空白、首个分隔线前有非空白内容、`boundary` 使用转义、`boundary*` 扩展参数或不符合 RFC 2046 字符集与长度、正文中出现不合规的分隔线（以 `--boundary` 开头但不是合法分隔或结束行，或位于结束行之后）、CRLF 表单的分段头部出现仅以 LF 结尾的行、受保护字段（如 `model`）的分段声明了非 UTF-8 的 `charset` 或带 `Content-Transfer-Encoding`、`Content-Type` 参数无法解析、某个分段的 `Content-Disposition` 不是恰好一个、无法解析、不是 `form-data`，或使用了 `name*` 扩展参数。

正文非空且带多个 `Content-Type` 头的请求同样返回 `400 ambiguous_request_body`。代理不解压请求体：正文非空且 `Content-Encoding` 含 `identity` 以外的编码时返回 `415 unsupported_content_encoding`（响应附 `Accept-Encoding: identity`），不转发。

### 连接与落库性能

上游连接共用一个 Transport：启用 TLS 会话恢复与 HTTP/2（含 30 秒空闲 PING 探测死连接），拨号超时 8 秒；拨号阶段超时按“尚未发送”切换下一目标，父请求取消或超时仍不重试。请求体只做字节级的顶层改写（模型名、`include_usage`），其余内容按客户端原文转发。用量先写入恢复日志（fsync）再由后台批量写入数据库，额度计数在响应结束前即时更新；请求完成统计异步写入。管理端报表可能比请求结束晚几毫秒出现。关停时最多等待 30 秒写完排队的用量，超时未写入的部分留在恢复日志，下次启动自动补齐且不重复计费。

### Responses WebSocket

OpenAI 目标可在管理页开启「Responses 优先使用 WebSocket」，对应 target 字段 `prefer_websocket`，默认关闭。该开关只影响同步 `POST /v1/responses` 的上游传输；Chat Completions 和后台生成不走此适配。HTTP 客户端仍可请求 SSE 或最终 JSON。

双协议目标可在管理页设置 `openai_prefix`、`anthropic_prefix` 和 `supports_responses`。新建规范 `dual_protocol` 目标时，两个前缀默认为空，Responses 默认关闭；请按上游文档填写，不会按计费类型、域名或模型自动推断。编辑时保留已保存的值，清空前缀或取消勾选后保存即可清除设置。切换为其他类型只隐藏这三项，不会清空值；旧 `bailian` / `bailian_api` 配置的兼容归一化保持不变。模型行的「测试」发送最小 HTTP 请求检查上游响应，成功仅表示 HTTP 检测通过，不验证 WebSocket 通道。

支持 OpenAI Chat 请求的目标可显式开启「请求流式用量（仅补缺失值）」，对应目标字段 `request_stream_usage_if_missing`，默认关闭，旧目标不自动开启。仅当实际请求路径为 `chat/completions` 且 `stream` 为布尔 `true` 时，补上缺失的 `stream_options.include_usage=true`。已有 `true` / `false` 或异常类型（含 `null`）均保留；其他嵌套成员不变。Responses、Anthropic、Gemini 原生、图片与非流式请求不受影响。执行顺序是净化或 multipart 转换 → `custom_body` 顶层替换 → 模型改写 → 补缺失值；`custom_body` 显式配置优先，自动策略不修正其中的现有值。切换目标时从客户端原始请求重新装配，不继承上一目标的补值。上游仍不返回用量时记录 `incomplete` / `missing_usage`，不估算 token。管理 API 更新省略此字段会保留原值，显式 `false` 关闭；切换目标类型只隐藏控件，不清空已保存的开关值。

官方 OpenAI SDK 也可用原生 WS 连接代理的 `/v1/responses`，并使用代理客户端密钥鉴权。SDK 的 `baseURL` 指向代理的 `/v1` 地址。原生入口保留既有 Codex WS 路径，不受上述 HTTP 传输开关影响，支持 `openai` 和 `dual_protocol` 目标的顺序 `response.create`。每个生成仍检查客户端权限、目标准入和额度，并记录用量；不支持命名 `stream_id`、并行生成或 Realtime 事件。

连接按客户端隔离，原生下游连接另有独立会话边界。代理不自动添加 `previous_response_id`，也不修改 `store`。原生连接的身份变化返回 `409`，要求客户端携带完整历史重建会话；HTTP 客户端的上游 WS 续接只允许该客户端仍存活的连接中缓存的最新 response，不可用时返回 `previous_response_not_found`。关闭开关后的 HTTP 上游续接仍取决于上游是否保存该 response。

HTTP 请求的 WS 优先及原生 WS 仅在握手返回 `400/404/405/426/501`、尚未尝试发送生成事件且上下文有效时允许回退上游 HTTP；原生客户端仍收到 WS 事件，HTTP 业务错误状态和错误码保留。认证、权限、限流、取消和请求超时不会触发这种回退；HTTP 请求的上游 WS 握手若被 429/额度/凭据类错误拒绝（尚未发送生成事件），按普通 HTTP 响应的密钥池规则在本次请求内重试或换 key，原生 WS 的生成前握手拒绝也可按相同规则在本轮换 key；发送结果不确定、流中断或终态失败均不重放生成，生成后的终态 key 反馈只影响下一轮。缺失用量标记为不完整，不按零费成功处理。握手以 `404/405/426/501` 拒绝后，同一握手身份（URL 与转发头，含上游凭据）在 10 分钟内新建连接时直接走 HTTP，已有连接及其续接不受影响；`400` 可能来自单个客户端的请求头，不做记忆；配置更新会清除该记录。HTTP 请求的上游 WS 池在空闲 5 分钟或连接年龄达到 55 分钟后退役；空闲连接定期 ping，无响应即退役；池容量已满时本次请求改走 HTTP（尚未发送生成事件）。请求结束后会在后台为同一客户端预拨一条未使用过的备用连接（每客户端至多 1 条），供下一个新请求直接使用，减少握手；后台拨号失败不影响 key、熔断与回退记录。原生 WebSocket 会话在两轮之间上游空闲 30 秒时发送 ping，避免续接时连接已被上游关闭。请求与事件 JSON 按原始字节转发，只增删必要的顶层成员。不带 `previous_response_id` 的新请求优先使用新连接，避免覆盖同一客户端其他会话的续接点。配置更新使旧池连接退役。

原生下游连接两轮之间空闲 15 分钟后以 1000 关闭，服务端每 30 秒发送 ping；同一客户端凭据最多 64 个并发原生会话，超出返回 `429`。服务关闭时先在关闭宽限期内让进行中的生成完成并落账，再以 1001 关闭会话；超出宽限期的生成被取消并记为不完整用量。原生客户端需处理断线并显式决定如何重连与续接。

## Codex Ultrafast WebSocket 示例

Codex 可以经 LLMs Proxy → CLIProxyAPI → Codex OAuth 上游使用 Responses WebSocket。此配置要求 CLIProxyAPI 已配置可用的 Codex OAuth 账户，且该账户的 auth metadata/attributes 中 `websockets=true`。三层凭据用途不同：Codex 的 `env_key` 使用分配给 Codex 客户端的 LLMs Proxy key；LLMs Proxy 目标的 `APIKey` 使用 CLIProxyAPI 自身的 API key；CLIProxyAPI 持有 Codex OAuth 登录凭据和对应账户资格。

在 LLMs Proxy 的目标配置中，选择 CLIProxyAPI 对应目标，设置 endpoint type 为 `openai` 或 `dual_protocol`（目前 Responses WebSocket 仅支持这两类，不选 `azure_openai`），并填写 CLIProxyAPI 的 endpoint 和 API key。`CustomBody` 添加 key `service_tier`，value 设为字符串 `ultrafast`（JSON 中写作 `"ultrafast"`）。网页模型行左侧是上游名称，填 `gpt-6.1-sol`；右侧是客户端别名，填 `codex-sol-ultrafast`。这样客户端通用别名映射到上游模型名，上游模型名保留原始拼写。若当前 Codex 版本没有该自定义别名的模型元数据，还需提供相匹配的 Codex model catalog 条目。网页中的目标和映射修改需要保存后才会生效。

Codex 的 `~/.codex/config.toml` 示例：

```toml
model = "codex-sol-ultrafast"
model_provider = "llms-proxy"
service_tier = "ultrafast"

[model_providers.llms-proxy]
name = "LLMs Proxy"
base_url = "https://proxy.example.com/v1"
wire_api = "responses"
supports_websockets = true
env_key = "LLMS_PROXY_API_KEY"
requires_openai_auth = false
```

启动 Codex 的进程需要从安全凭据入口获得 `LLMS_PROXY_API_KEY` 环境变量。`env_key` 是 Codex 客户端连接 LLMs Proxy 的凭据；不要把 key 复制进 TOML、命令参数、日志或 Git 提交。不要把 `requires_openai_auth` 设为 `true`：Codex 会在该设置下忽略 `env_key`。`supports_websockets` 声明 provider 支持 Responses WebSocket；`base_url` 配合 `wire_api = "responses"` 让 Codex 使用 `/v1/responses`，WebSocket 地址为 `wss://proxy.example.com/v1/responses`。

此集成覆盖普通 Codex Responses WebSocket 会话：客户端发送扁平的 `response.create` 事件，可用 `generate = false` 预热，并通过 `previous_response_id` 顺序续轮。每个新事件都会重新校验客户端 key、ACL、IP 限制和目标准入，并沿用普通请求的用量记录。它不表示支持并行 `stream_id`、中途 steering 或 `response.append`。若已有 `previous_response_id` 的连接上目标、URL 或认证身份发生变化，服务会返回 `409`；客户端应携带完整历史重新连接，不会由代理静默切换上游并重放。

配置只能表明请求偏好，不能证明上游实际使用了 Ultrafast。验收时分别核对发出的请求包含 `service_tier: "ultrafast"`，以及上游响应 `response.service_tier` 回读到的实际档位；还要确认 CLIProxyAPI 选中的 Codex OAuth 账户符合计划资格。OpenAI 当前说明 Ultrafast 支持 GPT-6 Astra 和 GPT-6.1 Sol；Pro $500 的 included usage 按 Standard 的 8 倍计，超出后使用可用 credits，购入 credits 按 Standard 的 6 倍计。请参考 [Codex Speed 指南](https://learn.chatgpt.com/docs/agent-configuration/speed)、[Codex 配置参考](https://learn.chatgpt.com/docs/config-file/config-reference)、[Codex Gateway 兼容要求](https://learn.chatgpt.com/docs/enterprise/gateway-compatibility)、[Responses WebSocket 指南](https://developers.openai.com/api/docs/guides/websocket-mode)、[CLIProxyAPI Codex 客户端配置](https://github.com/router-for-me/CLIProxyAPIDocs/blob/main/docs/en/agent-client/codex.md)、[CLIProxyAPI Codex WebSocket 配置样例](https://github.com/router-for-me/CLIProxyAPI/blob/main/config.example.yaml#L583-L626) 和 [CLIProxyAPI WebSocket auth 选择逻辑](https://github.com/router-for-me/CLIProxyAPI/blob/main/internal/runtime/executor/codex_websockets_executor.go#L123-L140)。仓库中的配置示例和 CLIProxyAPI 上游源码不代表你部署的版本已支持该能力；部署后仍须回读实际版本，并分别记录请求 tier 与响应实际 tier。

## 测试

```sh
make test
go vet ./...
go vet -tags=e2e ./test/e2e/
go build ./...
go test -race -count=1 ./internal/proxy/... ./internal/nosql/... ./internal/usage/... ./internal/quota/... ./internal/admin/... ./cmd/proxy/...
make integration
node scripts/testing/admin_ui_test.cjs
make e2e
python3 -m unittest scripts.test_verify_azure_v1_models scripts.test_workflow_image_tag
```

`make test` 会先执行 `make check-docs`，再运行 `go test ./...`。文档检查要求 `AGENTS.md` 存在。此处记录项目门禁，不表示本次运行过测试。

`make e2e` 编译真实代理二进制，对接进程内确定性虚假上游 `internal/fakellm`，不调用任何真实模型。管理前端测试工具见 [测试说明](scripts/testing/README.md)。

上游 API 凭据和客户端访问密钥只保存在受保护的本地配置或环境变量中；不提交到 Git，不写入命令行参数，也不打印到日志。未执行的检查必须明确标记为未验证。

## 贡献流程

- **规格记在 Issue。** 每个可独立验证的目标对应一个 GitHub Issue（由 SpecGit 管理），写明 Why / Scope / Approach / Acceptance；进度、阻塞与证据链接也记在其中。Pull Request 用 `Closes #N` 或 `Refs #N` 关联。
- **功能分支合入 `dev`。** 在功能分支上开发，以 Pull Request 合入 `dev`；不直接推送 `dev` 或 `main`。按 [PR 模板](.github/pull_request_template.md) 填写说明与关联 Issue、TDD 证据、已跑门禁、未验证范围，以及是否影响计费、协议或兼容性。
- **`main` 只做 fast-forward。** `dev` 验收通过后，`main` 以 fast-forward 前进到已验收的 `dev` 提交并打 tag；不在 `main` 上单独提交或合并。
- **行为变更遵循 TDD。** 先补一个因正确原因失败的测试，再修改实现，并展示同一测试通过。保持行为不变的重构和仅文档改动不需要人为制造失败，但要写明已跑的检查。
- **先在本地跑门禁**，使用 `go.mod` 声明的 Go 版本和与 CI 相同的命令。

CI（[`.github/workflows/ci.yml`](.github/workflows/ci.yml)）在推送到 `dev`、`main` 以及目标为二者的 Pull Request 上运行，只用 GitHub 托管 runner，不使用任何 secret，同一 ref 上的新运行会取消旧运行。以下 job 名称是 `dev` 与 `main` 的必需检查，保持稳定：

| Job | 命令 |
|---|---|
| `test` | `make check-docs`、`go vet ./...`、`go vet -tags=e2e ./test/e2e/`、`go build ./...`、`go test ./...` |
| `race` | 对 `internal/proxy`、`internal/nosql`、`internal/usage`、`internal/quota`、`internal/admin` 与 `cmd/proxy` 执行 `go test -race -count=1` |
| `e2e` | `make e2e` |
| `integration` | `make integration` |
| `admin-ui` | `node scripts/testing/admin_ui_test.cjs` |
| `scripts` | 受跟踪的 `scripts/**/test_*.py` 与 `scripts/test_*.py` Python 单测（`python3 -m unittest`） |

`.github/workflows/docker-build.yml` 是可选的手动镜像构建，不属于必需检查；发布默认只附源码包。

## 许可证

[MIT](LICENSE)
