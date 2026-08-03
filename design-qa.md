# codex-carpool 正式面板设计 QA

- 已确认原型：`C:\Users\10491\.codex\generated_images\019f9851-53ff-7141-bb13-867af8c68b9a\call_ZlgwdKFSYPt8yFqwSx8Q81mA.png`
- 正式前端截图：`C:\Users\10491\Desktop\codex-carpool\design-assets\ui-qa\design-qa-green-main.jpg`
- 原型对照图：`C:\Users\10491\Desktop\codex-carpool\design-assets\ui-qa\design-qa-comparison.jpg`
- 验收视口：1536 × 1024
- 验收状态：中文浅色、英文暗色

## 视觉与交互结果

- 四张额度概览卡、Key 列表、单 Key 用量分析、绿色官方账号池和日志区保持已确认的结构层级。
- Key 行提供编辑、重置和删除操作；模型策略只显示摘要，悬浮提示保留完整清单。
- Key 操作列固定保留 18% 宽度，模型策略列收敛为 26%；中文与英文的编辑、重置、删除按钮均完整位于表格和选中行内。
- 单 Key 分析包含环形进度、请求/成功率/结算指标、轻量 Canvas 折线趋势和访问时段。
- 官方账号池使用独立翡翠绿色视觉层级，刷新额度与配置账号池按钮均位于卡片内部。
- 日志默认每页 10 条；两个日志视图通过 Tab 切换，同一时刻只显示一套筛选栏和分页。
- 两类日志的所有数据列均固定为单行展示，超出列宽时使用省略号；账号、决策、请求内容及说明等完整值通过悬浮提示保留。
- 使用与策略日志的 11 列、插件运行与错误日志的 5 列均支持拖动调整列宽，也支持键盘左右键微调；宽度按浏览器本地持久化，刷新后保持。
- 使用与策略日志增加独立“查看”操作；详情弹窗完整展示时间、Key、模型、决策、Token、HTTP、账号、原因代码、请求内容和说明，长文本保留换行并支持滚动阅读。
- Key 列表最多展示约 5 行并使用吸顶表头；账号池概览固定，账号卡片列表独立滚动，数据增加不会继续撑高左右卡片。
- 中文页面未出现乱码、逐字插入或 `至ken`；英文页面与暗色主题均可正常渲染。
- 原紫色主题已完整替换为低饱和翡翠绿色，包含按钮、状态、图表、进度环、账号池及暗色主题变量；未残留紫色色值。
- 页面无外部图标 CDN，所有图标来自本地 SVG sprite。

## 性能与边界

- 折线图使用原生 Canvas，不引入第三方图表库。
- 模型清单与日志均采用有界渲染；既有请求批处理和观察器边界保持不变。
- 本次只修改正式前端与对应静态测试，没有修改额度引擎、SQLite 数据、CPA 路由或宿主接口。
- 浏览器验证未发现 P0、P1 或 P2 视觉与交互问题。
- JavaScript 语法解析、重复 ID、外部脚本、日志单筛选栏、双语安全替换和横向溢出检查均通过。
- 日志详情弹窗已完成浏览器交互回归：打开、完整内容绑定和上下两个关闭入口均通过；验收截图为 `C:\Users\10491\Desktop\codex-carpool\design-assets\ui-qa\design-qa-log-detail-dialog.png`。
- 日志单行省略、完整悬浮提示、决策徽标裁切、两类表格列宽调整及刷新后保持均已完成浏览器回归；验收截图为 `C:\Users\10491\Desktop\codex-carpool\design-assets\ui-qa\design-qa-log-resizable-columns.png`。
- Key 操作列已按中英文分别做边界测量，按钮均未超出单元格或表格容器；验收截图为 `C:\Users\10491\Desktop\codex-carpool\design-assets\ui-qa\design-qa-key-actions-contained.png`。
- 使用 18 个 Key 与 12 个账号完成压力布局验证：Key 列表与账号列表均产生内部滚动，左右卡片高度保持一致，页面无横向溢出。
- 当前 Windows 沙箱无法启动 Go 测试进程；Linux 构建测试仍应作为发布前的最终门禁。

final result: passed
