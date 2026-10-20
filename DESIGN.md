---
name: LLMs Proxy 管理台
description: 深蓝导航与浅色工作区构成的中文运维界面，以清晰读数和紧凑表格支持日常管理。
colors:
  primary: "#3b82f6"
  primary-hover: "#2563eb"
  activity-input: "#2563eb"
  activity-output: "#087f74"
  activity-concurrency: "#355d9d"
  sidebar: "#1e293b"
  text: "#1e293b"
  secondary-text: "#64748b"
  activity-secondary: "#526174"
  muted-text: "#94a3b8"
  workspace: "#f1f5f9"
  surface: "#ffffff"
  border: "#e2e8f0"
  danger: "#ef4444"
  success: "#22c55e"
  warning: "#f59e0b"
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
rounded:
  xs: "3px"
  sm: "4px"
  md: "6px"
  lg: "8px"
  xl: "12px"
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
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    padding: "7px 14px"
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.surface}"
    rounded: "{rounded.md}"
    padding: "7px 14px"
  input:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text}"
    rounded: "{rounded.md}"
    height: "34px"
  panel:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.lg}"
    padding: "16px 20px"
  stat-card:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text}"
    rounded: "{rounded.lg}"
    padding: "16px 20px"
  client-table:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text}"
    padding: "10px 16px"
---

# Design System: LLMs Proxy 管理台

## Overview

**Creative North Star: "深蓝侧栏里的清晰工作面"**

这是一个面向中文运维工作的管理界面：稳定的深蓝导航为浅色内容区提供清楚边界，蓝色动作色标出可操作位置，内容以易扫读的数字、表格和分区面板为主。整体使用系统无衬线字体，信息密度偏高但层次直接，适合长时间查看和维护配置。

首页把全站活动、双趋势图和客户端明细放在同一条工作流中。图表中的输入、输出和并发各有颜色标记；两张图分别可获得键盘焦点，并同步同一时间桶游标；表格仍是识别并切换客户端的主要入口。全站统计保持可见，历史完成统计和费用作为次级信息折叠展示。

**Key Characteristics:**
- 深蓝侧栏与浅灰工作区形成稳定的框架。
- 明亮蓝色标识主要操作和选中状态。
- 以数字、趋势和可排序表格组织高密度运营信息。
- 系统中文字体保持各平台自然呈现。

## Colors

蓝色承担导航选中、主要操作和输入趋势；青绿区分输出，深蓝区分并发，灰阶承载背景、文字和边界。sidecar 中的八阶色带仅供色板预览，不定义新的语义色值。

### Primary
- **工作蓝** (`{colors.primary}`)：主要按钮、选中导航和关键交互状态。
- **按压蓝** (`{colors.primary-hover}`)：按钮悬停、聚焦与当前可操作文字的加深状态。
- **输入蓝** (`{colors.activity-input}`)：首页 Token 输入曲线与图例。
- **输出青绿** (`{colors.activity-output}`)：首页 Token 输出曲线与图例，和输入线保持可辨识差异。
- **并发靛蓝** (`{colors.activity-concurrency}`)：首页并发峰值曲线。

### Neutral
- **深石板** (`{colors.sidebar}`)：侧栏背景；同一深色也用于主要文本。
- **次级石板** (`{colors.secondary-text}`)：说明、单位、图表轴和辅助读数。
- **雾灰文字** (`{colors.muted-text}`)：沿用在深色侧栏文字和旧通用统计卡标签上；后者在浅底上的对比不足，不作为新浅底正文或小号说明用色。
- **首页辅助石板** (`{colors.activity-secondary}`)：首页时间、图表轴、单位和状态说明。
- **工作区浅灰** (`{colors.workspace}`)：应用内容区底色。
- **纯白表面** (`{colors.surface}`)：顶栏、卡片、面板、按钮和表单背景。
- **细线灰** (`{colors.border}`)：表格分隔、面板描边和表单边框。
- **错误红** (`{colors.danger}`)：错误和危险操作。
- **成功绿** (`{colors.success}`)：成功状态和正向额度状态。
- **提醒橙** (`{colors.warning}`)：警告状态和提醒信息。

### Named Rules
**The One Accent Rule.** 蓝色是主要交互强调色；输入、输出和并发趋势使用各自的图例颜色，不以颜色作为唯一的数据区分方式，输出线同时使用虚线。

## Typography

**Display Font:** 无独立展示字体；标题沿用系统无衬线字体栈。
**Body Font:** 系统无衬线字体栈（含 PingFang SC、Microsoft YaHei 回退）。
**Label/Mono Font:** 数字使用等宽数字特性；密钥、代码和技术值使用 SF Mono、Consolas 或系统等宽回退。

**Character:** 字形克制、紧凑，中文优先适配操作系统自带字体。数字采用表格数字对齐，便于比较读数。

### Hierarchy
- **Title** (600，16px，1.5)：顶栏标题、面板标题及对话框标题。
- **Body** (400，14px，1.5)：后台默认正文和主要表单文字。
- **Body Small** (400，13px，1.5)：表格内容与按钮文字。
- **Label** (500，12px，1.5)：表头、字段标签、状态和辅助说明。
- **Metric** (650，30px，1.2)：首页实时汇总数字；移动端缩至26px。
- **Micro Label** (400，10–11px)：图表轴、徽标和低优先级元数据。

### Named Rules
**The Readable Numbers Rule.** 计数和时间读数使用表格数字，避免刷新或对齐变化妨碍比较。

## Layout

后台采用固定宽度侧栏与弹性主工作区的横向框架。桌面侧栏宽240px，顶栏高56px，内容区通常使用24px内边距；窄屏内容区收至16px，侧栏转为可展开的覆盖层。页面内部用网格、弹性行和面板组织表单与数据表，表格在空间不足时横向滚动。

首页依次放置标题与刷新状态、四列汇总带、并排双图、客户端搜索与可排序表格，历史统计折叠在底部。窄屏汇总变为两列，图表改为单列，客户端表格保留姓名列并可横向滚动。常见间距落在4、8、12、16、20与24px节奏中。

## Elevation & Depth

界面采用轻量阴影配合边框和背景色差分层。普通卡片仅有细微底部投影，悬停时稍加强；菜单、提示和模态框以更明显的阴影表达浮层关系。阴影用于层级和交互反馈，不替代边框或状态颜色。

### Shadow Vocabulary
- **轻表面** (`0 1px 3px rgba(0,0,0,0.08)`)：常规统计卡片、面板和顶栏。
- **悬停面板** (`0 4px 12px rgba(0,0,0,0.1)`)：统计卡片与短提示的浮起状态。
- **模态层** (`0 20px 60px rgba(0,0,0,0.2)`)：聚焦的编辑对话框。

### Named Rules
**The Border Before Shadow Rule.** 静态内容先靠表面和细边框分组；更明显的阴影留给悬停或浮层。

## Shapes

基础控件使用轻微圆角，数据面板和卡片通常为8px；首页图表与客户端容器为12px。筛选标签采用胶囊形，徽标和表格标记用更紧凑的直角圆角。输入框以细边框界定，聚焦时切换蓝色边框并出现柔和外环。

## Components

### Buttons
按钮紧凑、直截了当，文字沿用页面系统字体。次级按钮为白底细边框；主要按钮使用工作蓝，悬停加深；危险按钮使用红色。常用按钮内边距为7px 14px，圆角6px；小按钮缩小内边距和字号。可聚焦控件应保留清楚的键盘焦点。

### Chips
状态徽标使用浅色背景和深色文字；紧凑标签以3–4px圆角呈现。首页趋势图例以短实线区分并发与输入，以虚线区分输出。

### Cards / Containers
统计卡和通用面板使用白色表面、浅灰描边、8px圆角与轻微阴影，内边距通常为16px 20px。旧通用统计卡标签沿用雾灰变量；首页辅助文字使用更深的首页辅助石板。首页图表容器使用12px圆角，客户端表格容器裁切内部边界并在表头下方使用分隔线。

### Inputs / Fields
表单控件为白底、浅灰边框、6px圆角，标准高度34px、水平内边距12px。聚焦时边框变为工作蓝并显示淡蓝外环；禁用或只读状态转为灰底并降低交互提示。

### Navigation
侧栏固定在桌面左侧，深蓝底色，文字和图标纵向居中排列。悬停使用较浅的深蓝底，当前项以蓝色左边线和低透明蓝底标识。移动端侧栏从左侧滑入并配合遮罩层。

### Client Activity Table
首页客户端表格是主要的对象选择控件。列标题可排序，行悬停有浅灰底色，选中行使用浅蓝底色；客户端名称可以聚焦并激活。窄屏表格横向滚动，首列固定以便持续辨认客户端。

### Activity Charts
双图共享时间范围和同一时间桶游标；每张图分别可获得键盘焦点，悬停或在当前聚焦图上使用方向键时，两张图同步显示对应时间桶。并发曲线使用靛蓝，输入使用蓝色，输出使用青绿虚线；读数始终包含时间段和单位。

## Do's and Don'ts

### Do:
- **Do** 使用深蓝侧栏、浅色内容区和蓝色主要操作色维持页面框架。
- **Do** 在浅色背景上使用首页辅助石板呈现小号说明，避免沿用对比不足的旧雾灰标签色。
- **Do** 通过文字、单位和图例说明数据含义，不只依赖颜色。
- **Do** 为数据表保留清楚的列对齐、分隔线和可见排序状态。
- **Do** 在窄屏缩小间距并保留表格横向浏览能力。
- **Do** 让键盘聚焦状态与悬停状态同样清楚。

### Don't:
- **Don't** 用大面积饱和色替代浅色工作区。
- **Don't** 让颜色成为输入、输出或并发数据的唯一识别方式。
- **Don't** 用持续动画或滚动计数干扰实时读数。
- **Don't** 把全站样式规则误当成要求其他页面采用首页图表布局。
