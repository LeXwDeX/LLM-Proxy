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
| `ui_embed.go` / `handlers_ui.go` / `ui/` | 内嵌 SPA 资源与受限资源路由；HTML 骨架、样式与按领域拆分的 JS 模块 |

## 🚨 会话与角色守卫（安全边界，禁止绕过）

1. **所有 `/admin/*` 路由必须经过 `SessionManager.Middleware`**（在 `cmd/proxy/router.go` 挂载）；不要新增绕过会话中间件的 admin 子路由。
2. **读写分级**：`routes.go::routes` 内部按「读路由（所有已登录角色可访问）/ 写路由（`r.With(readonlyGuard)` 包裹）」分组。新增接口时：
   - 只读查询 → 放入读路由组；
   - 任何 mutating 操作（POST/PUT/DELETE、config reload）→ 必须放入 `ReadonlyGuard` 包裹的写路由组；
   - 用户管理接口 → 额外在 handler 内检查 `session.Role == RoleAdmin`（admin-only）。
3. **禁止 admin 自降级**：管理员不能把自己的角色改成 readonly（`handlers_users.go` 已有防护，改用户管理逻辑时保留）。
4. **change-password 例外**：`POST /admin/api/change-password` 对包括 readonly 在内的所有已登录用户开放。
5. **公开枚举与能力由领域代码声明**：UI 通过 `GET /admin/data/schema` 加载选项、徽章、协议能力和默认值；endpoint type 来源为 `internal/config/endpoint_type.go`。旧 `/endpoint-types` 接口保留兼容。定价、角色、额度作用域及目标状态引用各自领域定义，不在 JS 维护第二份表。

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
- **启动顺序**：`core.js` 提供基础操作，`schema.js` 加载并验证元数据，`bootstrap.js` 最后执行。Schema 成功后才初始化功能模块并设置 ready；失败提供重试，编辑不得提前启用。新增模块加入 HTML 脚本清单，`ui_embed.go` 自动 embed；`ui_modules_test.go` 与 Node 行为门禁检查清单、依赖和初始化。

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
