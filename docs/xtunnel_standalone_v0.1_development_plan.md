# XTunnel Standalone V0.1 开发执行计划

> **文档用途**：将《XTunnel Standalone 第一阶段完整技术方案 V0.1》转换为可执行、可推进、可验收的开发 Backlog
>
> **进度基线日期**：2026-08-25
>
> **当前阶段**：M0 工程初始化
>
> **当前结论**：`M0-01`、`M0-03` 至 `M0-08`、`M0-10`、`M0-11` 已完成；`M0-02` 的 Token-only Bootstrap 已有新的跨平台 CI 证据，等待用户复审；`M0-09` 已有 Linux systemd、Windows 提升权限 SCM、Linux amd64/arm64 原生 OCI Smoke 与跨平台 CI 证据，仍缺 Compose IPv4/IPv6 Runtime Smoke 及复审，因此保持实施中。

---

# 1. 文档权威与使用规则

本文档是 **开发任务、依赖、进度和验收证据** 的唯一跟踪入口，但不重复或取代技术契约。

权威顺序固定为：

1. 产品边界、架构和行为语义：[`docs/xtunnel_standalone_v0.1.md`](./xtunnel_standalone_v0.1.md)。
2. Protocol v1 Wire Contract：`api/proto/common.proto`、`control.proto`、`work.proto`。
3. Server 配置：`configs/server.schema.json`；Agent Bootstrap：本文与 `internal/agent/bootstrap` 的单 Connection Token 契约，精确 Token 编码在 M05-02 冻结。
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
4. 按任务级强依赖推进：只有被后续任务明确列为入口 Gate 的 Gate 任务 `DONE`，才可启动该后续里程碑；完整里程碑的最终 Gate 仍必须在最终发布前完成。
5. 阻塞项不得只写“待确认”，必须写清需要谁、提供什么、解锁哪些任务。

---

# 3. 总体关键路径

```text
M0 核心基础项（M0-01 至 M0-08、M0-10、M0-11）
 ↓
M0.5 Protocol v1 Contract Freeze
 ↓
M1 Secure TCP Data Plane Baseline
├──→ M2 Replica & Credential Lifecycle ──┐
└──→ M3 Configuration + Health ─────────┘
                    ↓
           M4 Product Data Plane
                                           ↓
                                  M5 REST API + Web
                                           ↓
                                  M6 Observability
                                           ↓
                                  M7 Hardening + Alpha Gate

M0-09 部署/包装验收 ──→ M0-12 完整 M0 Gate ──→ M7 Alpha Gate
```

依赖规则：

- M0.5 是 M1 Protocol Handler 的强制入口 Gate。
- M0-09 的 OCI/Compose、systemd 和 SCM 部署验收可在核心功能完成后推进；它不阻塞 M0.5 或 M1，但 M0-12 必须在 Alpha 发布 Gate 前完成。
- M2 和 M3 在 M1 Gate 后可并行，但 M4 产品数据面必须同时等待 M2 的 Replica Selection/Failover 和 M3 的 Tunnel/Binding/Snapshot 契约。
- M5 的 OpenAPI Entry Gate 可在 M4 后半段提前准备，Handler 和 Web 实现必须等 Gate 通过。
- M6 的 Logging/Metrics 骨架可从 M0/M1 纵向渗透，但 M6 Gate 要求完整产品链路可观测。
- M7 只调优和验证已存在的正确性边界，不允许第一次实现 M1/M3 应有的上限与恢复机制。

---

# 4. 进度仪表盘

| 里程碑 | 任务数 | 已完成 | 状态 | 入口依赖 | 退出 Gate |
| --- | ---: | ---: | --- | --- | --- |
| M0 工程初始化 | 12 | 9 | `IN_PROGRESS` | 技术方案基线 | M0-12 |
| M0.5 Protocol Freeze | 10 | 0 | `IN_PROGRESS` | M0-06 | M05-10 |
| M1 Secure TCP Baseline | 14 | 0 | `NOT_STARTED` | M05-10（各任务仍遵守其 M0 核心前置） | M1-14 |
| M2 Replica/Credential | 8 | 0 | `NOT_STARTED` | M1-14 | M2-08 |
| M3 Config/Health | 13 | 0 | `NOT_STARTED` | M1-14 | M3-13 |
| M4 Product Data Plane | 10 | 0 | `NOT_STARTED` | M2-08 + M3-13 | M4-10 |
| M5 REST API/Web | 11 | 0 | `NOT_STARTED` | M3-13 + M4-10（M5-01 可在 M4 后半段准备） | M5-11 |
| M6 Observability | 7 | 0 | `NOT_STARTED` | M5-11 | M6-07 |
| M7 Hardening/Alpha | 10 | 0 | `NOT_STARTED` | M2-08 + M3-13 + M4-10 + M5-11 + M6-07 | M7-10 |
| **合计** | **95** | **9** |  |  |  |

`M0=IN_PROGRESS` 只表示项目已进入该阶段，不表示其中任务已完成。

---

# 5. M0：工程初始化

## 5.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M0-01 | 建立 Go Module 与目录骨架 | 无 | `go.mod`、`cmd/server`、`cmd/agent`、`internal/*` 骨架 | `go.mod` 声明 `go 1.27` 并由 `toolchain` 记录稳定的精确 `go1.27.x` 版本；提供 `GOTOOLCHAIN=local` 的版本检查入口；`go test ./...`、`go vet ./...`；无空壳公共抽象 | `DONE` |
| M0-02 | Server Config Schema + Agent Token Bootstrap | M0-01 | `configs/server.schema.json`、Server Config、Agent Token Bootstrap CLI | Server Strict YAML 与 CLI>Env>YAML>Default；Agent 无 Schema/YAML，`run` 仅按 `--token`>`XTUNNEL_TOKEN`>OS Service Credential 取值；Linux 使用 systemd Credential，Windows SCM 使用 ProgramData DPAPI Machine-scope Credential；非空、无首尾空白、`xta_` 前缀、8192-byte 上限；未知 Flag/位置参数失败 | `IN_PROGRESS` |
| M0-03 | Server/Agent 进程骨架 | M0-01、M0-02 | `cmd/server/main.go`、`cmd/agent/main.go`、启停生命周期 | 两个进程均可启动；SIGTERM 退出且释放资源 | `DONE` |
| M0-04 | 结构化日志基座 | M0-01 | 共享 Logging 配置与 JSON Handler | 级别、时间、`request_id/trace_id` 字段稳定；无 Secret 输出 | `DONE` |
| M0-05 | Server Data Target/External Lock + SQLite/Migration | M0-01、M0-02 | Stable Target/External Lock、`migrations/`、`internal/repository/sqlite` | 数据库访问统一使用 GORM；必须先计算 Stable Data Target 并获取 Data Directory 外的同一把 Lock，再检查 Restore Journal/Open SQLite；双进程在触碰 DB/PKI 前拒绝；新库、幂等启动、中断 Migration 测试；引入依赖前先确认 | `DONE` |
| M0-06 | 锁定 Proto 工具链骨架 | M0-01 | `buf*.yaml`、`tools/versions.env`、`tools/go.mod`、`bootstrap-proto.sh`、`proto.sh` | `tools/go.mod` 与根 Module 使用相同 Go 1.27.x 工具链；`GOTOOLCHAIN=local` 构建 protoc-gen-go；Buf/protoc-gen-go 精确版本与分发包 SHA-256 可校验；不回落 PATH；三个 Wrapper 子命令可运行 | `DONE` |
| M0-07 | OpenAPI 骨架与校验 | M0-01 | `api/openapi/openapi.yaml`、校验入口 | 校验器选型/版本经依赖变更确认；OpenAPI Validate 通过；无占位 Server URL；CI 可执行漂移检查 | `DONE` |
| M0-08 | Web 工程、生产构建与 Go Embed | M0-01、M0-07 | `web/package*.json`、Vite/React 骨架、`web/embed.go` | `npm ci`、Web Build、Go Embed 通过；Lockfile 不由 CI 改写 | `DONE` |
| M0-09 | OCI/Compose 双栈、Server Shell Packaging 与跨平台 Agent Binary Self-install | M0-03、M0-08 | `deploy/docker`、Server-only `deploy/systemd`、Agent `service install/uninstall`、未接入启动路径的双栈监听原语 | OCI amd64/arm64、非 root、只读镜像、Server Data Volume + Runtime tmpfs；Agent 无 Volume，使用 `XTUNNEL_TOKEN` 且默认 `CMD ["run"]`。Linux Agent 在 root/systemd>=249 快速失败，原子安装 Binary、root-only Credential 与 Managed Unit；Windows Agent 支持 amd64/arm64、SCM、`NT AUTHORITY\LocalService`、ProgramFiles Binary 与 ProgramData DPAPI Machine-scope Credential，重复安装 Replace Existing + Write Through，Stop/Shutdown 最多 30s，异常返回非零并配置 non-crash recovery；两端持久启动项只含 Binary + `run` 且无 Secret，均以 managed marker 拒绝覆盖/删除非受管同名服务，卸载保留 Credential；Windows 自卸载按需延迟到重启删除运行中 EXE；Compose IPv4/IPv6、原生 tcp4/tcp6、完整 Smoke | `IN_PROGRESS` |
| M0-10 | CI 和跨平台构建矩阵 | M0-02至 M0-08 | CI Workflow | CI/OCI Builder 固定与 `go.mod toolchain` 一致的 `go1.27.x` 精确版本并设置 `GOTOOLCHAIN=local`；干净 checkout 中 Proto/Web/Go 顺序构建；Linux amd64/arm64 进程 Smoke。M0-09 的 Compose Runtime Smoke 仍由 M0-09 单独验收 | `DONE` |
| M0-11 | 首个 Admin Bootstrap | M0-03、M0-05 | `admin create`、`SETUP_REQUIRED`、本机 Bootstrap Socket/离线写入路径 | 无 Admin 时只启 Management；Server 运行时仅通过权限 `0600` 的本机 Socket 事务创建，停止时取得 External Lock 后写入；密码仅从 TTY/文件读取；重复创建拒绝 | `DONE` |
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
M0-02 至 M0-08 ── M0-10
M0-01 至 M0-11 + M0-10 ── M0-12（完整 M0 / 发布前 Gate）
```

## 5.3 M0 Gate Checklist

- [ ] 根 `go.mod` 声明 `go 1.27`，根/工具 Module、CI 和 OCI Builder 使用同一个稳定、精确的 `go1.27.x` 补丁版本；验收设置 `GOTOOLCHAIN=local`，并记录匹配的 `go env GOVERSION`/`GOTOOLCHAIN`。
- [ ] `go test ./...` 通过。
- [ ] `go vet ./...` 通过。
- [ ] Server Config Schema、Strict Decode 与覆盖优先级通过；Agent Token Bootstrap 三种来源、优先级、输入边界与 Secret 不回显测试通过。
- [ ] Server 在 External Lock 前不触碰 SQLite/PKI，第二进程快速失败。
- [ ] 全新库进入 `SETUP_REQUIRED`，运行中/离线 `admin create` 与重复拒绝测试通过。
- [ ] `./tools/proto.sh lint`、`breaking`、`generate-check` 的 M0 骨架流程可执行。
- [ ] `npm ci` 和 Web Production Build 通过，产物被 Go Embed。
- [ ] Linux Server/Agent amd64/arm64 与 Windows Agent amd64/arm64 Binary 可构建和启动。
- [ ] OCI/Compose Smoke、Server Shell Packaging Smoke、Linux Agent root/systemd>=249 Self-install 与 Windows Agent Administrator/SCM/LocalService/DPAPI Self-install 的 Managed Marker、权限、Secret、启动、重复安装 Replace Existing + Write Through、30s Stop/Shutdown、non-crash recovery、卸载及运行中 EXE 延迟删除失败边界通过。
- [ ] 干净 checkout CI 通过。

---

# 6. M0.5：Protocol v1 Contract Freeze

## 6.1 开工限制

M0.5 Gate 通过前，禁止开发 Server/Agent Protocol Handler。可并行开发不依赖 Wire Contract 的 Repository、Lock 骨架、Origin Dialer 和测试 Harness。

## 6.2 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M05-01 | 冻结 Common Types | M0-06 | `api/proto/common.proto` | package/go_package、enum 数值、reserved range、ErrorCode 完整 | `REVIEW` |
| M05-02 | 冻结 Connection Token + Auth/Control Contract | M05-01 | Connection Token v1 编码/解析契约、`api/proto/control.proto` | Token 仍是单个不透明 `xta_...`；冻结 Endpoint、TLS Trust、Agent/Token Identity、Secret 的精确编码/完整性/版本分派与失败语义；裸 Auth Frame、ControlEnvelope、Snapshot/ConfigAck、Health Batch 完整 | `REVIEW` |
| M05-03 | 冻结 Work Contract | M05-01 | `api/proto/work.proto` | WorkHello/Ready/Open/Response；RAW 切换；各状态唯一裸 Message | `REVIEW` |
| M05-04 | 生成代码与 Breaking Baseline | M05-01至 M05-03 | `internal/protocol/gen`、Buf Initial Baseline | 明确记录“首次冻结无历史前代”的 Baseline 建立方式，禁止与自身比较伪装 Breaking 证据；生成结果提交；`lint/breaking/generate-check` 通过 | `REVIEW` |
| M05-05 | Frame Codec 契约实现 | M05-04 | `internal/protocol/frame`、`codec` | UVarint 分片/合并、上限、EOF、Auth/Control/Work 分层测试 | `REVIEW` |
| M05-06 | 递归 Unknown Field 拒绝 | M05-04 | 共享 Validator 与表驱动测试 | Auth、Control、Work、Snapshot 全覆盖 | `REVIEW` |
| M05-07 | Deterministic Protobuf Bytes | M05-04、M05-06 | Snapshot/WorkHello 确定性字节构造器 | Snapshot 稳定排序并包含 Revision；WorkHello 清空 MAC 后重建已知字段；固定 Runtime 版本 | `REVIEW` |
| M05-08 | Protocol Golden Vectors | M05-02、M05-07 | `tests/golden/protocol-v1/*` | Connection Token v1、WorkHello、Snapshot 固定字节/Hash/HMAC；ConfigAck Revision 关联；测试不自动改 Fixture | `REVIEW` |
| M05-09 | 状态/方向/幂等契约测试 | M05-02、M05-03、M05-05 | Protocol State Test | Token 未知版本/畸形/超限/完整性失败；Auth 提交点、Control 非法方向、Work 直接关闭、ConfigAck/Drain 幂等 | `REVIEW` |
| M05-10 | M0.5 Protocol Gate | M05-01至 M05-09 | Protocol Freeze 证据 | 下方 Gate Checklist 全部通过；M0-09 部署验收和 M0-12 完整 M0 Gate 不阻塞本 Gate | `IN_PROGRESS` |

## 6.3 M0.5 Gate Checklist

- [x] `./tools/proto.sh lint` 通过。
- [x] `./tools/proto.sh breaking` 通过。
- [x] `./tools/proto.sh generate-check` 通过。
- [x] Golden Vector 逐字节比较通过。
- [x] Auth Success/Failure Transcript 及 Auth→Established 提交边界通过。
- [x] Connection Token v1 编码、解析、版本、完整性和语义字段 Golden Vector 通过。
- [x] Control/Work 方向、状态、乱序、重复、Unknown Field 全部测试通过。
- [x] Snapshot Deterministic Bytes、Revision 与 ConfigAck 关联/幂等测试通过。
- [x] Proto 变更已完成独立 Protocol Review。

---

# 7. M1：Secure TCP Data Plane Baseline

M1 只要求“一个逻辑 Agent + 一个 Instance + 一个静态 TCP Tunnel”，但必须使用正式身份、安全协议和真实资源上限。

身份链固定为“逻辑 Agent → 每次进程启动生成的 ephemeral Instance → 每次连接建立的 Session”。Agent 不持久化稳定机器身份、Instance 或 Session 身份，也不为这些运行态身份维护本地数据目录或锁。

## 7.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M1-01 | Agent/Connection Token 领域模型 | M0-05、M05-02 | Server Domain/Repository | Agent 状态、Token Identity/Version、Secret Hash、签发时连接描述；完整 Token 不落库；边界测试 | `NOT_STARTED` |
| M1-02 | Connection Token 创建与验证 | M1-01 | Application Service + Repository | 使用当前 Agent Gateway Endpoint/TLS Trust 签发单个 `xta_...`；CSPRNG、常量时间比较、一次性完整返回 | `NOT_STARTED` |
| M1-03 | Ephemeral Instance/Session 身份 | M1-01 | Agent 内存身份、Server Registry Key | 身份链为 Agent→ephemeral Instance→Session；Instance 每次进程启动生成且仅驻留内存，Session 每次连接生成；ID 格式、冲突拒绝、重启更换 Instance、Session generation fencing 通过；Agent 不维护本地身份状态或目录锁 | `NOT_STARTED` |
| M1-04 | Agent Gateway TLS/ALPN + Server Identity Rotation | M0-05、M0-11、M05-10 | Server Gateway、Token-derived Agent Dialer、`gateway rotate-key --maintenance`、Rotation Journal | 首个 Admin 完成前 Gateway 不启动；Agent 只从 Connection Token 取得 Endpoint/public-or-pinned Trust；TLS1.3；ALPN empty/unknown 拒绝；Handshake 上限；Server 停止并持 External Lock 时轮换；新 Pin 只进入后续新 Token；Journal 恢复与私钥 `0600` | `NOT_STARTED` |
| M1-05 | Connection Token Auth 与 Control Session 建立 | M1-02至 M1-04 | Auth Handler、Session Secret、Session Registry | Token 连接描述/身份/Secret 作为整体校验；Auth Failure 可区分；Success flush 提交点；generation fencing | `NOT_STARTED` |
| M1-06 | AgentRuntime 所有权与线性化 | M1-03、M1-05 | Runtime Registry、ActiveWork | 固定 Lock 规则；锁内无 IO/Close/阻塞；计数 exactly-once | `NOT_STARTED` |
| M1-07 | Control Session Owner/Outbox | M1-05、M1-06 | Single Reader/Writer/Owner、有界队列 | 优先级、合并、Snapshot/ConfigAck 有序、队列满关闭、无 goroutine leak | `NOT_STARTED` |
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
| M2-01 | Multi-Replica Runtime Registry | M1-14 | 多 Instance Registry | 同一逻辑 Agent/Token 可连接多个 ephemeral Instance；独立 Session/Pool/Counter | `NOT_STARTED` |
| M2-02 | Instance Selection Baseline | M2-01 | Selection Strategy | 只按 Current Session、非 DRAINING、Idle/Capacity 过滤后 Least Active + RR tie-break；并发公平性测试；Revision/Health Eligible 留给 M3-09 接入 | `NOT_STARTED` |
| M2-03 | Online Instance Lifecycle/Observability | M1-03、M2-01 | Runtime Lifecycle Events + Query/Metrics | 连接、Session Replacement、DRAINING、断开状态可查询并有结构化日志/指标；Agent 重启产生新 Instance；不维护机器/主机历史 | `NOT_STARTED` |
| M2-04 | Token Rotate/Revoke | M1-02、M2-01 | Credential Lifecycle Service | Rotate 使用当前 Endpoint/TLS Trust 签发新 Connection Token 并进入并存期；Revoke 新认证失败；完整 Token 不落库/日志 | `NOT_STARTED` |
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

# 9. M3：Configuration + Health

## 9.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M3-01 | Tunnel/Binding 领域与存储 | M1-14、M0-05 | Domain + SQLite Repository | 不变量、唯一性、引用关系和容量边界 | `NOT_STARTED` |
| M3-02 | Application Service + Version Transaction | M3-01 | Server Application Service | Service Aggregate 修改在单事务递增 version/revision；并发写不丢失 | `NOT_STARTED` |
| M3-03 | Snapshot Builder/Size Gate | M3-02 | AgentSnapshot Builder | 稳定排序、绑定数/字节上限在事务前校验 | `NOT_STARTED` |
| M3-04 | Agent In-Memory Atomic Apply | M3-03、M05-08、M1-07 | Agent Config Runtime + ConfigAck | 完整 Snapshot 校验成功后原子替换内存配置；Apply 失败保留当前运行 Revision 并返回稳定错误；不写 Agent 配置或信任状态文件 | `NOT_STARTED` |
| M3-05 | Token-only Startup/Reconnect + Remote Config | M3-04、M1-05 | Agent Bootstrap/Reconnect Integration | Agent 仅凭 Connection Token 建连；每次启动或重连从 Server 获取完整 Desired Snapshot；Apply 成功并 Ack 前不进入 Eligible；Server 不可达时不上线且无本地配置回退 | `NOT_STARTED` |
| M3-06 | Snapshot Reconcile/Observed Revision | M3-03至 M3-05、M1-07 | Reconciler + ConfigAck | 过期 Revision 拒绝；高 Revision 合并；完整 Apply 的 Ack 后才 Eligible；重连重新获取完整 Snapshot | `NOT_STARTED` |
| M3-07 | Origin Resolver | M3-03、M3-04 | Agent Origin Resolver | 仅从当前已原子 Apply 的内存 Snapshot 解析 HTTP/HTTPS/TCP、DNS/IPv4/IPv6、TLS Server Name 与 SSRF 边界 | `NOT_STARTED` |
| M3-08 | 中心 Health Scheduler | M3-07 | Heap/Wheel Scheduler + Semaphores | 全局/per-origin 并发、Rate、initial/interval jitter；无 per-binding ticker | `NOT_STARTED` |
| M3-09 | Health Batch/Revision Fencing + Eligible Selection | M3-06、M3-08、M2-02 | Pending Accumulator、Batch Reporter、完整 Instance Eligible Filter | `tunnel_id` 合并；出队分配 generation；将 required/observed Revision 和 Per-Tunnel Health 接入 M2 Selection；旧 Revision Health 不放行 | `NOT_STARTED` |
| M3-10 | Health Target Budget Manager | M3-01、M3-08、M2-06 | Reserve/Commit/Release Manager | `(agent_id,instance_id)` 所有权；固定锁顺序；重连不双计费/误释放 | `NOT_STARTED` |
| M3-11 | Agent/Instance/Service Status | M3-06、M3-09、M3-10 | `internal/server/status` | 状态优先级唯一；Origin Health 不污染 Agent/Instance；Web 不重算 | `NOT_STARTED` |
| M3-12 | Durable Operations：Backup/Restore | M0-05、M1-04、M3-02 | `backup create/restore`、Backup Manifest、Restore Journal | 在线 Create 通过本机控制通道建立 Config Write Barrier；离线 Create/Restore 使用同一 Stable Target External Lock；备份 SQLite + Gateway TLS Identity；Manifest/Hash/Schema 校验；同盘 staging/rollback/journal 可恢复 | `NOT_STARTED` |
| M3-13 | M3 Gate | M3-01至 M3-12 | Application Service Integration + Server Durable Operation Crash Tests | 下方 Checklist 全部通过 | `NOT_STARTED` |

## 9.2 M3 Gate Checklist

- [ ] 通过 Application Service 修改 Origin，Agent 无需重启即生效。
- [ ] Snapshot 的 Deterministic Bytes、Revision、大小和 Binding 边界均可自动化验证。
- [ ] Agent 完整校验后原子替换内存配置；失败保留当前 Revision 并返回明确 ConfigAck。
- [ ] Agent 启动/重连必须拉取完整 Desired Snapshot；Server 不可达时不上线且无本地配置回退。
- [ ] Health Rate/Concurrency/Jitter/Batch/Revision Fencing 通过。
- [ ] 超过 Agent/Global Health Target Budget 的 Config Write 和 Replica Auth 被拒绝。
- [ ] 满容量 Session Replacement 不 Double Reserve，旧 cleanup 不释放新 Reservation。
- [ ] `backup create/restore` 在线/离线路径通过，Manifest 覆盖 SQLite 与 Gateway TLS Identity，Restore 不与旧目录合并；Server Journal 在各提交点崩溃后可恢复。

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
| M6-01 | 全链路 JSON Logging | M1-14、M5-11 | 稳定日志字段 | request/trace/session/connection 可关联；Secret 脱敏；级别正确；Windows SCM 模式提供可持久检索的 Event Log Source 或等价受支持 Sink，不能仅依赖不保证可见的 stderr | `NOT_STARTED` |
| M6-02 | Prometheus Metrics | M4-10 | `/metrics` + Metric Registry | 请求数/错误率/P50/P99、Session/Pool/Limit/Health；低基数 Label | `NOT_STARTED` |
| M6-03 | OpenTelemetry Trace | M4-10 | Server→Agent Trace Propagation | `ingress.Accept→tunnel.DialContext→transport.Acquire→origin.Dial→proxy.Bidirectional` 可关联 | `NOT_STARTED` |
| M6-04 | Usage Aggregation | M4-10、M0-05 | Usage Buffer/Flush/Repository | 字节/连接计数 exactly-once；Batch Flush；重启无负数/重复 | `NOT_STARTED` |
| M6-05 | Error/Status Observability | M3-11、M6-01、M6-02 | Error Code Dashboard Data | Agent Offline/Replica Offline/Origin Down/No Capacity/Protocol Error 可区分 | `NOT_STARTED` |
| M6-06 | 运维诊断流程 | M6-01至 M6-05 | Runbook + Dashboard | 从报警可定位到状态、Metric、Trace 和日志；覆盖 Linux systemd 与 Windows SCM 的启动失败、恢复重启和 30s Stop/Shutdown 超时诊断 | `NOT_STARTED` |
| M6-07 | M6 Gate | M6-01至 M6-06 | Observability 验收证据 | 故障注入下五类核心问题均可唯一定位 | `NOT_STARTED` |

## 12.2 M6 Gate Checklist

- [ ] 关键链路每个 Span 名称符合 `<package>.<FuncName>`。
- [ ] HTTP/RPC/Control 跨边界 Trace Context 正确传递。
- [ ] 日志可通过 `trace_id` 回到同一 Trace。
- [ ] Metrics 不使用 agent/instance/tunnel/connection ID 作高频 Label。
- [ ] 注入 Offline、Origin Down、No Capacity 和 Protocol Error 时，状态、日志、Metric 和 Trace 一致。
- [ ] Windows SCM 模式的启动、运行回调异常、恢复重启与 Stop/Shutdown 超时均可从持久日志入口定位；JSON stderr 不能作为唯一证据。

---

# 13. M7：Hardening + Alpha Release Gate

## 13.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M7-01 | Limits/Timeout/Rate Benchmark | M1-12、M3-10、M4-09 | `tests/benchmark` + 调优证据 | 只调整 Schema 默认值；不删除预算维度；记录 CPU/RAM/FD 环境 | `NOT_STARTED` |
| M7-02 | Reconnect Storm/Backoff/Fencing | M2-07、M6-02 | Chaos Test | 大量 Replica 重连无同步风暴；永久错误不快速重试；旧代无污染 | `NOT_STARTED` |
| M7-03 | Graceful Shutdown Chaos | M1-13、M4-10 | Server/Agent Drain Test | 每个 Drain 阶段丢包/延迟/对端消失；deadline 后无残留 FD/goroutine | `NOT_STARTED` |
| M7-04 | Server Persistence/Filesystem Failpoints | M0-05、M1-04、M3-12 | Crash/EIO/Disk-full Suite | Server SQLite Migration、Gateway Rotation Journal、Backup/Restore 的 write/fsync/rename 断点；只验证 Server durable operation 的异常注入和恢复收敛，不首次实现维护命令 | `NOT_STARTED` |
| M7-05 | Race/Concurrency Suite | M2-08、M3-13、M4-10 | Race CI Job | `go test -race ./...`；Session Replacement、Config Write、Usage Flush、Listener Reconcile | `NOT_STARTED` |
| M7-06 | Protocol/Parser Fuzz | M05-10、M4-10 | `tests/fuzz` | UVarint/Frame/Envelope/WorkHello/Host/Path/Forwarded Header；Crash/OOM/无界分配为零 | `NOT_STARTED` |
| M7-07 | Goroutine/FD/Memory Leak | M1-14、M4-10 | Leak Test Harness | 连接 churn、Cancel、Reconnect、Drain 后回基线 | `NOT_STARTED` |
| M7-08 | Large Transfer/Privileged Network Chaos | M4-10 | Linux namespace + netem/nftables Suite | 1GB 上下行、Loss/Jitter/Reset/Half-Close；字节无丢失/重复 | `NOT_STARTED` |
| M7-09 | Release/Upgrade/Backup-Restore Matrix | M0-09、M3-12、M7-04 | Release Candidate Evidence | Linux amd64/arm64 Binary/OCI/systemd 与 Windows Agent amd64/arm64 Binary/SCM；前台 `run --token`、OCI `XTUNNEL_TOKEN` + 默认 `run`、Linux systemd LoadCredential、Windows ProgramData DPAPI Machine-scope Credential；两端 Agent Binary `service install/uninstall` 的安装/升级/卸载覆盖 Managed Marker、Binary 替换、Secret 不落 SCM/argv 和非托管 Unit/Service 拒绝边界；Windows 覆盖运行中 EXE 的 Replace Existing/Write Through 与 Self-uninstall `DELAY_UNTIL_REBOOT` 收敛；Upgrade/Migration/Backup/Restore 后 Agent 仅凭 Token 重连并重新获取完整配置；仅验证 M3 已实现的维护命令 | `NOT_STARTED` |
| M7-10 | XTunnel Standalone Alpha Gate | M0-12、M7-01至 M7-09 | Alpha 发布签核 | 下方所有发布 Gate 通过，无 P0/P1 未决项 | `NOT_STARTED` |

## 13.2 Alpha Release Gate Checklist

- [ ] 干净 checkout 完整 CI 通过。
- [ ] Unit、Integration、E2E、Contract、Golden、Race、Fuzz 全部通过。
- [ ] Privileged Network Chaos 与所有 Server Durable Operation Crash/Filesystem Failpoint 通过。
- [ ] Linux amd64/arm64 Binary/OCI/Server Shell Packaging/systemd Agent 与 Windows Agent amd64/arm64 SCM Self-install/升级/卸载通过，Windows 延迟到重启的 EXE 删除最终收敛，且非托管 Unit/Service 不被覆盖或删除。
- [ ] SQLite Backup→Migration→Restore→Agent Reconnect 并重新获取完整配置通过。
- [ ] 满负载和重连风暴下无负计数、超额资源、FD/goroutine 泄漏。
- [ ] 日志、镜像、Server 配置、systemd Unit/ExecStart、Windows SCM ImagePath/Registry、Backup 和测试 Fixture 中无 Connection Token/Secret；ProgramData 只保留 ACL 受限的 DPAPI Machine-scope 密文。
- [ ] 已记录 Benchmark 环境、结果、推荐默认值和容量边界。
- [ ] 发布文档明确 Alpha 限制与 V0.1 不支持能力。

---

# 14. 当前可立即执行的任务队列

当前 `M0-01`、`M0-03` 至 `M0-08`、`M0-10` 已完成。按“核心功能先行、部署验收后置”的任务级依赖约定，当前待办为：

1. `M05-01` — Common Types 已完成验收，等待用户 Protocol Review；通过后进入 `M05-02` 和 `M05-03`。
2. `M0-02` — Server Config Schema + Agent Token Bootstrap，新的跨平台 CI 已通过，等待用户复审；它仍是完整 M0 Gate 与 M1 资源配置接入的核心前置。
3. `M0-09` — Token-only Agent 的 OCI/Compose 与跨平台 Binary Self-install；Linux systemd、Windows SCM 及 Linux amd64/arm64 OCI Smoke 已通过，仍等待 Compose IPv4/IPv6 Runtime Smoke 与复审。该部署专项不阻塞 M0.5/M1。

`M0-10` 已于 2026-08-25 经用户 Code Review 通过并标记 `DONE`。`M0-11` 已取得用户复审及原生 Linux CI Race 证据，现为 `DONE`。`M0-02` 因删除 Agent Schema/Loader 并改写 Bootstrap 输入而保持 `IN_PROGRESS`，只等待用户复审；`M0-09` 保持 `IN_PROGRESS`，只等待 Compose 双栈 Runtime Smoke 与复审。完整 M0 的 `M0-12` 维持为发布前 Gate；M0.5 和 M1 仅遵守各自表格中列出的核心任务依赖。

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
| OpenAPI Validator/Generator | M0-07/M5-01 开工前 | M0-07 已批准并锁定 vacuum `v0.30.0` 官方 Linux amd64/arm64 归档与二进制 SHA-256，唯一入口为 `tools/openapi.sh validate`；M5-01 首次引入 Generator 前仍需单独确认，CI 不维护第二套方式 |
| Web 依赖与 Node 版本 | M0-08 开工前 | 已批准 Node `24.19.0`、npm `11.17.0`、React/React DOM `19.2.8`、Vite `8.2.2`、Plugin React `6.1.0`、TypeScript `6.0.2` 与对应类型包；用户在管理菜单出现真实图标需求后追加批准 `lucide-react 1.34.0`；直接依赖精确锁定，npm 11 生成并提交 Lockfile，CI 只运行 `npm ci`；Tailwind/shadcn/Router/Query 等继续等待 M5 真实使用点 |
| OCI 基础镜像、Compose 双栈与跨平台 Agent Service 权限模型 | M0-09 开工前 | 已批准三个固定多架构基础镜像摘要、Compose 双栈 Profile 与原生 tcp4/tcp6 监听原语；OCI 使用 `65532:65532` 与只读根，只有 Server 挂载 Data Volume 和 `/run/xtunnel` tmpfs，Agent 无 Volume，从 `XTUNNEL_TOKEN` 取得 Token 并默认执行 `run`；Compose 输入 `XTUNNEL_AGENT_TOKEN` 映射到容器环境；Server 保留 Shell 包装。Agent 在 Linux/Windows 统一使用 Binary `service install --token` 与 `service uninstall`，不提供用户安装脚本。Linux 要求 root/systemd>=249，原子安装到 `/usr/local/bin/xtunnel-agent`，Credential 目录/Source 为 `root:root 0700/0600`，Unit 首行为 `# Managed by xtunnel-agent service install` 且 `ExecStart=/usr/local/bin/xtunnel-agent run`。Windows 支持 amd64/arm64，要求提升权限的 Administrator 与 SCM；ServiceName=`XTunnelAgent`、DisplayName=`XTunnel Agent`、账户=`NT AUTHORITY\LocalService`，Binary=`%ProgramFiles%\XTunnel\xtunnel-agent.exe`，Credential=`%ProgramData%\XTunnel\credentials\agent.token.dpapi` 并使用 `CRYPTPROTECT_LOCAL_MACHINE | CRYPTPROTECT_UI_FORBIDDEN`，SCM ImagePath 仅含安装 Binary + `run`，Description marker 精确为 `Managed by xtunnel-agent service install`；重复安装使用 `MoveFileEx(REPLACE_EXISTING | WRITE_THROUGH)`，Stop/Shutdown 最多 30s，运行异常返回非零并配置 non-crash recovery。两端均拒绝覆盖/删除非受管同名服务，卸载删除受管服务并保留平台 Credential；Windows 从运行中已安装 EXE 自卸载时使用 `MoveFileEx(DELAY_UNTIL_REBOOT)` 安排重启删除 Binary，Linux 另保留服务用户 |
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
- 剩余风险：后续 M0-03 验收中已重跑并通过配置包 Race Suite，先前的本机 GCC 临时故障已解除。`management.public_url/allowed_hosts` 的 IDNA 规范化仍属于后续 Management 边界实现，本任务只冻结并校验配置结构，未新增未经确认的 IDNA 依赖。
- 解锁的后续任务：`M0-03`、`M0-05`；`M0-04`、`M0-06`、`M0-07` 继续保持 `READY`。

## 2026-08-24 · M0-03 · DONE

- 负责人：Codex
- Commit/PR：`93c4d5c3142aa70a3f781a1def793f64f0bd2e1d`（本地提交，未推送）。
- 产物：`xtunnel-server`、`xtunnel-agent` 前台进程入口；`--config`/可重复 `--set` 配置 CLI；`SIGINT`/`SIGTERM` 到 Context 的生命周期桥接；命令行、配置失败、Context 取消和非 Windows 真实 SIGTERM 测试。
- 验收命令：`$env:GOTOOLCHAIN='local'; ./tools/check-go-version.ps1`；`gofmt`；`go test ./...`；`go test -race ./cmd/server ./cmd/agent`；`go test -cover ./cmd/server ./cmd/agent`；`go vet ./...`；Linux amd64 `go test -c`/`go build` 交叉编译；WSL `timeout --preserve-status --signal=TERM 1s` 双进程 Smoke；`git diff --check`。
- 验收结果：用户 Code Review 通过；在提交 `93c4d5c3142a` 的干净工作区复验，Go 1.27.0 本地工具链检查、全包测试、命令包 Race、Vet 和 Linux amd64 交叉编译通过；Server/Agent 命令包语句覆盖率均为 75.0%；两个交叉编译的 Linux Binary 在 WSL 中保持前台运行并在真实 SIGTERM 后以退出码 0 结束。
- 剩余风险：WSL Ubuntu 未安装 Go，非 Windows 的测试源码已完成 Linux amd64 编译但未在 WSL 内执行 `go test`；M0-10 仍需在原生 Linux amd64/arm64 CI 中执行完整测试与进程 Smoke。本任务没有真实 Listener、Session 或数据库资源，完整有界 Drain 由 M1-13 实现。
- 解锁的后续任务：`M0-09` 继续等待 `M0-08`；`M0-11` 继续等待 `M0-05`。可独立推进的下一任务为 `M0-04`。

## 2026-08-24 · M0-04 · DONE

- 负责人：Codex
- Commit/PR：`11d9e760ae4b48300ec50eb74a92121fb1153a3e`（本地提交，未推送）。
- 产物：`internal/logging` 共享 `log/slog` JSON Handler、日志级别映射、UTC RFC3339Nano 时间、稳定 `component/event/request_id/trace_id` 字段、明确 Secret 属性脱敏；Server/Agent 已接入启动与停止事件。
- 验收命令：`$env:GOTOOLCHAIN='local'; ./tools/check-go-version.ps1`；`gofmt`；`go mod tidy`；`go test ./...`；`go test -race ./internal/logging ./cmd/server ./cmd/agent`；`go test -cover ./internal/logging ./cmd/server ./cmd/agent`；`go vet ./...`；`git diff --check`。
- 验收结果：用户 Code Review 通过；本机 `go1.27.0` / `GOTOOLCHAIN=local` 检查通过；全包测试、定向 Race、Vet、格式和依赖整理通过；日志包语句覆盖率 100.0%，Server/Agent 命令包均为 75.9%。测试按行解析 JSON，覆盖四级阈值、字段规范化、真实关联 ID 注入、空 ID 不写入、顶层与嵌套 Secret 脱敏以及双进程生命周期事件。
- 剩余风险：属性键脱敏不能识别被调用方拼入 `event`、错误文本或任意对象内部的 Secret，因此日志 API 注释和技术方案明确禁止这些调用方式；全链路日志审计与真实 Trace Context 注入仍由 `M6-01`、`M6-03` 完成。当前未引入 Gin、OpenTelemetry、日志轮转或第三方日志依赖。
- 解锁的后续任务：可进入 `M0-05`；该任务首次引入 GORM/SQLite 依赖并新增数据库 Schema，必须先取得明确确认。

## 2026-08-24 · M0-05 · DONE

- 负责人：Codex。
- Commit/PR：`8c883b0ef2428548e98ed6cb99f65150f7571d81`（本地提交，未推送）；原实现与 Bootstrap 结构调整均已通过用户 Code Review。
- 产物：Stable Data Target 解析与 Canonical 校验；Linux `/run/xtunnel/server-lock-<sha256>.lock` 非阻塞 External Lock；Pending Restore Journal 启动前检测；基于 `gorm.io/gorm v1.31.2` 和纯 Go `github.com/libtnb/sqlite v1.2.2` 的 SQLite Store；`schema_migrations(version, applied_at)` 首个显式 forward-only Migration；Server 启停顺序与资源释放接入。选择纯 Go Driver 是为了保持 Linux amd64/arm64 的 `CGO_ENABLED=0` 构建，不引入交叉 C 工具链。
- 验收命令：`$env:GOTOOLCHAIN='local'; ./tools/check-go-version.ps1`；`gofmt`；`go mod tidy`；`go mod verify`；`go test ./...`；`go test -race ./internal/server/datadir ./internal/repository/sqlite ./cmd/server`；`go test -cover ./internal/server/datadir ./internal/repository/sqlite ./cmd/server`；`go vet ./...`；`git diff --check`；`CGO_ENABLED=0` 的 Linux amd64/arm64 Test Binary 与 Server/Agent Binary 交叉编译；WSL 执行 Linux amd64 Data Target、External Lock、SQLite、Server、Agent Test Binary，并以普通 Runtime UID 与 root 分别执行 External Lock Suite。
- 验收结果：用户已在开工前确认 GORM/SQLite/x/sys 精确依赖、间接依赖与首个数据库 Schema；Go 1.27.0 本地工具链、全包测试、定向 Race、Vet、格式、依赖校验和跨架构纯 Go 编译通过；WSL Linux amd64 测试全部通过，覆盖跨进程抢锁、锁前不触碰数据库、Journal 先于 leaf、root 离线维护复用锁、多物理连接 PRAGMA/外键、全新库、幂等启动、高版本拒绝、SQL 错误/Context 取消原子回滚和修复后继续 Migration。三路独立只读复审均无剩余阻断。
- 剩余风险：M0-05 只检测 Pending Restore Journal 并安全拒绝，正式完成/回滚状态机由 `M3-12` 实现；Linux arm64 当前只有交叉编译证据，原生运行由 `M0-10` CI 矩阵补齐；`/run/xtunnel` 的 systemd/OCI 创建由 `M0-09` 实现；V0.1 Windows 不提供生产 External Lock；当前 Schema 仅包含 Migration 账本，领域表由后续任务逐步添加。
- 解锁的后续任务：`M0-11` 的 M0-05 依赖已完成；当前按串行开发约定进入 `M0-07`。

## 2026-08-24 · M0-06 · DONE

- 负责人：Codex。
- Commit/PR：`8c883b0ef2428548e98ed6cb99f65150f7571d81`（本地提交，未推送）；用户 Code Review 已通过。
- 产物：Buf v2 Module/Generate 配置（使用 `clean: true` 清理纯生成目录）；空 `api/proto` 骨架；`STANDARD` Lint（仅排除与冻结平铺路径冲突的 `PACKAGE_DIRECTORY_MATCH`）与 `FILE` Breaking Policy；固定 Buf `v1.72.0`、`protoc-gen-go v1.36.12`、Linux amd64/arm64 官方分发包 SHA-256 的 `tools/versions.env`；使用 `go 1.27` / `toolchain go1.27.0` 与 Go `tool` directive 的独立 `tools` Module；只使用 `.tools/bin` 的 POSIX Bootstrap 和 `lint`、`breaking`、`generate-check` Wrapper；`.tools` 忽略规则与 Shell LF 属性。当前没有外部 Proto module 依赖，Buf 明确不生成 `buf.lock`，未手写空文件。
- 验收命令：`$env:GOTOOLCHAIN='local'; ./tools/check-go-version.ps1`；`tools/go.mod` 下 `go mod tidy`、`go mod verify` 与 `go build -mod=readonly`；WSL 中首次/二次 `bootstrap-proto.sh`；Buf/Generator 版本、amd64/arm64 SHA-256 与 Go Build Metadata 检查；`sh -n`、`dash -n`、`bash --posix -n`；`proto.sh lint|breaking|generate-check`；真实临时 Proto Lint、无 Baseline 硬失败、生成漂移和孤立 `*.pb.go` 失败用例；伪造 PATH 工具隔离；根 Module `go test ./...`、`go vet ./...`；`git diff --check`。
- 验收结果：根/工具 Module 均固定 Go 1.27.0 且 `GOTOOLCHAIN=local`；工具 Module 校验和通过，Linux amd64 Generator 由固定 Module 构建并运行，Linux arm64 Generator 交叉构建通过；两个 Buf 官方发行包 SHA-256 均实测匹配。Bootstrap 首次安装与第二次幂等运行通过；三个空骨架命令按契约输出 `SKIP`，不宣称 Protocol Gate PASS。真实临时 Proto 通过 Lint，Breaking 在无初始 Baseline 时拒绝，Generate Check 检出 staged/untracked 漂移和孤立生成物，`clean: true` 实测删除旧生成物，伪造 PATH 未被使用；根 Module 全包测试和 Vet 通过。
- 剩余风险：WSL 未原生安装 Go，本次 Bootstrap 使用受控的 Windows Go 1.27.0 互操作入口构建 Linux amd64 Generator；原生 Linux amd64/arm64 干净 checkout 由 `M0-10` CI 补齐。当前环境没有 ShellCheck，已完成三种 Shell 语法检查但未运行 `shellcheck -s sh`。`buf.lock` 要等首次真实外部 Proto module 依赖出现后由 Buf 生成。Windows Git 配置不跟踪 file mode，提交时必须把 `bootstrap-proto.sh`、`proto.sh` 和既有 `check-go-version.sh` 记录为 `100755`。
- 解锁的后续任务：可进入 `M0-07`；`M05-01` 的 M0-06 依赖已完成。

## 2026-08-24 · M0-03/M0-05 · 结构复审通过

- 负责人：Codex。
- Commit/PR：`8c883b0ef2428548e98ed6cb99f65150f7571d81`（本地提交，未推送）；用户 Code Review 已通过。
- 产物：`cmd/server` 与 `cmd/agent` 均只保留单一 `main.go` 进程入口；CLI、配置装载、日志、信号生命周期和资源编排下沉到 `internal/server/bootstrap`、`internal/agent/bootstrap`；Server 的 Stable Data Target、External Lock、Restore Journal、Canonical Data Directory 与 GORM/SQLite 启动顺序保持不变。
- 验收命令：`gofmt`；`go test ./internal/agent/bootstrap ./internal/server/bootstrap ./cmd/agent ./cmd/server`；`go test ./...`；`go test -race ./internal/agent/bootstrap ./internal/server/bootstrap`；`go test -cover ./internal/agent/bootstrap ./internal/server/bootstrap`；`go vet ./...`；Linux amd64 `CGO_ENABLED=0` Binary/Test Binary 交叉编译；WSL 执行两组 Test Binary，并对实际 Server/Agent Binary 执行真实 `SIGTERM` Smoke。
- 验收结果：命令目录仅剩两个 `main.go`，依赖方向固定为 `cmd → internal/*/bootstrap`；迁移后的单元测试、Race、Vet、Linux Test Binary 和真实进程 Smoke 全部通过。Agent/Server 均输出 `process_started`、收到 `SIGTERM` 后输出 `process_stopped` 并以退出码 0 结束；Server 实际创建 `xtunnel.db` 和 Data Directory 外的 External Lock。
- 状态影响：本次仅修正既有 M0-03/M0-05 的代码归属，不改变公共 API、Protocol、Config、数据库 Schema 或 Gate；结构复审通过后，M0-05 与 M0-06 进入 `DONE`，下一任务为 `M0-07`。

## 2026-08-24 · M0-07 · DONE

- 负责人：Codex。
- Commit/PR：`fc6ab176070f4eb3120f4605c7b895494c144a56`（实现）、`f5c7e5f73b8142b0a3a34faa9006b526954529be`（WSL 可执行文件隔离修复）与 `a7559e2`（验收证据同步），均为本地提交、未推送；用户 Code Review 已通过。
- 产物：OpenAPI `3.1.0` 可校验骨架；唯一同源 Server 基路径 `/api/v1`；基于 vacuum `v0.30.0` Recommended Ruleset 的项目规则，机器锁定 OpenAPI 方言、唯一 Server、基路径和无 Server Variables；固定 Linux amd64/arm64 官方归档与解压后二进制 SHA-256 的 `tools/versions.env`；只安装到 `.tools/bin` 的 POSIX Bootstrap；唯一 `validate` Wrapper 与隔离负例测试；README 中的 Windows/WSL 使用方式。当前没有业务 Endpoint、DTO、Generator 或 Gin 依赖，避免提前承担 M5 Contract。
- 验收命令：`$env:GOTOOLCHAIN='local'; go env GOVERSION; go env GOTOOLCHAIN; ./tools/check-go-version.ps1`；根/工具 Module `go mod verify`；根 Module `go test ./...`、`go vet ./...`；工具 Module `go build -mod=readonly ./...`；WSL 中 `bootstrap-openapi.sh` 首次安装与二次幂等运行；`sh -n`、`dash -n`、`bash --posix -n`；`openapi.sh validate`；`test-openapi.sh`；vacuum 版本、官方归档与二进制 SHA-256 独立核验；`git diff --check`。
- 验收结果：用户已在开工前确认 vacuum `v0.30.0` 依赖和 OpenAPI `3.1.0` 方言，且本轮 Code Review 已通过；本机工具链为 `go1.27.0` 且 `GOTOOLCHAIN=local`，根 Module 测试/Vet 和根/工具 Module 校验通过；Canonical Validate 得分 `100/100`。隔离测试覆盖方言漂移、占位/错误基路径、多 Server、Server Variables、Malformed YAML、未解析 `$ref`、工具篡改、PATH 伪工具、未知命令的稳定退出码和安装归档校验失败不覆盖旧工具；Wrapper 固定用法错误退出码 2、校验失败退出码 1。提交态复验发现并修复 WSL/NTFS 对刚执行二进制的短暂占用，篡改测试改用从未执行过的独立副本，不增加 sleep 或重试；连续三轮回归和最终干净 Checkout 复验通过，工作区无漂移。
- 剩余风险：当前环境没有 ShellCheck，未执行 `shellcheck -s sh`；Linux arm64 已独立核验官方归档与二进制哈希但未原生执行，由 `M0-10` CI 矩阵补证。M0-07 只提供 CI 可调用的校验入口，尚未创建 CI Workflow；M5 OpenAPI Breaking、Generated Contract Drift、真实 Handler/Client 零漂移及 Gin 版本确认均未开始。
- 解锁的后续任务：`M0-08` 的依赖已满足并已开始；`M5-01` 继续等待其余前置契约任务。

## 2026-08-24 · M0-08 · DONE

- 负责人：Codex。
- Commit/PR：`c20393f`（本地提交，未推送）；用户 Code Review 已通过。
- 产物：固定 Node `24.19.0`、npm `11.17.0`、精确 React/Vite/TypeScript 依赖及 `lucide-react 1.34.0` 的 Web Module 和 npm 11 Lockfile；包含克制的响应式后台管理骨架与语义匹配的菜单图标；要求受信 Loopback Certificate 的 Vite HTTPS 开发入口和保持 Host/Origin/Path 的 `/api/v1` 代理；生产 `dist` 的 Go Embed；Server 在打开 Data Target/SQLite 前完成嵌入资源校验；README 与技术方案中的构建顺序、开发方式和 M5 真实 Auth E2E 边界。Tailwind、shadcn/ui、React Router、TanStack Query、Gin、业务 DTO/API Client 和静态资源 HTTP Handler 均未提前引入。
- 验收命令：`node --version`；`npm --version`；`npm --prefix web ci`；`npm --prefix web run check`；`npm --prefix web run build`；构建前后 `package.json`/`package-lock.json` SHA-256 对比；`tools/test-web-proxy.ps1`；PowerShell Parser；`go test ./web ./internal/server/bootstrap`；`go test -race ./web ./internal/server/bootstrap`；`go test ./...`；`go vet ./...`；`go build ./cmd/server`；`CGO_ENABLED=0` Linux amd64/arm64 Server 交叉构建；WSL Linux amd64 真实 Server 进程 Smoke；临时移走 `web/dist` 后 Go Embed 编译失败检查；Playwright 1280×720 与 390×844 页面复查；`git diff --check`。
- 验收结果：Node/npm 精确版本匹配；`lucide-react 1.34.0` 的 Peer Dependency 明确支持 React 19；`npm ci` 审计 0 漏洞，类型检查与 Vite `8.2.2` Production Build 通过，Lockfile SHA-256 保持 `904BB3F144C1AC2ECA513FDD2626EFA524AF0CD7E33F240A8C11C19E2AE7A909`；Harness 自动覆盖缺证书、缺私钥的非零快速失败，HTTPS `/api/v1` 转发及 Host/Origin/Path 保持，并验证调用者环境变量恢复。Go 1.27.0 本地工具链下定向测试、Race、全包测试、Vet、Server Build 均通过；最终 Binary 可检出内嵌页面标记；缺失 `dist` 时 `go:embed` 编译期失败。Linux amd64 Binary 在 WSL 中以权限 `0700` 的 Runtime Directory 启动并正常停止；Linux arm64 交叉构建通过。桌面与移动视口管理菜单均使用独立语义图标，页面无横向溢出，浏览器控制台 0 Error/0 Warning。三路独立复审发现的问题均已修正，无未处理 P0/P1/P2。
- 剩余风险：`dist` 按设计不提交，干净 Checkout 必须先执行 `npm ci`/Web Build 再执行 Go Test/Build，该顺序由 `M0-10` CI 固化；Linux arm64 只有交叉构建、没有原生运行证据；PowerShell Harness 已通过 Parser 和真实执行，当前环境未运行 PSScriptAnalyzer；真实 Login、Secure Cookie、CSRF POST 与 Logout E2E 依赖 `M5-03`，由 `M5-10` 完成。本记录不勾选 M0 Gate。
- 解锁的后续任务：`M0-09` 的 M0-08 依赖已完成，但尚未开始并继续等待用户明确指令；`M5-03` 继续等待其他前置任务。

## 2026-08-24 · M0-09 · DONE

- 负责人：Codex。
- Commit/PR：`88ab3eb`（本地提交，未推送）；2026-08-25 用户 Code Review 通过。
- 产物：按多架构索引摘要固定 Node `24.19.0`、Go `1.27.0` 与 Distroless Nonroot 的多阶段 Dockerfile；Server/Agent 独立 Binary Stage 和最终镜像；Dockerfile 专属 allowlist Context；覆盖 amd64/arm64、非 root、只读根、Data Volume、Server Runtime tmpfs、镜像隔离、同卷二次启动与 SIGTERM 的 OCI Smoke；新增 Compose v2 双栈 Profile 与隔离镜像/网络/Volume 的 Smoke，Management 只发布到宿主回环，Agent Gateway 显式发布 IPv4/IPv6；新增未接入启动路径的双栈监听原语与 Config IPv6 测试；使用 `xtunnel-server`/`xtunnel-agent` 双用户、独立 Runtime/State Directory、角色配置权限和保留式卸载的 systemd Unit、安装/卸载与隔离 Smoke；同步 README 与长期部署契约。
- 验收命令：`shellcheck --shell=sh deploy/docker/*.sh deploy/systemd/*.sh`；`sh -n`、`dash -n`、`bash --posix -n`；四组 `deploy/docker/smoke.sh --target <server|agent> --platform <linux/amd64|linux/arm64>`；`docker compose version`、`docker compose --file deploy/docker/compose.dualstack.yaml config --quiet`；amd64/arm64(QEMU) `sh deploy/docker/dualstack-smoke.sh --skip-build`；Docker CLI 等价双栈 Network/容器地址检查；三个固定基础摘要的 `docker buildx imagetools inspect`；`deploy/systemd/smoke.sh`；`$env:GOTOOLCHAIN='local'; ./tools/check-go-version.ps1`；Windows `go test ./...`、`go test -race ./internal/server/bootstrap ./internal/server/config`、`go vet ./...`；Linux 固定 Go 1.27 镜像定向 Listener Test；`npm --prefix web run check`；`npm --prefix web run build`；`git diff --check`。
- 验收结果：原 OCI/systemd 证据保持有效。WSL 安装并实测 Docker Compose `2.40.3+ds1-0ubuntu1~22.04.1`，官方 Config 校验通过；amd64 原生与 arm64 QEMU 的 Compose Smoke 均验证 Server/Agent 获得 IPv4/IPv6 地址、Management/Agent Gateway 四组 Host Binding 分配非零端口、独立 Data Volume、Server Runtime tmpfs、`65532:65532`、只读 RootFS、`CapDrop=ALL`、No New Privileges、独立入口和 SIGTERM 退出。Go 1.27.0 本地工具链下全包测试、定向 Race、Vet 通过；Linux 固定 Go 1.27 镜像实际验证 tcp4/tcp6 同端口 Dial/Accept、`IPV6_V6ONLY=1`、第二地址族绑定失败后第一地址族端口释放。ShellCheck、三种 POSIX 语法、Compose Config 与 Diff Check 通过；Smoke 临时容器、网络、Volume、镜像标签和测试 Cache 均已清理。
- 剩余风险：默认 Compose 冷构建两次在 WSL BuildKit Go 编译阶段超过 360 秒后终止，`--skip-build` 使用本轮前已验收的 amd64/arm64 镜像完成运行层验证；同一 WSL 的 Linux Race 冷编译也超过 360 秒，未记录为通过。arm64 仍是 QEMU 仿真，不等同于原生 Runner；原生干净 Checkout、Registry Manifest 和该环境冷编译问题由 `M0-10` 补齐。当前双栈监听原语没有生产调用者，Management、Agent Gateway、Ingress、TLS、Session 和公网 IPv6 E2E 均未实现；Host Binding 不是应用连通或公网可达证据。本记录不勾选 M0 Gate。
- 解锁的后续任务：`M0-10` 的依赖已满足并进入 `READY`；实施前须取得 CI 配置变更的用户明确确认。

## 2026-08-25 · M0-10 · DONE

- 负责人：Codex。
- Commit/PR：`7c89a002c8ca7729442b2619f28a47654105f899`（已推送至 `master`）；GitHub Actions Run [`32799308530`](https://github.com/lifei6671/xtunnel/actions/runs/32799308530) 成功；2026-08-25 用户 Code Review 通过。
- 产物：`.github/workflows/ci.yml`。Workflow 固定 `actions/checkout`、`actions/setup-go`、`actions/setup-node` 的提交 SHA，固定 Go `1.27.0`、Node `24.19.0`、npm `11.17.0` 与 `GOTOOLCHAIN=local`，在原生 Linux amd64/arm64 Runner 上复用 Proto、OpenAPI、Web、Go 和 OCI Smoke 入口。
- 验收命令：两种架构均执行 `tools/check-go-version.sh`；`bootstrap-proto.sh` 与 `proto.sh lint|breaking|generate-check`；`bootstrap-openapi.sh`、`openapi.sh validate`、`test-openapi.sh`；`npm ci/check/build`；根/工具 Module 校验、`go test ./...`、定向 `go test -race`、`go vet ./...`、Server/Agent Build；Server/Agent 的 `deploy/docker/smoke.sh`；`git diff --check` 与工作区清洁检查。
- 验收结果：Run `32799308530` 的 `verify (amd64)` 与 `verify (arm64)` 均成功；两种原生架构都完成全部 11 个主验证步骤，OCI Smoke、生成文件清洁检查和退出清理均通过。
- 剩余风险：本任务不运行需要 root 和真实 systemd 的 `deploy/systemd/smoke.sh`，该证据保留在 M0-09；Compose 双栈 Smoke 不作为公网 IPv6、应用 Listener 或生产连通性证据。M0 Gate 仍等待 M0-11、M0-12 及其全部 Checklist。
- 解锁的后续任务：`M0-11` 的技术依赖已满足；开工前先完成数据库 Schema、权限与对外 CLI 影响审计并取得所需确认。

## 2026-08-25 · M0-02/M0-09 · Agent 轻状态基线，M0-09 IN_PROGRESS

- 负责人：Codex；用户已明确确认公共 Protocol、Config Schema、数据库长期设计和 systemd/OCI 部署边界调整；本轮未提交、未推送。
- 产物：Agent Config Schema/Go Struct 缩减为 `server`、`auth.token_file`、`logging`；删除 Agent `data_dir` 与本地 WorkPool/Reconnect/Control/Health 策略；systemd Agent 删除 StateDirectory 并从 `/etc/xtunnel/agent.token` 只读凭据；Agent OCI/Compose 删除持久 Volume 并改用只读 Secret；总方案、README 与 M0.5/M1/M2/M3/M7 后续任务同步为 Ephemeral Instance + Full Remote Snapshot + In-memory Atomic Apply。
- 验收命令：`go env GOVERSION`、`go env GOTOOLCHAIN`；`gofmt`；定向与全包 `go test`、定向 `go test -race`、全包 `go vet`；Linux amd64 Agent Bootstrap Test Binary 交叉编译；`sh -n`、`dash -n`、`bash --posix -n`、ShellCheck；`docker compose ... config --quiet`；WSL 真实 `deploy/systemd/smoke.sh`；`git diff --check` 和术语/任务计数一致性检查。
- 验收结果：Go `go1.27.0` / `GOTOOLCHAIN=local` 匹配；Agent 配置定向/全包测试、Race、Vet 与 Linux 交叉编译通过；旧本地策略字段由 Strict Schema 拒绝；三种 POSIX Shell 语法、ShellCheck、Compose Config 与 Diff Check 通过；WSL systemd install/start/restart/stop/uninstall Smoke 通过且无残留。M0-02 的 Config Loader/Schema 能力仍满足既有验收，不改变其 `DONE` 状态。
- 状态影响：Agent 部署产物已改变，但当前 Docker Socket 无访问权限，未运行更新后的真实 Agent OCI 生命周期与 Compose 双栈 Smoke，因此 M0-09 从 `DONE` 重新标记为 `IN_PROGRESS`，仪表盘减为 `9/12`；本记录不勾选 M0 Gate，也不改变用户正在实施的 M0-11 状态。
- 剩余风险与解除条件：在可访问 Docker Engine 的 Linux amd64/arm64 或 CI 环境运行更新后的 Server/Agent OCI Smoke 与 Compose 双栈 Smoke，并取得 M0-10 后要求的 CI Run 证据；通过独立复审且无未处理问题后，M0-09 才能回到 `DONE`。

## 2026-08-25 · M0-02/M0-09 · Token-only Agent 契约，IN_PROGRESS

- 负责人：Codex；用户最终确认 Agent 只接收一个版本化 Connection Token，授权删除 Agent Schema/Loader、用户准备的 Token 文件与 Agent YAML，并同步部署契约；本轮未提交、未推送。
- 契约与产物：本记录明确取代上一条“Agent Bootstrap Config + Token File”目标模型，但不篡改其历史证据。用户只复制单个不透明 `xta_...`；其语义携带 Endpoint、TLS Trust、Agent/Token Identity 与 Secret，精确编码/解析/Golden Vector 留给 `M05-02`。前台使用 `--token`，OCI 使用 `XTUNNEL_TOKEN`，systemd 安装器接收一次 `--token` 并内部创建 root-only LoadCredential Source；持久 Unit 无 Secret/参数。Agent 无 Schema/YAML、无用户管理的 Token 文件、无本地业务/配置状态；旧 `docs/XTunnel.md` 增加历史稿警示并指向当前两份权威文档。
- 验收命令：`go env GOVERSION/GOTOOLCHAIN`；`gofmt`；Agent Bootstrap 定向/全包 `go test`、`go test -race ./...`、`go vet ./...`；Linux amd64 Agent Binary 与 Bootstrap Test Binary 交叉编译；三种 POSIX Shell 语法与 ShellCheck；`docker compose ... config --quiet`；WSL 真实 systemd install/start/restart/stop/uninstall Smoke；`git diff --check`；旧 Agent YAML/Schema/Token File/独立 Endpoint/TLS 输入残留扫描和任务计数重算。
- 验收结果：Go `go1.27.0` / `GOTOOLCHAIN=local` 匹配；Token 来源优先级、Shape 校验、旧参数拒绝、Secret 不泄露、SIGTERM、全包 Test/Race/Vet 和 Linux amd64 交叉编译通过；Shell/Compose Config 通过；systemd 249 下 Token 字符串安装、root-only Credential Source、LoadCredential 运行时读取、无参数 Agent、重启/卸载保留与无残留 Smoke 通过。整合复审发现并删除 Compose/OCI 遗留 Agent `--set` 参数，新增 Container Cmd 为空断言；真实 Docker Runtime Smoke 未运行。
- 状态影响：删除 Agent Schema/Loader 并重写 Bootstrap 输入实质改变 `M0-02` 产物和验收契约；M0-10 完成后尚无覆盖新契约的 CI Run/复审，因此 `M0-02` 从 `DONE` 回退为 `IN_PROGRESS`。`M0-09` 继续 `IN_PROGRESS`，等待更新后的真实 OCI/Compose Smoke。仪表盘为 M0 `8/12`、总计 `8/95`；本记录不勾选 M0 Gate，也不改变 `M0-11` 状态。
- 剩余风险与解除条件：`M0-02` 仍需 Linux amd64/arm64 CI Run 与合并前复审；`M0-09` 仍需真实 Agent OCI `XTUNNEL_TOKEN` 生命周期、Compose 双栈 Smoke 和 CI Run。当前 Docker Socket 权限拒绝。`service install --token` 按用户选择直接接收字符串，调用方应避免把真实 Token 留在共享终端历史或进程采集系统中；安装器自身不回显或记录 Token。Docker Gate 未通过前不得恢复 `M0-09=DONE`。
- Self-install 追补产物：本轮进一步以 Agent Binary `service install/uninstall` 取代上一版 Agent Shell 安装入口；公开命令统一为 `xtunnel-agent run --token 'xta_...'`、`sudo xtunnel-agent service install --token 'xta_...'` 与 `sudo xtunnel-agent service uninstall`。Binary 自身原子安装到 `/usr/local/bin/xtunnel-agent`，内嵌首行为 `# Managed by xtunnel-agent service install` 的托管 Unit，`ExecStart=/usr/local/bin/xtunnel-agent run`；Credential Source 仍为 `/etc/xtunnel/credentials/agent.token`。Agent 卸载删除托管 Unit 与已安装 Binary，保留 Credential 和服务用户；Server Shell 包装保持不变，Agent OCI 默认执行 `run`。本条追补明确取代上一条验收结果中 Agent Shell 安装与“无参数 Agent”的现行契约，但保留其历史证据。
- Self-install 验收状态：Go `go1.27.0` / `GOTOOLCHAIN=local` 下，CLI/Token/内嵌 Unit/Managed Marker/原子写入/非 Linux 拒绝等定向测试、全包 Test/Vet/Race，以及 Linux amd64/arm64 Binary 与 Test Binary 交叉编译通过；三种 POSIX Shell 语法、ShellCheck、Compose Config 与 Diff Check 通过。最终 Linux amd64 Binary 在 WSL systemd 249 中完成首次安装、重复安装、启动、重启、停止、再启动与安装后 Binary 自卸载 Smoke；重复安装前后 MainPID 不同，证明新 Binary/Token 会经 restart 生效。Smoke 同时验证 root-only Credential、运行时 LoadCredential、无 Agent YAML/State、Unit 无 Secret、卸载保留 Credential/用户，且清理后无 Unit、目录、用户或临时构建残留。Docker Socket 仍因权限拒绝，真实 OCI/Compose Runtime Smoke 未执行；本记录不勾选 M0 Gate。
- Self-install 状态影响：Agent Binary Self-install 证据已补齐，但 `M0-02`、`M0-09` 继续为 `IN_PROGRESS`，M0 仍为 `8/12`、总计仍为 `8/95`，未新增 `DONE`。`M0-09` 仍等待真实 Agent OCI/Compose 与 CI Run；Docker Gate 未通过。

## 2026-08-25 · M0-02/M0-09 · Windows Agent SCM 契约，IN_PROGRESS

- 负责人：Codex；用户已确认 Windows Agent Service 属于 V0.1 支持范围，并授权同步平台、Credential 与安装契约；本轮同步修改 Go 实现、Windows Smoke、CI、README 与权威技术文档，不提交、不推送、不改变暂存区。
- 冻结契约：Windows Agent 支持 amd64/arm64，使用同一 Binary 的 `run`、`service install --token` 与 `service uninstall`，不提供用户安装脚本。SCM ServiceName=`XTunnelAgent`、DisplayName=`XTunnel Agent`、账户=`NT AUTHORITY\LocalService`；Binary 安装到 `%ProgramFiles%\XTunnel\xtunnel-agent.exe`；Token 使用 `CRYPTPROTECT_LOCAL_MACHINE | CRYPTPROTECT_UI_FORBIDDEN` 加密到 `%ProgramData%\XTunnel\credentials\agent.token.dpapi`。SCM ImagePath 只含安装 Binary + `run`，Description marker 精确为 `Managed by xtunnel-agent service install`；非受管同名 Service 拒绝覆盖/删除；卸载立即删除受管 Service，Binary 可立即删除时直接删除，从运行中已安装 EXE 自卸载时安排下次系统重启删除，DPAPI Credential 及目录始终保留。Linux systemd 契约保持不变。
- 静态产物：当前工作区已实现 Windows SCM/DPAPI Service、同平台测试、`deploy/windows/smoke.ps1` 与 `windows-2022` CI Job。运行行为包含 ProgramFiles/ProgramData Known Folder、受保护 ACL、Machine-scope DPAPI、固定 Description marker、LocalService、30s Stop/Shutdown、非零异常退出、non-crash recovery、重复安装 Replace Existing + Write Through，以及从已安装 EXE 自卸载时 `DELAY_UNTIL_REBOOT` 删除。Event Log Source 尚未实现，SCM 模式 JSON stderr 不保证持久可见；该缺口进入 `M6-01/M6-06`，不能按 M0 日志存在推断生产可观测性通过。
- 已验证证据：`go1.27.0` / `GOTOOLCHAIN=local`；全包 `go test ./...`、`go vet ./...`、`go test -race ./...`，Agent Service/Bootstrap 定向 Test/Vet/Race，Linux amd64/arm64 Agent Build，Windows arm64 Agent Build 与 Bootstrap/Service Test Binary 编译均通过；本机 Windows DPAPI LocalMachine Roundtrip 真实执行通过。Windows CI/Harness 侧 `go mod verify`、Agent 定向 Test/Race/Vet、Windows arm64 Build/Test Compile 通过；`smoke.ps1` 在 Windows PowerShell 5.1 与 PowerShell 7 Parser、ASCII-only、Diff Check 通过，非管理员 Binary/Harness 均在写目标目录前快速拒绝，Harness 能同时报告主失败与 Cleanup 失败。
- 未验证边界：当前 Windows 宿主不是管理员，正向 SCM install/reinstall/stop/start/uninstall Smoke 未运行；新增 GitHub `windows-2022` CI Job 尚未触发。运行中已安装 EXE 的 `DELAY_UNTIL_REBOOT` 路径只有注入单元测试，尚未真实触发；真实 Agent OCI/Compose Runtime Smoke 仍因 Docker Socket 权限未执行。本条不记录 Windows SCM/Docker/CI Gate PASS。
- 状态影响：`M0-02`、`M0-09` 继续为 `IN_PROGRESS`；M0 保持 `8/12`、总计保持 `8/95`，未新增 `DONE`。`M0-09` 仍等待提升权限的真实 Windows SCM Smoke、真实 Agent OCI/Compose Runtime Smoke 及 M0-10 后的新 CI Run/复审证据。

## 2026-08-25 · M0-11 · REVIEW

- 负责人：Codex；`ee9227a` 的首个 Admin Bootstrap 实现已完成用户复审。本轮未提交、未推送，未改变暂存区。
- 产物：`admin create` 的 TTY/文件密码输入、离线 External Lock 写入、Linux `0600` Unix Socket 与 root `SO_PEERCRED` 授权路径保持既有契约。本轮让 Socket 的 Context 监听协程在首管创建成功或显式 `Close` 后立即退出，避免 Server 生命周期内的残留协程；新增真实 root/non-root peer、授权拒绝不写库及 Socket 关闭路径测试。
- 验收命令：`GOTOOLCHAIN=local go test -count=1 ./...`；`GOTOOLCHAIN=local go test -race -count=1 ./...`；`GOTOOLCHAIN=local go vet ./...`；`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./internal/server/bootstrap ./internal/repository/sqlite`；使用本机 Go `go1.27.0` 交叉编译 Linux amd64 Bootstrap Test Binary，并在 WSL Ubuntu 22.04 分别以普通用户和 root 运行 Socket/离线用例；`git diff --check`。
- 验收结果：本机 Go 环境为 `go1.27.0` / `GOTOOLCHAIN=local`；全包 Test、Race、Vet 与 Linux 目标 Vet 通过。WSL Linux 真实运行验证普通用户被 `SO_PEERCRED` 拒绝且数据库未写入、Socket 保留；root 成功创建首个管理员；`0600` Socket、目标 Hash 拒绝、客户端断连提交、显式关闭与离线路径重复拒绝均通过。临时 Linux Test Binary 已清理。
- 状态影响：`M0-11` 转为 `REVIEW`；M0 和总计的 `DONE` 数仍为 `8/12`、`8/95`。本记录不勾选 M0 Gate，也不将 `M0-11` 标记为 `DONE`。
- 剩余风险与解除条件：本机没有 Linux Go/gcc，无法执行 Linux `-race`；必须在 `ee9227a` 之后包含本轮改动的原生 Linux CI Run 中执行相应 Race Suite，才可考虑 `M0-11=DONE`。M0-02 与 M0-09 的独立外部验证缺口不受本任务影响。

## 2026-08-25 · M0-02 · IN_PROGRESS

- 负责人：Codex；本轮只补齐 Token-only Agent Bootstrap 的契约边界测试，未提交、未推送，未改变暂存区。
- 产物：验证恰好 `8192` bytes 的 `xta_` Token 可接受；`service install` 对缺失 Token、未知 Flag、位置参数和非法 Token 失败且不调用安装逻辑、不回显 Token；Windows DPAPI Credential 文件读取、缺失文件和损坏密文的失败链均不泄露明文。生产路径仅抽出内部文件读取函数，ProgramData Known Folder 与 Machine-scope DPAPI 契约未改变。
- 验收命令：`GOTOOLCHAIN=local go test -count=1 ./internal/agent/bootstrap ./internal/agent/service ./cmd/agent`；`GOTOOLCHAIN=local go test -race -count=1 ./internal/agent/bootstrap ./internal/agent/service`；`GOTOOLCHAIN=local go vet ./internal/agent/bootstrap ./internal/agent/service ./cmd/agent`；`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./internal/agent/bootstrap ./internal/agent/service`；Linux amd64 Bootstrap Test Binary 在 WSL Ubuntu 22.04 运行；Windows arm64 Agent、Bootstrap Test 与 Service Test Binary 交叉编译；`git diff --check`。
- 验收结果：本机 Go 环境为 `go1.27.0` / `GOTOOLCHAIN=local`；定向 Test、Race、Vet 与 Linux 目标 Vet 通过。WSL 实际验证 Token 来源优先级、8192-byte 包含边界、拒绝旧参数/位置参数、Token 不回显和 SIGTERM 前台退出；Windows arm64 三项编译通过。Windows arm64 临时产物位于用户 Temp，受当前执行环境的删除策略限制未能自动删除；它们不在工作区或暂存区。
- 状态影响：`M0-02` 保持 `IN_PROGRESS`；M0 和总计的 `DONE` 数仍为 `8/12`、`8/95`。本记录不勾选 M0 Gate，也不将“已有 CI Workflow”记为本轮 CI 证据。
- 剩余风险与解除条件：需让包含 `ee9227a` 和本轮未提交改动的干净提交进入原生 Linux amd64/arm64 与 Windows CI，获得 Token-only 契约的 Test/Race/Vet/Build 证据，并完成用户复审后，才能考虑 `M0-02=DONE`。Windows 提升权限 SCM Smoke、OCI/Compose Runtime Smoke 属于 `M0-09`，不作为 M0-02 的完成条件。

## 2026-08-25 · M0-09 · IN_PROGRESS

- 负责人：Codex；本轮只重新尝试 Token-only Agent OCI Runtime 验证，未提交、未推送，未改变暂存区。
- 验收命令：以 WSL Ubuntu 22.04 root 访问 Docker Engine，运行 `./deploy/docker/smoke.sh --target agent --platform linux/amd64`；随后只读检查 Docker Container、Network 和 Volume 遗留。
- 验收结果：Windows 宿主未安装 Docker CLI，WSL 普通用户无 Docker Socket 权限；WSL root 可访问 Docker `29.1.3`、Compose `2.40.3`。Agent OCI Smoke 在 BuildKit 冷构建阶段 360 秒内没有产生运行结果，按计划阈值中止，不能记录为 Smoke PASS。检查确认没有运行中/已停止的 Smoke Container，也没有 `xtunnel` Network 或 Volume 遗留。
- 状态影响：`M0-09` 保持 `IN_PROGRESS`；M0 和总计的 `DONE` 数仍为 `8/12`、`8/95`。本记录不勾选 M0 Gate，不把 Docker Engine 可访问或 BuildKit 已开始构建记为 OCI Runtime 证据。
- 剩余风险与解除条件：在具备可复现冷构建性能的干净 Linux amd64/arm64 环境或 CI，完成 Server/Agent OCI 及 Compose 双栈 Runtime Smoke；另需在提升权限的 Windows 环境运行 SCM 正向 Harness。当前非管理员 Windows 会话只能完成静态测试，不能替代真实 Service 安装/启动/停止/卸载证据。

## 2026-08-25 · M0 CI 收尾验证 · M0-11 DONE / M0-02、M0-09 IN_PROGRESS

- 负责人：Codex；用户授权提交、推送与提升权限 Windows SCM Smoke。代码提交 `95f9dcb` 补齐 Bootstrap/Admin Socket 契约测试，CI #5 的 Windows checkout 因嵌入 systemd Unit 被 Git 转为 CRLF 而让 LF 专用测试误失败；提交 `d1557db` 将该测试归一化为逻辑行结束符后重新触发 CI。未修改冻结 Protocol、Config Schema、OpenAPI、产品契约或依赖。
- 验收证据：本机 `go1.27.0` / `GOTOOLCHAIN=local` 下，`go test -count=1 ./internal/agent/... ./cmd/agent`、`go test -race -count=1 ./internal/agent/bootstrap ./internal/agent/service`、`go vet ./internal/agent/... ./cmd/agent` 与 `go test -count=1 ./internal/server/bootstrap` 通过。提升权限执行 `deploy/windows/smoke.ps1` 的真实 SCM install/reinstall/start/stop/uninstall Harness 退出码为 0，清理后 Service、ProgramFiles Binary 与 ProgramData Credential 均由 Harness 清理。CI Run `32815381791`（提交 `d1557db`）的 `verify (amd64)`、`verify (arm64)` 和 `Windows Agent service` 全部成功；Linux 两个原生 Job 均执行 Server/Agent OCI Smoke，Windows Job 执行 Windows Service Smoke。
- 状态影响：`M0-11` 的用户复审、Socket 权限/失败路径本机与 WSL 证据，以及 CI Linux Race 已齐备，标记 `DONE`。M0 为 `9/12`，总计为 `9/95`。`M0-02` 新 CI 已覆盖 Token-only Bootstrap 的 Test/Race/Vet/Build，但仍等待用户复审；`M0-09` 的 Linux systemd、提升权限 Windows SCM 和原生双架构 OCI Smoke 已补齐，仍为 `IN_PROGRESS`。
- 未完成边界：CI Workflow 不执行 `docker compose` 双栈 Profile，且本轮未取得 Compose IPv4/IPv6 Runtime Smoke；不得把 OCI Smoke 推论为 Compose Gate PASS。`M0-09` 完成前还需要该真实 Compose Smoke 与用户复审；因此 `M0-12` Gate 仍不得开始或勾选。

## 2026-08-25 · M0-09 · Compose 双栈 Runtime 重试未通过

- 负责人：Codex；本轮未改动代码、Compose 定义、冻结契约或依赖，未暂存、未提交、未推送。执行环境为 WSL Ubuntu 22.04 root，Docker Engine `29.1.3`、Docker Compose `2.40.3`；普通 WSL 用户没有 Docker Socket 权限。
- 验收命令：两次执行 `./deploy/docker/dualstack-smoke.sh --platform linux/amd64`。每次都只输出 BuildKit 加载 compose/Dockerfile 定义，随后在顺序冷构建阶段 360 秒内未产生容器启动或 Runtime 断言结果，按既定阈值中断；不得记录为 Compose Smoke PASS。中断后检查没有本轮完整构建留下的容器、网络、卷或临时镜像。
- 环境演练：为区分 Builder 与 Compose 环境，使用 17 小时前生成的 `xtunnel-server-smoke:amd64`/`xtunnel-agent-smoke:amd64` 执行 `--skip-build`。该演练已创建唯一项目名的双栈 Network、Volume 和两个 Container，但在创建后 120 秒无后续输出而中断；历史镜像不绑定当前提交，不能作为验收证据。清理检查发现其遗留 Network/Volume，无容器；随后仅删除已核实的该项目 Network/Volume，确认无残留。
- 状态影响：`M0-09` 保持 `IN_PROGRESS`，M0 仍为 `9/12`、总计仍为 `9/95`；不勾选 M0 Gate，不把 Engine 可访问、Network 创建、历史镜像演练或 OCI CI Smoke 视为 Compose Runtime PASS。
- 剩余风险与解除条件：需要在 Builder 可在阈值内完成当前提交镜像的 Linux Docker/Compose 环境，运行并通过当前提交的 Compose IPv4/IPv6 Runtime Smoke；成功证据必须单独记录实际镜像来源、架构、Docker/Compose 版本、项目名和清理检查，再经用户复审才可考虑进入 `REVIEW`。该 Smoke 仍不证明 Management/Gateway 应用连通或公网 IPv6 路由。

## 2026-08-25 · M05-01 · REVIEW

- 负责人：Codex；用户明确确认开始 Protocol Freeze。新增唯一 Wire Authority `api/proto/common.proto`，固定 `package xtunnel.protocol.v1` 与 `go_package=github.com/lifei6671/xtunnel/internal/protocol/gen;protocolv1`；只冻结共享 ID 格式注释、`ErrorCode` 全表及 `WorkReadyStatus`、`OpenStatus`、`IngressType`、`HealthType`、`HealthStatus`、`ConfigApplyStatus`，没有提前定义 Auth、Control、Snapshot、Health 消息或 Work 消息。
- Lint 契约：总方案明确 `ERROR_CODE_OK=0` 与 `HEALTH_STATUS_UNKNOWN=0` 是仅有的有意零值例外。Buf STANDARD 默认要求 `_UNSPECIFIED`，因此 `buf.yaml` 只对 `api/proto/common.proto` 的 `ENUM_ZERO_VALUE_SUFFIX` 设置 `ignore_only`；`internal/protocol/contract/common_contract_test.go` 完整锁定七个 enum 的成员、顺序与数值，以及七种 ID 前缀说明，防止该例外放宽 Protocol 约束。
- 验收命令：Windows 本机在 `go1.27.0` / `GOTOOLCHAIN=local` 下执行 `gofmt`、`go test -count=1 ./internal/protocol/contract` 与 `go test -count=1 ./...` 均通过；WSL Ubuntu 22.04 root 使用仓库受管 Buf 执行 `./tools/proto.sh lint` 通过。`./tools/proto.sh breaking` 在 Proto 已存在而 M05-04 初始不可变 Baseline 尚未配置时明确失败，已作为预期失败分支验证，不得视为 Breaking PASS；WSL root 没有 Go，因此未在该环境运行 Go 版本检查。
- 状态影响：`M05-01` 转为 `REVIEW`，M0.5 里程碑转为 `IN_PROGRESS`；M0.5 和全局 `DONE` 计数均不增加，M0 Gate、M05-10 与所有 Protocol Handler 仍不得开始。待用户复审、提交后取得相应 CI 证据，再按任务依赖推进 M05-02/M05-03 与 M05-04 初始 Baseline。

## 2026-08-25 · PLAN-ORDER · 核心功能先行

- 决策：用户明确授权将部署相关工作后置，先完成核心功能；本记录只调整开发顺序，不删除或弱化 V0.1 的部署、权限与发布验收契约。
- 计划调整：M0-09 的 OCI/Compose、systemd 与 Windows SCM 部署验收不再阻塞 M0.5 Protocol Gate 或 M1。M05-10 仅依赖 M05-01 至 M05-09；M1 入口改为 M05-10，并继续遵守每个 M1 任务表中列出的 M0-05、M0-11 等核心前置。M0-12 仍包含 M0-09，且已成为 M7-10 Alpha 发布 Gate 的显式前置。
- 验收命令：`git diff --check`；计划依赖、任务状态和仪表盘逐项复核。
- 验收结果：本轮未修改代码、Proto、Schema、OpenAPI、配置或部署产物；未改变任何产品任务状态、计数或 Gate 结论。
- 剩余风险：M05-01 仍在用户 Protocol Review 中，未获得复审结论前不得开始 M05-02/M05-03；M0-02 和 M0-09 仍保留各自的复审/验收缺口。
- 解锁的后续任务：用户通过 M05-01 Review 后，可按 M05-02/M05-03 的冻结依赖推进；M1 仍须等待 M05-10 `DONE`。

## 2026-08-25 · M05-02 至 M05-09 · IN_PROGRESS

- 负责人：Codex；用户授权继续完成 M0.5 核心 Protocol 实现，并明确确认新增直接依赖 `google.golang.org/protobuf v1.36.12`。本轮未提交、未推送，也未改变既有暂存区。
- 契约与产物：新增 `api/proto/control.proto`、`api/proto/work.proto` 和不可变初始 Buf Baseline；生成 `internal/protocol/gen`；实现分层 UVarint Frame Codec、递归 Unknown Field 拒绝、确定性 Protobuf 编码、Connection Token 编码与校验、固定 Golden Vectors，以及 Auth/Control/Work 状态、方向与幂等性契约测试。`ConfigAck` 的 Revision 另以 `config-ack-v1.hex` 固定，所有新增手写代码与 Proto 注释均使用简体中文。
- 验收命令：在 Windows `go1.27.0` / `GOTOOLCHAIN=local` 下执行 `go mod verify`、`go test -count=1 ./...`、`go test -race -count=1 ./internal/protocol/...`、`go vet ./...` 均通过；WSL Ubuntu 22.04 使用受管 Buf 执行 `./tools/proto.sh lint`、`./tools/proto.sh breaking` 均通过。首次 Breaking 使用已落盘 Baseline，而非与当前 Proto 自比较。
- 未通过或未运行项：`./tools/proto.sh generate-check` 需要干净 Linux Git checkout；当前工作区存在本轮未提交/未跟踪产物，且 Windows `.git` 在 WSL 下不可作为 Git 工作树使用，因此不能作为通过证据。CI 尚无本次提交可运行；Protocol 独立复审和 M05-10 Gate Checklist 也尚未完成。
- 状态影响：M05-02 至 M05-09 保持 `IN_PROGRESS`，M05-10 仍为 `NOT_STARTED`；M0.5 仪表盘和全局 `DONE` 计数均不增加，M1 不得开始。本轮不勾选任何产品任务。
- 剩余风险与解除条件：提交前需重新暂存所有最新 Proto、Baseline、生成文件和测试；在干净 Linux checkout/CI 通过 `generate-check` 后，连同用户的 M0.5 Protocol Review 完成 Gate 复审，才可将 M05-10 置为 `DONE` 并解锁 M1。

## 2026-08-25 · M05-10 · IN_PROGRESS

- 负责人：Codex；用户完成 M0.5 Protocol Review 后授权继续下一步。本记录只将已完成实现与验证推进至 `REVIEW`，不把临时验证快照伪装成项目提交或 CI 证据。
- 干净快照验证：从当前 `HEAD=6a594c242608070ff90693586ee98f256a4bd501` 创建原生 WSL Git 克隆，将当前已跟踪变更和未跟踪的 Protocol 源码/测试复制到隔离目录；仅在该临时目录创建一次性快照提交 `a97fa75ff2e145f30e645ef7ffa6d697c005e018`，随后执行 `./tools/proto.sh lint`、`breaking`、`generate-check`，全部通过且快照工作树干净。临时目录已删除；该快照不修改项目工作区、暂存区、提交历史或远端。
- 既有验证：Windows `go1.27.0` / `GOTOOLCHAIN=local` 下的全包 Test、Protocol Race、Vet、Golden 逐字节比较和 `git diff --check` 均通过；Auth/Control/Work 状态与方向、Unknown Field、Token 完整性/版本、Snapshot/ConfigAck 契约均由对应表驱动测试覆盖。
- 状态影响：M05-01 至 M05-09 转为 `REVIEW`，M05-10 转为 `IN_PROGRESS`；M0.5 与全局 `DONE` 计数仍为 0，本轮不勾选任何产品任务，M1 仍被 M05-10 锁定。
- 剩余风险与解除条件：M0-10 已要求后续 `DONE` 附真实 CI Run 证据。需先对当前工作区最新版本进行正式提交并触发 CI；CI 全绿后才能将 M05-10 置为 `DONE` 并开始 M1。当前存在 staged 与 unstaged 混合变更，提交前必须重新暂存最新文件。
