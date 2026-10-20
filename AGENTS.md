# 项目 Agent 指引

本文件约束本仓库的二次开发与 Agent 工作。仓库实现以当前源码、用例和脚本为准。文中的价格口径、访问策略与验证要求是开发约束，不表示任何部署环境已按此配置或已验收。涉及运行中实例的状态、发布状态或模型 ACL（访问控制）时，须在授权范围内实际回读，并记录核验日期。

## 项目概况

LLMs Proxy 是 Go HTTP 代理服务，为客户端提供统一入口，并按模型映射和目标配置将请求转发到多个上游。存储使用 bbolt，不依赖外部数据库服务。

- Go module：`github.com/LeXwDeX/LLM-Proxy`；Go 版本见 `go.mod`。
- 服务入口：`cmd/proxy/main.go`；默认配置路径 `config/config.json`，配置未提供时程序可生成默认配置。
- 支持的规范 `endpoint_type` 以 [`internal/config/endpoint_type.go`](internal/config/endpoint_type.go) 为准：`azure_openai`、`openai`、`claude`、`gemini`、`openai_image`、`dual_protocol`。DeepSeek、百炼以及旧别名归一到对应规范类型；不要把旧别名当成新类型。
- 转发逻辑在 `internal/proxy/`；管理后台在 `internal/admin/`；计价解析在 `internal/pricing/`；用量与费用契约在 `internal/usage/`；额度准入在 `internal/quota/`；持久化在 `internal/nosql/`。
- Provider 能力、展示元数据及运行策略标识统一在 `internal/config/endpoint_type.go` 声明，registry 与路径过滤派生消费；原生协议与可接受请求格式分别声明。新增类型先复用已有策略，确需新策略时补工厂实现和对应回归。
- 管理前端通过 `/admin/data/schema` 读取公开业务枚举及默认值；领域验证与显示使用同一集合。Schema 未成功加载不得启用编辑，不在 HTML/JS 重复声明 Provider、计价、角色与额度周期选项。
- 启动时会执行已有迁移和用量恢复步骤，见 `cmd/proxy/main.go`。本地启动应使用独立配置和数据路径，不能指向生产数据库。
- 代理不依赖任何厂商 SDK（见 `go.mod`）。认证、路径改写、错误分类、SSE/WS 用量解析和计价都在本仓库实现，以便原样透传上游字节，并对多厂商差异保持可控。

## 架构方向：原厂规则 + Patch 规则

已确定的方向（2026-10-12 确立，2026-10-13 补充 OpenRouter、MiniMax）：

- **原厂规则**：OpenAI（Chat、Responses、Images 等）、Anthropic Messages、Gemini 是原厂协议。它们的认证、路径、用量解析和错误分类写在通用代码中，作为默认行为。
- **Patch 规则**：第三方基于原厂协议的扩展视为 Patch。当前 Patch 分类为 `qwen`、`glm`、`deepseek`、`kimi`、`openrouter`、`minimax`。Patch 只描述与原厂的差异，例如额外的用量字段、厂商错误码、HTTP 200 中的错误体。
- **两条独立轴**：`endpoint_type` 只决定认证、路径和协议能力；目标的 `usage_adapter` 决定用量与错误按原厂规则（`standard`）还是某个 Patch 处理。不要为厂商名新增 `endpoint_type`，也不要用 `endpoint_type` 触发厂商行为。
- **只按显式选择启用**：Patch 只由持久化目标显式选择的 `usage_adapter` 启用，对该目标的全部模型生效。禁止按 host、模型名、响应文本推断厂商。旧 `auto` 的 host 推断只用于导入迁移，不得扩展。
- **修改归属**：上游改格式时只改对应 Patch；只有出现新的通用字段（如缓存写）时才改原厂规则。未选择 Patch 的目标保持原厂行为，这是每个 Patch 必须保留的反例边界。
- **代码落点**：选项在 `internal/config/config.go` 与 `internal/config/schema.go` 声明，经 `/admin/data/schema` 下发；选择逻辑在 `internal/proxy/usage_adapter.go`（`resolveUsageAdapter` 选用量 Patch，`targetVendor` 选错误 Patch）；厂商实现放在 `internal/proxy/usage_patch_<vendor>.go` 与 `internal/proxy/provider_error_<vendor>.go`。Gemini 用量始终走原生解析。OpenRouter、MiniMax 只有错误 Patch，用量走原厂解析。具体判据见 [`internal/proxy/AGENTS.md`](internal/proxy/AGENTS.md)。
- **Patch 不是协议转换**：Patch 不改变客户端可用的 API 路径或请求格式，也不扩大重试范围。新增或修改 Patch 须在现有红线内说明是否封 key、换 key、冷却或 failover。
- **新增 Patch 的证据要求**：记录官方文档来源和核对日期；为每条判据补 fakellm preset、单元测试和 e2e。e2e 同时覆盖“选择该 Patch”的正向行为和“`standard`”反例。官方未证实的形态在代码和文档中标为合同假设。文档核对不等于真实 API 验收。

### fakellm：确定性虚假上游

- 实现位于 `internal/fakellm/`，独立入口为 `cmd/fakellm/`，镜像为 `Dockerfile.fakellm`。它在任意路径前缀下应答代理能转发的全部协议（OpenAI Chat/Responses/Images、Azure、Anthropic、Gemini 等），回复和 token 用量由请求字节确定性推导，计费断言可以精确比较。
- 用途：`make e2e` 在进程内启动 fakellm 并运行真实代理二进制；`make ui-sim` 用它作为管理页面模拟的上游；`cmd/fakellm -legacy-mock` 提供兼容旧 mock 合同的模式。它们都不调用真实模型。
- 行为通过请求内指令、按 key 规则或全局场景（`/_control`）控制。flavor 模拟厂商的线格式；preset 是按官方文档建模的厂商错误快照（如 `glm_1302`、`kimi_quota`、`openrouter_403_budget`、`minimax_1008`），包含用于反例的形态。
- fakellm 是 Patch 规则的测试基座：每条 Patch 判据都应有对应 preset。preset 只代表文档合同，不能替代真实厂商验收，也不能作为扩大测试授权的理由。
- MiniMax 仅实现官方有据的形态：错误 Patch 只读非流式 Chat Completions HTTP 200 JSON 的 `base_resp`；流式、非 2xx、Anthropic Messages 与 Responses 走原厂规则。fakellm 的 MiniMax 流式 chunk 无 `base_resp`，`include_usage` 默认 false，此时所有 chunk 均为 `usage: null`；显式请求用量时沿用 OpenAI 原厂末尾空 choices 用量块，位置为原厂形态推定，MiniMax 官方未示例。Anthropic 错误复用标准协议 preset。来源为 [Chat OpenAPI](https://platform.minimaxi.com/docs/api-reference/text-chat-openai)、[Anthropic 协议](https://platform.minimaxi.com/docs/api-reference/text-chat-anthropic) 与 [错误码页](https://platform.minimaxi.com/docs/api-reference/errorcode)，核对日期 2026-10-13；文档核对不等于真实 API 验收。

## 信息来源与工作区保护

1. 当前用户在本轮给出的明确要求优先于旧记录。代码和脚本只能证明当前实现，不能证明任何部署环境的状态。
2. 将“期望策略”“仓库当前实现”“运行实例回读结果”分开陈述。没有执行或读取到的状态，不写成已完成或已验收。
3. 开始编辑前检查 `git status` 和 diff。保留已有改动、未跟踪文件及用户删除；不使用 `git reset --hard`、`git clean` 或其他会丢失用户内容的命令。
4. 用户要求只改文档时，不改业务代码、配置或部署行为；用户明确说不跑测试时，不运行测试。
5. 当前受维护的说明是 [`README.md`](README.md)（英文主文档）及其中文版 [`README.zh-CN.md`](README.zh-CN.md)、本文件、[`internal/proxy/AGENTS.md`](internal/proxy/AGENTS.md)、[`internal/admin/AGENTS.md`](internal/admin/AGENTS.md) 及 [`scripts/testing/README.md`](scripts/testing/README.md)。不要添加指向不存在文件的链接；只有用户明确要求时才新建或重建指定文档。修改 README 时，英文版与中文版须在同一改动中同步，章节结构保持一致。
6. 发现文档与源码不一致时，核对相关用例并修正文档中的实现说明。已确定的策略仍是约束，不能因代码未落实就删除策略或扩大权限。

## 多 Agent 与代码证据

- 每项任务必须使用多 Agent。主 Agent 负责调度、撰写方案、集成和会审；编码实施优先委派编码 Agent；简单探索、文档和重复检查委派探索 Agent；重大方案决策交给能力最强的决策 Agent。主 Agent 沿用用户当前选择的模型。具体 Agent 类型和工具名称按所用工具的实际能力选择，本文件不绑定特定工具。
- 代码交付在问题全部处理完毕、进入测试环境验收之前，须派遣当前可用的最强模型做一次全面代码审核（本文件不固定模型名称，按当时可用模型选择），审核发现的问题修复后再进入验收和生产晋级。
- 主 Agent 为子 Agent 明确文件范围和职责。共享工作区时不得覆盖或撤销他人的改动。终审发现问题后，由实施 Agent 修复，再提交终审。
- 结构探索优先使用代码图谱或索引类工具（如可用）。每次会话开始或上下文压缩恢复后，先确认项目及索引状态与代次。
- 默认做中等深度核验：按符号查找定义，追踪调用关系，读取关键实现片段；复杂关系用多跳查询，整体结构用架构概览。处理相关结果的分页。
- 找到证据文件后，检查索引对全部证据路径的覆盖。否定或穷尽结论还须检查对应范围。覆盖无缺口记录不等于完整性证明；覆盖不全或已过期时，直接读取遗漏源码。
- 字面量、配置、脚本和非代码文档可用文本搜索与直接读取。索引工具不足或不可用时回退源码，并说明证据限制。
- 委派前，主 Agent 先完成相关结构查询及覆盖检查。交接项目、索引代次、证据层级、范围、符号、路径、调用关系、分页进度、覆盖缺口和待核实问题。子 Agent 没有索引工具时，只能使用已交接证据和直接源码，不能声称做过图谱核验。

## 二次开发要求

以下是实施约束。不得把这些要求写成已有功能或已通过的验证。

### 范围与兼容

- 先明确问题、改动范围、验收条件和直接消费者。使用隔离的功能分支，分支命名不绑定特定工具。不要在同一改动中顺带重构无关模块。
- 保留现有 API 路径、错误码、JSON 字段、配置读取与默认值语义。确需破坏兼容时，先说明影响，并提供迁移步骤和验证方案。
- 修改 `internal/proxy/` 或 `internal/admin/` 前，先阅读对应目录的 `AGENTS.md`。目录级实现说明也须与当前源码和用例核对。

### Provider、路由与请求生命周期

- `endpoint_type` 元数据只在 `internal/config/endpoint_type.go` 定义。新增类型须同步认证与路径策略、请求格式能力表、管理端元数据消费和回归；涉及用量或计价时，还须同步解析和模型目录。旧别名归一到现有类型，不能仅为厂商名称新增类型。
- 扩展协议时，明确支持的路径、请求格式、响应格式和转换范围。`dual_protocol` 不表示任意协议互转。保留已有 Azure 字段净化、multipart 转 JSON、自定义注入和模型名改写的各自适用条件。
- 客户端别名和上游模型名分别处理。切换 target（上游目标）时，从客户端原始模型名、请求体和路径重新计算映射，不能沿用上一目标的改写。
- 改动重试或 failover（切换上游目标）时，分别验证普通上游错误、网络错误、密钥池限流和额度耗尽。保留当前密钥池的有限重试例外，不得扩展为任意错误重试。取消与超时不得触发重试，避免重复推理或扣费。
- 每次实际选中的目标都须经过其准入检查。切换密钥、目标或结束流式请求时，平衡并发占用，关闭响应体并释放上下文。相关回归须覆盖失败、取消和资源释放。

### 计费与额度

- token 费用公式统一使用 `usage.CostRates.CostFor`。价格选择复用 `internal/pricing.Resolver`，不能在代理、后台或报表中再实现一套公式。
- 可选价格的 `nil` 表示缺值并回退，显式 `0` 表示零价。显式缓存读和不同期限的缓存写是缓存总量的子集，不能作为额外 token 重复计费。
- 历史费用按事件的 target、model 和时间选价。保留目标模板、全局模板、目录价的回退顺序及 provider 价格隔离；不能用当前时间重算历史峰谷价。
- 官方基础价与模型组扣费系数分别保存。统计和额度消费只折算一次。正系数下，额度上限为组基数 × 职级系数 × 个人系数；模型组扣费系数不进入该乘法。保留各系数为 `0` 时现有的免费或不限语义，并分别验证。
- 新增 usage 字段时，同步响应解析、事件保存、聚合、统计和额度消费。用量写入失败及恢复路径须可重试且不重复扣费；不能只验证客户端收到了成功响应。

### 持久化与管理权限

- bbolt 的 bucket、字段、键格式或聚合结构变化须兼容已有数据，或提供明确迁移。迁移须可重复执行，验证失败后可恢复，并保护已有快照；不能用删库重建完成升级。
- 保留用量恢复日志与数据库的一致性。备份或恢复方案须说明数据库、恢复日志及迁移快照的处理，不能覆盖新实例已产生的数据。
- 新增 `/admin/*` 接口须经过会话中间件。写操作使用 `ReadonlyGuard`，用户管理额外校验管理员角色，并沿用审计记录。保留已有修改本人密码接口的角色例外，不能把例外扩展到其他写操作。
- 管理页面改动须检查内嵌资源、接口和页面行为。弹层、Tooltip 和 JS 模块加载遵循 [`internal/admin/AGENTS.md`](internal/admin/AGENTS.md)，不能只凭编译通过验收 UI。

### 验证与交付

- 代码行为变化按 TDD 实施：先建立有意义的失败用例，再改实现。覆盖受影响行为、失败边界和直接消费者；不要写只复述实现的测试。
- 代理改动验证 `internal/proxy`；权限或 UI 改动验证 `internal/admin`；计费改动联查 `pricing`、`usage`、`quota`、`nosql` 及相关后台消费者。并发改动按影响范围增加 race 检查。
- 涉及协议、Patch、用量、计价或响应编码的改动，在 `make e2e` 中补充或运行对应的 fakellm 成功与失败用例，覆盖适用的 JSON、SSE、WS、显式 Patch 与 `standard` 反例，并回读用量、费用及不完整诊断。fakellm 合同验证不代替真实上游验收。
- 仅文档任务检查路径、命令、事实来源和 diff，执行 `make check-docs` 与 `git diff --check`。不因文档编辑主动部署或调用真实模型。
- 交付说明写清改动、已执行检查、未验证范围和环境状态。提交、推送、部署与发布是不同阶段，不能互相代替验收。

## 模型名称、协议和上游转发

- 客户端模型别名匹配按系统的大小写不敏感规则处理；发给 provider 的上游模型名必须保留配置中的原始字符和大小写。不要把上游 model string 统一转成小写。
- 客户端别名一律使用通用名，不带版本号：例如 `qwen-max`（而非 `qwen3.8-max`、`qwen3.7-max`）、`glm-flash` / `glm-max`（而非 `glm-5.3-flash`、`glm-5.3-glm`）。版本号只出现在 `model_mappings[].upstream` 上游映射值中；文档示例同样使用通用名。
- 本服务以 API 透传为主，同时有 provider 认证、路径与模型改写等有限处理。不要声称代理会把一种 API 协议自动转换为另一种协议，除非当前代码和对应回归覆盖明确证明该路径支持转换。客户端需要使用上游支持的 API 路径、协议和请求格式。
- 配置模型映射时，客户端别名与 provider 上游模型名是两个独立值；核对上游官方要求的拼写、大小写和 API 路径。

## 价格与系数口径

- 模型基础价按对应 provider 官方 API 价格填写，并保留基础价和扣费系数两个独立字段。系数 `0` 表示用户侧免费折算，不能因此把官方基础价改成零。
- 额度基数、模型组和系数属于部署数据，由运营方配置；不要在代码或示例中写死某个部署的档位。写配置前核对当前模型组、系数和本轮要求，不擅自合并或改名。
- 官方以人民币标价时，按部署方选定的固定汇率换算为美元后填写，同一价表使用同一口径；官方美元价直接使用，不再次换汇。按每百万 token 存储的价格保留小数点后 6 位。
- 分时段价格（如 DeepSeek 峰时、谷时）按官方价格和对应生效时段分别配置；不能合并成单一价，也不能把价率与免费系数混为一谈。
- 职级集合和系数由数据配置决定，不在代码或示例中写死。
- 修改计费行为前检查 `usage.CostRates.CostFor`、`internal/pricing.Resolver` 及相关测试，避免在多个层重复实现同一公式。

## 开发与测试流程

1. 先查重并明确问题、范围和验收条件，再在隔离分支实施；保留当前工作区的用户改动。
2. 规格用 GitHub Issue 记录（SpecGit 管理）：每个可独立验证的目标对应一个 Issue，写明 Why / Scope / Approach / Acceptance，进度、阻塞与证据链接也记在 Issue；PR 用 `Closes #N` 或 `Refs #N` 关联。
3. 按 TDD 验证本地行为：行为变更必须先建立能复现问题、且因正确原因失败的用例，再修改实现并确认通过；验证范围覆盖受影响行为及直接消费者。保持行为不变的重构与仅文档改动不需要人为制造失败用例，但要写明已跑的检查。
4. 运行本地项目门禁；本地可运行的必需门禁不得留给远端 CI 首次发现。本地使用 `go.mod` 声明的 Go 版本，与 CI 一致。
5. 功能分支以 Pull Request 合入 `dev`，不直接推送 `dev` 或 `main`。PR 按 [`.github/pull_request_template.md`](.github/pull_request_template.md) 填写说明、关联 Issue、TDD 证据（先失败的测试与修复后的通过）、已跑门禁、未验证范围，以及是否影响计费、协议或兼容性。
6. `dev` 验收通过后，`main` 只以 fast-forward 前进到已验收的 `dev` 提交并打 tag；不在 `main` 上单独提交或产生合并提交。
7. 发布镜像时，部署与已验证提交完全一致的不可变镜像，不以 `latest` 或重新构建的镜像替代已验证制品。上线前备份配置与 bbolt 数据库；若新实例已写入数据，不能用旧数据库覆盖新数据。

### CI 必需检查

GitHub Actions（[`.github/workflows/ci.yml`](.github/workflows/ci.yml)）在推送到 `dev`、`main` 以及目标为 `dev`、`main` 的 PR 上运行。只用 GitHub 托管 runner，不读取任何 secret；同一 ref 的新运行会取消旧运行。下列 job 名称是 `dev` 与 `main` 分支保护的必需检查，须保持稳定；改名或拆分时同步分支保护与本节：

| job | 命令 |
|---|---|
| `test` | `make check-docs`、`go vet ./...`、`go vet -tags=e2e ./test/e2e/`、`go build ./...`、`go test ./...` |
| `race` | `go test -race -count=1 ./internal/proxy/... ./internal/nosql/... ./internal/usage/... ./internal/quota/... ./internal/admin/... ./cmd/proxy/...` |
| `e2e` | `make e2e` |
| `integration` | `make integration` |
| `admin-ui` | `node scripts/testing/admin_ui_test.cjs` |
| `scripts` | 受跟踪的 `scripts/**/test_*.py` 与 `scripts/test_*.py` Python 单测（`python3 -m unittest`） |

`.github/workflows/docker-build.yml` 是可选的手动镜像构建，不属于必需检查；发布默认只附源码包。

常用本地命令：

```sh
make test                 # check-docs + go test ./...
go vet ./...
go vet -tags=e2e ./test/e2e/
go build ./...            # 与 CI 一致的编译检查
go test -race -count=1 ./internal/proxy/... ./internal/nosql/... ./internal/usage/... ./internal/quota/... ./internal/admin/... ./cmd/proxy/...
make build                # 先尝试联网更新模型目录，再构建 bin/llms-proxy
make integration          # go test -tags=integration ./test/...
make e2e                  # 真实代理二进制 + 进程内虚假上游 internal/fakellm，不调用真实模型
node scripts/testing/admin_ui_test.cjs # 管理前端模块行为门禁
python3 -m unittest scripts.test_verify_azure_v1_models scripts.test_workflow_image_tag
```

改动行为后按影响范围执行相应门禁。仅在用户明确说不跑测试时才跳过，并如实说明未验证范围。`scripts/check-agents-docs.sh` 检查 `AGENTS.md` 存在；不要求存在 `CLAUDE.md`。不要把未执行的门禁描述为通过。

## 安全与边界

- 安全检查限定为模型层的业务与代码审计，以及可预见的安全处理。
- 不得将真实 provider key、客户端 key、管理员密码、生产快照或含凭据的 `.env` 提交到 Git；环境文件仅保存在各自机器并限制权限。
- 私有部署资料（内网地址、主机、部署脚本、环境说明等）只放在仓库根目录的 `local/`。该目录已被 `.gitignore` 与 `.dockerignore` 忽略，并带有独立 `go.mod`，不参与根模块构建。不得把其中的内容复制进受跟踪的文件、提交信息或 PR 描述。
- 密钥只能从指定的环境变量或安全凭据入口读取。不得把密钥值写入文档、命令回显、日志、截图或提交。
- 默认测试使用 fakellm，不调用真实模型。需要调用真实上游模型时，先取得用户对该模型测试的明确同意；不得从“发布”“回归”或已有密钥推断授权。
- 测试环境和正式环境使用各自的配置、数据库及卷；禁止共用 bbolt 文件或为了测试停止正式服务。
- 不执行删除数据卷、清理用户文件、重置管理员、覆盖生产数据库等破坏性操作，除非用户对该具体操作给出明确授权。
- 修改 `internal/proxy/` 或 `internal/admin/` 前，先阅读对应目录中的 `AGENTS.md`。


<!-- specgit:project:v2:start -->
## SpecGit 2

Read project-root `.agents/skills/specgit-native/SKILL.md` before work; if missing, initialize with the shared `specgit init` CLI or restore an existing integration with `specgit setup` ([installation](https://github.com/LeXwDeX/SpecGit/blob/main/docs/agent-install.md)).
<!-- specgit:project:v2:end -->
