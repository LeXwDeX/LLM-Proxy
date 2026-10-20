---
version: 1
slug: "internal-admin-ui-js-overview-js"
primary_target: "internal/admin/ui/js/overview.js"
related_targets: ["internal/admin/ui/index.html"]
---

# 首页实时活动

Mode: Operate. Scope: 管理后台首页及其只读数据接口，其他业务页保持现状。用户需要先看全站，再定位当前客户端与最近五分钟用量；已有72小时完成统计和历史费用退居辅助区。用户选择直接制作交互页面；已确认客户端表格优先，上方汇总与双曲线，点选客户端联动。

## Direction contract

THESIS: 在同一工作面把当前请求、五分钟用量和具体客户端联系起来；用真实趋势与可排序明细承担内容，不用静态日志占据首页。

OWN-WORLD: 沿用深蓝侧栏、浅色工作区、系统中文字体和现有按钮控件；首页改为紧凑统计带、对齐双曲线和客户端表格，输入/输出用有明确图例的不同颜色与线型。

STORY: 操作者先读全站在途和用量，再点客户端，双曲线立即切换到该客户端；全站数字始终保留，返回全站动作清楚。读数有时间、单位和采样状态。

FIRST VIEWPORT: 保留现有侧栏和顶部导航。内容顶端为「实时活动」、更新时间、刷新；下方一条水平汇总带。中间两张等高曲线并排，共用五分钟时间轴，标题明确「并发峰值 / 10秒」与「Token / 10秒」。客户端搜索和可排序表格紧随曲线，首屏能看到明细起点；旧统计在底部折叠辅助区。手机纵向排曲线，指标两列，表格保留姓名与核心读数。主要动作是点选客户端，控件置于影响范围上方。

FORM: 客户端联动的汇总—双曲线—明细工作面，候选4；surface seed 52f92403，dealt 4/7/6。用户已确认第一种布局。标志交互：悬停/聚焦任一曲线的时间点，两图同步显示该时间段读数；点选表格保留行焦点并联动图表。刷新数字无滚动计数和整页闪烁；范围切换仅有短暂、尊重减少动画设置的淡入。

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance

## Data and states

新接口 /admin/api/activity 每5秒获取；历史 /admin/api/overview 低频获取，保留原合同。当前请求客户端仅计有在途请求的名称；WebSocket会话单列。最近五分钟Token为已记录的输入（含缓存和独立工具输入）与输出，非估算实时生成量。缺少采样的并发曲线断开，首次加载、零活动、接口失败和部分采样区分。失败保留上次数据和更新时间，短异常提示可见。

解释长文统一收进信息图标，可悬停/聚焦/触屏查看；role=tooltip不拦截下层命中，不含交互内容。安全沿用会话权限，不回显任何keys。

## Validation and unresolved choices

桌面1440×1000和手机390×844同批浏览器检查：曲线、筛选排序、键盘读数、tooltip跨行命中、触屏、自动刷新暂停恢复、错误和零值。没有待用户提供的数据或素材。用户已确认布局，无待决的实施选择。
