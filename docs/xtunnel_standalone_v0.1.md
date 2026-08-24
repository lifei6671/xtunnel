# XTunnel Standalone 第一阶段完整技术方案 V0.1

> **文档状态**：开发基线
> **目标状态**：完成后可作为 XTunnel Alpha 发布
> **核心语言**：Go 1.27
> **部署形态**：单 Server + 多逻辑 Agent + 多 Agent Replica
> **数据存储**：SQLite
> **数据传输**：TLS/TCP
> **Web**：React + TypeScript + Vite + Tailwind CSS + shadcn/ui
> **Public HTTP 前置代理**：Caddy / Nginx
> **Agent Gateway 默认端口**：TCP 7443，可配置
> **核心定位**：可直接部署使用的集中式反向隧道 Standalone 产品
> **修订日期**：2026-08-24
> **本次修订**：冻结 Go 1.27 工具链基线、Protocol v1 交付门、Control Session 并发所有权、Agent Trust State、状态聚合、统一配置、Health 容量、OpenAPI 契约与里程碑依赖

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
创建逻辑 Agent
        ↓
获得 Agent Token
        ↓
启动一个或多个 Agent Instance
        ↓
Server 识别 Agent ONLINE
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

Agent 管理

Agent Token

Agent Replica

Agent Instance 状态

Agent Control Session

TCP Work Pool

Tunnel

Tunnel Binding

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
Agent Token 认证

Installation Identity

Instance Identity

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

Config Snapshot

Last Known Config

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
        Agent Instance   Agent Instance
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
Tunnel
   │
   ▼
Agent Instance
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
Config

Server / Agent JSON Schema + Config Drift Check
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
Tunnel
  │
  ▼
TunnelBinding
  │
  ▼
Agent
  │
  ▼
Agent Instance
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

# 7. Agent 的定义

第一阶段必须明确：

> Agent 不是一台机器，也不是一个运行进程。

Agent 是：

> 一组共享相同 Tunnel Binding 和 Origin Configuration，并被视为等价流量入口的逻辑 Connector。

因此：

```text
Agent
│
├── Instance A
├── Instance B
└── Instance C
```

多个 Instance 可以：

```text
运行在同一台机器

运行在多台机器

运行在多个 Container

未来运行在多个 Kubernetes Pod
```

只要它们能够按照相同配置访问相同语义的 Origin。

---

# 8. 身份层次

XTunnel 第一阶段定义四级身份：

```text
Agent
    ↓
Installation
    ↓
Instance
    ↓
Session
```

对应：

```text
agent_id

installation_id

instance_id

session_id
```

---

# 9. agent_id

代表：

```text
逻辑 Agent
```

由 Server 创建。

例如：

```text
ag_01ARZ3NDEKTSV4RRFFQ69G5FAV
```

它拥有：

```text
Agent Name

Agent Token

Tunnel Bindings

Desired Revision

Configuration
```

例如：

```text
Agent:
production-office

Services:
├── Jenkins
├── GitLab
└── SSH
```

---

# 10. installation_id

表示：

> 一个具体 Agent 安装目录。

第一次启动时生成：

```text
inst_01ARZ3NDEKTSV4RRFFQ69G5FAV
```

保存在：

```text
<data-dir>/installation.id
```

不会因为进程重启改变。

例如：

```text
Host A
/var/lib/xtunnel/a
→ inst_01ARZ3NDEKTSV4RRFFQ69G5FAV

Host A
/var/lib/xtunnel/b
→ inst_01ARZ3NDEKTSV4RRFFQ69G5FAW
```

这样同一台服务器可以运行多个 Replica。

---

# 11. instance_id

每启动一个：

```text
xtunnel-agent
```

进程生成新的：

```text
instance_id
```

例如：

```text
ai_01ARZ3NDEKTSV4RRFFQ69G5FAV
```

进程重启后：

```text
instance_id
```

重新生成。

它代表：

> 当前正在运行的具体 Agent 进程。

---

# 12. session_id

Agent 每次成功建立 Control Session：

```text
Server
↓
Generate Session ID
```

例如：

```text
sess_01ARZ3NDEKTSV4RRFFQ69G5FAV
```

断网重连：

```text
Instance ID
保持

Session ID
重新生成
```

如果进程重启：

```text
Instance ID
重新生成

Session ID
重新生成
```

---

# 13. 身份树示例

```text
Agent
ag_01ARZ3NDEKTSV4RRFFQ69G5FAV
│
├── Installation
│   inst_01ARZ3NDEKTSV4RRFFQ69G5FAV
│   │
│   ├── Instance ai_01ARZ3NDEKTSV4RRFFQ69G5FAV
│   │      └── Session sess_01ARZ3NDEKTSV4RRFFQ69G5FAV
│   │
│   └── Instance ai_01ARZ3NDEKTSV4RRFFQ69G5FAW
│          └── Session sess_01ARZ3NDEKTSV4RRFFQ69G5FAW
│
└── Installation
    inst_01ARZ3NDEKTSV4RRFFQ69G5FAW
    │
    └── Instance ai_01ARZ3NDEKTSV4RRFFQ69G5FAX
           └── Session sess_01ARZ3NDEKTSV4RRFFQ69G5FAX
```

---

# 14. 同一主机运行多个 Agent

允许：

```bash
xtunnel-agent install \
  --data-dir /var/lib/xtunnel/replica-a \
  ...

xtunnel-agent install \
  --data-dir /var/lib/xtunnel/replica-b \
  ...
```

两个实例使用：

```text
相同 Agent Token
```

但拥有不同：

```text
installation_id
instance_id
session_id
```

---

# 15. Data Directory Lock

同一个 Data Directory：

```text
只允许一个 Agent 进程
```

启动时打开：

```text
agent.lock
```

并对该文件获取进程全生命周期持有的非阻塞 OS 文件锁：

```text
Linux: flock(LOCK_EX | LOCK_NB)
```

锁文件残留不代表 Data Directory 正在使用。只有文件锁获取失败才拒绝启动。

创建和打开锁文件时必须禁止跟随符号链接，文件权限为 `0600`。

如果：

```text
/data-dir
```

已经被另一个 Agent 使用：

```text
Agent refuses to start
```

避免：

```text
Snapshot 冲突

Token File 冲突

Installation ID 冲突

并发文件写入
```

同机 Replica 使用不同 Data Directory。

---

# 16. Agent Token

Agent Token 是：

> 逻辑 Agent 的长期 Credential。

它不是一次性 Enrollment Token。

例如：

```text
xta_A7dP...
```

生成：

```text
crypto/rand
32 bytes
```

然后：

```text
Base64URL
```

加固定前缀：

```text
xta_
```

---

# 17. Agent Token 生命周期

默认：

```text
长期有效

允许多个 Instance 同时使用

可以 Rotate

可以 Revoke
```

管理员创建 Agent 后：

```text
Agent
 ↓
Generate Token
 ↓
Show Once
```

Token 明文只在创建或 Rotate 时返回一次。

数据库只保存：

```text
SHA-256(token)
```

由于 Token 拥有 256 bit 随机熵，不需要使用 Argon2 等慢哈希。

---

# 18. Token Rotation

管理员：

```text
Rotate Token
```

Server：

```text
Generate Token v2
        ↓
v1 = REVOKED_FOR_NEW_SESSION
        ↓
v2 = ACTIVE
```

语义：

```text
已有 Control Session
继续工作

旧 Token
不能建立新的 Control Session

新 Token
允许建立新 Session
```

如果需要安全强制下线：

```text
Revoke Agent
```

而不是 Rotate。

---

# 19. Agent Revoke

管理员执行：

```text
Revoke Agent
```

Server：

```text
Agent status = REVOKED
        ↓
所有 Token 禁用
        ↓
关闭所有 Control Session
        ↓
关闭所有 Idle WorkConn
        ↓
关闭所有 Active WorkConn
```

属于强安全操作。

Revoke 的 Desired State、全部 Token 状态和 `agents.version` 必须在同一个 `BEGIN IMMEDIATE` 事务中提交并受 `If-Match` 保护；事务提交后再在 Runtime Lock 内收集需要关闭的 Session/WorkConn，释放锁后执行实际 Close。

---

# 20. Agent Token 表

```sql
CREATE TABLE agent_tokens (
    id TEXT PRIMARY KEY,

    agent_id TEXT NOT NULL,

    token_hash BLOB NOT NULL UNIQUE,

    version INTEGER NOT NULL,

    status TEXT NOT NULL,

    created_at INTEGER NOT NULL,

    revoked_at INTEGER,

    FOREIGN KEY(agent_id)
        REFERENCES agents(id)
        ON DELETE CASCADE,

    UNIQUE(agent_id, version)
);
```

状态：

```text
ACTIVE
REVOKED_FOR_NEW_SESSION

REVOKED
```

第一阶段一个 Agent 只允许一个 ACTIVE Token。

SQLite 使用条件唯一索引保证该约束：

```sql
CREATE UNIQUE INDEX one_active_token_per_agent
ON agent_tokens(agent_id)
WHERE status = 'ACTIVE';
```

Rotate 必须在同一个 `BEGIN IMMEDIATE` 事务中完成旧 Token 状态更新、新 Token 插入和 version 递增。

这里同时包含 Agent Aggregate 的乐观锁：事务先校验 `agents.version == If-Match`，成功后把 `agents.version` 递增一次。Token 自身的 `agent_tokens.version` 继续表示 Credential 代次，两种 Version 不得混用。

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

Agent：

```yaml
server:
  endpoint: tunnel.example.com:7443
```

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
Agent Token Authentication
```

不强制使用 Client Certificate。

TLS Session Resumption 只作为性能优化，不能改变 Agent Token、WorkHello HMAC、Budget Lease 或 Replay 检查。V0.1 禁止在 Agent Protocol 上使用 0-RTT Application Data；是否启用受限 Client Session Cache 由 5000 Instance 重连与 WorkConn 建连基准决定，并必须定义 Ticket Key 生命周期。未启用 Resumption 不能影响功能正确性或发布 Gate。

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

自签证书默认有效期为 397 天。Server 在剩余 30 天时使用原有 Private Key 自动续签新证书，因此 SPKI 不变，Agent 无需更新 Pin。续签必须原子写入并热加载；失败时继续使用仍有效的旧证书，同时产生 ERROR 日志和告警 Metric。

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
/data/pki/
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
xtunnel-server gateway rotate-key --maintenance \
  --new-pin-output /secure/xtunnel-new-gateway-pin
```

命令要求 Server 已停止并取得 Server External Lock；如果任何 Server 进程仍持锁则拒绝。新 Pin 文件使用 `O_CREATE | O_EXCL`、权限 `0600`，禁止输出到 stdout。命令读取 Installation 清单，生成新的 Key/Certificate 到 `pki` 同盘临时文件，写入并 fsync Gateway Identity Rotation Journal 后再原子 rename。崩溃后 Server 启动必须先根据 Journal 完成或回滚，禁止加载 Key/Certificate 不匹配的组合；成功后写入 Security Audit Event。

完整维护流程：

```text
停止 xtunnel-server
 ↓
执行 gateway rotate-key --maintenance
 ↓
通过受信配置管理或本机 Secret 文件更新所有 Agent server_pin
 ↓
重启每个 Agent，使其重新读取 Pin
 ↓
核对 Installation 清单与遗漏项
 ↓
启动 xtunnel-server
```

Agent 不热加载 `server_pin`；只有进程启动时从权限受控 Config 读取。禁止在 Pin 不匹配时自动接受新 Key、TOFU 覆盖或回落 `--insecure`。未完成 Pin 更新并重启的 Installation 会保持离线，必须人工重新配置；这是 V0.1 明确接受的维护中断。在线双 Pin 轮换留到后续协议版本。

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

Agent 首先建立：

```text
TLS
```

然后发送：

```protobuf
message AgentAuthRequest {
    string token = 1;

    string installation_id = 2;
    string instance_id = 3;

    string hostname = 4;
    string machine_id = 5;

    string version = 6;
    string os = 7;
    string arch = 8;

    uint32 min_protocol = 9;
    uint32 max_protocol = 10;

    uint64 observed_revision = 11;

    repeated string capabilities = 12;

    string observed_signing_key_id = 13;
    string observed_server_epoch = 14;
}
```

---

# 29. Server Authentication

Server：

```text
SHA256(token)
 ↓
agent_tokens lookup
 ↓
Token ACTIVE?
 ↓
Agent REVOKED?
 ↓
Protocol compatible?
 ↓
Authentication success
```

身份冲突规则：

```text
installation_id 已绑定其他 agent_id
→ INSTALLATION_ID_CONFLICT

同一 instance_id 已存在 Current Session
→ 新 Session 完成认证后递增 generation，并 fencing 旧 Session

旧 Session cleanup
→ 只能清理自己的 session_id + generation
```

`installation_id`、`instance_id`、`machine_id` 是 Agent Token 认证后的运行时标识，不是独立安全凭据。Server 不得在 Token 验证前依据这些自报字段执行覆盖或删除。

V0.1 的信任边界是：持有某个 Agent Token 的进程，被视为该逻辑 Agent 的完整受信副本。它可以注册新的 Installation、接收该 Agent 的全部 Binding，并承接对应业务流量；Installation ID 只用于审计和冲突检测，不能降低 Token 泄漏后的权限。Server 首次见到 Installation 时必须写入 Security Event，并在 Web Console 中突出展示。若不同主机之间不能共享这一信任边界，管理员必须为它们创建不同的逻辑 Agent；每 Installation 独立 Enrollment Credential 不属于 V0.1。

认证统一返回显式 Result，而不是只定义成功响应：

```protobuf
message AgentAuthResult {
    oneof result {
        AgentAuthSuccess success = 1;
        AgentAuthFailure failure = 2;
    }
}

message AgentAuthSuccess {
    string agent_id = 1;

    string session_id = 2;

    bytes session_secret = 3;

    uint32 protocol_version = 4;

    uint64 desired_revision = 5;

    uint32 heartbeat_interval_ms = 6;

    string config_signing_key_id = 7;
    bytes config_signing_public_key = 8;

    string server_epoch = 9;
}

message AgentAuthFailure {
    ErrorCode error_code = 1;
    uint32 retry_after_ms = 2;
}
```

`config_signing_public_key` 只在 Agent 尚未确认该 `key_id`，或 Server 正处于签名 Key 轮换期时返回。

认证失败流程固定为：

```text
TLS Established
 ↓
AgentAuthRequest
 ↓
AgentAuthResult{failure: AgentAuthFailure}
 ↓
在 control.write_timeout 内 flush 完整 Frame
 ↓
Close TLS Connection
```

除 TLS 已经不可写或对端提前关闭外，禁止用直接 EOF 代替认证失败结果。`TOKEN_INVALID`、`TOKEN_REVOKED`、`AGENT_REVOKED`、`INSTALLATION_ID_CONFLICT`、`VERSION_UNSUPPORTED` 和可重试的 Server 容量错误必须能够被 Agent 区分。只有可重试错误允许设置非零 `retry_after_ms`；永久 Credential、Pin 或版本错误不得通过短周期自动重连放大负载。

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

避免每一个 WorkConn 都发送长期 Agent Token。

---

# 31. WorkConn Authentication

Agent 新建 WorkConn：

```protobuf
message WorkHello {
    string agent_id = 1;

    string installation_id = 2;

    string instance_id = 3;

    string session_id = 4;

    string work_id = 5;

    reserved 6;

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
agent_id         = ag_<26-char Crockford ULID>
installation_id  = inst_<26-char Crockford ULID>
instance_id      = ai_<26-char Crockford ULID>
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

Instance 必须匹配

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

所有 Protocol v1 结构化 Message，只要自身或任意递归子消息存在 Protobuf Unknown Fields，就必须在业务、HMAC、签名、Revision 或 Transition 判断前以 `PROTOCOL_ERROR` 拒绝。该规则同时覆盖 Auth、Control、Work、Snapshot 和本地 Last Known Snapshot 恢复。禁止在某一端 discard、另一端 preserve，也禁止把未知字段静默带入 deterministic marshal。V1 需要扩展时必须发布 Protocol v2，或新增由已协商 Capability 明确启用的独立 Message，不能向既有 v1 Message 偷加字段。

MAC/签名输入必须由已验证的已知字段重新构造，清空 `mac`、`signatures` 或 `signature_by_current` 后使用固定版本 `google.golang.org/protobuf` 的 `proto.MarshalOptions{Deterministic: true}` 生成。升级该 Runtime 必须重新运行全部 Golden Vector；Golden Vector 字节变化属于 Protocol Breaking Change。

---

# 33. Protocol Framing

Control Session 和 WorkConn 的结构化阶段统一使用：

```text
UVarint Frame Length
+
Protobuf Message
```

Frame 内层类型按连接与状态唯一确定：AUTH 使用裸 `AgentAuthRequest` / `AgentAuthResult`；ESTABLISHED/DRAINING Control 使用 `ControlEnvelope`；WorkConn 在 RAW 前按状态使用唯一合法的裸 Work Message。

AUTH 阶段使用同样 UVarint Length 和 MaxAuthFrameSize，但不把 Auth Message 放入 `ControlEnvelope`。Server 只在完整 `AgentAuthResult.success` Frame 已在 `write_timeout` 内 flush 成功后才原子切换到 `ESTABLISHED`；Agent 只在完整解码并验证该 Success 后切换。两个提交点之前双方均禁止发送或接受 `ControlEnvelope`。Auth Failure flush 后直接关闭，不进入 ControlEnvelope 阶段。

Control Session Envelope：

Envelope：

```protobuf
message ControlEnvelope {
    uint32 protocol_version = 1;

    oneof payload {
        Heartbeat heartbeat = 10;

        AgentSnapshot config_snapshot = 11;

        ConfigAck config_ack = 12;

        WorkDemand work_demand = 13;

        TunnelHealthBatch tunnel_health_batch = 14;

        DrainRequest drain_request = 15;

        Error error = 16;

        ConfigKeyTransition config_key_transition = 17;

        EpochTransition epoch_transition = 18;

        DrainAck drain_ack = 19;
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
| AgentAuthRequest | ✓ | × | ✓ | × | × |
| AgentAuthResult | × | ✓ | ✓ | × | × |
| Heartbeat | ✓ | × | × | ✓ | ✓ |
| AgentSnapshot | × | ✓ | × | ✓ | × |
| ConfigAck | ✓ | × | × | ✓ | ✓ |
| WorkDemand | × | ✓ | × | ✓ | × |
| TunnelHealthBatch | ✓ | × | × | ✓ | ✓ |
| DrainRequest | ✓ | × | × | ✓ | 幂等 |
| DrainAck | × | ✓ | × | ✓ | 幂等 |
| ConfigKeyTransition | × | ✓ | × | ✓ | × |
| EpochTransition | × | ✓ | × | ✓ | × |
| Error | ✓ | ✓ | × | ✓ | ✓ |

AUTH 阶段不使用 `ControlEnvelope.Error`。Server 能安全解码 `AgentAuthRequest` 但发现版本、未知字段或认证语义错误时，发送 `AgentAuthResult.failure{error_code: PROTOCOL_ERROR 或对应 Auth Error}`，flush 后关闭；Frame 已无法安全解码时直接关闭。Agent 在 AUTH 收到无法解码、非法 oneof 或非期望 Result 时直接关闭，不发送 Control Error。

`ESTABLISHED/DRAINING` 收到错误方向、当前状态不允许的 Message 时，接收端应在仍可安全写入时发送 `ControlEnvelope.Error{error_code: PROTOCOL_ERROR}`，随后关闭 Control Session。完全相同的 Transition、DrainRequest 和 DrainAck 必须幂等，返回或重发当前状态；同一 ID 但内容不同必须视为 `PROTOCOL_ERROR`。`protocol_version` 必须等于 TLS/Auth 协商出的版本，任何不一致都关闭 Session。

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

收到 `OPEN_OK` 后，已经位于 socket/buffer 中但不属于 OpenResponse Frame 的剩余字节，必须作为 RAW 数据原样交给代理层，禁止丢弃或重复。

---

# 34. Frame Size

统一限制：

```text
MaxControlFrameSize = 1 MB

MaxAuthFrameSize = 64 KB

MaxWorkFrameSize = 64 KB

MaxAgentSnapshotBytes = 768 KiB
```

`MaxAgentSnapshotBytes` 是 AgentSnapshot 本体的业务上限，必须低于 1 MB Control Frame，为 Envelope、签名、未知字段和后续兼容字段预留空间。禁止把 1 MB 当作可用 Payload 大小。

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

错误隔离范围固定为：Control Frame 错误关闭对应 Control Session；WorkHello、WorkReady、OpenRequest 或 OpenResponse Frame 错误只关闭对应 WorkConn。只有认证级错误，或同一 Control Session 在滑动窗口内连续超过协议违规阈值，才关闭并短暂封禁整个 Control Session，禁止因单条业务连接的 malformed Frame 清空整个 Instance。

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

# 36. Runtime Ownership and Agent Runtime Registry

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
├── ConfigKeyTransition / EpochTransition（严格有序）
├── ConfigAck（含 Transition observed fields）
└── 最新 Heartbeat

Coalescible
├── AgentSnapshot              key = agent_id，保留最高 revision
├── WorkDemand                 key = instance_id，保留最高 generation
└── TunnelHealth pending accumulator，按 tunnel_id 保留最新项
```

旧 Heartbeat 尚未发送时由新 Heartbeat 覆盖，不允许累计。Health 结果在唯一 pending accumulator 中按 `tunnel_id` 合并；只在出队并冻结为不可变 `TunnelHealthBatch` 时才分配严格递增的 `generation`，已冻结 Frame 不再改写。Transition 必须先于依赖新 Key/Epoch 的 Snapshot 入队和写出。Normal Queue 满时，先执行上述合并；仍无法容纳的新消息不得无限等待。High Priority Queue 满、完整 Frame 在 `write_timeout` 内无法写完，或 Owner 无法保证消息次序时，记录 `SESSION_RESOURCE_EXHAUSTED` 并关闭该 Session。关闭动作必须解除 readLoop/writeLoop 的阻塞并等待二者退出，禁止遗留 goroutine。

Server 内存：

```go
type AgentRuntime struct {
    mu sync.Mutex

    AgentID string

    Instances map[string]*InstanceRuntime

    ActiveWork map[string]*ActiveWorkRuntime
}
```

Instance：

```go
type InstanceRuntime struct {
    InstanceID     string
    InstallationID string

    Hostname string

    CurrentSession *ControlSession

    SessionGeneration uint64

    TCP *TCPTransport

    Health map[string]TunnelHealth

    ConnectedAt time.Time

    Draining bool
}
```

Active WorkConn 必须独立于 Current Session 保存：

```go
type ActiveWorkRuntime struct {
    ConnectionID string
    AgentID      string
    InstanceID   string
    SessionID    string
    Generation   uint64
    WorkID       string

    Cancel context.CancelFunc
    WorkConn net.Conn
    PeerConn net.Conn
    closeOnce sync.Once
}
```

一个逻辑 Agent 内所有 Instance、Session、WorkPool、Health 和 ActiveWork 状态变化，都必须在对应 `AgentRuntime.mu` 下线性化。不同 Agent 使用不同锁；禁止建立跨 Agent 的嵌套 Runtime Lock。

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

Session 清理只能在 `instance_id + session_id + generation` 仍匹配时修改 Current Session。旧 Session 的延迟清理不得删除或关闭新 Session。

Agent Revoke 必须通过 Agent 级 ActiveWork Registry 找到并关闭所有旧、新 Session 的 Active WorkConn。

旧 Session cleanup 只能关闭属于旧 Session 的 Idle/Opening WorkConn。已经进入 ACTIVE 的旧 WorkConn 必须继续留在 Agent 级 ActiveWork Registry，直到自然结束、Agent Revoke 或 drain timeout；cleanup 不得仅因 Session 已被 fencing 就删除或关闭它们。

---

# 37. Runtime State 不进 SQLite

以下对象：

```text
Control Session

WorkConn

Active Tunnel Connection

Agent Instance

Session Secret

Pending OPEN
```

全部：

```text
只存在内存
```

Server 重启后通过 Agent 重连重新建立。

---

# 38. Agent Installation 持久化

SQLite 可以保存：

```text
Installation Metadata
```

用于 UI 展示历史主机信息。

```sql
CREATE TABLE agent_installations (
    id TEXT PRIMARY KEY,

    agent_id TEXT NOT NULL,

    hostname TEXT,

    machine_id TEXT,

    os TEXT,
    arch TEXT,

    first_seen_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,

    FOREIGN KEY(agent_id)
        REFERENCES agents(id)
        ON DELETE CASCADE
);
```

它不代表实时 Session。

---

# 39. Agent 状态

逻辑 Agent 状态：

```text
PENDING

ONLINE

DEGRADED

OFFLINE

REVOKED
```

计算规则：

```text
Agent revoked
→ REVOKED

Agent 创建后从未成功完成认证
→ PENDING

曾经成功认证，但当前没有 Current Control Session
→ OFFLINE

至少一个 Instance Status == ONLINE
→ ONLINE

至少一个 Current Control Session，
但所有 Instance 都是 DEGRADED / DRAINING
→ DEGRADED
```

Agent Status 只聚合 Connector Runtime，不读取任何 Tunnel/Origin Health。某个 Instance 可以访问 SSH Origin、但不能访问 Jenkins Origin，此时 Agent/Instance 仍可能 ONLINE；差异只反映在对应 Service Status。

---

# 40. Instance 状态

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
+ Instance-wide Transport 可以接受新 Work

DEGRADED
= Current Control Session 存活
+ Heartbeat Fresh
+ Instance-wide Transport 持续无法接受新 Work
```

`Instance-wide Transport` 只包含 Control/WorkPool/Budget/FD 等 Instance 级能力。Per-Tunnel Origin Health 不参与 Instance Status。Heartbeat 超时或 Control Session 关闭后，Instance 不保留一个永久 OFFLINE Runtime 状态，而是按下述 Tombstone 规则删除或保留。

Instance 不保存永久 OFFLINE Runtime 对象。

如果当前 Session 断开且没有旧 Active WorkConn：

```text
从 Runtime Registry 删除
```

如果仍有旧 Active WorkConn，则保留不可选择的 Instance Tombstone，直到 Active 数归零。它不参与新连接选择，但继续承担计数、Usage、日志和 Revoke 归属。

Installation 页面通过：

```text
last_seen_at
```

显示历史。

---

# 41. Heartbeat

默认：

```text
heartbeat_interval = 10s

heartbeat_timeout = 30s
```

`AgentAuthSuccess.heartbeat_interval_ms` 是该 Control Session 的 Server 权威值，必须满足 `0 < heartbeat_interval <= heartbeat_timeout / 3`。Agent 认证成功后立即采用，不能继续使用本地旧默认值。Server 以本地单调时钟记录“最后一次成功收到 Heartbeat”的时间并判断 Timeout，不使用客户端 `timestamp_ms` 计算存活，避免时钟漂移造成误下线。

Agent：

```protobuf
message Heartbeat {
    uint64 timestamp_ms = 1;

    uint64 observed_revision = 2;

    uint32 tcp_idle = 3;
    uint32 tcp_active = 4;

    uint64 ingress_bytes = 5;
    uint64 egress_bytes = 6;

    string observed_signing_key_id = 7;
    string observed_server_epoch = 8;

    uint32 requested_target_idle = 9;
    uint32 tcp_connecting = 10;
}
```

Heartbeat 的 `ingress_bytes` / `egress_bytes` 按第 110 节全局方向定义，是当前 Control Session 建立以来的累计诊断值，Session 重建后从零开始。它们只用于 Replica 运行态对账和异常检测，不写入 `usage_minutes`，也不与 Server 数据面 `UsageCounter` 再次相加；持久化计费/报表以 Server 数据面计数为唯一来源，避免双重入账。

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
        tunnelID string,
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

Agent Instance

TCP Pool
```

---

# 44. TCP Work Pool

每个 Instance 独立维护：

```text
Control Session
+
Work Pool
```

例如：

```text
Agent
│
├── Instance A
│   ├── Control
│   └── WorkPool A
│
└── Instance B
    ├── Control
    └── WorkPool B
```

默认：

```yaml
transport:
  tcp:
    min_idle: 4
    target_idle: 8
    max_idle: 32
    max_connecting: 16
    max_total: 256
```

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

`desired_non_active` 是该 Instance 的 `Connecting + Idle + Opening` 绝对目标，不是“再增加多少”。Agent 只处理最新 `demand_generation`；目标降低或公网 Pending 消失时，Server 必须发送新 generation 更新或取消 Demand。

Server 发送 WorkDemand 前，必须由全局 WorkConn Budget Manager 为 `budget_lease_id` 预留最多 `max_new_connections` 个槽位。Lease 绑定 `agent_id + instance_id + session_id + session_generation`，不能跨 Session 或 Instance 使用。双方在收到消息时分别用本地 monotonic clock 从 `lease_ttl_ms` 建立 Deadline，禁止比较跨主机绝对时间；Server 是 Lease 是否仍有效的最终裁决者。Agent 只能在本地 Deadline 前建连，并在 WorkHello 中携带该 Lease ID；Server 在 WorkHello 验证阶段检查自己的 Deadline 并原子消费一个槽位。未消费槽位在 TTL 到期、Session 关闭或 Demand 取消时归还。Agent 仍受本地 `max_connecting` 限制，不能把 Lease 当作绕过本地上限的许可。

Control Session 建立后，Agent 通过 Heartbeat 报告 `requested_target_idle`；Server 综合全局、Agent、Instance 和 FD 预算生成初始 Demand。Agent 不得在没有有效 Lease 时主动创建 WorkConn。

公网请求最多等待：

```text
2s
```

即：

```text
work_acquire_timeout = 2s
```

---

# 49. Tunnel OPEN

Server 获得 WorkConn 后发送：

```protobuf
message OpenRequest {
    uint32 protocol_version = 1;

    string connection_id = 2;

    string tunnel_id = 3;

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
        tunnelID string,
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
}
```

第一阶段：

```text
http

https

tcp
```

`Origin.Host` 接受规范化的 IP Literal 或 DNS Hostname。禁止在 Snapshot Apply 时把 DNS 永久解析并保存为单一 IP。

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

Tunnel 是逻辑 Service。

```sql
CREATE TABLE tunnels (
    id TEXT PRIMARY KEY,

    name TEXT NOT NULL,

    enabled INTEGER NOT NULL DEFAULT 1,

    version INTEGER NOT NULL DEFAULT 1,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
```

---

# 58. Tunnel Binding

```sql
CREATE TABLE tunnel_bindings (
    id TEXT PRIMARY KEY,

    tunnel_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,

    required_revision INTEGER NOT NULL DEFAULT 0,

    origin_scheme TEXT NOT NULL,
    origin_host TEXT NOT NULL,
    origin_port INTEGER NOT NULL,

    tls_verify INTEGER NOT NULL DEFAULT 1,
    tls_server_name TEXT,

    origin_http_host TEXT,

    connect_timeout_ms INTEGER NOT NULL DEFAULT 5000,

    health_type TEXT,
    health_path TEXT,
    health_interval_ms INTEGER,
    health_timeout_ms INTEGER,
    health_expected_status_min INTEGER,
    health_expected_status_max INTEGER,
    health_failure_threshold INTEGER,
    health_success_threshold INTEGER,

    enabled INTEGER NOT NULL DEFAULT 1,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,

    FOREIGN KEY(tunnel_id)
        REFERENCES tunnels(id)
        ON DELETE CASCADE,

    FOREIGN KEY(agent_id)
        REFERENCES agents(id)
        ON DELETE RESTRICT,

    UNIQUE(tunnel_id)
);
```

V0.1 的 Web、REST、Application Service、Repository 和 SQLite 必须共同保证：

```text
一个 Tunnel
恰好一个 Binding
只绑定一个逻辑 Agent
```

不得只在 UI 层限制。读取到零个或多个 Binding 都属于持久化不变量破坏，Runtime Reconcile 必须进入 `APPLY_FAILED`，不能自行猜测 Agent。跨 Agent Binding、健康状态合并和选择策略留到真正设计完成后再通过 Migration 引入，V0.1 不提前预留不可执行状态。

---

# 59. Agent Replica 的配置语义

同一逻辑 Agent 下：

```text
所有 Instance
```

获得相同 Tunnel Binding。

因此必须满足：

> 每个 Instance 都应该能使用相同 Origin 配置正确访问同一个逻辑服务。

例如：

```text
Origin:
10.10.0.20:8080
```

所有 Replica 都应能访问。

如果：

```text
127.0.0.1:8080
```

则所有 Replica 本机：

```text
127.0.0.1:8080
```

应代表相同服务。

---

# 60. Instance Selection

Tunnel 建立流程：

```text
Tunnel
 ↓
Binding
 ↓
Logical Agent
 ↓
Eligible Instances
 ↓
Instance Selection
 ↓
TCP WorkPool
```

过滤：

```text
Control Session ONLINE

Not Draining

Health Check Disabled
或 Tunnel Health == HEALTHY

Instance ObservedRevision >= Tunnel RequiredRevision
```

---

# 61. Instance Selection Algorithm

第一阶段采用两级选择：

```text
1. Eligible 且 idle > 0
   ↓
   原子 Acquire Idle WorkConn

2. 多个 Instance 都有 Idle 时
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

如果候选 Instance 的 Idle WorkConn 被并发请求抢走：

```text
立即尝试下一个有 Idle 的 Instance
```

只有所有 Eligible Instance 都没有 Idle WorkConn 时，才进入 WorkDemand 流程。

不引入复杂动态权重算法。

---

# 62. 无空闲 WorkConn

如果：

```text
有可用 Instance

但所有 idle = 0
```

Server 按 Instance 聚合所有 Pending OPEN 的需求，只保留一个最新绝对目标。需求上升时更新 Demand；Pending 减少或取消时同步降低目标，禁止每个公网请求分别广播一条 Demand。

Server：

```text
向最多 2 个最佳 Instance
发送 WorkDemand
```

等待：

```text
work_acquire_timeout
```

谁先提供 WorkConn：

```text
谁承担连接
```

避免一次请求广播所有 Replica。

---

# 63. Connector HA

如果：

```text
Agent A
├── Instance 1
└── Instance 2
```

Instance 1 Crash：

```text
已有 Instance 1 Connection
允许失败
```

新连接：

```text
自动选择 Instance 2
```

第一阶段因此天然支持：

```text
Agent Replica HA
```

---

# 64. Desired Revision

每个逻辑 Agent 保存：

```text
desired_revision

observed_revision
```

例如：

```text
desired = 18

observed = 17
```

代表至少一个配置更新尚未完成。

---

# 65. 多 Instance Revision

因为一个 Agent 可以拥有多个 Instance：

Server Runtime 实际维护：

```text
Agent Desired Revision
        │
        ├── Instance A Observed 18
        ├── Instance B Observed 18
        └── Instance C Observed 17
```

逻辑 Agent：

```text
observed_revision
```

数据库字段只用于摘要：

```text
MIN(instance observed)
```

不是数据面判断依据。

运行时以：

```text
Instance ObservedRevision
```

为准。

Instance Selection 还必须满足：

```text
Instance ObservedRevision
>=
Tunnel RequiredRevision
```

每次修改 Tunnel Binding 时记录该 Tunnel 对应的 `required_revision`。旧 Revision Instance 不得承接该 Tunnel 的新连接。

配置发布顺序：

```text
SQLite Commit Desired State
 ↓
Build Agent Snapshot
 ↓
Push Snapshot
 ↓
ConfigAck
 ↓
Instance ObservedRevision 更新
 ↓
该 Instance 对对应 Tunnel Eligible
```

不存在满足 Revision 的 Instance 时返回：

```text
503 CONFIG_NOT_OBSERVED
```

---

# 66. Agent Snapshot

```protobuf
message AgentSnapshot {
    string agent_id = 1;

    uint64 revision = 2;

    repeated TunnelBindingConfig bindings = 3;

    repeated SnapshotSignature signatures = 4;

    string server_epoch = 5;
}

message SnapshotSignature {
    string key_id = 1;
    bytes signature = 2;
}

message TunnelBindingConfig {
    string tunnel_id = 1;
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

V0.1 同时限制：

```text
max_bindings_per_agent = 1000

deterministic serialized AgentSnapshot <= 768 KiB

encoded ControlEnvelope <= MaxControlFrameSize
```

所有可能改变 Agent Snapshot 的 Management 写入，必须在 SQLite Commit 前从事务内 Candidate State 构建受影响 Agent 的完整 Snapshot，并检查 Binding 数、确定性序列化大小和最终 Envelope 大小。超限返回 `422 AGENT_BINDING_LIMIT` 或 `422 SNAPSHOT_TOO_LARGE`，事务不得提交。

Server 启动和 Migration 后也必须对现有数据执行同一检查；不合法时保持 Public Listener 未启动并报告具体 Agent/大小，禁止进入“Agent 重连 → 收到超大 Frame → 再重连”的循环。V0.1 不实现 Snapshot 分片或依赖压缩绕过上限；带整体 Hash 和签名的分片协议留到后续版本。

---

# 67. Snapshot Signing

Server 独立生成：

```text
Ed25519 Config Signing Key
```

Server：

```text
signing_bytes =
"xtunnel-config-v1"
||
deterministic_protobuf(AgentSnapshotWithoutSignatures)

signature = Ed25519.Sign(config_signing_private_key, signing_bytes)
```

签名和验签必须使用 deterministic protobuf；`signatures` 字段在计算时清空。未知字段不得静默参与一次、忽略另一次，Server 与 Agent 必须使用同一规范化实现。

Agent：

```text
Receive
 ↓
Verify Signature
 ↓
Revision Check
 ↓
Apply
```

---

# 68. Config Signing Key

Server：

```text
/data/pki/config-sign.key

/data/pki/config-sign.pub

/data/pki/server.epoch
```

Agent 不再把单独的 `config-sign.pub` 视为权威信任状态。Agent 端固定使用：

```text
<data-dir>/identity/trust-state.pb
```

保存完整、可恢复的本地信任状态。该文件是本地持久化格式，不是线上 Protocol Message：

```protobuf
message AgentTrustState {
    uint32 format_version = 1;

    SigningKey current_key = 2;
    SigningKey next_key = 3;

    string current_epoch = 4;

    EpochTransition pending_epoch_transition = 5;
    ConfigKeyTransition pending_key_transition = 6;

    uint64 observed_revision = 7;
    bytes snapshot_sha256 = 8;

    bytes last_key_transition_hash = 9;
    bytes last_epoch_transition_hash = 10;
}

message SigningKey {
    string key_id = 1;
    bytes public_key = 2;
}
```

`format_version` 在 V0.1 固定为 `1`。读取到未知格式版本、非法 Key 长度、Key ID 与 Public Key 不匹配、Epoch 为空但已存在 Snapshot，或 TrustState 与 Snapshot 无法恢复到同一提交时，Agent 必须快速失败并给出本地恢复错误，禁止重新 TOFU 或覆盖 Pin。

Agent 首次认证成功后获得：

```text
config signing public key
```

并 Pin 到本地。

后续 Snapshot 必须验证。

Key Transition 和 Epoch Transition 的 Protobuf 必须包含旧 Key ID、新 Key/Epoch、有效起始 Revision 和旧 Key 签名；Agent 只接受由当前 Pin Key 验证通过且目标值与当前值不同的 Transition。重复 Transition 必须幂等。

```protobuf
message ConfigKeyTransition {
    string current_key_id = 1;
    string next_key_id = 2;
    bytes next_public_key = 3;
    uint64 valid_from_revision = 4;
    bytes signature_by_current = 5;
    string transition_id = 6;
}

message EpochTransition {
    string current_epoch = 1;
    string next_epoch = 2;
    uint64 created_at_ms = 3;
    bytes signature_by_current = 4;
    string current_key_id = 5;
    uint64 valid_from_revision = 6;
    string transition_id = 7;
}

message ConfigAck {
    uint64 observed_revision = 1;
    string observed_signing_key_id = 2;
    string observed_server_epoch = 3;
    ConfigApplyStatus apply_status = 4;
    ErrorCode error_code = 5;
    string transition_id = 6;
    bytes transition_artifact_sha256 = 7;
    string observed_next_signing_key_id = 8;
    string observed_next_server_epoch = 9;
}

enum ConfigApplyStatus {
    CONFIG_APPLY_STATUS_UNSPECIFIED = 0;
    CONFIG_APPLIED = 1;
    CONFIG_REJECTED = 2;
}
```

Transition 的签名输入固定为：

```text
"xtunnel-config-key-transition-v1"
|| deterministic_protobuf(ConfigKeyTransitionWithoutSignature)

"xtunnel-epoch-transition-v1"
|| deterministic_protobuf(EpochTransitionWithoutSignature)
```

计算时清空 `signature_by_current`，并沿用 Snapshot 的 deterministic protobuf 与未知字段规则。`transition_id` 是 Server 生成并持久化的不变 ID，纳入被签名内容。

Transition Artifact Hash 的字节定义固定为：

```text
SHA-256(
  deterministic_protobuf(
    完整 ConfigKeyTransition 或 EpochTransition
    （包含 transition_id 与 signature_by_current）
  )
)
```

该 Hash 不包含外层 `ControlEnvelope`、UVarint Length 或 TLS Record 字节。Config Key 和 Epoch Transition 的 Golden Vector 必须同时固定 deterministic Artifact Bytes 与该 SHA-256。

ConfigAck 语义固定为：

- 普通 Snapshot Ack：`transition_id` 与两个 `observed_next_*` 字段必须为空；`observed_signing_key_id/observed_server_epoch` 表示已持久化的 Current 值。
- Key Transition Ack：必须同时携带已持久化的 `transition_id`、按上述定义计算的 Artifact SHA-256 和 `observed_next_signing_key_id`；`observed_next_server_epoch` 必须为空。
- Epoch Transition Ack：必须同时携带已持久化的 `transition_id`、按上述定义计算的 Artifact SHA-256 和 `observed_next_server_epoch`；`observed_next_signing_key_id` 必须为空。

Transition 成功 Ack 的其余字段也是唯一的：`observed_revision`、`observed_signing_key_id` 和 `observed_server_epoch` 必须等于 durable TrustState 中的 Current 值，`apply_status=CONFIG_APPLIED`，`error_code=ERROR_CODE_OK`。Server 只在 Ack 的 Current 字段、Transition ID、Artifact Hash 和目标 Next 值全部与 Journal/Installation 状态一致时才标记该 Installation `ACKED`。字段缺失、类型混用或值不一致均不得推进过渡状态。Transition 签名、ID 或语义校验失败时发送 `ControlEnvelope.Error{PROTOCOL_ERROR}` 并关闭，不发 ConfigAck；本地 Persist/fsync/rename 失败时直接关闭 Session 且不 Ack，重连后由 Server 重发同一 Artifact。

首次 Pin 只允许发生在：

```text
Agent 本地不存在 trust-state.pb
+
Agent Gateway TLS 已按 public CA 或 server SPKI pin 验证成功
+
Agent Token Authentication 成功
```

已存在 Pin 时，AuthResponse 中不同的 Key 不得直接覆盖本地文件；Agent 已记录 `server_epoch` 时，AuthResponse 中不同的 Epoch 也只能触发 Transition 协商，不能直接覆盖。首次安装可以在 TLS 与 Token Authentication 都成功后同时 Pin Key 和初始 Epoch。

签名 Key 轮换使用双 Key 过渡：

```text
Current Key 签名 Next Public Key + Next Key ID
 ↓
Agent 验证并保存 Next Key
 ↓
过渡期 Snapshot 使用 Current + Next 双签名
 ↓
确认所有属于非 REVOKED Agent、且未 ACK_EXCLUDED 的已知 Installation 已观察 Next Key
 ↓
切换 Next 为 Current
```

“在线”不能作为结束过渡期的条件。Server 必须保留旧 Key 可验证的 Transition Artifact、旧/新双签 Snapshot 能力和逐 Installation Ack 集合，直到每个属于非 REVOKED Agent、且未被排除的已知 Installation 已 Ack。长期离线 Installation 只能由管理员在给出原因后显式标记为 `ACK_EXCLUDED`，使其不再阻塞 Current 切换。

`ACK_EXCLUDED` 只是管理员从本次 Transition 等待集合中排除历史 Installation 的审计决定，不是安全隔离或强制重新注册。共享 Agent Token 的持有者仍可提交新 Installation ID；V0.1 不宣称能靠该状态阻止它。被排除的旧 Installation 以后重连时，Server 仍必须补发由其旧 Pin 可验证的完整 Transition Chain，使其升级后再接收当前 Snapshot。

Transition 与逐 Installation 状态必须持久化：

```sql
CREATE TABLE config_transitions (
    id TEXT PRIMARY KEY,
    transition_type TEXT NOT NULL,
    artifact BLOB NOT NULL,
    state TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    completed_at INTEGER
);

CREATE TABLE installation_transition_acks (
    transition_id TEXT NOT NULL,
    installation_id TEXT NOT NULL,
    status TEXT NOT NULL, -- PENDING | ACKED | ACK_EXCLUDED
    observed_at INTEGER,
    excluded_at INTEGER,
    excluded_by TEXT,
    exclusion_reason TEXT,
    PRIMARY KEY(transition_id, installation_id)
);
```

`ACK_EXCLUDED` 必须记录管理员、时间和非空原因。Ack、排除决定和“允许 Current 切换”的持久化状态必须在同一 SQLite 事务中提交；Key/Epoch 文件切换继续遵循下述 Journal + fsync Barrier，并在崩溃恢复时幂等完成。即使 Transition 已完成，其签名 Artifact 和必要的 Chain 也不得因排除而删除；只有相关 Installation 被正式删除且保留期结束后，才能按审计策略归档。

Config Key 与 Epoch 过渡都写入持久化 Transition Journal。顺序固定为：生成 Transition → 使用旧 Key 签名 → 连同旧值、新值、起始 Revision、Ack 集合原子持久化 → fsync → 才切换 Current 并开始发送。进程崩溃后必须从 Journal 恢复同一个过渡，不得重新随机生成或覆盖旧 Artifact。

Agent 收到 Key/Epoch Transition 后，必须先用 Current Key 验证签名并检查 ID、Revision 和 Epoch，再计算完整 Artifact 的 SHA-256。完全相同 Hash 的重复 Transition 幂等，可重新 Ack；同一 `next_key_id` 或目标 Epoch 对应不同 Artifact/Key 内容时，以 `PROTOCOL_ERROR` 拒绝且不得覆盖 TrustState。

Transition 落盘顺序固定为：

```text
Verify Transition
 ↓
Build New AgentTrustState
 ↓
write trust-state.pb.tmp
 ↓
fsync(file)
 ↓
atomic rename → trust-state.pb
 ↓
fsync(identity directory)
 ↓
Atomic Swap In-memory TrustState
 ↓
ConfigAck（携带 Transition ID + Artifact Hash + observed Next Key/Epoch）
```

Ack 只能表示对应 TrustState 已经 durable，不能在临时文件写入后提前发送。该 Ack 必须使用上述显式 Transition 字段，不得用 Current 字段暗示 Next 已落盘。

Gateway TLS Key、Config Signing Key、`server_epoch` 和 SQLite 必须纳入同一套一致性备份。丢失旧 Signing Key 时不得自动生成新 Key 后继续推送配置。

---

# 69. Revision Rollback

Agent 使用 `(server_epoch, revision)` 判断回退。

如果：

```text
incoming revision
<
local revision
```

默认：

```text
Reject
```

防止旧配置重放。

正常运行时不允许降低同一 `server_epoch` 下的 Revision。

从旧 SQLite 备份恢复时，管理员必须显式执行：

```text
xtunnel-server recovery new-epoch
```

Server 生成新的随机 `server_epoch`，并使用当前已 Pin 的 Config Signing Key 签署 Epoch Transition。Agent 只有在 Transition 能由当前信任 Key 验证时才接受新 Epoch，然后从新 Revision 重新同步。

AuthRequest 和 Heartbeat 必须携带 Agent 当前观察到的 Epoch 与 Signing Key ID。Server 根据 Transition Journal 决定补发 Snapshot、ConfigKeyTransition 或 EpochTransition，禁止只根据 revision 数值猜测。Epoch 过渡同样保留旧/新值和逐 Installation Ack，直到所有属于非 REVOKED Agent 的已知 Installation 完成，或由管理员留下审计理由后将特定历史 Installation 标记为 `ACK_EXCLUDED`；排除不删除它未来追赶所需的签名 Chain。

V0.1 不提供绕过签名或自动清空 Agent 本地 Snapshot 的回退方式。

---

# 70. Config Apply

Agent：

```text
Receive Snapshot
 ↓
Verify Signature
 ↓
Validate Config
 ↓
Build New Resolver
 ↓
write + fsync config/snapshot.next
 ↓
fsync config directory
 ↓
write + fsync identity/trust-state.pb.tmp
 ↓
atomic rename trust-state.pb.tmp → trust-state.pb
 ↓
fsync identity directory（持久化提交点）
 ↓
atomic rename snapshot.next → snapshot.pb
 ↓
fsync config directory
 ↓
Atomic Swap Runtime Resolver
 ↓
ConfigAck
```

新 TrustState 的 `observed_revision`、`snapshot_sha256`、Current/Next Key 和 Epoch 必须与 Candidate Snapshot 一致。`trust-state.pb` 的 atomic rename 是逻辑切换点，随后 `fsync(identity directory)` 成功返回才是 durable commit point。二者之间崩溃时，文件系统允许恢复到旧或新目录项；重启必须根据 TrustState 中的 `observed_revision/snapshot_sha256`、`snapshot.pb` 和 `snapshot.next` 的 Hash 按第 71 节规则幂等收敛，不得仅根据 rename 是否曾返回猜测。任何 Persist、Hash、rename 或目录 fsync 失败时不得切换运行态或发送 ConfigAck。Durable Commit 后的 Resolver Swap 必须是不可失败操作。

Apply 必须：

```text
idempotent
```

---

# 71. Last Known Config

Agent：

```text
<data-dir>/config/snapshot.pb

<data-dir>/config/snapshot.next

<data-dir>/identity/trust-state.pb
```

重启恢复规则：

```text
TrustState.snapshot_sha256 == snapshot.pb SHA-256
→ 删除无关 snapshot.next，正常加载

TrustState.snapshot_sha256 == snapshot.next SHA-256
→ 完成 snapshot.next → snapshot.pb promote + fsync

TrustState 仍指向旧 snapshot.pb，存在不同 Hash 的 snapshot.next
→ 提交点前遗留，删除 snapshot.next

TrustState 与 snapshot.pb / snapshot.next 均不匹配
→ 快速失败，禁止猜测恢复或重新 Pin
```

写入 `snapshot.next` 后必须先 fsync 文件和 `config` 目录，确保 TrustState 提交后至少存在一份可恢复的新 Snapshot。恢复完成前不得连接 Gateway 或发送 observed Revision。加载 Last Known Config 时同样执行 Snapshot Signature、Unknown Field、Hash、Revision、Key 和 Epoch 全量校验，不能因为文件来自本地磁盘就跳过。

Server 暂时不可连接：

```text
Agent 可以加载 Last Known Config
```

但由于 Server 本身不可访问，此时主要用于：

```text
快速恢复

避免配置丢失
```

---

# 72. Origin Health

Health Check 在每个 Agent Instance 本地执行。

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

这些字段属于 Tunnel Binding，必须同时进入：

```text
SQLite tunnel_bindings

TunnelBindingConfig Protobuf

Service API

Web Console
```

HTTP 默认期望状态范围为 `200-399`。任何自定义范围、成功阈值和失败阈值都必须持久化并随 Snapshot 下发，禁止只保存在 UI 状态中。

每个 Agent Instance 只能运行一个中心化 Health Scheduler，禁止为每个 Binding 启动独立永久 Ticker/Goroutine。调度器固定包含：

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

Agent 默认契约：

```yaml
health:
  max_concurrent: 64
  max_checks_per_second: 50
  max_concurrent_per_origin: 4
  initial_jitter: 1.0
  interval_jitter: 0.2
  report_flush_interval: 1s
  report_batch_size: 128
```

首次检查在 `[0, interval]` 均匀随机分散；后续检查间隔为 `interval × random(0.8, 1.2)`。Rate/Concurrency 已满时，Scheduler 只能在仍可满足该 Binding 的配置 Interval 和 Stale TTL 时短暂排队；无法满足时必须报告 `HEALTH_BUDGET_EXCEEDED`，不能静默把 10 秒检查拖成更长周期后继续显示正常。

V0.1 产品容量约束：

```text
health-enabled bindings × online replicas
<= 2000 / logical agent

global scheduled health targets
<= 50000 / Server
```

Management 写入必须按事务内 Candidate Binding 与当前在线 Replica 数预检；新 Instance 认证导致预算超限时，Server 返回可重试的 `HEALTH_BUDGET_EXCEEDED` Auth Failure 和 `retry_after_ms`，不建立半可用 Session。相关数值属于第 156 节唯一 Limits 契约，可经 Benchmark 调整，但不得移除这一预算维度。

Management Candidate 和 Control Auth 必须通过同一个内存 `HealthTargetBudgetManager` 执行原子 `Reserve → Commit/Release`，禁止先分别读取计数再决定。配置写入先按 Candidate Delta 预留，再执行短 SQLite 事务；事务失败释放 Reservation，提交成功后 Commit 并触发 Reconcile。

Runtime Health Budget 的唯一所有权 Key 是 `(agent_id, instance_id)`，`session_generation` 只用于 fencing，不作为额外计费对象。首个 Session 在发布到 Runtime Registry 前，按该 Agent 当前 health-enabled Binding 数预留并 Commit；同一 Instance 重连时，必须按 `AgentRuntime.mu → HealthTargetBudgetManager.mu` 的唯一锁顺序将已有 Reservation 原子转移给新 generation，不得重复计费。`HealthTargetBudgetManager.mu` 持有期间禁止反向获取任何 `AgentRuntime.mu`；需要跨多个 Agent 重算时，必须先在各 Agent Lock 内生成不可变 Delta，释放 Agent Lock 后再单独进入 Budget Lock，不允许同时持有多个 Agent Lock。旧 generation cleanup 因 CAS 失败时不得释放新 generation 持有的 Reservation；只有 Instance Runtime 最终删除或 Tombstone 结束时才按所有权 Key 释放。首次预留失败则发送 Auth Failure，不发布半可用 Session。Budget Lock 内禁止 SQLite、网络 IO 或等待 Channel。Server Restart 从 SQLite Desired State 和重建中的唯一 Instance Runtime 重新计算，任何计数不变量破坏都阻止对应 Session/Config 发布，而不是产生负数或超额 Target。

---

# 73. Health 状态

```text
UNKNOWN

HEALTHY

UNHEALTHY
```

启用 Health Check 时：

```text
UNKNOWN 不参与 Instance Selection

至少一次成功后才进入 HEALTHY

Server 本地 received_at 超过 2 × interval
且未收到同 binding_revision 的新 Health Report
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

# 76. Tunnel Health Report

```protobuf
message TunnelHealth {
    string tunnel_id = 1;

    HealthStatus status = 2;

    uint32 latency_ms = 3;

    string error_code = 4;

    uint64 checked_at_ms = 5;

    uint64 binding_revision = 6;
}

message TunnelHealthBatch {
    uint64 generation = 1;
    repeated TunnelHealth items = 2;
}
```

Health 是：

```text
Per Instance
Per Tunnel
```

而不是只有 Agent 级状态。

Health Report 必须绑定产生它的 `binding_revision`，值取对应 `TunnelBindingConfig.required_revision`。Server 只接受它与 SQLite/Runtime 中该 Tunnel 当前 `required_revision` 完全相等的报告；旧 Revision 或未知 Revision 全部丢弃，不能覆盖新状态。`checked_at_ms` 只用于 UI/日志展示，不参与新旧裁决；Health 新鲜度使用 Server 本地 monotonic `received_at` 与配置的 Stale TTL 判断，禁止比较 Agent 与 Server 的 wall clock。Agent-level Revision 因其他 Tunnel 变化而递增时，未变化 Tunnel 的 Health Checker 不必重启。

Agent 应用新 Snapshot 时，必须先取消受影响 Tunnel 的旧 Health Checker，并把对应状态原子重置为 `UNKNOWN`，再启动带新 Revision 的检查。启用 Health Check 的 Tunnel 只有在新 Revision 首次检查成功后才可 Eligible；旧 Origin 的 HEALTHY 不能沿用到新 Origin。

Agent 不逐条发送 Health Frame。Report Batcher 在 `report_flush_interval` 到达、累计达到 `report_batch_size`，或进入 Drain 前生成 `TunnelHealthBatch`；同一批次内每个 Tunnel 只保留最新结果。`generation` 在当前 Control Session 内严格递增，Server 丢弃重复或倒退 Batch，但仍以每个 Item 的 `binding_revision` 作为配置新旧的最终裁决。Batch 序列化后仍必须小于 MaxControlFrameSize；超限时按不超过 `report_batch_size` 的子批次拆分，并为每个 Frame 使用新 generation。

Server 在 Binding `required_revision` 变化时也必须立即把所有 Instance 上该 Tunnel 的 Runtime Health 重置为 `UNKNOWN`。Instance Selection 除检查状态为 HEALTHY 外，还必须检查已存 Health 的 `binding_revision == Tunnel required_revision`，因此 ConfigAck 先于新 Health Report 到达也不会短暂放行旧 HEALTHY。

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
Tunnel
  ↓
Instance
  ↓
WorkConn
  ↓
Agent
  ↓
Origin
```

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
Host → Tunnel
```

---

# 81. HTTP Route

```sql
CREATE TABLE http_routes (
    id TEXT PRIMARY KEY,

    tunnel_id TEXT NOT NULL,

    hostname TEXT NOT NULL,

    path_prefix TEXT NOT NULL DEFAULT '/',

    preserve_host INTEGER NOT NULL DEFAULT 1,

    enabled INTEGER NOT NULL DEFAULT 1,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,

    FOREIGN KEY(tunnel_id)
        REFERENCES tunnels(id)
        ON DELETE CASCADE,

    UNIQUE(hostname, path_prefix)
);
```

HTTPS 开关不需要放进 Route。

TLS 已经属于前置代理。

---

# 82. HTTP Route Matching

匹配：

```text
Exact Host
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

先移除 dot segment，再执行匹配

Prefix 必须按路径段边界匹配
```

Canonical Path Prefix 规则：根路径只能存为 `/`；非根 Prefix 移除所有尾部 `/` 后保存。因此 `/admin` 与 `/admin/` 在写入边界规范化为同一个 `/admin`，数据库唯一约束禁止重复语义 Route。请求 `/admin`、`/admin/` 和 `/admin/...` 都匹配该 Prefix，但 `/administrator` 不匹配。

例如 `/admin` 只匹配 `/admin` 和 `/admin/...`，不得匹配 `/administrator`。

对编码斜杠 `%2F`、重复斜杠和保留字符不做隐式等价折叠；Router 与转发给 Origin 的路径必须来自同一个规范化结果。无法保证 Router 与 Origin 解释一致的请求应返回 `400 INVALID_PATH`。

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

输入格式、Canonical Host、端口和增量冲突等可预判错误必须在提交前返回。完整 Snapshot 构建若因数据库内部不一致失败，Desired State 保留并进入 `APPLY_FAILED`，Reconcile 不得发布部分结果；修复后按最新 generation 重试。最终 Atomic Swap 本身必须是不可失败操作。

TCP `Listen` 等无法与 SQLite 原子提交的外部副作用使用 Desired/Actual 状态：

```text
desired = ENABLED
actual = APPLYING | ACTIVE | APPLY_FAILED
```

API 返回 Desired State 写入结果；Service Status 明确展示 Runtime Apply Error，不得返回“失败”但暗中保留已提交配置。

老请求继续使用：

```text
旧 Snapshot
```

新请求：

```text
立即使用新 Snapshot
```

若该 Route 依赖尚未被任何 Instance 观察到的新 Agent Revision，则 Route 可以进入 Snapshot，但请求必须返回 `CONFIG_NOT_OBSERVED`，禁止回落到旧 Revision Instance。

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

HTTP Connection Pool 必须按 Tunnel 隔离。ReverseProxy 将 Request URL 的内部目标设置为：

```text
http://<tunnel_id>.xtunnel.invalid
```

该内部 Host 只用于 `http.Transport` 的连接池 Key 和 DialContext 解析，不发送给 Origin。不得只把 Tunnel ID 放进 `context.Context`，因为 KeepAlive 复用连接时不会再次调用 DialContext。

请求的实际 `Host` Header 按 `preserve_host` 规则单独设置。任何情况下都禁止不同 Tunnel 共用同一个 Transport Pool Key。

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

只有最靠近 XTunnel 的受信代理提供的协议和 Host 元数据可以作为：

```text
X-Forwarded-Proto

X-Forwarded-Host
```

客户端位于最外层的不可信 X-Forwarded-For 值不得直接成为 original client。

否则：

```text
删除所有外部 Forwarded Header
 ↓
使用 Peer IP 重新生成
```

多 Header、多值、空值、非法 IP、超长代理链一律返回 `400 INVALID_FORWARDED_HEADER`。代理链最多保留 32 跳。

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

`preserve_host=true`：

```text
Host
```

保持公网请求 Host。

`preserve_host=false`：

```text
Host = TunnelBinding.origin_http_host
```

HTTP/HTTPS Origin 在 `preserve_host=false` 时必须设置 `origin_http_host`；默认建议使用 Origin 的规范化 `host:port`。TCP Origin 禁止设置该字段。

该值由 Server 组装 HTTP 请求 Header 使用，但不出现在 OpenRequest。Agent 仍只按 Tunnel ID 解析和连接 Origin。

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

`OpenRequest.client_addr` 只用于 Agent 日志、Tracing 和审计。V0.1 不向 TCP Origin 注入 PROXY Protocol，Origin 看到的对端地址是 Agent；需要真实客户端 IP 做访问控制的 Origin 不属于 V0.1 透明转发能力范围。未来如支持，只能在每个 Binding 显式 opt-in，并且 Origin 必须明确声明接受 PROXY v1/v2，禁止默认注入。

---

# 94. TCP Route Schema

```sql
CREATE TABLE tcp_routes (
    id TEXT PRIMARY KEY,

    tunnel_id TEXT NOT NULL,

    public_port INTEGER NOT NULL UNIQUE,

    enabled INTEGER NOT NULL DEFAULT 1,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,

    FOREIGN KEY(tunnel_id)
        REFERENCES tunnels(id)
        ON DELETE CASCADE
);
```

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

单个 `Listen(port)` 在启动或 Reconcile 中失败时，只把对应 TCP Service 标记为 `APPLY_FAILED`，记录稳定 Error Code、端口和最近失败时间；Management、Agent Gateway、HTTP Ingress 及其他成功 Listener 继续启动。Periodic Reconcile 重试该 Desired Route。只有全局 Listener 配置无效、必需基础端口失败或数据库/身份初始化失败，才阻止 Server READY。

---

# 96. TCP Port Range

默认：

```yaml
tcp_ingress:
  bind: "0.0.0.0"

  min_port: 10000
  max_port: 60000
```

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

Agent Offline：

```text
503
AGENT_OFFLINE
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
TUNNEL_CAPACITY
```

Config Not Observed：

```text
503
CONFIG_NOT_OBSERVED
```

Tunnel Disabled：

```text
503
TUNNEL_DISABLED
```

Route Not Found：

```text
404
```

---

# 98. TCP 错误语义

TCP Route 遇到：

```text
Agent Offline

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

0x1001 TUNNEL_NOT_FOUND
0x1002 TUNNEL_DISABLED
0x1003 AGENT_OFFLINE
0x1004 NO_HEALTHY_INSTANCE
0x1005 CONFIG_NOT_OBSERVED

0x2001 ORIGIN_REFUSED
0x2002 ORIGIN_TIMEOUT
0x2003 ORIGIN_UNREACHABLE
0x2004 ORIGIN_RESET
0x2005 ORIGIN_TLS_ERROR

0x3001 WORK_POOL_EXHAUSTED
0x3002 AGENT_BUSY
0x3003 OPEN_DRAINING
0x3004 HEALTH_BUDGET_EXCEEDED

0x4001 TOKEN_INVALID
0x4002 TOKEN_REVOKED
0x4003 AGENT_REVOKED
0x4004 SESSION_INVALID
0x4005 SESSION_RESOURCE_EXHAUSTED
0x4006 INSTALLATION_ID_CONFLICT

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
Tunnel
 ↓
Instance
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

用户 Web Console 不需要理解：

```text
Tunnel

Binding

HTTPRoute

TCPRoute
```

产品层统一叫：

```text
Service
```

创建 Service 实际事务：

```text
Tunnel
+
TunnelBinding
+
Route
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

  "agent_id": "ag_01ARZ3NDEKTSV4RRFFQ69G5FAV",

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
    "type": "http",
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

POST/PATCH Service DTO 与 `TunnelBindingConfig` 使用同名 Health 字段。边界校验固定为：

```text
1000 <= interval_ms <= 3600000

100 <= timeout_ms < interval_ms

1 <= failure_threshold <= 20

1 <= success_threshold <= 20

100 <= expected_status_min <= expected_status_max <= 599
```

未提供 Health 时显式落库为 Disabled；提供部分 Health 字段时由 Application Service 补全默认值后再持久化，API GET 必须返回完整有效值，保证往返一致。

Server Transaction：

```text
BEGIN IMMEDIATE

next_revision = agent.desired_revision + 1

INSERT tunnel

INSERT tunnel_binding(required_revision = next_revision)

INSERT http_route

SET agent.desired_revision = next_revision

COMMIT
```

Revision 规则：

```text
修改 Origin / Health / Binding Enabled
→ 在同一事务递增对应 Agent Revision，并更新 required_revision

切换 Binding Agent
→ 同时递增旧 Agent 和新 Agent Revision

只修改 HTTP Host / Path 或 TCP Public Port
→ 不修改 Agent Revision，只重建 Server Route Snapshot

删除 Service
→ 递增原 Agent Revision，使 Agent 删除本地 Binding
```

PATCH、enable、disable、delete 必须复用同一 Application Service 事务规则，禁止各 Handler 自行决定是否递增 Revision。

这些 Service Mutation 还必须在同一事务校验 `tunnels.version == If-Match`，并只把 Aggregate Version 递增一次；Agent `desired_revision` 是否递增仍严格按上述字段变化规则决定。ETag Version 用于管理员并发写保护，Agent Revision 用于配置分发，两者语义独立。

---

# 104. SQLite

数据库：

```text
<server.data_dir>/xtunnel.db
```

V0.1 不提供独立 `database.path`。SQLite、PKI、Epoch、Transition Journal 必须全部位于同一个 Canonical `server.data_dir` 管理边界内，避免两个不同 Data Directory 指向同一外部数据库而绕过互斥锁，也保证 Backup/Restore 不会组合出不同代的 DB 与身份文件。

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

在线 `backup create` 通过本机控制通道暂停新的 Config Write，等待当前短事务结束，再使用 SQLite Backup API 获取一致数据库镜像；在同一 Barrier 内复制 Gateway TLS Key、Config Signing Key、`server_epoch` 和 Transition Journal。备份 Manifest 必须记录格式版本、数据库 Schema 版本、文件清单和 SHA-256。若 Server 未运行，命令必须先获取与 Server 相同的外部锁。

备份包等同于长期私钥材料。输出文件必须使用 `O_CREATE | O_EXCL`、权限 `0600` 且禁止跟随符号链接；临时目录权限 `0700`，失败必须清理，禁止输出到 stdout 或复用已有目标文件。

`backup restore` 只允许 Server 停止后执行，必须先由 `realpath(parent) + basename` 计算 Stable Data Target、获取同一外部锁，再校验 Manifest/Hash/Schema 兼容性。恢复内容写到同盘 sibling staging 目录，然后按“旧目录 rename 为 rollback → staging rename 为正式目录 → fsync 父目录”的顺序切换。切换前在父目录写入权限 `0600` 的 `.xtunnel-restore-<hash>.journal`，其中记录 Stable Target、staging、rollback、当前 phase 和 Manifest Hash；三条路径都必须校验为同一 `realpath(parent)` 的直接子项。Journal 位于替换边界外且跨重启保留，崩溃后下次 Server/Restore 命令先按 Stable Target 取得同一把锁，再完成或回滚，不能要求 leaf 预先存在。外部锁在整个流程中不变，因此替换 Data Directory 不会替换当前持有的 Lock inode。禁止与现有数据库合并，也禁止只复制 `xtunnel.db` 而遗漏 WAL 或 PKI。集成测试必须覆盖“备份 → Migration → 恢复 → Agent 通过原 Pin 重连”以及两个 rename 之间崩溃后的回滚。

---

# 105. Repository Layer

业务代码禁止直接：

```go
db.Query(...)
gormDB.Where(...)
```

V0.1 的 SQLite Repository 统一使用 GORM。业务持久化不得绕过 Repository
直接操作 `*gorm.DB`；仅连接初始化、逐连接 PRAGMA 自检和 SQLite Backup API
等 GORM 不提供等价能力的基础设施路径可以访问底层 `database/sql`。

Repository：

```go
type AgentRepository interface {
    Create(...)
    Get(...)
    List(...)
    Update(...)
}

type AgentTokenRepository interface {
    ...
}

type TunnelRepository interface {
    ...
}

type RouteRepository interface {
    ...
}

type UsageRepository interface {
    ...
}

type Store interface {
    WithTx(ctx context.Context, fn func(TxStore) error) error
}

type TxStore interface {
    Agents() AgentRepository
    AgentTokens() AgentTokenRepository
    Tunnels() TunnelRepository
    Routes() RouteRepository
    Usage() UsageRepository
}
```

跨表不变量只能由 Application Service 在一次 `Store.WithTx` 中完成。传入的
`TxStore` 中所有 Repository 必须共享同一个事务作用域内的 `*gorm.DB`，
Repository 自身不得再开启或提交事务；Service 创建、切换 Binding、删除
Service、Token 轮换等多表操作都遵循这一规则。

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
    updated_at INTEGER NOT NULL
);
```

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

`/run/xtunnel` 权限为 `0700` 并归 XTunnel Runtime UID 所有。systemd 通过 `RuntimeDirectory=xtunnel` 创建；OCI Image 预创建同一目录并以固定非 root UID 运行。离线维护命令由 root 创建或访问该目录。禁止要求非 root Server 直接写 `/run/lock`。

`server.data_dir` 必须是绝对路径。Stable Data Target 的计算只依赖 `realpath(parent_dir) + basename(data_dir)`：父目录必须已存在且不是符号链接，leaf 名称必须合法；leaf 可以在 Restore 的中间崩溃状态下暂时不存在。Lock 使用非阻塞 OS 独占锁、禁止跟随符号链接、权限 `0600`，由进程全生命周期持有，残留文件本身不代表已加锁。离线 Admin、Backup、Restore 和 Recovery 命令必须用完全相同的 Stable Target/Hash 算法复用同一把锁。

获取锁后，进程必须先检查父目录中的 Restore Journal 并完成或回滚，再要求正式 Data Directory 存在、拒绝 leaf 符号链接，并验证其 `realpath` 等于 Stable Data Target。正常启动不能为了通过校验而创建一个空 leaf 目录；全新部署的目录由安装流程预先创建。

密码修改或管理员禁用时，必须删除该用户的所有 Admin Session。

---

# 107. Admin Session

```sql
CREATE TABLE admin_sessions (
    id TEXT PRIMARY KEY,

    user_id TEXT NOT NULL,

    token_hash BLOB NOT NULL UNIQUE,

    expires_at INTEGER NOT NULL,

    created_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,

    FOREIGN KEY(user_id)
        REFERENCES admin_users(id)
        ON DELETE CASCADE
);
```

Session Token 使用 32 byte `crypto/rand`，数据库保存 `SHA-256(token)`。

默认策略：

```text
absolute_ttl = 12h

idle_ttl = 30min

logout = 删除数据库 Session
```

---

# 108. Agent Table

```sql
CREATE TABLE agents (
    id TEXT PRIMARY KEY,

    name TEXT NOT NULL,

    version INTEGER NOT NULL DEFAULT 1,

    desired_revision INTEGER NOT NULL DEFAULT 0,

    observed_revision INTEGER,

    last_seen_at INTEGER,

    revoked_at INTEGER,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
```

ONLINE/OFFLINE：

```text
不写 status 字段
```

实时计算。

`observed_revision` 只保存最近一次在线 Instance Revision 最小值的 UI 摘要。它在 ConfigAck、Instance 上下线时更新，不作为数据面选择依据；Server 重启后没有在线 Instance 时允许为 `NULL`。

---

# 109. Usage Table

只保存聚合数据：

```sql
CREATE TABLE usage_minutes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,

    bucket_time INTEGER NOT NULL,

    tunnel_id TEXT NOT NULL,

    connections INTEGER NOT NULL DEFAULT 0,

    ingress_bytes INTEGER NOT NULL DEFAULT 0,

    egress_bytes INTEGER NOT NULL DEFAULT 0,

    errors INTEGER NOT NULL DEFAULT 0,

    UNIQUE(bucket_time, tunnel_id)
);
```

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

Server：

```go
type UsageCounter struct {
    Connections atomic.Uint64

    IngressBytes atomic.Uint64
    EgressBytes  atomic.Uint64

    Errors atomic.Uint64
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

Flusher 对每个 Counter 使用 `Swap(0)` 取得本批增量，并通过：

```sql
INSERT ... ON CONFLICT(bucket_time, tunnel_id)
DO UPDATE SET value = value + excluded.value;
```

写入失败时把尚未持久化的增量合并回内存 Counter。V0.1 Usage 属于 best-effort 统计：进程 `kill -9` 最多允许丢失当前 60 秒内存桶，但不得重复累计已经确认提交的批次。该误差必须在 Dashboard 和文档中说明。

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
HttpOnly

Secure

SameSite=Lax

Host-only（禁止设置 Domain）

Path=/api/v1
```

Management 只能通过 HTTPS 前置代理或本机 loopback 访问。客户端 IP、Scheme 和 Host 使用独立于 Tunnel Ingress 的 `management.trusted_proxies` 规则解析。

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

CSRF Token 为绑定 Admin Session 的独立随机值，通过响应 Body 获取，并使用自定义 Header：

```text
X-XTunnel-CSRF
```

Server 必须同时校验 Token、`Origin` 和目标 Management Host。CSRF Token 不写 Cookie、不进入 URL、不记录日志。

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

另设 Server 全局失败预算，避免攻击者通过大量用户名绕过限制。

连续失败增加：

```text
cooldown
```

只统计失败登录；成功登录不会清除同 IP 的全局攻击计数。所有限流 Key 必须使用 Management Trusted Proxy 规则得到的客户端 IP，禁止直接使用 loopback Peer 或未经验证的 X-Forwarded-For。

---

# 116. Agent Gateway Auth Limit

即使 Token 高熵，也需要限制握手资源消耗。

只统计失败认证，并使用分层且有界的带 Burst 令牌桶：

```text
Normalized Peer IP-only Bucket

Normalized Peer IP + token_hash_prefix Bucket

Server Global Failed-auth Bucket
```

IP-only Bucket 防止攻击者不断更换随机 Token 绕过 `IP + prefix`；组合 Bucket 限制对单个 Credential 前缀的集中尝试；Global Bucket 限制分布式攻击。所有失败同时记入适用层级，成功认证不消耗失败预算，也不能清空 IP 或全局失败计数。

同 NAT 下多个合法 Agent 共享 IP 时，成功认证不消耗失败预算。另设全局 Pending TLS/Auth 上限和单 IP 握手并发上限，具体 Rate/Burst 必须通过 5000 Instance 重连风暴测试确定，不能把示例值作为不可调整的硬编码值。桶表使用有最大容量和过期时间的 LRU，避免随机 IP/Token 造成内存 DoS。

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

---

# 118. 前端目录

```text
web/
├── src/
│   ├── components/
│   │   ├── ui/
│   │   ├── agent/
│   │   └── service/
│   │
│   ├── pages/
│   │   ├── login/
│   │   ├── dashboard/
│   │   ├── agents/
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

开发模式使用 `web/vite.config.ts`：

```text
Vite HTTPS Dev Server
  /api/v1/*
      ↓ same-origin proxy
Go Management 127.0.0.1:8080
```

本地开发证书只用于 Loopback，`/api/v1` 代理保留浏览器可见 Host/Origin，并由开发配置显式加入 Management Allowed Hosts。这样 Secure Cookie、Origin 和 CSRF 仍走生产同源模型；禁止为联调增加 `Access-Control-Allow-Origin: *`、关闭 Secure Cookie 或跳过 CSRF。`npm run dev` 与代理配置必须进入 M0 开发说明。

开发者通过 `XTUNNEL_DEV_TLS_CERT` 和 `XTUNNEL_DEV_TLS_KEY` 指向本机已信任的 Loopback Certificate；文件位于仓库外，或位于被 `web/.gitignore` 排除的 `.dev-certs/`，目录权限 `0700`、Key `0600`。缺失证书时 `npm run dev` 必须给出可操作错误并退出，禁止自动提交证书、私钥或静默降级 HTTP。M0 Smoke Test 必须通过 Vite Proxy 完成 Login、Secure Cookie、CSRF POST 和 Logout。

---

# 120. Web 页面

V0.1：

```text
Login

Dashboard

Agents

Agent Detail

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

Logical Agents

Online Instances

Offline Agents

Services Ready

Services Error

Active Connections

Connections Today

Ingress Traffic Today

Egress Traffic Today

Recent Errors
```

---

# 122. Agent List

字段：

```text
Name

Status

Instances Online

Version Summary

Services

Active Connections

Last Seen
```

操作：

```text
View

Create Service

Rotate Token

Revoke
```

---

# 123. Agent Detail

展示：

```text
Agent Name

Agent ID

Status

Token Version

Desired Revision

Services
```

Instances：

```text
Hostname

Installation ID

Instance ID

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

# 124. Create Agent

输入：

```text
Name
```

创建成功：

```text
Agent ID

Agent Token

Install Command
```

例如：

```bash
xtunnel-agent install \
  --server tunnel.example.com:7443 \
  --token-file '<secure-token-file>' \
  --server-pin 'sha256:xxxx'
```

Token：

```text
只展示一次
```

Token 与安装命令必须分开展示。Web Console 不得把明文 Token 拼接进可复制命令；用户先将 Token 写入权限为 `0600` 的临时 Secret 文件，再执行安装。

---

# 125. Rotate Token

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
旧实例保持在线

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

Logical Agent
```

从 Agent Detail 创建：

```text
Agent 固定
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

当 Public Access 的 `Preserve Host=false` 时，HTTP Host Header 为必填。

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

Agent

Healthy Instances

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

AGENT_OFFLINE

ORIGIN_UNHEALTHY

NO_CAPACITY

CONFIG_SYNCING

APPLY_FAILED

READY
```

如果多个 Replica：

```text
至少一个可用 Instance
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
AGENT_OFFLINE
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
case noConnectedInstance:
    return AGENT_OFFLINE
case noInstanceObservedRequiredRevision:
    return CONFIG_SYNCING
case healthEnabled && noHealthyRevisionEligibleInstance:
    return ORIGIN_UNHEALTHY
case noRuntimeCapacity:
    return NO_CAPACITY
default:
    return READY
}
```

`CONFIG_SYNCING` 必须先于 `ORIGIN_UNHEALTHY`，因为旧 Revision Health 明确不可用。该算法只能在 `internal/server/status` 实现一次；Dashboard、Service Detail 和 Web Console 必须展示 API 返回值，禁止在前端或不同 Handler 中重新计算。

`CONFIG_SYNCING` 表示 Desired State 已提交，但尚无满足 Tunnel RequiredRevision 的 Instance。`APPLY_FAILED` 表示 TCP Listener 等 Runtime 副作用无法达到 Desired State；详情必须包含稳定错误码和最近失败时间。

---

# 134. REST API

REST API 的唯一权威来源固定为：

```text
api/openapi/openapi.yaml
```

M5 Handler、TypeScript Client、Mock、DTO 校验和契约测试必须从该文件生成或由 CI 验证一致；本文只定义产品语义。M5 开始前必须冻结全部 Request/Response Schema、Required/Nullable、分页、错误响应、ETag 和 HTTP Status，禁止由 Handler 与 Web 分别维护 DTO。

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

`page_size` 默认 `50`、最大 `200`；无下一页时 `next_page_token` 为空或省略，具体表现必须在 OpenAPI 中固定。Token 对客户端完全 opaque，Server 不信任其中任何可解码内容，并至少校验 Resource Type、排序字段、最后一条记录和 Filter Hash；客户端不得解析或构造。非法、过期或与当前 Filter 不匹配的 Token 返回 `400 INVALID_PAGE_TOKEN`。

PATCH 统一要求 `Content-Type: application/merge-patch+json`，并使用 JSON Merge 语义的显式 DTO：

```text
omitted → 不修改
null    → 仅对 OpenAPI 标记 nullable 的字段执行清空
value   → 修改为该值
```

未知字段返回 `400 INVALID_REQUEST`。非 Nullable 字段传 `null` 返回 `422 VALIDATION_FAILED`。嵌套对象的 omitted/null/value 语义必须逐字段生成测试，禁止用 Go 零值猜测“未提供”。

Agent 和 Service 都是 Aggregate Root，`agents.version` 与 `tunnels.version` 是各自并发版本。单个 Resource 的 GET、POST Create 和 PATCH 成功响应返回强 ETag；List Response 不返回 Aggregate ETag：

```http
ETag: "7"
```

PATCH、DELETE、Rotate、Revoke、Enable 和 Disable 必须携带单个精确 `If-Match`，不接受 `*`。缺失返回 `428 PRECONDITION_REQUIRED`，语法错误或多值返回 `400 INVALID_IF_MATCH`。Application Service 在同一事务中执行：

```text
UPDATE aggregate
SET ..., version = version + 1
WHERE id = ? AND version = expected
```

DELETE 使用 `DELETE ... WHERE id = ? AND version = expected`；Action 使用等价的条件 UPDATE。任何路径受影响行数为零时，必须在同一事务内区分 `404 RESOURCE_NOT_FOUND` 与 `412 RESOURCE_VERSION_CONFLICT`，不能把两者都返回 404。

版本不匹配返回 `412 RESOURCE_VERSION_CONFLICT`，不得覆盖其他管理员已经提交的修改。除成功删除的 Resource 外，Action/Mutation 返回新 ETag；涉及 Tunnel/Binding/Route 的 Service 变更，只递增 `tunnels.version` 一次，同时继续遵循第 103 节 Agent Revision 规则。

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

返回 Agent Token、Rotate Token 或其他一次性 Secret 的响应必须包含：

```http
Cache-Control: no-store
Pragma: no-cache
```

不得被 Dashboard Recent Activity、Access Log Body 或前端持久化缓存记录。Settings 页面在 V0.1 只读展示 `/system/config` 的非敏感有效配置和“需重启”标记；不提供修改 Server/Agent 主配置的 API。

---

# 135. Agent API

```text
POST /agents

GET /agents

GET /agents/{id}

PATCH /agents/{id}

GET /agents/{id}/instances

POST /agents/{id}/token/rotate

POST /agents/{id}/revoke

DELETE /agents/{id}
```

`DELETE /agents/{id}` 只允许删除没有任何 Tunnel Binding 的 Agent。存在引用时返回：

```text
409 AGENT_IN_USE

binding_count

referencing_service_ids（有界分页）
```

删除不得隐式级联 Service、Route 或 Binding。需要停用凭据时使用 `POST /agents/{id}/revoke`；需要删除 Agent 时，管理员必须先显式迁移或删除引用它的 Service。

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

创建或修改 Service 在提交前触发 Agent Snapshot 预算检查。超限统一返回：

```text
422 AGENT_BINDING_LIMIT
或
422 SNAPSHOT_TOO_LARGE

agent_id
binding_count
snapshot_bytes
configured_limit
```

响应不得包含 Origin Credential、Token 或完整 Snapshot。第二个 Binding 指向同一 Tunnel 时返回 `409 TUNNEL_BINDING_CONFLICT`。

---

# 137. Dashboard API

```text
GET /dashboard
```

返回：

```text
Agent Counts

Instance Counts

Service Counts

Active Connections

Traffic Summary

Recent Errors
```

---

# 138. System API

```text
GET /system/info

GET /system/health

GET /system/config
```

敏感配置：

```text
不得返回 Token

不得返回 Private Key
```

---

# 139. API Error

统一：

```json
{
  "error": {
    "code": "AGENT_OFFLINE",
    "message": "agent is offline",
    "request_id": "req_01K..."
  }
}
```

HTTP Status 和业务 Error Code 分离。

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
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;

        proxy_pass http://127.0.0.1:8080;
    }
}
```

Tunnel：

```nginx
server {
    listen 443 ssl;
    server_name *.tunnel.example.com;

    client_max_body_size 2g;

    location / {
        proxy_http_version 1.1;

        proxy_set_header Host $host;

        # 覆盖客户端自带 XFF，禁止把不可信前缀透传给 XTunnel。
        proxy_set_header X-Forwarded-For $remote_addr;

        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;

        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;

        proxy_request_buffering off;
        proxy_buffering off;

        proxy_read_timeout 1h;
        proxy_send_timeout 1h;

        proxy_pass http://127.0.0.1:8081;
    }
}
```

以上示例满足 WebSocket、1GB Upload/Download 和 streaming 的第一阶段验收下限。生产环境仍需根据业务调整 body size、长连接 timeout 和带宽限制，但不得重新启用整请求缓冲。

---

# 142. Server 配置

Server 配置的唯一机器可读契约为 `configs/server.schema.json`，Agent 配置的唯一机器可读契约为 `configs/agent.schema.json`。JSON Schema 必须与 Go Config Struct、示例配置和配置测试从同一字段清单生成或由 CI 做双向一致性检查。字段类型、默认值、范围、是否必填、Secret 标记和是否可热加载只允许在 Schema 中定义一次；本文示例不构成第二份默认值来源。

覆盖优先级固定为：

```text
CLI > XTUNNEL_* Environment > YAML > Schema Default
```

YAML 使用 Strict Decode，未知字段或重复 Key 直接启动失败；未知 CLI Flag 直接失败；`XTUNNEL_*` 命名空间下无法映射到 Schema 的变量直接失败。Duration 统一使用 Go Duration String，大小统一使用整数 Byte。V0.1 不热加载 Server/Agent 主配置；变更后必须显式重启，动态 Service/Binding 配置仍通过 Revision/Snapshot 生效。

两份配置 Schema 固定使用 JSON Schema Draft 2020-12。每个叶子字段必须显式声明 `x-secret` 和 `x-reloadable`；V0.1 主配置的 `x-reloadable` 全部为 `false`。环境变量名由 Schema 点分路径转换：路径段大写后使用双下划线连接，例如 `management.public_url` 对应 `XTUNNEL_MANAGEMENT__PUBLIC_URL`。数组覆盖值使用 JSON Array，标量覆盖值按 Schema 类型解析。CLI 层同样使用 Schema 点分路径。Server/Agent 公共配置入口固定为可选的 `--config <path>` 和可重复的 `--set <schema.path>=<value>`；不接受位置参数，未知 Flag 或 Schema 路径直接失败，同一路径重复覆盖时以后出现的值为准。

推荐：

```yaml
server:
  data_dir: /var/lib/xtunnel

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

agent_runtime:
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

# 143. Agent 配置

```yaml
server:
  endpoint: tunnel.example.com:7443

  tls:
    mode: pinned
    server_pin: "sha256:..."

auth:
  token_file: /var/lib/xtunnel/token

data_dir: /var/lib/xtunnel

transport:
  tcp:
    min_idle: 4
    target_idle: 8
    max_idle: 32
    max_connecting: 16
    max_total: 256

reconnect:
  initial_delay: 1s
  max_delay: 30s
  jitter: 0.2

control:
  high_priority_queue: 32
  normal_queue: 128
  write_timeout: 5s

health:
  max_concurrent: 64
  max_checks_per_second: 50
  max_concurrent_per_origin: 4
  initial_jitter: 1.0
  interval_jitter: 0.2
  report_flush_interval: 1s
  report_batch_size: 128

logging:
  level: info
  format: json
```

---

# 144. Agent Local Files

```text
/var/lib/xtunnel/
├── config.yaml
├── token
├── installation.id
├── agent.lock
│
├── identity/
│   └── trust-state.pb
│
└── config/
    ├── snapshot.pb
    └── snapshot.next     # 仅 Apply/Crash Recovery 期间存在
```

权限：

```text
directory 0700

token 0600

trust-state.pb 0600

snapshot.pb / snapshot.next 0600
```

---

# 145. Agent Install

V0.1 官方支持矩阵固定为：

```text
Server: Linux amd64 / arm64

Agent: Linux amd64 / arm64

Service Manager: systemd

Container: OCI 前台进程模式（不执行 install 子命令）
```

Windows Service、macOS launchd、Alpine OpenRC 和其他 Unix Service Manager 不属于 V0.1 支持范围。代码可以保持跨平台抽象，但 Alpha 发布、文件锁、权限、安装脚本和验收只对上述矩阵作承诺；不允许仅因 Go 能编译就宣称平台受支持。

```bash
xtunnel-agent install \
    --server tunnel.example.com:7443 \
    --token-file /run/secrets/xtunnel-agent-token \
    --server-pin sha256:xxx
```

安装程序还可以从交互式隐藏输入读取 Token。禁止支持 `--token <明文>`；Token 不得出现在 shell history、进程参数、环境变量或日志中。

Install：

```text
Create Data Dir
 ↓
Generate Installation ID
 ↓
Store Token
 ↓
Write Config
 ↓
Install systemd
 ↓
Enable
 ↓
Start
```

读取 Secret 后，安装程序以原子写方式保存到 `<data-dir>/token`，权限为 `0600`。安装成功后是否删除外部 Secret 文件由部署系统负责，XTunnel 不擅自删除用户提供的文件。

---

# 146. 多 Replica 安装

同机：

```bash
xtunnel-agent install \
  --data-dir /var/lib/xtunnel/r1 \
  --service-name xtunnel-agent-r1 \
  --token-file /run/secrets/xtunnel-agent-token \
  ...

xtunnel-agent install \
  --data-dir /var/lib/xtunnel/r2 \
  --service-name xtunnel-agent-r2 \
  --token-file /run/secrets/xtunnel-agent-token \
  ...
```

两者属于：

```text
同一个逻辑 Agent
```

但：

```text
不同 Installation
```

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
继续登记在 AgentRuntime.ActiveWork

不计入新 Session 的 Idle Pool

继续计入原 Instance 的 Active、Usage 和日志

可被 Agent Revoke 或 drain timeout 定位并关闭
```

如果：

```text
Agent Revoke
```

则 Active 也强制关闭。

---

# 149. Agent 重连

同一个运行进程：

```text
instance_id 不变

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

重连时 Server 为该 Instance 递增 `session_generation`。旧 Session cleanup 必须携带旧 generation，并通过 Compare-And-Swap 确认自己仍是 Current Session；若 CAS 失败，只能清理属于旧 Session 且尚未 ACTIVE 的 Idle/Opening Registry 项，禁止修改新 Session 状态。旧 `ActiveWork` 仍在全局 Registry 中，只能自然结束，或由 Revoke / 明确 drain timeout 路径通过其自身 `closeOnce` 关闭。

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
Sync Revision
 ↓
Refill Work Pool
```

SQLite 配置全部保留。

---

# 151. Agent Restart

Agent Restart：

```text
Instance ID 改变
```

已有该 Instance 业务连接：

```text
中断
```

其他 Replica：

```text
继续服务
```

新 Agent Process：

```text
自动加入 Logical Agent
```

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

Flush Usage

Close Agent Sessions

Close SQLite
```

默认：

```text
drain_timeout = 30s
```

Server 超过 `drain_timeout` 后必须主动关闭剩余 Public、Origin、WorkConn 和 Control socket，然后再 Flush/Close SQLite；不得只等待 Context 自然取消，也不得无限阻塞 Shutdown。

---

# 153. Agent Graceful Shutdown

```text
SIGTERM
 ↓
Send DrainRequest
 ↓
Stop Refill WorkPool
 ↓
Server Mark Instance Draining
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

Drain 是两阶段握手。双方收到 Request 后分别以本地 monotonic clock 和相对 `drain_timeout_ms` 建立 Deadline，禁止比较跨主机绝对时间。Server 先把 Instance 从选择集合摘除，并等待已经 Acquire 的 `OPENING` 完成或失败，之后才发送同一 `drain_id` 的 Ack。Agent 在收到 Ack 前仍接受这些已在途 OPEN；收到 Ack 后才拒绝新 OPEN。重复 Request/Ack 必须幂等。若握手超时，Agent 以 `OPEN_DRAINING` 失败仍在途 OPEN，Server 在尚未进入 RAW 时可重新选择其他 Eligible Replica。

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
  max_agents: 1000

  max_instances: 5000

  max_instances_per_agent: 100

  max_bindings_per_agent: 1000

  max_health_targets_per_agent: 2000

  max_health_targets_global: 50000

  max_agent_snapshot_bytes: 786432

  max_active_connections: 20000

  max_connections_per_agent: 5000

  max_connections_per_tunnel: 5000

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
```

WorkConn 全局预算包含 Connecting、Idle、Opening 和 Active。每个 Instance 的 `target_idle` 是 best effort，只能通过 Server 发放的 WorkDemand Budget Lease 补池；达到全局 Idle/FD 预算后不得继续建连。Budget Manager 必须同时实施 per-agent 和 per-instance 公平份额，并为有真实 Pending OPEN 的 Agent 保留最小可用额度，禁止某个拥有大量 Replica 的 Agent 抢占全部 Idle。

`configs/server.schema.json` 是所有 Server 硬限制的唯一机器权威和默认值来源。第 156 节只是由 Schema 生成或经 CI 反向校验的人类可读镜像，不得独立修改。`max_instances_per_agent` 在 Control Auth Commit 前执行；Health 两级 Target Budget 在 Management Candidate 校验和新 Instance Auth Commit 前执行。各 Limit 自其所属里程碑起必须进入真实分配/状态转换路径，不能只解析配置或只上报 Metric：Data Plane/Frame/Queue/FD Limit 从 M1 生效，Health Target Budget 从 M3 生效，HTTP 入口限制从 M4 生效。M7 允许根据 Benchmark 调整默认值，但不能第一次实现这些上限。

公网公平性限制分两层：Active Connection 上限和 Accept/Open Rate Token Bucket。Raw TCP 使用实际 Peer IP；HTTP 使用第 91 节 Trusted Proxy 规则得到的 normalized client IP。HTTP 还执行可配置的请求速率限制，不能只限制底层长连接。所有 per-source 状态使用有容量上限和过期时间的分片 LRU；运维可按 NAT 场景调整数值，但不能绕过 Agent/Tunnel/Global 上限。

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

agent_id

installation_id

instance_id

session_id

tunnel_id

connection_id

trace_id

event

error_code
```

每条日志固定包含 `timestamp`、`level`、`component` 和 `event`。`timestamp`
使用 UTC RFC3339Nano，`level` 使用 `debug/info/warn/error` 小写值。`request_id`、
`trace_id` 及各业务 ID 只在真实上下文存在时写入，不输出空值，也不在日志层生成
替代 ID。标准 JSON 日志不保留 `slog` 默认的 `time` 和 `msg` 字段。

---

# 159. 禁止日志输出

绝不能记录：

```text
Agent Token

Admin Password

Session Cookie

Session Secret

TLS Private Key

Authorization Header

Config Signing Private Key
```

共享日志 Handler 会对上述明确的敏感属性名写出 `[REDACTED]`。调用方仍不得把
Secret 拼入 `event`、错误文本或任意非敏感字段，也不得直接记录完整 Config、
HTTP Header、Cookie、请求体或认证对象。

---

# 160. Metrics

Server：

```text
xtunnel_agents_online

xtunnel_instances_online

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
```

Agent：

```text
xtunnel_agent_control_connected

xtunnel_agent_tcp_idle

xtunnel_agent_tcp_active

xtunnel_agent_origin_connect_total

xtunnel_agent_origin_errors_total

xtunnel_agent_health_checks_in_flight

xtunnel_agent_health_budget_exceeded_total

xtunnel_agent_config_revision
```

Server 默认通过独立 loopback 端点暴露 Prometheus：

```yaml
metrics:
  listen: "127.0.0.1:9090"
  path: /metrics
```

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

---

# 161. Prometheus Cardinality

禁止直接使用：

```text
agent_id

instance_id

tunnel_id

connection_id
```

作为高频 Metrics Label。

Service 级统计进入：

```text
Usage Aggregator
```

---

# 162. Repository Structure

```text
xtunnel/
├── .gitignore
├── go.mod
├── go.sum
├── buf.yaml
├── buf.gen.yaml
├── buf.lock
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
│   │   ├── app/
│   │   ├── identity/
│   │   ├── control/
│   │   ├── config/
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
│   └── systemd/
│
├── configs/
│   ├── server.schema.json
│   └── agent.schema.json
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
            ├── config-key-transition.json
            ├── epoch-transition.json
            └── README.md
```

Proto 工具链固定使用 Buf 管理，但不引入 gRPC：

```text
buf.yaml        → module / lint / breaking policy
buf.gen.yaml    → protoc-gen-go 输出到 internal/protocol/gen
buf.lock        → Proto module lock
tools/versions.env → 精确 Buf 版本/分发包 SHA-256与预期 Plugin 版本
tools/go.mod / go.sum → 固定 protoc-gen-go Module 与校验和
```

生成文件提交仓库。统一命令：

```bash
./tools/bootstrap-proto.sh
./tools/proto.sh lint
./tools/proto.sh breaking
./tools/proto.sh generate-check
```

`bootstrap-proto.sh` 读取 `versions.env`，把匹配 SHA-256 的 Buf Release Binary 安装到根 `.gitignore` 排除的 `.tools/bin`；`protoc-gen-go` 则从 `tools/go.mod/go.sum` 通过 `go build -mod=readonly` 构建到同一目录，并核对输出版本。`proto.sh` 只调用该目录的绝对路径，在每次执行前核对版本，禁止回落到开发机 PATH。`generate-check` 执行 generate 后再运行：

```bash
git diff --exit-code -- api/proto internal/protocol/gen buf.lock
```

CI 调用同一 Wrapper；不得维护另一套安装或生成命令。签名/HMAC 使用的消息必须继续显式调用 deterministic protobuf Marshal，并用跨平台 Golden Vector 验证；生成代码一致不等于签名字节已经正确。

每个 Protocol Golden Vector 必须包含固定 Private/Public Key、`session_secret`、nonce、完整输入字段、deterministic protobuf hex、带 Domain Separator 的 signing input hex、HMAC/Signature hex 和最终 Message hex。测试逐字节比较已有 Fixture；禁止在普通测试运行中自动重写 Fixture。更新 Fixture 必须作为显式 Protocol Review 变更，并同时通过 unknown-field、字段乱序、空字段和签名失败测试。

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
Recover / Roll Back Gateway Identity Rotation Journal
 ↓
Load/Create Gateway TLS Identity
 ↓
Load Config Signing Key
 ↓
Load/Create Server Epoch
 ↓
Load Route Snapshot
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
Start Usage Flusher
 ↓
READY
```

Server External Lock 必须在读取 Restore Journal、Open SQLite、Migration、PKI/Epoch Load/Create 和任何 Runtime 初始化之前获取，并一直持有到所有 Listener、Agent Session 和 SQLite 都关闭。Lock Identity 只依赖始终存在的父目录和稳定 leaf 名，不依赖 leaf 当前存在，因此两个 rename 之间崩溃后仍能取得同一把锁。Lock 文件不在 Data Directory 内，Restore 替换目录不会改变已锁 inode。第二个指向同一 Stable Data Target 的 Server 必须在触碰数据库或身份文件之前快速失败，不能等端口绑定冲突才退出。

Gateway TLS Identity、Config Signing Key 和 Server Epoch 默认只允许在全新 Data Directory 初始化时创建。唯一例外是管理员显式执行第 26 节离线 `gateway rotate-key --maintenance`；普通 Server Start 绝不自动触发该例外。如果数据库或 Installation 历史已经存在但任一文件缺失，且没有可恢复的 Rotation Journal，Server 必须快速失败并要求从一致性备份恢复，禁止静默生成新身份。

没有 Admin User 时，Management 状态为 `SETUP_REQUIRED`；HTTP/TCP Public Ingress 和 Agent Gateway 在首个管理员完成初始化前不启动。

---

# 164. Agent Start

```text
Load Config
 ↓
Acquire Data Dir Lock
 ↓
Load/Create Installation ID
 ↓
Load Token
 ↓
Load Pinned Gateway Identity + AgentTrustState（首次安装可不存在）
 ↓
Recover snapshot.next / snapshot.pb Commit
 ↓
Verify Last Snapshot Hash + Signature + Revision + Key + Epoch
 ↓
Generate Instance ID
 ↓
Connect Agent Gateway
 ↓
TLS Verify
 ↓
Agent Token Auth
 ↓
Create Control Session
 ↓
Sync Revision
 ↓
Build Origin Resolver
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

Agent State

Instance State

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

Instance Selection

Config Revision

Snapshot Binding Count + Serialized Size Boundary

Session Generation Fencing

Control Session Single Reader / Single Writer

Control Outbox Priority / Coalescing / Full-close

AgentRuntime Linearization + Lock-free IO Rule

ActiveWork CloseOnce + Counter Exactly-once

Reconcile Generation Monotonicity

Snapshot Signature

Transition Signature + Journal Recovery

Transition Ack / Exclusion Persistence

Transition Ack ID / Artifact Hash / Observed Next Field Validation

AgentTrustState Transition Idempotency

AgentTrustState / Snapshot Crash Recovery At Every Commit Step

SQLite Repository

SQLite PRAGMA On Every Pooled Connection

ConfigWriteCoordinator Serialization

Health Revision Fencing

WorkDemand Coalescing + Budget Lease Expiry

Health Scheduler Rate / Concurrency / Jitter / Batch

Agent / Instance / Service Status Priority

Strict Config Schema + Override Precedence

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
Token Authentication

Pinned Certificate Same-SPKI Renewal

Offline Gateway Key Rotation: Old Pin Rejected / New Pin Accepted / Omitted Installation Offline

Gateway Identity Rotation Journal Crash Recovery

Multiple Replica

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

Old Revision Instance Ineligible

Concurrent Revision 18 / 19 Reconcile

Health

Old Revision Health Report Cannot Override New UNKNOWN/UNHEALTHY

Health Batch Generation / Split / Deduplication

Health Target Budget Rejects Config Write And Excess Replica Auth

Health Budget At-capacity Session Replacement Does Not Double Reserve Or Release New Generation

Token Rotation

SQLite Migration Upgrade + Interrupted Migration

Concurrent Config Write + Usage Flush + Token Rotate + Installation Upsert Without Unhandled SQLITE_BUSY

Backup → Migration → Restore → Agent Reconnect

Backup Secret File Permissions + Symlink Rejection

Restore Crash Between Directory Renames

Second Server Refuses Same Data Directory Before SQLite/PKI Access

Independent `database.path` Config Is Rejected

Concurrent Service Writes Both Present In Runtime Snapshot

Second Binding For Same Tunnel Rejected By DB + Application Service

Agent Delete With Binding Returns 409 AGENT_IN_USE

Offline Installation Receives Preserved Key/Epoch Transition

ACK_EXCLUDED Installation Later Catches Up Through Preserved Transition Chain

AgentTrustState Commit-point Crash Before/After Every fsync And rename

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

Streaming

Known-Length Low-rate Small-chunk Response Flush <= 100ms + Margin

KeepAlive

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

Agent Replica Failure
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

# 172. Replica Test

一个逻辑 Agent：

```text
1 Replica

2 Replica

10 Replica

100 Replica
```

验证：

```text
Load Distribution

Replica Crash

Replica Reconnect

WorkPool Isolation

Global WorkConn Budget

WorkConn Budget Fairness Across Agents/Replicas

FD Budget Counts Public + WorkConn Socket Pair

Instance A idle=0、Instance B idle>0 时优先使用 B
```

---

# 173. Agent Scale Test

Mock Agent：

```text
100

500

1000
```

逻辑 Agent。

总 Instance：

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
kill -9 Agent Instance

kill -9 XTunnel Server
```

检查：

```text
Replica Failover

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
Invalid Agent Token

Revoked Token

Token Brute Force

Token Rotation

Agent Revoke

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

Config Key Transition

Server Epoch Transition

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

Linux amd64 / arm64 Build Matrix

Multi-arch OCI Image

systemd Packaging Smoke Harness

Config

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

Linux amd64 与 arm64 Binary 均可构建；arm64 在原生或受控 Runner 完成进程启动 + Config/Shutdown Smoke

OCI amd64/arm64 Image 以前台非 root 进程运行，验证只读镜像、持久化 Data Volume 和 SIGTERM 进程退出/资源释放 Smoke（M0 不要求真实 Session Drain）

systemd 执行 install / start / restart / stop / uninstall Smoke

全新 checkout 按固定顺序完成 Web Build 和 Go Build

CI 使用 `npm ci`，缺失或与 `package.json` 不一致的 Lockfile 直接失败；CI 不自动生成或改写 Lockfile

Server/Agent Config Schema 校验、Strict YAML、CLI/Env/YAML/Default 优先级测试通过

OpenAPI Skeleton Validate 通过且不存在未解析占位 Server URL

Vite HTTPS Proxy 完成 Login / Secure Cookie / CSRF POST / Logout Smoke
```

---

# 179. M0.5：Protocol v1 Contract Freeze

完成：

```text
common.proto / control.proto / work.proto

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

Golden Vector 逐 byte PASS

Auth Success / Failure Transcript PASS

Auth 裸 Frame 与 Established Envelope 切换边界 PASS

Control / WorkConn 全部非法方向和非法状态 Case PASS

WorkConn 错误方向/状态/Unknown Field 直接关闭 Case PASS

Transition Ack ID / Artifact Hash / observed Next 组合校验 PASS

Auth、Control、Work、Snapshot 及本地 Last Known Snapshot 的全部结构化 Message 递归 Unknown Field Case 均被拒绝
```

M0.5 是强制 Gate，不是可与 M1 并行补写的文档任务。M0.5 未通过，禁止实现 Server/Agent Protocol Handler；允许继续开发与 Wire Contract 无关的 Lock、Repository、Proxy、Origin Dialer 和测试 Harness。

---

# 180. M1：Secure TCP Data Plane Baseline

M1 使用正式身份和安全协议，但产品能力只要求一个 Agent、一个 Instance、一个静态 Tunnel。

完成：

```text
Protocol v1 Generated Contract

Agent Entity + Token Verification

Installation ID + Instance ID + Session ID

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
Agent
 ↓
Echo Origin
```

验收必须包含逐字节分片、多个 Frame 合并、`OPEN_OK + RAW 首包` 同一次 Read、Half-Close、Context Cancel、Control Reconnect、旧 Session cleanup 不影响新 Session、Outbox 合并/满载关闭以及所有 M1 硬限制生效；断言字节零丢失、零重复，测试结束后 FD 与 goroutine 回到基线。

---

# 181. M2：Replica & Credential Lifecycle

完成：

```text
Multi Replica

Instance Selection

Installation History

Token Rotation + Revoke

Agent Revoke

Old Session ActiveWork Preservation

Replica Failover
```

验收：

```text
同一个 Token

启动多个 Agent

Server 能独立识别所有 Instance

新连接自动分布

旧 Session ActiveWork 自然完成，Revoke 可跨代关闭
```

---

# 182. M3：Configuration + Trust + Health

完成：

```text
Tunnel

Binding

Revision

Snapshot

Signature

AgentTrustState

Config Key / Epoch Transition

Origin Resolver

Health Scheduler + Batch Report

Health Target Budget
```

验收：

```text
Integration Test 通过 Application Service 修改 Origin

Agent 无需重启

自动生效

TrustState / Snapshot 在每个 Commit Crash Point 后可恢复

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

Agent CRUD

Replica View

Token Rotate

Service CRUD

Service Status

Settings
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
Agent Offline

Replica Offline

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

Server/Agent JSON Schema 与 Go Config Struct Drift Check = PASS

OpenAPI Validate + Generated Client/Server Contract Drift Check = PASS

npm ci / npm run build = PASS，Lockfile 零漂移

Linux amd64 / arm64 Build Matrix = PASS，arm64 Protocol Smoke = PASS

OCI amd64 / arm64 Manifest + Foreground / Volume / SIGTERM Smoke = PASS

systemd Install / Restart / Uninstall Smoke = PASS

协议 Fuzz Corpus = PASS，零 Panic / OOM

Control Outbox 满载、Session Replace、Drain/OPEN 并发 = PASS，零 Frame 交错

AgentTrustState / Snapshot 全 Commit Point Crash Recovery = PASS

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
Create Agent
 ↓
Copy Token
 ↓
写入 0600 Secret File
 ↓
Install Agent
 ↓
Agent ONLINE
```

---

## Replica

同一 Token：

```text
Replica A
Replica B
```

Server：

```text
Instances = 2
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
ADR-001-logical-agent-and-replica.md

ADR-002-agent-token-authentication.md

ADR-003-agent-identity-hierarchy.md

ADR-004-agent-gateway-port.md

ADR-005-external-http-tls-termination.md

ADR-006-tcp-work-connection.md

ADR-007-no-tcp-multiplex.md

ADR-008-route-tunnel-binding-model.md

ADR-009-origin-resolved-only-by-agent.md

ADR-010-tunnel-channel-abstraction.md

ADR-011-runtime-state-memory-only.md

ADR-012-sqlite-desired-state.md

ADR-013-revision-and-snapshot.md

ADR-014-config-signing.md

ADR-015-agent-instance-selection.md

ADR-016-no-quic-v0.1.md

ADR-017-proto-is-wire-contract.md

ADR-018-control-session-and-runtime-ownership.md

ADR-019-agent-trust-state-commit.md

ADR-020-status-aggregation.md

ADR-021-config-schema-authority.md

ADR-022-health-scheduler-and-budget.md

ADR-023-openapi-etag-concurrency.md
```

---

# 189. 第一阶段最重要的工程约束

开发过程中不得破坏：

```text
1. Agent 是逻辑 Connector，不是进程。

2. 一个 Agent 可以拥有多个 Replica。

3. Agent Token 属于逻辑 Agent，可以重复用于 Replica。

4. Installation / Instance / Session 必须独立建模。

5. Token 默认长期有效，但必须支持 Rotate 和 Revoke。

6. WorkConn 不重复发送长期 Agent Token。

7. Route 必须经过 Tunnel 和 Binding。

8. OPEN 只能携带 Tunnel ID，
   不能携带 Origin 地址。

9. Origin 只能由 Agent 本地配置解析。

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

20. Snapshot 必须签名。

21. `.proto` 是唯一 Wire Contract，M0.5 未通过不得实现协议 Handler。

22. 每条 Control Session 只能有一个 Reader、一个 Writer、一个 State Owner。

23. Runtime State 在 AgentRuntime Lock 下线性化，锁内禁止 IO、阻塞和 Conn.Close。

24. Agent TrustState 与 Snapshot 必须按持久化提交点恢复到同一代。

25. Agent/Instance 不聚合 Origin Health；Service Status 只由 Server 统一计算。

26. Server/Agent 主配置以 JSON Schema 为唯一机器可读契约并 Strict Decode。

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
                  Logical Agent
                           │
                ┌──────────┼──────────┐
                │          │          │
                ▼          ▼          ▼
            Instance A Instance B Instance C
                │          │          │
                ▼          ▼          ▼
              Origin     Origin     Origin
```

公网请求：

```text
Route
 ↓
Tunnel
 ↓
Binding
 ↓
Logical Agent
 ↓
Healthy Replica
 ↓
TCP WorkConn
 ↓
Origin
```

这是第一阶段需要真正建立起来的核心系统。

第一阶段完成后，XTunnel 应已经具备：

```text
集中管理

多 Agent

Agent Replica

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
