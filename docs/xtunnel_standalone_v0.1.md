# XTunnel Standalone 第一阶段完整技术方案 V0.1

> **文档状态**：开发基线
> **目标状态**：完成后可作为 XTunnel Alpha 发布
> **核心语言**：Go 1.27
> **部署形态**：单 Server + 多 Tunnel + 每 Tunnel 多 Connector
> **数据存储**：SQLite
> **数据传输**：TLS/TCP
> **Web**：React + TypeScript + Vite + Tailwind CSS + shadcn/ui
> **Public HTTP 前置代理**：Caddy / Nginx
> **Agent Gateway 默认端口**：TCP 7443，可配置
> **核心定位**：可直接部署使用的集中式反向隧道 Standalone 产品
> **修订日期**：2026-08-25
> **本次修订**：领域模型对齐 Cloudflare Tunnel：Tunnel 持有一枚可重复取回的 ACTIVE Token，同一 Token 可启动多个临时 Connector；全部 Service 挂在 Tunnel 下，新连接默认按 Least Active + RR Tie-break 选择 Connector。Agent Binary 保持 Token-only、远端托管、无本地业务或配置状态

---

## Go 工具链基线（冻结）

- 项目 Go 语言版本固定为 **Go 1.27**，根 `go.mod` 必须声明 `go 1.27`。
- M0-01 必须选择一个稳定的 `go1.27.x` 补丁版本，由 `go.mod` 的 `toolchain` 指令记录，并在 CI、OCI Builder 和版本检查入口中使用同一个精确版本。开发、测试、代码生成、发布构建必须设置 `GOTOOLCHAIN=local` 并使用该工具链，不允许自动下载、静默切换或回落到其他 Go 版本。
- 根 `go.mod` 是工具链版本权威；`tools/go.mod` 必须使用相同的 `go`/`toolchain` 版本，Proto 生成工具也必须由同一个 Go 1.27.x 工具链构建。补丁版本升级必须显式同步两个 Module、CI、OCI Builder、版本检查和构建证据。
- 项目代码允许并应在适合的场景优先使用 Go 1.27 已稳定发布的语言、标准库和运行时特性；V0.1 不承诺兼容 Go 1.26 及更早版本，也不为旧工具链保留兼容垫片。
- 使用 Go 1.27 特性必须服务于当前实现的正确性、可维护性、性能或简化目标，并由相关测试覆盖；不得为了“体现新版本”引入无关抽象。
- `GOEXPERIMENT`、开发分支、tip-only API 或尚未进入稳定 Go 1.27 的能力不属于项目基线，使用前必须单独评审并获得明确授权。
- 所有 Go 验收命令执行前必须记录 `go env GOVERSION`；版本不是 `go1.27.x` 时快速失败，不得把结果记录为任务或 Gate 证据。

---

# 1. 项目目标

第一阶段不以验证网络协议为最终目标，而是交付一个真正能够部署使用的 XTunnel Standalone 产品。

用户只需要：

```text
公网服务器
    │
    ├── Caddy / Nginx
    └── xtunnel-server

内网服务器
    │
    └── xtunnel-agent
```

即可完成：

```text
创建 Tunnel
        ↓
获得 Tunnel Token
        ↓
使用同一 Token 启动一个或多个 Connector
        ↓
Server 识别 Tunnel ONLINE
        ↓
创建代理服务
        ↓
配置 Origin
        ↓
配置 HTTP / TCP 公网入口
        ↓
公网访问内网服务
```

第一阶段最终需要支持：

```text
HTTP
HTTPS（由前置代理终止 TLS）
WebSocket
Raw TCP
SSH
数据库 TCP 等通用 TCP 协议
```

---

# 2. 第一阶段产品边界

## 2.1 必须实现

### Server

```text
单节点运行

SQLite

管理员认证

Web Console

Tunnel 管理

Tunnel Token

Connector 运行副本

Connector 状态

Agent Control Session

TCP Work Pool

Service

HTTP Route

TCP Route

HTTP Reverse Proxy

WebSocket

TCP Listener

Config Revision

Config Snapshot

Origin Health

流量统计

Metrics

Structured Logging

Graceful Shutdown
```

### Agent

```text
Tunnel Token 认证

Ephemeral Connector Identity

Control Session

Session Authentication

Auth Bare-frame → Established Envelope Atomic Handoff

ALPN Empty / Unknown Rejection

Heartbeat Interval Negotiation

TCP Work Pool

Tunnel OPEN

Origin Resolver

Origin Dial

HTTP Origin

HTTPS Origin

TCP Origin

Health Check

Remote Config Snapshot

In-memory Atomic Config Apply

Automatic Reconnect

Graceful Shutdown
```

---

# 3. 第一阶段明确不实现

以下能力不进入 V0.1：

```text
QUIC

UDP Tunnel

TCP Multiplex

HTTP/3

Server Cluster

PostgreSQL

Control Plane 独立部署

Edge 独立部署

多租户

Organization

RBAC

OIDC

Zero Trust Access Policy

流量内容审计

WAF

CDN

L3 VPN

CIDR 网络访问

Agent 自动升级

Server 间业务流量转发
```

第一阶段只保留未来扩展所需要的接口和协议版本能力。

---

# 4. 第一阶段总体架构

推荐生产部署：

```text
                           Internet
                              │
                   ┌──────────┴──────────┐
                   │                     │
               HTTPS :443           TCP :7443
                   │                     │
                   ▼                     ▼
             Caddy / Nginx        XTunnel Server
                   │               Agent Gateway
          ┌────────┴────────┐             ▲
          │                 │             │
          ▼                 ▼             │
     Admin Host        Tunnel Host        │
          │                 │             │
          ▼                 ▼             │
127.0.0.1:8080    127.0.0.1:8081         │
   Management         HTTP Ingress        │
          │                 │             │
          └─────────┬───────┘             │
                    ▼                     │
              XTunnel Server              │
                    │                     │
                  SQLite                  │
                                          │
                     ┌────────────────────┘
                     │
               TLS/TCP Tunnel
                     │
             ┌───────┴───────┐
             │               │
             ▼               ▼
         Connector A     Connector B
             │               │
             ▼               ▼
           Origin          Origin
```

TCP Route：

```text
Internet
   │
   │ :10022
   ▼
XTunnel Server
TCP Listener
   │
   ▼
Service
   │
   ▼
Tunnel
   │
   ▼
Eligible Connector
   │
   ▼
Origin :22
```

---

# 5. 核心进程

第一阶段只提供两个核心二进制：

```text
xtunnel-server

xtunnel-agent
```

其中：

```text
xtunnel-server
=
Management Plane
+
HTTP/TCP Data Ingress
+
Agent Gateway
+
Runtime Manager
+
SQLite
+
Embedded Web Console
```

Agent：

```text
xtunnel-agent
=
Identity
+
Token Bootstrap
+
In-memory Remote Config Runtime
+
Control Session
+
TCP Transport
+
Origin Proxy
```

---

# 6. 核心领域模型

固定使用：

```text
Route
  │
  ▼
Service
  │
  ▼
Tunnel
  │
  ▼
Connector
  │
  ▼
Origin
```

禁止简化为：

```text
Route
  ↓
Agent
  ↓
Origin
```

V0.1 的 HTTP 协议边界是：Public Client 到 Caddy/Nginx 可以使用 HTTP/2；前置代理到 XTunnel HTTP Ingress 使用 HTTP/1.1；XTunnel 经 Tunnel 到 Origin 也使用 HTTP/1.1 字节流。V0.1 不支持 h2c、HTTP/2 帧透明转发或端到端 HTTP/2 多路复用。WebSocket 使用 HTTP/1.1 Upgrade。

---

# 7. Tunnel、Connector 与 Service

管理平面的顶层对象是 Tunnel。Tunnel 持有一枚当前 ACTIVE Connection Token、一个 Connector 运行池以及零到多个代理 Service：

```text
Tunnel
├── Stable Connection Token
├── Connector A / B / C
└── Service A / B / C
    └── Origin / Route
```

Connector 是运行 `xtunnel-agent` 的具体进程副本，不是需要预创建的持久化 Credential。管理端的“添加 Connector”是部署向导：读取并展示该 Tunnel 当前完全相同的 ACTIVE Token，不创建数据库 Connector 行、不生成新 Token，也不递增 Token Version。Connector 只有在认证成功后才出现在运行时列表中。

Service 是 Tunnel 下的代理服务配置。HTTP、TCP、SSH 等公网 Route 都指向 Service；Service 保存 Origin、Health 和 Revision。流量先解析 Service，再从其所属 Tunnel 的 Eligible Connector 中选择一条 WorkConn。

`xtunnel-agent` 继续作为客户端 Binary 名称；产品领域不再存在独立的“逻辑 Agent”聚合。

---

# 8. 身份层次

```text
Tunnel
    ↓
Connector
    ↓
Session
```

对应：

```text
tunnel_id = tun_<ULID>
connector_id = con_<ULID>
session_id = sess_<ULID>
```

Tunnel 由 Server 创建并持久化。每次启动 `xtunnel-agent` 进程都会在内存中生成新的 Connector ID；同一进程重连保持 Connector ID，进程重启重新生成。每次成功建立 Control Session 都生成新的 Session ID。

Server 使用 `tunnel_id + connector_id + session_id + generation` 做 Session fencing、WorkConn 归属和日志关联。Connector 与 Session 均不落库、不绑定物理机器、安装目录或长期设备记录。

---

# 9. tunnel_id

`tunnel_id` 是 Server 创建并持久化的 Tunnel 主键，格式为 `tun_<ULID>`。Tunnel 拥有名称、Token、Desired Revision、Connector 运行池和 Service 集合，是管理 API 的 Aggregate Root。

---

# 10. connector_id

每次启动 `xtunnel-agent` 进程都会在内存中生成 `con_<ULID>`。同一进程重连保持 Connector ID，进程重启重新生成；它不是独立 Credential，也不建立持久化 Connector 表。

---

# 11. session_id

每次 Control Session 认证成功后由 Server 生成 `sess_<ULID>`。同一 Connector 重连会得到新 Session ID，并递增该 `(tunnel_id, connector_id)` 的 Session Generation。

---

# 12. 身份树与多 Connector

同一 Tunnel 的所有 Connector 使用完全相同的当前 Token，并获得该 Tunnel 的完整 Service Snapshot。它们可以运行在同机、多机、容器或未来的 Kubernetes Pod 中，但必须能够按相同 Service 配置访问相同语义的 Origin。

```text
Tunnel tun_...
│
├── Connector con_...
│      └── Session sess_...
├── Connector con_...
│      └── Session sess_...
└── Connector con_...
       └── Session sess_...
```

多个 Connector 默认互为负载副本。新业务连接按“Eligible + 有 Idle WorkConn → Least Active → Round Robin tie-break”选择；已经进入 RAW 或已转发业务字节的连接不因负载均衡自动迁移或重放。

---

# 13. 同一主机运行多个 Connector

允许多个进程或容器使用同一 Tunnel Token。它们拥有不同 `connector_id` 和 `session_id`，并被视为同一 Tunnel 下互为负载副本的等价 Connector。

---

# 14. Connector 运行身份边界

Connector 与 Session 都是运行时标识。持有某个 Tunnel Token 的进程可以接收该 Tunnel 的全部 Service 配置并承接流量；需要隔离信任边界时必须创建不同 Tunnel 和 Token。

---

# 15. Connector 轻状态边界

Connector 不维护 Data Directory、安装身份、本地数据库、本地配置或本地 Desired State。运行输入只有 Binary 和一个不透明版本化 Connection Token。Token 同时携带 Server Endpoint、TLS Trust Descriptor、Tunnel/Token Identity 与认证 Secret。

以下对象只存在于当前进程内存：

```text
connector_id / session_id
current revision + full Service Snapshot
Origin Resolver / Health / WorkPool
Control Session / WorkConn
```

多个进程可以同时使用同一 Token；它们不共享可写目录。进程退出后 Connector 消失，重新启动后仅凭同一 Token 连接并取得完整当前配置。

---

# 16. Tunnel Connection Token

Tunnel Token 是长期有效、可重复部署到多个 Connector 的单个不透明 `xta_...` 字符串，不是一次性 Enrollment Token。其语义字段为：

```text
format version
Server Endpoint
TLS Trust Descriptor
tunnel identity
token identity / version
authentication secret
```

当前 Token Version 内返回的文本必须逐字节稳定。只有显式 Rotate，或 Endpoint/TLS Trust 变化导致管理员执行 Rotate，才生成新 Token。认证 Secret 由 CSPRNG 生成并提供至少 256 bit 随机熵。

---

# 17. Token 创建、获取与存储

创建 Tunnel 时签发第 1 代 ACTIVE Token。之后每次“添加 Connector”或“复制部署命令”都读取同一枚 ACTIVE Token：

```text
Create Tunnel → Issue Token v1
Add Connector A → Reveal Token v1
Add Connector B → Reveal Token v1
Add Connector C → Reveal Token v1
```

Reveal 不得创建 Token、不递增 Version，也不得按当前配置重新编码文本。完整 Token 使用 AES-256-GCM 加密后写入 SQLite；认证路径另外保存 `SHA-256(authentication_secret)` 并使用常量时间比较。AEAD AAD 至少绑定 `tunnel_id + token_id + token_version`，防止数据库行间密文替换。

Token Credential Master Key 是 Data Directory 内独立的 32 字节密钥，必须原子创建、权限 `0600`，不得复用 Gateway TLS Private Key。只要 `tunnel_tokens` 已有密文，Master Key 缺失、截断或权限错误就必须快速失败；空 Token 表（包括全新库和从旧 Schema 升级但尚未签发 Token 的库）允许首次创建。Backup/Restore 必须把 SQLite 与该 Key 作为同一一致性单元。

Token 返回响应必须 `Cache-Control: no-store`，不得进入日志、URL、Recent Activity 或前端持久化缓存；Reveal 和 Rotate 都必须写 Security Audit Event。

---

# 18. Token Rotation

Rotate 才生成新 Token Version：旧 Token 进入 `REVOKED_FOR_NEW_SESSION`，新 Token 进入 `ACTIVE`；既有 Session 继续工作，新认证只接受新 Token。

Reveal、Rotate 与 Token Revoke 只能通过 Credential Lifecycle Application Service 执行。Rotate 和 Revoke 使用 Tunnel aggregate `version` 作为精确 `If-Match`：Token 状态迁移、下一代 Token 写入、Tunnel Version 推进与对应 Security Audit Event 必须处于同一个 `BEGIN IMMEDIATE + synchronous=FULL` 事务。Rotate 复用当前 Token 中冻结的 Endpoint/TLS Trust，不从可能已经变化的外部配置重新编码；Token Revoke 只禁止新认证，不关闭已有 Session/ActiveWork。

如果 COMMIT 已成功、但恢复连接级 `synchronous=NORMAL` 失败，Repository 返回可识别的 `post-commit cleanup` 错误。Application 必须同时返回已经提交的真实结果（Rotate 包括新 Token），派生 committed audit 日志，并禁止把该错误误报成回滚后重试；普通事务错误仍返回零结果且不产生 committed 日志。

---

# 19. Tunnel Revoke

强制下线使用 Revoke Tunnel，原子禁用全部 Token，并在事务提交后关闭该 Tunnel 的全部 Session、Idle WorkConn 和 ActiveWork。

Token 状态、Tunnel Desired State 与 `tunnels.version` 必须在同一个 `BEGIN IMMEDIATE` 事务中提交并受 `If-Match` 保护。锁内不得执行网络 IO 或 Close。

耐久 COMMIT 后，Application 才调用当前进程唯一 Session Manager 的 `RevokeTunnel`，由它先建立永久 Auth/Install Fence，再在锁外关闭该 Tunnel 全 generation 的 Control Session、Idle/Connecting/Opening WorkConn 与 ActiveWork。已经撤销且 Version 仍匹配的重复请求不再次递增 Version，但必须重新执行 Runtime 收敛；并发 Revoke/Shutdown 必须等待同一 per-Session cleanup 完成，不能因查找入口已摘除而提前返回。

`TUNNEL_REVOKE/SUCCEEDED` 表示权威持久化吊销已经提交。若之后 Runtime 收敛失败，原事件保持不可变，并追加同一 request/trace 关联、独立 event/operation ID 的 `TUNNEL_REVOKE/FAILED` 事件，稳定 `error_code=RUNTIME_CONVERGENCE_FAILED`；失败证据写入失败也必须进入返回错误链。

---

# 20. Tunnel Token 表

```sql
CREATE TABLE tunnel_tokens (
    id TEXT PRIMARY KEY,
    tunnel_id TEXT NOT NULL,
    secret_hash BLOB NOT NULL UNIQUE,
    token_ciphertext BLOB NOT NULL,
    version INTEGER NOT NULL,
    status TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER CHECK (revoked_at IS NULL OR revoked_at > 0),
    FOREIGN KEY(tunnel_id) REFERENCES tunnels(id) ON DELETE CASCADE,
    UNIQUE(tunnel_id, version)
);

CREATE UNIQUE INDEX one_active_token_per_tunnel
ON tunnel_tokens(tunnel_id)
WHERE status = 'ACTIVE';
```

数据库不得出现 `xta_...` 明文或认证 Secret 原文。Token Ciphertext 属于 Secret，错误、日志和 Metric 均不得输出。

---

# 21. Agent Gateway

Agent 与 Server 之间使用独立 Agent Gateway。

默认：

```text
TCP :7443
```

允许配置：

```text
:8443
:9443
:443
```

等任意端口。

Server：

```yaml
agent_gateway:
  listen: ":7443"
```

Agent 从 Connection Token 解析 Endpoint；不读取独立 Agent 配置。

---

# 22. Agent Gateway 为什么独立

Management、HTTP Ingress 和 Agent Transport 分离：

```text
8080
Management

8081
HTTP Data Ingress

7443
Agent Transport
```

Agent Gateway 不依赖：

```text
Caddy HTTP reverse_proxy

Nginx HTTP proxy
```

Agent：

```text
直接连接 Gateway
```

---

# 23. Agent Gateway L4 Proxy

如果运行环境必须使用：

```text
443
```

可以通过：

```text
HAProxy TCP

Nginx stream

Caddy Layer4
```

做：

```text
TCP passthrough
```

V0.1 的 passthrough 必须保持 Agent TLS ClientHello 为上游收到的首字节，不支持在 Agent Gateway 前注入 PROXY Protocol v1/v2 Header。开启 PROXY Header 会导致 TLS Handshake 失败。

因此经 L4 Proxy 接入时，XTunnel 看到的 Peer IP 是 Proxy IP，所有 Peer-IP 握手限流也会共享该地址。运维必须按代理出口规模调整单 IP Burst/并发限制，同时保留 Server Global Auth Budget；不得误以为 XTunnel 能恢复真实 Agent IP。受信 L4 Proxy + PROXY v2 解析留到后续版本。

第一阶段不支持通过：

```text
普通 HTTP reverse_proxy
```

承载 Agent Protocol。

---

# 24. Agent Gateway TLS

Agent Gateway 强制：

```text
TLS >= 1.3
```

第一阶段：

```text
Server Authentication
+
Tunnel Token Authentication
```

不强制使用 Client Certificate。

TLS Session Resumption 只作为性能优化，不能改变 Tunnel Token、WorkHello HMAC、Budget Lease 或 Replay 检查。V0.1 禁止在 Agent Protocol 上使用 0-RTT Application Data；是否启用受限 Client Session Cache 由 5000 Connector 重连与 WorkConn 建连基准决定，并必须定义 Ticket Key 生命周期。未启用 Resumption 不能影响功能正确性或发布 Gate。

---

# 25. Agent Gateway TLS 模式

提供两种模式。

## public

用户配置受公共 CA 信任的证书：

```yaml
agent_gateway:
  tls:
    mode: public
    cert_file: /etc/xtunnel/server.crt
    key_file: /etc/xtunnel/server.key
```

Agent 使用系统 CA 验证：

```text
certificate
+
hostname
```

---

## pinned

Standalone 默认推荐。

Server 第一次启动生成：

```text
Persistent TLS Private Key
+
Self-Signed Certificate
```

Pinned 模式校验：

```text
SPKI Fingerprint
+
Certificate Validity Window
```

自签证书默认有效期为 397 天。Server 每次启动以及常驻期间的周期检查中，在剩余
`<=30` 天时使用原有 Private Key 自动续签新证书，因此 SPKI 不变，Agent 无需更新
Pin。已经过期但 Key 仍有效的证书也必须在 Agent Gateway Listen 前按同一 SPKI 续签，
避免 Server 离线跨过续签窗口后无法恢复。续签必须原子写入并热加载；失败时继续使用
仍有效的旧证书，同时产生 ERROR 日志和告警 Metric。若当前 Wall Clock 早于已加载证书
的 `NotBefore`，Server 必须以包含当前时间和有效期边界的明确错误拒绝启动/续签，不得
静默签发新证书掩盖明显的系统时钟回退。

计算：

```text
SHA-256(SPKI)
```

例如：

```text
sha256:ABCD...
```

创建 Agent 时安装命令包含：

```text
--server-pin
```

Agent 只在：

```text
SPKI Fingerprint
```

匹配时允许建立 TLS。

禁止：

```text
--insecure
```

作为正常配置。

---

# 26. Pinned TLS 文件

Server：

```text
<server.data_dir>/pki/
├── agent-gateway.key
└── agent-gateway.crt
```

Private Key：

```text
0600
```

服务器重启不得重新生成 Key。

否则所有 Agent Pin 都会失效。

Server 必须暴露：

```text
xtunnel_gateway_certificate_expiry_seconds
```

并在剩余 30 天、7 天、1 天时告警。Public 模式同样监控用户提供证书，但续签由外部证书管理系统负责。

Pinned Private Key 泄漏或必须更换 SPKI 时，V0.1 不提供通过旧 Control Session 自动轮换 Gateway Pin 的协议。唯一合法入口是离线维护命令：

```bash
xtunnel-server gateway rotate-key --maintenance
```

命令要求 Server 已停止并取得 Server External Lock；如果任何 Server 进程仍持锁则拒绝。命令生成新的 Key/Certificate 到 `pki` 同盘临时文件，写入并 fsync Gateway Identity Rotation Journal 后再原子 rename。崩溃后 Server 启动必须先根据 Journal 完成或回滚，禁止加载 Key/Certificate 不匹配的组合；成功后写入 Security Audit Event。新 Pin 不作为独立 Agent 配置或用户文件输出，而是由之后签发的 Connection Token 携带。

完整维护流程：

```text
停止 xtunnel-server
 ↓
执行 gateway rotate-key --maintenance
 ↓
启动 xtunnel-server；旧 pinned Token 因 Pin 不匹配保持离线
 ↓
通过 Web/API Rotate 每个 Tunnel Token，使新 Token 携带当前 Endpoint 与 Pin
 ↓
把新的单字符串 Token 重新部署到前台、容器、Linux systemd 或 Windows SCM
 ↓
重启 Agent 并核对全部 Connector 恢复 ONLINE
```

Agent 只使用 Connection Token 内的 TLS Trust Descriptor，禁止在 Pin 不匹配时自动接受新 Key、TOFU 覆盖或回落 `--insecure`。未部署新 Token 并重启的 Agent 进程会保持离线；这是 V0.1 明确接受的维护中断。在线双 Pin/Token 轮换留到后续协议版本。

---

# 27. Agent Gateway ALPN

同一个：

```text
TCP :7443
```

同时承载：

```text
Control Session
Work Connection
```

ALPN：

```text
xtunnel-control/1

xtunnel-work/1
```

TLS 握手完成后，Server 只接受精确协商为 `xtunnel-control/1` 或 `xtunnel-work/1`。客户端未提供 ALPN、协商结果为空或结果未知时立即关闭 TLS Connection，且不得读取或尝试解释任何 Auth/Work Frame；禁止默认回落到 Control Handler。

Server：

```text
Accept TCP
 ↓
TLS Handshake
 ↓
ALPN
 ↓
┌─────────────────┬─────────────────┐
│ control/1       │ work/1          │
▼                 ▼
Control Handler   Work Handler
```

---

# 28. Control Session Authentication

Protocol v1 的唯一权威来源固定为：

```text
api/proto/common.proto
api/proto/control.proto
api/proto/work.proto
```

`.proto` 中的 package、field number、enum number、reserved range 和 message direction 才是线上协议契约。本文中的 Protobuf 片段只解释设计语义，不得作为第二份可独立修改的 Schema。任何协议字段或 enum 变更必须先修改 `.proto`，通过 Buf lint、breaking check、generate drift check 和 Protocol Golden Vector 后，再同步本文语义说明。

M0.5 完成前，禁止开始 Server/Agent Protocol Handler。Protocol v1 固定：

```text
package = xtunnel.protocol.v1

go_package = <当前 Go Module>/internal/protocol/gen;protocolv1
```

实际 Go Module Path 在 M0 创建 `go.mod` 时确定；M0.5 必须把完整值写入所有 Proto，之后不得使用相对或占位 `go_package`。

Agent 先解析 Connection Token，取得 Endpoint 与 TLS Trust Descriptor，再建立：

```text
TLS
```

然后发送：

```protobuf
message ConnectorAuthRequest {
    string connection_token = 1;

    string connector_id = 2;
    string hostname = 3;

    string version = 4;
    string os = 5;
    string arch = 6;

    uint32 min_protocol = 7;
    uint32 max_protocol = 8;

    repeated string capabilities = 9;
}
```

---

# 29. Server Authentication

Server：

```text
Parse + validate Connection Token version/integrity
 ↓
Lookup token identity + tunnel identity
 ↓
Constant-time verify authentication secret hash
 ↓
Token ACTIVE?
 ↓
Tunnel REVOKED?
 ↓
Protocol compatible?
 ↓
Authentication success
```

Connection Token 的连接描述、身份和 Secret 必须作为同一受保护语义整体解析；未知版本、缺字段、超限、完整性失败或身份不一致都在认证提交前拒绝。M0 Agent Bootstrap 只做输入形状校验，真正的编码与解析器由 M05-02 冻结并由 M1-02/M1-05 接入。除认证所需的 Token 外，Agent 不向 Server 请求 Endpoint 或 TLS Trust，也不接受 Server 在 TLS 建立后反向覆盖 Token 内的信任根。

运行身份规则：

```text
同一 connector_id 已存在 Current Session
→ 新 Session 完成认证后递增 generation，并 fencing 旧 Session

旧 Session cleanup
→ 只能清理自己的 session_id + generation
```

`connector_id` 是 Token 认证后的临时运行标识，不是独立安全凭据。Server 不得在 Token 验证前依据自报字段执行覆盖或删除。

V0.1 的信任边界是：持有某个 Tunnel Token 的进程，被视为该 Tunnel 的完整受信 Connector。它可以接收该 Tunnel 的全部 Service 配置并承接对应业务流量。Server 只把 Connector 作为当前在线运行副本观测对象，不建立跨重启 Installation 身份或设备注册记录。若不同主机之间不能共享这一信任边界，管理员必须为它们创建不同的 Tunnel。

认证统一返回显式 Result，而不是只定义成功响应：

```protobuf
message ConnectorAuthResult {
    oneof result {
        ConnectorAuthSuccess success = 1;
        ConnectorAuthFailure failure = 2;
    }
}

message ConnectorAuthSuccess {
    string tunnel_id = 1;

    string session_id = 2;

    bytes session_secret = 3;

    uint32 protocol_version = 4;

    uint64 desired_revision = 5;

    uint32 heartbeat_interval_ms = 6;
}

message ConnectorAuthFailure {
    ErrorCode error_code = 1;
    uint32 retry_after_ms = 2;
}
```

认证成功后，Server 必须向新 Control Session 下发当前完整 TunnelSnapshot；Connector 不依赖本地 Revision 决定是否跳过首份配置。

认证失败流程固定为：

```text
TLS Established
 ↓
ConnectorAuthRequest
 ↓
ConnectorAuthResult{failure: ConnectorAuthFailure}
 ↓
在 control.write_timeout 内 flush 完整 Frame
 ↓
Close TLS Connection
```

除 TLS 已经不可写或对端提前关闭外，禁止用直接 EOF 代替认证失败结果。`TOKEN_INVALID`、`TOKEN_REVOKED`、`TUNNEL_REVOKED`、`VERSION_UNSUPPORTED` 和可重试的 Server 容量错误必须能够被 Connector 区分。只有可重试错误允许设置非零 `retry_after_ms`；永久 Credential、Pin 或版本错误不得通过短周期自动重连放大负载。

---

# 30. session_secret

每个 Control Session 生成：

```text
32 byte random
```

只存在：

```text
Server memory
+
Agent memory
```

不写数据库。

用途：

```text
认证 WorkConn
```

避免每一个 WorkConn 都发送长期 Tunnel Token。

---

# 31. WorkConn Authentication

Agent 新建 WorkConn：

```protobuf
message WorkHello {
    string tunnel_id = 1;

    string connector_id = 2;

    string session_id = 3;

    string work_id = 4;

    reserved 5, 6;

    bytes nonce = 7;

    bytes mac = 8;

    string budget_lease_id = 9;
}
```

其中：

```text
mac = HMAC-SHA256(
    session_secret,
    "xtunnel-work-v1"
    || deterministic_protobuf(WorkHelloWithoutMAC)
)
```

其中：

```text
nonce = 32 byte crypto/rand

所有字符串必须先通过对应 ID 格式校验
```

Protocol v1 的 ID 格式必须在 `common.proto` 对应注释和共享校验包中固定为 ASCII、带类型前缀的 ULID：

```text
tunnel_id        = tun_<26-char Crockford ULID>
connector_id     = con_<26-char Crockford ULID>
service_id       = svc_<26-char Crockford ULID>
session_id       = sess_<26-char Crockford ULID>
work_id          = work_<26-char Crockford ULID>
connection_id    = conn_<26-char Crockford ULID>
budget_lease_id  = lease_<26-char Crockford ULID>
drain_id         = drain_<26-char Crockford ULID>
```

类型前缀固定为上述小写形式，ULID Body 固定为 26 位大写 Crockford Base32。接收端拒绝错误大小写、错误前缀、错误长度、非 Crockford 字符和额外空白。ID 校验必须发生在日志字段、Map Key、HMAC 输入或状态查找之前。

---

# 32. WorkHello 防重放

Server 检查：

```text
Session 必须存在

Connector 必须匹配

Work ID 不得重复

HMAC 正确

Budget Lease 属于当前 Session 且仍在 Server monotonic Deadline 内
```

WorkHello 不使用 Agent 与 Server 的 wall clock 做认证裁决，不要求通过 NTP 才能建立数据面连接。`work_id` 在对应 Budget Lease 生命周期内唯一；`nonce` 必须随机且参与 HMAC。

Server 使用按 Lease 分桶的有界 Replay Cache。WorkHello 验证、Lease 槽位消费和 Replay 登记必须在同一个临界区原子完成：

```text
Cache Entry 保留到 Lease monotonic Deadline

Lease 到期 / Session 关闭 → 清理对应分桶

超过 max_replay_entries_per_session
→ SESSION_RESOURCE_EXHAUSTED
```

即使 Replay Cache 条目已经随过期 Lease 清理，旧 WorkHello 也会因 Lease 无效而拒绝。`timestamp_ms` 的 Protobuf field number 6 永久保留，不得在 Protocol v1 中复用。

所有 Protocol v1 结构化 Message，只要自身或任意递归子消息存在 Protobuf Unknown Fields，就必须在业务、HMAC 或 Revision 判断前以 `PROTOCOL_ERROR` 拒绝。该规则覆盖 Auth、Control、Work 和 Snapshot。禁止在某一端 discard、另一端 preserve，也禁止把未知字段静默带入 deterministic marshal。V1 需要扩展时必须发布 Protocol v2，或新增由已协商 Capability 明确启用的独立 Message，不能向既有 v1 Message 偷加字段。

HMAC 输入必须由已验证的已知字段重新构造，清空 `mac` 后使用固定版本 `google.golang.org/protobuf` 的 `proto.MarshalOptions{Deterministic: true}` 生成。Snapshot 的确定性序列化仍用于大小 Gate 和 Golden Vector。升级该 Runtime 必须重新运行全部 Golden Vector；Golden Vector 字节变化属于 Protocol Breaking Change。

---

# 33. Protocol Framing

Control Session 和 WorkConn 的结构化阶段统一使用：

```text
UVarint Frame Length
+
Protobuf Message
```

Frame 内层类型按连接与状态唯一确定：AUTH 使用裸 `ConnectorAuthRequest` / `ConnectorAuthResult`；ESTABLISHED/DRAINING Control 使用 `ControlEnvelope`；WorkConn 在 RAW 前按状态使用唯一合法的裸 Work Message。

AUTH 阶段使用同样 UVarint Length 和 MaxAuthFrameSize，但不把 Auth Message 放入 `ControlEnvelope`。Server 只在完整 `ConnectorAuthResult.success` Frame 已在 `write_timeout` 内 flush 成功后才原子切换到 `ESTABLISHED`；Agent 只在完整解码并验证该 Success 后切换。两个提交点之前双方均禁止发送或接受 `ControlEnvelope`。Auth Failure flush 后直接关闭，不进入 ControlEnvelope 阶段。

Control Session Envelope：

Envelope：

```protobuf
message ControlEnvelope {
    uint32 protocol_version = 1;

    oneof payload {
        Heartbeat heartbeat = 10;

        TunnelSnapshot config_snapshot = 11;

        ConfigAck config_ack = 12;

        WorkDemand work_demand = 13;

        ServiceHealthBatch service_health_batch = 14;

        DrainRequest drain_request = 15;

        Error error = 16;

        DrainAck drain_ack = 17;
    }
}
```

Control Session 状态固定为：

```text
AUTH
ESTABLISHED
DRAINING
CLOSED
```

消息方向和合法状态必须同时写入 `control.proto` 注释，并由表驱动 Protocol State Test 锁定：

| Message | Agent → Server | Server → Agent | AUTH | ESTABLISHED | DRAINING |
| --- | ---: | ---: | ---: | ---: | ---: |
| ConnectorAuthRequest | ✓ | × | ✓ | × | × |
| ConnectorAuthResult | × | ✓ | ✓ | × | × |
| Heartbeat | ✓ | × | × | ✓ | ✓ |
| TunnelSnapshot | × | ✓ | × | ✓ | × |
| ConfigAck | ✓ | × | × | ✓ | ✓ |
| WorkDemand | × | ✓ | × | ✓ | × |
| ServiceHealthBatch | ✓ | × | × | ✓ | ✓ |
| DrainRequest | ✓ | × | × | ✓ | 幂等 |
| DrainAck | × | ✓ | × | ✓ | 幂等 |
| Error | ✓ | ✓ | × | ✓ | ✓ |

AUTH 阶段不使用 `ControlEnvelope.Error`。Server 能安全解码 `ConnectorAuthRequest` 但发现版本、未知字段或认证语义错误时，发送 `ConnectorAuthResult.failure{error_code: PROTOCOL_ERROR 或对应 Auth Error}`，flush 后关闭；Frame 已无法安全解码时直接关闭。Agent 在 AUTH 收到无法解码、非法 oneof 或非期望 Result 时直接关闭，不发送 Control Error。

`ESTABLISHED/DRAINING` 收到错误方向、当前状态不允许的 Message 时，接收端应在仍可安全写入时发送 `ControlEnvelope.Error{error_code: PROTOCOL_ERROR}`，随后关闭 Control Session。完全相同的 DrainRequest 和 DrainAck 必须幂等，返回或重发当前状态；同一 ID 但内容不同必须视为 `PROTOCOL_ERROR`。`protocol_version` 必须等于 TLS/Auth 协商出的版本，任何不一致都关闭 Session。

WorkConn：

```text
TLS + ALPN xtunnel-work/1
 ↓
WorkHello Frame
 ↓
WorkReady Frame
 ↓
IDLE
 ↓
OpenRequest Frame
 ↓
OpenResponse Frame
 ↓
RAW
```

```protobuf
message WorkReady {
    string work_id = 1;
    WorkReadyStatus status = 2;
    ErrorCode error_code = 3;
}

```

`common.proto` 必须完整定义并冻结至少以下 enum：

```text
ErrorCode
WorkReadyStatus
OpenStatus
IngressType
HealthType
HealthStatus
ConfigApplyStatus
```

零值规则只有两个有意例外：`ERROR_CODE_OK=0` 和 `HEALTH_STATUS_UNKNOWN=0`；其他状态 enum 的零值均为 `*_UNSPECIFIED`，接收端禁止把它解释为成功。初始固定映射为：

```text
WorkReadyStatus: UNSPECIFIED=0, READY=1, REJECTED=2
OpenStatus:      UNSPECIFIED=0, OK=1, ERROR=2
IngressType:     UNSPECIFIED=0, HTTP=1, TCP=2
HealthType:      UNSPECIFIED=0, DISABLED=1, TCP=2, HTTP=3
HealthStatus:    UNKNOWN=0, HEALTHY=1, UNHEALTHY=2
ConfigApplyStatus: UNSPECIFIED=0, APPLIED=1, REJECTED=2
```

WebSocket 属于 HTTP/1.1 Upgrade，HTTPS 已由前置代理终止，都不增加独立 IngressType。第 99 节列出的 Error Code 数值必须原样进入 `common.proto`；新增值只能使用未占用编号，删除值必须 `reserved`。

每个 Frame 必须使用严格 bounded reader。读取结构化 Frame 时不得预读 Frame 边界外的数据。

Frame Length 必须使用表示该值的最短 Canonical UVarint。非最短编码、超过
`uint64` 表示范围、十字节后仍未终止或声明长度超过当前阶段上限，必须在分配
Payload 前按 `PROTOCOL_ERROR` 拒绝。

收到 `OPEN_OK` 后，已经位于 socket/buffer 中但不属于 OpenResponse Frame 的剩余字节，必须作为 RAW 数据原样交给代理层，禁止丢弃或重复。

---

# 34. Frame Size

统一限制：

```text
MaxControlFrameSize = 1 MB

MaxAuthFrameSize = 64 KB

MaxWorkFrameSize = 64 KB

MaxTunnelSnapshotBytes = 768 KiB
```

`MaxTunnelSnapshotBytes` 是 TunnelSnapshot 本体的业务上限，必须低于 1 MB Control Frame，为 Envelope、未知字段和后续兼容字段预留空间。禁止把 1 MB 当作可用 Payload 大小。

任何：

```text
invalid varint

negative/overflow length

frame > 对应阶段上限

malformed protobuf
```

立即：

```text
PROTOCOL_ERROR
```

错误隔离范围固定为：Control Frame 错误关闭对应 Control Session；WorkHello、WorkReady、OpenRequest 或 OpenResponse Frame 错误只关闭对应 WorkConn。只有认证级错误，或同一 Control Session 在滑动窗口内连续超过协议违规阈值，才关闭并短暂封禁整个 Control Session，禁止因单条业务连接的 malformed Frame 清空整个 Connector。

---

# 35. Protocol Version

第一阶段：

```text
XTunnel Protocol v1
```

Agent Hello 包含：

```text
min_protocol
max_protocol
capabilities
```

Server：

```text
计算共同版本
```

没有共同版本：

```text
VERSION_UNSUPPORTED
```

---

# 36. Runtime Ownership and Tunnel Runtime Registry

每条 Control Session 的并发模型固定为：

```text
TLS Conn
  │
  ├── readLoop（唯一 ReadFrame 调用者）
  │       ↓
  │   SessionOwner（唯一状态所有者）
  │       ↓
  │   bounded + coalesced ControlOutbox
  │       ↓
  └── writeLoop（唯一 WriteFrame 调用者）
          ↓
       TLS Conn
```

任何 Heartbeat Timer、Snapshot Reconciler、WorkDemand、Drain、Health Checker 或其他业务 goroutine 都禁止直接调用 Control TLS Conn 的 `Read`、`Write`、`ReadFrame` 或 `WriteFrame`。它们只能把事件投递给 `SessionOwner`；只有 `writeLoop` 可以写入完整的 `UVarint Length + Protobuf Payload` Frame。

默认 Outbox 契约：

```yaml
control:
  high_priority_queue: 32
  normal_queue: 128
  write_timeout: 5s
```

队列语义固定为：

```text
High Priority
├── Error
├── DrainRequest / DrainAck
├── ConfigAck
└── 最新 Heartbeat

Coalescible
├── TunnelSnapshot             key = tunnel_id，保留最高 revision
├── WorkDemand                 key = connector_id，保留最高 generation
└── ServiceHealth pending accumulator，按 service_id 保留最新项
```

旧 Heartbeat 尚未发送时由新 Heartbeat 覆盖，不允许累计。Health 结果在唯一 pending accumulator 中按 `service_id` 合并；只在出队并冻结为不可变 `ServiceHealthBatch` 时才分配严格递增的 `generation`，已冻结 Frame 不再改写。Snapshot 按 Revision 串行 Apply，较高 Revision 可以覆盖尚未开始 Apply 的较低 Revision。Normal Queue 满时，先执行上述合并；仍无法容纳的新消息不得无限等待。High Priority Queue 满、完整 Frame 在 `write_timeout` 内无法写完，或 Owner 无法保证消息次序时，记录 `SESSION_RESOURCE_EXHAUSTED` 并关闭该 Session。关闭动作必须解除 readLoop/writeLoop 的阻塞并等待二者退出，禁止遗留 goroutine。

Server 内存：

```go
type TunnelRuntime struct {
    mu sync.Mutex

    TunnelID string

    Connectors map[string]*ConnectorRuntime

    ActiveWork map[string]*ActiveWorkRuntime
}
```

Connector：

```go
type ConnectorRuntime struct {
    ConnectorID string

    Hostname string

    CurrentSession *ControlSession

    SessionGeneration uint64

    TCP *TCPTransport

    Health map[string]ServiceHealth

    ConnectedAt time.Time

    Draining bool
}
```

Active WorkConn 必须独立于 Current Session 保存：

```go
type ActiveWorkRuntime struct {
    ConnectionID string
    TunnelID     string
    ConnectorID  string
    SessionID    string
    Generation   uint64
    WorkID       string

    Cancel context.CancelFunc
    WorkConn net.Conn
    PeerConn net.Conn
    closeOnce sync.Once
}
```

一个 Tunnel 内所有 Connector、Session、WorkPool、Health 和 ActiveWork 状态变化，都必须在对应 `TunnelRuntime.mu` 下线性化。不同 Tunnel 使用不同锁；禁止建立跨 Tunnel 的嵌套 Runtime Lock。

固定线性化点：

```text
Session Replacement
= lock → generation++ → CurrentSession = newSession → unlock

Acquire Idle
= lock → IDLE → OPENING → 从 Idle Pool 移除 → unlock

OPEN_OK
= lock → OPENING → ACTIVE → 注册 ActiveWork → unlock

Active Close
= lock → Active Registry remove → CLOSED → unlock

Drain
= lock → Draining=true → 从选择集合摘除 → unlock
```

持有 Runtime Lock 时禁止进行网络 IO、SQLite 操作、阻塞 Channel Send、等待 goroutine 或调用 `Conn.Close`。需要关闭的 Conn 和 Cancel Handle 在锁内收集，释放锁后通过 `closeOnce` 执行 `Cancel → SetDeadline(now) → Close`。所有 Counter、Budget Lease 和 Registry 删除必须由同一个终止路径执行且只执行一次。

Session 清理只能在 `connector_id + session_id + generation` 仍匹配时修改 Current Session。旧 Session 的延迟清理不得删除或关闭新 Session。

Tunnel Revoke 必须通过 Tunnel 级 ActiveWork Registry 找到并关闭所有旧、新 Session 的 Active WorkConn。

旧 Session cleanup 只能关闭属于旧 Session 的 Idle/Opening WorkConn。已经进入 ACTIVE 的旧 WorkConn 必须继续留在 Tunnel 级 ActiveWork Registry，直到自然结束、Tunnel Revoke 或 drain timeout；cleanup 不得仅因 Session 已被 fencing 就删除或关闭它们。

---

# 37. Runtime State 不进 SQLite

以下对象：

```text
Control Session

WorkConn

Active Tunnel Connection

Connector

Session Secret

Pending OPEN
```

全部：

```text
只存在内存
```

Server 重启后通过 Agent 重连重新建立。

---

# 38. Ephemeral Connector 观测

Connector 只存在于 Server Runtime Registry，不写入 SQLite。Web 和 API 可以展示当前在线或仍有 ActiveWork 的 Tombstone Connector：

```text
connector_id
hostname / os / arch / version
connected_at / last_heartbeat_at
session_id / generation
WorkPool / ActiveWork / Health
```

进程退出且 Tombstone 清理完成后，该 Connector 从运行态消失。V0.1 不提供跨重启 Installation History、设备清单或机器身份审计；长期审计依赖结构化安全事件和 Server 日志，而不是 Agent 本地身份文件。

Runtime 以完整 `(tunnel_id, connector_id, session_id, generation)` fencing 更新 Connected、Heartbeat、Draining、Disconnected；旧 generation 不得覆盖新 generation 的 Metadata、时间或状态。同一 Connector replacement、Revoke 和 Shutdown 期间，Manager 必须继续持有所有尚未完成 cleanup 的 retiring Session，直到 Authenticator、Pool、Owner 与 Registry 收敛完成。

稳定生命周期日志事件为 `connector_connected`、`connector_session_replaced`、`connector_draining`、`connector_disconnected`。只允许输出真实存在的 `tunnel_id`、`connector_id`、`session_id`、`generation`、`connector_status`、`hostname`、`os`、`arch`、`version`、`reason`；项目 Bootstrap 创建的统一 JSON Logger 必须注入 Session Manager，不得用默认 Logger 或静默丢弃生产事件。

M2 在 Session Manager 暴露无 Label 聚合 Metric Source：`xtunnel_connectors_online`、`xtunnel_control_sessions_online`、`xtunnel_active_connections`、`xtunnel_tcp_idle_work_connections`、`xtunnel_tcp_active_work_connections`。M6 只负责将该唯一 Source 导出到 `/metrics`，不得另建第二套计数状态。

---

# 39. Tunnel 状态

Tunnel 状态：

```text
PENDING

ONLINE

DEGRADED

OFFLINE

REVOKED
```

计算规则：

```text
Tunnel revoked
→ REVOKED

Tunnel 创建后从未有 Connector 成功完成认证
→ PENDING

曾经成功认证，但当前没有 Current Control Session
→ OFFLINE

至少一个 Connector Status == ONLINE
→ ONLINE

至少一个 Current Control Session，
但所有 Connector 都是 DEGRADED / DRAINING
→ DEGRADED
```

Tunnel Status 只聚合 Connector Runtime，不读取任何 Service/Origin Health。某个 Connector 可以访问 SSH Origin、但不能访问 Jenkins Origin，此时 Tunnel/Connector 仍可能 ONLINE；差异只反映在对应 Service Status。

“从未成功认证”由 Tunnel 持久化的 `first_authenticated_at` 判定，而不是从当前
Runtime 是否为空反推。该字段只在完整 Connector Auth Success Frame 已写出后首次
记录；因此 Server 重启后，曾经成功认证但当前没有 Current Control Session 的
Tunnel 仍然是 `OFFLINE`，不会退回 `PENDING`。

---

# 40. Connector 状态

```text
ONLINE

DEGRADED

DRAINING
```

计算规则：

```text
DRAINING
= 已进入 Drain，两阶段握手尚未结束

ONLINE
= Current Control Session 存活
+ Heartbeat Fresh
+ 已完成当前 Session 的首份完整 Config Apply/Ack
+ Connector-wide Transport 可以接受新 Work

DEGRADED
= Current Control Session 存活
+ Heartbeat Fresh
+ 首份完整 Config Apply/Ack 尚未完成，
  或 Connector-wide Transport 持续无法接受新 Work
```

`Connector-wide Transport` 只包含 Control/WorkPool/Budget/FD 等 Connector 级能力。Per-Service Origin Health 不参与 Connector Status。Heartbeat 超时或 Control Session 关闭后，Connector 不保留一个永久 OFFLINE Runtime 状态，而是按下述 Tombstone 规则删除或保留。

Status 只读取 Tunnel Runtime 已发布的 Lifecycle 与 Eligibility 快照；Session Manager
中尚未成功发布的数据不能提前改变展示状态。Lifecycle 已进入 `DRAINING` 时，即使
WorkPool 尚处在同一次 Drain 操作的切换窗口，也必须立即展示 `DRAINING`。

Connector 不保存永久 OFFLINE Runtime 对象。

如果当前 Session 断开且没有旧 Active WorkConn：

```text
从 Runtime Registry 删除
```

如果仍有旧 Active WorkConn，则保留不可选择的 Connector Tombstone，直到 Active 数归零。它不参与新连接选择，但继续承担计数、Usage、日志和 Revoke 归属。

Web 只展示当前在线 Connector 和尚有 ActiveWork 的 Tombstone，不把一次进程生命周期伪装成永久设备记录。

---

# 41. Heartbeat

默认：

```text
heartbeat_interval = 10s

heartbeat_timeout = 30s
```

`ConnectorAuthSuccess.heartbeat_interval_ms` 是该 Control Session 的 Server 权威值，必须满足 `0 < heartbeat_interval <= heartbeat_timeout / 3`。Agent 认证成功后立即采用，不能继续使用本地旧默认值。Server 以本地单调时钟记录“最后一次成功收到 Heartbeat”的时间并判断 Timeout，不使用客户端 `timestamp_ms` 计算存活，避免时钟漂移造成误下线。

Agent：

```protobuf
message Heartbeat {
    uint64 timestamp_ms = 1;

    uint64 observed_revision = 2;

    uint32 tcp_idle = 3;
    uint32 tcp_active = 4;

    uint64 ingress_bytes = 5;
    uint64 egress_bytes = 6;

    uint32 tcp_connecting = 7;
}
```

Heartbeat 的 `ingress_bytes` / `egress_bytes` 按第 110 节全局方向定义，是当前 Control Session 建立以来的累计诊断值，Session 重建后从零开始。它们只用于 Connector 运行态对账和异常检测，不写入 `usage_minutes`，也不与 Server 数据面 `UsageCounter` 再次相加；持久化计费/报表以 Server 数据面计数为唯一来源，避免双重入账。

超过：

```text
30s
```

没有 Heartbeat：

```text
Session Closed
```

---

# 42. TCP Transport 抽象

第一阶段即定义：

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

    Available(
        serviceID string,
    ) bool

    Acquire(
        ctx context.Context,
        req OpenRequest,
    ) (TunnelChannel, error)

    Stats() TransportStats

    Close() error
}
```

第一阶段实现：

```text
TCPTransport
```

---

# 43. Tunnel Dialer

公网入口只依赖：

```go
type TunnelDialer interface {
    DialContext(
        ctx context.Context,
        tunnelID string,
        metadata ConnectionMetadata,
    ) (TunnelChannel, error)
}
```

HTTP Proxy 和 TCP Proxy 不直接依赖：

```text
WorkConn

Connector

TCP Pool
```

---

# 44. TCP Work Pool

每个 Connector 独立维护：

```text
Control Session
+
Work Pool
```

例如：

```text
Tunnel
│
├── Connector A
│   ├── Control
│   └── WorkPool A
│
└── Connector B
    ├── Control
    └── WorkPool B
```

Server 的 WorkDemand 策略默认目标为：

```text
min_idle = 4
target_idle = 8
max_idle = 32
```

Agent 不维护这些本地配置。V0.1 Binary 使用不可由远端放大的安全硬上限：`max_connecting = 16`、`max_total = 256`；Server 下发的 Demand 必须同时受 Server Budget 和 Agent 本地硬上限钳制。

`max_total` 包含：

```text
CONNECTING + IDLE + OPENING + ACTIVE
```

---

# 45. WorkConn 生命周期

```text
CONNECTING
    ↓
TLS_HANDSHAKE
    ↓
AUTHENTICATING
    ↓
IDLE
    ↓
OPENING
    ↓
ACTIVE
    ↓
CLOSED
```

一个 WorkConn：

```text
只处理一个业务 Connection
```

禁止：

```text
ACTIVE → IDLE
```

WorkConn 消息方向与状态固定为：

| Message / Data | 方向 | 合法状态 |
| --- | --- | --- |
| WorkHello | Agent → Server | AUTHENTICATING |
| WorkReady | Server → Agent | AUTHENTICATING |
| OpenRequest | Server → Agent | IDLE；收到后原子进入 OPENING |
| OpenResponse | Agent → Server | OPENING |
| RAW Bytes | 双向 | ACTIVE |

WorkConn 在 RAW 之前不使用 Envelope，每个方向和状态只有一种合法的裸 Message 类型，因此禁止通过“先尝试解码哪个 Message”推断类型。正常的认证/打开拒绝只能通过期望状态中的 `WorkReady.status/error_code` 或 `OpenResponse.status/error_code` 表达。错误方向、重复的非幂等 Frame、状态不允许的 Frame、Unknown Field 或 `work_id/connection_id` 不匹配时，接收端直接关闭对应 WorkConn，不发送额外结构化 Error Frame。任何一方都不得在 `OPEN_OK` 前发送 RAW，也不得在进入 ACTIVE 后重新解释或发送结构化 Frame；ACTIVE 中的错误只能通过关闭/Half-Close 表达。

---

# 46. WorkConn KeepAlive

Idle WorkConn 长时间无业务时可能被：

```text
NAT

Firewall

Middlebox
```

关闭。

默认：

```text
TCP KeepAlive = 30s

WorkConn Max Idle Age = 5min
```

Agent 定期主动替换过老 WorkConn。

---

# 47. Work Pool 补充

目标：

```text
idle >= target_idle
```

如果：

```text
idle < min_idle
```

立即请求 Server 更新 Budget Demand；收到 Lease 后，本轮最多新建：

```text
min(
  target_idle - idle,
  lease remaining slots,
  max_connecting - connecting
)
```

最大并发创建：

```text
max_connecting
```

---

# 48. WorkDemand

公网流量到达时，如果：

```text
Agent Online

但没有 Idle WorkConn
```

Server：

```protobuf
message WorkDemand {
    string budget_lease_id = 1;
    uint32 desired_non_active = 2;
    uint32 max_new_connections = 3;
    uint32 lease_ttl_ms = 4;
    uint64 demand_generation = 5;
}
```

`desired_non_active` 是该 Connector 的 `Connecting + Idle + Opening` 绝对目标，不是“再增加多少”。Agent 只处理最新 `demand_generation`；目标降低或公网 Pending 消失时，Server 必须发送新 generation 更新或取消 Demand。

Server 发送 WorkDemand 前，必须由全局 WorkConn Budget Manager 为 `budget_lease_id` 预留最多 `max_new_connections` 个槽位。Lease 绑定 `tunnel_id + connector_id + session_id + session_generation`，不能跨 Session 或 Connector 使用。双方在收到消息时分别用本地 monotonic clock 从 `lease_ttl_ms` 建立 Deadline，禁止比较跨主机绝对时间；Server 是 Lease 是否仍有效的最终裁决者。Agent 只能在本地 Deadline 前建连，并在 WorkHello 中携带该 Lease ID；Server 在 WorkHello 验证阶段检查自己的 Deadline 并原子消费一个槽位。未消费槽位在 TTL 到期、Session 关闭或 Demand 取消时归还。Agent 仍受本地 `max_connecting` 限制，不能把 Lease 当作绕过本地上限的许可。

Control Session 建立后，Server 根据远端策略、当前 Heartbeat Pool 计数、全局/Tunnel/Connector/FD 预算生成初始 Demand。Agent 不得在没有有效 Lease 时主动创建 WorkConn，也不得因远端 Demand 超过 Binary 硬上限而无界分配。

公网请求最多等待：

```text
2s
```

即：

```text
work_acquire_timeout = 2s
```

---

# 49. Service OPEN

Server 获得 WorkConn 后发送：

```protobuf
message OpenRequest {
    uint32 protocol_version = 1;

    string connection_id = 2;

    string service_id = 3;

    string trace_id = 4;

    string client_addr = 5;

    uint64 timestamp_ms = 6;

    IngressType ingress_type = 7;

    string traceparent = 8;
    string tracestate = 9;
}
```

明确禁止：

```text
origin_host

origin_port
```

出现在 OpenRequest。

---

# 50. Origin Resolver

Agent 本地：

```go
type OriginResolver interface {
    Resolve(
        serviceID string,
    ) (Origin, error)
}
```

例如：

```text
tun_abc
 ↓
https://127.0.0.1:8443
```

只有 Agent 根据：

```text
Tunnel ID
```

决定 Origin。

---

# 51. Origin 模型

```go
type Origin struct {
    Scheme string

    Host string
    Port uint16

    ConnectTimeout time.Duration

    TLSVerify bool

    TLSServerName string

    HTTPHostHeader string

    DisableChunkedEncoding bool

    DisableHappyEyeballs bool

    HTTPIdleConnectionTimeout time.Duration

    HTTPMaxIdleConnections uint32

    TCPKeepAliveInterval time.Duration
}
```

第一阶段：

```text
http

https

tcp
```

`Origin.Host` 接受规范化的 IP Literal 或 DNS Hostname。禁止在 Snapshot Apply 时把 DNS 永久解析并保存为单一 IP。

Agent 会按照 Server Desired State 主动访问 Origin，因此拥有 Tunnel 管理权限的 Server
和管理员属于 Connector 内网访问的受信控制面。V0.1 不默认禁止 Loopback、RFC1918 或
其他私网 Origin，否则会破坏内网穿透的核心用途；可选 CIDR、Link-local、Metadata IP
或 DNS Suffix Egress Policy 留到后续版本，不在 V0.1 提前实现。

---

# 52. Origin Connection

## HTTP

```text
net.Dialer.DialContext
```

连接 Origin。

## HTTPS

Agent：

```text
TCP Dial
 ↓
TLS Client
 ↓
Certificate Verify
```

然后向 TunnelChannel 暴露：

```text
plaintext HTTP stream
```

因此 Server HTTP Proxy 不需要知道：

```text
Origin Address

Origin TLS Configuration
```

## TCP

直接：

```text
net.Dialer.DialContext
```

所有 Origin Scheme 共用同一个 Dial Policy：

```text
每次新建 Origin Connection 时解析 DNS

使用系统 Resolver，不在 Agent 内永久缓存解析结果

DNS + IPv4/IPv6 尝试 + TCP Connect + TLS Handshake
共同受 connect_timeout Context 约束

多 A/AAAA 地址使用 net.Dialer 的并发/回退策略
不自行固定第一个地址
```

Health Check 与真实业务连接必须复用同一个 Resolver、Dialer、TLS 和 Timeout Policy，禁止各自实现不同的 DNS 缓存或地址偏好。系统 Resolver 可以按其策略缓存，但 Snapshot Apply 不得把结果固化。

HTTPS 的 SNI 与证书校验名称按以下顺序确定：显式 `tls_server_name` 优先；否则 DNS Hostname 使用 `origin_host`；IP Literal 使用 IP SAN 校验。`tls_verify=true` 且无法得到有效校验名称时必须失败，禁止自动关闭证书校验。

V0.1 冻结以下 Service Proxy 选项；未显式填写时必须使用表中默认值，不得由
Server、Agent 或 Web 各自推断第二套默认值：

| 选项 | 默认值 | 适用范围 | V0.1 语义 |
| --- | ---: | --- | --- |
| `disable_chunked_encoding` | `false` | HTTP/HTTPS | 禁止向 Origin 发出 Chunked Request；不得为计算长度而整体缓存 Body |
| `disable_happy_eyeballs` | `false` | HTTP/HTTPS/TCP | `false` 保留系统 Dialer 的 IPv4/IPv6 快速回退；`true` 按 Resolver 地址顺序建连，不并行竞速 |
| `http_idle_connection_timeout_ms` | `90000` | HTTP/HTTPS | HTTP Transport 空闲 KeepAlive Connection 的最长保留时间 |
| `http_max_idle_connections` | `100` | HTTP/HTTPS | 每个隔离池最多保留的空闲 HTTP Connection，仍受全局 WorkConn/FD 硬预算限制 |
| `tcp_keepalive_interval_ms` | `30000` | HTTP/HTTPS/TCP | Origin TCP Socket KeepAlive 间隔；`0` 显式禁用 |

`connect_timeout_ms` 的 V0.1 语义保持不变：它是从 DNS 解析开始，经 IPv4/IPv6
尝试、TCP Connect 直到 HTTPS TLS Handshake 完成的同一总预算。V0.1 不再叠加独立
TLS Timeout；`disable_happy_eyeballs` 也不得重置或延长该总预算。HTTP 专属字段出现在
TCP Service 上必须在输入边界拒绝，不得静默忽略。

---

# 53. OPEN Response

Agent：

```protobuf
message OpenResponse {
    string connection_id = 1;

    OpenStatus status = 2;

    ErrorCode error_code = 3;

    uint32 origin_connect_latency_ms = 4;
}
```

成功：

```text
OPEN
 ↓
Origin Dial
 ↓
OPEN_OK
 ↓
RAW MODE
```

失败：

```text
OPEN
 ↓
OPEN_ERROR
 ↓
CLOSE
```

---

# 54. RAW MODE

进入：

```text
OPEN_OK
```

以后：

```text
XTunnel 不再解析业务数据
```

连接：

```text
Public Client
     ↕
Server
     ↕
TLS WorkConn
     ↕
Agent
     ↕
Origin
```

全部：

```text
streaming raw bytes
```

---

# 55. Half-Close

必须支持：

```text
Client CloseWrite
```

而不直接关闭整个连接。

接口：

```go
type HalfCloseConn interface {
    net.Conn

    CloseWrite() error
}
```

代理规则：

```text
A → EOF

B.CloseWrite()

B → A
继续

两边结束
才完全 Close
```

---

# 56. Bidirectional Proxy

统一：

```go
func ProxyBidirectional(
    ctx context.Context,
    a net.Conn,
    b net.Conn,
) error
```

内部：

```text
goroutine 1
A → B

goroutine 2
B → A
```

使用：

```text
context

errgroup

CloseWrite
```

fatal error：

```text
cancel
 ↓
关闭两边
```

`ctx.Done()` 与 fatal error 使用同一关闭路径：必须主动 Close 两端或设置立即 Deadline，解除阻塞中的 `Read`/`Write`。仅调用 `cancel()` 不足以终止 `net.Conn` IO。

普通单边 EOF 不属于 fatal error，仍按 Half-Close 规则让反方向继续完成。

不得留下 orphan goroutine。

---

# 57. Tunnel

Tunnel 是持久化聚合根：它持有 Token 与 Service，并在运行时关联不入库的
ephemeral Connector：

```sql
CREATE TABLE tunnels (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    desired_revision INTEGER NOT NULL DEFAULT 0,
    revoked_at INTEGER CHECK (revoked_at IS NULL OR revoked_at > 0),
    first_authenticated_at INTEGER CHECK (
        first_authenticated_at IS NULL OR first_authenticated_at > 0
    ),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
```

Tunnel 不保存单一 `observed_revision` 或 `last_seen_at`；多个 Connector 的在线、Revision 和最近活动属于运行态，聚合摘要不得反向充当数据面裁决依据。

`first_authenticated_at` 是唯一新增的跨重启认证历史事实：它只允许从 `NULL`
变为第一次成功认证的 UTC Unix 秒，重复认证不得改写，也不推进 Tunnel Aggregate
的 `version` 或 `updated_at`。完整 Success Frame 尚未写出时不得记录；一旦对端已经
观察到成功，即使后续本地交接失败，也保留该事实。它不是在线状态、Session、
Connector 或 `last_seen_at`，不得扩展为第二套 Runtime 持久化来源。

---

# 58. Service

```sql
CREATE TABLE services (
    id TEXT PRIMARY KEY,
    tunnel_id TEXT NOT NULL,
    name TEXT NOT NULL,
    required_revision INTEGER NOT NULL DEFAULT 0,
    origin_scheme TEXT NOT NULL,
    origin_host TEXT,
    origin_port INTEGER,
    origin_path TEXT,
    origin_quic_alpn TEXT,
    tls_verify INTEGER NOT NULL DEFAULT 1,
    tls_server_name TEXT,
    origin_http_host TEXT,
    connect_timeout_ms INTEGER NOT NULL DEFAULT 5000,
    disable_chunked_encoding INTEGER NOT NULL DEFAULT 0
        CHECK (disable_chunked_encoding IN (0, 1))
        CHECK (origin_scheme IN ('http', 'https') OR disable_chunked_encoding = 0),
    disable_happy_eyeballs INTEGER NOT NULL DEFAULT 0
        CHECK (disable_happy_eyeballs IN (0, 1)),
    http_idle_connection_timeout_ms INTEGER NOT NULL DEFAULT 90000
        CHECK (http_idle_connection_timeout_ms BETWEEN 1 AND 4294967295)
        CHECK (origin_scheme IN ('http', 'https') OR http_idle_connection_timeout_ms = 90000),
    http_max_idle_connections INTEGER NOT NULL DEFAULT 100
        CHECK (http_max_idle_connections BETWEEN 1 AND 4294967295)
        CHECK (origin_scheme IN ('http', 'https') OR http_max_idle_connections = 100),
    tcp_keepalive_interval_ms INTEGER NOT NULL DEFAULT 30000
        CHECK (tcp_keepalive_interval_ms BETWEEN 0 AND 4294967295),
    health_type TEXT,
    health_path TEXT,
    health_interval_ms INTEGER,
    health_timeout_ms INTEGER,
    health_expected_status_min INTEGER,
    health_expected_status_max INTEGER,
    health_failure_threshold INTEGER,
    health_success_threshold INTEGER,
    enabled INTEGER NOT NULL DEFAULT 1,
    version INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY(tunnel_id)
        REFERENCES tunnels(id)
        ON DELETE RESTRICT
);
```

一个 Tunnel 可以拥有零到多个 Service；一个 Service 只能归属一个 Tunnel。Origin 与 Health 配置直接属于 Service，不再存在 `tunnel_bindings` 中间表。HTTP/TCP Route 外键指向 `service_id`。
数据库外键必须使用 `RESTRICT`（或 SQLite 等价的 `NO ACTION`）兜底“Tunnel
仍有 Service 时返回 409”的 REST 契约；任何删除路径都不得隐式级联 Service 或 Route。

初始 `services` Schema 为后续小版本预留 `udp`、`quic` 与 `unix` Origin，但预留不代表
V0.1 Runtime 已支持这些 Scheme。字段组合必须满足：

- `http`、`https`、`tcp`、`udp`、`quic` 使用非空 `origin_host + origin_port`，且
  `origin_path` 必须为空。
- `unix` 只表示绝对文件系统路径的 `SOCK_STREAM`，必须使用 `origin_path`，Host/Port
  必须为空；不接受抽象 Namespace 或假端口。
- `quic` 表示 Agent 原生 QUIC Dial，必须提供单个 1—255 UTF-8 字节的
  `origin_quic_alpn`；透明转发既有
  QUIC 流量使用 `udp`，不得把两种语义混为一谈。
- UDP、QUIC 与 Unix Health 语义尚未冻结，预留行只能使用 Disabled Health。
- V0.1 Application、Snapshot 与 Agent 继续只接受 `http`、`https`、`tcp`，遇到预留
  Scheme 必须 fail closed；后续版本须先扩展 Protocol 与 Runtime 契约，不能仅凭数据库
  可写就宣称功能可用。

`000005_services.sql` 属于发布前初始建表基线。修改后的内容只作用于新数据库；开发期
已记录 Migration Version 5 的数据库不会自动重放，必须删除并重建。若存在必须保留的
已部署数据，则只能新增向前 Migration，禁止依赖修改后的 Version 5 原地升级。
上述 Service Proxy 字段是 M4-03/M4-07 的 V0.1 目标形状；持久化实现必须由向前
Migration 承载；当前落盘载体是 `migrations/000008_service_proxy_options.sql`。Wire 实现
必须由 Proto 机器权威承载，后续 REST 入口再同步 OpenAPI。
本节文档本身不是 Migration、Wire 发布或 REST 入口的通过证据。

---

# 59. Connector 的 Service 配置语义

同一 Tunnel 下所有 Connector 获得相同的完整 Service Snapshot。每个 Connector 都应能按同一 Service Origin 配置访问相同语义的后端。

```text
Origin:
10.10.0.20:8080
```

所有 Connector 都应能访问。若 Origin 是 `127.0.0.1:8080`，每个 Connector 本机的该地址必须代表同一 Service。

# 60. Connector Selection

业务连接建立流程：

```text
Route
 ↓
Service
 ↓
Tunnel
 ↓
Eligible Connectors
 ↓
Connector Selection
 ↓
TCP WorkPool
```

过滤：

```text
Control Session ONLINE

Not Draining

Health Check Disabled
或该 Connector 的 Service Health == HEALTHY

Connector ObservedRevision >= Service RequiredRevision
```

---

# 61. Connector Selection Algorithm

第一阶段采用两级选择：

```text
1. Eligible 且 idle > 0
   ↓
   原子 Acquire Idle WorkConn

2. 多个 Connector 都有 Idle 时
   ↓
   Least Active Connections
```

排序：

```text
active connections
```

最少的优先。

相同：

```text
round robin
```

示例：

```text
A active = 20

B active = 5

C active = 12
```

新连接：

```text
→ B
```

如果候选 Connector 的 Idle WorkConn 被并发请求抢走：

```text
立即尝试下一个有 Idle 的 Connector
```

只有所有 Eligible Connector 都没有 Idle WorkConn 时，才进入 WorkDemand 流程。

不引入复杂动态权重算法。

---

# 62. 无空闲 WorkConn

如果：

```text
有可用 Connector

但所有 idle = 0
```

Server 按 Tunnel 聚合所有 Pending OPEN，只维护一个共享 Pending Group 和一个最新
绝对目标。首个等待者按既有 Least Active + Round Robin 规则选择一个当前最佳且未
Draining 的 Connector；后续等待者加入同一 Group，不为每个公网请求单独建立 Demand。
需求上升时更新绝对目标，Pending 减少或取消时同步降低目标。

Server：

```text
只向该 Pending Group 当前选中的一个 Connector
发送绝对目标 WorkDemand
```

等待：

```text
work_acquire_timeout
```

如果选中 Connector 的 Session 被替换、进入 Draining、关闭或无法再提交 Lease，等待者
必须 exactly-once 释放旧 Group membership，并在同一个 `work_acquire_timeout` 剩余
时间内回到 Tunnel 级选择。此处属于尚未提交 WorkConn 的 Acquire 重选，不等于发送
`OpenRequest` 后的跨 Connector 业务重试；后者仍由 M2 的 Pre-RAW Failover 契约负责。

V0.1 禁止把一次 Pending OPEN 广播给全部 Connector，也不并行维护多个投机 Demand。

---

# 63. Connector HA

如果：

```text
Tunnel A
├── Connector 1
└── Connector 2
```

Connector 1 Crash：

```text
已有 Connector 1 Connection
允许失败
```

新连接：

```text
自动选择 Connector 2
```

第一阶段因此天然支持：

```text
Connector HA
```

发送 `OpenRequest` 后只有 `AcceptRaw` 之前的 Transport 失败允许受限重选：首 Connector 最多尝试两条当前 IDLE WorkConn，第二条只做非阻塞获取；两次失败或首次返回 `OPEN_DRAINING` 后，使用同一个 `connection_id`、排除失败 Connector，并在其他当前候选中最多提交一次跨 Connector OPEN。候选 IDLE 被并发抢走、replacement 或 drain 时可以继续检查下一个候选，但不得加入第二个 Pending Group、发送新 Demand 或阻塞等待备用 Work。

Protocol Error、普通 `OPEN_ERROR`、Context Cancel、本地 MarkActive/Limit/Register 失败均不跨 Connector 重选。`AcceptRaw` 是不可逆提交点；其后的 Deadline 清理失败、RAW Proxy 失败或已经转发任意业务字节都不得重放 OpenRequest 或业务字节。

---

# 64. Desired Revision

每个 Tunnel 保存：

```text
desired_revision

```

每个 Connector 的 `observed_revision` 只保存在 Runtime；Tunnel 是否全部收敛由在线 Connector 运行态聚合，不能用单一数据库字段替代。

---

# 65. 多 Connector Revision

因为一个 Tunnel 可以拥有多个 Connector：

Server Runtime 实际维护：

```text
Tunnel Desired Revision
        │
        ├── Connector A Observed 18
        ├── Connector B Observed 18
        └── Connector C Observed 17
```

运行时以：

```text
Connector ObservedRevision
```

为准。Connector Selection 还必须满足：

```text
Connector ObservedRevision
>=
Service RequiredRevision
```

每次修改 Service 时更新其 `required_revision`。旧 Revision Connector 不得承接该 Service 的新连接。

配置发布顺序：

```text
SQLite Commit Desired State
 ↓
Build Tunnel Snapshot
 ↓
Push Snapshot
 ↓
ConfigAck
 ↓
Connector ObservedRevision 更新
 ↓
该 Connector 对对应 Service Eligible
```

不存在满足 Revision 的 Connector 时返回：

```text
503 SERVICE_CONFIG_NOT_OBSERVED
```

---

# 66. Tunnel Snapshot

```protobuf
message TunnelSnapshot {
    string tunnel_id = 1;
    uint64 revision = 2;
    repeated ServiceConfig services = 3;
}

message ServiceConfig {
    string service_id = 1;
    string origin_scheme = 2;
    string origin_host = 3;
    uint32 origin_port = 4;
    uint32 connect_timeout_ms = 5;
    bool tls_verify = 6;
    string tls_server_name = 7;
    string origin_http_host = 8;
    HealthCheckConfig health = 9;
    bool enabled = 10;
    uint64 required_revision = 11;
}

message HealthCheckConfig {
    HealthType type = 1;
    string path = 2;
    uint32 interval_ms = 3;
    uint32 timeout_ms = 4;
    uint32 expected_status_min = 5;
    uint32 expected_status_max = 6;
    uint32 failure_threshold = 7;
    uint32 success_threshold = 8;
}
```

M4-03/M4-07 的 Wire 形状必须使用类型化的 `OriginConnectionOptions` 与
`HTTPProxyOptions`：前者对所有 Service 必填，后者对 HTTP/HTTPS 必填、对 TCP 必须缺失。
字段号只由 Proto 机器权威分配，总方案不维护第二份编号，也禁止使用通用
JSON/Map 绕过 Wire Contract。

V0.1 同时限制：

```text
max_services_per_tunnel = 1000

deterministic serialized TunnelSnapshot <= 768 KiB

encoded ControlEnvelope <= MaxControlFrameSize
```

所有可能改变 Tunnel Snapshot 的 Management 写入，必须在 SQLite Commit 前从事务内 Candidate State 构建受影响 Tunnel 的完整 Snapshot，并检查 Service 数、确定性序列化大小和最终 Envelope 大小。超限返回 `422 TUNNEL_SERVICE_LIMIT` 或 `422 SNAPSHOT_TOO_LARGE`，事务不得提交。

Server 启动和 Migration 后也必须对现有数据执行同一检查；不合法时保持 Public Listener 未启动并报告具体 Tunnel/大小，禁止进入“Connector 重连 → 收到超大 Frame → 再重连”的循环。V0.1 不实现 Snapshot 分片或依赖压缩绕过上限。

---

# 67. Snapshot 传输安全

TunnelSnapshot 只允许在已经完成 TLS Server 身份验证、Tunnel Token 认证和 Auth→Established 原子切换的 Control Session 中由 Server 下发。V0.1 不再建立独立的 Config Signing Key、Server Epoch 或离线签名验证链。

配置完整性和来源认证由以下边界共同保证：

```text
TLS 1.3
+ public CA 或显式 SPKI Pin
+ Tunnel Token Authentication
+ Control Protocol Direction/State Validation
+ Recursive Unknown-field Rejection
```

Agent 不接受本地 Snapshot、旁路文件或其他进程注入的业务配置；TLS 验证失败、Pin 不匹配或 Control Message 方向非法时必须拒绝配置并关闭连接。取消独立签名不会削弱 TLS Gateway 身份校验、Token Rotate/Revoke、Session Secret、WorkHello HMAC 或 Replay Protection。

---

# 68. Full Snapshot Sync

Server 是 Tunnel Desired State 的唯一权威。每个新 Control Session 认证成功后，Server 必须下发当前完整 TunnelSnapshot，而不是要求 Agent 从本地 Revision、差量文件或 Last Known Config 恢复。

```text
Auth Success
 ↓
Server build current full Snapshot
 ↓
Agent validate + prepare/start gated immutable candidate Runtime
 ↓
Atomic Swap in memory
 ↓
ConfigAck → Connector becomes revision-eligible
 ↓
Bounded Retire previous Runtime
```

Server 可以合并尚未开始 Apply 的旧 Revision，只保留最新完整 Snapshot；一旦某个 Revision 开始 Apply，后续 Snapshot 必须串行处理，禁止并发修改 Resolver。

Candidate 在发布前必须完成所有可能失败的字段校验、Resolver 构建和 Health 资源启动，
但启动后的资源保持 unpublished/gated，不得上报 Health 或参与 Connector Selection。
Atomic Swap 与 ConfigAck 成功后才有界 Retire 旧配置和 Health 资源；Retire 不得关闭旧
Revision 已进入 ACTIVE 的 WorkConn，也不得阻塞数据面。Candidate 在 Atomic Swap 前失败时，
必须释放自身资源并保持旧 Runtime 完整可用。

---

# 69. Revision 语义

`desired_revision` 由 Server 持久化并随 Desired State 事务递增。Agent 的
`observed_revision` 只代表当前进程、当前 Control Session 已成功应用的内存配置。
Agent 在递归 Unknown Field 拒绝后，按 Deterministic TunnelSnapshot Bytes 计算并在内存
记录 `snapshot_digest = SHA-256(deterministic TunnelSnapshot bytes)`，用于判断同一
Session 内相同 Revision 的 Payload 是否等价。Digest 不进入 Agent 本地文件，也不新增
Protocol v1 Wire 字段；新 Control Session 建立基线时，必须连同 `observed_revision` 重置。

同一 Control Session 内：

```text
incoming revision > observed revision
→ 可以 Apply

incoming revision == observed revision 且内容完全相同
→ 幂等 Ack

incoming revision == observed revision 且 snapshot_digest 不同
→ PROTOCOL_ERROR

incoming revision < observed revision
→ PROTOCOL_ERROR
```

新 Control Session 必须接受 Server 认证后下发的当前完整 Snapshot 作为该 Session 的新基线，即使 Server 因显式 Backup Restore 回到了较低 Revision。Agent 不承担跨 Server Restore 的持久化反回滚；Restore 的授权、锁、Manifest、审计和一致性由 Server 端 Durable Operations 保证。

---

# 70. Config Apply

Agent：

```text
Receive full Snapshot
 ↓
Validate Frame / Unknown Fields / Tunnel ID
 ↓
Validate Revision / Service Count / Serialized Size
 ↓
Validate every Service and its Origin / Health Policy
 ↓
Build immutable Resolver + Health Plan
 ↓
Start Candidate Resources
 ↓
Atomic Swap Runtime Config + observed_revision + snapshot_digest
 ↓
ConfigAck
 ↓
Bounded Retire Previous Runtime
```

```protobuf
message ConfigAck {
    uint64 observed_revision = 1;
    ConfigApplyStatus apply_status = 2;
    ErrorCode error_code = 3;
}
```

Apply 必须先完整构建并启动不可变但 gated 的 Candidate，再通过单一原子交换发布。
任何字段、Origin、Health、资源边界或构建错误都不得发布部分结果。Candidate 发布前不得
上报 Health 或参与选择。旧 Runtime 的取消和资源回收在 ConfigAck 后触发，具有独立
Deadline 和等待路径；不得先拆旧 Resolver/Health Runtime 再构建新 Runtime，也不得关闭
已进入 ACTIVE 的旧 WorkConn。

- 有旧内存配置时：保留旧配置，发送 `CONFIG_REJECTED`，等待更高 Revision。
- 首份配置失败时：Connector 保持不可选择，发送 `CONFIG_REJECTED`；Server 不得把它标记 ONLINE/Eligible。
- Apply 成功时：先完成内存交换，再发送 `CONFIG_APPLIED` Ack；Ack 的 `observed_revision` 必须等于当前内存 Revision。

V0.1 没有 Agent 本地 Persist、fsync、rename 或 Snapshot Crash Recovery 提交点。

---

# 71. Agent Restart 与 Control Reconnect

Agent 不保存 `snapshot.pb`、`snapshot.next`、`trust-state.pb` 或 Revision 文件。

新进程启动：

```text
observed revision = none
 ↓
Connect + Authenticate
 ↓
Receive full current Snapshot
 ↓
Apply in memory
 ↓
Ack + ONLINE
```

同一进程的 Control Session 断开时，当前内存配置可以继续服务已经进入 ACTIVE 的 WorkConn，并用于安全清理；Agent 按 Backoff 重连。新 Session 建立后仍接收完整当前 Snapshot并以它重建基线，不能要求 Server 提供本地差量链。

Server 不可达时，新 Agent 不能进入 ONLINE，也不能依赖本地旧配置伪装为可服务状态。这个取舍用远端启动依赖换取无 Agent 持久状态、无本地 Desired State 和更简单的恢复模型。

---
# 72. Origin Health

Health Check 在每个 Connector 本地执行。

支持：

```text
Disabled

TCP

HTTP
```

默认：

```yaml
health:
  interval: 10s
  timeout: 2s
  failure_threshold: 3
  success_threshold: 2
```

这些字段属于 Service，必须同时进入：

```text
SQLite services

ServiceConfig Protobuf

Service API

Web Console
```

HTTP 默认期望状态范围为 `200-399`。任何自定义范围、成功阈值和失败阈值都必须持久化并随 Snapshot 下发，禁止只保存在 UI 状态中。

每个 Connector 只能运行一个中心化 Health Scheduler，禁止为每个 Service 启动独立永久 Ticker/Goroutine。调度器固定包含：

```text
Timing Heap / Timing Wheel
+
Global Semaphore
+
Per-Origin Semaphore
+
Rate Limiter
+
Report Batcher
```

Agent V0.1 Binary 的中心调度安全上限固定为：

```text
max_concurrent = 64
max_checks_per_second = 50
max_concurrent_per_origin = 4
initial_jitter = 1.0
interval_jitter = 0.2
report_flush_interval = 1s
report_batch_size = 128
```

这些值不是 Agent 本地配置项。每个 Service 的 Health 行为随远端 Snapshot 下发；Server 的全局预算与 Agent Binary 的固定上限共同限制实际调度。需要调整硬上限时必须通过版本发布和容量基准，而不是在每台 Agent 上维护配置漂移。

首次检查在 `[0, interval]` 均匀随机分散；后续检查间隔为 `interval × random(0.8, 1.2)`。Rate/Concurrency 已满时，Scheduler 只能在仍可满足该 Service 的配置 Interval 和 Stale TTL 时短暂排队；无法满足时必须报告 `HEALTH_BUDGET_EXCEEDED`，不能静默把 10 秒检查拖成更长周期后继续显示正常。

V0.1 产品容量约束：

```text
health-enabled services × online connectors
<= 2000 / tunnel

global scheduled health targets
<= 50000 / Server
```

Management 写入必须按事务内 Candidate Service 与当前在线 Connector 数预检；新 Connector 认证导致预算超限时，Server 返回可重试的 `HEALTH_BUDGET_EXCEEDED` Auth Failure 和 `retry_after_ms`，不建立半可用 Session。相关数值属于第 156 节唯一 Limits 契约，可经 Benchmark 调整，但不得移除这一预算维度。

Management Candidate 和 Control Auth 必须通过同一个内存 `HealthTargetBudgetManager` 执行原子 `Reserve → Commit/Release`，禁止先分别读取计数再决定。配置写入先按 Candidate Delta 预留，再执行短 SQLite 事务；事务失败释放 Reservation，提交成功后 Commit 并触发 Reconcile。

Runtime Health Budget 的唯一所有权 Key 是 `(tunnel_id, connector_id)`，`session_generation` 只用于 fencing，不作为额外计费对象。首个 Session 在发布到 Runtime Registry 前，按该 Tunnel 当前 health-enabled Service 数预留并 Commit；同一 Connector 重连时，必须按 `TunnelRuntime.mu → HealthTargetBudgetManager.mu` 的唯一锁顺序将已有 Reservation 原子转移给新 generation，不得重复计费。`HealthTargetBudgetManager.mu` 持有期间禁止反向获取任何 `TunnelRuntime.mu`；需要跨多个 Tunnel 重算时，必须先在各 Tunnel Lock 内生成不可变 Delta，释放 Tunnel Lock 后再单独进入 Budget Lock，不允许同时持有多个 Tunnel Lock。旧 generation cleanup 因 CAS 失败时不得释放新 generation 持有的 Reservation；只有 Connector Runtime 最终删除或 Tombstone 结束时才按所有权 Key 释放。首次预留失败则发送 Auth Failure，不发布半可用 Session。Budget Lock 内禁止 SQLite、网络 IO 或等待 Channel。Server Restart 从 SQLite Desired State 和重建中的唯一 Connector Runtime 重新计算，任何计数不变量破坏都阻止对应 Session/Config 发布，而不是产生负数或超额 Target。

---

# 73. Health 状态

```text
UNKNOWN

HEALTHY

UNHEALTHY
```

启用 Health Check 时：

```text
UNKNOWN 不参与 Connector Selection

UNKNOWN 首次成功
→ HEALTHY

UNKNOWN 连续 failure_threshold 次失败
→ UNHEALTHY

HEALTHY 连续 failure_threshold 次失败
→ UNHEALTHY

UNHEALTHY 连续 success_threshold 次成功
→ HEALTHY

任一相反结果都会清零当前连续计数

Server 本地 received_at 超过 2 × interval
且未收到同 service_revision 的新 Health Report
→ UNKNOWN
```

如果：

```text
Health Check Disabled
```

视为：

```text
HEALTHY
```

---

# 74. TCP Health

```text
Dial Origin
 ↓
Connected
 ↓
Close
```

只验证：

```text
TCP Reachability
```

---

# 75. HTTP Health

默认：

```text
GET /health
```

健康：

```text
200-399
```

支持配置：

```text
Path

Timeout

Interval

Expected Status Range
```

---

# 76. Service Health Report

```protobuf
message ServiceHealth {
    string service_id = 1;

    HealthStatus status = 2;

    uint32 latency_ms = 3;

    string error_code = 4;

    uint64 checked_at_ms = 5;

    uint64 service_revision = 6;
}

message ServiceHealthBatch {
    uint64 generation = 1;
    repeated ServiceHealth items = 2;
}
```

Health 是：

```text
Per Connector
Per Service
```

而不是只有 Tunnel 级状态。

Health Report 必须绑定产生它的 `service_revision`，值取对应 `ServiceConfig.required_revision`。Server 只接受它与 SQLite/Runtime 中该 Service 当前 `required_revision` 完全相等的报告；旧 Revision 或未知 Revision 全部丢弃，不能覆盖新状态。`checked_at_ms` 只用于 UI/日志展示，不参与新旧裁决；Health 新鲜度使用 Server 本地 monotonic `received_at` 与配置的 Stale TTL 判断，禁止比较 Agent 与 Server 的 wall clock。Tunnel Revision 因其他 Service 变化而递增时，未变化 Service 的 Health Checker 不必重启。

Agent 应用新 Snapshot 时，必须先取消受影响 Service 的旧 Health Checker，并把对应状态原子重置为 `UNKNOWN`，再启动带新 Revision 的检查。启用 Health Check 的 Service 只有在新 Revision 首次检查成功后才可 Eligible；旧 Origin 的 HEALTHY 不能沿用到新 Origin。

Agent 不逐条发送 Health Frame。Report Batcher 在 `report_flush_interval` 到达、累计达到 `report_batch_size`，或进入 Drain 前生成 `ServiceHealthBatch`；同一批次内每个 Service 只保留最新结果。`generation` 在当前 Control Session 内严格递增，Server 丢弃重复或倒退 Batch，但仍以每个 Item 的 `service_revision` 作为配置新旧的最终裁决。Batch 序列化后仍必须小于 MaxControlFrameSize；超限时按不超过 `report_batch_size` 的子批次拆分，并为每个 Frame 使用新 generation。

同一 Agent 进程在 Control Session 重连后，如果完整 Snapshot Apply/幂等确认成功，必须在
`ConfigAck` 后立即把当前 Revision 下所有 Service 的最新 Health 状态，通过 Owner/Outbox
按新 Session 的 generation 序列做一次完整 Batch Flush，不能等待下一次 Health Interval。
受 Snapshot 影响而已经重置为 `UNKNOWN` 的
Service 上报 `UNKNOWN`；未变化 Service 可以上报进程内仍有效且 Revision 匹配的状态。
这样 Server Restart 后可以恢复 Runtime Health，但绝不能把 `UNKNOWN` 临时当作
`HEALTHY`。

Server 在 Service `required_revision` 变化时也必须立即把所属 Tunnel 的所有 Connector 对该 Service 的 Runtime Health 重置为 `UNKNOWN`。Connector Selection 除检查状态为 HEALTHY 外，还必须检查已存 Health 的 `service_revision == Service required_revision`，因此 ConfigAck 先于新 Health Report 到达也不会短暂放行旧 HEALTHY。

---

# 77. HTTP Public Ingress

HTTP 请求链路：

```text
Browser
  ↓
HTTPS
  ↓
Caddy / Nginx
  ↓
HTTP
  ↓
127.0.0.1:8081
  ↓
XTunnel HTTP Router
  ↓
Route → Service
  ↓
Tunnel
  ↓
Connector
  ↓
WorkConn
  ↓
Agent
  ↓
Origin
```

生产支持边界固定为：HTTP Ingress 默认只监听 Loopback，并由 Caddy/Nginx 等 HTTPS
前置代理访问。若部署者显式绑定非 Loopback 地址，Server 必须输出明确 Security
Warning，且 `http_ingress.trusted_proxies` 必须与真实拓扑一致。V0.1 不把明文 HTTP
Ingress 直接暴露到公网作为受支持部署方式。

---

# 78. XTunnel 不管理公网 HTTPS

V0.1 不负责：

```text
ACME

公网 HTTPS Certificate

TLS Renewal

Wildcard Certificate
```

这些由：

```text
Caddy

Nginx + Certbot

其他负载均衡器
```

负责。

---

# 79. 推荐域名模型

最简单方式：

```text
*.tunnel.example.com
```

统一：

```text
DNS → Server
```

Caddy：

```text
*.tunnel.example.com
       ↓
127.0.0.1:8081
```

然后 XTunnel 根据：

```text
Host
```

匹配 Tunnel。

---

# 80. 自定义域名

V0.1 支持自定义 Host Route：

```text
app.company.com
```

但：

```text
DNS

TLS Certificate

Caddy/Nginx Route
```

由运维人员负责配置。

XTunnel 只负责：

```text
Host → HTTP Route → Service → Tunnel
```

---

# 81. HTTP Route

```sql
CREATE TABLE http_routes (
    id TEXT PRIMARY KEY,

    service_id TEXT NOT NULL,

    hostname TEXT NOT NULL,

    path_prefix TEXT NOT NULL DEFAULT '/',

    preserve_host INTEGER NOT NULL DEFAULT 1,

    enabled INTEGER NOT NULL DEFAULT 1,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,

    FOREIGN KEY(service_id)
        REFERENCES services(id)
        ON DELETE RESTRICT,

    UNIQUE(hostname, path_prefix)
);
```

HTTPS 开关不需要放进 Route。

TLS 已经属于前置代理。

---

# 82. HTTP Route Matching

匹配：

```text
Exact Canonical Host（请求端口移除后）
+
Longest Path Prefix
```

例如：

```text
api.example.com/
api.example.com/admin
```

请求：

```text
/admin/users
```

匹配：

```text
/admin
```

请求 `/admin/` 及其子路径同样匹配 Canonical Prefix `/admin`。

V0.1 不实现：

```text
Regex Route

复杂 Predicate

Header Route

Method Route
```

Path Prefix 写入和匹配前必须使用同一套规范：

```text
必须以 / 开头

只按 URL Path 语义匹配，不使用文件系统路径规则

拒绝控制字符、反斜杠和非法 percent-encoding

拒绝明文或编码后的 dot segment、encoded slash 和 encoded backslash

Prefix 必须按路径段边界匹配
```

Canonical Path Prefix 规则：根路径只能存为 `/`；非根 Prefix 移除所有尾部 `/` 后保存。因此 `/admin` 与 `/admin/` 在写入边界规范化为同一个 `/admin`，数据库唯一约束禁止重复语义 Route。请求 `/admin`、`/admin/` 和 `/admin/...` 都匹配该 Prefix，但 `/administrator` 不匹配。

例如 `/admin` 只匹配 `/admin` 和 `/admin/...`，不得匹配 `/administrator`。

Router 对公网请求不得使用 `path.Clean` 或文件系统路径规则自动改写。对重复斜杠、
Trailing Slash 和保留字符不做隐式等价折叠；`/foo`、`/foo/`、`/foo//bar` 可以具有
不同的 Origin 语义。Router 与转发给 Origin 的路径必须来自同一次 Parse/Validate 的
结果。`RawPath` 为空是 Go HTTP 请求的正常输入；非法 percent-encoding、encoded
slash/backslash、明文或编码 dot-segment、控制字符、非法 UTF-8、request-target 中明文
fragment 分隔符、多重编码后会形成上述危险路径的输入，以及非空 `RawPath` 与
`URL.Path`/`RequestURI` 无法保持一致解释时，统一返回 `400 INVALID_PATH`，不得
normalize-and-hope。

---

# 83. Host Normalization

进入 Router 前：

```text
lowercase

strip trailing dot

strip port

IDNA normalize

validate hostname
```

规范化必须发生在 API 写入边界，并且 SQLite 只保存规范化后的 hostname。数据库唯一约束与 Router 必须使用同一个 canonical hostname，禁止同时保存大小写、尾点或 IDNA 表达不同但语义相同的 Host。

拒绝：

```text
非法 Host

控制字符

畸形域名
```

---

# 84. Route Snapshot

公网请求热路径：

```text
绝不查询 SQLite
```

Server：

```go
type RouteSnapshot struct {
    HTTP map[string]*HostRoutes

    TCP map[uint16]*TCPRoute

    Tunnels map[string]*TunnelRuntime
}
```

使用：

```go
atomic.Pointer[RouteSnapshot]
```

---

# 85. Route 更新

全局 Route Generation 持久化为 SQLite 单行权威，禁止为不同入口建立独立代次：

```sql
CREATE TABLE route_config_state (
    id INTEGER PRIMARY KEY NOT NULL CHECK (id = 1),
    generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0)
);

INSERT INTO route_config_state(id, generation) VALUES (1, 0);
```

`http_routes`、`tcp_routes` 与该单行 generation 必须在同一个
`BEGIN IMMEDIATE` 事务中提交；运行时只消费提交后的完整状态。

Route Desired State 还包含 Tunnel 与 Service 绑定。任何会改变 Server Route Snapshot
所缓存字段的 Service Mutation（包括 Enabled、Origin、Proxy Options、
RequiredRevision 和 Service Delete）也必须在同一事务推进 generation；不能只在
`http_routes`/`tcp_routes` 行变化时通知 Route owner，否则在线快照会永久保留旧值。

TCP Route 创建输入未指定 `public_port` 时，ConfigWriteCoordinator 在
Normalize/Validate 阶段从 `tcp_ingress.min_port..max_port` 逻辑池分配具体端口；
事务内基于最新完整 Desired State 再次裁决，并把该端口、所属 Service 新版本、
Tunnel 新 `desired_revision` 和全局 Route Generation 一次提交。当前快照已经可以
确定的池耗尽或显式端口范围/保留/占用冲突必须在事务前拒绝；事务内重验发现并发
冲突时整笔回滚。两类失败都不得推进任何版本或 dirty 状态。全局 Route Generation
只是 Server Reconcile fencing，不是客户端 ETag；互不相关 Route 写入不得因它推进
而相互报版本冲突。

```text
API Write
 ↓
ConfigWriteCoordinator（Server 内单写入协调器）
 ↓
Normalize + Validate Input / Mutation
 ↓
BEGIN IMMEDIATE
 ↓
Commit Desired State + Increment config_generation
 ↓
Mark Reconcile Dirty(config_generation)
 ↓
Single Reconcile Loop 从 SQLite 读取最新完整状态
 ↓
Build + Validate Full Route Snapshot
 ↓
Atomic Swap（仅当 generation 仍是最新）
```

所有 Management 配置写入必须通过同一个 `ConfigWriteCoordinator` 串行进入事务；Handler 不得绕过它直接提交。事务前的 Candidate 只能校验本次输入和增量，绝不能成为发布产物。提交后的唯一发布路径由单 Reconcile Loop 从权威 SQLite 重建完整 Snapshot，避免并发请求都从旧 Runtime 基线构建 Candidate 而丢失对方已提交的 Route。

多个 dirty wakeup 只合并为一次最新 generation 的构建；Build 期间 generation 前进时，
必须丢弃旧结果并立即按最新 generation 重建，旧 Candidate 不得覆盖更新的 Desired State。
V0.1 不为此提前增加 `coalesce_window` 等用户配置项。

输入格式、Canonical Host、端口和增量冲突等可预判错误必须在提交前返回。完整 Snapshot 构建若因数据库内部不一致失败，Desired State 保留并进入 `APPLY_FAILED`，Reconcile 不得发布部分结果；修复后按最新 generation 重试。最终 Atomic Swap 本身必须是不可失败操作。

TCP `Listen` 等无法与 SQLite 原子提交的外部副作用使用 Desired/Actual 状态：

```text
desired = ENABLED
actual = APPLYING | ACTIVE | APPLY_FAILED
```

Actual 只包含当前完整 Snapshot 中启用且合法的 Route。逻辑端口池不会在启动时预先
`Listen` 整段范围；未分配、禁用和已删除 Route 都不持有 Socket 或 Listener FD。
Server 启动 FD Gate 仍按整个逻辑池容量并额外包含一个原子换口候选预留上界，使
后续新增 Route 不依赖启动时数据库中已有 Listener 数量。

API 返回 Desired State 写入结果；Service Status 明确展示 Runtime Apply Error，不得返回“失败”但暗中保留已提交配置。

老请求继续使用：

```text
旧 Snapshot
```

新请求：

```text
立即使用新 Snapshot
```

若该 Route 指向的 Service 尚未被任何 Connector 观察到所属 Tunnel 的新 Revision，则 Route 可以进入 Snapshot，但请求必须返回 `SERVICE_CONFIG_NOT_OBSERVED`，禁止回落到旧 Revision Connector。

---

# 86. HTTP Reverse Proxy

Server 使用：

```text
net/http

httputil.ReverseProxy
```

自定义：

```text
Transport

DialContext
```

DialContext：

```text
忽略目标 Socket 地址
 ↓
读取 Tunnel ID
 ↓
TunnelDialer
```

HTTP Connection Pool 必须按不可变 Service 配置隔离。隔离键至少包含：

```text
TunnelID + ServiceID + ConfigVersion
```

实现可将该 tuple 编码为只在进程内使用的 opaque Transport/Pool Key，但它不发送给
Origin。不得只把 Tunnel ID 或 Service ID 放进 `context.Context`，因为 KeepAlive
复用连接时不会再次调用 DialContext。新配置版本发布后不得把旧池 WorkConn 借给新版本。
请求已经匹配到 Route 后，新建 WorkConn 只能选择该 Service `RequiredRevision` 与 Route
配置版本精确相等的 Connector；Tunnel 的 `ObservedRevision` 可以因其他 Service 更新而
更高，但不得据此把当前 Service 的其他 Revision 借给旧 Route。

一个 WorkConn 对应一条 HTTP/1.1 TCP Connection，而不是一个 HTTP
Request。同一 `TunnelID + ServiceID + ConfigVersion` 的顺序请求可以由
`http.Transport` KeepAlive 复用已经建立的 WorkConn；V0.1 的限制是单条
HTTP/1.1 Connection 不支持并发 Multiplex。E2E 必须验证同隔离键连续请求复用，
且任何情况下不得跨 Tunnel、Service 或配置版本复用连接。

每个隔离池的空闲连接数受 Service `http_max_idle_connections` 限制，但所有池的 WorkConn、
Idle Connection 与 FD 总量仍必须服从全局硬预算，不得以“每池未超限”绕过全局上限。
请求的实际 `Host` Header 按第 90 节优先级单独设置，不参与连接池 Key。

ReverseProxy 默认设置有限正间隔：

```text
FlushInterval = 100ms
```

对 `text/event-stream`、未知 Content-Length 或明确 Streaming Response，使用 `ReverseProxy` 的 streaming 识别结果立即 Flush；WebSocket Upgrade 走双向流复制。禁止为了省事对所有普通响应全局 `FlushInterval = -1`，避免每次小 Write 都触发额外 syscall。E2E 必须覆盖“已知 Content-Length、每秒写入小于缓冲区的小块响应”，验证首块延迟不超过 FlushInterval + 调度余量。

---

# 87. Origin HTTPS

因为 Origin TLS 在 Agent 建立：

```text
Server ReverseProxy
```

内部始终视 Tunnel 为：

```text
HTTP byte stream
```

即使 Origin：

```text
https://127.0.0.1:8443
```

也是：

```text
Server
  ↓ plaintext HTTP over tunnel
Agent
  ↓ TLS
Origin
```

这样 Server 数据面不需要 Origin TLS 信息。

---

# 88. Forwarded Headers

XTunnel 前面存在：

```text
Caddy / Nginx
```

因此不能简单删除所有：

```text
X-Forwarded-*
```

必须引入：

```yaml
http_ingress:
  trusted_proxies:
    - 127.0.0.1/32
    - ::1/128
```

---

# 89. Trusted Proxy 规则

首先删除客户端直接提供的：

```text
Forwarded

X-Real-IP

所有无法通过受信代理链验证的 X-Forwarded-*
```

如果请求 TCP Peer：

```text
属于 trusted_proxies
```

则从右向左解析代理链：

```text
Peer IP
 ↓
依次剥离属于 trusted_proxies 的地址
 ↓
第一个不受信地址 = original client
```

`X-Forwarded-For` 只接受一个 Header 行，该行内部允许 1 至 32 个逗号分隔的纯 IP
地址。实际 Peer 是代理链的可信锚点；从最靠近 XTunnel 的右侧地址开始剥离可信代理，
遇到第一个不受信地址后停止，更左侧值视为该 Client 可伪造输入并忽略。受信 Peer 未提供
`X-Forwarded-For` 时使用 Peer IP；链中全部地址都属于 `trusted_proxies` 时使用最左侧、
也就是最远端的受信地址。

只有最靠近 XTunnel 的受信代理提供的协议和 Host 元数据可以作为：

```text
X-Forwarded-Proto

X-Forwarded-Host
```

`X-Forwarded-Proto` 只接受单值 `http` 或 `https`；`X-Forwarded-Host` 必须通过与
公网 Host 相同的严格 HTTP authority 校验，尤其 IPv6 必须使用方括号。两者缺失时分别
使用直连请求 Scheme 和原始 Host。

客户端位于最外层的不可信 X-Forwarded-For 值不得直接成为 original client。

否则：

```text
删除所有外部 Forwarded Header
 ↓
使用 Peer IP 重新生成
```

未受信 Peer 提供的 Forwarded Header 不解析其内容，直接删除后重建。受信 Peer 路径中，
重复 Header 行、`X-Forwarded-Proto`/`X-Forwarded-Host` 逗号多值、空值、非法 IP 或
超过 32 跳的代理链一律返回 `400 INVALID_FORWARDED_HEADER`；不得在歧义输入中选择
第一个值或降级猜测。

---

# 90. 转发给 Origin 的 Headers

最终：

```text
X-Forwarded-For
= original client

X-Forwarded-Proto
= original scheme

X-Forwarded-Host
= original Host
```

Host 的唯一优先级是：

```text
1. Service.origin_http_host 非空
   → Host = Service.origin_http_host

2. 否则 preserve_host = true
   → Host = 公网请求 Host

3. 否则
   → Host = 规范化 Origin host[:port]
```

`origin_http_host` 是 HTTP/HTTPS Service 的显式覆盖，即使 `preserve_host=true` 也优先。
未提供显式覆盖且 `preserve_host=false` 时不得留空 Host，必须使用规范化 Origin
`host[:port]`。TCP Origin 禁止设置 `origin_http_host` 或 `preserve_host`。

该值由 Server 组装 HTTP 请求 Header 使用，但不出现在 OpenRequest。Agent 只按
`service_id` 从已原子应用的 Tunnel Snapshot 解析并连接对应 Origin。

---

# 91. HTTP Streaming

严格禁止：

```text
读取整个 Request Body
```

或者：

```text
读取整个 Response
```

全部：

```text
streaming
```

`disable_chunked_encoding=true` 只改变转发给 HTTP/HTTPS Origin 的 Request Framing，不改变
“全链路 Streaming”的不变量。只有已有可信 `Content-Length` 或能在不读取 Body 的情况下
安全确定长度时才可转发；长度无法安全确定时必须显式拒绝请求，禁止先整体缓存
再计算长度。Chunked Origin Response 继续按流式读取，不得为删除 Transfer-Encoding 而整体缓存。

必须通过：

```text
1GB Upload

1GB Download
```

测试。

---

# 92. WebSocket

不设计独立 Tunnel Protocol。

链路：

```text
WebSocket Upgrade
 ↓
ReverseProxy
 ↓
TunnelChannel
 ↓
Agent
 ↓
Origin
```

入口只把 HTTP/1.1 `GET`、`Connection` 含 `upgrade` token、单值
`Upgrade: websocket` 且没有 Request Body/Transfer-Encoding 的请求识别为 WebSocket；
`Sec-WebSocket-*` 继续透明转发并由 Origin/Client 按 WebSocket 协议裁决。已知长度超过
Server Body 上限的握手先按通用入口边界返回 `413 REQUEST_BODY_TOO_LARGE`；其余带 Body
的 WebSocket 握手、h2c、HTTP/2 或其他 Upgrade 都在 Tunnel Dial 前返回
`501 UPGRADE_NOT_SUPPORTED`。带 Body 的两种拒绝都关闭客户端连接复用，避免请求体写入
与 101 后的双向复制竞争同一 WorkConn。

每次 WebSocket Upgrade 使用一条 fresh HTTP/1.1 Transport/WorkConn，不进入普通
KeepAlive 池，也不在握手字节已经写出后跨 WorkConn 重试。Origin 响应头受 10 秒
阶段预算约束；101 后 Client 与 Tunnel backend 共用 1 小时 sliding idle window，
任一方向成功读写真实字节都会同时推进两端 Deadline。该窗口只限制双方完全无字节
进展，不是 WebSocket 总生命周期时限；Ping/Pong 与业务帧都属于进展。

Client 或 Origin 单边 EOF 继续遵守 TCP Half-Close，允许反方向完成；完整断连、idle
到期、Context Cancel 或 Shutdown Hard Deadline 必须主动关闭两端。Hijack 后的
Handler 仍由 HTTP request owner 计数，Graceful Shutdown 先等待自然结束，到期后
取消基础 Context 并等待 Client、WorkConn 和 Handler 全部归零。

需要测试：

```text
Upgrade

Bidirectional Message

Ping/Pong

Long Connection

Client Close

Server Close

Agent Disconnect
```

---

# 93. Public TCP Route

TCP Route：

```text
Public Port
 ↓
Tunnel
```

例如：

```text
10022
 ↓
SSH
 ↓
Agent
 ↓
192.168.10.20:22
```

`OpenRequest.client_addr` 只用于 Agent 日志、Tracing 和审计。V0.1 不向 TCP Origin 注入 PROXY Protocol，Origin 看到的对端地址是 Agent；需要真实客户端 IP 做访问控制的 Origin 不属于 V0.1 透明转发能力范围。未来如支持，只能在每个 Service 显式 opt-in，并且 Origin 必须明确声明接受 PROXY v1/v2，禁止默认注入。

---

# 94. TCP Route Schema

```sql
CREATE TABLE tcp_routes (
    id TEXT PRIMARY KEY,

    service_id TEXT NOT NULL,

    public_port INTEGER NOT NULL UNIQUE,

    enabled INTEGER NOT NULL DEFAULT 1,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,

    FOREIGN KEY(service_id)
        REFERENCES services(id)
        ON DELETE RESTRICT
);
```

“未指定端口”只存在于未来 Management Create 请求的输入层；Normalize/Validate
必须在提交前把它解析为具体端口。持久化层继续以 `public_port NOT NULL UNIQUE` 为
唯一权威，不允许用 `NULL`、`0` 或额外 allocator 表表达尚未分配状态。

---

# 95. TCP Listener Manager

```text
Desired TCP Routes
       ↓
Reconcile
       ↓
Actual Listener
```

新增：

```text
Listen(port)
```

删除：

```text
Stop Accept
```

已经建立连接：

```text
继续运行
```

直到自然关闭。

单个 `Listen(port)` 在启动或 Reconcile 中失败时，只把对应 TCP Service 标记为
`APPLY_FAILED`，记录稳定错误码 `LISTEN_FAILED`、Route、端口、当前 Route Generation
和最近失败时间；同一 Service 任一当前 Route 失败即可使该 Service 进入
`APPLY_FAILED`。Management、Agent Gateway、HTTP Ingress 及其他成功 Listener
继续启动。Route Config Write 的 dirty 通知立即触发 Reconcile，Periodic Reconcile
继续重试仍失败的 Desired Route。只有全局 Listener 配置无效、必需基础端口失败或
数据库/身份初始化失败，才阻止 Server READY；原始 OS 错误文本不属于稳定 API 契约。

端口 A→B 时必须先成功绑定并发布 B，再停止 A；B 失败时 A 继续服务，并同步使用
当前 Service/Tunnel/RequiredRevision 等非端口 Route 值。Route 删除、
禁用、Service 禁用或 Tunnel 撤销则立即停止旧入口，不等待替代 Listener。同端口只
更新 Route 内容时复用既有 Socket 并原子替换 Route 指针。连接登记和 WaitGroup 增加前
必须重读当前 Route Snapshot，并要求其 Generation 与 Listener 已发布 Generation 精确
相等；该读取是准入线性化点。已经可见的新代次拒绝旧 Listener 准入；若新代次在读取
之后发布，本次连接视为发布前已准入，继续使用当时捕获的旧 Route 值。

Listener、Accept owner 和连接 Handler 都有唯一 owner、取消路径与同步 Wait。Listener
或 Accept owner 无法收敛的 Close 失败必须保留 residual ownership 并向 Runtime owner
报告，不得静默遗失 FD。普通客户端连接在准入拒绝或 Handler 正常返回后的 Close 错误
只由 Manager 最终 Close 聚合，不进入进程 Fatal Runtime Channel；Handler panic 仍为
fatal，并与同次连接 Close 错误合并上报。

TCP Listener 在 Accept 时把不可变 Route 值复制为 `DialRequest`，并将真实
Public Peer 一并交给 Tunnel Proxy。Proxy 内部 10 秒 Pre-OPEN Context 只约束
Work Acquire 与 OPEN；OPEN_OK 后 ACTIVE 回到 Listener Manager Context，不受该 Timer
限制。ActiveWork 同时注册 WorkConn、Public Peer 与取消句柄，使 Tunnel
Revoke、Drain 和 Shutdown 都能在锁外主动解阻两端 IO；正常双 EOF 则继续
通过同一 exactly-once Finish 路径释放 Lease、连接和配额。

---

# 96. TCP Port Range

默认：

```yaml
tcp_ingress:
  bind: "0.0.0.0"

  min_port: 10000
  max_port: 60000
```

`min_port..max_port` 是包含两端的逻辑预留池，不是启动时预监听的物理 Socket 集合。
创建 TCP Route 时既可显式指定池内端口，也可省略端口并由 Server 确定性选择一个
未被任意 Desired Route 占用且不在保留集合中的端口；提交后数据库始终保存非零具体
端口。禁用 Route 继续占用其逻辑端口，删除 Route 才释放；数据库唯一约束是最终并发
兜底。分配阶段不试绑 OS Socket，外部进程占用等系统冲突在 Listener Reconcile 中
进入 `APPLY_FAILED` 并周期重试，不回滚 Desired State。

禁止：

```text
80

443

Agent Gateway Port

Management Port

HTTP Ingress Port
```

冲突。

---

# 97. HTTP 错误语义

Tunnel Offline：

```text
503
TUNNEL_OFFLINE
```

Origin Refused：

```text
502
ORIGIN_REFUSED
```

Origin Timeout：

```text
504
ORIGIN_TIMEOUT
```

No Capacity：

```text
503
WORK_POOL_EXHAUSTED
```

Config Not Observed：

```text
503
SERVICE_CONFIG_NOT_OBSERVED
```

Service Disabled：

```text
503
SERVICE_DISABLED
```

Route Not Found：

```text
404
```

---

# 98. TCP 错误语义

TCP Route 遇到：

```text
Tunnel Offline

Origin Unavailable

Capacity Exhausted
```

直接：

```text
Close Connection
```

并在 Server 日志记录明确：

```text
error_code
```

---

# 99. Protocol Error Codes

```text
0x0000 OK

0x1001 SERVICE_NOT_FOUND
0x1002 SERVICE_DISABLED
0x1003 TUNNEL_OFFLINE
0x1004 NO_HEALTHY_CONNECTOR
0x1005 SERVICE_CONFIG_NOT_OBSERVED

0x2001 ORIGIN_REFUSED
0x2002 ORIGIN_TIMEOUT
0x2003 ORIGIN_UNREACHABLE
0x2004 ORIGIN_RESET
0x2005 ORIGIN_TLS_ERROR

0x3001 WORK_POOL_EXHAUSTED
0x3002 CONNECTOR_BUSY
0x3003 OPEN_DRAINING
0x3004 HEALTH_BUDGET_EXCEEDED

0x4001 TOKEN_INVALID
0x4002 TOKEN_REVOKED
0x4003 TUNNEL_REVOKED
0x4004 SESSION_INVALID
0x4005 SESSION_RESOURCE_EXHAUSTED

0x5001 PROTOCOL_ERROR
0x5002 VERSION_UNSUPPORTED

0x6001 INTERNAL_ERROR
```

以上分类和值必须在 M0.5 原样写入 `common.proto/ErrorCode`。M0.5 完成后，`.proto` 是唯一 Wire Authority；本文仅保留便于架构阅读的分类镜像，并由 CI 生成检查保证与 Proto 一致。已发布数值永久不得复用。

---

# 100. Connection ID

所有公网连接生成：

```text
conn_<ULID>
```

例如：

```text
conn_01ARZ3NDEKTSV4RRFFQ69G5FAV
```

完整链路：

```text
Public
 ↓
Route
 ↓
Service
 ↓
Tunnel
 ↓
Connector
 ↓
Origin
```

始终携带：

```text
connection_id

trace_id
```

---

# 101. Service 聚合模型

Service 是 Tunnel 下的独立 Aggregate。它直接保存 Origin、Health、Enabled 和 Required Revision，并由 HTTPRoute 或 TCPRoute 指向；不再创建或维护中间关联资源。

```text
Tunnel
└── Service
    ├── Origin / Health
    └── HTTPRoute 或 TCPRoute
```

---

# 102. Service 类型

V0.1：

```text
HTTP Service

TCP Service
```

HTTP Origin：

```text
http

https
```

TCP Origin：

```text
tcp
```

---

# 103. Service 创建

例如：

```json
{
  "name": "jenkins",

  "tunnel_id": "tun_01ARZ3NDEKTSV4RRFFQ69G5FAV",

  "origin": {
    "scheme": "http",
    "host": "127.0.0.1",
    "port": 8080
  },

  "exposure": {
    "type": "http",
    "hostname": "jenkins.tunnel.example.com",
    "path_prefix": "/"
  },

  "health": {
    "type": "HTTP",
    "path": "/health",
    "interval_ms": 10000,
    "timeout_ms": 2000,
    "expected_status_min": 200,
    "expected_status_max": 399,
    "failure_threshold": 3,
    "success_threshold": 2
  }
}
```

POST/PATCH Service DTO 与 `ServiceConfig` 使用同名 Health 字段。边界校验固定为：

```text
1000 <= interval_ms <= 3600000

100 <= timeout_ms < interval_ms

1 <= failure_threshold <= 20

1 <= success_threshold <= 20

100 <= expected_status_min <= expected_status_max <= 599
```

未提供 Health 时显式落库为 Disabled；提供部分 Health 字段时由 Application Service 补全默认值后再持久化，API GET 必须返回完整有效值，保证往返一致。

`exposure` 是 Service 内嵌的封闭联合类型，Wire discriminator 固定为小写
`http|tcp`，不公开内部 Route ID。Create 必须提供一个 Exposure；PATCH 中 omitted
表示不修改、`null` 表示移除公网 Exposure、对象表示按最终 `type` 校验并替换。
HTTP 返回 canonical `hostname`、`path_prefix` 与 `preserve_host`；TCP 返回
`public_port`。HTTP `preserve_host` 默认 `true`；TCP Create 省略 `public_port` 时由
Config Write Coordinator 从配置端口池原子分配。删除 Service 必须在同一事务先清理其
Exposure，不能留下 Route 引用。

SQLite 必须以单表唯一索引和跨 `http_routes`/`tcp_routes` 的双向写入约束共同保证
每个 Service 最多只有一个 Exposure。升级时若历史数据已经存在同表重复或跨表重复，
Migration 必须整体回滚并拒绝启动，不能任意保留一条或静默修复。

Server Transaction：

```text
BEGIN IMMEDIATE

next_revision = tunnel.desired_revision + 1

INSERT service(tunnel_id, required_revision = next_revision)

INSERT http_route

SET tunnel.desired_revision = next_revision

SET route_config_state.generation = generation + 1

COMMIT
```

Revision 规则：

```text
修改 Origin / Proxy Options / Health / Service Enabled
→ 在同一事务递增所属 Tunnel Revision、Route Generation，并更新 required_revision；
  Agent Snapshot 和 Server Route Snapshot 分别由各自 dirty 通知收敛

切换 Service 所属 Tunnel
→ V0.1 禁止；必须在目标 Tunnel 新建 Service，再显式删除旧 Service

创建、修改或删除 HTTP/TCP Route（包括只修改 Host / Path / Public Port）
→ 在同一事务递增 Service Version、所属 Tunnel Revision 与 Route Generation；
  Agent Snapshot 和 Server Route Snapshot 分别由各自 dirty 通知收敛

删除 Service
→ 递增所属 Tunnel Revision，使全部 Connector 删除本地 Service
```

PATCH、enable、disable、delete 必须复用同一 Application Service 事务规则，禁止各 Handler 自行决定是否递增 Revision。

这些 Service Mutation 必须同时校验 Service ETag 与所属 Tunnel 当前版本，并只按对应 Aggregate 的语义递增。Service 的强 ETag 是同时绑定 `service.version` 与所属
`tunnel.version` 的 opaque composite token；客户端不得解析或构造，Handler 负责还原为
Application Service 已有的双版本 CAS 输入。Create Service 的 `If-Match` 使用父 Tunnel
强 ETag。ETag Version 用于管理员并发写保护，Tunnel Revision 用于向 Connector 分发配置，
两者语义独立。

---

# 104. SQLite

数据库：

```text
<server.data_dir>/xtunnel.db
```

V0.1 不提供独立 `database.path`。SQLite、data-dir-owned Pinned Gateway TLS Identity、Token Credential Master Key 和 Server Durable Operation Journal 必须全部位于同一个 Canonical `server.data_dir` 管理边界内，避免两个不同 Data Directory 指向同一外部数据库而绕过互斥锁，也保证 Backup/Restore 不会组合出不同代的数据库、Token 密文与密钥材料。Public TLS 的证书和私钥由外部证书管理系统负责，不进入 XTunnel Backup Archive；Manifest 只记录 `tls_mode=public`。位于 Stable Target 父目录中的 Restore Journal 是替换边界外唯一允许的 Durable Operation Journal 例外。

配置：

```text
journal_mode = WAL

foreign_keys = ON

busy_timeout = 5000

synchronous = NORMAL
```

这些 PRAGMA 必须作用于 GORM 底层 `database/sql` Pool 创建的每一条物理连接，而不是只在启动时对某一条临时连接执行。SQLite Driver 必须通过 DSN 或 ConnectHook 固定设置，并在连接初始化失败时拒绝把该连接放入 Pool。启动自检至少查询 `foreign_keys`、`busy_timeout` 和 `journal_mode`；集成测试要把 Pool 扩到多连接并逐连接验证约束实际生效。

所有数据库事务：

```text
必须短事务
```

数据面热路径不访问 DB。

Migration 使用 `schema_migrations(version, applied_at)` 记录版本，并遵循：

```text
forward-only

单个 Migration 在事务内完成

数据库版本高于当前 Binary 支持版本 → 拒绝启动

Migration 失败 → 不启动任何公网 Listener
```

升级前必须创建一致性备份：

```bash
xtunnel-server backup create --output /secure/backup/xtunnel-backup.tar

xtunnel-server backup restore --input /secure/backup/xtunnel-backup.tar
```

在线 `backup create` 通过 `/run/xtunnel/backup-<stable-target-hash>.sock` 暂停新的 Config Write 和 Pinned Identity 自动续签，等待当前短事务或续签结束，再使用 SQLite Backup API 获取一致数据库镜像；在同一 Barrier 内复制 data-dir-owned Pinned Gateway TLS Identity 与 Token Credential Master Key。Socket 位于权限 `0700` 的 Runtime Directory，文件权限为 `0600`，只接受 Linux root peer；root CLI 还必须用 `SO_PEERCRED` 校验服务端 UID 与 Runtime Directory owner 一致。协议使用带 `version`、Stable Target Hash 和 acquire/release 动作的单行 JSON，grant/release 响应必须回显同一 Target Hash。Socket 只授予可取消、连接所有的 Barrier Lease，不传输 Archive 或 Secret；CLI 在租约存续期间从本地一致性边界构建归档，单一 Socket Reader 把连接 EOF、Server Shutdown 或提前响应转换为 Create Context 取消，且完整 Archive 必须在 release ACK 成功后才允许发布；取消、协议失败或租约丢失都必须删除未发布输出并 exactly-once 释放 Lease。若 Socket 不存在，命令必须先获取与 Server 相同的外部锁；Socket 存在但连接、授权或响应绑定失败时不得静默回退到离线路径。

Backup Archive 固定为 canonical POSIX USTAR，`manifest.json` 使用格式版本 `1` 并作为唯一 Manifest。Manifest 使用稳定路径排序，记录数据库实际 Schema 版本、`tls_mode`，以及每个归档普通文件的相对 POSIX 路径、大小、权限和 SHA-256。归档只允许固定白名单：SQLite Backup API 生成的自包含 `xtunnel.db`、精确 32 字节的 Tunnel Token Master Key，以及 Pinned Gateway 最终 Identity；Public TLS 外部证书和私钥、SQLite WAL/SHM、未知文件都不得归档。Gateway Rotation Journal 或 `.rotate` 临时文件存在时 Backup 必须 fail closed，要求先由正常启动或维护流程完成身份与审计 Reconciliation，禁止把包含源 Data Directory 绝对路径的未决轮换状态移植进 Archive。root CLI 必须以 Linux `openat2 + O_NOFOLLOW + fstat` 固定源数据库 inode，Schema 检查与 SQLite Backup API 只能通过同一打开 FD 的 `/proc/self/fd` 引用访问；每次 SQLite 打开前后及操作完成后都要安全重开原路径并用 `SameFile` 核对，rename、symlink 或路径替换一律 fail closed 并删除候选，避免 main DB 改名后遗漏仍留在原名的未 checkpoint WAL。SQLite Backup API 的目标 `xtunnel.db` 已包含调用时刻 WAL 可见状态；“禁止只复制 `xtunnel.db` 而遗漏 WAL”专指禁止裸拷贝在线主库文件，不要求把 WAL/SHM 写入 Archive。V0.1 固定限制 `xtunnel.db <= 64 GiB`、Pinned Private Key `<= 64 KiB`、Pinned Certificate `<= 1 MiB`；Parser 必须拒绝 PAX/GNU sparse、绝对路径、`..`、重复项、未知项、symlink、hardlink、设备文件、FIFO、Hash/大小/权限不一致、非 canonical 结束块和归档结尾后的任何未声明字节。Data Directory 内文件使用 `openat2(RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_XDEV)` 捕获，禁止经中间目录 symlink 或子挂载越界读取。

备份包等同于长期私钥材料。创建过程必须先在输出父目录中以 `O_CREATE | O_EXCL` 写入随机隐藏候选，权限固定为 `0600`，并以 `openat2` 拒绝输出路径任意中间 symlink；临时快照目录权限 `0700`。创建时必须保留输出父目录 FD，失败清理只能使用该 FD 做 relative unlink 并同步同一目录，不得重新解析可被 rename/替换的绝对路径。候选完成文件 `fsync`、Close 且在线 release ACK 成功后，才允许用同一父目录 FD 的 `renameat2(RENAME_NOREPLACE)` 原子发布最终路径并 `fsync` 父目录；已有目标必须由内核原子拒绝覆盖。禁止在 ACK 前暴露最终路径、输出到 stdout 或复用已有目标文件。

`backup restore` 只允许 Server 停止后执行，必须先由 `realpath(parent) + basename` 计算 Stable Data Target、获取同一外部锁，再校验 Manifest/Hash/Schema 兼容性。只接受大于零且不高于当前 Binary 支持上限的 Schema 版本；较旧版本原样 Restore，由下一次正常启动执行 forward-only Migration，离线 Backup/Restore 校验路径不得先运行 Migration。staging 必须在第一次 rename 前通过完整白名单/Hash/权限、immutable SQLite `quick_check`、精确 `schema_migrations`、Token Master Key 与 Pinned key/certificate 配对校验；其中全部 `tunnel_tokens` 都必须用归档 Master Key 和行身份 AAD 解密，严格解析规范 Token，并核对 Tunnel ID、Token ID、Version 与 `secret_hash`，合法长度但属于另一数据库的 Key 必须拒绝。任何失败都只删除 staging，旧 target 保持不动。恢复内容写到同盘 sibling staging 目录，然后按“旧目录 rename 为 rollback → staging rename 为正式目录 → fsync 父目录”的顺序切换。切换前在父目录写入权限 `0600` 的 `.xtunnel-restore-<hash>.journal`，文件名中的 `<hash>` 固定为 Stable Data Target 的 SHA-256；Journal 同时保存 canonical Manifest 与 Manifest Hash，二者必须相互匹配。staging 与 rollback 也必须由同一 Hash 固定派生。三条路径都必须校验为同一 `realpath(parent)` 的直接子项，且不得是 symlink 或独立挂载点。

Restore Journal 版本 `1` 的 phase 固定为 `prepared`、`rollback_ready`、`installed`。每次 phase 更新都必须以同父目录 `O_CREATE | O_EXCL | O_NOFOLLOW` 临时文件写入，完成文件 `fsync`、原子 rename 和父目录 `fsync`，禁止原地 truncate。恢复决策必须同时验证 phase 与正式 target/staging/rollback 的实际存在组合：两次目录 rename 之间优先把 rollback 恢复为正式目录；第二次 rename 已完成但 phase 尚未更新时，只有新 target 按 Journal Manifest 完整重验通过才允许前向完成，否则恢复旧 rollback；路径越界、对象类型异常或不可判定组合一律 fail closed。无 Journal 时只允许清理由 `target + staging + no rollback` 唯一判定的 pre-Journal 孤儿 staging；任何孤儿 rollback 都阻止启动。成功安装并验证后才能删除 rollback；删除前必须用 FD-relative 两阶段遍历证明整棵树无 symlink、特殊文件或不同 `statx mount ID`，禁止 `RemoveAll` 穿过 nested bind mount，之后才逐层 unlink。最后删除 Journal，并在每次目录项变化后 `fsync` 父目录。root 离线 Restore 必须沿用原 target 或 rollback 的 Runtime UID/GID，不能信任 Archive UID/GID；staging 根在完成内容校验前保持 root `0700`，只在交付前最后变更 owner。Journal 位于替换边界外且跨重启保留，崩溃后下次 Server/Restore 命令先按 Stable Target 取得同一把锁，再完成或回滚，不能要求 leaf 预先存在。禁止与现有数据库合并。集成测试必须覆盖“备份 → Migration → 恢复 → Agent 通过原 Pin 重连”以及两个 rename 之间崩溃后的回滚。

维护命令仅记录稳定事件 `backup_create_completed` 与 `backup_restore_completed`，字段固定为 `target_hash`、`manifest_sha256`、`schema_version`、`mode=online|offline`；不得记录 Archive 路径、Data Directory 路径、Token、Key、证书内容或其他 Secret。

---

# 105. Repository Layer

业务代码禁止直接：

```go
db.Query(...)
gormDB.Where(...)
```

V0.1 的 SQLite Repository 统一使用 GORM。业务持久化不得绕过 Repository
直接操作 `*gorm.DB`；仅连接初始化、逐连接 PRAGMA 自检和 SQLite Backup API
等 GORM 不提供等价能力的基础设施路径可以访问底层 `database/sql`。全部写入只能
从 `WithTx`/`WithDurableTx` 取得统一 `writeGate` 后执行；`Read` 与
`ReadConsistent` 生成的 Repository 视图必须标记为只读，任何 mutator 即使通过现有
组合接口被调用也要快速失败，不能绕过在线 Backup Barrier。

Repository：

```go
type TunnelRepository interface {
    Create(...)
    Get(...)
    List(...)
    Update(...)
}

type TunnelTokenRepository interface {
    ...
}

type ServiceRepository interface {
    ...
}

type RouteRepository interface {
    ...
}

type UsageRepository interface {
    ...
}

type Store interface {
    Read(ctx context.Context, fn func(RepositoryView) error) error
    WithTx(ctx context.Context, fn func(TxStore) error) error
}

type RepositoryView interface {
    Tunnels() TunnelRepository
    TunnelTokens() TunnelTokenRepository
    Services() ServiceRepository
    Routes() RouteRepository
    Usage() UsageRepository
}

type TxStore interface {
    RepositoryView
}
```

认证、Token Reveal 等纯读热路径必须使用 `Store.Read`，不得通过 `BEGIN IMMEDIATE`
争抢 SQLite 写锁。跨表不变量只能由 Application Service 在一次 `Store.WithTx` 中完成。传入的
`TxStore` 中所有 Repository 必须共享同一个事务作用域内的 `*gorm.DB`，
Repository 自身不得再开启或提交事务；Service 创建/删除、Tunnel Token
轮换和 Tunnel Revoke 等多表操作都遵循这一规则。

Migration 继续使用显式、可审查的 forward-only 版本和
`schema_migrations` 记录；不得用 GORM `AutoMigrate` 取代版本化 Migration。

V0.1：

```text
repository/sqlite
```

---

# 106. Admin User

```sql
CREATE TABLE admin_users (
    id TEXT PRIMARY KEY,

    username TEXT NOT NULL UNIQUE,

    password_hash TEXT NOT NULL,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,

    -- NULL means the user has never completed a successful login.
    last_login_at INTEGER
);
```

`last_login_at` 仅在一次成功登录完成后更新；创建首个管理员不会写入该字段。

M5-03 的 v9 Migration 会在创建 `admin_sessions` 的同一事务内，把历史数据库中规范的
小写 UUID Admin ID 转换为新的 `adm_<ULID>`。遇到损坏或无法规范化的历史 ID 时整个
Migration 回滚，不得留下部分转换状态。

密码：

```text
Argon2id
```

第一阶段固定最低参数：

```text
memory = 64 MiB

iterations = 3

parallelism = min(4, runtime CPU)

salt = 16 byte crypto/rand
```

参数必须随 Hash 一起编码，后续登录成功时允许按新参数重新 Hash。

首次管理员不使用默认用户名或默认密码。全新数据库启动后 Management API 只返回 `SETUP_REQUIRED`，管理员通过本机命令初始化：

```bash
xtunnel-server admin create \
  --username admin \
  --password-file /run/secrets/xtunnel-admin-password
```

也允许从 TTY 隐藏输入读取密码，但禁止通过命令行参数传递密码。

Server 未运行时，初始化命令获取 Server External Lock 后直接写入；Server 正以 `SETUP_REQUIRED` 运行时，命令通过仅本机 root 可访问的 `/run/xtunnel/admin-bootstrap.sock` 请求 Server 完成事务。该 Unix Socket 权限为 `0600`，首个管理员创建成功后立即关闭并删除。已有管理员时两条路径都必须拒绝重复初始化。

Server External Lock 位于 Data Directory 替换边界之外：

```text
/run/xtunnel/server-lock-<sha256(stable-data-target)>.lock
```

`/run/xtunnel` 权限为 `0700` 并归 XTunnel Runtime UID 所有。systemd 通过 `RuntimeDirectory=xtunnel` 创建；OCI Image 预创建同一目录并固定以 `UID:GID 65532:65532` 运行。OCI 使用只读根文件系统时，运行器必须把 `/run/xtunnel` 挂载为该 UID/GID 可写、权限 `0700` 的 tmpfs。离线维护命令由 root 创建或访问该目录。禁止要求非 root Server 直接写 `/run/lock`。生产默认 Stable Parent 固定为 `/var/lib/xtunnel`，systemd `StateDirectory` 与 OCI Volume 都挂载该父目录；正式 `server.data_dir` 默认为其直接子目录 `/var/lib/xtunnel/data`。安装流程和 OCI Image 必须预创建权限 `0700`、归 Runtime UID/GID 所有的 `data` leaf，不能把正式 leaf 本身用作不可 rename 的 Volume mountpoint。

`server.data_dir` 必须是绝对路径。Stable Data Target 的计算只依赖 `realpath(parent_dir) + basename(data_dir)`：父目录必须已存在且不是符号链接，leaf 名称必须合法；leaf 可以在 Restore 的中间崩溃状态下暂时不存在。Lock 使用非阻塞 OS 独占锁、禁止跟随符号链接、权限 `0600`，由进程全生命周期持有，残留文件本身不代表已加锁。离线 Admin、Backup、Restore 和 Recovery 命令必须用完全相同的 Stable Target/Hash 算法复用同一把锁。

获取锁后，进程必须先检查父目录中的 Restore Journal 并完成或回滚，再要求正式 Data Directory 存在、拒绝 leaf 符号链接，并验证其 `realpath` 等于 Stable Data Target。正常启动不能为了通过校验而创建一个空 leaf 目录；全新部署的目录由安装流程预先创建。

密码修改或管理员禁用时，必须删除该用户的所有 Admin Session。

---

# 107. Admin Session

```sql
CREATE TABLE admin_sessions (
    id TEXT PRIMARY KEY
        CHECK (
            length(id) = 30
            AND substr(id, 1, 4) = 'ads_'
            AND substr(id, 5, 1) GLOB '[0-7]'
            AND substr(id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),

    user_id TEXT NOT NULL
        CHECK (
            length(user_id) = 30
            AND substr(user_id, 1, 4) = 'adm_'
            AND substr(user_id, 5, 1) GLOB '[0-7]'
            AND substr(user_id, 5) NOT GLOB '*[^0123456789ABCDEFGHJKMNPQRSTVWXYZ]*'
        ),

    token_hash BLOB NOT NULL UNIQUE
        CHECK (typeof(token_hash) = 'blob' AND length(token_hash) = 32),

    csrf_token BLOB NOT NULL
        CHECK (typeof(csrf_token) = 'blob' AND length(csrf_token) = 32),

    expires_at INTEGER NOT NULL CHECK (expires_at > 0),

    created_at INTEGER NOT NULL CHECK (created_at > 0),
    last_seen_at INTEGER NOT NULL CHECK (last_seen_at > 0),

    FOREIGN KEY(user_id)
        REFERENCES admin_users(id)
        ON DELETE CASCADE,

    CHECK (expires_at > created_at),
    CHECK (last_seen_at >= created_at AND last_seen_at < expires_at)
);

CREATE INDEX admin_sessions_user ON admin_sessions(user_id);
CREATE INDEX admin_sessions_expiration ON admin_sessions(expires_at);
CREATE INDEX admin_sessions_idle_expiration ON admin_sessions(last_seen_at);
```

Session Token 和 CSRF Token 分别使用独立的 32 byte `crypto/rand`。Cookie 中的原始
Session Token 永不落库，数据库只保存 `SHA-256(token)`；CSRF Token 需要由
`GET /api/v1/auth/me` 恢复，因此保存独立原始随机值，但不写 Cookie、URL、日志或错误文本。

默认策略：

```text
absolute_ttl = 12h

idle_ttl = 30min

logout = 删除数据库 Session
```

成功校验管理员口令后、创建新 Session 前，Repository 每次最多清理 128 条绝对过期或
空闲超过 30 分钟的 Session。读取会话时同样检查绝对到期与空闲到期；`last_seen_at`
最多每分钟触碰一次，避免每个请求都产生 SQLite 写入。

---

# 108. Tunnel 与 Service 表

Tunnel、Tunnel Token 与 Service 的持久化结构以第 20、57、58 节为唯一契约。Connector、Session、在线状态、`observed_revision` 与 `last_seen_at` 都是运行态，不写入 SQLite。

Tunnel 的 ONLINE/OFFLINE 状态由当前 Connector Registry 实时计算；Service 状态由 Tunnel 可用性、Connector 对该 Service 的 Revision 观测与 Origin Health 统一计算。数据库不得缓存一个可能在 Server 重启后失真的在线状态。

---

# 109. Usage Table

只保存按 UTC Bucket、Tunnel 与 Service 唯一归属的聚合数据。三层表字段一致，
`usage_hours` 与 `usage_days` 仅把 Bucket 对齐约束分别改为 3600 和 86400 秒：

```sql
CREATE TABLE usage_minutes (
    bucket_time INTEGER NOT NULL CHECK (bucket_time > 0 AND bucket_time % 60 = 0),
    tunnel_id TEXT NOT NULL,
    service_id TEXT NOT NULL,
    connections INTEGER NOT NULL DEFAULT 0 CHECK (connections >= 0),
    ingress_bytes INTEGER NOT NULL DEFAULT 0 CHECK (ingress_bytes >= 0),
    egress_bytes INTEGER NOT NULL DEFAULT 0 CHECK (egress_bytes >= 0),
    errors INTEGER NOT NULL DEFAULT 0 CHECK (errors >= 0),
    PRIMARY KEY (bucket_time, tunnel_id, service_id)
);
```

Usage 是已经发生的历史事实，不建立随 Tunnel 或 Service 删除而级联清理的外键。

---

# 110. Usage Direction

统一定义：

```text
Ingress Bytes
=
Public Client → Origin

Egress Bytes
=
Origin → Public Client
```

避免不同组件：

```text
RX / TX
```

语义相反。

---

# 111. Usage Aggregation

Server 使用一个进程级 Owner 聚合 `(UTC minute, tunnel_id, service_id)`：

```go
type UsageDelta struct {
    BucketTime time.Time
    TunnelID   string
    ServiceID  string
    Connections, IngressBytes, EgressBytes, Errors uint64
}
```

每：

```text
60s
```

批量：

```text
memory
 ↓
SQLite transaction
```

Flusher 每 60 秒交换整张内存 Bucket Map，释放锁后批量事务写入：

```sql
INSERT ... ON CONFLICT(bucket_time, tunnel_id, service_id)
DO UPDATE SET value = value + excluded.value;
```

写入失败时，未确认提交的完整批次与并发新增量合回内存；确认提交的批次绝不重放。
待提交 Bucket 总数固定上限 65,536，计数以 SQLite `INTEGER` 上限饱和并显式失败，
不得因 Repository 长期故障形成无界内存。V0.1 Usage 属于 best-effort 统计：进程
`kill -9` 最多允许丢失当前 60 秒内存桶，但不得重复累计已确认提交的批次。

完成的 UTC minute 在同一事务中先累加到 hour、再删除 minute；完成的 hour 随后以
相同顺序累加到 day 并删除。Crash 后重跑保持幂等，稳态只保留当前未完成的 minute
和 hour。Day 固定保留当前 UTC 日与此前 6 日，共 7 日；每次 Rollup 提交后最多执行
256 页 Incremental Vacuum。20,000 Service × 7 日的 140,000 条 day 行容量 Benchmark
为约 31.05 ms/op、249.6 bytes/row，约 33.3 MiB，因此 V0.1 不新增 Retention 配置项。
旧库首次升级 v11 时必须在 Migration 事务前执行一次完整 `VACUUM`，把文件转换为
Incremental Auto Vacuum；失败或取消不会记录 v11，但启动耗时随旧库大小增长。

成功公网逻辑 OPEN 只在 `OPEN_OK`、Active Work 注册和最终 Context 检查均通过后记
一次 Connections；内部 Work 重试、跨 Connector Failover 与 Heartbeat 都不重复计数。
最终失败的逻辑 OPEN 只记一次 Errors。RAW 后端连接的 `Write` 计 Ingress，`Read` 计
Egress，并只累计成功传输的 `n`。Service 与 Dashboard 的当日 Usage 只读 Repository，
允许最多 60 秒最终一致性，不与进程内未 Flush Map 合并。

禁止：

```text
每请求写 DB
```

---

# 112. Runtime Reconciler

即使 Standalone 也实现：

```text
Desired State
+
Runtime State
```

Reconcile 触发：

```text
Server Startup

Config Write

Agent Connect

Agent Disconnect

Periodic 5s
```

所有触发只向单一 Reconcile Loop 标记 dirty，不得并发执行多个 Runtime Build。
多个 dirty wakeup 必须合并；Build 期间 `reconcile_generation` 前进时，丢弃旧结果并立即
按最新 generation 重建，不增加面向用户的 coalesce 配置。

每次构建记录：

```text
reconcile_generation
desired_revision_snapshot
```

Swap 前必须再次确认 generation 仍是最新；过期结果直接丢弃并重新构建。`atomic.Pointer` 只负责读写安全，不承担版本单调性。

保证：

```text
SQLite
 ↓
Runtime
```

可以恢复。

---

# 113. Admin Authentication

第一阶段只有：

```text
Local Administrator
```

登录成功生成：

```text
256-bit Random Session Token
```

Cookie：

```text
Name=xtunnel_admin_session

HttpOnly

Secure

SameSite=Lax

Host-only（禁止设置 Domain）

Path=/api/v1
```

Management 只能通过 HTTPS 前置代理或本机 loopback 访问。客户端 IP、Scheme 和 Host 使用独立于 Tunnel Ingress 的 `management.trusted_proxies` 规则解析；未受信任代理提供的 Forwarded 元数据不得改变判定，非 Loopback 的明文请求直接拒绝。HTTP Server 固定使用 10 秒 Header Read、30 秒 Read、30 秒 Write 和 90 秒 Idle Timeout。Shutdown 先关闭新请求准入，再排空既有 Handler；Deadline 到期后主动关闭连接并等待 Handler 所有权归零，SQLite 只能在 Management 完成收敛后关闭。

---

# 114. CSRF

所有：

```text
POST

PUT

PATCH

DELETE
```

管理请求要求：

```text
CSRF Token
```

CSRF Token 为绑定 Admin Session 的独立 32-byte 随机值，Wire 使用 43 字符、无 padding
base64url，通过响应 Body 获取，并使用自定义 Header：

```text
X-XTunnel-CSRF
```

Server 必须同时校验 Token、`Origin` 和目标 Management Host。CSRF Token 不写 Cookie、不进入 URL、不记录日志。
Login 与 `/auth/me` 因在 Body 返回 CSRF Token，响应同样必须带 `Cache-Control: no-store`
和 `Pragma: no-cache`。

唯一例外是尚未建立 Session 的 `POST /api/v1/auth/login`：它不要求 Session-bound CSRF Token，但必须同时满足 `Origin` 与规范化 Management Host 同源、`Content-Type: application/json`，并拒绝表单、`text/plain`、缺失 Origin 的请求和跨站重定向来源。非浏览器客户端也必须显式发送正确 Origin。登录仍受第 115 节限流约束。除 Login 外，Logout 和所有其他状态变更请求都必须通过 Session CSRF 校验。

---

# 115. Login Rate Limit

默认分别限制：

```text
5 attempts
/
normalized client IP + normalized username
/
minute
```

另设每分钟 100 次失败的 Server 全局预算，避免攻击者通过大量用户名绕过限制。

连续失败增加：

```text
cooldown
```

同一 Key 的冷却时间逐级固定为 `1m / 2m / 4m / 8m / 15m`；失败状态使用最多
4096 项的 LRU，30 分钟无活动后回收。密码校验最多允许 4 个并发槽位，容量已满时
立即返回 `429` 和 `Retry-After: 1`，不得排队放大 Argon2 内存占用，也不把该拒绝计入
失败预算。

只统计失败登录；成功登录不会清除同 IP 的全局攻击计数。所有限流 Key 必须使用 Management Trusted Proxy 规则得到的客户端 IP，禁止直接使用 loopback Peer 或未经验证的 X-Forwarded-For。

Login、Gateway 和 Public Ingress 的 Token Bucket/LRU 状态只存在于当前单 Server 内存，
Server Restart 后会重置。这是 Standalone V0.1 的明确限制；本阶段不引入 Redis、持久化
Bucket 或分布式 Rate Limit。

---

# 116. Agent Gateway Auth Limit

即使 Token 高熵，也需要限制握手资源消耗。

只统计失败认证，并使用分层且有界的带 Burst 令牌桶：

```text
Normalized Peer IP-only Bucket

Normalized Peer IP + bounded token_fingerprint Bucket

Server Global Failed-auth Bucket
```

IP-only Bucket 防止攻击者不断更换随机 Token 绕过 `IP + fingerprint`；组合 Bucket 限制对单个 Credential 的集中尝试；Global Bucket 限制分布式攻击。Fingerprint 必须从已限制到 8192 bytes 的原始输入以不可逆方式计算，不能把 Token Identity 或 Secret 写入日志/Metric。所有失败同时记入适用层级，成功认证不消耗失败预算，也不能清空 IP 或全局失败计数。

同 NAT 下多个合法 Agent 共享 IP 时，成功认证不消耗失败预算。另设全局 Pending TLS/Auth 上限和单 IP 握手并发上限，具体 Rate/Burst 必须通过 5000 Connector 重连风暴测试确定，不能把示例值作为不可调整的硬编码值。桶表使用有最大容量和过期时间的 LRU，避免随机 IP/Token 造成内存 DoS。

并限制：

```text
Max Pending TLS Handshakes

Max Pending Auth
```

---

# 117. Web Console 技术栈

固定：

```text
React

TypeScript

Vite

Tailwind CSS

shadcn/ui

Lucide

React Router

TanStack Query
```

需要复杂 Table 时：

```text
TanStack Table
```

按需引入。

不使用大型企业 UI Framework。

M0-08 的工程基线固定为 Node `24.19.0`、npm `11.17.0`、React/React DOM `19.2.8`、Vite `8.2.2`、Plugin React `6.1.0`、TypeScript `6.0.2` 和用于管理菜单图标的 `lucide-react 1.34.0`，直接依赖必须精确写入 `package.json` 并由 npm 11 Lockfile 锁定。M0-08 只引入已有真实使用点的 React/Vite/TypeScript 基础与 Lucide 图标；Tailwind CSS、shadcn/ui、React Router 和 TanStack Query 仍是冻结的产品技术方向，在 M5 首次出现真实使用点时分别确认版本并引入，不得以空配置或未使用依赖提前占位。

---

# 118. 前端目录

```text
web/
├── src/
│   ├── components/
│   │   ├── ui/
│   │   ├── tunnel/
│   │   └── service/
│   │
│   ├── pages/
│   │   ├── login/
│   │   ├── dashboard/
│   │   ├── tunnels/
│   │   ├── services/
│   │   └── settings/
│   │
│   ├── api/
│   ├── hooks/
│   ├── lib/
│   ├── router/
│   └── app.tsx
│
├── .gitignore
├── vite.config.ts
├── package.json
└── package-lock.json
```

---

# 119. Embedded Web

构建：

```text
Vite
 ↓
dist
```

Go：

```go
// web/embed.go
package webui

import "embed"

//go:embed all:dist
var Dist embed.FS
```

最终部署：

```text
一个 xtunnel-server binary
```

不需要：

```text
独立 Node Server
```

构建顺序固定为：

```text
cd web
 ↓
npm ci
 ↓
npm run build
 ↓
生成 web/dist
 ↓
go build ./cmd/server
```

`web/embed.go` 与 `web/dist` 位于同一个 Go Package 目录，避免 `go:embed` 跨目录。CI 在执行 `go build` 前必须构建前端；发布源码包若不包含 dist，则普通 Go 构建目标必须依赖该前置步骤。

Server 与 Agent 的产品版本只由 `internal/buildinfo.Version()` 读取。正式 Binary 构建必须同时使用 `-X github.com/lifei6671/xtunnel/internal/buildinfo.version=<version>` 注入同一版本；未注入的普通开发构建固定返回 `(devel)`。版本不读取运行时 Environment、Config 或 VCS Metadata，避免遗漏注入时伪装成正式产物。

开发模式使用 `web/vite.config.ts`：

```text
Vite HTTPS Dev Server
  /api/v1/*
      ↓ same-origin proxy
Go Management 127.0.0.1:8080
```

本地开发证书只用于 Loopback，`/api/v1` 代理保留浏览器可见 Host/Origin，并由开发配置显式加入 Management Allowed Hosts。这样 Secure Cookie、Origin 和 CSRF 仍走生产同源模型；禁止为联调增加 `Access-Control-Allow-Origin: *`、关闭 Secure Cookie 或跳过 CSRF。`npm run dev` 与代理配置必须进入 M0 开发说明。

开发者通过 `XTUNNEL_DEV_TLS_CERT` 和 `XTUNNEL_DEV_TLS_KEY` 指向本机已信任的 Loopback Certificate；文件位于仓库外，或位于被 `web/.gitignore` 排除的 `.dev-certs/`，目录权限 `0700`、Key `0600`。缺失证书时 `npm run dev` 必须给出可操作错误并退出，禁止自动提交证书、私钥或静默降级 HTTP。M0-08 使用临时上游 Fixture 验证 HTTPS、`/api/v1` 代理以及 Host/Origin 保持，只能记为 Proxy Harness 证据；真实 Login、Secure Cookie、CSRF POST 和 Logout E2E 依赖 M5-03 的 Auth Handler 与 Web Login，必须在 M5-10 完成，不得由 M0 Fixture 冒充产品链路。

---

# 120. Web 页面

V0.1：

```text
Login

Dashboard

Tunnels

Tunnel Detail

Services

Service Detail

Create Service

Settings
```

---

# 121. Dashboard

展示：

```text
Server Status

Tunnels

Online Connectors

Offline Tunnels

Services Ready

Services Error

Active Connections

Connections Today

Ingress Traffic Today

Egress Traffic Today

Recent Errors
```

当前 Dashboard 已直接展示 Server Status、Tunnel/Connector/Service 计数、Active Connections、当日 Connections/Ingress/Egress Usage 与 Recent Errors。Server Status 只来自 System Health owner，页面不得根据资源数量重算；Usage 和 Recent Errors owner 已就绪时，即使当前值为空或为零也必须返回 `AVAILABLE`，`UNAVAILABLE` 只保留给对应 Read Model 尚未接入的状态，不得混淆“暂无数据”和“能力不可用”。

---

# 122. Tunnel List

字段：

```text
Name

Status

Connectors Online

Version Summary

Services

Active Connections

Last Seen
```

操作：

```text
View

Create Service

Add Connector

Rotate Token

Revoke
```

---

# 123. Tunnel Detail

展示：

```text
Tunnel Name

Tunnel ID

Status

Token Version

Desired Revision

Services
```

Connectors：

```text
Hostname

Connector ID

Version

OS

Architecture

Status

Idle WorkConn

Active Connections

Connected Since

Last Heartbeat
```

---

# 124. Create Tunnel

输入：

```text
Name
```

创建成功：

```text
Tunnel ID

Connection Token (`xta_...`)

Foreground / Container / Linux systemd / Windows SCM usage
```

例如：

```bash
xtunnel-agent run --token 'xta_...'

docker run --rm -e XTUNNEL_TOKEN='xta_...' xtunnel-agent:<version>

sudo xtunnel-agent service install --token 'xta_...'
```

Windows 提升权限的 PowerShell：

```powershell
.\xtunnel-agent.exe service install --token 'xta_...'
```

用户只需复制一个完整 Connection Token；不再另外复制 Endpoint、Pin、Tunnel ID 或准备 Token 文件。Token 创建后仍可从 Tunnel Detail 的“添加 Connector”向导再次安全获取，当前 Token Version 内返回的文本逐字节相同。

---

# 125. Add Connector 与 Rotate Token

Add Connector：

```text
读取当前 ACTIVE Tunnel Token
返回相同部署命令
不创建 Connector 数据库行
不生成新 Token
不递增 Token Version
```

Connector 使用该 Token 认证成功后自动出现在 Tunnel Runtime 列表中。

管理员确认：

```text
Rotate
```

返回：

```text
New Token
```

UI 明确提示：

```text
旧 Connector 保持在线

旧 Token 无法重连

请更新部署 Secret
```

---

# 126. Create Service Wizard

流程：

```text
Basic
 ↓
Origin
 ↓
Public Access
 ↓
Health Check
 ↓
Review
```

---

# 127. Basic

字段：

```text
Service Name

Tunnel
```

从 Tunnel Detail 创建：

```text
Tunnel 固定
```

---

# 128. Origin

字段：

```text
Protocol

Host

Port

Connect Timeout

HTTP Host Header（HTTP/HTTPS 可选）

Disable Chunked Encoding（HTTP/HTTPS）

Disable Happy Eyeballs

HTTP Idle Timeout（HTTP/HTTPS）

HTTP Max Idle Connections（HTTP/HTTPS）

TCP KeepAlive Interval（0 表示禁用）
```

HTTPS：

```text
TLS Verify

TLS Server Name
```

默认：

```text
TLS Verify = true
```

HTTP Host Header 始终是显式覆盖，其优先级高于 Preserve Host；二者都未生效时回落到
规范化 Origin `host[:port]`。HTTP 专属字段在 TCP Service 表单中不展示，接口边界也必须拒绝。

---

# 129. HTTP Public Access

字段：

```text
Hostname

Path Prefix

Preserve Host
```

UI 提示：

```text
DNS 和前置 Caddy/Nginx
需能将此 Host 转发至 XTunnel HTTP Ingress
```

---

# 130. TCP Public Access

字段：

```text
Public Port
```

Server 实时检查：

```text
Port Range

Port Occupied

Route Conflict
```

---

# 131. Health Check

选项：

```text
Disabled

TCP

HTTP
```

HTTP：

```text
Path

Interval

Timeout

Expected Status

Failure Threshold

Success Threshold
```

Web 表单使用与 Service DTO 相同的范围校验，并始终展示 Server 返回的完整值，不在前端维护另一套隐式默认值。

---

# 132. Service Detail

展示：

```text
Status

Tunnel

Healthy Connectors

Origin

Public Route

Active Connections

Connections Today

Ingress Today

Egress Today

Last Error
```

操作：

```text
Edit

Disable

Enable

Delete
```

---

# 133. Service Status

```text
DISABLED

TUNNEL_OFFLINE

ORIGIN_UNHEALTHY

NO_CAPACITY

CONFIG_SYNCING

APPLY_FAILED

READY
```

如果 Tunnel 下有多个 Connector：

```text
至少一个可用 Connector
+
Origin Healthy
+
ObservedRevision >= RequiredRevision
```

即可：

```text
READY
```

Service Status 必须由 Server 的唯一状态计算模块生成，固定优先级为：

```text
DISABLED
>
APPLY_FAILED
>
TUNNEL_OFFLINE
>
CONFIG_SYNCING
>
ORIGIN_UNHEALTHY
>
NO_CAPACITY
>
READY
```

规范算法：

```go
switch {
case !service.Enabled:
    return DISABLED
case runtimeApplyFailed:
    return APPLY_FAILED
case noConnectedConnector:
    return TUNNEL_OFFLINE
case noConnectorObservedRequiredRevision:
    return CONFIG_SYNCING
case healthEnabled && noHealthyRevisionEligibleConnector:
    return ORIGIN_UNHEALTHY
case noRuntimeCapacity:
    return NO_CAPACITY
default:
    return READY
}
```

`CONFIG_SYNCING` 必须先于 `ORIGIN_UNHEALTHY`，因为旧 Revision Health 明确不可用。该算法只能在 `internal/server/status` 实现一次；Dashboard、Service Detail 和 Web Console 必须展示 API 返回值，禁止在前端或不同 Handler 中重新计算。

`CONFIG_SYNCING` 表示 Desired State 已提交，但尚无满足 Service RequiredRevision 的 Connector。`APPLY_FAILED` 表示 TCP Listener 等 Runtime 副作用无法达到 Desired State；详情必须包含稳定错误码和最近失败时间。

---

# 134. REST API

REST API 的唯一权威来源固定为：

```text
api/openapi/openapi.yaml
```

M5 Handler、TypeScript Client、Mock、DTO 校验和契约测试必须从该文件生成或由 CI 验证一致；本文只定义产品语义。M5 开始前必须冻结全部 Request/Response Schema、Required/Nullable、分页、错误响应、ETag 和 HTTP Status，禁止由 Handler 与 Web 分别维护 DTO。

M5-02 固定使用 `oapi-codegen v2.8.0` 从同一 OpenAPI 生成 Go Models、`net/http` Server 与 Strict Server Contract，提交路径为 `internal/server/managementapi/contract.gen.go`；Go Runtime 锁定 `oapi-codegen/runtime v1.6.0` 与 `nullable v1.1.0`，PATCH 的 omitted/null/value 由生成类型表达。TypeScript Schema 固定使用 `openapi-typescript 7.13.0` 生成到 `web/src/api/schema.gen.ts`，开启 immutable 与 read/write markers；`web/src/api/client.ts` 只用 `openapi-fetch 0.17.0` 装配 `/api/v1` 和 same-origin Credential，不手写第二套 DTO。

Web 工程继续使用 TypeScript `6.0.2`；由于 `openapi-typescript 7.13.0` 的工具侧 Peer Range 不包含 TypeScript 6，生成 CLI 隔离在 `tools/openapi-ts` 并以独立 Lockfile 锁定 TypeScript `5.9.3`。统一入口是 `tools/openapi.sh generate|generate-check`，CI 必须先按两个 Lockfile 执行 `npm ci`，再以 `generate-check` 同时比较 Go 与 TypeScript 字节。生成文件禁止手工修改；OpenAPI 中为联合类型显式声明的 discriminator mapping 只纠正生成器选择的类型名，不改变既有 Wire 值，也不改写不可变初始 Baseline。

基础：

```text
/api/v1
```

Authentication：

```text
POST /auth/login

POST /auth/logout

GET  /auth/me
```

尚无 Admin 时，`POST /auth/login` 固定返回 `409 SETUP_REQUIRED`，引导管理员使用受支持的
本机 Bootstrap 路径创建首个 Admin；不得把该状态伪装为密码错误或 500。

所有 List API 统一使用不透明 Cursor：

```http
GET /api/v1/services?page_size=50&page_token=...
```

```json
{
  "items": [],
  "next_page_token": "..."
}
```

`page_size` 默认 `50`、最大 `200`；无下一页时省略 `next_page_token`。Token 对客户端完全 opaque，Server 不信任其中任何可解码内容，并至少校验 Resource Type、排序字段、最后一条记录和 Filter Hash；客户端不得解析或构造。非法、过期或与当前 Filter 不匹配的 Token 返回 `400 INVALID_PAGE_TOKEN`。

PATCH 统一要求 `Content-Type: application/merge-patch+json`，并使用 JSON Merge 语义的显式 DTO：

```text
omitted → 不修改
null    → 仅对 OpenAPI 标记 nullable 的字段执行清空
value   → 修改为该值
```

未知字段返回 `400 INVALID_REQUEST`。非 Nullable 字段传 `null` 返回 `422 VALIDATION_FAILED`。嵌套对象的 omitted/null/value 语义必须逐字段生成测试，禁止用 Go 零值猜测“未提供”。

Tunnel 和 Service 都是 Aggregate Root，`tunnels.version` 与 `services.version` 是各自并发版本。单个 Resource 的 GET、POST Create 和 PATCH 成功响应返回强 ETag；List Response 不返回 Aggregate ETag。Tunnel ETag 使用带引号的十进制 Version：

```http
ETag: "7"
```

PATCH、DELETE、Rotate、Revoke、Enable 和 Disable 必须携带单个精确 `If-Match`，不接受 `*`。缺失返回 `428 PRECONDITION_REQUIRED`，语法错误或多值返回 `400 INVALID_IF_MATCH`。Application Service 在同一事务中执行：

Service Create 的 `If-Match` 是父 Tunnel ETag；Service 后续 Mutation 使用绑定
Service Version 与 Tunnel Version 的强 opaque composite ETag。客户端只回传完整值，
不得依赖其内部编码。

```text
UPDATE aggregate
SET ..., version = version + 1
WHERE id = ? AND version = expected
```

DELETE 使用 `DELETE ... WHERE id = ? AND version = expected`；Action 使用等价的条件 UPDATE。任何路径受影响行数为零时，必须在同一事务内区分 `404 RESOURCE_NOT_FOUND` 与 `412 RESOURCE_VERSION_CONFLICT`，不能把两者都返回 404。

版本不匹配返回 `412 RESOURCE_VERSION_CONFLICT`，不得覆盖其他管理员已经提交的修改。除成功删除的 Resource 外，Action/Mutation 返回新 ETag；涉及 Service/Route 的写入递增 `services.version`，并在同一事务内让所属 Tunnel 的 `desired_revision` 只递增一次。

HTTP Status 固定为：

| HTTP | 语义 |
| ---: | --- |
| 200 | GET/PATCH 或带响应 Body 的 Action 成功 |
| 201 | Resource Created |
| 204 | 无响应 Body 的 Delete/Action 成功 |
| 400 | JSON、Header、分页 Token 或请求格式错误 |
| 401 | 未认证或 Session 失效 |
| 403 | 已认证但操作不允许 |
| 404 | Resource 不存在 |
| 409 | 领域不变量冲突 |
| 412 | ETag/Version 冲突 |
| 422 | 业务字段或容量校验失败 |
| 428 | 缺少必须的 If-Match |
| 429 | Rate Limited |
| 500 | Internal Error |

返回 Tunnel Token、Reveal/Rotate Token 或其他 Secret 的响应必须包含：

```http
Cache-Control: no-store
Pragma: no-cache
```

不得被 Dashboard Recent Activity、Access Log Body 或前端持久化缓存记录。Settings 页面在 V0.1 只读展示 `/system/config` 的非敏感 Server 有效配置和“需重启”标记；不提供修改 Server 主配置或 Connector 本地配置的 API。

---

# 135. Tunnel 与 Connector API

```text
POST /tunnels

GET /tunnels

GET /tunnels/{id}

PATCH /tunnels/{id}

GET /tunnels/{id}/connectors

GET /tunnels/{id}/token

POST /tunnels/{id}/token/rotate

POST /tunnels/{id}/token/revoke

POST /tunnels/{id}/revoke

DELETE /tunnels/{id}
```

管理端“添加 Connector”只生成使用当前 Tunnel Token 的部署指引，不创建 Connector 持久化记录，也不签发新 Token。`GET /tunnels/{id}/token` 每次通过审计化 Credential Lifecycle `Reveal` 返回当前 ACTIVE Token 的同一完整值；只有显式 Rotate 才创建新 Token Version。Token Revoke 只拒绝当前 Token 的新认证，不关闭既有 Session，也不把 Tunnel 标记为 Revoked；Tunnel Revoke 才撤销整个 Tunnel 并收敛全部 generation。Reveal、Rotate 和 Create Tunnel 等返回 Secret 的响应都必须使用 `no-store`。

`DELETE /tunnels/{id}` 只允许删除没有任何 Service 的 Tunnel。存在引用时返回：

```text
409 TUNNEL_IN_USE

service_count

referencing_service_ids（有界分页）
```

删除不得隐式级联 Service 或 Route。需要停用凭据时使用 `POST /tunnels/{id}/revoke`；需要删除 Tunnel 时，管理员必须先显式迁移或删除它下面的 Service。

Control AUTH 必须在完成 Token 格式解析并取得 Tunnel ID 后、执行持久化 Verify 前登记 per-Tunnel admission，并在认证成功后把该租约转交 Session Manager，直到 startup reservation 发布或交接失败才释放。Tunnel Delete 的持久化提交完成后，Session Runtime 必须建立删除专用的临时准入栅栏，等待已准入 AUTH、startup、全部 generation Session 和 ActiveWork 收敛，再删除对应 Runtime 定位项并释放临时栅栏。该路径不得复用 Tunnel Revoke 的永久墓碑语义。任一运行态清理失败时，持久化删除不回滚，临时栅栏保持 fail closed，并向管理端返回 `RUNTIME_CONVERGENCE_FAILED`，不得误报删除成功。

---

# 136. Service API

```text
POST /services

GET /services

GET /services/{id}

PATCH /services/{id}

DELETE /services/{id}

POST /services/{id}/enable

POST /services/{id}/disable
```

Service 的 `origin`、`health`、`exposure` 都使用 OpenAPI 中的封闭类型；内部 Route ID
不进入 REST。Create Service 的 `If-Match` 校验父 Tunnel，后续 PATCH/DELETE/enable/disable
使用 Service composite ETag。Exposure 的创建、切换、移除以及 Service 删除必须复用同一
Application 事务；Origin、Proxy Options、Health、Enabled 或 Exposure 的有效快照变化，
以及 Service Delete，都只推进一次 Tunnel Revision 与 Route Generation，普通更新同时只
推进一次 Service Version。事务提交后才分别通知 Tunnel Snapshot 与 Route Snapshot owner。

创建或修改 Service 在提交前触发 Tunnel Snapshot 预算检查。超限统一返回：

```text
422 TUNNEL_SERVICE_LIMIT
或
422 SNAPSHOT_TOO_LARGE

tunnel_id
service_count
snapshot_bytes
configured_limit
```

响应不得包含 Origin Credential、Token 或完整 Snapshot。Service 直接且只属于一个 Tunnel，不再存在中间关联资源。

---

# 137. Dashboard API

```text
GET /dashboard
```

返回：

```text
Tunnel Counts

Connector Counts

Service Counts

Active Connections

Traffic Summary

Recent Errors
```

Dashboard API 复用 System Health 的状态 owner，以及既有 Tunnel/Service Application 投影计算资源计数；不同 owner 的只读快照允许最终一致，但不得访问 SQLite 重算运行态或在前端推导第二套状态。M6 Usage 从 Repository 返回 `AVAILABLE` 当日聚合；Recent Errors 由进程内固定五槽 Latest Projection 返回 `AVAILABLE`，每类只保留最新一条并按 UTC 时间倒序输出，重启后为空但不伪装成 `UNAVAILABLE`。诊断码只允许 `TUNNEL_OFFLINE`、`CONNECTOR_OFFLINE`、`ORIGIN_DOWN`、`NO_CAPACITY`、`PROTOCOL_ERROR`；Tunnel/Connector 离线消费完成 generation fencing 的非预期生命周期边沿，Origin/Capacity/Protocol 只消费最终逻辑 OPEN，内部 Failover 不重复。固定中文文案不得包含底层错误、Origin、Header、IP 或 Secret；`request_id` 仅在真实关联值存在时返回。

---

# 138. System API

```text
GET /system/info

GET /system/health

GET /system/config
```

`GET /system/info.version` 来自 Server/Agent 共用的链接期 Build Version owner；正式构建必须显式注入，普通开发构建返回 `(devel)`，运行时输入不能覆盖。`started_at` 在进程入口捕获，`uptime_seconds` 使用该启动时间携带的单调时钟计算。`GET /system/health` 执行真实注入检查，Dashboard 只复用聚合结果，不另行重算。

敏感配置：

```text
不得返回 Token

不得返回 Private Key
```

`GET /system/health` 与其他 System API 一样要求 Admin Session。`GET /system/config`
不得直接序列化内部 `Config`，只允许返回 Management Public URL、Agent Gateway Public
Hostname/TLS Mode、TCP Ingress Port Range、Logging Level，以及 OpenAPI 明列的产品限额；
路径、Listen Address、Trusted Proxy、Certificate/Key Path 和内部 Frame/Queue 预算均不返回。
当前这些字段全部标记 `changes_require_restart=true`。

## Security Audit Query

```text
GET /security-audit-events
```

该资源只提供 GET，不提供 POST/PUT/PATCH/DELETE。结果使用严格的
`(occurred_at, event_id) DESC` Keyset，允许按 Action、Result、Resource Type、Resource ID
和 UTC 时间范围过滤；`from` 为 inclusive，`to` 为 exclusive。SQLite 按秒存储事件时间，
带小数的时间边界向上取整到下一秒后比较；opaque Cursor 必须绑定全部 Filter。REST 将
32-byte Digest 编码为 64 字符小写 hex，可空 JSON 字段保持 `null`，所有时间统一返回
RFC3339 UTC `Z`。

---

# 139. API Error

统一：

```json
{
  "error": {
    "code": "TUNNEL_OFFLINE",
    "message": "tunnel is offline",
    "request_id": "req_01K..."
  }
}
```

HTTP Status 和业务 Error Code 分离。

业务 `error.code` 使用 OpenAPI 冻结的封闭枚举；Handler 只能把内部错误映射到该列表，
不得以符合大写命名规则为由临时发明新码。新增或重命名 Error Code 必须走显式
Contract Change Review。

可选 `error.details` 不是任意对象，而是以 `type` 判别的封闭联合类型：
`FIELD_VIOLATIONS`、`TUNNEL_IN_USE`、`SNAPSHOT_LIMIT` 或
`RESOURCE_VERSION_CONFLICT`。每种 Details 的字段、Required/Nullable 与上限只由 OpenAPI
定义；不得在 Handler 中临时增加 Metadata 或泄露 composite ETag 的内部版本编码。

---

# 140. Caddy 示例

Management：

```text
admin.example.com
    ↓
127.0.0.1:8080
```

Tunnel：

```text
*.tunnel.example.com
    ↓
127.0.0.1:8081
```

概念配置：

```caddy
admin.example.com {
    reverse_proxy 127.0.0.1:8080
}

*.tunnel.example.com {
    reverse_proxy 127.0.0.1:8081
}
```

TLS 和 wildcard certificate 配置由实际部署环境负责。

可执行的 Caddy 配置位于 `deploy/reverse-proxy/Caddyfile`。前置代理监听公网
HTTPS/WSS，XTunnel HTTP Ingress 只监听同一主机网络命名空间中的 loopback；
代理到 XTunnel 固定使用明文 HTTP/1.1。Caddy 必须保留完整 Host authority 与
Origin，并覆盖客户端提供的 `X-Forwarded-For/Proto/Host`，不能把不可信前缀
追加到权威代理链。公网 Header Read 固定为 `10s`；响应刷新使用与 XTunnel
ReverseProxy 对齐的有限 `100ms` 间隔，禁止使用负 `flush_interval`；负值低延迟
模式会在客户端断开后继续执行 upstream 请求，延迟 WorkConn 与 ACTIVE Lease
归还。Caddy upstream 不设置方向独立的读写 timeout，WebSocket 的双向共享 `1h`
idle 继续只由 XTunnel 裁决。

---

# 141. Nginx 示例

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    server_name admin.example.com;

    location / {
        proxy_set_header Host $http_host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $http_host;

        proxy_pass http://127.0.0.1:8080;
    }
}
```

Tunnel：

```nginx
server {
    listen 443 ssl;
    server_name *.tunnel.example.com;

    client_header_timeout 10s;
    large_client_header_buffers 4 1m;
    client_max_body_size 0;

    location / {
        proxy_http_version 1.1;

        proxy_set_header Host $http_host;

        # 覆盖客户端自带 XFF，禁止把不可信前缀透传给 XTunnel。
        proxy_set_header X-Forwarded-For $remote_addr;

        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $http_host;

        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;

        proxy_request_buffering off;
        proxy_buffering off;

        proxy_read_timeout 24d;
        proxy_send_timeout 24d;

        proxy_pass http://127.0.0.1:8081;
    }
}
```

以上示例满足 WebSocket、1GB Upload/Download 和 streaming 的第一阶段验收下限。
公网 Header Read 固定为 `10s`；`large_client_header_buffers 4 1m` 避免默认单字段
8 KiB 上限先于 XTunnel 拒绝合法 Header，聚合上限仍由 Server Schema 裁决。Nginx
使用 `client_max_body_size 0` 关闭前置代理自身的 Body 大小裁决；请求体唯一上限由
`configs/server.schema.json` 的 `limits.max_http_body_bytes` 定义，并由 XTunnel 返回
稳定 413。标准 Nginx HTTP Proxy 不能表达“任一方向进展同时续期”的共享 idle，
`24d` 是 Nginx 支持上限内、远高于 XTunnel `1h` 业务窗口的方向性 ceiling；严格单向连续流超过 24 天时
必须增加反向 heartbeat 或使用 Caddy。不得在前置代理另设更小固定 Body/Header
阈值，也不得重新启用整请求缓冲。

可执行的 Nginx 主配置模板位于 `deploy/reverse-proxy/nginx.conf.template`；必须只
替换 `XTUNNEL_*` 部署变量，保留 `$http_host`、`$remote_addr`、`$scheme` 与
`$http_upgrade` 等 Nginx 运行时变量。`$http_host` 用于保留非默认端口，不能用
可能丢失原始 authority 端口的 `$host` 代替。证书与私钥只由部署环境挂载，不能
写入仓库或镜像层。

---

# 142. Server 配置

Server 配置的唯一机器可读契约为 `configs/server.schema.json`。JSON Schema 必须与 Server Go Config Struct、示例配置和配置测试从同一字段清单生成或由 CI 做双向一致性检查。字段类型、默认值、范围、是否必填、Secret 标记和是否可热加载只允许在 Schema 中定义一次；本文示例不构成第二份默认值来源。

覆盖优先级固定为：

```text
CLI > XTUNNEL_* Environment > YAML > Schema Default
```

Server YAML 使用 Strict Decode，未知字段或重复 Key 直接启动失败；未知 CLI Flag 直接失败；`XTUNNEL_*` 命名空间下无法映射到 Schema 的变量直接失败。Duration 统一使用 Go Duration String，大小统一使用整数 Byte。V0.1 不热加载 Server 主配置；变更后必须显式重启，动态 Service 配置仍通过 Revision/Snapshot 生效。

Server 配置 Schema 固定使用 JSON Schema Draft 2020-12。每个叶子字段必须显式声明 `x-secret` 和 `x-reloadable`；V0.1 Server 主配置的 `x-reloadable` 全部为 `false`。环境变量名由 Schema 点分路径转换：路径段大写后使用双下划线连接，例如 `management.public_url` 对应 `XTUNNEL_MANAGEMENT__PUBLIC_URL`。数组覆盖值使用 JSON Array，标量覆盖值按 Schema 类型解析。CLI 层同样使用 Schema 点分路径。Server 配置入口固定为可选的 `--config <path>` 和可重复的 `--set <schema.path>=<value>`；不接受位置参数，未知 Flag 或 Schema 路径直接失败，同一路径重复覆盖时以后出现的值为准。Agent 不使用此 Schema/Loader，Bootstrap 输入按下一节的单 Token 契约处理。

可直接复制的完整注释示例见 `configs/server.example.yaml`；Agent 的 Token-only 输入说明见
`configs/README.md` 与 `configs/agent-bootstrap.env.example`。下面只保留 Server 的核心部署
片段，不作为第二份字段清单或默认值权威。

推荐：

```yaml
server:
  data_dir: /var/lib/xtunnel/data

management:
  listen: "127.0.0.1:8080"

  public_url: "https://admin.example.com"

  allowed_hosts:
    - "admin.example.com"

  trusted_proxies:
    - "127.0.0.1/32"
    - "::1/128"

http_ingress:
  listen: "127.0.0.1:8081"

  trusted_proxies:
    - "127.0.0.1/32"
    - "::1/128"

agent_gateway:
  listen: "0.0.0.0:7443"

  public_hostname: "tunnel.example.com"

  tls:
    mode: pinned

transport:
  tcp:
    work_acquire_timeout: 2s

control:
  high_priority_queue: 32
  normal_queue: 128
  write_timeout: 5s

tcp_ingress:
  bind: "0.0.0.0"

  min_port: 10000
  max_port: 60000

connector_runtime:
  heartbeat_interval: 10s
  heartbeat_timeout: 30s

metrics:
  listen: "127.0.0.1:9090"
  path: /metrics

logging:
  level: info
  format: json
```

`management.public_url` 必填，必须是绝对 `https` URL，Path 只能是空或 `/`，并禁止包含 Userinfo、Query 或 Fragment。它规范化为 `scheme + IDNA ASCII host + effective port`。`allowed_hosts` 使用规范化的 `host[:port]`，只补充允许到达 Management Handler 的 Host，不扩大合法 Origin；Host 同样 lowercase、IDNA ASCII、移除尾点并规范化默认端口。

Management 请求先按 `management.trusted_proxies` 得到可信 Scheme/Host，再执行：

```text
Request Host ∈ {public_url derived host} ∪ allowed_hosts

Login Origin == normalized public_url Origin
```

开发模式的 Vite Loopback Origin 必须显式进入开发专用 `public_url/allowed_hosts`，不能由 CSRF Handler 猜测，也不能在生产配置中自动放行。`limits` 的完整字段、默认值和范围只以 `configs/server.schema.json` 为准；第 156 节是由 Schema 自动生成或 CI 校验的人类可读镜像，禁止人工独立维护默认值。其他章节中的 YAML 只能作为部署示例。

---

# 143. Tunnel Token Bootstrap

Agent 没有 YAML、`--config`、`--set` 或本地配置 Schema。唯一 Secret 输入是一个完整 Connection Token，来源优先级固定为：

```text
1. --token 'xta_...'                 # 前台交互或安装输入
2. XTUNNEL_TOKEN='xta_...'           # OCI/Compose
3. OS Service Credential                # Linux systemd LoadCredential 或 Windows SCM + DPAPI
```

Linux 的第 3 级来源为 `$CREDENTIALS_DIRECTORY/xtunnel-agent.token`；Windows SCM 的第 3 级来源为 `%ProgramData%\XTunnel\credentials\agent.token.dpapi` 经 DPAPI Machine-scope 解密后的 Token。高优先级来源存在时覆盖低优先级来源；没有任何来源时启动失败。未知 Flag、位置参数、空的 Linux `CREDENTIALS_DIRECTORY`、Windows Credential 缺失/ACL 错误/DPAPI 解密失败都必须直接失败。Agent 不接受 Endpoint、TLS Trust、Agent Identity 或认证 Secret 的独立覆盖，避免把同一连接契约拆成多份本地配置。

M0 Bootstrap 只执行以下输入边界校验：

```text
非空
没有首尾空白
以 `xta_` 开头
UTF-8 byte length <= 8192
```

这不是 Connection Token Protocol 验证。精确编码、版本分派、完整性、语义字段和 Golden Vector 由 M05-02 冻结；Agent Gateway 实现接入后，未知版本或解析失败必须在发起网络连接前快速失败。

Tunnel 下的 Service、Origin、Health Policy 和 Revision 全部由 Server 远端托管并通过完整 Snapshot 下发。WorkPool、Reconnect、Control Queue 和 Health Scheduler 的安全边界使用 Server Policy 与 Binary 固定上限。Agent 日志在 V0.1 使用 Binary 固定的安全默认值，Token 不得进入日志。

---

# 144. Agent Local Inputs

Agent 自身没有 Data Directory，也不创建或维护任何长期状态、业务配置或用户配置文件。运行方式固定为：

```text
Foreground: xtunnel-agent run --token 'xta_...'
OCI:        XTUNNEL_TOKEN='xta_...' xtunnel-agent run
systemd:    LoadCredential 提供运行时 xtunnel-agent.token
Windows:    SCM 启动 xtunnel-agent.exe run，并读取 DPAPI Machine-scope Credential
```

Agent Binary 的 `service install` 子命令内部创建 `/etc/xtunnel/credentials/agent.token`，父目录为 `root:root 0700`，文件为 `root:root 0600`。Binary 内嵌带 XTunnel managed marker 的 Unit：

```systemd
# Managed by xtunnel-agent service install
LoadCredential=xtunnel-agent.token:/etc/xtunnel/credentials/agent.token
ExecStart=/usr/local/bin/xtunnel-agent run
```

该 Source 是 Binary 自安装逻辑私有的持久 systemd Credential 输入，不是要求用户预先准备或日常维护的 Token 文件；Linux `run` 只读取 systemd 暴露的运行时 Credential，不知道 Source 路径，也不复制、轮换或覆盖它。Unit 不把 Secret 放入 `ExecStart`、Environment 或 Unit 参数。

Windows Binary 自安装到 `%ProgramFiles%\XTunnel\xtunnel-agent.exe`，并把一次性安装 Token 使用 `CRYPTPROTECT_LOCAL_MACHINE | CRYPTPROTECT_UI_FORBIDDEN` 加密为 `%ProgramData%\XTunnel\credentials\agent.token.dpapi`。SCM Service `XTunnelAgent`（DisplayName `XTunnel Agent`）使用 `NT AUTHORITY\LocalService`，ImagePath 只包含安装 Binary 与 `run`，不包含 Token。该 DPAPI Blob 是安装器内部 Credential，不是用户配置文件或可跨机器复制的明文备份；ACL 只允许管理员、系统和运行服务所必需的身份访问。Token Rotate 后，Linux 或 Windows 管理员都用新的单字符串 Token 重新执行 `service install`，由 Binary 安全替换平台 Credential 并重启 Agent。

OCI/Compose 直接把部署环境中的 Secret 映射为容器内 `XTUNNEL_TOKEN`，不挂 Agent 配置、Token Secret 文件或持久 Volume。Server 与 Agent 的官方 OCI 构建必须同时提供同一个 `XTUNNEL_VERSION`；Dockerfile 校验 1 至 64 个安全字符后，通过 `-X github.com/lifei6671/xtunnel/internal/buildinfo.version` 注入两个 Binary，缺失或非法时构建失败。

---

# 145. Service Self-install

V0.1 官方支持矩阵固定为：

```text
Server: Linux amd64 / arm64

Agent: Linux amd64 / arm64；Windows amd64 / arm64

Service Manager: Linux systemd >= 249；Windows SCM

Container: OCI 前台进程模式 + Compose v2 双栈部署 Profile
```

macOS launchd、Alpine OpenRC 和其他 Unix Service Manager 不属于 V0.1 支持范围。Linux Server/Agent `service install/uninstall` 要求 root、amd64/arm64 和 systemd 249 及以上；Windows Agent 要求 amd64/arm64、提升权限的 Administrator 和可用 SCM。平台、架构、权限或 Service Manager 不满足时，必须在创建账户、注册服务或写任何目标文件前快速失败。Alpha 发布和验收只对上述矩阵作承诺。

Server 与 Agent 在 Linux 都不公开或调用用户安装脚本，全部服务生命周期由对应 Binary 的 `service install/uninstall` 管理；Windows 仍只支持 Agent SCM 自安装。Server 不新增 `run` 子命令，官方 Unit 继续用根命令 `--config /etc/xtunnel/server.yaml` 启动。

Server `service install --config PATH` 创建 `xtunnel-server:xtunnel-server` 系统用户/组，把当前可执行文件、指定配置和内嵌 Unit 原子安装到 `/usr/local/bin/xtunnel-server`、`/etc/xtunnel/server.yaml` 与 `/etc/systemd/system/xtunnel-server.service`；配置权限为 `root:xtunnel-server 0640`，`/var/lib/xtunnel/data` 为服务身份所有的 `0700` Canonical Data Target。Unit 首行 marker 精确为 `# Managed by xtunnel-server service install`。上一版官方 Shell Unit 只能按完整规范化字节精确匹配后接管；其他无 marker、被修改、symlink、目录或外来同名 Unit 必须拒绝覆盖和卸载。安装完成执行 daemon-reload、enable、restart 与 is-active；文件发布或激活失败必须恢复此前 Binary、配置、Unit 和受管服务状态。

`service install` 创建 `xtunnel-agent:xtunnel-agent` 系统用户/组，把当前可执行文件原子安装到 `/usr/local/bin/xtunnel-agent`，创建 root-only Credential Source，写入 Binary 内嵌的 managed Unit，然后执行 daemon-reload、enable、restart 与 is-active 检查。即使服务已经运行，重复安装也必须重启，使新的 Binary、Token 和 Unit 立即生效。Agent 只使用可清理的 `/run/xtunnel-agent` Runtime Directory，不创建 StateDirectory。目标 Unit 不是普通文件，或首行不精确等于 `# Managed by xtunnel-agent service install` 时必须拒绝覆盖；更新 Binary、Token 或 Unit 时不得留下部分安装。

Windows `service install` 把当前可执行文件安装到 `%ProgramFiles%\XTunnel\xtunnel-agent.exe`，写入 DPAPI Machine-scope Credential，再通过 SCM 创建或更新 `XTunnelAgent`。Service 使用 `NT AUTHORITY\LocalService`，ImagePath 只执行 `"%ProgramFiles%\XTunnel\xtunnel-agent.exe" run`。Binary 内嵌的 Description managed marker 精确为 `Managed by xtunnel-agent service install`；同名 Service 缺少该 marker 时拒绝覆盖或卸载。重复安装使用 Windows `MoveFileEx(REPLACE_EXISTING | WRITE_THROUGH)` 语义分别原子替换 Binary/Credential，更新受管 SCM 配置并重启服务；任一步失败必须明确返回，不得静默报告安装成功。

Agent OCI Image 固定 `CMD ["run"]`，容器只运行数据面进程，不允许在镜像内执行服务安装。官方 Compose v2 Profile 使用自定义 Bridge 并启用 IPv6；IPv4 保持 Docker Bridge 默认启用。Server 与 Agent 必须同时获得 IPv4、IPv6 容器地址。Management 仅发布到宿主机 IPv4/IPv6 回环，Agent Gateway 才发布到宿主机全部 IPv4/IPv6 地址。部署者设置 Compose 输入变量 `XTUNNEL_VERSION` 与 `XTUNNEL_AGENT_TOKEN`；前者以同值构建两个 Binary，后者只映射为 Agent 容器内的 `XTUNNEL_TOKEN`。Agent 不声明 Secret mount 或持久 Volume。

Linux 安装命令：

```bash
sudo ./xtunnel-server service install --config ./server.yaml
sudo xtunnel-agent service install --token 'xta_...'

sudo /usr/local/bin/xtunnel-server service uninstall
sudo xtunnel-agent service uninstall
```

Windows 安装命令（提升权限的 PowerShell）：

```powershell
.\xtunnel-agent.exe service install --token 'xta_...'

& "$env:ProgramFiles\XTunnel\xtunnel-agent.exe" service uninstall
```

`--token` 是一次性安装输入，不是持久服务参数。Self-installer 校验 Token 形状后写入 Linux root-only Credential Source 或 Windows DPAPI Machine-scope Blob；持久 Unit 与 SCM ImagePath 都只执行平台 Binary 的 `run`。安装与运行路径都不得回显或记录 Token。

```text
Preflight root + Linux + running systemd >= 249
 ↓
Create service user/group
 ↓
Atomically install current Binary
 ↓
Create/Replace root-only LoadCredential Source
 ↓
Install embedded managed Unit
 ↓
Enable + Start
```

```text
Preflight Administrator + Windows amd64/arm64 + SCM
 ↓
Install/Replace Binary under ProgramFiles
 ↓
Create/Replace ProgramData DPAPI Machine-scope Credential
 ↓
Create/Update managed XTunnelAgent SCM Service as LocalService
 ↓
Register managed XTunnelAgent Application Event Log Source
 ↓
Start/Restart + Query Running
```

Windows SCM 的 Stop/Shutdown 最多等待 30 秒；Agent 运行回调异常必须返回非零 Service Exit，SCM 同时为 non-crash failure 配置恢复重启，避免错误退出被伪装成成功。`service install` 在 `Application` 日志下注册唯一 `XTunnelAgent` Event Source，并用与 SCM Service 相同的 managed marker 证明归属；同名 Source 缺失 marker、标准值被修改或不受管理时拒绝覆盖/删除。SCM 模式把共享 JSON Handler 的完整单行记录按 `debug/info→Information`、`warn→Warning`、`error→Error` 映射到该 Source；Source 缺失、被修改、打不开或写入失败时服务启动/运行必须失败，不得静默退回 stderr。`service uninstall` 只删除确认受管的 Source，Windows Smoke 必须查询真实 Application Event、解析 JSON 固定字段并确认 Token 未出现。

Linux `service uninstall` 只在 Unit 带匹配 managed marker 时停止、禁用并删除对应 Unit 与已安装 Binary；Server 还允许精确识别上一版官方 Shell Unit。Server 保留配置、凭据、数据和服务用户/组；Agent 保留平台 Credential 与服务用户/组。Windows `service uninstall` 只在 `XTunnelAgent` 带匹配 Description marker 时停止并删除 SCM Service，随后删除 `%ProgramFiles%\XTunnel\xtunnel-agent.exe`；若命令正由该已安装 EXE 自身执行，文件锁导致无法立即删除时，必须使用 `MoveFileEx(DELAY_UNTIL_REBOOT)` 安排在下次系统重启删除，不能虚假报告已即时消失。未知或人工管理的同名 Unit/Service 必须拒绝删除。

---

# 146. 多 Connector 运行

同一个 Tunnel Token 可以被多个 Agent 进程或容器使用：

```bash
xtunnel-agent run --token 'xta_...'

XTUNNEL_TOKEN='xta_...' xtunnel-agent run
```

它们属于同一个 Tunnel，但每次启动各自生成独立 `connector_id`，不需要不同 Data Directory。Agent Self-installer 在 V0.1 每台主机只管理一个固定 systemd Unit 或 `XTunnelAgent` SCM Service；同机多进程由容器编排或管理员自建服务模板管理，不额外引入 Agent 本地状态。

---
# 147. Agent Reconnect

使用：

```text
Exponential Backoff
+
Jitter
```

例如：

```text
1s

2s

4s

8s

16s

30s

30s...
```

加：

```text
±20%
```

随机抖动。

---

# 148. Control Session 断开

Session 断开：

```text
CAS 清除该 generation 的 CurrentSession
 ↓
无旧 ActiveWork：从 Runtime Registry 删除
仍有旧 ActiveWork：保留不可选择 Tombstone
```

Server：

```text
关闭该 Session 的所有 Idle WorkConn
```

已经：

```text
ACTIVE
```

的 WorkConn：

```text
允许继续直到自然结束
```

这些旧 Active WorkConn：

```text
继续登记在 TunnelRuntime.ActiveWork

不计入新 Session 的 Idle Pool

继续计入原 Connector 的 Active、Usage 和日志

可被 Tunnel Revoke 或 drain timeout 定位并关闭
```

如果：

```text
Tunnel Revoke
```

则 Active 也强制关闭。

---

# 149. Connector 重连

同一个运行进程：

```text
connector_id 不变

session_id 改变
```

旧 Idle WorkConn：

```text
全部废弃
```

新 Session：

```text
重新建立 Work Pool
```

旧 Active WorkConn 可继续直到完成。

重连时 Server 为该 Connector 递增 `session_generation`。旧 Session cleanup 必须携带旧 generation，并通过 Compare-And-Swap 确认自己仍是 Current Session；若 CAS 失败，只能清理属于旧 Session 且尚未 ACTIVE 的 Idle/Opening Registry 项，禁止修改新 Session 状态。旧 `ActiveWork` 仍在全局 Registry 中，只能自然结束，或由 Revoke / 明确 drain timeout 路径通过其自身 `closeOnce` 关闭。

---

# 150. Server Restart

Server Crash：

```text
所有业务连接中断
```

Agent：

```text
Detect Disconnect
 ↓
Backoff
 ↓
Reconnect
 ↓
Authenticate
 ↓
Receive + Apply Full Current Snapshot
 ↓
ConfigAck
 ↓
Refill Work Pool
```

SQLite 配置全部保留。

V0.1 不实现 Hot Restart、Listener FD 继承或 Active Session 跨进程迁移。M7 必须在
固定测试环境记录以下恢复测量值，而不是未经 Benchmark 预先承诺固定 SLO：

```text
T_control_reconnect
T_config_ready
T_workpool_ready
T_first_success
```

测试从 Server Restart 开始，直到 Connector 重连、完整 Snapshot Ack、WorkPool 恢复
以及首条新业务连接成功；已有业务连接在单 Server Crash 时中断属于 V0.1 明确限制。

---

# 151. Agent Restart

Agent Restart：

```text
Connector ID 改变
```

已有该 Connector 业务连接：

```text
中断
```

其他 Connector：

```text
继续服务
```

新 Agent Process：

```text
生成新 Connector ID
 ↓
认证并完整拉取当前 Snapshot
 ↓
Ack 后加入 Tunnel
```

Agent Restart 不读取本地业务配置或历史 Revision。

---

# 152. Server Graceful Shutdown

SIGTERM：

```text
Stop Management Write

Stop HTTP New Accept

Stop TCP New Accept

Stop Tunnel OPEN

Mark Draining

Wait Active Connections

Deadline 后主动关闭剩余 Public / Origin / WorkConn

Close Agent Sessions

Final Flush / Rollup Usage

Close SQLite
```

默认：

```text
drain_timeout = 30s
```

Server 超过 `drain_timeout` 后必须主动关闭剩余 Public、Origin、WorkConn 和 Control socket，然后再 Flush/Close SQLite；不得只等待 Context 自然取消，也不得无限阻塞 Shutdown。

Usage Owner 必须晚于公网入口、Tunnel、Session 和 Route Owner 排空，并用独立 5 秒
Context 执行最终 Flush/Rollup；失败显式进入 Shutdown 结果。SQLite 只能在此后关闭。

---

# 153. Agent Graceful Shutdown

```text
SIGTERM
 ↓
Send DrainRequest
 ↓
Stop Refill WorkPool
 ↓
Server Mark Connector Draining
 ↓
Server Stop New Acquire + Finish In-flight OPENING
 ↓
Receive DrainAck
 ↓
Stop Accepting OPEN
 ↓
Wait Active
 ↓
Close
```

```protobuf
message DrainRequest {
    string drain_id = 1;
    uint32 drain_timeout_ms = 2;
}

message DrainAck {
    string drain_id = 1;
    uint32 remaining_active = 2;
}
```

Drain 是两阶段握手。双方收到 Request 后分别以本地 monotonic clock 和相对 `drain_timeout_ms` 建立 Deadline，禁止比较跨主机绝对时间。Server 先把 Connector 从选择集合摘除，并等待已经 Acquire 的 `OPENING` 完成或失败，之后才发送同一 `drain_id` 的 Ack。Agent 在收到 Ack 前仍接受这些已在途 OPEN；收到 Ack 后才拒绝新 OPEN。重复 Request/Ack 必须幂等。若握手超时，Agent 以 `OPEN_DRAINING` 失败仍在途 OPEN，Server 在尚未进入 RAW 时可重新选择其他 Eligible Connector。

默认：

```text
agent_drain_timeout = 30s
```

超过 drain timeout 后必须主动关闭剩余 Origin 和 WorkConn socket。仅取消 Context 不会唤醒阻塞中的 `net.Conn.Read`；代理层必须通过 Close 或 Deadline 解除阻塞，并确保 goroutine、FD 和 Active Counter 最终归零。

---

# 154. Buffer 管理

数据代理使用：

```text
32KB
```

Buffer。

使用：

```go
sync.Pool
```

复用。

`32KB` 是 V0.1 当前 `io.Copy` 路径的默认工作缓冲基线，不是从其他项目继承的固定最优值。M7 必须在相同
硬件、连接数和负载下比较至少 `16KB`、`32KB`、`64KB` 的 Throughput、CPU、Syscall、
RSS、Heap 和 GC，再决定是否调整；不得为未测量的“最佳值”修改协议或增加配置项。

禁止：

```text
每连接固定分配 MB 级内存
```

---

# 155. Backpressure

全部依赖：

```text
TCP Flow Control
```

禁止：

```text
read entire payload
 ↓
memory queue
 ↓
send
```

所有流量：

```text
streaming
```

---

# 156. Server Limits

默认：

```yaml
limits:
  max_tunnels: 1000

  max_connectors: 5000

  max_connectors_per_tunnel: 100

  max_services_per_tunnel: 1000

  max_health_targets_per_tunnel: 2000

  max_health_targets_global: 50000

  max_tunnel_snapshot_bytes: 786432

  max_active_connections: 20000

  max_connections_per_tunnel: 5000

  max_connections_per_service: 5000

  max_connections_per_source_ip: 200

  max_open_rate_per_source_ip: 50

  max_open_burst_per_source_ip: 100

  max_http_requests_per_source_ip_per_second: 100

  max_work_connections: 60000

  max_idle_work_connections: 20000

  max_connecting_work_connections: 1000

  max_pending_opens: 1024

  max_pending_auth: 512

  max_pending_tls_handshakes: 512

  max_replay_entries_per_session: 4096

  max_control_frame_bytes: 1048576

  max_auth_frame_bytes: 65536

  max_work_frame_bytes: 65536

  max_http_header_bytes: 65536

  max_http_body_bytes: 2147483648
```

WorkConn 全局预算包含 Connecting、Idle、Opening 和 Active。每个 Connector 的 `target_idle` 是 best effort，只能通过 Server 发放的 WorkDemand Budget Lease 补池；达到全局 Idle/FD 预算后不得继续建连。Budget Manager 必须同时实施 per-tunnel 和 per-connector 公平份额，并为有真实 Pending OPEN 的 Tunnel 保留最小可用额度，禁止某个拥有大量 Connector 的 Tunnel 抢占全部 Idle。

`configs/server.schema.json` 是所有 Server 硬限制的唯一机器权威和默认值来源。第 156 节只是由 Schema 生成或经 CI 反向校验的人类可读镜像，不得独立修改。`max_connectors_per_tunnel` 在 Control Auth Commit 前执行；Health 两级 Target Budget 在 Management Candidate 校验和新 Connector Auth Commit 前执行。各 Limit 自其所属里程碑起必须进入真实分配/状态转换路径，不能只解析配置或只上报 Metric：Data Plane/Frame/Queue/FD Limit 从 M1 生效，Health Target Budget 从 M3 生效，HTTP 入口限制从 M4 生效。M7 允许根据 Benchmark 调整默认值，但不能第一次实现这些上限。

公网公平性限制分两层：Active Connection 上限和 Accept/Open Rate Token Bucket。Raw TCP 使用实际 Peer IP；HTTP 使用第 91 节 Trusted Proxy 规则得到的 normalized client IP。HTTP 还执行可配置的请求速率限制，不能只限制底层长连接。所有 per-source 状态使用最多 32 个分片的有界 LRU；总状态容量由 `max_active_connections + max_pending_opens` 派生，容量压力淘汰各分片最久未访问项。OPEN 条目的 TTL 是 `max(ceil(max_open_burst_per_source_ip / max_open_rate_per_source_ip), 1s)`，HTTP Request 条目的 Burst 与 Rate 相同且 TTL 为 1 秒；Server 重启后状态清空。运维可按 NAT 场景调整数值，但不能绕过 Agent/Tunnel/Global 上限。

Raw TCP 在 OS `Accept` 成功后、连接登记和 Handler goroutine 创建前消费一个来源 OPEN Token；后续 Tunnel OPEN 不重复扣减。HTTP 在可信代理规范化成功后为每个请求消费一个 Request Token，只在 Transport 确实需要新建 Tunnel WorkConn 时再消费 OPEN Token，KeepAlive 复用不额外扣减；任何下游失败都不退还已经消费的 Token。HTTP ACTIVE 额度覆盖一次在途 Request 或完整 WebSocket Handler 生命周期，不能绑定到可跨请求、跨来源复用的 HTTP WorkConn；Raw TCP ACTIVE 仍覆盖完整公网连接生命周期。Pending 到 ACTIVE 的四级 Global/Tunnel/Service/Source IP 迁移与所有 Lease 释放必须线性化且 exactly-once。

HTTP Request Body 使用 `max_http_body_bytes` 做流式上限，不允许为判定大小而整体缓存；该 Schema 字段是完整受支持链路的唯一 Body 大小裁决，Caddy/Nginx 前置代理不得另设更小固定阈值。已知 `Content-Length` 超限必须在 Tunnel Dial 前返回 `413 REQUEST_BODY_TOO_LARGE`，流式读取中超限同样返回该稳定码并禁止复用客户端连接。HTTP Request Rate 或新建 WorkConn OPEN Rate 超限返回 `429 RATE_LIMITED` 与 `Retry-After: 1`。`max_http_header_bytes` 继续直接作用于 Go HTTP Server，超限在 Handler 前使用标准 `431 Request Header Fields Too Large`，不另造 JSON 错误契约；前置代理的单字段缓冲必须至少容纳 Schema 允许的最大 `1 MiB` Header，不得以默认值提前拒绝。TCP 限流拒绝不向公网连接写入带内错误文本，只关闭 Socket。

Server 启动时根据 `RLIMIT_NOFILE` 校验配置：

```text
required_fd_budget
=
listeners
+ sqlite_and_files
+ control_sessions
+ work_connections
+ public_active_connections
+ pending_tls_and_auth
+ management_http_metrics_accepted
+ safety_margin
```

每条 Active Tunnel 通常同时占用一条 WorkConn FD 和一条公网客户端 FD，两者必须分别计数，禁止假设 `max_active_connections` 已包含在 `max_work_connections` 内。预算不成立时快速失败并给出各项所需与实际 FD 数；运行时 Budget Manager 同样保留 Management/Metrics 和短时 Accept 峰值余量，禁止依赖 `too many open files` 兜底。

任何：

```text
queue

buffer

chan

pending request
```

必须有容量上限。

---

# 157. Timeout

默认：

```text
TLS Handshake
10s

Agent Authentication
10s

Auth Result Write
5s

HTTP Header Read
10s

HTTP Request Body Idle
60s sliding

WebSocket Active Idle
1h sliding（任一方向字节进展同时续期两端）

Public TCP Pre-OPEN Idle
10s

Heartbeat Interval
10s

Heartbeat Timeout
30s

Work Acquire
2s

OPEN Handshake
6s

Origin Connect
5s

HTTP Origin Idle Connection
90s

Origin TCP KeepAlive
30s（0 禁用）

Control Frame Idle
30s
```

HTTP 数据流本身：

```text
不设置统一短超时
```

“不设置统一短超时”仅指完整请求、响应和 WebSocket 生命周期；Header、握手以及无字节进展的读写仍必须受阶段性 Idle Timeout 和容量限制。

避免破坏：

```text
Streaming

WebSocket

Long Polling
```

---

# 158. Logging

统一：

```text
JSON Structured Logging
```

字段：

```text
timestamp

level

component

request_id

tunnel_id

connector_id

session_id

service_id

connection_id

generation

trace_id

event

error_code
```

每条日志固定包含 `timestamp`、`level`、`component` 和 `event`。`timestamp`
使用 UTC RFC3339Nano，`level` 使用 `debug/info/warn/error` 小写值。`request_id`、
`trace_id` 及各业务 ID 只在真实上下文存在时写入，不输出空值，也不在日志层生成
替代 ID。标准 JSON 日志不保留 `slog` 默认的 `time` 和 `msg` 字段。

M6-01 冻结以下跨 Server/Agent 的运行事件；生命周期只记录已经验证并真实存在的
关联 ID：

```text
management_request_completed
http_ingress_request_completed
tunnel_connection_opened
tunnel_connection_failed
tunnel_connection_closed
agent_origin_connection_failed
agent_connection_failed
agent_connection_opened
agent_connection_closed
windows_service_starting
windows_service_running
windows_service_stop_requested
windows_service_stopped
windows_service_failed
```

Management 与公网 HTTP 请求分别记录 `method/status_code/duration_ms` 和
`method/status_code`；HTTP KeepAlive 通过实际取得的后端连接关联 `connection_id`，不能
把首个请求的连接上下文伪造给后续请求。Tunnel OPEN/终止记录 Tunnel、Service、
Connector、Session、Connection 与 generation；Agent 只在 OpenRequest 通过协议校验并
提交 OPENING 后绑定 `service_id/connection_id/trace_id`。成功生命周期使用 `info`，
客户端或容量等可恢复失败使用 `warn`，取消使用 `debug`，协议/内部错误和无法继续的
Windows Service 失败使用 `error`。失败日志只写有限 `error_code`，不得写底层错误文本；
M6-01 只承接已存在 `trace_id` 的注入和关联，不创建 Span，也不传播 W3C Trace Context，
该职责仍属于 M6-03。

---

# 159. 禁止日志输出

绝不能记录：

```text
Tunnel Token

Admin Password

Session Cookie

Session Secret

TLS Private Key

Authorization Header
```

共享日志 Handler 会对上述明确的敏感属性名写出 `[REDACTED]`。调用方仍不得把
Secret 拼入 `event`、错误文本或任意非敏感字段，也不得直接记录完整 Config、
HTTP Header、Cookie、请求体或认证对象。

Security Audit Event 是持久化安全证据，不等同于可丢弃的普通运行日志。M1-04 的最小
机器契约由 `migrations/000003_security_audit_events.sql` 与 Repository 校验共同执行：

- `event_id=evt_<ULID>` 是主键，`operation_id=op_<ULID>` 全局唯一；相同两个 ID 且全部
  字段一致的重放视为成功，任一 ID 已绑定到不同内容时以冲突失败，绝不覆盖旧证据。
- M1 枚举只允许 `event=SECURITY_OPERATION_RESULT`、`action=GATEWAY_KEY_ROTATE`、
  `actor_type=LOCAL_OPERATOR`、`resource_type=GATEWAY_IDENTITY`，以及
  `result=SUCCEEDED|FAILED`。离线维护命令没有已认证个人身份或网络来源，`actor_id` 与
  `source_ip` 必须为 `NULL`；`resource_id` 为 1—256 字节的 Gateway Public Hostname。
- `error_code` 最多 64 字节，成功时必须为 `NULL`，失败时必须非空；`request_id` 与
  `trace_id` 可空，非空时最多 128 字节。`before_state_digest` 与
  `after_state_digest` 可空，非空时必须精确为 32 字节；`occurred_at` 是大于零的 UTC
  Unix 秒。
- M1 不提供通用 Metadata 列，因而不存在可写的任意 Metadata 字段。未来增加 Metadata
  必须先冻结字段允许列表、单值长度和总大小，且绝不能保存业务正文、Secret、Private
  Key、Cookie 或 Credential 原文。
- Application Audit Writer 只暴露 `Append`。SQLite Trigger 拒绝 UPDATE/DELETE；提交
  使用固定物理连接在 `BEGIN IMMEDIATE` 前临时切换 `synchronous=FULL`。耐久 COMMIT 成功后
  才派生结构化 `security_audit_event` 日志；若随后恢复普通 `NORMAL` 模式失败，必须返回
  可识别的 post-commit cleanup 错误和已提交结果，不能把它误报成回滚。普通 Observer 不得
  替代持久化写入。

M2 的前向 Migration `000004_credential_lifecycle_audit.sql` 在单一 Migration 事务内重建
该表并完整复制 v3 证据、恢复时间索引和 append-only Trigger。新增枚举只包括
`CONNECTION_TOKEN_REVEAL`、`CONNECTION_TOKEN_ROTATE`、`CONNECTION_TOKEN_REVOKE`、
`TUNNEL_REVOKE`；这些事件必须使用已认证的 `actor_type=ADMIN`、合法 `adm_` Actor，
资源分别为 `TUNNEL_TOKEN` 或 `TUNNEL`，`resource_id` 只保存安全的 `tun_` ID。Token 文本、
Secret、Hash 与 Ciphertext 不得进入事件、日志、错误或测试输出。

`gateway rotate-key --maintenance` 在换钥前完成 Migration、Writer 初始化、旧 SPKI 读取及
`event_id/operation_id` 生成；上述步骤失败时不得换钥。v2 Rotation Journal 在替换 Identity
文件前持久化事件/操作 ID、发生时间、资源标识及前后 SPKI SHA-256 Digest，不保存 Private
Key、Token 或其他 Credential。崩溃恢复可以继续完成同一组文件替换，但在权威审计事件幂等
追加成功前必须保留 Journal。

普通 Server 启动在 Admin Bootstrap 或 Gateway Listener 启动前执行 Reconciliation：先恢复
Identity 文件，再验证当前 SPKI 等于 Journal 的 after-state，随后使用原事件/操作 ID 幂等追加
SQLite 事件，最后才删除并同步 Journal。若写入失败，Server 启动失败；若维护命令在 Identity
提交后写入失败，Identity 不回滚，命令以非零退出并记录稳定错误码
`AUDIT_WRITE_FAILED_AFTER_COMMIT`。再次执行维护命令时必须先完成旧事件的 Reconciliation
并直接结束，不得在待审计操作上再轮换一次；数据库已提交但 Journal 尚未清理的重放只能生成
同一条持久化事件。

若事件已经耐久提交且 Journal unlink 成功，但最后一次 PKI 目录同步失败，当前操作记录
`AUDIT_JOURNAL_DIRECTORY_SYNC_FAILED` Warning 后仍按成功结束，避免把 cleanup durability 的
不确定性误报成审计写失败并诱导第二次换钥。断电后 Journal 如果重新出现，启动流程仍以原
ID 幂等重放并再次清理。

M1 写事件，M5 只实现查询 API且不提供 UPDATE/DELETE，M6 实现导出、告警和 Dashboard。

---

# 160. Metrics

Server：

```text
xtunnel_connectors_online

xtunnel_control_sessions_online

xtunnel_active_connections

xtunnel_tcp_idle_work_connections

xtunnel_tcp_active_work_connections

xtunnel_open_total

xtunnel_open_errors_total

xtunnel_ingress_bytes_total

xtunnel_egress_bytes_total

xtunnel_origin_errors_total

xtunnel_health_targets

xtunnel_health_budget_rejections_total

xtunnel_gateway_certificate_expiry_seconds

xtunnel_open_duration_seconds

xtunnel_origin_connect_duration_seconds

xtunnel_reconcile_duration_seconds

xtunnel_reconcile_errors_total

xtunnel_route_snapshot_bytes

xtunnel_route_snapshot_routes

xtunnel_reconcile_coalesced_total
```

九项无 Label Gauge 为 `connectors_online`、`control_sessions_online`、
`active_connections`、`tcp_idle_work_connections`、`tcp_active_work_connections`、
`health_targets`、`gateway_certificate_expiry_seconds`、`route_snapshot_bytes` 和
`route_snapshot_routes` 是 Gauge。`open_total`、`ingress_bytes_total`、
`egress_bytes_total`、`health_budget_rejections_total` 和
`reconcile_coalesced_total` 是无 Label Counter；`open_errors_total`、
`origin_errors_total` 和 `reconcile_errors_total` 是只带 `error_code` 的 Counter。
`open_duration_seconds`、`origin_connect_duration_seconds` 和
`reconcile_duration_seconds` 是 Histogram，前两者固定 Bucket 为
`0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30` 秒，
Reconcile 固定 Bucket 为
`0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5` 秒。
P50/P99 由 PromQL `histogram_quantile` 计算，不另建进程内 Quantile Gauge。

`error_code` 只允许 Protocol v1 `ErrorCode` 的冻结枚举名称；成功不写错误 Counter，
未识别的值和没有公开协议码的内部失败统一归并为 `ERROR_CODE_INTERNAL_ERROR`，不得把
底层错误文本写入 Label。`open_total` 和 Open Histogram 按一次公网逻辑 OPEN 计数，
内部 Connector Failover/Wire OPEN attempt 不得重复累计。Origin Error 与 Connect Duration
读取该逻辑 OPEN 最终 `OpenResponse` 已携带的 `error_code` 和
`origin_connect_latency_ms`，不新增 Wire 字段。

`route_snapshot_bytes/routes` 聚合当前所有成功发布的确定性 Tunnel Snapshot，`routes`
表示其中实际序列化的 Route/Service 条目总数，不统计数据库内未进入这些 Snapshot 的
Service；同 Tunnel 新版本替换旧贡献，Revoke/Delete 同步扣除其贡献。
Ingress/Egress Byte Counter 是进程生命周期、重启归零的无 Service 维度遥测，不是 M6-04
Usage exactly-once 权威。所有 Gauge 只读取既有 Runtime/Owner 的聚合快照；新增 Health、
Gateway 和 Reconcile Source 必须是 O(1) 聚合值，采集不得向 Prometheus 暴露包含 Tunnel、
Connector、Service 或 Source IP 的高基数 Map。

V0.1 的 M6-02 不暴露 Agent 本地 Prometheus 端点，也不新增 Agent Config、Proto 或本地业务
状态。原规划中的八项 `xtunnel_agent_*` 指标延期到单独的 Agent 暴露与多实例抓取契约，
不属于本阶段验收范围。

Server 默认通过独立 loopback 端点暴露 Prometheus：

```yaml
metrics:
  listen: "127.0.0.1:9090"
  path: /metrics
```

实现使用进程私有 Registry 与独立 `ServeMux`，只注册配置中的精确 Path，不挂接 Default
Mux、Management API 或公网 Ingress。Schema 继续允许操作者显式配置非 loopback 地址；
Metrics 端点本身不增加认证，非 loopback 暴露必须由部署网络策略隔离。

核心链路使用 OpenTelemetry Span：

```text
ingress.Accept
 ↓
tunnel.DialContext
 ↓
transport.Acquire
 ↓
origin.Dial
 ↓
proxy.Bidirectional
```

Span 命名采用 `<package>.<FuncName>`。Server 将 W3C `traceparent`、`tracestate` 放入 OpenRequest，Agent 恢复远端 Context 并创建子 Span；日志中的 `trace_id` 必须来自同一 Trace Context，禁止自行生成一条无法关联的平行 ID。

M6-03 使用官方 OpenTelemetry Go `v1.46.0`，Server 与 Agent 分别持有进程私有的
TracerProvider 和 W3C Trace Context Propagator，不写入 OTel 全局状态。公网 HTTP/TCP
入口一律以 `trace.WithNewRoot()` 创建本地 Root，不提取或信任客户端 `traceparent`；
HTTP KeepAlive 上的每个请求也分别创建 Root。Server→Agent 只复用既有
`OpenRequest.trace_id/traceparent/tracestate`，不新增 Wire 字段。三字段全空时保持禁用兼容；
只要任一字段非空，`trace_id` 与 `traceparent` 必须同时存在并指向同一个合法 Trace，
`tracestate` 必须可解析，否则 Agent 在 Origin Dial 前按 `PROTOCOL_ERROR` 终止 WorkConn。

Trace 导出只由进程启动环境启用：

```text
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
OTEL_EXPORTER_OTLP_ENDPOINT
OTEL_EXPORTER_OTLP_TRACES_HEADERS / OTEL_EXPORTER_OTLP_HEADERS
OTEL_EXPORTER_OTLP_TRACES_TIMEOUT / OTEL_EXPORTER_OTLP_TIMEOUT
OTEL_EXPORTER_OTLP_TRACES_COMPRESSION / OTEL_EXPORTER_OTLP_COMPRESSION
OTEL_EXPORTER_OTLP_TRACES_PROTOCOL / OTEL_EXPORTER_OTLP_PROTOCOL
OTEL_EXPORTER_OTLP_TRACES_INSECURE / OTEL_EXPORTER_OTLP_INSECURE
```

未配置 Endpoint 时使用 no-op Provider，不导出 Span。Trace-specific 值优先于 Base 值；
Base Endpoint 自动追加 `/v1/traces`。协议固定为 `http/protobuf`，生产 Endpoint 必须使用
HTTPS，HTTP 只允许 `localhost` 或 Loopback IP；`INSECURE=true` 不能把 HTTPS 降级。
Headers、Endpoint 与配置错误绝不回显原值，文件型 CA、Client Certificate 和 Client Key
环境变量在 V0.1 明确拒绝，避免证书/私钥路径和上游 Parser 错误进入全局 Logger。自定义
CA/mTLS 由前置 Collector 或代理终止。

BatchSpanProcessor 使用固定 2048 有界队列、512 Batch，不启用阻塞入队；Batch、Export 与
Shutdown 上限均为 5 秒。Collector/网络错误由本地 Exporter Wrapper 截断，只通过项目
JSON Logger 最多报告一次 `event=tracing_export_failed error_code=EXPORT_FAILED`，不记录
Endpoint、Header 或底层错误文本。Server 在 Ingress/Gateway/Session/Route/SQLite 收敛后
Flush，Agent 在 Connector/WorkConn/Origin/Health Owner 收敛后 Flush；两者都不让 Collector
阻塞数据面或无限延长退出。Root 默认全采样，容量与比例采样留待 M6 Gate 压测后冻结，
V0.1 不增加 Server Schema、Agent 本地业务配置或第二套 Trace 配置入口。

---

# 161. Prometheus Cardinality

禁止直接使用：

```text
tunnel_id

connector_id

service_id

connection_id
```

作为高频 Metrics Label。

Service 级统计进入：

```text
Usage Aggregator
```

Runtime、Session Registry、WorkPool、Route Snapshot 和 Health Runtime 是唯一运行态权威。
Logs、Metrics、Trace 和 Diagnostics 可通过有界、非阻塞 Observer 派生；Web Runtime View
可以直接读取不可变 Runtime Snapshot，或消费明确标注 staleness 的 Observer View，但
Observer 永不成为权威。Observer 阻塞或普通遥测队列满不得反向阻塞数据面。允许丢弃的
仅是非权威遥测事件，并必须累计 Drop Metric；Security Audit、Usage exactly-once Delta
和任何 Runtime Mutation 不得通过这一丢弃路径实现。

M6 提供 Agent Connectivity Diagnostic。它必须复用生产 Token Parser、Endpoint Parser、
DNS Resolver、TCP Dialer、TLS Builder、SPKI Verifier 和 ALPN 配置，禁止另写一套“诊断
专用”连接栈。默认 Precheck 只验证 Token 结构、DNS、TCP、TLS、证书/Pin 和 ALPN，
不得建立会替换正式 Connector Session 的隐藏认证；若未来增加真实 Auth/Snapshot
诊断，必须先冻结不会污染在线 Session 的独立协议语义。输出至少区分 `PASS`、
`WARNING`、`FAIL`，汇总为 `READY`、`READY_DEGRADED`、`NOT_READY`。Token 输入复用
正式 Bootstrap 的安全来源，不在示例中鼓励把真实 Token 放入共享 Shell History。

V0.1 公共入口固定为 `xtunnel-agent diagnose [--token string]`，Token 来源顺序与正式
Bootstrap 一致：显式 `--token`、`XTUNNEL_TOKEN`、systemd Credential。命令使用生产
10 秒总 Dial Budget，先解析 Token 与 Endpoint，再分别对 Control 和 Work ALPN 执行
DNS、TCP、TLS 1.3、Public CA 或 Pinned SPKI、精确 ALPN 阶段检查；IP Literal 的 DNS
阶段直接 `PASS`。证书剩余不超过 30 天时 Trust 阶段返回 `WARNING`，汇总为
`READY_DEGRADED`；任一失败立即汇总为 `NOT_READY` 并以非零码退出。诊断只建立和关闭
TLS 连接，不发送应用层 Auth 或 Snapshot，不输出完整 Token、Endpoint 或底层错误文本。

Dashboard 额外返回 Server 冻结的 `gateway_certificate`：`tls_mode`、UTC `expires_at`、
`remaining_seconds`、`level`、`recent_renewal_failed` 与可空稳定错误码。等级边界固定为
`HEALTHY`、30 天内 `WARNING`、7 天内 `CRITICAL`、1 天内 `EMERGENCY`、已过期
`EXPIRED`；Web 不得自行重算。Prometheus 版本化规则只消费
`xtunnel_gateway_certificate_expiry_seconds`，用 `time()` 形成互斥的 30 天、7 天、1 天
和已过期窗口。Pinned 热续签成功记录 `gateway_certificate_renewed`；失败记录
`gateway_certificate_renewal_failed` 与 `error_code=CERTIFICATE_RENEWAL_FAILED`。Server
日志可保留经过调用方边界约束的结构化运维错误上下文，但不得包含 Token、私钥或证书
内容；Dashboard/API 只暴露有限错误码，不返回底层错误。Public 证书仍由外部系统续签。

`GET /api/v1/security-audit-events/export` 复用查询接口的 Admin Session Cookie 与六个
筛选条件，返回 `application/x-ndjson`、`Cache-Control: no-store` 和固定下载文件名。
Server 在发送响应前同时固定 SQLite append 序号上界与 `(occurred_at,event_id)` 排序
上界，之后按 opaque keyset 每批最多读取 200 行；并发新增事件即使回填旧时间也不得
混入本次导出，空结果返回空 Body。读取、取消或写入在流开始后失败时必须立即中止连接，
不得伪造完整文件或追加 JSON/500；客户端只可在完整下载成功后原子发布候选文件。

Linux systemd 与 Windows SCM 的 M6-06 Smoke 必须在隔离 Runner 中验证真实启动失败、
持久日志/退出状态和恢复重启。systemd 以运行时 drop-in 缩短 Stop 超时，并把测试时
`KillSignal` 改为不终止进程的 `SIGCONT` 来制造超时诊断，退出后恢复环境；生产 Unit
不预先修改 `TimeoutStopSec` 或 `KillSignal`。Windows 通过
隔离 DPAPI Credential 副本验证 `CREDENTIAL_LOAD_FAILED` 和恢复后的新进程；M6 Gate
另构建只在 CI 使用的 Windows SCM Helper，临时把同一受管 Service 的 ImagePath 切换到
该 Helper，分别让首次运行回调返回错误和忽略 Stop 取消，真实经过生产 Handler 的
`RUNTIME_FAILED` 与 30 秒 `STOP_TIMEOUT` 分支。Smoke 必须从 Application Event Log
定位稳定错误码、确认非零 Service Exit 和恢复后的新 PID，并在每轮 `finally` 恢复生产
ImagePath、停止 Helper、启动原 Agent；Helper、Marker 或测试参数不得进入生产 Binary、
持久 SCM 配置或用户 CLI。具体处置命令与证据边界以 `docs/operations_runbook.md` 为准。

---

# 162. Repository Structure

```text
xtunnel/
├── .gitattributes
├── .gitignore
├── go.mod
├── go.sum
├── buf.yaml
├── buf.gen.yaml
├── buf.lock                 # 有外部 Proto module 依赖时由 Buf 生成
├── tools/
│   ├── versions.env
│   ├── go.mod
│   ├── go.sum
│   ├── bootstrap-proto.sh
│   └── proto.sh
│
├── cmd/
│   ├── server/
│   │   └── main.go
│   │
│   └── agent/
│       └── main.go
│
├── api/
│   ├── proto/
│   │   ├── common.proto
│   │   ├── control.proto
│   │   └── work.proto
│   │
│   └── openapi/
│       └── openapi.yaml
│
├── internal/
│   ├── protocol/
│   │   ├── frame/
│   │   ├── codec/
│   │   ├── version/
│   │   └── gen/
│   │
│   ├── tunnel/
│   │   ├── channel.go
│   │   ├── dialer.go
│   │   ├── proxy.go
│   │   └── error.go
│   │
│   ├── transport/
│   │   ├── transport.go
│   │   └── tcp/
│   │       ├── pool.go
│   │       ├── channel.go
│   │       └── work_conn.go
│   │
│   ├── server/
│   │   ├── bootstrap/
│   │   ├── service/
│   │   ├── app/
│   │   ├── auth/
│   │   ├── api/
│   │   ├── web/
│   │   ├── agent/
│   │   ├── session/
│   │   ├── tunnel/
│   │   ├── route/
│   │   ├── ingress/
│   │   │   ├── http/
│   │   │   └── tcp/
│   │   ├── listener/
│   │   ├── config/
│   │   ├── pki/
│   │   ├── usage/
│   │   └── metrics/
│   │
│   ├── agent/
│   │   ├── bootstrap/
│   │   ├── service/
│   │   ├── app/
│   │   ├── identity/
│   │   ├── control/
│   │   ├── transport/
│   │   ├── origin/
│   │   ├── health/
│   │   └── daemon/
│   │
│   └── repository/
│       ├── repository.go
│       └── sqlite/
│
├── migrations/
│
├── web/
│   ├── embed.go
│   ├── .gitignore
│   ├── vite.config.ts
│   ├── package.json
│   ├── package-lock.json
│   ├── src/
│   └── dist/
│
├── deploy/
│   ├── docker/
│   ├── systemd/
│   │   └── smoke.sh                # Linux Server/Agent Binary Self-install 隔离验收
│   └── windows/
│       └── smoke.ps1               # Windows Agent SCM Self-install
│
├── configs/
│   └── server.schema.json
│
├── docs/
│   ├── architecture/
│   ├── protocol/
│   └── adr/
│
└── tests/
    ├── integration/
    ├── e2e/
    ├── benchmark/
    ├── fuzz/
    └── golden/
        └── protocol-v1/
            ├── workhello.json
            ├── snapshot.json
            └── README.md
```

Proto 工具链固定使用 Buf 管理，但不引入 gRPC：

```text
buf.yaml        → v2 module / STANDARD lint / FILE breaking policy
buf.gen.yaml    → protoc-gen-go 输出到 internal/protocol/gen
buf.lock        → 存在外部 Proto module 依赖时由 Buf 生成的 lock
tools/versions.env → 精确 Buf 版本/分发包 SHA-256与预期 Plugin 版本
tools/go.mod / go.sum → 固定 protoc-gen-go Module 与校验和
```

V0.1 的三份 Proto 按冻结路径直接位于 `api/proto` 根目录，因此 Lint 仅排除与该路径契约冲突的 `PACKAGE_DIRECTORY_MATCH`，其余 `STANDARD` 规则保持启用。M0-06 固定 Buf `v1.72.0` 与 `protoc-gen-go v1.36.12`；Buf 官方 Linux amd64/arm64 单文件分发包分别固定 SHA-256。当前没有外部 Proto module 依赖，Buf `dep update` 不生成 `buf.lock`；禁止手写空 Lock File，后续出现真实依赖时只接受 Buf 生成结果。

生成文件提交仓库。统一命令：

```bash
./tools/bootstrap-proto.sh
./tools/proto.sh lint
./tools/proto.sh breaking
./tools/proto.sh generate-check
```

`bootstrap-proto.sh` 只支持 Linux amd64/arm64。它读取 `versions.env`，把匹配 SHA-256 的 Buf Release Binary 安装到根 `.gitignore` 排除的 `.tools/bin`；`protoc-gen-go` 则从 `tools/go.mod/go.sum` 通过 `GOTOOLCHAIN=local go build -mod=readonly` 构建到同一目录，并核对输出版本。下载或构建先写同目录临时产物，校验成功后才替换正式文件。`proto.sh` 只调用该目录的绝对路径，在每次执行前核对版本，禁止回落到开发机 PATH。`buf.gen.yaml` 使用 `clean: true`，生成前清理纯生成目录，防止删除 Proto 后遗留孤立 `*.pb.go`。`generate-check` 执行 generate 后再运行：

```bash
git diff --exit-code -- api/proto internal/protocol/gen buf.lock
```

还必须通过 `git status --porcelain --untracked-files=all` 检测同一范围内的 staged/untracked 漂移，因为 `git diff` 不覆盖这两类文件。M0-06 尚无 `.proto`：`lint` 和 `generate-check` 明确报告无输入，`breaking` 明确报告初始契约尚未冻结；空输入只能作为 Wrapper 机械链路证据。一旦出现 `.proto` 而 M05-04 尚未建立不可变初始 Baseline，`breaking` 必须失败，禁止与当前 Schema 自比较伪造 PASS。

CI 调用同一 Wrapper；不得维护另一套安装或生成命令。HMAC 输入与 Snapshot 大小 Gate 必须继续显式调用 deterministic protobuf Marshal，并用跨平台 Golden Vector 验证；生成代码一致不等于安全字节已经正确。

WorkHello Golden Vector 必须包含固定 `session_secret`、nonce、完整输入字段、deterministic protobuf hex、带 Domain Separator 的 HMAC input、HMAC 和最终 Message hex；Snapshot Golden Vector 固定完整输入、deterministic protobuf hex 与字节大小。测试逐字节比较已有 Fixture；禁止在普通测试运行中自动重写 Fixture。更新 Fixture 必须作为显式 Protocol Review 变更，并同时通过 unknown-field、字段乱序和空字段测试。

---

# 163. Server Start

```text
Load Config
 ↓
Initialize Logging
 ↓
Resolve Stable Data Target From realpath(parent) + basename
 ↓
Acquire Server External Lock
 ↓
Recover / Roll Back Pending Restore Journal
 ↓
Validate Canonical Data Directory
 ↓
Open SQLite
 ↓
Run Migration
 ↓
检查 tunnel_tokens 是否已有密文
 ↓
Load/Create 独立 Tunnel Token Master Key
 ↓
Recover / Roll Back Gateway Identity Rotation Journal
 ↓
Load/Create Gateway TLS Identity
 ↓
Load Route Snapshot
 ↓
Start Usage Flusher
 ↓
Start Management :8080
 ↓
Check Admin Bootstrap State
 ↓
SETUP_REQUIRED → 只保留 Management，等待本机 admin create
 ↓
Restore TCP Listeners
 ↓
Start HTTP Ingress :8081
 ↓
Start Agent Gateway :7443
 ↓
Start Runtime Reconciler
 ↓
READY
```

Server External Lock 必须在读取 Restore Journal、Open SQLite、Migration、Tunnel Token Master Key Load/Create、Gateway PKI Load/Create 和任何 Runtime 初始化之前获取，并一直持有到所有 Listener、Connector Session 和 SQLite 都关闭。Lock Identity 只依赖始终存在的父目录和稳定 leaf 名，不依赖 leaf 当前存在，因此两个 rename 之间崩溃后仍能取得同一把锁。Lock 文件不在 Data Directory 内，Restore 替换目录不会改变已锁 inode。第二个指向同一 Stable Data Target 的 Server 必须在触碰数据库或身份文件之前快速失败，不能等端口绑定冲突才退出。

Gateway TLS Identity 默认只允许在全新 Data Directory 初始化时创建。唯一例外是管理员显式执行第 26 节离线 `gateway rotate-key --maintenance`；普通 Server Start 绝不自动触发该例外。如果数据库已经存在但 Gateway Identity 文件缺失，且没有可恢复的 Rotation Journal，Server 必须快速失败并要求从一致性备份恢复，禁止静默生成新身份。

没有 Admin User 时，Management 状态为 `SETUP_REQUIRED`；HTTP/TCP Public Ingress 和 Agent Gateway 在首个管理员完成初始化前不启动。

---

# 164. Agent Start

```text
Resolve `--token` / `XTUNNEL_TOKEN` / OS Service Credential
 ↓
Validate Bootstrap Shape
 ↓
Parse Connection Token Version / Integrity / Semantics
 ↓
Derive Endpoint + TLS Trust + Tunnel/Token Credential
 ↓
Generate Connector ID
 ↓
Connect Agent Gateway
 ↓
TLS Verify
 ↓
Tunnel Token Auth
 ↓
Create Control Session
 ↓
Receive Full Current Snapshot
 ↓
Validate + Atomic Swap In-memory Resolver
 ↓
ConfigAck
 ↓
Start Health Checker
 ↓
Heartbeat Pool Intent
 ↓
Receive Budget Lease + Fill Work Pool
 ↓
Heartbeat
 ↓
ONLINE
```

---

# 165. Unit Tests

必须覆盖：

```text
Token Hash

Token Rotation

Tunnel State

Connector State

Session Authentication

Work HMAC

WorkHello Replay Without Wall Clock Dependency

Work Frame Boundary + RAW Handoff

WorkConn Unexpected Structured Message Direct Close Before RAW

Protocol v1 Direction / State Matrix

Protocol v1 Recursive Unknown-field Rejection

Protocol Golden Vector Byte Equality

Frame Decoder

Route Matcher

Origin Resolver

Work Pool

Connector Selection

Config Revision

Snapshot Service Count + Serialized Size Boundary

Session Generation Fencing

Control Session Single Reader / Single Writer

Control Outbox Priority / Coalescing / Full-close

TunnelRuntime Linearization + Lock-free IO Rule

ActiveWork CloseOnce + Counter Exactly-once

Reconcile Generation Monotonicity

Snapshot Deterministic Bytes + In-memory Atomic Apply

Full Snapshot On Every New Control Session

SQLite Repository

SQLite PRAGMA On Every Pooled Connection

ConfigWriteCoordinator Serialization

Health Revision Fencing

WorkDemand Coalescing + Budget Lease Expiry

Health Scheduler Rate / Concurrency / Jitter / Batch

Tunnel / Connector / Service Status Priority

Strict Server Config Schema + Override Precedence

Tunnel Token Bootstrap Source Precedence + Shape Limits

Lease / Health / Drain Monotonic Time Semantics

Drain Two-phase State Machine

Forwarded Header Sanitization

Canonical Host + Path Segment Boundary

Canonical Path Prefix Trailing Slash

Origin DNS / IPv4 / IPv6 Dial Policy

OpenAPI Schema / Nullable PATCH / ETag Contract
```

---

# 166. Integration Tests

```text
Server
 ↕
Agent
 ↕
Fake Origin
```

覆盖：

```text
Connection Token Parse / Version / Authentication

Pinned Certificate Same-SPKI Renewal

Offline Gateway Key Rotation: Old Connection Token Rejected / Newly Issued Token Accepted

Gateway Identity Rotation Journal Crash Recovery

Multiple Connector

Control Reconnect

Old Session Cleanup After New Session

Old Session Cleanup Preserves ActiveWork

Concurrent Session Replace + Drain + OPEN

WorkConn Registration

OPEN

OPEN_OK

OPEN_ERROR

Config Push

Snapshot 768 KiB Boundary + Oversize Transaction Rejection

Revision

Old Revision Connector Ineligible

Concurrent Revision 18 / 19 Reconcile

Health

Old Revision Health Report Cannot Override New UNKNOWN/UNHEALTHY

Health Batch Generation / Split / Deduplication

Health Target Budget Rejects Config Write And Excess Connector Auth

Health Budget At-capacity Session Replacement Does Not Double Reserve Or Release New Generation

Token Rotation

SQLite Migration Upgrade + Interrupted Migration

Concurrent Config Write + Usage Flush + Token Rotate Without Unhandled SQLITE_BUSY

Backup → Migration → Restore → Agent Reconnect

Backup Secret File Permissions + Symlink Rejection

Restore Crash Between Directory Renames

Second Server Refuses Same Data Directory Before SQLite/PKI Access

Independent `database.path` Config Is Rejected

Concurrent Service Writes Both Present In Runtime Snapshot

Service Cannot Move Across Tunnel Without Explicit Application Transaction

Tunnel Delete With Service Returns 409 TUNNEL_IN_USE

Stateless Agent Restart Receives Full Current Snapshot Before ONLINE

Authorized Server Restore Establishes New Session Baseline Without Agent Local State

Agent/Server Wall Clock Skew ±5min Does Not Expire Lease, Health Or Drain Early

Agent/Server Wall Clock Skew Does Not Affect WorkHello Authentication

Origin DNS Address Change Without Agent Restart

Health And Business Dial Share Resolver / SNI Policy

Concurrent PATCH With Same ETag Returns One Success + One 412

OpenAPI Generated Client / Server Contract Drift Check
```

---

# 167. HTTP E2E

测试：

```text
GET

POST

PUT

Chunked Request

Chunked Response

Disable Chunked + Known Content-Length

Disable Chunked + Unknown Length Explicit Rejection Without Whole-body Buffering

Streaming

Known-Length Low-rate Small-chunk Response Flush <= 100ms + Margin

KeepAlive

KeepAlive Pool Isolated By Tunnel + Service + Config Version

HTTP Idle Timeout / Max Idle Connections + Global WorkConn/FD Budget

Host Priority: origin_http_host > preserve_host > origin host

Client Disconnect

Large Header

Malformed Host

Path Segment Boundary

Dot Segment

Encoded Slash

Forwarded Proxy Chain

`/admin` And `/admin/` Canonical Route Conflict
```

---

# 168. WebSocket E2E

```text
Connect

Bidirectional Message

Ping/Pong

Client Close

Origin Close

Long Connection

Connector Failure
```

---

# 169. TCP E2E

至少：

```text
TCP Echo

SSH

Database TCP Protocol
```

重点：

```text
Half-Close

TCP Origin Receives No Implicit PROXY Header

Disable Happy Eyeballs Preserves Total Connect Timeout

TCP KeepAlive 30s Default And 0 Disables
```

---

# 170. 大文件测试

必须：

```text
1GB Upload

1GB Download
```

监控：

```text
RSS

Heap

GC

Goroutine

CPU
```

内存不得随文件大小线性增长。

---

# 171. 并发测试

第一阶段：

```text
100

1000

5000
```

并发 Connection。

统计：

```text
Success Rate

P95 Tunnel Open

Memory

CPU

File Descriptor

Goroutine
```

---

# 172. Connector Load Test

一个 Tunnel：

```text
1 Connector

2 Connector

10 Connector

100 Connector
```

验证：

```text
Load Distribution

Connector Crash

Connector Reconnect

WorkPool Isolation

Global WorkConn Budget

WorkConn Budget Fairness Across Tunnels/Connectors

FD Budget Counts Public + WorkConn Socket Pair

Connector A idle=0、Connector B idle>0 时优先使用 B
```

---

# 173. Agent Scale Test

Mock Agent：

```text
100

500

1000
```

Tunnel。

总 Connector：

```text
5000
```

验证：

```text
Heartbeat

Session Registry

Reconnect Storm

Memory
```

---

# 174. Network Chaos

使用：

```text
network namespace

tc netem

nftables
```

模拟：

```text
RTT 100ms

Loss 1%

Loss 5%

Jitter 50ms

Bandwidth 10Mbps

TCP Reset
```

测试分级固定为：

```text
Pull Request Gate
├── Unit / Integration / Race / Short Fuzz
└── 无特权的断连、超时和短写故障注入

Nightly Privileged Linux
├── network namespace
├── tc netem / nftables
└── 受限 Loss / Jitter / Reset / Reconnect Matrix

Release Gate
├── 完整 Network Chaos
├── Crash Test
├── Scale / Resource Leak
└── Backup / Restore Recovery
```

Nightly 和 Release 必须运行在允许 Network Namespace 与 `CAP_NET_ADMIN` 的专用 Linux Runner，不能因普通 CI 无权限而静默跳过。Release Gate 失败阻止 Alpha Artifact 发布。每次运行上传 Kernel、Go 版本、网络参数、测试 Seed、关键 Metrics 和日志摘要，便于复现。

---

# 175. Crash Test

必须：

```text
kill -9 Agent Connector

kill -9 XTunnel Server
```

检查：

```text
Connector Failover

Agent Reconnect

SQLite Integrity

Snapshot Integrity

WorkConn Cleanup

Goroutine Leak
```

---

# 176. Security Test

覆盖：

```text
Invalid Tunnel Token

Revoked Token

Token Brute Force

Token Rotation

Tunnel Revoke

Invalid Server Pin

Expired Pinned Certificate

PROXY v1/v2 Prefix Before Agent TLS Is Rejected

Invalid Work HMAC

Replay WorkHello

WorkHello Replay Cache Capacity

Invalid Session

Oversized Frame

Malformed Protobuf

Unauthorized Tunnel ID

Config Snapshot Replay

Config Snapshot Wrong Tunnel ID

Config Revision Rollback Within One Session

Admin CSRF

Admin Brute Force

Forwarded Header Spoof

Forwarded Multi-Hop Spoof
```

---

# 177. Fuzz

必须 Fuzz：

```text
UVarint Decoder

Frame Decoder

Control Envelope

WorkHello

Host Parser

Path Prefix Matcher

Forwarded Header Parser
```

所有网络输入：

```text
Untrusted Input
```

---

# 178. M0：工程初始化

完成：

```text
Go 1.27 Module 与固定工具链

Repository

CI

Linux Server/Agent amd64/arm64 + Windows Agent amd64/arm64 Build Matrix

Multi-arch OCI Image

Compose v2 IPv4/IPv6 Bridge Profile + Host Port Binding Smoke

双栈静态监听底层原语（M0 未接入产品启动路径）

OCI Builder/Runtime Base 使用以下不可变多架构索引摘要：
node:24.19.0-bookworm-slim@sha256:3638d9a6fe4030bd716be989438248074489337ba3275657f93595428be4fc03
golang:1.27.0-bookworm@sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466
gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

Linux Server/Agent systemd 与 Windows Agent SCM Binary Self-install Smoke Harness

Server Config + Agent Token Bootstrap

Logging

SQLite Migration

Proto Toolchain + Schema Skeleton

OpenAPI Skeleton + Validate

Pinned Buf / protoc-gen-go Toolchain + Generate Drift Check

Web Skeleton

Web Production Build + Go Embed

Locked Web Dependencies (`package-lock.json`)

Vite HTTPS Dev Proxy + CSRF Same-origin Flow
```

验收：

```text
go.mod 声明 go 1.27，根 Module/tool Module/CI/OCI Builder 使用同一个稳定 go1.27.x 补丁版本

所有 Go 验收命令设置 GOTOOLCHAIN=local，证据中的 go env GOVERSION 均为已批准的精确 go1.27.x 版本

Server / Agent 可以启动

Linux Server/Agent amd64/arm64 与 Windows Agent amd64/arm64 Binary 均可构建；arm64 在原生或受控 Runner 完成 Agent Token Bootstrap 与 Shutdown Smoke

OCI amd64/arm64 Image 以前台非 root 进程运行，验证只读镜像、Server 持久化 Data Volume、Agent `XTUNNEL_TOKEN` 注入、Agent 无 Volume 和 SIGTERM 进程退出/资源释放 Smoke（M0 不要求真实 Session Drain）

Compose v2 Profile 为 Server/Agent 分配 IPv4/IPv6 地址，建立 Management 回环和 Agent Gateway 宿主机全部 IPv4/IPv6 地址的端口绑定，并保持非 root、只读根、Server Data Volume、Agent 无持久卷与 Server Runtime tmpfs 边界

双栈监听原语完成原生 tcp4/tcp6 Dial/Accept、同端口、IPV6_V6ONLY 与第二地址族绑定失败清理测试；真实产品 Listener 连通仍留到 M1/M4

Linux Server/Agent Binary `service install/uninstall`、managed marker、systemd >=249、原子发布/回滚、start/restart/stop Smoke；Server 配置/Data Target/上一版官方 Unit 接管；Agent LoadCredential；Windows Agent SCM/LocalService、ProgramFiles Binary、ProgramData DPAPI Machine-scope Credential、Description marker、Replace Existing + Write Through、30s Stop/Shutdown、non-crash recovery、install/reinstall/restart/uninstall 与按需延迟到重启删除 Binary Smoke

全新 checkout 按固定顺序完成 Web Build 和 Go Build

CI 使用 `npm ci`，缺失或与 `package.json` 不一致的 Lockfile 直接失败；CI 不自动生成或改写 Lockfile

Server Config Schema、Strict YAML、CLI/Env/YAML/Default 优先级测试通过；Agent `--token`/`XTUNNEL_TOKEN`/Linux systemd Credential/Windows DPAPI Credential 优先级和输入边界测试通过

OpenAPI Skeleton Validate 通过且不存在未解析占位 Server URL

Vite HTTPS Proxy Harness 验证证书失败、`/api/v1` 转发和 Host/Origin 保持；真实 Login / Secure Cookie / CSRF POST / Logout E2E 留到 M5
```

---

# 179. M0.5：Protocol v1 Contract Freeze

完成：

```text
common.proto / control.proto / work.proto

Connection Token v1 精确编码 / 版本 / 完整性 / 解析失败语义

固定 package / go_package

完整 Message / Enum / Reserved Range

Control 与 WorkConn 方向/状态矩阵

Recursive Unknown-field Rejection

Deterministic Protobuf 规则

Protocol Golden Vector
```

验收：

```text
./tools/proto.sh lint = PASS

./tools/proto.sh breaking = PASS

./tools/proto.sh generate-check = PASS

Connection Token / Protocol Golden Vector 逐 byte PASS

Auth Success / Failure Transcript PASS

Auth 裸 Frame 与 Established Envelope 切换边界 PASS

Control / WorkConn 全部非法方向和非法状态 Case PASS

WorkConn 错误方向/状态/Unknown Field 直接关闭 Case PASS

Snapshot Deterministic Bytes / Revision / ConfigAck 组合校验 PASS

Auth、Control、Work、Snapshot 的全部结构化 Message 递归 Unknown Field Case 均被拒绝
```

M0.5 是强制 Gate，不是可与 M1 并行补写的文档任务。M0.5 未通过，禁止实现 Server/Agent Protocol Handler；允许继续开发与 Wire Contract 无关的 Lock、Repository、Proxy、Origin Dialer 和测试 Harness。

---

# 180. M1：Secure TCP Data Plane Baseline

M1 使用正式身份和安全协议，先完成一个 Tunnel、一个 Connector、一个静态 Service 的安全数据面基线；领域模型和协议从一开始就使用最终的 Tunnel→Connector→Service 语义。

完成：

```text
Protocol v1 Generated Contract

Tunnel Entity + Stable Retrievable Token Issuance / Verification

Ephemeral Connector ID + Session ID

Runtime Registry + Session Generation Fencing

Control Single Reader / Writer / State Owner

Bounded + Coalesced Control Outbox

Agent Gateway

TLS

Control Session

Session Secret

WorkConn HMAC

Replay Protection

Work Pool

Budget Lease

Frame / Auth / Queue / Connection Limits

Timeout + FD Budget

OPEN

OPEN_OK

RAW

Half-Close

Baseline Control Reconnect

Baseline Server / Agent Graceful Shutdown
```

验收：

```text
Public TCP
 ↓
Server
 ↓
Service
 ↓
Echo Origin
```

验收必须包含逐字节分片、多个 Frame 合并、`OPEN_OK + RAW 首包` 同一次 Read、Half-Close、Context Cancel、Control Reconnect、旧 Session cleanup 不影响新 Session、Outbox 合并/满载关闭以及所有 M1 硬限制生效；断言字节零丢失、零重复，测试结束后 FD 与 goroutine 回到基线。

---

# 181. M2：Credential Lifecycle & Failover Hardening

同一 Token 的多 Connector 并存、默认负载选择和优雅排空已经是 M1 数据面基线，
不能留到 M2 才首次实现。M2 在该基线上增加规模化并发验证、凭据生命周期、
可观测性与故障切换。

完成：

```text
Multi-Connector Scale / Isolation / Churn Suite

Connector Selection Hardening（Least Active + RR under churn）

Online Connector Lifecycle + Observability

Token Rotation + Revoke

Tunnel Revoke

Old Session ActiveWork Preservation

Connector Failover
```

验收：

```text
同一个 Token

并发启动 3 个以上 Agent Connector

Server 能独立识别所有 Connector

新连接在 Session churn 与容量变化下仍自动、公平分布

旧 Session ActiveWork 自然完成，Revoke 可跨代关闭
```

---

# 182. M3：Configuration + Health

完成：

```text
Service（直接属于 Tunnel）

Revision

Snapshot

In-memory Atomic Apply

Full Snapshot On Start / Reconnect

Origin Resolver

Health Scheduler + Batch Report

Health Target Budget
```

验收：

```text
Integration Test 通过 Application Service 修改 Origin

Agent 无需重启

自动生效

Agent 仅凭 Connection Token 启动且不读取本地状态，完整拉取并 Ack 后才 ONLINE

Health Budget 与配置 Interval 均可被自动化验证
```

M3 不依赖尚未实现的 Web Console 或 REST Handler。测试直接调用与后续 REST API 共用的 Application Service；M5 只增加 HTTP 契约和界面，不重新实现配置写入逻辑。

---

# 183. M4：HTTP + TCP Product Data Plane

完成：

```text
Route Snapshot

HTTP Router

Reverse Proxy

Forwarded Headers

WebSocket

TCP Listener Manager
```

验收：

```text
HTTP

HTTPS via Caddy

WebSocket

SSH

Raw TCP
```

全部正常。

---

# 184. M5：OpenAPI + REST API + Web Console

M5 Entry Gate（Handler 和 Web 并行开发前必须通过）：

```text
OpenAPI 完整冻结

Pagination / PATCH / ETag / Error Schema 完整

Lint + Breaking Check + Generated Contract Drift Check = PASS
```

入口 Gate 通过后，OpenAPI 在 M5 期间只能通过显式 Contract Change Review 修改，不得由 Handler 或 UI 实现反向定义契约。

对外 HTTP API 服务允许采用 Gin 作为路由和 Middleware 框架，优先用于 Management REST API。Gin 只承担 HTTP 适配、参数绑定、认证授权 Middleware 和响应写出；OpenAPI 仍是唯一 API 契约，Handler 必须调用既有 Application Service，不得在框架层重新实现事务或业务规则。Tunnel HTTP Ingress、Streaming Reverse Proxy、WebSocket 和其他数据面路径按各自协议语义实现，不因为采用 Gin 而强行接入同一套路由栈。具体 Gin 版本在首次实现对应 API 任务时确认并锁定，本决策不要求在尚无 Handler 的阶段提前引入依赖。

完成：

```text
Login

Generated OpenAPI Client / Server Contract

Pagination / PATCH / ETag

Dashboard

Tunnel CRUD

Connector View

Token Reveal / Rotate / Revoke

Service CRUD

Service Status

Settings

Security Audit Read-only Query

Service Composite ETag / Nested Exposure
```

验收：

```text
日常使用无需操作 SQLite

无需手改 Agent Service Config

并发 PATCH 不丢失更新

OpenAPI Client / Server Contract 零漂移
```

---

# 185. M6：Observability

完成：

```text
JSON Log

Metrics

Usage Aggregation

Error Code

Dashboard Health
```

能够快速定位：

```text
Tunnel Offline

Connector Offline

Origin Down

No Capacity

Protocol Error
```

---

# 186. M7：Hardening

完成：

```text
Resource Limit / Timeout / Rate Limit 参数调优

Graceful Shutdown Chaos + Deadline / Leak 收敛

Reconnect Storm + Backoff / Fencing 收敛

Crash Recovery Failpoint 覆盖收敛

磁盘满 / EIO / fsync / rename Failpoint

Race Detector

Goroutine Leak

Fuzz

Concurrency

Large Transfer

Privileged Network Chaos

Token-only Foreground / OCI + Linux systemd / Windows SCM Agent Binary Self-install Deployment Matrix
```

M7 不允许第一次实现 Frame、Queue、Auth、Connection、FD 或 Health Budget 上限；这些正确性边界必须从 M1/M3 起存在。M7 只负责压测校准、异常注入、泄漏消除和发布阈值收敛。

完成后：

```text
XTunnel Standalone Alpha
```

可以发布。

---

# 187. 第一阶段最终验收流程

## 通用通过标准

以下 Gate 必须全部满足，禁止只以“可以访问”或“看起来稳定”判定通过：

```text
go test ./... = PASS

go test -race ./... = PASS，零 Data Race

`./tools/proto.sh lint / breaking / generate-check` = PASS

Protocol v1 Golden Vector + Direction/State Matrix = PASS

Server JSON Schema 与 Go Config Struct Drift Check + Agent Token Bootstrap Contract Tests = PASS

OpenAPI Validate + Generated Client/Server Contract Drift Check = PASS

npm ci / npm run build = PASS，Lockfile 零漂移

Linux Server/Agent amd64/arm64 + Windows Agent amd64/arm64 Build Matrix = PASS，arm64 Protocol Smoke = PASS

OCI amd64 / arm64 Manifest + Agent `XTUNNEL_TOKEN` / Server Volume / SIGTERM Smoke = PASS

Linux Agent Binary Self-install / Managed Marker / LoadCredential / Restart / Uninstall Smoke = PASS，Unit ExecStart 仅为 `xtunnel-agent run` 且无 Secret

Windows Agent Binary Self-install / SCM / LocalService / ProgramFiles / DPAPI Machine-scope Credential / Description Marker / Restart / Uninstall Smoke = PASS，SCM ImagePath 仅为安装 Binary + `run` 且无 Secret，运行中 EXE 延迟删除最终收敛

协议 Fuzz Corpus = PASS，零 Panic / OOM

Control Outbox 满载、Session Replace、Drain/OPEN 并发 = PASS，零 Frame 交错

Stateless Agent Restart / Reconnect Full Snapshot Gate = PASS

Health Scheduler Rate/Concurrency/Batch 与两级 Target Budget = PASS

1000 并发连接成功率 >= 99.9%

5000 并发连接成功率 >= 99.5%

同机或 RTT <= 1ms 基线环境下 Tunnel Open P95 <= 200ms

1GB Upload / Download SHA-256 与 Origin 端一致

测试结束并等待 30s 后：Active Connection = 0

测试结束并等待 30s 后：FD 回到基线 + 10 以内

测试结束并等待 30s 后：Goroutine 回到基线 + 20 以内

日志扫描：Token / Password / Cookie / Private Key 命中数 = 0

Release Privileged Chaos / Crash / Restore Gate = PASS，并上传可复现 Artifact
```

基线报告必须记录 CPU、内存、Go 版本、内核、`RLIMIT_NOFILE`、RTT、带宽和测试配置。若环境不满足延迟基线，可以单独标记性能 Gate 未验证，但功能、安全和资源泄漏 Gate 不得跳过。

## Server

```text
全新 Linux Server
 ↓
安装 xtunnel-server
 ↓
配置 Caddy/Nginx
 ↓
启动
 ↓
SETUP_REQUIRED
 ↓
本机执行 admin create
 ↓
Public Listener READY
 ↓
Web Console Available
```

---

## Agent

```text
Create Tunnel
 ↓
Copy one Connection Token
 ↓
sudo xtunnel-agent service install --token 'xta_...'
或在提升权限的 Windows PowerShell 中执行 `.\xtunnel-agent.exe service install --token 'xta_...'`
 ↓
启动后拉取完整当前 Snapshot
 ↓
Tunnel ONLINE
```

---

## Connector

同一 Token：

```text
Connector A
Connector B
```

Server：

```text
Connectors = 2
```

停止 A：

```text
B 继续接受新连接
```

---

## HTTP

Origin：

```text
127.0.0.1:8080
```

Route：

```text
app.tunnel.example.com
```

最终：

```text
https://app.tunnel.example.com
```

正常。

---

## Upload / Download

```text
1GB Upload

1GB Download
```

内存稳定。

---

## WebSocket

长连接：

```text
>= 1h
```

稳定。

---

## TCP

```text
Server :10022
 ↓
Agent
 ↓
Origin :22
```

SSH 正常。

---

# 188. 第一阶段 ADR

仓库初始化建议立即建立：

```text
ADR-001-tunnel-and-connector.md

ADR-002-versioned-connection-token.md

ADR-003-connector-session-identity-hierarchy.md

ADR-004-agent-gateway-port.md

ADR-005-external-http-tls-termination.md

ADR-006-tcp-work-connection.md

ADR-007-no-tcp-multiplex.md

ADR-008-route-service-tunnel-model.md

ADR-009-origin-resolved-only-by-agent.md

ADR-010-tunnel-channel-abstraction.md

ADR-011-runtime-state-memory-only.md

ADR-012-sqlite-desired-state.md

ADR-013-revision-and-snapshot.md

ADR-014-stateless-agent-config-apply.md

ADR-015-connector-selection.md

ADR-016-no-quic-v0.1.md

ADR-017-proto-is-wire-contract.md

ADR-018-control-session-and-runtime-ownership.md

ADR-019-full-snapshot-on-session-start.md

ADR-020-status-aggregation.md

ADR-021-server-config-schema-and-agent-token-bootstrap.md

ADR-022-health-scheduler-and-budget.md

ADR-023-openapi-etag-concurrency.md
```

---

# 189. 第一阶段最重要的工程约束

开发过程中不得破坏：

```text
1. Tunnel 是管理与认证聚合根；Agent 进程连接后形成临时 Connector。

2. 一个 Tunnel 可以同时拥有多个 Connector，默认共同承载流量并互为备份。

3. Connection Token 属于 Tunnel，携带 Endpoint、TLS Trust、Tunnel/Token Identity 与 Secret；添加 Connector 重复取回同一 ACTIVE Token，只有显式 Rotate 才产生新版本。

4. Connector / Session 必须是临时运行身份，不得引入 Installation 持久化。

5. Token 默认长期有效，但必须支持 Rotate 和 Revoke。

6. WorkConn 不重复发送长期 Tunnel Token。

7. Route 必须指向 Service，Service 必须直接属于一个 Tunnel；不再存在中间关联资源。

8. OPEN 只能携带 Service ID，
   不能携带 Origin 地址。

9. Origin 只能由 Agent 根据 Server 下发的当前内存配置解析。

10. Runtime Socket 只存在内存。

11. SQLite 不承担数据面热路径。

12. TCP 一个 WorkConn 只承载一个业务 Connection。

13. Proxy 只能依赖 TunnelChannel。

14. 所有业务数据必须 Streaming。

15. Half-Close 必须正确。

16. HTTP TLS 由前置 Caddy/Nginx 处理。

17. Agent Gateway 使用独立 TCP 端口。

18. 所有网络 Frame、Queue、Connection 都必须有上限。

19. 配置同步必须使用 Revision。

20. Snapshot 只能经已认证的 TLS Control Session 下发，并完整校验后原子替换。

21. `.proto` 是唯一 Wire Contract，M0.5 未通过不得实现协议 Handler。

22. 每条 Control Session 只能有一个 Reader、一个 Writer、一个 State Owner。

23. Runtime State 在 TunnelRuntime Lock 下线性化，锁内禁止 IO、阻塞和 Conn.Close。

24. Agent 不保存本地 Desired State；每个新 Control Session 必须获取完整当前 Snapshot。

25. Tunnel/Connector 不聚合 Origin Health；Service Status 只由 Server 统一计算。

26. 只有 Server 主配置使用 JSON Schema 与 Strict Decode；Agent 只接受一个版本化 Connection Token。

27. Health Check 必须中心调度、批量上报并服从两级 Target Budget。

28. REST API 以 OpenAPI 为唯一契约，Mutation 使用 ETag/If-Match 防止丢失更新。
```

---

# 190. 第一阶段最终产品形态

最终：

```text
                    Caddy / Nginx
                          │
                          ▼
                    XTunnel Server
                    ┌─────────────┐
                    │ Management  │
                    │ HTTP Ingress│
                    │ TCP Ingress │
                    │ Agent GW    │
                    │ SQLite      │
                    └──────┬──────┘
                           │
                 TLS/TCP Tunnel
                           │
                      Tunnel
                           │
                ┌──────────┼──────────┐
                │          │          │
                ▼          ▼          ▼
            Connector A Connector B Connector C
                │          │          │
                ▼          ▼          ▼
              Service    Service    Service
                 │          │          │
                 ▼          ▼          ▼
               Origin     Origin     Origin
```

公网请求：

```text
Route
 ↓
Service
 ↓
Tunnel
 ↓
Eligible Connector
 ↓
TCP WorkConn
 ↓
Origin
```

这是第一阶段需要真正建立起来的核心系统。

第一阶段完成后，XTunnel 应已经具备：

```text
集中管理

多 Tunnel

多 Connector 负载与互备

HTTP Tunnel

TCP Tunnel

WebSocket

动态配置

健康检查

流量统计

稳定 TCP 数据面

完整 Web Console
```

它不再是一个简单内网穿透 POC，而是一个可以长期运行、能够作为后续 XTunnel Cluster 和 Platform 基础的 **Standalone Reverse Tunnel Platform**。
