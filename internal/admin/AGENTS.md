# internal/admin — 管理后台开发约束

> 后台管理 REST API + 登录门户 + 内嵌 SPA。改动前先读本文件；项目级约束见根目录 `AGENTS.md`，主要路由在 `routes.go`，会话挂载见 `cmd/proxy/router.go`。仓库当前不维护 `docs/architecture.md`。

## 模块地图

| 文件 | 职责 |
|---|---|
| `handler.go` / `routes.go` | 依赖组装 / 全部 `/admin/*` REST 路由和读写守卫 |
| `handlers_{resource}.go` | 按资源拆分的 CRUD 与查询；目标、部署池、客户端、计价、额度、字典、用户、用量、审计、Trace 各归对应文件 |
| `handlers_config.go` / `http_helpers.go` | 配置与运行时更新协调 / HTTP 输出和内部错误处理 |
| `schema.go` | 从领域定义组装只读管理 Schema，无凭据、动态配置与数据库数据 |
| `session.go` | 会话管理：内存 Session 存储 + HMAC-SHA256 签名 Cookie、角色常量（`RoleAdmin`/`RoleReadonly`）、`Middleware`、`ReadonlyGuard` |
| `portal.go` | `/login`、`/logout` 登录门户页面与提交处理 |
| `users.go` | 管理员账号认证与密码哈希（随机盐 + SHA-256） |
| `ui_embed.go` / `ui_assemble.go` / `handlers_ui.go` | 内嵌 SPA 资源；包初始化时把 `ui/index.html` 的 include 行展开为完整 HTML（`adminUIHTML`）；受限 JS 路由 `/ui/js/*` |
| `ui/index.html` | 仅含外壳：head、侧栏/布局、页面与弹层 include 行、toast 容器、启动 watchdog、脚本清单 |
| `ui/css/admin.css` | 全部样式，由 `index.html` 的 `<style>` 内 include 引入 |
| `ui/partials/pages/<page>.html` / `ui/partials/modals/<kebab-id>.html` | 每页一个 `div.page`；每个 body 级弹层（`.modal-overlay` / `.quota-drawer-overlay`）一个文件，各含其前导注释行 |
| `ui/js/core/`、`ui/js/pages/<name>/` | 脚本：`core`、`schema`、`navigation`、`bootstrap` 在 `core/`；功能模块在 `pages/<name>/<name>.js`。`*.test.cjs` 不被 embed |

## 🚨 会话与角色守卫（安全边界，禁止绕过）

1. **所有 `/admin/*` 路由必须经过 `SessionManager.Middleware`**（在 `cmd/proxy/router.go` 挂载）；不要新增绕过会话中间件的 admin 子路由。
2. **读写分级**：`routes.go::routes` 内部按「读路由（所有已登录角色可访问）/ 写路由（`r.With(readonlyGuard)` 包裹）」分组。新增接口时：
   - 只读查询 → 放入读路由组；
   - 任何 mutating 操作（POST/PUT/DELETE、config reload）→ 必须放入 `ReadonlyGuard` 包裹的写路由组；
   - 用户管理接口 → 额外在 handler 内检查 `session.Role == RoleAdmin`（admin-only）。
3. **禁止 admin 自降级**：管理员不能把自己的角色改成 readonly（`handlers_users.go` 已有防护，改用户管理逻辑时保留）。
4. **change-password 例外**：`POST /admin/api/change-password` 对包括 readonly 在内的所有已登录用户开放。
5. **公开枚举与能力由领域代码声明**：UI 通过 `GET /admin/data/schema` 加载选项、徽章、协议能力和默认值；endpoint type 来源为 `internal/config/endpoint_type.go`。旧 `/endpoint-types` 接口保留兼容。定价、角色、额度作用域及目标状态引用各自领域定义，不在 JS 维护第二份表。

## 外壳行为约定（会话 / 版本 / 深链）

- **会话过期不丢草稿**：`core.api()` 收到 `401 {"error":"unauthorized"}` 时，若有打开的弹层/抽屉（`.modal-overlay.open`、`.quota-drawer-overlay.open`），或存在被标记为脏的表单，则不跳转登录页，而是显示持久横幅并以 `SessionExpiredError` 拒绝，调用方应保留表单。`core.setFormDirty(key, true)` 是可选接入的脏表单标记，目前没有页面使用。`background:true` 的请求遇 401 永不跳转，只显示横幅并拒绝。无未保存界面的前台请求仍跳转 `/login?next=<当前路径+hash>`，5 秒内未离开页面则复位重试锁。
- **后台轮询不续期**：`api(url, {background: true})` 发送 `X-Admin-Background: 1`，`SessionManager.Middleware` 对该请求不滑动会话、不重发 Cookie。页面的纯轮询应逐步改用此选项。会话绝对上限当前无配置项，暂未实现。
- **部署过期提示**：`/admin/api/me` 返回 `version`；core 记录首次版本，之后每 5 分钟及标签页恢复可见时后台复查，变化则显示“后台已更新，请刷新页面”。
- **启动顺序**：schema 成功并 `initialize()` 后立即加载总览；`/api/me` 与 trace 探测并行，布局保持 `inert`、body 保持 `readonly-mode` 直到角色确认。`ready` 不等待 trace 探测。缺失模块（脚本加载失败）在启动横幅点名。
- **深链**：`#<page>` 选择页面；用户点击导航用 `pushState`（Back 可用），启动与纠正用 `replaceState`；`hashchange` 在白名单与可达性内切换页面，不可达（如只读用户的 `#users`）则纠正为当前页。非总览深链等角色确认后才加载，保证页面以正确的只读状态渲染。登录页脚本把 URL hash 带入 `next`。缺失模块的启动横幅会点名模块及 watchdog 记录的失败脚本路径，boot watchdog 在 `data-boot-error` 存在时不再覆盖。

## CRUD 接口约定

- 资源路由模式：`GET/POST /admin/data/{resource}`，`PUT/DELETE /admin/data/{resource}/{name}`；新增资源沿用此模式。
- 数据落地在 `internal/nosql`（bbolt）；handler 只做参数校验 + store 调用 + `writeJSON` 响应，不要在 handler 里写存储逻辑。
- mutating 操作需写审计日志（audit store），保持与现有 CRUD handler 一致。
- **目标 ETag 与稳定 key ID**（`target_etag.go`、`nosql/target_etag.go`）：目标的 `etag` 是 `targets` bucket 中该目标**存储的原始 JSON 字节**的 SHA-256 前 16 字节（base64url 无填充，前缀 `t1-`），不持久化任何序列号，所以存量目标、旧二进制改写、`List` 规范化回写、迁移都会自然使旧 ETag 失效，没有“未版本化”的无保护状态。`GET /data/targets` 返回 `etag` 与 `key_ids`（与 `key_runtime_indices` 同形，只读角色同样返回，不含 key 值）。`PUT/DELETE /data/targets/{name}`、`…/pause`、`…/resume` 可带 `If-Match: "<etag>"`（容忍引号与 `W/` 前缀；也可是逗号分隔的列表，任一项匹配即通过；`*` 表示目标存在即匹配；空项、畸形项或非 `t1-` 形态返回 400）。handler 预检（`resolveIfMatch`）只用于快速失败，存储层在写事务内重新计算当前字节的 ETag 并比较（`UpdateIfMatch`/`DeleteIfMatch`）才是最终裁决，二者是有意的纵深防御。不匹配或目标不存在返回 409 `{"error":"target changed; refresh targets","code":"target_revision_conflict"}`——前置条件失败有意沿用 409（未采用 412），code 沿用旧名，前端按它识别；成功响应回传新 `etag`。key ID 为 `HMAC(secret, "admin/target-key-id/v2\x00" + lower(trim(目标名)) + "\x00" + key)` 前 16 字节（base64url，v2 起加入目标名域分隔，同一 key 在不同目标上 ID 不同；密钥存 meta `target_key_id_secret`；同目标内重复 key 追加 `.1`、`.2`）。**密钥不可用时 `key_ids` 为 JSON `null`（不是数组）**，其余字段照常；此时客户端不得提交 key 字段，服务端 PUT 对缺省（nil 指针）的 `api_key`/`api_keys` 保持现有 key 不变。`__existing_key_id__:<id>` 引用未知 ID 返回 400；`PUT …/keys/id/{key_id}/block|unblock` 按 ID 封禁，不检查 If-Match，也不改变存储内容，因此目标 ETag 不变。旧 `__existing_key_index__` + `X-Target-Key-Snapshot` 路径仅作兼容保留（下个版本移除），语义恢复为**全局**运行时 revision：任何目标发布后旧快照 token 都失效并返回 409，这是预期行为，只影响升级期间残留的旧标签页；按 ID 封禁/解封（`SetKeyBlockedByValue`）不推进 revision，旧快照 token 不受影响。按 ID 封禁/解封写审计记录（action `block_target_key`/`unblock_target_key`、目标名、`key_id=<id>`，不含 key 值）。
- **前端目标编辑合同**（`ui/js/pages/targets/`）：打开编辑时记录 `editingETag`，保存**始终**发送 `If-Match`（已存在目标必有 ETag；缺失则拒绝保存）和 `__existing_key_id__:<id>` 引用，不再发 `X-Target-Key-Snapshot`，也没有本地“快照/确认”拦截，由服务端裁决。`key_ids` 不可用（`null`、长度不符、或掩码显示有 key 的槽位 ID 为 `null`）时 key 列表只读显示（提示“密钥列表暂不可编辑，其他字段可正常保存”），保存请求**完全不含** `api_key`/`api_keys`，其余字段照常保存。409 `target_revision_conflict` 保留弹窗与草稿并提供“重新加载”；确认后用服务器最新内容**替换整个表单**（放弃草稿，含未保存的新增 key）。**结果未确认**由 `core.isUnconfirmedOutcome(err)` 统一判定：TimeoutError、NetworkError、任何 HTTP 5xx（含网关 502/503/504）、错误体不是 JSON（`api()` 标记 `invalidBody`）；带 JSON 体的 4xx 是明确失败，仍显示红色“保存失败”。（`isTransientError` 只用于启动期 GET 重试，与写操作无关。）保存遇未确认结果时不报红色失败，而是回读该目标并**按内容判定**：把服务器副本与本次请求实际发送的字段比较（key 字段按 key_id 集合与顺序比较，新输入的 key 按掩码比较；省略的字段不参与；null/空串/false/空集合视为等价）。内容一致即视为已保存（绿色“已保存”，按成功流程关闭弹窗）；不一致且 ETag 已变化则保留草稿，进入与 409 相同的冲突提示与“重新加载”流程，不替换表单；ETag 未变化则提示“保存未完成，可重试”并保留草稿。新建目标遇未确认结果：回读后目标存在且内容一致视为已创建；存在但内容不一致提示同名目标已存在并保留草稿；不存在提示可重试。列表行的删除/暂停/恢复遇未确认结果：先 `loadTargets` 回读，删除后目标不存在、或暂停/恢复后状态已等于期望值视为成功，否则黄色提示“操作结果未确认，列表已刷新”，不报红色失败。保存途中编辑会话已切换时，失败同样 toast。响应带 `warning`（如落库后 auth 应用失败的 `auth reload pending`）时显示黄色提示，不报失败。保存期间弹窗内全部控件 disabled（`setTargetFormLocked`）；封禁/解封走按 ID 路由；列表暂停/恢复/删除带列表中的 ETag，冲突时提示并刷新。别的目标变化不影响正在编辑的目标；被编辑目标 ETag 变化只显示非阻断提示。
- **提交管道**：目标与部署池的全部写 handler 经 `commitConfigChange`：`PrepareConfig` 校验 + 临时 auth store 校验 → 落库（CAS 用 `*IfMatch`）→ `CommitPrepared` → auth → `configManager.Replace` → `costCache.Invalidate`，全程在 `configMu` 内；释放锁之后（`configTxn.Unlock`、`scheduleQuotaRefresh`、reload）调用 `warmCostCache` 在锁外预先重建计价缓存，API 路径的下一次计费不再回读库。客户端增/改/删 handler 同样持有 `configMu`（否则会被目标保存落库后以旧 clients 列表做的 auth 应用覆盖），但锁内只做 clientStore 写入与 `reloadAuthFromClientStore`，`quotaManager.Evaluate`（可能较慢）在释放 `configMu` 之后执行，不阻塞目标保存。落库失败时运行时不变；不要再先应用运行时后落库。落库成功之后的步骤不得改变响应：auth 应用失败只记 Error 日志，响应仍为成功并附 `"warning":"auth reload pending"`（下一次 reload 收敛）。`CommitPrepared` 锁内只做 `replaceTargetsLocked` 原有的工作并推进全局 revision；代理核心不维护按目标的 revision。
- **额度刷新与最终一致窗口**：改变额度解析结果的写 handler（目标删除、模型组增改删、职级更新/删除、计价与定价模板写入）**不得**同步调用 `quota.Manager.RefreshAll`，也不得在持有 `configMu` 时刷新。统一经 `h.scheduleQuotaRefresh(quota.RefreshScope{...})`（持锁 handler 用 `configTxn.refresh`，在释放锁之后登记）：后台、去抖（默认 300ms 合并连续写）、有界并发（2），且只重算受影响客户端（目标：模型组绑定引用该目标；模型组：plan 含该组，改名带新旧两个名字；职级：绑定该职级；计价为 `All`；另外一律纳入正在校准与处于校准失败的客户端）。**最终一致窗口** = 去抖 + 排队 + 单客户端 Evaluate 耗时（本身约 2.5–3s）：窗口内准入仍使用已校准的旧计数器（旧绑定/系数/限额），不会放行未校准客户端，也不影响用量记账；整点校准兜底收敛。单客户端写（客户端创建/改额度、plan 同步）仍同步 `Evaluate` 该客户端，因为调用方马上要读到它的新状态。
  - **配置代次**：所有影响额度判定的写入（模型组、职级、计价、目标、部署池、客户端、reload）在**提交成功之后**推进 `quota.Manager` 的配置代次（`ScheduleRefresh` 自带；目标/池经 `commitConfigChange` 调 `noteQuotaConfigChange`；客户端写在 `Evaluate` 前调 `NoteConfigChange`；新建职级也经 `ScheduleRefresh`）。`evaluate` 读配置前记下代次，写计数器前代次已变则丢弃结果重算（上限 5 次，每次尝试各自持有记账 gate、尝试之间释放并指数退避，不得整体包住 gate）。超限时：**已有计数器**的客户端保留旧计数器、不改变准入状态（与窗口内沿用已校准旧计数器一致），记 Warn 并延后重新入队重算；**冷客户端**（尚无计数器）按校准失败处理，保持 fail-closed。`Increment` 冷启动同样记代次，读配置期间代次变化则不建计数器。这保证「正在校准、尚无计数器」的冷客户端不会用旧限额落地计数器。新增会影响额度判定的写入口必须在落库成功后推进代次。
  - **请求路径校准**：冷客户端首个请求最多等待 15s（`calibrationRequestTimeout`），超时按校准失败处理（拒绝并退避）；退避期满而校准仍在进行时一律跳过、不再等待；校准 goroutine 纳入 `Manager.Stop` 的跟踪，panic 被恢复并按校准失败处理。
  - **写响应与回读**：已登记后台刷新的写响应附 `"quota_refresh":"pending"`（目标删除、模型组增改删、职级更新、计价/定价模板写入；目标/部署池的创建与更新不改变额度解析结果，不带该字段）。进度回读复用 `GET /admin/metrics` 的 `quota_refresh`：`config_generation`（已提交的额度相关变更数）、`completed_batches`、`pending`（有刷新排期或运行中）。前端在对应保存成功提示后附“额度将在后台刷新”（计价页保存/删除、目标列表行操作与目标保存，仅当响应带 `quota_refresh`；计价模板删除为 204 无响应体，提示固定附带）。
- **configTxn（锁外审计与计价缓存预热）**：目标/部署池的写 handler 用 `txn := h.lockConfig(r); defer txn.Unlock()`，锁内只做校验与落库，`txn.audit(...)` 与 `txn.refresh(...)` 推迟到释放 `configMu` 之后执行。审计时间戳在 `txn.audit`（持锁）内分配且严格递增，写入推迟到释放锁之后，因此审计顺序等于提交顺序，与锁外写入快慢无关。审计仍是同步、可靠的写入（响应结束前已落库），没有合并进目标写事务（目标写入口众多，合并需改所有 store 的签名；审计失败本就不影响业务结果）。例外：密钥封禁/解封 handler（`handleSetTargetKeyBlocked*`）的审计保留在锁内——它们在审计之后计算条件快照的后继令牌，必须与运行时变更互斥（`TestIssue68Successor…` 钉住）。`handleListTargets` 的 ETag 读库在取 `configMu` 之前完成（先读 ETag 再取配置快照：并发写入只会产生「旧 ETag + 新配置」→ 保存时冲突，不会丢更新），锁内只取配置快照与运行时 key 快照。
- 接口行为变化须同步处理器及其回归测试；本仓库当前不维护 `docs/api-contract.md`，不要创建指向该文件的引用。

## 测试要求

- 改动后必须全绿：`go test ./internal/admin/ -count=1`。
- 涉及会话/守卫的改动，重点核对 `handler_test.go` 中 readonly 角色被写接口拒绝的用例。
- UI 为内嵌页面，改动后运行 `node scripts/testing/admin_ui_test.cjs`，并在本机独立实例（例如 `make ui-sim`）验证登录、页面加载和受影响交互，不能只凭编译通过验收。真实浏览器脚本 `scripts/testing/admin_ui_browser_test.cjs` 依赖本机已安装的 Playwright/Chromium，不在 CI 中运行；`go test ./internal/admin/` 在有 Node 时调用 Node 行为门禁及部分 `*.test.cjs` 单元用例，CI 还会递归运行 UI 子树中的 Node 单元测试，可从项目根目录用 `(cd internal/admin/ui && node --test)` 运行同一范围。
- **模块初始化约定**：模块经 `AdminApp.register(name, factory)` 声明，按名称初始化；跨模块调用使用明确导出与 `app.bind`，不依赖工厂数组位置或其他模块私有变量。`core.js` 末尾残留的 `window.*` 导出仅为兼容，新代码不得依赖；新增全局桥接仅限 HTML 事件属性（当前只有 `closeTraceDetail`）。
- **启动顺序**：`core.js` 提供基础操作，`schema.js` 加载并验证元数据，`bootstrap.js` 最后执行。Schema 成功后才初始化功能模块并设置 ready；失败提供重试，编辑不得提前启用。新增模块放入 `ui/js/pages/<name>/<name>.js`（启动期基础设施放 `ui/js/core/`），并在 `ui/index.html` 脚本清单中按顺序加入 `<script src="/admin/ui/js/pages/<name>/<name>.js">`（路径含子目录；`handlers_ui.go` 会为 src 追加版本参数）；`ui_embed.go` 的 embed 模式覆盖 `core/*.js` 与 `pages/*/*.js`，无需修改。`ui_modules_test.go` 与 Node 行为门禁检查清单、依赖和初始化。

## UI 文件布局与 include 机制

- `ui/index.html` 中独占一行的 `<!-- @include 相对路径 -->`（可带缩进，路径相对 `ui/`）整行（含换行）被所引文件内容逐字替换，不重排缩进；被引文件必须以换行结尾，可嵌套。目标缺失、循环引用或残留 `@include` 标记会使包初始化 panic，测试同样覆盖。
- Go 在 `ui_assemble.go` 装配；Node 测试统一经 `scripts/testing/assemble_admin_html.cjs`（规则与 Go 一致，`TestAssembledHTMLMatchesNodeAssembly` 与 `ui/assemble.test.cjs` 守卫），不得再直接读取 `index.html` 当完整页面。
- 新增页面：在 `ui/partials/pages/` 新建文件（内含该页的前导注释行与唯一的 `div.page`），在 `index.html` 对应位置加 include 行。新增弹层：在 `ui/partials/modals/` 新建文件（恰好一个 body 级弹层），在 `index.html` 的弹层区加 include 行。`TestModalPartialsHoldExactlyOneOverlay` 与 `TestPagePartialsHoldExactlyOnePage` 强制这两条。
- 脚本落点：页面脚本放 `ui/js/pages/<name>/*.js`（仅一层目录，由 `ui_embed.go` 的 `pages/*/*.js` 模式 embed；更深层级不会被 embed）；`core/` 同理只含 `*.js`。页面拆成多个文件时的硬性规则：由页面的一个模块持有共享可变状态，经明确的对象或访问器暴露，不保留过期副本；页面对外 API（其他页面 `bind` 的名称）保持在页面主模块名下（必要时用门面模块）；跨模块访问只用 `app.bind` / `initializeModule`；加载顺序即 `index.html` 脚本清单顺序；文件放在 `js/pages/<page>/`。拆分出的新文件必须按依赖顺序加入 `index.html` 脚本清单：清单中每个标签必须解析到 embed 文件，每个 embed 的 `.js` 恰好被一个标签加载，文件名可含字母、数字、`_`、`-`、`.`（`TestUIModuleManifestMatchesEmbeddedAssets` 强制）。`core/core.js` 必须第一个、`core/bootstrap.js` 最后一个加载。
- `fake-clock.cjs` 及其他 `*.cjs`（含 `*.test.cjs`、`*.browser.cjs`）不会被 embed，也不属于脚本清单。
- 针对某页面脚本的检查使用 `adminUIPageScripts(t, "<page>")`（Go）或 `scripts/testing/admin_ui_scripts.cjs` 的 `pageSource("<page>")`（Node），它们拼接该目录下全部文件，目录为空或内容为空时失败；不要把测试写死到单个文件路径。脚本清单（Go：`adminUIScriptManifest()`，Node：`manifest()`）从装配后的 HTML 推导，不要再维护硬编码列表。
- 装配拒绝：同一 partial 被 include 两次（会产生重复 DOM id）、任何被引文件含 UTF-8 BOM。
- 样式写入 `ui/css/admin.css`；`index.html` 内除 boot watchdog 外不得新增内联脚本；watchdog 位于 `<head>`、早于全部 `/admin/ui/js/` 脚本，并以捕获阶段 `error` 监听把加载失败或加载期报错的脚本路径记入 `window.__adminBootFailures`，`bootstrap.js` 的启动横幅据此列出“以下脚本加载失败”，不得按模块名猜测路径。

## 拆分约定（硬性规则）

- 页面文件放在 `ui/js/pages/<page>/`；主模块文件 `<page>.js` 注册 `<page>`；部件模块注册 camelCase 的 `<page><Part>`（如 `targetsSave`、`overviewState`），文件名可与模块名不同。
- 其他页面只 `app.bind` 某页面的主模块（`ui/page-boundaries.test.cjs` 强制）；跨页通用的辅助函数放入 `core`。
- 没有任何模块 `app.bind` 的部件，须由所属页面主模块用 `app.requires([...])` 声明，使启动检查能发现其加载失败。
- `index.html` 加载顺序：状态与辅助模块先于页面主模块。

## UI 弹层统一规范（强制）

2026-08-31 起：index.html 内全部弹层（`.modal-overlay` 标准弹窗、`.quota-drawer-overlay` 侧滑抽屉）必须遵守，由 `TestUIOverlaysMountedAtBody`（`ui_syntax_test.go`）机器强制：

1. **body 直连挂载**：弹层开标签时 div 深度必须为 0（`<body>` 直接子节点），禁止嵌入任何 `.page`、panel 或其他弹层——宿主隐藏（`display:none`）会连坐全部后代且无任何报错（曾因此吞掉 9 个弹层，直到部署后才发现）。
2. **仅两类组件**：标准弹窗 `.modal-overlay > .modal`；侧滑抽屉 `.quota-drawer-overlay > .quota-drawer`。不新造第三种形态。
3. **唯一开关**：只经 `openModal(id) / closeModal(id)`（带 null 防护），禁止直接 `classList.add("open")`。
4. **标签配平**：`</div>` 与 `<div>` 必须严格配平，门禁同时断言全文净深度为 0。
5. 豁免名单：`traceDetailModal`（页内详情层，`display:none` 内联控制，历史一致设计）。

## 全局 Tooltip 交互规范

- 信息提示统一使用 `role="tooltip"`，由全局 CSS 设置 `pointer-events:none`，不得拦截鼠标或遮挡下层控件的命中。
- 鼠标移到哪个触发元素就更新为该元素的提示；不得因鼠标处于提示框的显示区域而保持旧内容。
- Tooltip 仅展示信息，不放置可点击、可聚焦或需鼠标滚动的控件；需要交互的内容使用现有弹窗或抽屉。
- 验证必须用真实鼠标坐标从上排移向被提示覆盖的下排，确认下层元素命中、提示切换和点击正常。
