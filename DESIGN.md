---
name: LLMs Proxy 管理台
description: 深色侧栏与主题化工作区构成的中文运维界面，用一套离线可用的亮/暗 token 承载清晰的读数、表格与配置表单。
colors:
  bg: "#F4F5F7"
  surface: "#FFFFFF"
  surface-2: "#F8F9FB"
  line: "#E3E6EB"
  line-2: "#EEF0F3"
  ink: "#14171C"
  ink-2: "#444C59"
  ink-3: "#5C6573"
  accent: "#2B59D9"
  accent-ink: "#FFFFFF"
  accent-soft: "#E9EEFC"
  out: "#A65314"
  out-soft: "#FBEEE2"
  ok: "#1C7A4A"
  ok-soft: "#E2F2E9"
  warn: "#8A5700"
  warn-soft: "#FAF0DA"
  bad: "#B42318"
  bad-soft: "#FCE8E6"
  muted-soft: "#EEF0F3"
  side: "#11141A"
  side-2: "#1D222B"
  side-ink: "#D3D8DF"
  side-ink-2: "#8E97A4"
typography:
  body:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "PingFang SC", "Microsoft YaHei", sans-serif'
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.5
  title:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "PingFang SC", "Microsoft YaHei", sans-serif'
    fontSize: "16px"
    fontWeight: 600
    lineHeight: 1.5
  label:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "PingFang SC", "Microsoft YaHei", sans-serif'
    fontSize: "12px"
    fontWeight: 500
    lineHeight: 1.5
  mono:
    fontFamily: 'ui-monospace, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace'
    fontSize: "12px"
    fontWeight: 400
    lineHeight: 1.5
rounded:
  card: "10px"
  control: "8px"
  badge: "6px"
  nav: "7px"
  pill: "20px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "12px"
  lg: "16px"
  xl: "20px"
  section: "24px"
components:
  button-secondary:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    padding: "7px 14px"
  button-primary:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.accent-ink}"
    rounded: "{rounded.control}"
    padding: "7px 14px"
  input:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.control}"
    height: "34px"
  panel:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.card}"
    padding: "16px 20px"
  stat-card:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.card}"
    padding: "16px 20px"
  badge:
    backgroundColor: "{colors.muted-soft}"
    textColor: "{colors.ink-3}"
    rounded: "{rounded.badge}"
    padding: "2px 8px"
  client-table:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    padding: "10px 16px"
---

# Design System: LLMs Proxy 管理台

## Overview

**Creative North Star: "深色侧栏里的双主题工作面"**

这是一个面向中文运维工作的管理界面。固定的深色侧栏为工作区提供清楚边界，工作区本身可以在亮色与暗色之间切换。两套主题共用同一组语义 token：背景、表面、线条、文字、强调色和状态色，不按页面重复定义颜色。

页面以数字、表格和分区面板组织高密度信息。系统字体提供自然的跨平台中文呈现；数字、ID 和技术值使用系统等宽字体并开启表格数字，便于比较和对齐。所有图标为内联线性 SVG，不依赖外部字体、图标库或 CDN。

**Key Characteristics:**
- 深色侧栏固定，工作区亮/暗主题同源。
- 蓝色强调动作与选中状态，状态色只用 soft 背景加深色前景。
- 高密度表格与清楚的数字对齐。
- 全部资源内联，断网或内网部署可加载。

## Colors

以下两套值来自已确认原型的 `.app` 与 `.app.dark`，实现中逐字保留。所有 token 定义在 `html.app`；暗色由 `html.app.dark` 覆盖。组件只能使用这些语义 token 或由它们派生的兼容别名。

### Light tokens

| Token | Value | 用途 |
|---|---|---|
| `--bg` | `#F4F5F7` | 工作区底色 |
| `--surface` | `#FFFFFF` | 卡片、面板、输入、按钮表面 |
| `--surface-2` | `#F8F9FB` | 表头、悬停、次级表面 |
| `--line` | `#E3E6EB` | 面板、输入、表格边界 |
| `--line-2` | `#EEF0F3` | 表格内分隔线 |
| `--ink` | `#14171C` | 主文字 |
| `--ink-2` | `#444C59` | 次级文字 |
| `--ink-3` | `#5C6573` | 说明、单位、弱化文字 |
| `--accent` | `#2B59D9` | 主操作、选中、输入趋势 |
| `--accent-ink` | `#FFFFFF` | 强调色表面上的文字 |
| `--accent-soft` | `#E9EEFC` | 蓝色浅底、选中行、信息徽章 |
| `--out` | `#A65314` | 输出趋势、输出型徽章前景 |
| `--out-soft` | `#FBEEE2` | 输出型徽章背景 |
| `--ok` | `#1C7A4A` | 成功、正常、额度充足 |
| `--ok-soft` | `#E2F2E9` | 成功徽章背景 |
| `--warn` | `#8A5700` | 警告、接近限额 |
| `--warn-soft` | `#FAF0DA` | 警告徽章背景 |
| `--bad` | `#B42318` | 错误、危险操作、超额 |
| `--bad-soft` | `#FCE8E6` | 错误徽章背景 |
| `--muted-soft` | `#EEF0F3` | 禁用、中性徽章、未配置 |
| `--side` | `#11141A` | 侧栏背景 |
| `--side-2` | `#1D222B` | 侧栏悬停、选中、分隔 |
| `--side-ink` | `#D3D8DF` | 侧栏正文 |
| `--side-ink-2` | `#8E97A4` | 侧栏分组标题、版本、辅助文字 |

### Dark tokens

| Token | Value |
|---|---|
| `--bg` | `#0D0F12` |
| `--surface` | `#15181D` |
| `--surface-2` | `#1A1E24` |
| `--line` | `#272C34` |
| `--line-2` | `#20252C` |
| `--ink` | `#E7EAEE` |
| `--ink-2` | `#B9C0CA` |
| `--ink-3` | `#959EAB` |
| `--accent` | `#7D9CFF` |
| `--accent-ink` | `#0D0F12` |
| `--accent-soft` | `#1C2645` |
| `--out` | `#F0A15C` |
| `--out-soft` | `#3A2715` |
| `--ok` | `#5CC48C` |
| `--ok-soft` | `#14301F` |
| `--warn` | `#E6B450` |
| `--warn-soft` | `#33270F` |
| `--bad` | `#F2796D` |
| `--bad-soft` | `#3A1916` |
| `--muted-soft` | `#22272E` |
| `--side` | `#090B0E` |
| `--side-2` | `#181C23` |

`--side-ink` 与 `--side-ink-2` 在暗色下继承亮色值，因为侧栏本身在两种主题中都保持深色。

### Named Rules

**The Token Pair Rule.** 状态徽章始终由 `*-soft` 背景与对应前景 token 组成：成功用 `--ok-soft`/`--ok`，警告用 `--warn-soft`/`--warn`，错误用 `--bad-soft`/`--bad`，输出型用 `--out-soft`/`--out`，中性用 `--muted-soft`/`--ink-3`。不要在组件或动态 HTML 中写十六进制状态色。

**The One Accent Rule.** `--accent` 是唯一主交互强调色。输出、成功、警告、错误只表达数据或状态语义，不替代主操作色。图表同时用颜色和线型区分数据，颜色不是唯一线索。

## Typography

**Body Font:** 系统无衬线字体栈：`-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, "PingFang SC", "Microsoft YaHei", sans-serif`。

**Mono Font:** 系统等宽字体栈：`ui-monospace, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace`，并启用 `font-variant-numeric: tabular-nums`。

**Character:** 中文使用操作系统自带字形；数字、ID、Key 掩码、时间戳、价格和技术值使用等宽字体。页面不请求网络字体。

### Hierarchy

- **Title** (700，18px，1.2)：顶栏页面标题。
- **Panel Title** (600，15px，1.5)：面板标题。
- **Body** (400，14px，1.5)：默认正文和表单文字。
- **Body Small** (400，13px，1.5)：表格内容、按钮文字、说明。
- **Label** (500，12px，1.5)：表头、字段标签、状态和辅助说明。
- **Metric** (650，30px，1.2)：首页实时汇总数字；移动端缩至 26px。
- **Micro Label** (400–500，10–11px)：图表轴、DEBUG 标记和低优先级元数据。

### Named Rules

**The Readable Numbers Rule.** 数字表格、统计值、ID 和技术值使用等宽字体和 `tabular-nums`。不要在普通正文里混用等宽字体。

## Layout

后台采用固定侧栏与弹性工作区的横向框架。桌面侧栏宽 240px，顶栏高 56px，内容区通常使用 24px 内边距。侧栏内边距为 12px，导航项高 36px，分组之间保留 18px 间距。

在 768px 以下，侧栏变为从左滑入的覆盖层，内容区内边距缩至 16px。600px 以下顶栏收紧：用户徽章只保留头像，修改密码按钮只保留图标；图标按钮保留 `aria-label`。表格始终放在横向滚动容器内，首列按现有页面规则固定，不因主题改造压缩列宽。

首页先显示托管链路、区间选择与区间卡片、目标健康、客户端/模型费用 Top 5 和最近变更；实时活动的四列汇总带、并排双图、客户端搜索与表格、历史统计折叠区位于下层辅助区。窄屏链路和区间卡片两列，图表单列，费用排行单列，表格横向滚动。

## Elevation & Depth

界面用表面色差和细边框分层，阴影只表达轻微浮起或浮层关系。亮色主题使用低透明黑投影；暗色主题使用更深、更弱的投影，避免灰色光晕。

- **轻表面**：卡片、面板、顶栏的边缘层次。
- **悬停面板**：统计卡片和短提示的浮起状态。
- **模态层**：编辑对话框和抽屉的独立层级。

**The Border Before Shadow Rule.** 静态内容先靠 `--surface` 与 `--line` 分组；更明显的阴影只留给悬停或浮层。侧栏与内容区之间不使用大面积阴影。

## Shapes

- **卡片与面板**：10px 圆角，1px `--line` 边框。
- **按钮与输入**：8px 圆角；输入高度 34px。
- **徽章与标签**：6px 圆角；状态徽章内边距 2px 8px。
- **导航项**：7px 圆角，36px 高。
- **胶囊控件**：时间范围按钮、用户徽章和计数徽章使用 20px 或 999px 胶囊圆角。
- **模态框**：12px 圆角；抽屉保持右侧贴边。

**The Compact Chip Rule.** 徽章不做胶囊形；胶囊只用于可切换的时间范围、用户身份和计数摘要。

## Components

### Buttons

按钮使用页面系统字体，内边距 7px 14px，8px 圆角。次级按钮为 `--surface` 背景、`--line` 边框和 `--ink` 文字，悬停改为 `--surface-2`。主要按钮为 `--accent` 背景和 `--accent-ink` 文字。危险按钮使用 `--bad` 背景和 `--accent-ink` 文字。按钮内 SVG 统一 16px，不拉伸文字。

仅图标按钮必须带 `aria-label`，并保留至少 34px 的可点击尺寸。按钮的键盘焦点使用清晰的外环，不依赖悬停。

### Badges and Chips

状态徽章统一使用 6px 圆角和 `*-soft`/前景 token 组合。`DEBUG`、只读、额度、模型类别和客户端授权标签都遵循此规则。动态生成的徽章通过 CSS 类或 `var(--...)` 内联样式取色，不写死颜色值。

首页趋势图例用短实线区分并发与输入，用虚线区分输出；图例文字始终与颜色同时出现。

### Cards and Containers

统计卡和面板使用 `--surface`、`--line`、10px 圆角和轻阴影，内边距通常为 16px 20px。面板标题使用 15px/600，标题区与内容区用 `--line` 分隔。首页图表和客户端容器保留 12px 圆角。

代码块、Trace 详情和提示浮层使用 `--side` 深色背景及 `--side-ink` 文字，在两种主题下保持稳定的代码阅读表面。

### Inputs and Fields

输入、下拉和多行文本使用 `--surface` 背景、`--line` 边框、8px 圆角和 34px 标准高度。聚焦时边框切换为 `--accent` 并显示柔和外环。禁用和只读控件使用 `--muted-soft` 背景与 `--ink-3` 文字，保持明确但降低交互提示。

### Navigation

侧栏使用 `--side` 背景和 `--side-ink` 正文。导航分为五组，顺序固定：

1. **首页**：总览。
2. **上游与接入**：目标与部署池、客户端。
3. **计费与额度**：模型计费、额度管理、模型目录。
4. **观测**：消费统计、审计日志、请求记录。
5. **系统**：后台用户。

分组标题使用 11px、字距 0.06em 和 `--side-ink-2`。导航项高 36px、7px 圆角；悬停和当前项使用 `--side-2` 背景与白色文字，当前项的图标使用浅蓝强调色。当前项设置 `aria-current="page"`；导航项可用 Tab 聚焦，并可用 Enter 或空格激活。后台用户和请求记录入口默认隐藏，分别由角色和 Trace 能力开关控制。

移动端顶栏的菜单按钮切换侧栏覆盖层，并同步更新 `aria-expanded`。

### Overview Panels

总览的托管链路是四段横条，依次显示下游客户端、代理、上游目标和密钥池；缺失数据源显示不可用，不使用示意值。实时状态卡片下方的运维提示行（`#chainOpsNotice`）用 12px `--warn` 文字列出非零的积压、丢弃、观察预算拒绝和自启动累计失败计数，全部为零或数据缺失时隐藏。区间选择是胶囊按钮组，当前项以 `--accent` 背景和 `aria-pressed` 表示。区间卡片使用 `--card` 表面、10px 圆角和表格数字，显式零值显示 `0` 或 `$0.00`，缺失值显示「不可用」。

目标健康、费用 Top 5 和最近变更使用 12px 圆角的表面面板。异常目标排在前面；密钥只显示汇总，单 Key 和零 Key 显示各自的明确降级文案。费用排行榜和最近变更位于横向滚动容器内；窄屏时费用面板改为单列。最近变更只显示时间、操作者、动作、对象和结果，不显示审计 detail。

### Client Activity Table

总览辅助区中的客户端表格仍是主要的对象选择控件。列标题可排序，行悬停使用 `--surface-2`，选中行使用 `--accent-soft`；客户端名称可聚焦并激活。窄屏时表格横向滚动并固定首列。其他业务表沿用现有滚动容器，不因主题改造改变列顺序或操作方式。

### Activity Charts

并发峰值使用柱状（`activity-concurrency-bar`，颜色取 `--activity-concurrency`，亮色 `#355d9d`，暗色 `#8FA9FF`），未知桶留空不连通。输入/输出 Token 保持折线：输入使用 `--accent`，输出使用 `--out` 并带虚线。双图共享时间范围和同一时间桶游标。每张图可独立获得键盘焦点，悬停或在聚焦图上使用方向键时，两图同步显示对应时间桶。读数包含时间段和单位。

### Theme Toggle

顶栏的主题按钮是纯图标按钮，带 `aria-label`、`aria-pressed` 和动态标题。亮色主题显示月亮图标，暗色主题显示太阳图标。初始主题读取 `localStorage` 的 `llms-proxy-admin-theme`，缺省为亮色；切换后写回同一键。主题在 `core.js` 中应用，不改变模块注册、Schema 就绪或启动顺序。

**The Predictable Theme Rule.** 主题只由用户选择和已保存的偏好决定，不跟随系统自动切换。存储不可用时回退到亮色且不阻断启动。

## Dark Mode

暗色主题只覆盖 `.app.dark` 中的语义 token。组件不得写死亮色值，否则暗色下会出现亮块。覆盖范围包括：

- 工作区、表面、表头、悬停、选中行和输入。
- 文字、次级文字、说明文字和边框。
- 主按钮、危险按钮、Toast、徽章、进度条和滚动条。
- 图表曲线、轴、网格线与提示层。
- 侧栏、顶栏和弹层。

`color-scheme` 随主题在 `light` 与 `dark` 之间切换，以匹配原生表单控件和滚动条。品牌标记的蓝色、侧栏当前项的白字和遮罩层保持固定，不随主题取反。

## Accessibility

- **图标**：所有 emoji 图标已替换为内联线性 SVG；装饰性 SVG 设置 `aria-hidden="true"`。
- **名称**：仅图标按钮必须带 `aria-label`；可见文本与可访问名称保持一致。
- **状态**：主题按钮暴露 `aria-pressed`；导航项暴露 `aria-current`；移动菜单按钮暴露 `aria-expanded`。
- **键盘**：导航项可聚焦并用 Enter/空格激活；弹层继续由 `openModal`/`closeModal` 控制，关闭按钮有可访问名称；焦点环使用 `--accent` 或侧栏浅蓝色，宽度至少 2px。
- **颜色**：状态同时用文字说明，不以颜色为唯一识别方式。
- **数据表**：表头保留语义和排序状态；表格在窄屏可横向滚动，不隐藏关键列。
- **动效**：保留现有短过渡；不新增连续动画或滚动计数。

## Do's and Don'ts

### Do:
- **Do** 所有页面共用 `.app` 与 `.app.dark` 的语义 token。
- **Do** 在动态 HTML 中使用 `var(--...)` 或语义 class 表达状态色。
- **Do** 使用系统字体栈和 `tabular-nums` 呈现数字、ID 与价格。
- **Do** 为图标按钮提供 `aria-label`，为装饰图标提供 `aria-hidden`。
- **Do** 保持弹层挂在 body、只允许 `.modal-overlay` 与 `.quota-drawer-overlay`，开关只经 `openModal`/`closeModal`。
- **Do** 保留现有 `AdminApp.register`、bootstrap 顺序、Schema ready 和 ReadonlyGuard。
- **Do** 让键盘聚焦状态与悬停状态同样清楚。

### Don't:
- **Don't** 加载 Google Fonts、CDN、外部图标库或任何远程资源。
- **Don't** 在组件或动态字符串中写死亮色十六进制值。
- **Don't** 使用 emoji 作为界面图标。
- **Don't** 把示意图或固定业务数字写进页面代码。
- **Don't** 为暗色模式复制第二套组件样式；只覆盖 token。
- **Don't** 用颜色作为输入、输出、并发或状态数据的唯一区分方式。
