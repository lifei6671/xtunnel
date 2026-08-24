# XTunnel Standalone V0.1 开发执行计划

> **文档用途**：将《XTunnel Standalone 第一阶段完整技术方案 V0.1》转换为可执行、可推进、可验收的开发 Backlog
>
> **进度基线日期**：2026-08-24
>
> **当前阶段**：M0 工程初始化
>
> **当前结论**：`M0-02` 已完成实现、用户 Code Review、提交与干净工作区验收；下一任务为 `M0-03`

---

# 1. 文档权威与使用规则

本文档是 **开发任务、依赖、进度和验收证据** 的唯一跟踪入口，但不重复或取代技术契约。

权威顺序固定为：

1. 产品边界、架构和行为语义：[`docs/xtunnel_standalone_v0.1.md`](./xtunnel_standalone_v0.1.md)。
2. Protocol v1 Wire Contract：`api/proto/common.proto`、`control.proto`、`work.proto`。
3. Server/Agent 配置：`configs/server.schema.json`、`configs/agent.schema.json`。
4. REST API：`api/openapi/openapi.yaml`。
5. 开发任务与进度：本文档。
6. 开发工具链与协作约束：根目录 `AGENTS.md`。

如果任务描述与上述权威契约冲突，必须先修正任务，不允许以“任务已排期”为由偏离契约。

---

# 2. 状态、证据与更新规则

## 2.1 任务状态

| 状态 | 含义 | 进入条件 |
| --- | --- | --- |
| `NOT_STARTED` | 未开始 | 默认状态 |
| `READY` | 可开工 | 依赖全部 `DONE`，输入契约已冻结 |
| `IN_PROGRESS` | 实施中 | 已有明确负责人和工作分支 |
| `REVIEW` | 待复审 | 产物已完成，所有验收命令已执行 |
| `BLOCKED` | 被阻塞 | 已记录阻塞原因、影响和解除条件 |
| `DONE` | 已完成 | 产物、测试、复审和验收证据全部齐备 |

## 2.2 `DONE` 的强制证据

每个任务只有同时满足以下条件才能标记 `DONE`：

- “产物”列中的文件或行为真实存在。
- 任务相关单元/集成/契约测试通过，断言覆盖关键业务字段和失败分支。
- 验收命令在干净 checkout 或 CI 中成功。
- 证据包含：Commit SHA、命令与结果摘要。M0-10 完成前，允许使用干净 checkout 的本地命令记录；M0-10 完成后，所有新 `DONE` 证据必须再附 CI Run 链接/编号。
- Go 任务的证据必须记录 `go env GOVERSION` 和 `go env GOTOOLCHAIN`；实际工具链必须是 M0-01 批准的精确 `go1.27.x` 版本，且 `GOTOOLCHAIN=local`。
- 没有未处理的安全、数据一致性、协议或资源泄漏类错误。
- 若修改契约，总方案、Proto/Schema/OpenAPI 和相关 Golden Fixture 已同步。

`文档已写`、`Schema 校验通过` 或 `VALID` 不等于功能 Gate 通过。必须有真实运行路径和自动化证据。

## 2.3 进度更新协议

每次合并与某任务相关的变更时：

1. 更新对应任务状态。
2. 在“执行记录”中增加一条证据。
3. 重算里程碑的 `DONE/总任务数`。
4. 只有里程碑 Gate 任务 `DONE` 才可启动下一个强依赖里程碑。
5. 阻塞项不得只写“待确认”，必须写清需要谁、提供什么、解锁哪些任务。

---

# 3. 总体关键路径

```text
M0 工程初始化
 ↓
M0.5 Protocol v1 Contract Freeze
 ↓
M1 Secure TCP Data Plane Baseline
├──→ M2 Replica & Credential Lifecycle ──┐
└──→ M3 Configuration + Trust + Health ─┘
                    ↓
           M4 Product Data Plane
                                           ↓
                                  M5 REST API + Web
                                           ↓
                                  M6 Observability
                                           ↓
                                  M7 Hardening + Alpha Gate
```

依赖规则：

- M0.5 是 M1 Protocol Handler 的强制入口 Gate。
- M2 和 M3 在 M1 Gate 后可并行，但 M4 产品数据面必须同时等待 M2 的 Replica Selection/Failover 和 M3 的 Tunnel/Binding/Snapshot 契约。
- M5 的 OpenAPI Entry Gate 可在 M4 后半段提前准备，Handler 和 Web 实现必须等 Gate 通过。
- M6 的 Logging/Metrics 骨架可从 M0/M1 纵向渗透，但 M6 Gate 要求完整产品链路可观测。
- M7 只调优和验证已存在的正确性边界，不允许第一次实现 M1/M3 应有的上限与恢复机制。

---

# 4. 进度仪表盘

| 里程碑 | 任务数 | 已完成 | 状态 | 入口依赖 | 退出 Gate |
| --- | ---: | ---: | --- | --- | --- |
| M0 工程初始化 | 12 | 2 | `IN_PROGRESS` | 技术方案基线 | M0-12 |
| M0.5 Protocol Freeze | 10 | 0 | `NOT_STARTED` | M0-06 | M05-10 |
| M1 Secure TCP Baseline | 14 | 0 | `NOT_STARTED` | M0-12 + M05-10 | M1-14 |
| M2 Replica/Credential | 8 | 0 | `NOT_STARTED` | M1-14 | M2-08 |
| M3 Config/Trust/Health | 13 | 0 | `NOT_STARTED` | M1-14 | M3-13 |
| M4 Product Data Plane | 10 | 0 | `NOT_STARTED` | M2-08 + M3-13 | M4-10 |
| M5 REST API/Web | 11 | 0 | `NOT_STARTED` | M3-13 + M4-10（M5-01 可在 M4 后半段准备） | M5-11 |
| M6 Observability | 7 | 0 | `NOT_STARTED` | M5-11 | M6-07 |
| M7 Hardening/Alpha | 10 | 0 | `NOT_STARTED` | M2-08 + M3-13 + M4-10 + M5-11 + M6-07 | M7-10 |
| **合计** | **95** | **2** |  |  |  |

`M0=IN_PROGRESS` 只表示项目已进入该阶段，不表示其中任务已完成。

---

# 5. M0：工程初始化

## 5.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M0-01 | 建立 Go Module 与目录骨架 | 无 | `go.mod`、`cmd/server`、`cmd/agent`、`internal/*` 骨架 | `go.mod` 声明 `go 1.27` 并由 `toolchain` 记录稳定的精确 `go1.27.x` 版本；提供 `GOTOOLCHAIN=local` 的版本检查入口；`go test ./...`、`go vet ./...`；无空壳公共抽象 | `DONE` |
| M0-02 | 定义 Config Schema 与加载器 | M0-01 | `configs/server.schema.json`、`agent.schema.json`、Server/Agent Config | Strict YAML；CLI > Env > YAML > Schema Default；未知字段/环境变量失败 | `DONE` |
| M0-03 | Server/Agent 进程骨架 | M0-01、M0-02 | `cmd/server/main.go`、`cmd/agent/main.go`、启停生命周期 | 两个进程均可启动；SIGTERM 退出且释放资源 | `READY` |
| M0-04 | 结构化日志基座 | M0-01 | 共享 Logging 配置与 JSON Handler | 级别、时间、`request_id/trace_id` 字段稳定；无 Secret 输出 | `READY` |
| M0-05 | Server Data Target/External Lock + SQLite/Migration | M0-01、M0-02 | Stable Target/External Lock、`migrations/`、`internal/repository/sqlite` | 数据库访问统一使用 GORM；必须先计算 Stable Data Target 并获取 Data Directory 外的同一把 Lock，再检查 Restore Journal/Open SQLite；双进程在触碰 DB/PKI 前拒绝；新库、幂等启动、中断 Migration 测试；引入依赖前先确认 | `READY` |
| M0-06 | 锁定 Proto 工具链骨架 | M0-01 | `buf*.yaml`、`tools/versions.env`、`tools/go.mod`、`bootstrap-proto.sh`、`proto.sh` | `tools/go.mod` 与根 Module 使用相同 Go 1.27.x 工具链；`GOTOOLCHAIN=local` 构建 protoc-gen-go；Buf/protoc-gen-go 精确版本与分发包 SHA-256 可校验；不回落 PATH；三个 Wrapper 子命令可运行 | `READY` |
| M0-07 | OpenAPI 骨架与校验 | M0-01 | `api/openapi/openapi.yaml`、校验入口 | 校验器选型/版本经依赖变更确认；OpenAPI Validate 通过；无占位 Server URL；CI 可执行漂移检查 | `READY` |
| M0-08 | Web 工程、生产构建与 Go Embed | M0-01、M0-07 | `web/package*.json`、Vite/React 骨架、`web/embed.go` | `npm ci`、Web Build、Go Embed 通过；Lockfile 不由 CI 改写 | `NOT_STARTED` |
| M0-09 | OCI 与 systemd 包装骨架 | M0-03、M0-08 | `deploy/docker`、`deploy/systemd` | amd64/arm64；非 root；只读镜像 + Data Volume；install/start/restart/stop/uninstall Smoke | `NOT_STARTED` |
| M0-10 | CI 和跨平台构建矩阵 | M0-02至 M0-09 | CI Workflow | CI/OCI Builder 固定与 `go.mod toolchain` 一致的 `go1.27.x` 精确版本并设置 `GOTOOLCHAIN=local`；干净 checkout 中 Proto/Web/Go 顺序构建；Linux amd64/arm64 进程 Smoke | `NOT_STARTED` |
| M0-11 | 首个 Admin Bootstrap | M0-03、M0-05 | `admin create`、`SETUP_REQUIRED`、本机 Bootstrap Socket/离线写入路径 | 无 Admin 时只启 Management；Server 运行时仅通过权限 `0600` 的本机 Socket 事务创建，停止时取得 External Lock 后写入；密码仅从 TTY/文件读取；重复创建拒绝 | `NOT_STARTED` |
| M0-12 | M0 Gate 验收 | M0-01至 M0-11 | M0 验收证据 | 下方 Gate Checklist 全部通过，且所有前置任务均有 CI Run 证据 | `NOT_STARTED` |

## 5.2 可并行推进

```text
M0-01
 ├── M0-02 ──┬── M0-03
 │           └── M0-05
 ├── M0-04
 ├── M0-06
 └── M0-07 ── M0-08

M0-03 + M0-08 ── M0-09
M0-03 + M0-05 ── M0-11
M0-02 至 M0-09 ── M0-10
M0-01 至 M0-11 + M0-10 ── M0-12
```

## 5.3 M0 Gate Checklist

- [ ] 根 `go.mod` 声明 `go 1.27`，根/工具 Module、CI 和 OCI Builder 使用同一个稳定、精确的 `go1.27.x` 补丁版本；验收设置 `GOTOOLCHAIN=local`，并记录匹配的 `go env GOVERSION`/`GOTOOLCHAIN`。
- [ ] `go test ./...` 通过。
- [ ] `go vet ./...` 通过。
- [ ] Server/Agent Config Schema、Strict Decode 与覆盖优先级测试通过。
- [ ] Server 在 External Lock 前不触碰 SQLite/PKI，第二进程快速失败。
- [ ] 全新库进入 `SETUP_REQUIRED`，运行中/离线 `admin create` 与重复拒绝测试通过。
- [ ] `./tools/proto.sh lint`、`breaking`、`generate-check` 的 M0 骨架流程可执行。
- [ ] `npm ci` 和 Web Production Build 通过，产物被 Go Embed。
- [ ] Linux amd64/arm64 Binary 可构建和启动。
- [ ] OCI 和 systemd Smoke 通过。
- [ ] 干净 checkout CI 通过。

---

# 6. M0.5：Protocol v1 Contract Freeze

## 6.1 开工限制

M0.5 Gate 通过前，禁止开发 Server/Agent Protocol Handler。可并行开发不依赖 Wire Contract 的 Repository、Lock 骨架、Origin Dialer 和测试 Harness。

## 6.2 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M05-01 | 冻结 Common Types | M0-06 | `api/proto/common.proto` | package/go_package、enum 数值、reserved range、ErrorCode 完整 | `NOT_STARTED` |
| M05-02 | 冻结 Auth/Control Contract | M05-01 | `api/proto/control.proto` | 裸 Auth Frame、ControlEnvelope、Transition/Ack、Health Batch 完整 | `NOT_STARTED` |
| M05-03 | 冻结 Work Contract | M05-01 | `api/proto/work.proto` | WorkHello/Ready/Open/Response；RAW 切换；各状态唯一裸 Message | `NOT_STARTED` |
| M05-04 | 生成代码与 Breaking Baseline | M05-01至 M05-03 | `internal/protocol/gen`、Buf Initial Baseline | 明确记录“首次冻结无历史前代”的 Baseline 建立方式，禁止与自身比较伪装 Breaking 证据；生成结果提交；`lint/breaking/generate-check` 通过 | `NOT_STARTED` |
| M05-05 | Frame Codec 契约实现 | M05-04 | `internal/protocol/frame`、`codec` | UVarint 分片/合并、上限、EOF、Auth/Control/Work 分层测试 | `NOT_STARTED` |
| M05-06 | 递归 Unknown Field 拒绝 | M05-04 | 共享 Validator 与表驱动测试 | Auth、Control、Work、Snapshot、Last Known Snapshot 全覆盖 | `NOT_STARTED` |
| M05-07 | Deterministic Protobuf 安全字节 | M05-04、M05-06 | HMAC/签名输入构造器 | 清空 MAC/签名字段；重建已知字段；固定 Runtime 版本 | `NOT_STARTED` |
| M05-08 | Protocol Golden Vectors | M05-07 | `tests/golden/protocol-v1/*` | WorkHello、Snapshot、Key/Epoch Transition 固定字节、Hash、HMAC/签名；测试不自动改 Fixture | `NOT_STARTED` |
| M05-09 | 状态/方向/幂等契约测试 | M05-02、M05-03、M05-05 | Protocol State Test | Auth 提交点、Control 非法方向、Work 直接关闭、Transition/Drain 幂等 | `NOT_STARTED` |
| M05-10 | M0.5 Protocol Gate | M0-12、M05-01至 M05-09 | Protocol Freeze 证据 | 下方 Gate Checklist 全部通过 | `NOT_STARTED` |

## 6.3 M0.5 Gate Checklist

- [ ] `./tools/proto.sh lint` 通过。
- [ ] `./tools/proto.sh breaking` 通过。
- [ ] `./tools/proto.sh generate-check` 通过。
- [ ] Golden Vector 逐字节比较通过。
- [ ] Auth Success/Failure Transcript 及 Auth→Established 提交边界通过。
- [ ] Control/Work 方向、状态、乱序、重复、Unknown Field 全部测试通过。
- [ ] Transition Ack 的 ID、Artifact Hash、Current/Next 字段组合通过。
- [ ] Proto 变更已完成独立 Protocol Review。

---

# 7. M1：Secure TCP Data Plane Baseline

M1 只要求“一个逻辑 Agent + 一个 Instance + 一个静态 TCP Tunnel”，但必须使用正式身份、安全协议和真实资源上限。

## 7.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M1-01 | Agent/Token 领域模型 | M0-05 | Server Domain/Repository | Agent 状态、Token Hash、Secret 不落库、边界测试 | `NOT_STARTED` |
| M1-02 | Token 创建与验证 | M1-01 | Application Service + Repository | CSPRNG、常量时间比较、一次性明文返回 | `NOT_STARTED` |
| M1-03 | Installation/Instance/Session 身份 + Agent Data Lock | M1-01 | Agent Identity 持久化、Agent Data Directory Lock、Server Registry Key | Agent 必须在读写 token/installation/trust/snapshot 前取得整个数据目录独占锁；双进程拒绝；ID 格式、原子落盘、冲突拒绝、重启稳定 | `NOT_STARTED` |
| M1-04 | Agent Gateway TLS/ALPN + Identity Rotation | M0-05、M0-11、M05-10 | Server Gateway、Agent Dialer、`gateway rotate-key --maintenance`、Rotation Journal | 首个 Admin 完成前 Gateway 不启动；TLS1.3；public/pinned mode；ALPN empty/unknown 拒绝；Handshake 上限；Server 停止并持 External Lock 时才能轮换；Journal write/fsync/rename 恢复；新 Pin 只写 `0600` 独占文件 | `NOT_STARTED` |
| M1-05 | Auth 与 Control Session 建立 | M1-02至 M1-04 | Auth Handler、Session Secret、Session Registry | Auth Failure 可区分；Success flush 提交点；generation fencing | `NOT_STARTED` |
| M1-06 | AgentRuntime 所有权与线性化 | M1-03、M1-05 | Runtime Registry、ActiveWork | 固定 Lock 规则；锁内无 IO/Close/阻塞；计数 exactly-once | `NOT_STARTED` |
| M1-07 | Control Session Owner/Outbox | M1-05、M1-06 | Single Reader/Writer/Owner、有界队列 | 优先级、合并、Transition 有序、队列满关闭、无 goroutine leak | `NOT_STARTED` |
| M1-08 | WorkHello HMAC/Lease/Replay | M1-05、M05-08 | Work Auth Handler + Replay Cache | HMAC Vector；Lease 消费与 Replay 原子；无 wall-clock 依赖 | `NOT_STARTED` |
| M1-09 | WorkPool 与 Budget Lease | M1-06、M1-08 | Server/Agent Work Pool | Connecting/Idle/Opening/Active 有界；Demand generation 合并；Lease 过期 | `NOT_STARTED` |
| M1-10 | OPEN 状态机 | M1-09 | OpenRequest/OpenResponse 处理 | IDLE→OPENING→ACTIVE/CLOSED；RAW 前传输失败最多在同一 Instance 换 WorkConn 重试一次，M1 不引入 Replica 重选；已转发业务字节绝不重试；超时/reset/失败资源只释放一次 | `NOT_STARTED` |
| M1-11 | RAW Streaming/Half-Close/Cancel | M1-10 | Bidirectional Proxy | `OPEN_OK + RAW` 同 Read 无丢失；Half-Close；Cancel 解除 IO 阻塞 | `NOT_STARTED` |
| M1-12 | M1 Resource/Timeout/FD Limits | M1-04至 M1-11 | Limit Manager + 配置接入 | Frame/Auth/Queue/Conn/Pending Open/Replay/FD 在真实路径生效 | `NOT_STARTED` |
| M1-13 | Baseline Reconnect/Graceful Shutdown | M1-07至 M1-12 | Agent Backoff、Server/Agent Drain | 网络/Server 容量错误使用 Jitter Backoff 且遵循 `retry_after`；Token/Pin/Version 永久错误停止快速重试；只在 Session 稳定运行后重置 Backoff；新 generation 不被旧 cleanup 破坏；deadline 后强制关闭 | `NOT_STARTED` |
| M1-14 | M1 Gate：TCP Echo End-to-End | M1-01至 M1-13 | `tests/integration` 中的 ephemeral Public Listener、静态 Tunnel/Origin Fixture、Echo Origin | Public TCP→Server→Agent→Echo；Harness 不新增临时生产 Schema；下方 Checklist 全通过 | `NOT_STARTED` |

## 7.2 M1 Gate Checklist

- [ ] 逐字节分片、多 Frame 合并、`OPEN_OK + RAW` 同 Read 通过。
- [ ] Half-Close、Context Cancel、Origin Reset/Timeout 测试通过。
- [ ] Control Reconnect 与旧 Session Cleanup 不影响新 generation。
- [ ] Outbox 合并、优先级和满载关闭通过。
- [ ] 所有 M1 资源上限在真实分配路径被拒绝，计数不为负。
- [ ] 测试结束后 FD 和 goroutine 回到基线。
- [ ] `go test ./...` 和 M1 Integration Suite 通过。

M1 的静态 Tunnel 只能由 Integration Test Harness 注入，不得为过渡测试新增一套临时生产配置，也不得提前制造绕开 M3 Application Service 的持久化接口。

---

# 8. M2：Replica & Credential Lifecycle

## 8.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M2-01 | Multi-Replica Runtime Registry | M1-14 | 多 Instance Registry | 同 Token 多 Installation/Instance；独立 Session/Pool/Counter | `NOT_STARTED` |
| M2-02 | Instance Selection Baseline | M2-01 | Selection Strategy | 只按 Current Session、非 DRAINING、Idle/Capacity 过滤后 Least Active + RR tie-break；并发公平性测试；Revision/Health Eligible 留给 M3-09 接入 | `NOT_STARTED` |
| M2-03 | Installation History | M1-03、M2-01 | Installation Repository + Query | first/last seen、machine/host 变化、Security Event 持久化 | `NOT_STARTED` |
| M2-04 | Token Rotate/Revoke | M1-02、M2-01 | Credential Lifecycle Service | Rotate 时并存期；Revoke 新认证失败；日志不泄密 | `NOT_STARTED` |
| M2-05 | Agent Revoke | M2-04 | Agent Revoke Workflow | 阻止新 Auth；关闭全代 Session/ActiveWork；幂等 | `NOT_STARTED` |
| M2-06 | Session Replacement 保留 ActiveWork | M2-01、M1-13 | Tombstone + Cross-generation Cleanup | 旧 Active 自然结束；旧 cleanup 只清 Idle/Opening；`closeOnce` | `NOT_STARTED` |
| M2-07 | Replica Failover + Pre-RAW Reselect | M2-02、M2-06 | Failover Integration Test | Replica 崩溃后新连接选其他 Instance；RAW 前符合契约的失败可最多跨 Replica 重选一次；已进 RAW 或已转发业务字节不自动重放 | `NOT_STARTED` |
| M2-08 | M2 Gate | M2-01至 M2-07 | M2 验收证据 | 多 Replica、Rotate/Revoke、Failover、ActiveWork 保留全通过 | `NOT_STARTED` |

## 8.2 M2 Gate Checklist

- [ ] 同一 Token 启动多个 Agent Replica，Server 能独立识别。
- [ ] 新连接按资格和负载分布，无单 Replica 饿饿/垄断。
- [ ] Token Rotate/Revoke 和 Agent Revoke 的在线/离线路径通过。
- [ ] 旧 Session ActiveWork 自然完成，Revoke 可跨 generation 关闭。
- [ ] Replica 崩溃/重连不造成计数泄漏、重复转发或新 Session 被旧 cleanup 清除。

---

# 9. M3：Configuration + Trust + Health

## 9.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M3-01 | Tunnel/Binding 领域与存储 | M1-14、M0-05 | Domain + SQLite Repository | 不变量、唯一性、引用关系和容量边界 | `NOT_STARTED` |
| M3-02 | Application Service + Version Transaction | M3-01 | Server Application Service | Service Aggregate 修改在单事务递增 version/revision；并发写不丢失 | `NOT_STARTED` |
| M3-03 | Snapshot Builder/Size Gate | M3-02 | AgentSnapshot Builder | 稳定排序、绑定数/字节上限在事务前校验 | `NOT_STARTED` |
| M3-04 | Snapshot Signing + Transition Journal | M3-03、M05-08 | Signing Service、Transition Repository | Current/Next 双签；Artifact 固定 Hash；Ack/Exclude 事务一致 | `NOT_STARTED` |
| M3-05 | AgentTrustState/Snapshot 持久化 | M3-04 | `identity/trust-state.pb`、`config/snapshot.pb` | fsync/rename/directory fsync；Hash/Revision/Key/Epoch 恢复同代 | `NOT_STARTED` |
| M3-06 | Snapshot Reconcile/Observed Revision | M3-03、M3-05、M1-07 | Reconciler + ConfigAck | 过期 Revision 拒绝；高 Revision 合并；Ack 后才 Eligible | `NOT_STARTED` |
| M3-07 | Origin Resolver | M3-03、M3-05 | Agent Origin Resolver | HTTP/HTTPS/TCP、DNS/IPv4/IPv6、TLS Server Name、SSRF 边界 | `NOT_STARTED` |
| M3-08 | 中心 Health Scheduler | M3-07 | Heap/Wheel Scheduler + Semaphores | 全局/per-origin 并发、Rate、initial/interval jitter；无 per-binding ticker | `NOT_STARTED` |
| M3-09 | Health Batch/Revision Fencing + Eligible Selection | M3-06、M3-08、M2-02 | Pending Accumulator、Batch Reporter、完整 Instance Eligible Filter | `tunnel_id` 合并；出队分配 generation；将 required/observed Revision 和 Per-Tunnel Health 接入 M2 Selection；旧 Revision Health 不放行 | `NOT_STARTED` |
| M3-10 | Health Target Budget Manager | M3-01、M3-08、M2-06 | Reserve/Commit/Release Manager | `(agent_id,instance_id)` 所有权；固定锁顺序；重连不双计费/误释放 | `NOT_STARTED` |
| M3-11 | Agent/Instance/Service Status | M3-06、M3-09、M3-10 | `internal/server/status` | 状态优先级唯一；Origin Health 不污染 Agent/Instance；Web 不重算 | `NOT_STARTED` |
| M3-12 | Durable Operations：Backup/Restore | M0-05、M3-04、M3-05 | `backup create/restore`、Backup Manifest、Restore Journal | 在线 Create 通过本机控制通道建立 Config Write Barrier；离线 Create/Restore 使用同一 Stable Target External Lock；备份 SQLite + Gateway Key + Config Signing Key + Epoch + Transition Journal；Manifest/Hash/Schema 校验；同盘 staging/rollback/journal 可恢复 | `NOT_STARTED` |
| M3-13 | M3 Gate | M3-01至 M3-12 | Application Service Integration + Crash Tests | 下方 Checklist 全部通过 | `NOT_STARTED` |

## 9.2 M3 Gate Checklist

- [ ] 通过 Application Service 修改 Origin，Agent 无需重启即生效。
- [ ] Snapshot 的 Revision、签名、大小和 Binding 边界均可自动化验证。
- [ ] TrustState/Snapshot 在每个 write/fsync/rename/directory-fsync 崩溃点可恢复到同一代。
- [ ] Key/Epoch Transition 重复、冲突 Artifact、离线追赶和 ACK_EXCLUDED 通过。
- [ ] Health Rate/Concurrency/Jitter/Batch/Revision Fencing 通过。
- [ ] 超过 Agent/Global Health Target Budget 的 Config Write 和 Replica Auth 被拒绝。
- [ ] 满容量 Session Replacement 不 Double Reserve，旧 cleanup 不释放新 Reservation。
- [ ] `backup create/restore` 在线/离线路径通过，Manifest 覆盖全部长期密钥和 Transition Artifact，Restore 不与旧目录合并。

---

# 10. M4：HTTP + TCP Product Data Plane

## 10.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M4-01 | Immutable Route Snapshot | M3-02、M3-11 | `internal/server/route` | Revision 原子替换；读路径无 SQLite；不完整 Snapshot 不发布 | `NOT_STARTED` |
| M4-02 | HTTP Host/Path Router | M4-01 | HTTP Matcher | Host 规范化、IDNA、端口、路径段边界、Trailing Slash 测试 | `NOT_STARTED` |
| M4-03 | Streaming Reverse Proxy | M4-02、M1-11 | HTTP Ingress Proxy + Tunnel-aware Transport | 不缓冲整请求/响应；1GB upload/download；Context Cancel；HTTP KeepAlive 不得跨 Tunnel 复用 Origin 连接 | `NOT_STARTED` |
| M4-04 | Forwarded/Trusted Proxy 边界 | M4-03 | Header Sanitizer + Peer Normalizer | 删除外部伪造 Forwarded Header；仅信任配置 CIDR | `NOT_STARTED` |
| M4-05 | WebSocket Upgrade | M4-03、M4-04 | WebSocket Proxy | Upgrade、双向流、Half-Close/断连、长连接 Timeout | `NOT_STARTED` |
| M4-06 | TCP Listener Manager | M3-02、M4-01 | Listener Reconciler | 数据库唯一性/端口范围/保留端口冲突在事务前拒绝；OS `Listen(port)` 失败不回滚 Desired State，只标记对应 Service `APPLY_FAILED` 并周期重试；其他 Listener 继续；新旧 Listener 原子交接；重启恢复 | `NOT_STARTED` |
| M4-07 | Raw TCP/SSH Data Plane | M4-06、M1-11、M2-02 | TCP Ingress | SSH/Raw TCP 逐字节转发；无协议特判；错误映射稳定 | `NOT_STARTED` |
| M4-08 | Caddy/Nginx HTTPS 集成 | M4-03、M4-05 | Deploy Example + E2E | HTTPS 在前置代理终止；Host/Origin/Forwarded 语义正确 | `NOT_STARTED` |
| M4-09 | Public Ingress Limits | M4-03、M4-06 | Per-source/Agent/Tunnel/Global Limits | LRU 有界 + TTL；HTTP Rate/Body/Header；TCP Accept/Open/Active 上限 | `NOT_STARTED` |
| M4-10 | M4 Gate | M4-01至 M4-09 | Product Data Plane E2E | HTTP/HTTPS/WebSocket/SSH/Raw TCP 全部通过 | `NOT_STARTED` |

## 10.2 M4 Gate Checklist

- [ ] HTTP Host + Path 路由、路径边界和错误页通过。
- [ ] Caddy/Nginx 后 HTTPS 和 WebSocket 通过。
- [ ] 1GB Upload/Download 不整体缓冲，内存不随 Body 线性增长。
- [ ] SSH 和通用 Raw TCP 可持续传输、Half-Close 和取消。
- [ ] Route/Listener Snapshot 并发更新无窗口期错路由。
- [ ] Public Ingress 所有上限在真实入口生效。

---

# 11. M5：OpenAPI + REST API + Web Console

## 11.1 Entry Gate

M5-01 通过前，Handler 和 Web 只能建骨架，不得各自定义 DTO、Nullable、错误码或分页语义。

对外 HTTP API Handler 可采用 Gin，但框架只作为 HTTP 适配层；OpenAPI、Generated Server Contract 和 Application Service 分别继续承担契约、传输类型和业务逻辑权威。首次引入 Gin 时仍须确认并锁定精确版本，禁止为尚未实现的 Handler 提前加入依赖。

## 11.2 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M5-01 | 冻结完整 OpenAPI | M3-02、M3-11、M4-02 | `api/openapi/openapi.yaml` | 全部 Schema/Required/Nullable/Error/Status/Pagination/PATCH/ETag 完整；Lint/Breaking PASS | `NOT_STARTED` |
| M5-02 | 生成 Client/Server Contract | M5-01 | Go Server Types + TypeScript Client | 可重复生成；干净 checkout 零漂移 | `NOT_STARTED` |
| M5-03 | Admin Login/Session/CSRF | M5-02、M0-08、M0-11 | Auth Handler + Web Login | Secure/HttpOnly/SameSite Cookie；Origin/Host 规则；Login/Logout/CSRF E2E | `NOT_STARTED` |
| M5-04 | Agent/Replica/Credential API | M2-08、M5-02 | REST Handler | CRUD/Rotate/Revoke；Token 只显示一次；`Cache-Control: no-store` | `NOT_STARTED` |
| M5-05 | Service/Tunnel/Binding API | M3-13、M4-10、M5-02 | REST Handler | 调用既有 Application Service；不在 Handler 重写事务逻辑 | `NOT_STARTED` |
| M5-06 | PATCH/ETag/Pagination 并发契约 | M5-04、M5-05 | Handler + Repository Tests | Agent 和 Service Aggregate 均覆盖 428/412；omitted/null/value；opaque token 50/200；version 原子递增 | `NOT_STARTED` |
| M5-07 | Settings/Read-only Runtime API | M5-02 | Settings Handler | 只返回允许公开的有效配置；不泄露 Secret | `NOT_STARTED` |
| M5-08 | Dashboard/Status UI | M5-02、M5-04、M5-05 | React Pages | 直接渲染 Server Status；不在前端重算状态 | `NOT_STARTED` |
| M5-09 | Agent/Service 管理 UI | M5-03至 M5-08 | CRUD/Rotate/Revoke/Replica View | 日常操作无需 SQLite 或手改 Agent Service Config | `NOT_STARTED` |
| M5-10 | Contract/E2E Test Suite | M5-02至 M5-09 | API Contract + Browser E2E | 错误码、并发 PATCH、CSRF、Token no-store、生成漂移全覆盖 | `NOT_STARTED` |
| M5-11 | M5 Gate | M5-01至 M5-10 | M5 验收证据 | 下方 Checklist 全部通过 | `NOT_STARTED` |

## 11.3 M5 Gate Checklist

- [ ] OpenAPI Lint、Breaking 与 Generated Drift Check 通过。
- [ ] API 实际响应与 OpenAPI Contract 零漂移。
- [ ] 并发 PATCH 不丢失更新，缺少 If-Match 返回 428，冲突返回 412。
- [ ] 分页 Token 不可由前端解析，默认 50，最大 200。
- [ ] Login/Secure Cookie/CSRF/Logout 完整 E2E 通过。
- [ ] Agent/Service/Replica/Token 日常工作流可在 Web 中完成。

---

# 12. M6：Observability

## 12.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M6-01 | 全链路 JSON Logging | M1-14、M5-11 | 稳定日志字段 | request/trace/session/connection 可关联；Secret 脱敏；级别正确 | `NOT_STARTED` |
| M6-02 | Prometheus Metrics | M4-10 | `/metrics` + Metric Registry | 请求数/错误率/P50/P99、Session/Pool/Limit/Health；低基数 Label | `NOT_STARTED` |
| M6-03 | OpenTelemetry Trace | M4-10 | Server→Agent Trace Propagation | `ingress.Accept→tunnel.DialContext→transport.Acquire→origin.Dial→proxy.Bidirectional` 可关联 | `NOT_STARTED` |
| M6-04 | Usage Aggregation | M4-10、M0-05 | Usage Buffer/Flush/Repository | 字节/连接计数 exactly-once；Batch Flush；重启无负数/重复 | `NOT_STARTED` |
| M6-05 | Error/Status Observability | M3-11、M6-01、M6-02 | Error Code Dashboard Data | Agent Offline/Replica Offline/Origin Down/No Capacity/Protocol Error 可区分 | `NOT_STARTED` |
| M6-06 | 运维诊断流程 | M6-01至 M6-05 | Runbook + Dashboard | 从报警可定位到状态、Metric、Trace 和日志 | `NOT_STARTED` |
| M6-07 | M6 Gate | M6-01至 M6-06 | Observability 验收证据 | 故障注入下五类核心问题均可唯一定位 | `NOT_STARTED` |

## 12.2 M6 Gate Checklist

- [ ] 关键链路每个 Span 名称符合 `<package>.<FuncName>`。
- [ ] HTTP/RPC/Control 跨边界 Trace Context 正确传递。
- [ ] 日志可通过 `trace_id` 回到同一 Trace。
- [ ] Metrics 不使用 agent/instance/tunnel/connection ID 作高频 Label。
- [ ] 注入 Offline、Origin Down、No Capacity 和 Protocol Error 时，状态、日志、Metric 和 Trace 一致。

---

# 13. M7：Hardening + Alpha Release Gate

## 13.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M7-01 | Limits/Timeout/Rate Benchmark | M1-12、M3-10、M4-09 | `tests/benchmark` + 调优证据 | 只调整 Schema 默认值；不删除预算维度；记录 CPU/RAM/FD 环境 | `NOT_STARTED` |
| M7-02 | Reconnect Storm/Backoff/Fencing | M2-07、M6-02 | Chaos Test | 大量 Replica 重连无同步风暴；永久错误不快速重试；旧代无污染 | `NOT_STARTED` |
| M7-03 | Graceful Shutdown Chaos | M1-13、M4-10 | Server/Agent Drain Test | 每个 Drain 阶段丢包/延迟/对端消失；deadline 后无残留 FD/goroutine | `NOT_STARTED` |
| M7-04 | Persistence/Filesystem Failpoints | M0-05、M1-04、M3-05、M3-12 | Crash/EIO/Disk-full Suite | SQLite Migration、Gateway Rotation Journal、Transition Journal、TrustState、Snapshot、Backup/Restore 的 write/fsync/rename 断点；只做异常注入和恢复收敛，不首次实现维护命令 | `NOT_STARTED` |
| M7-05 | Race/Concurrency Suite | M2-08、M3-13、M4-10 | Race CI Job | `go test -race ./...`；Session Replacement、Config Write、Usage Flush、Listener Reconcile | `NOT_STARTED` |
| M7-06 | Protocol/Parser Fuzz | M05-10、M4-10 | `tests/fuzz` | UVarint/Frame/Envelope/WorkHello/Host/Path/Forwarded Header；Crash/OOM/无界分配为零 | `NOT_STARTED` |
| M7-07 | Goroutine/FD/Memory Leak | M1-14、M4-10 | Leak Test Harness | 连接 churn、Cancel、Reconnect、Drain 后回基线 | `NOT_STARTED` |
| M7-08 | Large Transfer/Privileged Network Chaos | M4-10 | Linux namespace + netem/nftables Suite | 1GB 上下行、Loss/Jitter/Reset/Half-Close；字节无丢失/重复 | `NOT_STARTED` |
| M7-09 | Release/Upgrade/Backup-Restore Matrix | M0-09、M3-12、M7-04 | Release Candidate Evidence | amd64/arm64 Binary/OCI/systemd；Upgrade/Migration/Backup/Restore/Agent Reconnect；仅验证 M3 已实现的维护命令 | `NOT_STARTED` |
| M7-10 | XTunnel Standalone Alpha Gate | M7-01至 M7-09 | Alpha 发布签核 | 下方所有发布 Gate 通过，无 P0/P1 未决项 | `NOT_STARTED` |

## 13.2 Alpha Release Gate Checklist

- [ ] 干净 checkout 完整 CI 通过。
- [ ] Unit、Integration、E2E、Contract、Golden、Race、Fuzz 全部通过。
- [ ] Privileged Network Chaos 与所有 Crash/Filesystem Failpoint 通过。
- [ ] amd64/arm64 Binary、OCI、systemd 安装/升级/卸载通过。
- [ ] SQLite Backup→Migration→Restore→Agent Reconnect 通过。
- [ ] 满负载和重连风暴下无负计数、超额资源、FD/goroutine 泄漏。
- [ ] 日志、镜像、配置、Backup 和测试 Fixture 中无 Secret。
- [ ] 已记录 Benchmark 环境、结果、推荐默认值和容量边界。
- [ ] 发布文档明确 Alpha 限制与 V0.1 不支持能力。

---

# 14. 当前可立即执行的任务队列

第一批只启动：

1. `M0-01` — 建立 Go Module 与目录骨架。

`M0-01` 完成后，第二批可并行：

1. `M0-02` — Config Schema 与加载器。
2. `M0-04` — 结构化日志基座。
3. `M0-06` — Proto 工具链骨架。
4. `M0-07` — OpenAPI 骨架。

`M0-05` 还依赖 `M0-02`，必须等 Config Schema 与加载器 `DONE` 后再启动。

推进规则：

- 单个开发者同时最多保持 2 个 `IN_PROGRESS` 任务。
- 单轮团队 WIP 不超过 5 个互不写冲突的任务。
- 同一批并行任务不得同时修改同一 Runtime 所有者文件、同一 Migration 序列或已冻结 Proto；这些变更必须串行。
- 任务进入 `REVIEW` 后优先完成复审和验收，不继续无限开新任务。
- Gate 失败时回到导致失败的最小任务，不通过放宽验收标准推进。

---

# 15. 开工决策与审批点

以下事项不需要在本计划阶段预先猜测，但必须在对应任务中留下可追溯决定：

| 决策 | 最晚完成点 | 要求 |
| --- | --- | --- |
| Go Module Path 与 Go 1.27.x 补丁版本 | M0-01 完成前 | `go.mod` 声明 `go 1.27`；选择稳定的精确补丁版本并同步到根/工具 Module、CI、OCI Builder 和版本检查；设置 `GOTOOLCHAIN=local`，禁止占位值、自动切换、旧版本回落和未记录升级 |
| SQLite Driver/GORM/Migration 方案 | M0-05 开工前 | 用户已明确要求数据库访问使用 GORM；开工时仍需记录 GORM、SQLite Driver 和 Migration 组件的精确版本与选择依据，Migration 保持显式 forward-only，不以 `AutoMigrate` 取代版本管理 |
| Buf/protoc-gen-go 精确版本 | M0-06 完成前 | 记录版本、下载源、分发包 SHA-256 和生成结果 |
| OpenAPI Validator/Generator | M0-07/M5-01 开工前 | 唯一命令入口，CI 不维护第二套方式 |
| Web 依赖与 Node 版本 | M0-08 开工前 | 版本和 Lockfile 受控；新增/升级依赖前先确认 |
| 首次 Buf Breaking Baseline | M05-04 完成前 | 显式记录“无历史前代”，禁止与当前文件自比较 |
| CI/arm64/Privileged Runner | M0-10/M7-08 开工前 | 记录 Runner 架构和权限；特权 Chaos 不得静默跳过 |

本文档不授权修改公共 API、新增第三方依赖、修改数据库 Schema 或生产配置。执行到对应任务时，仍须遵守项目的 Ask First 边界。

---

# 16. 执行记录

每个已完成或阻塞的任务按以下格式追加：

```markdown
## YYYY-MM-DD · TASK-ID · 状态

- 负责人：
- Commit/PR：
- 产物：
- 验收命令：
- 验收结果：
- 剩余风险：
- 解锁的后续任务：
```

## 2026-08-24 · PLAN-BASELINE · DONE

- 负责人：Codex
- 产物：本开发执行计划。
- 验收结果：已建立 M0—M7 的任务 ID、依赖、产物、验收要点、Gate 和进度基线。
- 剩余风险：开发命令只有在对应工程脚本落盘并经 CI 验证后，才能作为真实 Gate 证据。
- 解锁的后续任务：`M0-01`。

## 2026-08-24 · M0-01 · DONE

- 负责人：Codex
- Commit/PR：`ca19a19b8ac0`（本地提交，未推送）。
- 产物：以 `github.com/lifei6671/xtunnel` 初始化根 Module，固定 `go 1.27` / `go1.27.0`；建立 `cmd/server`、`cmd/agent` 与 `internal/{protocol,tunnel,transport,server,agent,repository}` 骨架；新增 PowerShell/POSIX 工具链检查入口。
- 验收命令：`$env:GOTOOLCHAIN='local'; ./tools/check-go-version.ps1`；错误模式快速失败检查；`gofmt`；`go test ./...`；`go vet ./...`；`git diff --check`。
- 验收结果：用户 Code Review 通过；在提交 `ca19a19b8ac0` 的干净工作区复验，本机 `go env GOVERSION=go1.27.0`、`go env GOTOOLCHAIN=local`；PowerShell 成功分支通过，`GOTOOLCHAIN=auto` 在调用 Go 前退出；Go 全包测试与 Vet 通过；无格式错误。
- 剩余风险：当前环境没有 POSIX `sh`，未执行 `check-go-version.sh`；该脚本需在后续 Linux CI 中补充运行证据，不阻塞 Windows 上已存在且通过的工具链检查入口。
- 解锁的后续任务：`M0-02`、`M0-04`、`M0-06`、`M0-07`；`M0-05` 仍等待 `M0-02`。

## 2026-08-24 · M0-02 · DONE

- 负责人：Codex
- Commit/PR：`2a6a40a00a1c5fa93584915719eb9a338d9f9f68`（本地提交，未推送）。
- 产物：Server/Agent Draft 2020-12 JSON Schema、内嵌 Schema、Config Struct、四层覆盖加载器、Go Duration 类型、边界与跨字段校验、Schema/Struct 漂移测试；依赖固定为 `go.yaml.in/yaml/v3 v3.0.5` 与 `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`。
- 验收命令：`$env:GOTOOLCHAIN='local'; ./tools/check-go-version.ps1`；`gofmt`；`go mod tidy`；`go test ./...`；`go vet ./...`；`git diff --check`；并尝试 `go test -race ./internal/config ./internal/server/config ./internal/agent/config`。
- 验收结果：用户 Code Review 通过；在包含实现提交 `2a6a40a00a1` 与 `go.sum` 校验和修正提交 `801699593047` 的干净工作区复验，本机 `go1.27.0` / `GOTOOLCHAIN=local` 检查通过；全包单元测试、Vet、格式和依赖整理通过；覆盖 Strict YAML、重复 Key、多文档、未知 YAML/Env/CLI、四层优先级、数组/Duration 解析、TLS 条件、关键跨字段关系以及 Schema/Struct/元数据一致性。
- 剩余风险：Race Suite 被本机 MSYS2 GCC 16.1.0 编译 Windows `runtime/cgo` 时的内部编译器错误阻断，当前环境无可用替代 C 编译器；本任务代码不包含并发或 Config Write，常规测试已通过，后续 CI 仍需补 Race 证据。`management.public_url/allowed_hosts` 的 IDNA 规范化属于后续 Management 边界实现，本任务只冻结并校验配置结构，未新增未经确认的 IDNA 依赖。
- 解锁的后续任务：`M0-03`、`M0-05`；`M0-04`、`M0-06`、`M0-07` 继续保持 `READY`。
