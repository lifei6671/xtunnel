# XTunnel Interface Design System

> 本文件记录已在 Dashboard 与 M5-09 链路工作台中验证的项目级界面模式。
> 新页面应先复用这些规则；确有新的业务语义时再扩展，不为一次性页面增加平行风格。

## 1. 方向与感受

XTunnel 是给运维人员和开发者使用的网络控制台。操作者通常正在确认一条链路是否可用、部署 Connector、配置 Service，或处理凭据与并发冲突；界面必须让资源归属、当前状态和下一步动作一眼可见。

- 感受：冷静、精确、可信，像网络控制室与拓扑蓝图，而不是通用 SaaS 后台。
- 密度：中等偏密；运行数据紧凑，危险操作保留足够留白。
- 语气：直接说明 Server 权威事实和操作后果，不用营销化文案。
- 主结构：持续页面以轻边框和微弱表面差构建层级；阴影只用于 Dialog、Toast 等瞬时浮层。
- 动效：只使用约 `120ms` 的颜色、边框和背景过渡；不使用弹跳、弹簧或装饰性运动。

## 2. 产品领域

### Domain concepts

- Tunnel：Token、Connector、Service 的唯一管理边界。
- Control Chain：Server → Tunnel → Connector → Service 的控制链。
- Connector Session：实时、临时、只读的运行态连接。
- Service Route：Origin、Public Exposure、Health Check 组成的路由配置。
- Revision：期望配置与 Connector 已观察配置的同步标尺。
- Health Signal：ONLINE、READY、DEGRADED、ORIGIN_UNHEALTHY 等 Server 权威状态。
- Credential Lifecycle：Reveal、Rotate、Revoke 及一次性 Secret 展示。
- Concurrency Fence：ETag、generation 和 Tunnel 归属共同防止错资源操作。

### Color world

- 控制室深海军蓝：登录、控制链背景与高对比环境。
- 拓扑蓝：主要操作、当前导航、选中链路。
- 信号青：网络链路和实时信号的辅助强调。
- 健康绿：ONLINE、READY 和操作成功。
- 告警琥珀：DEGRADED、同步中和需注意状态。
- 故障红：撤销、删除、APPLY_FAILED 与错误反馈。
- 冷白与雾灰：持续工作区、边界和次级信息。

### Signature

项目签名是“链路工作台”：左侧选择 Tunnel，右侧沿操作顺序呈现
`01 · DEPLOY → 02 · ROUTE → SECURITY BOUNDARY`。它必须同时出现在以下可识别位置：

1. Tunnel 资源轨道与选中信号。
2. Connector / Service / Active / Revision / Last Seen 链路摘要。
3. 带编号和英文微标签的操作阶段标题。
4. Connector Runtime 卡片中的平台、Revision、容量与 Ack 状态。
5. Service 行中的协议、Origin、Exposure 和运行态。
6. 独立的凭据与 Tunnel 生命周期安全区。
7. 一次性 Credential Dialog 中按部署环境排列的命令。

### Rejected defaults

- 通用“侧栏 + 指标卡片 + 数据表” → 以 Tunnel 为上下文的链路工作台。
- 独立的“新增 Connector”表单 → 展示同一 ACTIVE Token 的部署方式，Connector 保持只读运行态。
- 无归属关系的 CRUD 表格 → 在一条控制链中并置资源、配置和 Runtime。
- 页面长期展示 Secret → 只在一次性 Dialog 中展示，关闭即清除。
- 浏览器自行推导健康状态 → 逐字渲染 Server 权威状态。
- 冲突后在旧表单原地重试 → 关闭旧上下文、刷新并要求重新打开。

## 3. 基础 Tokens

实际 CSS 变量以 `web/src/styles.css` 为当前实现来源；新样式优先引用变量，不散落新的无语义颜色。

### Typography

- UI：`"Segoe UI Variable", "Microsoft YaHei", "PingFang SC", sans-serif`。
- ID、Revision、协议标签、命令：`Consolas, "Microsoft YaHei", monospace`。
- 页面标题：紧凑字距、明确字重；不要用超大营销标题。
- 微标签：约 `0.62rem–0.72rem`、较高字重、适量字距，使用英文表示系统层级。
- 正文与说明：约 `0.76rem–0.88rem`，行高约 `1.5`。

### Core palette

- Primary：`#3563e9`
- Primary dark：`#2852cf`
- Primary soft：`#edf3ff`
- Signal：`#18a8c7`
- Signal soft：`#e8f8fb`
- Ready：`#208a58`
- Warning：`#b86c16`
- Danger：`#bd4141`
- Ink：`#263044`
- Muted：`#768196`
- Border：`#e3e8f0`
- Canvas：`#f4f6f9`
- Persistent surface：`#ffffff`
- Rail surface：`#f9fafc`
- Dark control surface：`#0c1830`

颜色只表达品牌、选择、状态或危险。不要用渐变装饰工作台；深色登录与控制链背景已有的细网格是网络拓扑语义，不应复制到普通内容卡片。

### Spacing and geometry

- 基准单位：`0.25rem`（4px）。布局优先使用 8、12、16、20、24px 的倍数。
- 页面主要间距：`1rem`。
- 持续面板圆角：约 `0.4rem`。
- 标准控件圆角：约 `0.32rem`；图标按钮约 `0.28rem`。
- Dialog 圆角：约 `0.42rem`；移动端 Bottom Sheet 顶部圆角 `0.5rem`。
- 标准按钮最小高度：`2.25rem`，横向 Padding `0.75rem`。
- 图标按钮：`2rem × 2rem`。
- Focus Ring：`3px solid rgb(53 99 233 / 22%)`，Offset `2px`。
- 持续布局采用 `1px` 冷灰边框，不用明显卡片阴影。

## 4. 页面骨架

### App Shell

- Topbar 高度 `3.5rem`，使用 Primary 蓝，承载品牌、当前上下文和管理员会话。
- 桌面 Sidebar 宽 `13.5rem`；当前项使用 Primary Soft 背景和左侧 `0.2rem` 信号条。
- 未实现导航必须禁用并明确显示“待接入”，不能伪造空页面。
- 主页面必须有 Breadcrumb、明确页面标题和一句当前权威/操作目标说明。

### Tunnel Workbench

- 桌面为 `13–17rem` Tunnel Rail + 自适应 Canvas。
- Rail 只负责资源切换、数量和运行信号；详情与写操作留在 Canvas。
- Canvas 顺序固定为：Tunnel Header → Chain Summary → Deploy → Route → Security Boundary。
- 操作顺序不得按后端 Endpoint 顺序或字母顺序重排。

### Responsive behavior

- `≤1080px`：Chain Summary 从 5 列收敛为 4 列加整行 Last Seen；Service 行压缩为两层。
- `≤820px`：Workbench 变为单列，Tunnel Rail 位于上方且限高；Connector Card 单列。
- `≤600px`：Toolbar 与阶段标题纵向排列，Chain Summary 两列，Service Route/Runtime 独占行。
- `≤600px` 的 Dialog 变为底部 Sheet，宽度 100%，最大高度 `94vh`。
- 页面根节点不得产生横向滚动；导航需要时可在自身区域内横向滚动。

## 5. 可复用组件模式

### Primary Button

- Height：最小 `2.25rem`
- Padding：`0.45rem 0.75rem`
- Radius：`0.32rem`
- Font：约 `0.72rem`、`650`
- Color：Primary 背景、白字；Hover 使用 Primary Dark
- Motion：`120ms ease`

### Secondary / Load More Button

- 与 Primary Button 同尺寸。
- 白色背景、冷灰边框、深灰文字。
- Hover 只加强边框并切换为 Primary Dark 文字，不抬升卡片。

### Icon Button

- Size：`2rem × 2rem`
- Radius：`0.28rem`
- 默认中性；危险动作只在 Hover 或明确危险区中使用红色。
- 必须有可描述具体资源和动作的 `aria-label`。

### Tunnel Rail Item

- 整行 Button，包含状态点、名称、Connector/Service 数量和 Chevron。
- 选中项使用 Primary Soft、浅蓝边框及 `aria-pressed`。
- 切换时立即把旧 Workspace 置为 Loading，迟到响应不得覆盖新 Tunnel。

### Chain Summary

- 数据项使用边框分隔，不拆成带阴影的独立指标卡。
- 标签和说明使用 Muted；数值使用高对比文本。
- ID、Revision、时间等机器值使用 Monospace。
- 所有状态和计数直接来自 Server，不在浏览器重算。

### Workbench Section Heading

- 第一行使用 `NN · VERB` 微标签，例如 `01 · DEPLOY`。
- 第二行是具体动作标题，第三行解释业务边界或下一步。
- 主操作位于右侧；移动端移至标题下方并左对齐。

### Connector Runtime Card

- 仅呈现实时 Session，不提供离线历史或持久化 CRUD。
- Header：Hostname、Connector ID、Server 状态。
- Body：Platform、Version、Observed Revision、Active/Idle Capacity。
- Footer：Config Ack 状态与 Last Heartbeat。

### Service Route Row

- Identity：协议标签、名称、Service ID。
- Route：Origin 为主信息，Exposure 为次信息。
- Runtime：Server 状态、Healthy Connector 和 Active Connection。
- Actions：编辑、启停、删除使用紧凑图标按钮；删除必须进入确认 Dialog。

### Segmented Control

- 用于少量互斥协议或类型选择，不使用原生 Select。
- 每个按钮必须提供 `aria-pressed`，提交期间统一禁用。
- Active 使用白色表面、Primary Dark 文本和极轻阴影；控件整体使用冷灰底。

### Dialog

- Desktop 居中，Mobile 变为 Bottom Sheet。
- 必须设置 `role="dialog"`、`aria-modal`、可访问标题、焦点陷阱与焦点恢复。
- 表单初始聚焦第一个业务字段；危险确认初始聚焦“取消”。
- `Escape` 和遮罩可关闭，但 Mutation Busy 期间必须拒绝关闭。
- 浮层可使用阴影；持续页面仍保持边框主导。

### Feedback Toast

- 固定在页面右上区域，层级高于 Dialog Backdrop，确保弹窗失败可见。
- Success 使用健康绿语义；Error 使用故障红语义。
- 使用 `role="status"` 或 `role="alert"`，不能只靠颜色传达。

## 6. 安全与并发也是交互设计

- Secret 只进入一次性 Credential Dialog 的 React State；关闭后从 DOM 清除。
- Secret 不得写入 URL、LocalStorage、SessionStorage、日志、截图 Fixture 或错误文本。
- “添加 Connector”表示读取同一枚 ACTIVE Token 的部署指引，不表示创建数据库 Connector。
- 所有 Mutation 使用内存 CSRF、同源 Origin 与 Server 详情响应头的原始 ETag。
- 不从 Version 推导 ETag，不解析 opaque Pagination Token。
- Service 编辑、启停和删除在打开操作上下文时读取最新详情与 ETag，并绑定该快照。
- `412` 必须关闭旧 Dialog、刷新 Workspace，再要求操作者重新打开；不要让旧 ETag 原地重试。
- Workspace、分页、Token Reveal 和动作前 GET 必须按 Tunnel ID / generation 围栏；迟到响应静默丢弃。
- `204` 成功按 HTTP Status 判断，不按空 Body 判断。

## 7. 状态完整性

每个新数据区域都必须设计并实现：

- Loading：明确说明正在读取哪个资源，使用 Live Region。
- Empty：解释为什么为空以及下一步动作。
- Error：显示可操作消息；网络失败不得产生未处理 Promise。
- Disabled：说明依赖尚未接入或当前资源不可操作。
- Stale / Conflict：关闭旧操作上下文并刷新，不静默覆盖。
- Busy：禁用会改变待提交值的控件，并阻止关闭正在提交的 Dialog。

Server 返回的状态文本是事实源。前端只负责映射为稳定中文标签、语义色和下一步提示，不自行重新判定业务状态。

## 8. 新页面验收清单

- 页面移除产品名后，仍能从 Tunnel、Revision、Route 和链路阶段识别这是网络控制台。
- 至少五个实际组件体现“链路工作台”签名，而不是只在标题中出现。
- 持续页面层级在眯眼观察时仍清晰，但边框和颜色不抢占注意力。
- 新颜色能映射到 Core Palette 或明确的新业务语义。
- 所有控件具备 Default、Hover、Focus、Disabled；数据具备 Loading、Empty、Error。
- Desktop、820px、600px 和 390px 下无页面级横向溢出。
- Dialog 键盘循环、Escape、焦点恢复和 Busy 边界通过。
- Secret、ETag、Opaque Token 与跨 Tunnel 迟到响应边界通过。
- Browser Console 无 Error/Warning；Storage 中无 Credential。

