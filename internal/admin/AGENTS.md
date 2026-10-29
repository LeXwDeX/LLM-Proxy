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
| `internal/admin/testdata/index.golden.html` | 拆分前 `index.html` 的快照；`TestAssembledHTMLMatchesGolden` 校验装配结果逐字节一致（仅脚本路径映射除外）。过渡性守卫，布局稳定后可删除 |

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
- **深链**：`#<page>` 选择页面；用户点击导航用 `pushState`（Back 可用），启动与纠正用 `replaceState`；`hashchange` 在白名单与可达性内切换页面，不可达（如只读用户的 `#users`）则纠正为当前页。非总览深链等角色确认后才加载，保证页面以正确的只读状态渲染。登录页脚本把 URL hash 带入 `next`。缺失模块的启动横幅会点名模块与应定义它的脚本，boot watchdog 在 `data-boot-error` 存在时不再覆盖。

## CRUD 接口约定

- 资源路由模式：`GET/POST /admin/data/{resource}`，`PUT/DELETE /admin/data/{resource}/{name}`；新增资源沿用此模式。
- 数据落地在 `internal/nosql`（bbolt）；handler 只做参数校验 + store 调用 + `writeJSON` 响应，不要在 handler 里写存储逻辑。
- mutating 操作需写审计日志（audit store），保持与现有 CRUD handler 一致。
- 接口行为变化须同步处理器及其回归测试；本仓库当前不维护 `docs/api-contract.md`，不要创建指向该文件的引用。

## 测试要求

- 改动后必须全绿：`go test ./internal/admin/ -count=1`。
- 涉及会话/守卫的改动，重点核对 `handler_test.go` 中 readonly 角色被写接口拒绝的用例。
- UI 为内嵌页面，改动后运行 `node scripts/testing/admin_ui_test.cjs`，并在本机独立实例（例如 `make ui-sim`）验证登录、页面加载和受影响交互，不能只凭编译通过验收。真实浏览器脚本 `scripts/testing/admin_ui_browser_test.cjs` 依赖本机已安装的 Playwright/Chromium，不在 CI 中运行；`go test ./internal/admin/` 在有 Node 时调用 Node 行为门禁及部分 `*.test.cjs` 单元用例，CI 还会递归运行 UI 子树中的 Node 单元测试，可从项目根目录用 `(cd internal/admin/ui && node --test)` 运行同一范围。
- **模块初始化约定**：模块经 `AdminApp.register(name, factory)` 声明，按名称初始化；跨模块调用使用明确导出与 `app.bind`，不依赖工厂数组位置或其他模块私有变量。`core.js` 末尾残留的 `window.*` 导出仅为兼容，新代码不得依赖；新增全局桥接仅限 HTML 事件属性（当前只有 `closeTraceDetail`）。
- **启动顺序**：`core.js` 提供基础操作，`schema.js` 加载并验证元数据，`bootstrap.js` 最后执行。Schema 成功后才初始化功能模块并设置 ready；失败提供重试，编辑不得提前启用。新增模块放入 `ui/js/pages/<name>/<name>.js`（启动期基础设施放 `ui/js/core/`），并在 `ui/index.html` 脚本清单中按顺序加入 `<script src="/admin/ui/js/pages/<name>/<name>.js">`（路径含子目录；`handlers_ui.go` 会为 src 追加版本参数）；`ui_embed.go` 的 embed 模式覆盖 `core/*.js` 与 `pages/*/*.js`，无需修改。`ui_modules_test.go` 与 Node 行为门禁检查清单、依赖和初始化。

## UI 文件布局与 include 机制

- `ui/index.html` 中独占一行的 `<!-- @include 相对路径 -->`（可带缩进，路径相对 `ui/`）整行（含换行）被所引文件内容逐字替换，不重排缩进；被引文件必须以换行结尾，可嵌套。目标缺失、循环引用或残留 `@include` 标记会使包初始化 panic，测试同样覆盖。
- Go 在 `ui_assemble.go` 装配；Node 测试统一经 `scripts/testing/assemble_admin_html.cjs`（规则一致，`ui/assemble.test.cjs` 与 Go 测试都对照同一份 golden），不得再直接读取 `index.html` 当完整页面。
- 新增页面：在 `ui/partials/pages/` 新建文件（内含该页的前导注释行与唯一的 `div.page`），在 `index.html` 对应位置加 include 行。新增弹层：在 `ui/partials/modals/` 新建文件（恰好一个 body 级弹层），在 `index.html` 的弹层区加 include 行。`TestModalPartialsHoldExactlyOneOverlay` 与 `TestPagePartialsHoldExactlyOnePage` 强制这两条。
- 脚本落点：页面脚本放 `ui/js/pages/<name>/*.js`（仅一层目录，由 `ui_embed.go` 的 `pages/*/*.js` 模式 embed；更深层级不会被 embed）；`core/` 同理只含 `*.js`。页面拆成多个文件时的硬性规则：由页面的一个模块持有共享可变状态，经明确的对象或访问器暴露，不保留过期副本；页面对外 API（其他页面 `bind` 的名称）保持在页面主模块名下（必要时用门面模块）；跨模块访问只用 `app.bind` / `initializeModule`；加载顺序即 `index.html` 脚本清单顺序；文件放在 `js/pages/<page>/`。拆分出的新文件必须按依赖顺序加入 `index.html` 脚本清单：清单中每个标签必须解析到 embed 文件，每个 embed 的 `.js` 恰好被一个标签加载，文件名可含字母、数字、`_`、`-`、`.`（`TestUIModuleManifestMatchesEmbeddedAssets` 强制）。`core/core.js` 必须第一个、`core/bootstrap.js` 最后一个加载。
- `fake-clock.cjs` 及其他 `*.cjs`（含 `*.test.cjs`、`*.browser.cjs`）不会被 embed，也不属于脚本清单。
- 针对某页面脚本的检查使用 `adminUIPageScripts(t, "<page>")`（Go）或 `scripts/testing/admin_ui_scripts.cjs` 的 `pageSource("<page>")`（Node），它们拼接该目录下全部文件，目录为空或内容为空时失败；不要把测试写死到单个文件路径。脚本清单（Go：`adminUIScriptManifest()`，Node：`manifest()`）从装配后的 HTML 推导，不要再维护硬编码列表。
- 装配拒绝：同一 partial 被 include 两次（会产生重复 DOM id）、任何被引文件含 UTF-8 BOM。
- 样式写入 `ui/css/admin.css`；`index.html` 内除 boot watchdog 外不得新增内联脚本。

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
