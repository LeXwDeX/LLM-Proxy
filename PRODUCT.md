# LLMs Proxy

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

管理后台供管理员与只读账号查看代理运行情况、模型用量、费用和访问配置。写操作由管理员权限与现有会话规则控制。

## Product Purpose

为客户端提供统一的模型请求入口，依据模型映射和目标配置转发到上游，并记录用量、计价与额度消费。管理后台帮助操作者查看谁在使用服务、请求状态和用量，并维护现有配置。

## Operating Context

这是已有 Go 服务内嵌的中文管理后台，使用原生 HTML/CSS/JavaScript 模块。页面通过已有管理会话访问；管理员与只读账号共享只读看板。现有导航提供首页、目标、客户端、额度、用量等业务入口。

## Capabilities and Constraints

- 代理以 API 透传为主，按配置处理认证、路径和模型名；不能宣称任意协议转换。
- 用量事件存于 bbolt；价格与额度遵循已有领域合同。展示统计不得另建计费公式。
- 当前在途请求与 Responses WebSocket 会话分别计数；HTTP keep-alive socket 不具备客户端归属，不能当作客户端请求并发。
- Token 来自已记录的用量事件，进行中尚未报告的用量不估算；未知和失败不以零替代。
- 只读页面不暴露客户端或上游凭据；权限、Schema 初始化和模块依赖沿用现有边界。
- 现有业务导航和管理台样式体系继续可用。此次首页改版的布局与交互决定保存在表面说明中，不扩展到其他业务页面。

## Evidence on Hand

- 当前实现与约束：AGENTS.md、internal/admin/AGENTS.md、internal/proxy/AGENTS.md。
- 内嵌前端：internal/admin/ui/index.html、internal/admin/ui/js/overview.js。
- 请求生命周期：internal/proxy/service.go、internal/proxy/responses_websocket.go。
- 用量合同：internal/usage/store.go、internal/nosql/usage.go。
- 用户确认：2026-10-14 以全站汇总及客户端明细展示实时活动和近五分钟 Token，并直接制作可交互页面。

## Product Principles

- 让操作者看清当前活动及最近用量，读数与数据范围明确。
- 未知、采样不足与读取失败如实显示。
- 监控展示沿用业务事实，不改变转发、计费和额度。

## Accessibility & Inclusion

首页解释信息要能通过鼠标悬停、键盘聚焦和触屏操作查看。图表精确读数支持键盘；异常短提示保持可见。
