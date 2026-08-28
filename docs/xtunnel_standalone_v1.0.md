# XTunnel Standalone V1.0 规划契约

> **状态：规划，尚未实现**
>
> 本文档只记录 V0.1 之后的候选产品方向与类型化扩展边界。它不是已发布能力清单，不改变
> `docs/xtunnel_standalone_v0.1.md` 的当前实现范围，也不能替代 Proto、OpenAPI、JSON Schema
> 或 Migration 的机器权威。

---

# 1. 边界声明

V0.1 仍只以 HTTP/HTTPS/TCP Origin、HTTP Host/Path Route 和 Public TCP Port Route
构建数据面。SSH 可以作为 Raw TCP 字节流穿透，但这只证明 L4 转发能力，不表示
XTunnel 已提供基于 Hostname 的 SSH 应用、身份策略、客户端登录、会话审计或 Bastion
体验。RDP 和 SMB 同理：Raw TCP 能承载协议字节，不等于已实现正式 Zero Trust 应用。

V1.0 候选能力必须通过各自的 Task、机器契约、安全评审和 E2E Gate 后才可转为“已支持”。

---

# 2. 类型化扩展原则

Service、Origin、Route 和 Access 扩展继续遵守：

- 使用受控 Enum 和按类型分支的 Message/Schema，不使用通用 JSON、`map<string, any>`
  或不受约束的“扩展字段袋”绕过 Protocol Review。
- 每个 `proxy_type` / Origin Scheme 只允许自己的字段组合；其他分支字段在输入边界快速失败，
  不得静默忽略。
- Server Desired State 仍是唯一业务配置来源；Agent 不增加本地 Service/Origin/Access
  配置文件。
- 新的 Secret、CA、Client Key 或 Bastion Credential 必须使用专用 Secret 引用与脱敏边界，
  不得直接进入日志、错误、普通 GET Response 或通用配置 Blob。

---

# 3. Proxy Type 计划

`proxy_type` 与 Origin Scheme 是两条独立轴线。HTTP、HTTPS、TCP、UNIX 和 UNIX+TLS
描述如何连接 Origin，不得写入 `proxy_type`。V1.0 候选 `proxy_type` 只表达客户端代理模式：

```text
NONE
SOCKS5
```

`NONE` 是普通发布模式；`SOCKS5` 只允许出现在明确支持客户端代理的应用类型中，例如未来
基于 Hostname 的 SSH/RDP 访问。SOCKS5 不是 TCP Origin 上的自由文本开关，也不能改变
Origin Scheme。它需要单独的类型化 Proxy Contract，明确认证模式、DNS 解析所有权、目标地址
约束、UDP Associate 是否支持及错误映射。这些细节在未来 Protocol/OpenAPI Review 中冻结，
本文档不预分配 Wire 字段号。

---

# 4. Hostname-based 应用计划

V1.0 记录以下正式应用体验的候选方向：

```text
SSH by Hostname
RDP by Hostname
SMB by Hostname
```

它们不等同于 V0.1 的 `public_port -> raw TCP` 映射。实现前至少需要分别冻结域名入口、
客户端发现/连接流程、身份与授权结果、协议与 Route 的绑定、会话生命周期和可审计事件。
在这些契约冻结前，UI 和文档不得把 Raw TCP 包装成已交付的 SSH/RDP/SMB Zero Trust 应用。

---

# 5. Origin 类型扩展

## 5.1 UNIX 与 UNIX+TLS

V1.0 候选支持 UNIX Domain Socket Origin 和在其上建立 TLS 的 UNIX+TLS Origin。两者都必须使用
类型化路径字段，只接受明确范围内的绝对文件系统路径；不接受伪 Host/Port，不把 Linux
Abstract Namespace 与普通路径混为同一语义。

UNIX+TLS 仍须按 TLS Origin 边界明确 Server Name、证书校验和 Secret 引用，不得因底层不是
TCP 就默认关闭校验。

## 5.2 Bastion

Bastion 是显式的多阶段连接策略，不是把“跳板地址”塞进 Origin Host。未来契约需要区分
Bastion Endpoint、目标 Origin、Credential/Secret Reference、Host Key/Certificate Policy、每阶段 Timeout
和错误归属。是否允许多跳、支持哪些协议、如何审计，必须在独立安全评审中冻结。

---

# 6. TLS 与 HTTP Origin 扩展

V1.0 候选增加：

- 独立 `tls_timeout`：与 DNS/TCP Connect 预算分开，但必须同时定义外层总 Deadline，
  避免阶段 Timeout 叠加后无界延长请求。V0.1 仍使用 DNS/TCP/TLS 共享的
  `connect_timeout`。
- Custom CA：使用受控 CA Bundle/Reference，明确叶子证书链、Server Name 和更新语义，
  不用 `tls_verify=false` 替代。
- mTLS Origin：使用独立 Client Certificate/Private Key Secret Reference，定义轮换、原子发布、
  错误脱敏与最小权限。
- HTTP/2 Origin：必须明确 ALPN、h2/h2c 边界、Stream 与 WorkConn 复用模型、流控、取消和全局
  连接预算。它不是打开 `http2=true` 就自动满足的布尔开关。

---

# 7. HTTP 内容压缩与隧道压缩扩展

V0.1 只透明传递客户端与 Origin 已协商的 `Content-Encoding`。客户端已有的
`Accept-Encoding` 可以到达 Origin，Origin 返回的 gzip/Brotli 响应也会保持编码原样返回；
Server 禁用的是 Go Transport 自动添加 gzip 和自动解压，不代表 Agent 已提供主动压缩。
Agent 在 OPEN 成功后仍按 RAW 字节流转发，不解析或改写 HTTP Header/Body。

V1.0 必须把下面两类能力建模为彼此独立的类型化策略，不能合并成一个含义模糊的
`compression=true`：

```text
HTTP Response Compression
Tunnel Stream Compression
```

本文档不预分配 Proto 字段号，也不冻结具体默认值；实现前必须先完成独立 Protocol、
OpenAPI、Migration 与安全评审。

## 7.1 Agent HTTP Response Compression

该能力允许 Agent 在解密 HTTPS Origin 响应或读取 HTTP Origin 响应后，依据客户端请求和
Service 策略流式生成新的 `Content-Encoding`。它只适用于 HTTP/HTTPS Service；TCP、SSH、
RDP、SMB 和其他 RAW 数据面不得接受或静默忽略这些字段。

未来类型化契约至少需要区分：

- 关闭/透明透传/自动协商三种策略，默认候选保持关闭或透明透传；具体 Enum 名称和默认值
  由未来 Protocol Review 冻结。
- 允许的 Codec，例如 gzip、Brotli，以及各 Codec 的受控压缩级别；不得接受任意算法名称
  或直接透传底层库参数。
- 可压缩 MIME 类型、最小 Body 大小、CPU/内存预算和并发上限。
- 不可转换条件，包括已有 `Content-Encoding`、`Cache-Control: no-transform`、Range/206、
  无 Body 状态码、HEAD、WebSocket Upgrade 和其他不能安全改写的响应。

Agent 必须完整遵守 `Accept-Encoding` 的编码列表、质量值和禁止项。客户端未接受某个编码时
不得强制使用；选择编码后必须正确维护 `Content-Encoding`、`Vary: Accept-Encoding`、
ETag/缓存语义、Trailer 和连接复用状态。流式压缩不得为了计算 `Content-Length` 整体缓存
响应；HTTP/1.1 下应使用合法的流式消息边界，并保持 Backpressure、Flush、Context Cancel、
Deadline 与连接关闭可传播。

自动压缩请求 Body 不属于本扩展。请求压缩会改变 Origin 应用输入语义，未来如需支持，必须
建立独立的显式 Opt-in 契约，不能由响应压缩策略顺带开启。

实现该能力意味着 Agent 从当前 RAW Proxy 增加 HTTP-aware 数据路径。解析器必须正确处理
同一 WorkConn 上顺序复用的多个 HTTP/1.1 Request/Response，并证明不会把前一响应的状态、
字典或尾部字节污染下一请求。无法安全解析或转换时应保持原响应或按冻结错误契约失败，
不得发送部分改写响应后静默回退。

## 7.2 Agent 与 Server 之间的 Tunnel Stream Compression

Tunnel Stream Compression 的目标是降低 Agent 与 Server 之间的链路流量，它不是 HTTP
`Content-Encoding`，也不能通过改写 HTTP Header 假装实现。V0.1 的 OPEN_OK 之后是无额外
Frame 的 RAW 字节流；任意一端单独插入 gzip/Brotli 都会直接破坏 Wire 数据。

未来实现必须在 Protocol vNext 中显式协商双方支持的 Codec、版本、方向和资源上限，并在
进入 RAW 前完成一致提交。未协商、版本不匹配或任一端不支持时必须保持未压缩或按冻结契约
失败，不能依赖启发式探测。压缩流需具备明确的结束、取消、Half-Close、损坏检测、解压上限、
背压和 exactly-once 资源释放语义；不能跨 Tunnel、Service、用户或连接复用压缩字典。

该路径还必须防御压缩炸弹、极端压缩级别造成的 CPU 饥饿，以及攻击者可控输入和响应 Secret
共享压缩上下文形成的侧信道。安全评审需要明确哪些认证/Access 响应禁止压缩，并为每个方向
设置压缩前后字节、压缩率、CPU 时间、错误和主动降级指标。

## 7.3 推荐所有权

- 只降低 Origin 到 Agent 的流量：优先在 Origin 原生启用 HTTP gzip/Brotli；V0.1 已可透明
  传递结果。
- 只降低公网客户端流量：优先由 Caddy/Nginx 或 Server HTTP 层负责，避免把普通 Agent
  变成 HTTP 转换节点。
- 需要降低 Agent 到 Server 的链路流量：使用独立 Tunnel Stream Compression，不与
  `Content-Encoding` 配置混用。
- 明确要求 Agent 根据客户端能力动态改写响应：使用 Agent HTTP Response Compression，
  并接受其 HTTP-aware 解析、缓存和安全边界。

---

# 8. Access 策略规划

Access 是 V1.0 的独立产品与安全边界。当前参考截图中 Access 区域处于折叠状态，没有可见字段；
因此本文档只保留“需要类型化 Access Policy”这一扩展点，不发明具体字段、默认值、
策略表达式、身份提供商或决策优先级。

未来 Access Contract 必须先明确保护对象、身份输入、决策输出、失败模式、缓存/撤销语义、
审计事件和管理员防锁死路径，然后在 Proto/OpenAPI/Schema 中定义具体类型。

---

# 9. 交付门禁

任一上述能力进入实现前，都必须：

1. 在开发计划创建明确 Task ID、依赖、产物和失败分支。
2. 先更新所属 Proto/OpenAPI/JSON Schema/Migration 机器权威，再更新 Runtime 与 UI。
3. 完成协议歧义、SSRF/Egress、Secret、权限、资源预算和取消/关闭顺序评审。
4. 用真实数据面 E2E、Race、故障注入和跨平台证据证明能力，不把 Schema 可解析或文件存在
   冒充 Gate 通过。
5. 内容压缩需覆盖协商质量值、禁止转换、已压缩响应、Range、SSE、WebSocket、缓存键、
   1GB 流式常量空间、取消和压缩侧信道；隧道压缩需覆盖跨版本协商、双向流、Half-Close、
   损坏/解压上限、CPU/内存硬预算和无字节污染回退。

在这些门禁完成前，V1.0 文档始终是规划来源，不是产品已实现证据。
