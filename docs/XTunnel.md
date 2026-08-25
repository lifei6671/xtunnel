# XTunnel 自研集中式反向隧道平台技术方案 V1.0

> [!WARNING]
> 本文是历史架构评审稿，不是当前实现或开发契约。Agent Bootstrap、Connection Token、Protocol、部署和里程碑均以 [`xtunnel_standalone_v0.1.md`](./xtunnel_standalone_v0.1.md) 与 [`xtunnel_standalone_v0.1_development_plan.md`](./xtunnel_standalone_v0.1_development_plan.md) 为准；本文中的旧 Connector/Agent 本地配置、Token 描述、`xtunnel-agent install` 等旧安装命令以及“Windows Service 后续再集成”的判断不得用于实现或验收。当前公开入口为 `xtunnel-agent run`、`xtunnel-agent service install` 与 `xtunnel-agent service uninstall`，V0.1 Agent 支持 Linux systemd 与 Windows SCM。

> 文档状态：架构评审稿
> 目标状态：可进入 POC 与工程开发
> 核心语言：Go
> 核心定位：自研集中式反向隧道平台
> 核心原则：Control Plane / Data Plane 分离、Connector 主动出站、TCP + QUIC 双传输、统一 Tunnel 抽象

---

# 1. 项目目标

XTunnel 用于将位于 NAT、企业局域网、家庭网络、IDC 内网等无法直接接受公网连接的服务，通过主动出站连接安全暴露到公网。

典型链路：

```text
Public Client
      │
      │ HTTPS / TCP
      ▼
┌─────────────┐
│    Edge     │
└──────┬──────┘
       │
       │ XTunnel Data Transport
       │ QUIC 或 TLS/TCP
       ▼
┌─────────────┐
│ Connector   │
└──────┬──────┘
       │
       │ TCP
       ▼
┌─────────────┐
│   Origin    │
└─────────────┘
```

用户只需要：

1. 创建 Connector。
2. 在内网服务器安装 Agent。
3. 创建 Tunnel。
4. 配置 Origin。
5. 创建公网 Route。

内网无需：

* 公网 IP；
* NAT 端口映射；
* 防火墙开放入站端口；
* 手工配置反向代理；
* 手工管理 Tunnel 进程。

---

# 2. V1 产品范围

## 2.1 必须支持

V1 正式版必须提供：

* 多租户 Organization；
* 用户和基础 RBAC；
* Connector 管理；
* Connector Enrollment；
* Tunnel 管理；
* Tunnel Binding；
* HTTP Route；
* HTTPS 公网访问；
* TCP Route；
* WebSocket；
* TCP Data Transport；
* QUIC Data Transport；
* TCP / QUIC 自动选择；
* Connector 状态；
* Tunnel 状态；
* Origin Health；
* Edge 管理；
* Desired State；
* 配置版本和一致性；
* mTLS；
* 审计日志；
* 基础流量统计；
* Prometheus Metrics；
* Agent 自动升级基础框架；
* CLI；
* Web Console。

## 2.2 V1 不实现

以下能力明确不进入 V1：

* UDP Tunnel；
* L3 VPN；
* CIDR 私网访问；
* Client VPN；
* CDN；
* WAF；
* Anycast；
* Zero Trust 用户身份访问；
* OIDC Access Policy；
* TCP 多路复用；
* HTTP/3 Public Ingress；
* 跨 Edge 连接迁移；
* 已建立 TCP 连接的无损 HA。

---

# 3. 非功能目标

V1 的技术目标按照以下指标设计。

| 指标                          |               V1 目标 |
| --------------------------- | ------------------: |
| 单 Edge Connector            |            ≥ 10,000 |
| 单 Edge 并发 Tunnel Connection |            ≥ 50,000 |
| 单 Connector 并发连接            |             ≥ 5,000 |
| TCP Tunnel 建连附加延迟           | P95 < 20ms，不含公网 RTT |
| QUIC Tunnel 建连附加延迟          | P95 < 10ms，不含公网 RTT |
| Edge 路由查询                   |             内存 O(1) |
| 配置最终一致                      |           正常网络 < 5s |
| Connector 离线检测              |            默认 < 30s |
| Edge 离线检测                   |            默认 < 15s |
| Control Plane 故障            |     已运行 Tunnel 不受影响 |
| Edge 单节点故障                  |      新连接能够切换其他 Edge |
| 数据面热路径                      |      不查询 PostgreSQL |
| 数据面热路径                      |   不访问 Control Plane |

这些是架构容量目标，不代表第一阶段 POC 即必须达到全部数字。

---

# 4. 核心架构原则

整个项目固定以下架构约束。

## ADR-001：控制面和数据面完全分离

```text
Control Plane
    │
    │ 配置 / 身份 / Route / 调度
    │
    ▼

Edge ═════════════════ Connector
          Data
```

业务流量：

```text
Edge → Connector
```

绝不：

```text
Edge → Control → Connector
```

Control Plane 故障时，已经下发的 Tunnel 必须继续工作。

---

## ADR-002：TCP + QUIC 双数据传输

XTunnel V1 必须同时支持：

```text
TLS/TCP
+
QUIC/UDP
```

定义：

```text
TCP = compatibility baseline
QUIC = preferred high-performance transport
```

TCP 不是降级后的残缺模式，而是一等 Transport。

---

## ADR-003：Edge Control Session 固定使用 TLS/TCP

Connector 和 Edge 之间始终保持：

```text
Connector
    │
    │ TLS/TCP
    ▼
Edge Control Session
```

即使 UDP 完全不可用，系统仍可以：

* 保持 Connector 在线；
* 心跳；
* 获取 Transport 状态；
* 创建 TCP Work Connection；
* 正常建立 Tunnel。

---

## ADR-004：TCP V1 不做 Multiplex

V1：

```text
1 Tunnel Connection
=
1 TLS/TCP Work Connection
```

不自行实现：

* Stream multiplex；
* WINDOW_UPDATE；
* 自定义 Stream Flow Control；
* Stream Scheduler。

避免把 V1 演化成自研 HTTP/2。

---

## ADR-005：QUIC 每 Stream 对应一个业务连接

```text
1 QUIC Connection
    │
    ├── Stream A → TCP A
    ├── Stream B → TCP B
    ├── Stream C → TCP C
    └── Stream D → TCP D
```

QUIC 本身提供 Stream 和 Connection 两级流控以及并发 Stream 限制，因此 XTunnel 不再叠加一层 multiplex protocol。

---

## ADR-006：Edge 不掌握 Origin 地址

Edge 只知道：

```text
tunnel_id
```

不知道：

```text
192.168.10.20:3306
127.0.0.1:8080
```

Origin 映射只存在于：

```text
Control Plane
+
Connector
```

从而防止被攻陷的 Edge 借 Connector 对客户内网进行任意横向访问。

---

## ADR-007：PostgreSQL 是 Desired State 唯一事实源

```text
PostgreSQL
=
Source of Truth
```

Redis、MQ、内存缓存均不得成为配置最终状态来源。

---

# 5. 总体系统架构

```text
                         ┌─────────────────────┐
                         │      Web UI         │
                         └──────────┬──────────┘
                                    │ HTTPS
                                    ▼
                    ┌────────────────────────────┐
                    │       Control Plane        │
                    │                            │
                    │ IAM / Organization         │
                    │ Connector Manager          │
                    │ Tunnel Manager             │
                    │ Route Manager              │
                    │ Edge Manager               │
                    │ Scheduler                  │
                    │ Config Reconciler          │
                    │ PKI                        │
                    │ Audit                      │
                    │ Usage                      │
                    └────────┬──────────┬────────┘
                             │          │
                        gRPC │          │ gRPC
                             │          │
                  ┌──────────┘          └──────────┐
                  │                                │
                  ▼                                ▼
          ┌──────────────┐                ┌──────────────┐
          │    Edge A    │                │    Edge B    │
          │              │                │              │
Internet ─► HTTP/HTTPS   │                │ HTTP/HTTPS   ◄─ Internet
          │ TCP          │                │ TCP          │
          │ Router       │                │ Router       │
          │ TLS          │                │ TLS          │
          └──────┬───────┘                └──────┬───────┘
                 │                               │
       ┌─────────┴────────────┐        ┌─────────┴────────────┐
       │                      │        │                      │
    QUIC/UDP              TLS/TCP   QUIC/UDP              TLS/TCP
       │                      │        │                      │
       └──────────────┬───────┴────────┴───────┬──────────────┘
                      │                        │
                      ▼                        ▼
             ┌─────────────────┐      ┌─────────────────┐
             │  Connector A    │      │  Connector B    │
             └────────┬────────┘      └────────┬────────┘
                      │                        │
           ┌──────────┼─────────┐              │
           ▼          ▼         ▼              ▼
        Web API      SSH      MySQL          Service
```

---

# 6. 核心组件

系统包含 4 个主要程序。

```text
xtunnel-control
xtunnel-edge
xtunnel-agent
xtctl
```

Web Console 单独部署。

---

# 7. xtunnel-control

采用：

```text
Modular Monolith
```

V1 不进行微服务拆分。

内部模块：

```text
internal/control/
├── auth/
├── organization/
├── member/
├── connector/
├── tunnel/
├── binding/
├── route/
├── edge/
├── scheduler/
├── config/
├── reconcile/
├── enrollment/
├── pki/
├── certificate/
├── audit/
├── quota/
└── usage/
```

职责：

* 管理租户；
* 管理用户；
* 管理 Connector；
* 管理 Tunnel；
* 管理 Route；
* 生成 Desired State；
* Edge 分配；
* Connector Enrollment；
* 证书签发；
* 设备吊销；
* RBAC；
* Audit；
* Usage 汇总。

Control Plane 不处理真实业务流量。

---

# 8. xtunnel-edge

Edge 是公网数据入口。

模块：

```text
internal/edge/
├── listener/
├── tlsmux/
├── http/
├── tcp/
├── router/
├── routecache/
├── session/
├── connector/
├── transport/
│   ├── tcp/
│   └── quic/
├── workpool/
├── proxy/
├── health/
├── metrics/
└── drain/
```

主要职责：

```text
Public HTTP/HTTPS
Public TCP
TLS termination
Route lookup
Connector Session Registry
TCP Work Pool
QUIC Connection Registry
Tunnel Channel
连接转发
统计
限流
Drain
```

---

# 9. xtunnel-agent

Connector Agent 部署在客户内网。

模块：

```text
internal/agent/
├── bootstrap/
├── enrollment/
├── identity/
├── control/
├── edge/
├── config/
├── reconcile/
├── transport/
│   ├── tcp/
│   └── quic/
├── workpool/
├── origin/
├── tunnel/
├── health/
├── metrics/
├── updater/
└── daemon/
```

职责：

* 首次注册；
* Device Identity；
* Control Plane Watch；
* Edge Assignment；
* Edge Control Session；
* TCP Work Pool；
* QUIC Session；
* Tunnel Open；
* Origin Dial；
* Health Check；
* 状态回报；
* 自动升级。

---

# 10. 网络端口

建议 V1：

| Endpoint               | Protocol | Port |
| ---------------------- | -------- | ---: |
| Control REST/gRPC      | TCP/TLS  |  443 |
| Edge Public HTTP       | TCP      |   80 |
| Edge Public HTTPS      | TCP/TLS  |  443 |
| Edge Connector Control | TCP/TLS  |  443 |
| Edge TCP Transport     | TCP/TLS  |  443 |
| Edge QUIC Transport    | UDP/QUIC |  443 |

即：

```text
Edge TCP :443

├── Public HTTPS
├── XTunnel Control
└── XTunnel TCP


Edge UDP :443

└── XTunnel QUIC
```

这样 Connector 只需要企业网络允许：

```text
TCP 443
```

即可完成基本工作。

---

# 11. TCP 443 协议分流

Agent 使用专用 SNI：

```text
connect.hk.edge.example.com
```

ALPN：

```text
xtunnel-control/1
xtunnel-work/1
```

普通公网请求：

```text
ALPN:

h2
http/1.1
```

Edge TLS Listener 在握手阶段根据：

```text
SNI
+
ALPN
```

选择配置。

Agent Endpoint：

```text
RequireAndVerifyClientCert
```

Public HTTPS：

```text
NoClientCert
```

握手完成后根据：

```go
ConnectionState.NegotiatedProtocol
```

分发给不同 Handler。

---

# 12. Connector 到 Control Plane

Agent 启动后首先连接：

```text
control.example.com:443
```

采用：

```text
gRPC bidirectional stream
+
TLS
+
Device Certificate
```

接口：

```protobuf
service ConnectorControl {
  rpc Watch(stream ConnectorEvent)
      returns (stream ControlEvent);
}
```

Agent 上报：

```text
Heartbeat
ObservedRevision
AgentVersion
EdgeStatus
TunnelHealth
TransportHealth
```

Control 下发：

```text
ConnectorSnapshot
EdgeAssignment
CertificateRotate
AgentUpgrade
Drain
```

---

# 13. Edge 到 Control Plane

Edge 同样主动连接 Control：

```protobuf
service EdgeControl {
  rpc Watch(stream EdgeEvent)
      returns (stream ControlEvent);
}
```

Edge 上报：

```text
Heartbeat
CPU
Memory
Connections
Traffic
ConnectorSessions
ActiveStreams
ObservedRevision
```

Control 下发：

```text
RouteSnapshot
EdgeConfig
Drain
Certificate
```

---

# 14. 数据传输统一抽象

整个数据面最重要的抽象：

```go
type TunnelChannel interface {
    net.Conn

    TunnelID() string
    ConnectionID() string
    Transport() TransportType
}
```

Transport：

```go
type DataTransport interface {
    Type() TransportType
    State() TransportState

    Available(tunnelID string) bool

    Acquire(
        ctx context.Context,
        req OpenRequest,
    ) (TunnelChannel, error)

    Stats() TransportStats

    Close() error
}
```

实现：

```text
TCPTransport
QUICTransport
```

上层：

```text
HTTP Proxy
TCP Listener
```

完全不需要感知 Transport 类型。

---

# 15. TCP Transport

TCP 是 V1 最基础 Transport。

架构：

```text
Connector
    │
    ├── Control Connection
    │
    ├── Idle WorkConn #1
    ├── Idle WorkConn #2
    ├── Idle WorkConn #3
    └── Idle WorkConn #N
```

所有连接均：

```text
TCP
 ↓
TLS 1.3
 ↓
mTLS
 ↓
XTunnel Protocol
```

---

# 16. TCP Work Connection

每个 Connector 为 Edge 维持一定数量空闲连接。

默认：

```yaml
transport:
  tcp:
    min_idle: 4
    target_idle: 8
    max_idle: 32
    max_connecting: 16
```

状态：

```text
CONNECTING
    ↓
AUTHENTICATING
    ↓
REGISTERING
    ↓
IDLE
    ↓
OPENING
    ↓
ACTIVE
    ↓
DRAINING
    ↓
CLOSED
```

V1 明确：

```text
ACTIVE → CLOSED
```

不允许：

```text
ACTIVE → IDLE
```

一个 Work Connection 只承载一个业务连接。

---

# 17. WorkConn 注册协议

建立 TLS 后：

```protobuf
message WorkHello {
    string connector_id = 1;
    string connector_session_id = 2;
    string work_id = 3;
    uint32 protocol_version = 4;
}
```

Edge：

```protobuf
message WorkReady {
    string work_id = 1;
    uint64 idle_timeout_ms = 2;
}
```

之后进入：

```text
IDLE
```

等待 Edge 分配业务连接。

---

# 18. TCP Tunnel 建立流程

```text
Public Client         Edge            Agent           Origin

     │                 │                │                │
     │──── TCP ───────►│                │                │
     │                 │                │                │
     │                 │ Acquire Idle   │                │
     │                 │ WorkConn       │                │
     │                 │                │                │
     │                 │──── OPEN ─────►│                │
     │                 │                │──── Dial ─────►│
     │                 │                │                │
     │                 │                │◄── Connected ──│
     │                 │◄── OPEN_OK ────│                │
     │                 │                │                │
     │◄════════════════ RAW DATA ═══════════════════════►│
```

---

# 19. TCP Work Pool 扩缩

Pool Manager 维护：

```text
idle
active
connecting
failed
```

目标：

```text
idle >= target_idle
```

当：

```text
idle < min_idle
```

Agent 主动快速补充。

Edge 在突发请求时可以通过 Control Session：

```protobuf
message WorkDemand {
    uint32 desired = 1;
}
```

Agent 最多并发：

```text
max_connecting
```

避免请求洪泛触发无限建连。

---

# 20. TCP 无可用 WorkConn

公网请求进入，但：

```text
idle = 0
```

Edge：

1. 通知 Agent `WorkDemand`；
2. 等待 `work_acquire_timeout`；
3. 超时则失败。

HTTP：

```text
503 Tunnel Capacity Unavailable
```

TCP：

```text
close connection
```

默认：

```text
work_acquire_timeout = 2s
```

可配置。

---

# 21. QUIC Transport

Connector 对 Edge 建立：

```text
QUIC / UDP 443
TLS >= 1.3
mTLS
ALPN xtunnel-quic/1
```

QUIC 规范使用 TLS 保护连接；QUIC TLS 要求不能协商到 TLS 1.3 以下版本。

V1：

```text
0-RTT = disabled
```

避免 replay 语义复杂化。

---

# 22. QUIC Session

结构：

```text
Connector
    │
    │ QUIC Connection
    ▼
Edge

├── Stream 01
├── Stream 05
├── Stream 09
├── Stream 13
└── ...
```

一个：

```text
Bidirectional QUIC Stream
```

对应一个：

```text
TunnelChannel
```

QUIC Connection Migration 可以应对客户端地址/端口改变，但它不能被视为跨 Edge Server HA 机制。RFC 9000 本身也不支持把正在运行的连接直接迁移到另一个服务器地址。

---

# 23. QUIC Flow Control

必须显式配置：

```go
quic.Config{
    InitialStreamReceiveWindow:     ...,
    MaxStreamReceiveWindow:         ...,
    InitialConnectionReceiveWindow: ...,
    MaxConnectionReceiveWindow:     ...,
    MaxIncomingStreams:             ...,
}
```

quic-go 支持 Stream 级和 Connection 级接收窗口，并会进行窗口 auto-tuning。

初始值不直接写死在协议中，通过 benchmark 决定。

建议起始测试区间：

```text
Stream window:
1MB ~ 8MB

Connection window:
16MB ~ 128MB

Max Streams:
1024 ~ 8192
```

---

# 24. Transport Manager

Agent：

```text
                   Transport Manager
                          │
              ┌───────────┴───────────┐
              │                       │
              ▼                       ▼
         TCP Transport           QUIC Transport
```

配置：

```yaml
transport:
  mode: auto
```

支持：

```text
auto
tcp
quic
```

---

# 25. auto 策略

启动：

```text
Agent Start
     ↓
Control Plane Connected
     ↓
Edge Assigned
     ↓
Edge TCP Control Connected
     ↓
TCP Work Pool Ready
     ↓
Tunnel Already Available
     ↓
后台启动 QUIC
     ↓
QUIC Healthy?
   /       \
 yes        no
 ↓           ↓
preferred    TCP only
```

因此：

> QUIC 探测失败不会阻塞 Tunnel 首次上线。

---

# 26. Transport 状态机

```text
UNKNOWN
   ↓
CONNECTING
   ↓
HEALTHY
   ↓
DEGRADED
   ↓
UNAVAILABLE
```

观测：

* handshake success rate；
* RTT；
* QUIC PTO；
* packet loss；
* recent failures；
* connection resets；
* available capacity。

切换原则：

```text
已有连接不迁移
新连接选择当前最佳 Transport
```

---

# 27. 防止 Transport 抖动

禁止：

```text
QUIC → TCP → QUIC → TCP
```

快速切换。

Transport Manager 使用：

```text
failure threshold
recovery threshold
minimum stable period
cooldown
```

例如：

```text
QUIC 连续失败 >= 3
→ DEGRADED

持续 DEGRADED >= 10s
→ 新连接走 TCP

后台每 30s probe

连续健康 >= 3 次
→ 恢复 QUIC

恢复后 cooldown = 60s
```

数值需要通过真实网络测试调整。

---

# 28. XTunnel Logical Protocol

TCP 和 QUIC 共用同一套业务语义：

```text
OPEN
OPEN_OK
OPEN_ERROR
RAW_DATA
FIN
RESET
```

只是承载方式不同。

TCP：

```text
WorkConn
```

QUIC：

```text
QUIC Stream
```

---

# 29. Frame 编码

控制消息采用：

```text
Unsigned Varint Length
+
Protobuf Payload
```

格式：

```text
+------------------+
| Frame Length     |
+------------------+
| Protobuf Payload |
+------------------+
```

限制：

```text
MaxFrameSize = 1MB
```

任何超过限制的 frame：

```text
PROTOCOL_ERROR
```

并关闭连接。

---

# 30. OpenRequest

```protobuf
message OpenRequest {
    uint32 protocol_version = 1;

    string connection_id = 2;

    string tunnel_id = 3;

    string trace_id = 4;

    string client_addr = 5;

    uint64 timestamp_ms = 6;

    uint32 flags = 7;
}
```

明确禁止出现：

```text
origin_host
origin_port
```

---

# 31. OpenResponse

```protobuf
message OpenResponse {
    string connection_id = 1;

    OpenStatus status = 2;

    ErrorCode error_code = 3;

    uint32 origin_connect_latency_ms = 4;
}
```

---

# 32. OPEN 后的数据阶段

握手成功：

```text
OPEN
↓
OPEN_OK
↓
RAW MODE
```

进入 RAW MODE 后：

> Tunnel Protocol 不再解析应用层内容。

直接转发字节。

---

# 33. Error Code

第一版：

```text
0x0000 OK

0x1001 TUNNEL_NOT_FOUND
0x1002 TUNNEL_DISABLED
0x1003 TUNNEL_NOT_READY

0x2001 ORIGIN_REFUSED
0x2002 ORIGIN_TIMEOUT
0x2003 ORIGIN_UNREACHABLE
0x2004 ORIGIN_RESET

0x3001 CONNECTOR_BUSY
0x3002 WORK_POOL_EXHAUSTED
0x3003 STREAM_LIMIT

0x4001 POLICY_DENIED
0x4002 QUOTA_EXCEEDED

0x5001 PROTOCOL_ERROR
0x5002 VERSION_UNSUPPORTED

0x6001 INTERNAL_ERROR
```

---

# 34. Connection ID

采用：

```text
ULID
```

例如：

```text
01K3...
```

一条连接从：

```text
Edge
→ Tunnel
→ Agent
→ Origin
```

始终携带：

```text
connection_id
trace_id
```

便于完整链路追踪。

---

# 35. Half-Close

这是 V1 必须解决的协议语义。

业务 TCP：

```text
Client CloseWrite
```

不能直接导致：

```text
Tunnel Close
```

需要支持：

```text
方向 A EOF
方向 B 继续发送
```

抽象：

```go
type HalfCloseConn interface {
    net.Conn

    CloseWrite() error
}
```

TCP Transport 使用 Go TLS 的 `CloseWrite()`；Go `crypto/tls.Conn` 原生提供写方向关闭语义。

QUIC：

```text
send side FIN
```

读方向继续保持。

---

# 36. Connector Origin Resolver

Connector 保存：

```text
TunnelID → Origin
```

例如：

```text
tun_01
→
tcp://127.0.0.1:3000
```

接口：

```go
type OriginResolver interface {
    Resolve(
        tunnelID string,
    ) (Origin, error)
}
```

Edge 无法指定其他地址。

---

# 37. Origin

模型：

```go
type Origin struct {
    Scheme string

    Host string
    Port uint16

    TLSEnabled bool
    TLSServerName string
    TLSVerify bool

    ConnectTimeout time.Duration
}
```

V1 Scheme：

```text
http
https
tcp
```

---

# 38. HTTP Public Ingress

使用 Go：

```text
net/http
httputil.ReverseProxy
```

核心结构：

```text
Browser
   ↓
HTTPS
   ↓
Edge
   ↓
HTTP Router
   ↓
ReverseProxy
   ↓
Tunnel RoundTripper
   ↓
TunnelChannel
   ↓
Connector
```

---

# 39. Tunnel Dialer

实现：

```go
type TunnelDialer interface {
    DialContext(
        ctx context.Context,
        tunnelID string,
        metadata ConnectionMetadata,
    ) (net.Conn, error)
}
```

内部：

```text
Tunnel
 ↓
Connector Selection
 ↓
Transport Selection
 ↓
QUIC Stream / TCP WorkConn
```

---

# 40. HTTP Route

路由模型：

```text
hostname
+
path prefix
```

例如：

```text
api.example.com/api/
```

匹配：

```text
hostname exact match
+
longest path prefix
```

禁止 V1 支持任意 regex，避免 Router 复杂化。

---

# 41. HTTP Forwarded Header

Edge 必须删除外部客户端传入的不可信：

```text
Forwarded
X-Forwarded-For
X-Forwarded-Proto
X-Forwarded-Host
```

然后重新生成：

```text
X-Forwarded-For = client IP
X-Forwarded-Proto = https
X-Forwarded-Host = incoming host
```

避免来源伪造。

---

# 42. WebSocket

不设计独立 Tunnel Protocol。

走：

```text
HTTP Upgrade
↓
Reverse Proxy
↓
长期 TunnelChannel
```

必须测试：

* 长连接；
* Client Close；
* Server Close；
* Ping/Pong；
* 网络抖动；
* Edge Drain。

---

# 43. TCP Public Route

例如：

```text
edge.example.com:10022
```

对应：

```text
Tunnel SSH
```

流程：

```text
TCP Listener
     ↓
Accept
     ↓
Route Lookup
     ↓
Connector Select
     ↓
Transport Select
     ↓
TunnelChannel
     ↓
Bidirectional Copy
```

---

# 44. Public TCP Listener Manager

动态维护：

```text
10022 → tun_ssh
13306 → tun_mysql
15432 → tun_pg
```

配置变化：

```text
Desired Listeners
      ↓
Reconcile
      ↓
Actual Listeners
```

删除 Route：

```text
Stop Accept
↓
existing connection continues
↓
connection drains naturally
```

---

# 45. Desired State

所有配置采用：

```text
Desired State
+
Observed State
```

Connector：

```text
desired_revision = 105
observed_revision = 103
```

说明：

```text
存在配置漂移
```

Agent Reconciler 应最终达到：

```text
105 == 105
```

---

# 46. Connector Snapshot

```protobuf
message ConnectorSnapshot {
    string connector_id = 1;

    uint64 revision = 2;

    repeated TunnelBinding bindings = 3;

    bytes signature = 4;
}
```

Agent 保存：

```text
/var/lib/xtunnel/config.snapshot
```

采用：

```text
write temp
fsync
atomic rename
```

保证异常断电不会留下半份配置。

---

# 47. Last Known Good

Control Plane 不可用时：

```text
Connector
    ↓
Last Known Good Snapshot
```

继续工作。

Edge：

```text
Last Known Route Snapshot
```

同理。

因此：

```text
Control Plane Down
```

不会导致：

```text
Existing Tunnel Down
```

---

# 48. Snapshot 签名

Control Plane 使用：

```text
Ed25519
```

对 Snapshot 签名。

Agent 保存 Control Config Signing Public Key。

Agent：

```text
Snapshot
 ↓
Verify Signature
 ↓
Verify Revision
 ↓
Apply
```

禁止：

```text
revision rollback
```

除非管理员使用显式 emergency rollback mechanism。

---

# 49. Edge Snapshot

Edge 获取：

```text
hostname → tunnel_id

port → tunnel_id

tunnel_id → allowed connector_id
```

没有：

```text
origin host
origin port
```

---

# 50. Edge Route Cache

请求热路径禁止查询：

```text
PostgreSQL
Control API
Redis
```

Edge 本地：

```go
type RouteSnapshot struct {
    HTTP map[string]*HostRoutes

    TCP map[uint16]*TCPRoute

    Tunnels map[string]*TunnelRuntime
}
```

---

# 51. Route Snapshot 原子更新

采用：

```go
atomic.Pointer[RouteSnapshot]
```

更新：

```text
Current Snapshot
      │
build new
      ▼
New Snapshot
      │
atomic swap
      ▼
New requests
```

旧请求继续使用旧 Snapshot 引用。

请求路径不需要写锁。

---

# 52. 数据领域模型

核心实体：

```text
Organization
│
├── User
├── Connector
├── Tunnel
│    └── TunnelBinding
│
├── HTTPRoute
├── TCPRoute
│
├── EdgeGroup
│    └── Edge
│
├── Domain
└── AuditLog
```

---

# 53. Organization

```sql
organizations (
    id UUID PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    slug VARCHAR(64) UNIQUE NOT NULL,
    status VARCHAR(32) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
```

---

# 54. Connector

```sql
connectors (
    id UUID PRIMARY KEY,

    organization_id UUID NOT NULL,

    name VARCHAR(128) NOT NULL,

    status VARCHAR(32) NOT NULL,

    os VARCHAR(32),
    arch VARCHAR(32),
    version VARCHAR(64),

    desired_revision BIGINT NOT NULL DEFAULT 0,
    observed_revision BIGINT NOT NULL DEFAULT 0,

    last_seen_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
```

状态：

```text
PENDING
ONLINE
DEGRADED
OFFLINE
DRAINING
REVOKED
```

---

# 55. Tunnel

Tunnel 是逻辑服务：

```sql
tunnels (
    id UUID PRIMARY KEY,

    organization_id UUID NOT NULL,

    name VARCHAR(128) NOT NULL,

    status VARCHAR(32) NOT NULL,

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
```

Tunnel 本身不和协议绑定。

---

# 56. Tunnel Binding

```sql
tunnel_bindings (
    id UUID PRIMARY KEY,

    tunnel_id UUID NOT NULL,
    connector_id UUID NOT NULL,

    origin_scheme VARCHAR(16) NOT NULL,
    origin_host VARCHAR(255) NOT NULL,
    origin_port INTEGER NOT NULL,

    tls_server_name VARCHAR(255),
    tls_verify BOOLEAN NOT NULL DEFAULT TRUE,

    priority INTEGER NOT NULL DEFAULT 100,
    weight INTEGER NOT NULL DEFAULT 100,

    enabled BOOLEAN NOT NULL DEFAULT TRUE,

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
```

支持未来：

```text
Tunnel
├── Connector A
└── Connector B
```

---

# 57. HTTP Route

```sql
http_routes (
    id UUID PRIMARY KEY,

    organization_id UUID NOT NULL,
    tunnel_id UUID NOT NULL,

    hostname VARCHAR(255) NOT NULL,
    path_prefix VARCHAR(1024) NOT NULL DEFAULT '/',

    edge_group_id UUID NOT NULL,

    enabled BOOLEAN NOT NULL DEFAULT TRUE,

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
```

唯一约束至少：

```text
organization_id
hostname
path_prefix
```

---

# 58. TCP Route

```sql
tcp_routes (
    id UUID PRIMARY KEY,

    organization_id UUID NOT NULL,
    tunnel_id UUID NOT NULL,

    edge_group_id UUID NOT NULL,

    public_port INTEGER NOT NULL,

    enabled BOOLEAN NOT NULL DEFAULT TRUE,

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
```

---

# 59. Edge

```sql
edges (
    id UUID PRIMARY KEY,

    name VARCHAR(128) NOT NULL,

    region VARCHAR(64) NOT NULL,

    public_hostname VARCHAR(255) NOT NULL,

    status VARCHAR(32) NOT NULL,

    weight INTEGER NOT NULL DEFAULT 100,

    desired_revision BIGINT NOT NULL DEFAULT 0,
    observed_revision BIGINT NOT NULL DEFAULT 0,

    last_seen_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
```

---

# 60. Edge Group

```text
EdgeGroup

cn-east
├── edge-sh-01
└── edge-sh-02
```

Tunnel Route 绑定：

```text
EdgeGroup
```

而不是单个 Edge。

这样后续 HA 不需要改变 Route 模型。

---

# 61. Enrollment

用户创建 Connector：

```http
POST /api/v1/connectors
```

Control 生成：

```text
connector_id
one-time enrollment token
```

安装：

```bash
xtunnel-agent install \
    --control https://control.example.com \
    --token enroll_xxxxxxxxx
```

---

# 62. Enrollment Token

必须：

```text
随机 >= 256 bit
一次性
TTL
数据库仅存 hash
使用后立即失效
```

Agent：

1. 本地生成 private key；
2. 创建 CSR；
3. 使用 Enrollment Token；
4. Control 验证；
5. CA 签发 Device Certificate；
6. Agent 保存 certificate + private key。

---

# 63. 内部身份体系

建议：

```text
Root CA
   ↓
Intermediate CA
   ├── Connector Certificate
   └── Edge Certificate
```

Root CA：

```text
offline
```

Control Plane 在线只持：

```text
Intermediate CA
```

生产环境可进一步托管到 KMS/HSM。

---

# 64. Device Identity

Certificate SAN：

```text
URI:
spiffe://xtunnel/org/{org}/connector/{connector}
```

Edge：

```text
spiffe://xtunnel/edge/{edge}
```

身份校验使用证书 URI，而不是：

```text
客户端传 connector_id 我就相信
```

应用协议里的 ID 必须和证书 ID 一致。

---

# 65. Certificate Rotation

Device Certificate 建议：

```text
短生命周期
```

例如：

```text
7 days
```

在剩余约：

```text
1/3 生命周期
```

时主动 rotate。

证书失效前 Agent 应多次重试。

---

# 66. Secret Storage

禁止数据库明文保存：

* CA Private Key；
* Certificate Private Key；
* API Secret；
* Enrollment Token；
* Service Token。

本地密钥：

Linux：

```text
0600
root ownership
```

Windows/macOS 后续集成 OS secure storage。

---

# 67. RBAC

V1：

```text
OWNER
ADMIN
DEVELOPER
VIEWER
```

权限建议：

```text
connector:read
connector:write

tunnel:read
tunnel:write

route:read
route:write

member:read
member:write

audit:read

organization:admin
```

Role 是 permission 集合。

业务代码校验：

```text
Permission
```

而不是大量：

```text
if role == admin
```

---

# 68. REST API

建议：

```text
/api/v1/
```

核心接口：

```text
POST   /organizations
GET    /organizations/:id

POST   /connectors
GET    /connectors
GET    /connectors/:id
DELETE /connectors/:id

POST   /tunnels
GET    /tunnels
GET    /tunnels/:id
PATCH  /tunnels/:id
DELETE /tunnels/:id

POST   /tunnels/:id/bindings
PATCH  /bindings/:id
DELETE /bindings/:id

POST   /routes/http
GET    /routes/http
DELETE /routes/http/:id

POST   /routes/tcp
GET    /routes/tcp
DELETE /routes/tcp/:id

GET    /edges
GET    /audit
```

使用 OpenAPI 维护接口契约。

---

# 69. API 写操作事务

例如创建 HTTP Route：

```text
BEGIN

INSERT http_route

UPDATE connector
SET desired_revision = desired_revision + 1

UPDATE edges
SET desired_revision = desired_revision + 1

INSERT config_outbox

COMMIT
```

---

# 70. Outbox Pattern

使用 PostgreSQL：

```sql
config_outbox
```

作为配置通知可靠队列。

它只解决：

```text
快速通知
```

最终一致性仍由：

```text
Revision + Snapshot Reconcile
```

保障。

即使 Outbox 消息丢失：

```text
Reconnect
 ↓
Revision Compare
 ↓
Full Snapshot
```

仍能恢复。

---

# 71. Agent Reconciler

Agent 内部：

```text
Watch Config
    ↓
receive revision 100
    ↓
validate signature
    ↓
compare local revision
    ↓
build desired runtime state
    ↓
apply
    ↓
persist snapshot
    ↓
observed_revision=100
```

必须保证 Apply：

```text
idempotent
```

重复应用同一个 revision 不产生副作用。

---

# 72. Edge Session Registry

Edge：

```text
Connector A
├── Control Session
├── TCP Pool
└── QUIC Session

Connector B
├── Control Session
├── TCP Pool
└── QUIC Session
```

抽象：

```go
type ConnectorRuntime struct {
    ID string

    Control *ControlSession

    TCP *TCPTransport

    QUIC *QUICTransport

    Health ConnectorHealth
}
```

---

# 73. Connector Selection

一个 Tunnel 可以绑定多个 Connector。

V1：

```text
priority
+
round robin
```

后续：

```text
weighted least connections
+
EWMA latency
```

选择流程：

```text
Tunnel
 ↓
Bindings
 ↓
filter enabled
 ↓
filter healthy
 ↓
min priority
 ↓
weighted selection
```

---

# 74. Origin Health

Agent 本地执行 Health Check。

类型：

```text
TCP
HTTP
```

配置：

```text
interval
timeout
failure_threshold
success_threshold
```

状态：

```text
UNKNOWN
HEALTHY
UNHEALTHY
```

Agent 将 Health 上报 Control，同时 Edge 获得可路由的健康摘要。

---

# 75. HTTP Route 失败语义

没有 Connector：

```text
503
TUNNEL_OFFLINE
```

Connector 在线但 Origin 不通：

```text
502
ORIGIN_UNAVAILABLE
```

容量不足：

```text
503
TUNNEL_CAPACITY
```

连接超时：

```text
504
ORIGIN_TIMEOUT
```

必须清晰区分，方便诊断。

---

# 76. 多 Edge

正式 HA 阶段：

```text
              DNS
               │
       ┌───────┴────────┐
       ▼                ▼
    Edge A            Edge B
       │                │
       └──────┬─────────┘
              │
          Connector
```

Agent 可同时保持：

```text
Edge A session
Edge B session
```

---

# 77. Edge Assignment

Control Plane Scheduler 下发：

```protobuf
message EdgeAssignment {
    repeated AssignedEdge edges = 1;
}
```

其中：

```text
hostname
region
priority
weight
```

Connector 不自己扫描所有 Edge。

---

# 78. Edge Scheduler

输入：

```text
Edge online
CPU
Memory
Active Connections
Active Streams
Traffic
Region

Connector RTT
Connector network
Connector region
```

输出：

```text
primary edges
backup edges
```

V1 初期可简单：

```text
static assignment
```

后续升级动态调度。

---

# 79. Edge 故障语义

Edge 突然宕机：

```text
已有连接
→ 断开
```

新连接：

```text
→ 其他 Edge
```

不承诺已存在 TCP Session 无损迁移。

这是必须写进产品 SLA 的明确边界。

---

# 80. Control Plane 故障

所有 Control 实例故障：

```text
Web Console unavailable
新配置 unavailable
```

但是：

```text
Edge last-known route
Connector last-known binding
```

继续工作。

因此：

```text
Existing Tunnel = available
```

---

# 81. Graceful Drain

Edge：

```text
ACTIVE
 ↓
DRAINING
 ↓
STOPPED
```

进入 DRAINING 后：

* Scheduler 不再分配新 Connector；
* 新业务连接优先其他 Edge；
* 已建立连接继续；
* 超过 drain timeout 后强制关闭。

Agent 同样支持 Drain。

---

# 82. Rate Limit 和 Quota

V1 至少提供：

```text
per organization:
max_connectors
max_tunnels
max_routes
max_connections

per connector:
max_active_connections

per tunnel:
max_connections
```

Edge 热路径维护本地 counter。

Control Plane 定期下发 quota snapshot。

---

# 83. 防资源耗尽

必须设置：

```text
TLSHandshakeTimeout
OriginConnectTimeout

MaxControlFrameSize

MaxHTTPHeaderBytes

MaxConnectorSessions

MaxIdleWorkConnections

MaxActiveWorkConnections

MaxQUICStreams

MaxPendingTunnelOpen

MaxPendingPublicConnections
```

所有队列都必须：

> 有上界。

绝对禁止无限：

```text
chan
buffer
goroutine
queue
```

---

# 84. Backpressure

TCP：

```text
io.CopyBuffer
```

天然受到 TCP flow control。

QUIC 则利用：

```text
Stream Flow Control
+
Connection Flow Control
```

不能：

```text
read entire request into memory
then send
```

全部使用 streaming。

---

# 85. Buffer 管理

使用：

```text
sync.Pool
```

管理有限 buffer。

例如：

```text
16KB / 32KB
```

具体大小 benchmark 决定。

禁止：

```text
每连接固定长期分配 1MB
```

避免 50k 连接导致几十 GB 内存。

---

# 86. Goroutine 模型

一个 Active Tunnel 一般允许：

```text
read pump
write pump
```

但需要：

```text
context cancel
errgroup
```

保证任一方向 fatal error 后另一方向能够退出。

不得产生 orphan goroutine。

---

# 87. 可观测性

三类：

```text
Metrics
Logs
Trace
```

技术：

```text
Prometheus
OpenTelemetry
Structured JSON Logging
```

---

# 88. Edge Metrics

低 Cardinality：

```text
xtunnel_edge_connectors

xtunnel_edge_active_connections

xtunnel_edge_tcp_idle_work_connections

xtunnel_edge_tcp_active_work_connections

xtunnel_edge_quic_connections

xtunnel_edge_quic_streams

xtunnel_edge_open_total

xtunnel_edge_open_errors_total

xtunnel_edge_bytes_rx_total

xtunnel_edge_bytes_tx_total

xtunnel_edge_transport_fallback_total
```

---

# 89. Agent Metrics

```text
xtunnel_agent_control_connected

xtunnel_agent_edge_connected

xtunnel_agent_tcp_idle

xtunnel_agent_tcp_active

xtunnel_agent_quic_connected

xtunnel_agent_active_tunnels

xtunnel_agent_origin_connect_total

xtunnel_agent_origin_connect_errors_total

xtunnel_agent_config_revision
```

---

# 90. 不要把 Tunnel ID 直接作为 Prometheus Label

否则：

```text
百万 Tunnel
→ 百万 Series
```

高维数据：

```text
organization_id
tunnel_id
route_id
```

进入 Usage Pipeline。

---

# 91. Usage Aggregation

Edge 内存维护：

```text
TunnelUsage
{
    connections
    rx_bytes
    tx_bytes
    requests
    errors
}
```

周期 flush：

```text
Edge
 ↓
Batch
 ↓
Control Usage API
```

禁止每一个 HTTP Request：

```text
INSERT PostgreSQL
```

---

# 92. Audit Log

记录：

```text
用户
时间
IP
Organization
Resource
Action
Before
After
Result
```

例如：

```text
Tunnel Create
Tunnel Delete
Connector Revoke
Route Create
Role Change
```

Audit 不记录业务正文。

---

# 93. 日志规范

必须统一字段：

```text
timestamp
level
component

organization_id
connector_id
edge_id
tunnel_id

connection_id
trace_id

error_code
```

生产日志禁止直接输出：

```text
token
certificate private key
authorization header
cookie
```

---

# 94. Repository

建议：

```text
xtunnel/
├── cmd/
│   ├── control/
│   ├── edge/
│   ├── agent/
│   └── xtctl/
│
├── api/
│   ├── proto/
│   └── openapi/
│
├── internal/
│   ├── protocol/
│   ├── transport/
│   ├── tunnel/
│   ├── edge/
│   ├── agent/
│   ├── control/
│   ├── config/
│   ├── identity/
│   ├── pki/
│   └── observability/
│
├── pkg/
│
├── migrations/
│
├── web/
│
├── deploy/
│   ├── docker/
│   ├── systemd/
│   └── helm/
│
├── docs/
│   ├── architecture/
│   ├── protocol/
│   ├── security/
│   └── adr/
│
└── tests/
    ├── integration/
    ├── e2e/
    ├── benchmark/
    └── chaos/
```

---

# 95. 技术栈

| 模块            | 技术                    |
| ------------- | --------------------- |
| Backend       | Go                    |
| Control RPC   | gRPC                  |
| External API  | REST/OpenAPI          |
| Protocol      | Protobuf              |
| TCP Security  | TLS 1.3               |
| QUIC          | quic-go               |
| Internal Auth | mTLS                  |
| Database      | PostgreSQL            |
| ORM           | 建议 sqlc / 手写 SQL      |
| DB Migration  | Goose / Atlas         |
| HTTP          | net/http              |
| Proxy         | httputil.ReverseProxy |
| Metrics       | Prometheus            |
| Trace         | OpenTelemetry         |
| CLI           | Cobra                 |
| Web           | React 或 Vue           |
| Container     | Docker                |
| Orchestration | Kubernetes 后期         |

QUIC 底层采用成熟实现而不是自行实现传输协议；quic-go 当前实现 RFC 9000/9001/9002 等 QUIC 协议族能力。

---

# 96. 第一阶段：Data Plane TCP POC

这是最先开发的部分。

不开发：

```text
Web
User
RBAC
Database
```

只实现：

```text
edge
agent
origin
```

链路：

```text
curl localhost:9000
      ↓
Edge
      ↓
TLS/TCP WorkConn
      ↓
Agent
      ↓
localhost:8080
```

必须完成：

* TLS；
* mTLS；
* Control Connection；
* Work Pool；
* WorkHello；
* OPEN；
* OPEN_OK；
* OPEN_ERROR；
* RAW；
* Half-Close；
* Reset；
* Timeout；
* Reconnect；
* Pool refill。

### 验收

必须通过：

```text
HTTP
SSH

1GB upload
1GB download

1000 concurrent connections

long connection

client half-close

server half-close

Agent restart

Edge restart

slow client

slow origin

race detector

goleak
```

---

# 97. 第二阶段：QUIC POC

建立：

```text
QUIC Transport
```

实现相同：

```text
OPEN
OPEN_OK
OPEN_ERROR
RAW
```

必须复用：

```text
TunnelChannel
```

上层不得因为添加 QUIC 修改 HTTP/TCP Proxy 逻辑。

### 验收

测试：

```text
1000 streams
5000 streams

1GB transfer

packet loss 1%
packet loss 5%

RTT 100ms

network change

QUIC disconnect

stream reset
```

---

# 98. 第三阶段：Transport Manager

完成：

```text
TCP baseline
+
QUIC preferred
+
auto fallback
```

实现：

* Transport health；
* fallback；
* recovery；
* cooldown；
* hysteresis。

### 验收

模拟：

```text
UDP block
UDP unblock

UDP 50% loss

TCP normal

QUIC handshake fail

QUIC 中途 fail
```

要求：

```text
新连接自动切 TCP
```

并在恢复后切回 QUIC。

---

# 99. 第四阶段：Control Plane MVP

增加：

```text
PostgreSQL
Organization
Connector
Tunnel
Binding
HTTP Route
TCP Route
Enrollment
Desired State
Revision
```

Agent 开始从 Control 获取配置。

### 验收用户流程

```text
创建 Connector
 ↓
复制 Token
 ↓
安装 Agent
 ↓
ONLINE
 ↓
创建 Tunnel
 ↓
Origin
 ↓
创建 Route
 ↓
公网可访问
```

---

# 100. 第五阶段：Web Product MVP

开发：

```text
Web Console
CLI
Audit
RBAC
Metrics
Usage
```

主要页面：

```text
Dashboard

Connectors
Tunnels
Routes
Edges
Members
Audit
Settings
```

此阶段完成后可定义为：

```text
XTunnel Alpha
```

---

# 101. 第六阶段：Production Hardening

重点不是增加功能，而是稳定性。

加入：

* Resource limit；
* Graceful Drain；
* Last Known Good；
* Certificate Rotation；
* Agent Upgrade；
* Quota；
* Rate Limit；
* Usage Aggregator；
* Retry Strategy；
* Circuit Breaker；
* Chaos Testing；
* Benchmark；
* Security Audit。

达到：

```text
Beta
```

---

# 102. 第七阶段：Multi-Edge HA

实现：

```text
EdgeGroup
Multiple Edge
Connector Multi Edge Session
Scheduler
Health
Failover
```

测试：

```text
kill -9 edge
```

要求：

```text
已有连接允许失败
新连接自动恢复
```

---

# 103. 第八阶段：高级网络能力

在 V1 以后再考虑：

```text
UDP
TCP Multiplex
HTTPS Passthrough
gRPC advanced routing
Private DNS
Private Network
L3 Routing
Access Policy
OIDC
Service Token
```

它们不应阻塞 V1。

---

# 104. 测试体系

必须维护四级测试。

## Unit

覆盖：

```text
Protocol
Route
Reconcile
Scheduler
State Machine
RBAC
```

## Integration

```text
Agent ↔ Edge

Agent ↔ Control

Edge ↔ Control
```

## E2E

```text
Public Client
→ Edge
→ Tunnel
→ Agent
→ Origin
```

## Chaos

模拟：

```text
packet loss
delay
jitter

network down
UDP blocked

Edge crash
Agent crash
Control crash

disk full
certificate expired
```

---

# 105. 网络测试环境

Linux：

```text
network namespace
tc netem
nftables
```

必须能够自动构造：

```text
RTT 100ms

loss 1%

loss 5%

UDP DROP

TCP DROP

bandwidth 10Mbps

jitter 50ms
```

作为 CI 的 nightly suite。

---

# 106. Benchmark

至少包含：

### Connections

```text
100
1,000
10,000
50,000
```

### Throughput

```text
1 stream

10 streams

100 streams

1000 streams
```

### Payload

```text
1KB
1MB
100MB
1GB
```

### Network

```text
RTT:
1ms
20ms
50ms
100ms
200ms

Loss:
0%
0.1%
1%
5%
```

---

# 107. 必须做 Fuzz Test

重点：

```text
Frame Decoder
Varint Decoder
OpenRequest

TLS ClientHello dispatch

Route parser

Protocol Version

SNI parser
```

所有来自公网的协议解析器均视为：

```text
untrusted input
```

---

# 108. Security Review Checklist

发布前必须逐项评审：

* 是否存在任意 Origin Dial；
* Edge 是否能伪造 Tunnel Binding；
* 是否存在 Organization 越权；
* Enrollment Token 是否可重放；
* Device Cert 是否可冒用其他 Connector；
* Snapshot 是否支持 replay；
* 是否支持 revision downgrade；
* Protocol frame 是否可能 OOM；
* Pending queue 是否有限制；
* 是否存在 goroutine leak；
* TLS 是否限制最低版本；
* 是否存在日志泄密；
* Agent Upgrade 是否验证签名。

---

# 109. Agent Upgrade

Control：

```text
desired_version
```

Agent：

```text
current_version
```

升级包：

```text
binary
+
SHA-256
+
Ed25519 signature
```

流程：

```text
Download
 ↓
Verify Hash
 ↓
Verify Signature
 ↓
stage
 ↓
restart
 ↓
health
```

升级失败应：

```text
rollback previous binary
```

---

# 110. 发布兼容策略

协议采用：

```text
XTunnel Protocol v1
```

每个组件有：

```text
software version
protocol version
capabilities
```

HELLO：

```text
min_protocol
max_protocol
capabilities
```

禁止假设：

```text
所有 Edge 和 Agent 永远同时升级
```

至少支持：

```text
N
N-1
```

版本滚动升级。

---

# 111. Shutdown

所有程序必须：

```text
SIGTERM
```

进入 Graceful Shutdown。

Edge：

```text
stop accepting
 ↓
drain connections
 ↓
flush usage
 ↓
close sessions
```

Agent：

```text
notify draining
 ↓
stop accepting OPEN
 ↓
wait active
 ↓
close
```

---

# 112. 部署模型

## 开发

```text
Docker Compose

PostgreSQL
Control
Edge
Web
Origin
```

Agent 可直接本机运行。

## 小规模生产

```text
Control Server
PostgreSQL

Edge A
Edge B
```

## 正式生产

```text
                 Load Balancer
                       │
            ┌──────────┼──────────┐
            ▼          ▼          ▼
        Control A  Control B  Control C
            │          │          │
            └──────────┼──────────┘
                       ▼
                PostgreSQL HA


                       DNS
                ┌──────┴──────┐
                ▼             ▼
             Edge A         Edge B
```

---

# 113. V1 关键技术风险

评审时应重点关注以下问题。

| 风险                   | 等级 |
| -------------------- | -- |
| TCP Half-Close 语义    | 高  |
| QUIC Flow Control 参数 | 高  |
| TCP Work Pool 突发容量   | 高  |
| goroutine/socket 泄漏  | 高  |
| Transport 自动切换抖动     | 高  |
| Edge 被攻陷后的信任边界       | 高  |
| Snapshot 一致性         | 高  |
| Multi Edge Route 一致性 | 中高 |
| PKI 生命周期             | 中高 |
| HTTP/WebSocket 长连接   | 中  |
| Usage 高 Cardinality  | 中  |

---

# 114. V1 最终核心架构

整个系统最终可以压缩成：

```text
                         Control Plane
                              │
                Desired State / Identity
                              │
                  ┌───────────┴───────────┐
                  │                       │
                  ▼                       ▼
                Edge                  Connector
                  │                       │
                  │                       │
                  │  ┌─────────────────┐  │
                  ├══│ QUIC Transport  │══┤
                  │  └─────────────────┘  │
                  │                       │
                  │  ┌─────────────────┐  │
                  ├══│ TCP Transport   │══┤
                  │  └─────────────────┘  │
                  │                       │
                  │                       ▼
                  │                    Origin
                  │
             Public Client
```

---

# 115. V1 最重要的工程边界

开发过程中不得轻易破坏以下约束：

```text
1. Control Plane 不承载业务数据。

2. Edge 不知道 Origin 地址。

3. TCP 是基础 Transport。

4. QUIC 是高性能 Transport。

5. TCP V1 不 Multiplex。

6. QUIC 一个 Stream 对应一个业务 Connection。

7. Transport 对上层统一表现为 TunnelChannel。

8. PostgreSQL 保存 Desired State。

9. Agent 和 Edge 使用 Last Known Good。

10. 所有配置都有 revision。

11. 所有网络队列都有容量上限。

12. 所有公开协议输入都属于不可信数据。

13. 所有设备身份基于证书，不基于自行声明的 ID。

14. 已建立业务 Connection 不跨 Transport 迁移。

15. 已建立业务 Connection 不跨 Edge 迁移。
```

---

# 116. 推荐实际开发顺序

不要先做后台。

工程顺序严格建议：

```text
Step 01
Protocol package

Step 02
TLS identity

Step 03
Edge TCP Control

Step 04
TCP WorkConn

Step 05
OPEN protocol

Step 06
Raw tunnel

Step 07
Half-close / Reset

Step 08
TCP Pool

Step 09
TunnelChannel abstraction

Step 10
QUIC Transport

Step 11
Transport Manager

Step 12
HTTP Reverse Proxy

Step 13
TCP Listener

Step 14
Control Plane

Step 15
Desired State

Step 16
Enrollment

Step 17
Web Console

Step 18
Multi Edge

Step 19
Production Hardening
```

第一条真正的研发里程碑不是：

```text
登录页面做好了
```

而应该是：

```text
公网 TCP
   ↓
Edge
   ↓
TLS/TCP
   ↓
Agent
   ↓
Origin

在高并发、Half-Close、
网络抖动、Agent Restart
条件下保持正确。
```

只要这一层成立，XTunnel 的核心技术底座就成立了。

---

# 117. 第一阶段建议直接建立的 ADR

仓库初始化时建议直接提交：

```text
ADR-001-control-data-plane-separation.md

ADR-002-dual-transport.md

ADR-003-tcp-control-session.md

ADR-004-tcp-work-connection.md

ADR-005-quic-stream-mapping.md

ADR-006-edge-cannot-know-origin.md

ADR-007-postgresql-source-of-truth.md

ADR-008-desired-state-reconciliation.md

ADR-009-snapshot-signing.md

ADR-010-last-known-good.md

ADR-011-no-udp-tunnel-v1.md

ADR-012-no-tcp-multiplex-v1.md

ADR-013-no-zero-rtt-v1.md

ADR-014-no-connection-migration-between-edge.md
```

这些 ADR 一旦通过评审，后续开发原则上不得在普通 Feature PR 中偷偷改变。

---

# 118. 最终阶段划分

最终建议把研发项目拆成：

```text
Phase 0
TCP Data Plane

Phase 1
QUIC Data Plane

Phase 2
Dual Transport Manager

Phase 3
Control Plane

Phase 4
HTTP/TCP Product MVP

Phase 5
Security + Observability + Production Hardening

Phase 6
Multi Edge + HA

Phase 7
Advanced Network
```

其中 **Phase 0～2 是核心技术验证阶段**。

Phase 3～4 是产品化阶段。

Phase 5 是生产化门槛。

Phase 6 以后才属于分布式基础设施能力扩展。

---

## 结论

XTunnel V1 的核心不是 Web 管理平台，而是三个基础能力：

```text
① Reliable TunnelChannel

② Dual Transport

③ Desired-State Control Plane
```

数据面采用：

```text
TCP:
1 Business Connection
=
1 TLS Work Connection


QUIC:
1 Business Connection
=
1 QUIC Bidirectional Stream
```

两种实现最终统一成：

```text
TunnelChannel
```

控制面则围绕：

```text
Organization
Connector
Tunnel
Binding
Route
Edge
Revision
Snapshot
```

建立完整生命周期。

按照这个边界推进，第一阶段可以先验证一个很小的数据面核心，但架构不会因为后续加入 Web、HA、多 Edge、权限、证书、QUIC 或更多协议而推倒重来。这应该作为 XTunnel 项目的 V1 架构基线。
