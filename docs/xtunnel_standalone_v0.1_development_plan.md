# XTunnel Standalone V0.1 开发执行计划

> **文档用途**：将《XTunnel Standalone 第一阶段完整技术方案 V0.1》转换为可执行、可推进、可验收的开发 Backlog
>
> **进度基线日期**：2026-08-30
>
> **当前阶段**：M5-10 Contract/E2E Test Suite · IN_PROGRESS（M2、M3、M4、M5-01 至 M5-09 均已 `DONE`）
>
> **当前结论**：用户已明确确认 M5-09 阶段复审通过，M5-09 转为 `DONE`，M5 更新为 `9/11`、全局更新为 `73/95`。M5-10 已落地 25/25 Operation 实际响应 Contract、全 Mutation CSRF、23/23 错误码分类，以及真实 Server/临时 SQLite 分别经 Caddy、Nginx HTTPS 的 Chromium 工作流；本地 Contract/OpenAPI/Web/Go 验证通过，但当前 Windows/WSL 环境没有可执行该 Linux Browser Gate 的 Docker/Go/Node 组合，也没有包含最终源码的精确 CI，因此 M5-10 继续保持 `IN_PROGRESS`，M5 Gate Checklist 六项继续全部未通过。

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
├──→ M2 Credential Lifecycle & Failover Hardening ────┐
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
- M2 和 M3 在 M1 Gate 后可并行，但 M4 产品数据面必须同时等待 M2 的 Connector Selection/Failover 和 M3 的 Service/TunnelSnapshot 契约。
- M5 的 OpenAPI Entry Gate 可在 M4 后半段提前准备，Handler 和 Web 实现必须等 Gate 通过。
- M6 的 Logging/Metrics 骨架可从 M0/M1 纵向渗透，但 M6 Registry、告警、查询 API 和 Dashboard 不得反向成为 M1 Gate 的依赖；安全操作的最小 Audit Event 契约与 append-only 写入由对应安全任务自身完成，M5/M6 只增加查询、导出和运维消费。
- M7 只调优和验证已存在的正确性边界，不允许第一次实现 M1/M3 应有的上限与恢复机制。

---

# 4. 进度仪表盘

| 里程碑 | 任务数 | 已完成 | 状态 | 入口依赖 | 退出 Gate |
| --- | ---: | ---: | --- | --- | --- |
| M0 工程初始化 | 12 | 9 | `IN_PROGRESS` | 技术方案基线 | M0-12 |
| M0.5 Protocol Freeze | 10 | 10 | `DONE` | M0-06 | M05-10 |
| M1 Secure TCP Baseline | 14 | 14 | `DONE` | M05-10 | M1-14 |
| M2 Credential/Failover Hardening | 8 | 8 | `DONE` | M1-14 | M2-08 |
| M3 Config/Health | 13 | 13 | `DONE` | M1-14 | M3-13 |
| M4 Product Data Plane | 10 | 10 | `DONE` | M2-08 + M3-13 | M4-10 |
| M5 REST API/Web | 11 | 9 | `IN_PROGRESS` | M3-13 + M4-10（M5-01 可在 M4 后半段准备） | M5-11 |
| M6 Observability | 7 | 0 | `NOT_STARTED` | M5-11 | M6-07 |
| M7 Hardening/Alpha | 10 | 0 | `NOT_STARTED` | M2-08 + M3-13 + M4-10 + M5-11 + M6-07 | M7-10 |
| **合计** | **95** | **73** |  |  |  |

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
| M05-01 | 冻结 Common Types | M0-06 | `api/proto/common.proto` | package/go_package、enum 数值、reserved range、ErrorCode 完整 | `DONE` |
| M05-02 | 冻结 Connection Token + Auth/Control Contract | M05-01 | Connection Token v1 编码/解析契约、`api/proto/control.proto` | Token 仍是单个不透明 `xta_...`；冻结 Endpoint、TLS Trust、Tunnel/Token Identity、Secret 的精确编码/完整性/版本分派与失败语义；Connector 裸 Auth Frame、ControlEnvelope、TunnelSnapshot/ConfigAck、ServiceHealth Batch 完整 | `DONE` |
| M05-03 | 冻结 Work Contract | M05-01 | `api/proto/work.proto` | WorkHello/Ready/Open/Response；RAW 切换；各状态唯一裸 Message | `DONE` |
| M05-04 | 生成代码与 Breaking Baseline | M05-01至 M05-03 | `internal/protocol/gen`、Buf Initial Baseline | 明确记录“首次冻结无历史前代”的 Baseline 建立方式，禁止与自身比较伪装 Breaking 证据；生成结果提交；`lint/breaking/generate-check` 通过 | `DONE` |
| M05-05 | Frame Codec 契约实现 | M05-04 | `internal/protocol/frame`、`codec` | UVarint 分片/合并、Canonical 最短编码、overflow、未终止、Frame 上限和 EOF；在分配 Payload 前拒绝超限 Length；Auth/Control/Work 分层测试完整 | `DONE` |
| M05-06 | 递归 Unknown Field 拒绝 | M05-04 | 共享 Validator 与表驱动测试 | Auth、Control、Work、Snapshot 全覆盖 | `DONE` |
| M05-07 | Deterministic Protobuf Bytes | M05-04、M05-06 | Snapshot/WorkHello 确定性字节构造器 | Snapshot 稳定排序并包含 Revision；WorkHello 清空 MAC 后重建已知字段；固定 Runtime 版本 | `DONE` |
| M05-08 | Protocol Golden Vectors | M05-02、M05-07 | `tests/golden/protocol-v1/*` | Connection Token v1、WorkHello、Snapshot 固定字节/Hash/HMAC；ConfigAck Revision 关联；测试不自动改 Fixture | `DONE` |
| M05-09 | 状态/方向/幂等契约测试 | M05-02、M05-03、M05-05 | Protocol State Test | Token 未知版本/畸形/超限/完整性失败；non-canonical/overflow/未终止 UVarint；Auth 提交点、Control 非法方向、Work 直接关闭、ConfigAck/Drain 幂等 | `DONE` |
| M05-10 | M0.5 Protocol Gate | M05-01至 M05-09 | Protocol Freeze 证据 | 下方 Gate Checklist 全部通过；M0-09 部署验收和 M0-12 完整 M0 Gate 不阻塞本 Gate | `DONE` |

## 6.3 M0.5 Gate Checklist

- [x] `./tools/proto.sh lint` 通过。
- [x] `./tools/proto.sh breaking` 通过。
- [x] 新 Tunnel/Connector/Service Contract 在 CI #11/#12 的 Linux amd64/arm64 干净 checkout 执行 `./tools/proto.sh generate-check` 通过，且两次 CI 整体全绿。
- [x] Golden Vector 逐字节比较通过。
- [x] Auth Success/Failure Transcript 及 Auth→Established 提交边界通过。
- [x] Connection Token v1 编码、解析、版本、完整性和语义字段 Golden Vector 通过。
- [x] Canonical UVarint、overflow、未终止、Frame 超限，以及 Control/Work 方向、状态、乱序、重复、Unknown Field 全部测试通过。
- [x] Snapshot Deterministic Bytes、Revision 与 ConfigAck 关联/幂等测试通过。
- [x] 新 Tunnel/Connector/Service Contract、Golden Vector 和未知 Token 版本拒绝已完成独立 Protocol Review。

---

# 7. M1：Secure TCP Data Plane Baseline

M1 只要求“一个 Tunnel + 一个 Connector + 一个静态 TCP Service”，但必须从一开始使用最终的身份、安全协议和真实资源上限。

身份链固定为“持久 Tunnel → 每次 Agent 进程启动生成的 ephemeral Connector → 每次连接建立的 Session”。同一 Tunnel 的全部 Connector 复用同一枚当前 ACTIVE Token；Connector 和 Session 不落库，也不维护本地数据目录或锁。Service 是 Tunnel 下的持久代理配置，M1 的静态 Service 仍由 Integration Harness 注入。

## 7.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M1-01 | Tunnel/Connection Token 领域模型 | M0-05、M05-02 | Server Domain/Repository + Token Master Key | Tunnel 与唯一 ACTIVE Token；完整 Token AES-256-GCM 密文、Secret Hash；Connector/Session/实时状态不落库；独立主密钥权限、丢失快速失败与边界测试 | `DONE` |
| M1-02 | Stable Connection Token 创建、获取与验证 | M1-01 | Application Service + Repository | 首次签发单个 `xta_...`；添加 Connector 重复获取逐字节相同 Token且不新增行/版本；CSPRNG、AEAD AAD、常量时间比较、篡改失败 | `DONE` |
| M1-03 | Ephemeral Connector/Session 身份 | M1-01 | Agent 内存身份、Server Registry Key | 身份链为 Tunnel→ephemeral Connector→Session；Connector 每次进程启动生成且仅驻留内存，Session 每次连接生成；同一 Tunnel 多 Connector 并存、同 Connector 重连 generation fencing；无 Connector 持久化 | `DONE` |
| M1-04 | Agent Gateway TLS/ALPN + Server Identity Rotation | M0-05、M0-11、M05-10 | Server Gateway、Token-derived Agent Dialer、`gateway rotate-key --maintenance`、Rotation Journal | 首个 Admin 完成前 Gateway 不启动；Agent 只从 Connection Token 取得 Endpoint/public-or-pinned Trust；TLS1.3；ALPN empty/unknown 拒绝；Handshake 上限；Pinned 证书启动及周期检查、剩余 `<=30` 天复用同一 SPKI 续签、已过期证书在监听前恢复、当前时钟早于 `NotBefore` 时显式失败；共享 `*tls.Config` 发布后不原地修改，按 fresh/immutable/`Clone` 或原子回调发布；Server 停止并持 External Lock 时轮换；新 Pin 只进入后续新 Token；Journal 恢复与私钥 `0600`；Rotation 成功追加 Minimal Security Audit Event；Race 覆盖 Pinned/Public 与续签热加载 | `DONE` |
| M1-05 | Tunnel Token Auth 与 Control Session 建立 | M1-02至 M1-04 | Connector Auth Handler、Session Secret、Session Registry | Token 连接描述/Tunnel 身份/Secret 作为整体校验；Auth Failure 可区分；Success flush 提交点；generation fencing | `DONE` |
| M1-06 | TunnelRuntime 所有权与线性化 | M1-03、M1-05 | Runtime Registry、ActiveWork | 固定 Lock 规则；锁内无 IO/Close/阻塞；计数 exactly-once | `DONE` |
| M1-07 | Control Session Owner/Outbox | M1-05、M1-06 | Single Reader/Writer/Owner、有界队列 | 优先级、合并、Snapshot/ConfigAck 有序、队列满关闭、无 goroutine leak | `DONE` |
| M1-08 | WorkHello HMAC/Lease/Replay | M1-05、M05-08 | Work Auth Handler + Replay Cache | HMAC Vector；Lease 消费与 Replay 原子；无 wall-clock 依赖 | `DONE` |
| M1-09 | WorkPool 与 Budget Lease | M1-06、M1-08 | Server/Agent Work Pool | Connecting/Idle/Opening/Active 有界；Demand generation 合并；Lease 过期 | `DONE` |
| M1-10 | OPEN 状态机 | M1-09 | OpenRequest/OpenResponse 处理 | IDLE→OPENING→ACTIVE/CLOSED；OPEN 只携带 `service_id`；RAW 前传输失败最多在同一 Connector 换 WorkConn 重试一次，M1 不引入跨 Connector 重选；已转发业务字节绝不重试；超时/reset/失败资源只释放一次 | `DONE` |
| M1-11 | RAW Streaming/Half-Close/Cancel | M1-10 | Bidirectional Proxy | `OPEN_OK + RAW` 同 Read 无丢失；Half-Close；Cancel 解除 IO 阻塞 | `DONE` |
| M1-12 | M1 Resource/Timeout/FD Limits | M1-04至 M1-11 | Limit Manager + 配置接入 | Frame/Auth/Queue/Conn/Pending Open/Replay/FD 在真实路径生效 | `DONE` |
| M1-13 | Baseline Reconnect/Graceful Shutdown | M1-07至 M1-12 | Agent Backoff、Server/Agent Drain | 网络/Server 容量错误使用 Jitter Backoff 且遵循 `retry_after`；Token/Pin/Version 永久错误停止快速重试；只在 Session 稳定运行后重置 Backoff；新 generation 不被旧 cleanup 破坏；deadline 后强制关闭 | `DONE` |
| M1-14 | M1 Gate：TCP Echo End-to-End | M1-01至 M1-13 | `tests/integration` 中的 ephemeral Public Listener、静态 Tunnel/Origin Fixture、Echo Origin | Public TCP→Server→Agent→Echo；Harness 不新增临时生产 Schema；下方 Checklist 全通过 | `DONE` |

## 7.2 M1 Gate Checklist

- [x] 逐字节分片、多 Frame 合并、`OPEN_OK + RAW` 同 Read 通过。
- [x] Half-Close、Context Cancel、Origin Reset/Timeout 测试通过。
- [x] Control Reconnect 与旧 Session Cleanup 不影响新 generation。
- [x] Outbox 合并、优先级和满载关闭通过。
- [x] 所有 M1 资源上限在真实分配路径被拒绝，计数不为负。
- [x] 测试结束后 FD 和 goroutine 回到基线。
- [x] `go test ./...` 和 M1 Integration Suite 通过。

M1 的静态 Service 只能由 Integration Test Harness 注入，不得为过渡测试新增一套临时生产配置，也不得提前制造绕开 M3 Application Service 的持久化接口。

---

# 8. M2：Credential Lifecycle & Failover Hardening

## 8.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M2-01 | Multi-Connector Scale/Isolation Suite | M1-14 | 并发 Connector/Session/Pool 测试矩阵 | 同一 Tunnel/Token 的 3 个以上 ephemeral Connector 在连接 churn 下保持独立 Session/Pool/Counter；无饥饿、无计数泄漏、无 Connector 持久化行 | `DONE` |
| M2-02 | Connector Selection Hardening | M2-01 | Selection Soak + Churn Suite | 在 Current Session 替换、DRAINING、Idle/Capacity 快速变化下保持 Least Active + RR 原子公平；Revision/Health Eligible 留给 M3-09 接入 | `DONE` |
| M2-03 | Online Connector Lifecycle/Observability | M1-03、M1-14 | Runtime Lifecycle Events + Query/Metrics | 连接、Session Replacement、DRAINING、断开状态可查询并有结构化日志/指标；Agent 重启产生新 Connector；不维护机器/主机历史 | `DONE` |
| M2-04 | Token Rotate/Revoke | M1-02、M1-14 | Credential Lifecycle Service | Rotate 使用当前 Endpoint/TLS Trust 签发新版本并撤销旧 Token 的新认证；普通 Add Connector 只返回当前同一 Token；Revoke 新认证失败；完整 Token 只加密入库且不入日志 | `DONE` |
| M2-05 | Tunnel Revoke | M2-04 | Tunnel Revoke Workflow | 阻止新 Auth；关闭该 Tunnel 全代 Session/ActiveWork；幂等 | `DONE` |
| M2-06 | Credential/Session Replacement 保留 ActiveWork | M2-04、M1-13 | Tombstone + Cross-generation Cleanup | Rotate 与 Session Replacement 期间旧 Active 自然结束；旧 cleanup 只清 Idle/Opening；`closeOnce` | `DONE` |
| M2-07 | Connector Failover + Pre-RAW Reselect | M2-02、M2-06 | Failover Integration Test | Connector 崩溃后新连接选其他 Connector；RAW 前符合契约的失败可最多跨 Connector 重选一次；已进 RAW 或已转发业务字节不自动重放 | `DONE` |
| M2-08 | M2 Gate | M2-01至 M2-07 | M2 验收证据 | 多 Connector 规模/抖动、Rotate/Revoke、Failover、ActiveWork 保留全通过 | `DONE` |

## 8.2 M2 Gate Checklist

- [x] 同一 Tunnel Token 并发启动 3 个以上 Connector，Server 能独立识别且 Token 文本完全相同。
- [x] 新连接在 Session churn 与容量变化下仍按资格、Least Active 和 RR tie-break 分布，无单 Connector 饥饿/垄断。
- [x] Token Rotate/Revoke 和 Tunnel Revoke 的在线/离线路径通过。
- [x] 旧 Session ActiveWork 自然完成，Revoke 可跨 generation 关闭。
- [x] Connector 崩溃/重连不造成计数泄漏、重复转发或新 Session 被旧 cleanup 清除。

---

# 9. M3：Configuration + Health

## 9.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M3-01 | Service 领域与存储 | M1-14、M0-05 | Domain + SQLite Repository | Service 直接归属 Tunnel；Origin/Health/Enabled/RequiredRevision 不变量、引用关系和容量边界；初始 Schema 预留 UDP/原生 QUIC/文件系统 Unix Socket，当前 Runtime 对预留 Scheme fail closed；无中间关联表 | `DONE` |
| M3-02 | Application Service + Version Transaction | M3-01 | Server Application Service | Service Aggregate 修改在单事务递增 version/revision；并发写不丢失 | `DONE` |
| M3-03 | Snapshot Builder/Size Gate | M3-02 | TunnelSnapshot Builder | 稳定排序、Service 数/字节上限在事务提交前校验 | `DONE` |
| M3-04 | Agent In-Memory Atomic Apply | M3-03、M05-08、M1-07 | Agent Config Runtime + ConfigAck | `Validate → Prepare/Build Candidate → Start Candidate Resources（保持 unpublished/gated，不发 Health、不参与选择）→ Atomic Publish Runtime + Revision + Digest → ConfigAck → 有界 Retire 旧配置/Health 资源`；Retire 不关闭旧 Revision 已进入 ACTIVE 的 WorkConn；Candidate 失败释放自身资源并保留当前 Revision；Digest 在递归 Unknown Field 拒绝后按 Deterministic Snapshot Bytes 计算，且只驻留内存 | `DONE` |
| M3-05 | Token-only Startup/Reconnect + Remote Config | M3-04、M1-05 | Agent Bootstrap/Reconnect Integration | Agent 仅凭 Connection Token 建连；每次启动或重连从 Server 获取完整 Desired Snapshot；Apply 成功并 Ack 前不进入 Eligible；Server 不可达时不上线且无本地配置回退 | `DONE` |
| M3-06 | Snapshot Reconcile/Observed Revision | M3-03至 M3-05、M1-07 | Reconciler + ConfigAck | Single Reconcile Loop；过期 Revision 拒绝，高 Revision debounce/coalesce；构建期间 generation 变化则丢弃旧 Candidate 并直接构建最新 generation；同 Revision 同 Digest 幂等、不同 Digest 协议错误；完整 Apply 的 Ack 后才 Eligible；新 Control Session 重置 observed revision/digest 基线并重新获取完整 Snapshot | `DONE` |
| M3-07 | Origin Resolver | M3-03、M3-04 | Agent Origin Resolver | 仅从当前已原子 Apply 的内存 Snapshot 解析 HTTP/HTTPS/TCP、DNS/IPv4/IPv6、TLS Server Name 与 SSRF 边界 | `DONE` |
| M3-08 | 中心 Health Scheduler | M3-07 | Heap/Wheel Scheduler + Semaphores | 全局/per-origin 并发、Rate、initial/interval jitter；无 per-service ticker；状态机固定为 UNKNOWN 首次成功进入 HEALTHY，UNKNOWN/HEALTHY 连续 `failure_threshold` 次失败进入 UNHEALTHY，UNHEALTHY 连续 `success_threshold` 次成功恢复 HEALTHY，反向结果重置连续计数 | `DONE` |
| M3-09 | Health Batch/Revision Fencing + Eligible Selection | M3-06、M3-08、M2-02 | Pending Accumulator、Batch Reporter、完整 Connector Eligible Filter | `service_id` 合并；出队分配 generation；Control 重连且完整 Snapshot Apply/Ack 后，Owner/Outbox 按新 Session generation 在 ConfigAck 后立即发送当前 Revision 全量 Health，之后再发增量 Batch；将 required/observed Revision 和 Per-Service Health 接入 M2 Selection；UNKNOWN/旧 Revision Health 不放行 | `DONE` |
| M3-10 | Health Target Budget Manager | M3-01、M3-08、M2-06 | Reserve/Commit/Release Manager | `(tunnel_id,connector_id)` 所有权；固定锁顺序；重连不双计费/误释放 | `DONE` |
| M3-11 | Tunnel/Connector/Service Status | M3-06、M3-09、M3-10 | `internal/server/status` | 状态优先级唯一；Origin Health 不污染 Tunnel/Connector；Web 不重算 | `DONE` |
| M3-12 | Durable Operations：Backup/Restore | M0-05、M1-01、M1-04、M3-02 | `backup create/restore`、Backup Manifest、Restore Journal | 在线 Create 通过本机控制通道建立 Config Write Barrier；离线 Create/Restore 使用同一 Stable Target External Lock；把 SQLite + data-dir-owned Pinned Gateway TLS Identity + Tunnel Token Master Key 作为同一一致性单元；Manifest/Hash/Schema 校验；同盘 staging/rollback/journal 可恢复 | `DONE` |
| M3-13 | M3 Gate | M3-01至 M3-12 | Application Service Integration + Server Durable Operation Crash Tests | 下方 Checklist 全部通过 | `DONE` |

## 9.2 M3 Gate Checklist

- [x] 通过 Application Service 修改 Origin，Agent 无需重启即生效。
- [x] Snapshot 的 Deterministic Bytes、Revision、大小和 Service Count 边界均可自动化验证。
- [x] Agent 以 Validate/Prepare/Start Gated Candidate/Publish/Ack/Retire 顺序原子替换内存配置；失败保留当前 Revision；Same Revision/Same Digest 幂等、Same Revision/Different Digest 拒绝，且 Candidate 资源生命周期可自动化验证。
- [x] Agent 启动/重连必须拉取完整 Desired Snapshot；Server 不可达时不上线且无本地配置回退。
- [x] Health Rate/Concurrency/Jitter/Batch/Revision Fencing、三态阈值、重连后的全量恢复 Batch 通过。
- [x] 超过 Tunnel/Global Health Target Budget 的 Config Write 和 Connector Auth 被拒绝。
- [x] 满容量 Session Replacement 不 Double Reserve，旧 cleanup 不释放新 Reservation。
- [x] `backup create/restore` 在线/离线路径通过，Manifest 覆盖 SQLite、Gateway TLS Identity 与 Tunnel Token Master Key，Restore 不与旧目录合并；Server Journal 在各提交点崩溃后可恢复。

---

# 10. M4：HTTP + TCP Product Data Plane

## 10.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M4-01 | Immutable Route Snapshot | M3-02、M3-11 | `internal/server/route` | SQLite Desired State 是唯一权威；Single Reconcile Loop 全量构建 Immutable Route Snapshot；多个 dirty wakeup 合并，Build 期间 generation 前进则丢弃旧结果并立即按最新 generation 重建；Atomic Swap，读路径无 SQLite，不完整 Snapshot 不发布；不提前增加 `coalesce_window` 配置 | `DONE` |
| M4-02 | HTTP Host/Path Router | M4-01 | HTTP Matcher | Host 规范化、IDNA、端口与路径段边界；非根 Route Prefix 移除全部尾部 `/`，`/foo` 与 `/foo/` 构成重复语义 Route；公网请求不使用通用 `path.Clean`，RawPath 为空是正常输入，非法 percent 编码、Path/RawPath/RequestURI 不一致、encoded slash/backslash、明文或编码 dot-segment、控制字符、非法 UTF-8、多重危险编码或 Router/Origin 无法保证一致解释时返回 `400 INVALID_PATH`；请求与转发保留 `/foo`、`/foo/`、`/foo//bar` 原始语义 | `DONE` |
| M4-03 | Streaming Reverse Proxy | M4-02、M1-11 | HTTP Ingress Proxy + Tunnel-aware Transport | 不缓冲整请求/响应；1GB upload/download；Context Cancel；Host 严格按 `origin_http_host > preserve_host > origin host` 决定；HTTP/HTTPS 实现 disable chunked、90s idle timeout、100 max idle 默认值，禁用 Chunked 且长度不可安全确定时显式拒绝而不整体缓存；HTTP/HTTPS Origin 同样遵守 disable Happy Eyeballs=false、TCP KeepAlive=30s/0 禁用与 DNS/TCP/TLS 共享 connect timeout；连接池至少以 TunnelID+ServiceID+配置版本隔离，新建 WorkConn 的 Connector Service RequiredRevision 必须与 Route 精确相等，且受全局 WorkConn/FD 硬预算限制；一个 WorkConn 对应一条 HTTP/1.1 TCP Connection 而非一个 Request，验证同隔离键连续请求复用且跨键绝不复用；M4-05 前 Upgrade 必须在 Reverse Proxy 前显式拒绝 | `DONE` |
| M4-04 | Forwarded/Trusted Proxy 边界 | M4-03 | Header Sanitizer + Peer Normalizer | 仅信任配置 CIDR 中的实际 TCP Peer；单个 XFF Header 最多 32 跳并从右向左验证；重复/空/非法可信代理元数据返回 `400 INVALID_FORWARDED_HEADER`；未受信 Peer 不解析伪造值；删除全部外部 Forwarded/X-Real-IP/未知 X-Forwarded-*，只重建权威 For/Proto/Host | `DONE` |
| M4-05 | WebSocket Upgrade | M4-03、M4-04 | WebSocket Proxy | 无 Request Body 的 Upgrade、双向流、Half-Close/断连、长连接 Timeout；已知超限 Body 返回 413，其余带 Body/Transfer-Encoding 的握手返回 501，均在 Tunnel Dial 前拒绝并关闭客户端复用 | `DONE` |
| M4-06 | TCP Listener Manager | M3-02、M4-01 | Listener Reconciler | `min_port..max_port` 是逻辑预留池，不预监听全范围；Route 可显式选端口或在事务内自动选择，具体端口持久化且全局唯一；端口范围/保留端口冲突在事务前拒绝，事务内重验并同步推进 Service Version/RequiredRevision、Tunnel DesiredRevision 与全局 Route Generation；OS `Listen(port)` 失败不回滚 Desired State，只标记对应 Service `LISTEN_FAILED` 并由 dirty wakeup + 周期扫描重试，其他 Listener 继续；同端口原子更新准入快照，换端口先监听新端口再释放旧端口；删除/禁用收口、重启恢复、有限 Drain/Close 与 FD 峰值预算 | `DONE` |
| M4-07 | Raw TCP/SSH Data Plane | M4-06、M1-11、M2-02 | TCP Ingress | SSH/Raw TCP 逐字节转发；无协议特判；`connect_timeout` 继续统一约束 DNS/TCP/TLS 总预算；disable Happy Eyeballs 默认 `false`，启用时不延长总预算；Origin TCP KeepAlive 默认 30s、`0` 禁用；错误映射稳定 | `DONE` |
| M4-08 | Caddy/Nginx HTTPS 集成 | M4-03、M4-05 | Deploy Example + E2E | HTTPS 在前置代理终止；Host/Origin/Forwarded 语义正确；Caddy 使用有限刷新并传播客户端取消；固定多架构镜像摘要，原生 amd64/arm64 CI 分别运行真实 HTTPS/WSS/断开收敛 E2E | `DONE` |
| M4-09 | Public Ingress Limits | M4-03、M4-06 | Per-source/Service/Tunnel/Global Limits | LRU 有界 + TTL；HTTP Rate/Body/Header，Body 上限只由 Server Schema 裁决；TCP Accept/Open/Active 上限 | `DONE` |
| M4-10 | M4 Gate | M4-01至 M4-09 | Product Data Plane E2E | HTTP/HTTPS/WebSocket/SSH/Raw TCP 全部通过 | `DONE` |

## 10.2 M4 Gate Checklist

- [x] HTTP Host + Path 路由通过，含 canonical Route Prefix、RawPath/RequestURI/encoded separator/dot-segment 歧义拒绝矩阵，以及公网请求重复斜线、Trailing Slash 保留语义。
- [x] Caddy/Nginx 后 HTTPS 和 WebSocket 通过。
- [x] 1GB Upload/Download 不整体缓冲，内存不随 Body 线性增长。
- [x] HTTP Service Proxy 选项默认值、Host 优先级、disable-chunked 拒绝分支、Transport/WorkConn 隔离键和全局预算均有自动化证据。
- [x] SSH 和通用 Raw TCP 可持续传输、Half-Close 和取消。
- [x] HTTP/HTTPS/TCP Origin 共用的 Happy Eyeballs 开关、TCP KeepAlive 间隔与 `connect_timeout` 总预算通过真实 Dial 路径验收。
- [x] Route/Listener Snapshot 并发更新无窗口期错路由；Reconcile 风暴只发布最新 generation，旧 Candidate 不覆盖新 Desired State。
- [x] Public Ingress 所有上限在真实入口生效。

---

# 11. M5：OpenAPI + REST API + Web Console

## 11.1 Entry Gate

M5-01 通过前，Handler 和 Web 只能建骨架，不得各自定义 DTO、Nullable、错误码或分页语义。

对外 HTTP API Handler 可采用 Gin，但框架只作为 HTTP 适配层；OpenAPI、Generated Server Contract 和 Application Service 分别继续承担契约、传输类型和业务逻辑权威。首次引入 Gin 时仍须确认并锁定精确版本，禁止为尚未实现的 Handler 提前加入依赖。

## 11.2 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M5-01 | 冻结完整 OpenAPI | M3-02、M3-11、M4-02 | `api/openapi/openapi.yaml` | 全部 Schema/Required/Nullable/Error/Status/Pagination/PATCH/ETag 完整；冻结 Security Audit Event 只读查询 Schema、稳定分页与错误结构，不提供 UPDATE/DELETE；Lint/Breaking PASS | `DONE` |
| M5-02 | 生成 Client/Server Contract | M5-01 | Go Server Types + TypeScript Client | 可重复生成；干净 checkout 零漂移 | `DONE` |
| M5-03 | Admin Login/Session/CSRF | M5-02、M0-08、M0-11 | Auth Handler + Web Login | Secure/HttpOnly/SameSite Cookie；Origin/Host 规则；Login/Logout/CSRF E2E | `DONE` |
| M5-04 | Tunnel/Connector/Credential API | M2-08、M5-02 | REST Handler | Tunnel CRUD/Tunnel Revoke；Token Reveal/Rotate/Revoke；Add Connector/Reveal 返回当前同一 Token；Connector 列表只读运行态；`Cache-Control: no-store` | `DONE` |
| M5-05 | Service API | M3-13、M4-10、M5-02 | REST Handler | Service 直接归属 Tunnel；调用既有 Application Service；不在 Handler 重写事务逻辑 | `DONE` |
| M5-06 | PATCH/ETag/Pagination 并发契约 | M5-04、M5-05 | Handler + Repository Tests | Tunnel 和 Service Aggregate 均覆盖 428/412；omitted/null/value；opaque token 50/200；version 原子递增 | `DONE` |
| M5-07 | Settings/Read-only Runtime/Audit API | M5-02 | Settings/Audit Handler | 只返回允许公开的有效配置；Audit Query 只读且分页稳定；敏感字段永不返回，不泄露 Secret | `DONE` |
| M5-08 | Dashboard/Status UI | M5-02、M5-04、M5-05 | React Pages | 直接渲染 Server Status；不在前端重算状态 | `DONE` |
| M5-09 | Tunnel/Connector/Service 管理 UI | M5-03至 M5-08 | Tunnel CRUD/Token/Connector View/Service CRUD | 日常操作无需 SQLite 或手改 Agent Service Config | `DONE` |
| M5-10 | Contract/E2E Test Suite | M5-02至 M5-09 | API Contract + Browser E2E | 错误码、并发 PATCH、CSRF、Token no-store、生成漂移全覆盖 | `IN_PROGRESS` |
| M5-11 | M5 Gate | M5-01至 M5-10 | M5 验收证据 | 下方 Checklist 全部通过 | `NOT_STARTED` |

## 11.3 M5 Gate Checklist

- [ ] OpenAPI Lint、Breaking 与 Generated Drift Check 通过。
- [ ] API 实际响应与 OpenAPI Contract 零漂移。
- [ ] 并发 PATCH 不丢失更新，缺少 If-Match 返回 428，冲突返回 412。
- [ ] 分页 Token 不可由前端解析，默认 50，最大 200。
- [ ] Login/Secure Cookie/CSRF/Logout 完整 E2E 通过。
- [ ] Tunnel/Create Connector Guide/Service/Token 日常工作流可在 Web 中完成。

---

# 12. M6：Observability

## 12.1 任务清单

| ID | 任务 | 依赖 | 产物 | 验收要点 | 状态 |
| --- | --- | --- | --- | --- | --- |
| M6-01 | 全链路 JSON Logging | M1-14、M5-11 | 稳定日志字段 | request/trace/session/connection 可关联；Secret 脱敏；级别正确；Security Audit 的结构化导出只来自已提交的 append-only Event，不以允许丢失的 Runtime Observer 代替；Windows SCM 模式提供可持久检索的 Event Log Source 或等价受支持 Sink，不能仅依赖不保证可见的 stderr | `NOT_STARTED` |
| M6-02 | Prometheus Metrics | M4-10 | `/metrics` + Metric Registry | 请求数/错误率/P50/P99、Session/Pool/Limit/Health；增加 Open、Origin Connect、Reconcile Duration Histogram，有限枚举 `error_code` Counter，Snapshot Bytes/Service Count/Coalesced Update 指标，以及 Gateway Certificate Expiry；禁止 tunnel/service/connector/connection ID 高基数 Label | `NOT_STARTED` |
| M6-03 | OpenTelemetry Trace | M4-10 | Server→Agent Trace Propagation | `ingress.Accept→tunnel.DialContext→transport.Acquire→origin.Dial→proxy.Bidirectional` 可关联 | `NOT_STARTED` |
| M6-04 | Usage Aggregation | M4-10、M0-05 | Usage Buffer/Flush/Repository | 字节/连接计数 exactly-once；Batch Flush；minute/hour/day Rollup 幂等且 Crash 后可重跑；先提交汇总再删除已 Rollup 明细；Retention、Compaction 与 Vacuum 策略由本任务容量 Benchmark 冻结，若决定可配置则先修改 Server Schema；重启无负数、重复或明细无限增长 | `NOT_STARTED` |
| M6-05 | Error/Status Observability | M3-11、M6-01、M6-02 | Error Code Dashboard Data | Tunnel Offline/Connector Offline/Origin Down/No Capacity/Protocol Error 可区分 | `NOT_STARTED` |
| M6-06 | 运维诊断流程 | M6-01至 M6-05 | Runbook + Dashboard + Agent Connectivity Diag | Diag 复用生产 Token Parser、Endpoint/DNS、Dialer、TLS Builder、Pin Verifier 和 ALPN，覆盖 DNS/TCP/TLS/Pin/ALPN/Auth/Snapshot Receive；不得复制宽松连接栈，也不得把完整 Token 写入 argv 作为唯一入口；输出 PASS/WARNING/FAIL 与 READY 变体；覆盖证书 30/7/1 天告警、Audit 查询/导出、Linux systemd 与 Windows SCM 的启动失败、恢复重启和 30s Stop/Shutdown 超时诊断 | `NOT_STARTED` |
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
| M7-01 | Limits/Timeout/Rate Benchmark | M1-12、M3-10、M4-09 | `tests/benchmark` + 调优证据 | 对 16/32/64 KiB Proxy Buffer、HTTP/1.1 WorkConn Capacity、Connector Selection CPU/Allocation 做 Benchmark；只依据本项目结果调整 Server Schema 默认值，不删除预算维度；记录 CPU/RAM/FD 环境 | `NOT_STARTED` |
| M7-02 | Reconnect Storm/Backoff/Fencing | M2-07、M6-02 | Chaos Test | 100/500/1000 Connector 使用 Stagger + Jitter 重连，无同步 TLS/Auth Storm；永久错误不快速重试；记录 Pending TLS/Auth、`retry_after`、FD/CPU/RAM；Server Restart 后测量 `T_control_reconnect`、`T_config_ready`、`T_workpool_ready`、`T_first_success` 分布，旧 generation 无污染 | `NOT_STARTED` |
| M7-03 | Graceful Shutdown Chaos | M1-13、M4-10 | Server/Agent Drain Test | 使用真实 TCP Half-Close、HTTP Streaming、WebSocket 和 Slow Origin 覆盖每个 Drain 阶段的丢包、延迟与对端消失；Graceful Period 后进入 Hard Deadline 并主动 Force Close；最终 FD/goroutine/计数归零 | `NOT_STARTED` |
| M7-04 | Server Persistence/Filesystem Failpoints | M0-05、M1-04、M3-12 | Crash/EIO/Disk-full Suite | Server SQLite Migration、Gateway Rotation Journal、Backup/Restore 的 write/fsync/rename 断点；验证 Backup ACK 前最终路径不可见，并评估 SIGKILL 遗留私有隐藏候选的显式安全清理策略，禁止并发 Create 下按前缀盲删；只验证 Server durable operation 的异常注入和恢复收敛，不首次实现维护命令 | `NOT_STARTED` |
| M7-05 | Race/Concurrency Suite | M2-08、M3-13、M4-10 | Race CI Job | `go test -race ./...`；Session Replacement、Config Write、Usage Flush、Listener Reconcile、共享 TLS Config/证书热加载；记录 TunnelRuntime Mutex/Block Profile 与 Connector Selection 热路径 Profile | `NOT_STARTED` |
| M7-06 | Protocol/Parser Fuzz | M05-10、M4-10 | `tests/fuzz` | Canonical/non-canonical UVarint、Frame/Envelope/WorkHello/Host、RawPath/RequestURI/encoded separator/dot-segment、Forwarded Header；Crash/OOM/无界分配为零 | `NOT_STARTED` |
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
- [ ] 已记录 Server Restart Recovery、HTTP/1.1 WorkConn Capacity、16/32/64 KiB Buffer、TunnelRuntime Mutex/Block Profile 的环境、结果、推荐默认值和容量边界。
- [ ] 发布文档明确 Alpha 限制与 V0.1 不支持能力。

---

# 14. 当前可立即执行的任务队列

当前 `M0-01`、`M0-03` 至 `M0-08`、`M0-10`、`M0-11`、`M05-01` 至 `M05-10`、`M1-01` 至 `M1-14`、`M2-01` 至 `M2-08`、`M3-01` 至 `M3-13`、`M4-01` 至 `M4-10`、`M5-01` 至 `M5-09` 已完成。当前待办为：

1. `M5-10` — Contract/E2E Test Suite 已进入 `IN_PROGRESS`；覆盖实际响应与 OpenAPI、错误码、并发 PATCH、全 Mutation CSRF、Secret no-store，以及真实 Server/SQLite/HTTPS/Chromium 管理工作流。
2. `M0-09` — 正式 Dockerfile 双架构、Windows SCM 与本轮双架构 systemd Packaging Smoke 均已通过，仍等待独立部署阶段复审后再决定是否 `DONE`。
3. `M0-02` — Token-only Bootstrap 等待用户复审。

`M0-12` 仍是 Alpha 前 Gate。M2、M3、M4、M5-01 至 M5-09 已完成；M5-10 正在实现，M5-11 继续等待其 `DONE`。M0-02 与 M0-09 保留各自独立 Review 边界，不因本次证据闭环自动转为 `DONE`。

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
| OpenAPI Validator/Generator | M0-07/M5-02 开工前 | M0-07 已批准并锁定 vacuum `v0.30.0` 官方 Linux amd64/arm64 归档与二进制 SHA-256；M5-02 已批准并锁定 `oapi-codegen v2.8.0`、`oapi-codegen/runtime v1.6.0`、`nullable v1.1.0`、`openapi-typescript 7.13.0`、工具侧 TypeScript `5.9.3` 与 `openapi-fetch 0.17.0`。唯一入口为 `tools/openapi.sh validate|breaking|generate|generate-check`，CI 不维护第二套方式；TypeScript Generator 因 Web TypeScript 6 Peer Range 冲突隔离在 `tools/openapi-ts`，不得使用 `--force` 或 `--legacy-peer-deps` 绕过。 |
| Web 依赖与 Node 版本 | M0-08 开工前 | 已批准 Node `24.19.0`、npm `11.17.0`、React/React DOM `19.2.8`、Vite `8.2.2`、Plugin React `6.1.0`、TypeScript `6.0.2` 与对应类型包；用户在管理菜单出现真实图标需求后追加批准 `lucide-react 1.34.0`，并在 M5-10 追加批准 `@playwright/test 1.62.1`；直接依赖精确锁定，npm 11 生成并提交 Lockfile，CI 只运行 `npm ci`，再通过本地锁定的 Playwright CLI 安装对应 Chromium；Tailwind/shadcn/Router/Query 等继续等待 M5 真实使用点 |
| OCI 基础镜像、Compose 双栈与跨平台 Agent Service 权限模型 | M0-09 开工前 | 已批准三个固定多架构基础镜像摘要、Compose 双栈 Profile 与原生 tcp4/tcp6 监听原语；OCI 使用 `65532:65532` 与只读根，只有 Server 挂载 Data Volume 和 `/run/xtunnel` tmpfs，Agent 无 Volume，从 `XTUNNEL_TOKEN` 取得 Token 并默认执行 `run`；Compose 输入 `XTUNNEL_AGENT_TOKEN` 映射到容器环境；Server 保留 Shell 包装。Agent 在 Linux/Windows 统一使用 Binary `service install --token` 与 `service uninstall`，不提供用户安装脚本。Linux 要求 root/systemd>=249，原子安装到 `/usr/local/bin/xtunnel-agent`，Credential 目录/Source 为 `root:root 0700/0600`，Unit 首行为 `# Managed by xtunnel-agent service install` 且 `ExecStart=/usr/local/bin/xtunnel-agent run`。Windows 支持 amd64/arm64，要求提升权限的 Administrator 与 SCM；ServiceName=`XTunnelAgent`、DisplayName=`XTunnel Agent`、账户=`NT AUTHORITY\LocalService`，Binary=`%ProgramFiles%\XTunnel\xtunnel-agent.exe`，Credential=`%ProgramData%\XTunnel\credentials\agent.token.dpapi` 并使用 `CRYPTPROTECT_LOCAL_MACHINE | CRYPTPROTECT_UI_FORBIDDEN`，SCM ImagePath 仅含安装 Binary + `run`，Description marker 精确为 `Managed by xtunnel-agent service install`；重复安装使用 `MoveFileEx(REPLACE_EXISTING | WRITE_THROUGH)`，Stop/Shutdown 最多 30s，运行异常返回非零并配置 non-crash recovery。两端均拒绝覆盖/删除非受管同名服务，卸载删除受管服务并保留平台 Credential；Windows 从运行中已安装 EXE 自卸载时使用 `MoveFileEx(DELAY_UNTIL_REBOOT)` 安排重启删除 Binary，Linux 另保留服务用户 |
| Minimal Security Audit Event Contract | M1-04 收口前 | 用户于 2026-08-26 明确确认数据库 Schema 变更；已冻结 bounded/nullable、`event_id`/`operation_id`、`event`/`action` 枚举、actor/resource/result、稳定失败语义和幂等边界，并以 `000003_security_audit_events.sql`、Repository 校验和 v2 Rotation Journal 落地。Security Audit append-only，禁止 UPDATE/DELETE，Secret/Credential/Private Key/Cookie 禁止入库；M1 写事件，M5 提供只读查询，M6 提供结构化导出、Dashboard 和 Runbook |
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

## 2026-08-25 · M05-10 · 本地提交与干净 Checkout 证据

- Commit/PR：`0294999edc07a24bc7d29d5efd71e9aadf218ba7`（本地提交，未推送）。提交范围为 M0.5 Protocol v1 的 Proto、初始 Baseline、生成代码、协议实现/测试、Golden Vector、工具脚本和开发计划；没有包含无关文件。
- 验收命令：Windows `go1.27.0` / `GOTOOLCHAIN=local` 下执行 `go mod verify`、`go test -count=1 ./...`、`go test -race -count=1 ./internal/protocol/...`、`go vet ./...`、`git diff --cached --check` 均通过。以该 Commit 创建原生 WSL 干净 Git 克隆，复制受管但被忽略的 `.tools/bin` 后确认工作树为空，并执行 `./tools/proto.sh lint`、`breaking`、`generate-check`，均通过；临时克隆已删除。
- CI 与 Gate：本提交未推送，GitHub Actions 无法取得该 Commit，故没有新的 CI Run；按 M0-10 的全局证据规则，M05-10 仍保持 `IN_PROGRESS`，M05-01 至 M05-09 仍保持 `REVIEW`，M0.5 与全局 `DONE` 计数不变，M1 继续锁定。本轮不勾选任何产品任务。
- 解锁条件：用户若授权推送此提交，待 GitHub Actions 对 `0294999` 的 CI 全绿后，再将 M05-10 置为 `DONE`；随后按用户的阶段 Review 规则停下，等待继续 M1 的明确指令。

## 2026-08-25 · M1-EARLY-START · 用户授权

- 决策：用户明确授权“允许 M05-10 未 DONE 时开始 M1”。该授权仅解除 M1 的开发顺序阻塞，绝不把 M05-10 的 `REVIEW` 状态、缺失 CI 证据或任何 M0/M0.5/M1 发布 Gate 伪装为通过。
- 实施范围：M1 里程碑转为 `IN_PROGRESS`，先启动不依赖数据库 Schema 或 Protocol 字段变更的 M1-04。Connection Token、Proto 和 Server 配置继续以既有机器契约为准；若后续任务需要数据库迁移、依赖、公共 API、Protocol 或配置变更，仍按项目确认规则单独请求授权。
- 验收边界：M1-14 和任何 `DONE` 状态仍须满足原有依赖、关键测试与 CI 证据；M05-10 的正式完成继续等待用户自行推送后产生的 CI Run。

## 2026-08-25 · M1-01 · REVIEW

- 负责人：Codex；用户已明确确认新增 SQLite Schema/Migration。新增前向 Migration `000002_agents.sql`，为逻辑 Agent 与 Connection Token 元数据建立 `agents`、`agent_tokens`、外键、Token Version/Hash 唯一性和每 Agent 单 ACTIVE Token 约束；未修改既有 Migration。
- 产物：新增 Repository 领域模型、SQLite `BEGIN IMMEDIATE` 事务边界和表驱动的领域/Schema/迁移升级/事务回滚测试。完整 `xta_...`、Endpoint、TLS Trust、认证 Secret、Instance、Session 与实时 Agent Status 均不落库；Token Secret Hash 固定为 32 字节，错误路径不输出其内容。
- 验收命令：`GOTOOLCHAIN=local go test -count=1 ./internal/repository/...`、`go test -race -count=1 ./internal/repository/...`、`go vet ./internal/repository/...`，以及整合后的 `go test -count=1 ./...`、`go test -race -count=1 ./internal/repository/... ./internal/server/gateway ./internal/agent/gateway ./internal/server/bootstrap`、`go vet ./...`、`git diff --check` 均通过。
- 状态影响：M1-01 转为 `REVIEW`；M1 里程碑仍为 `IN_PROGRESS`，计数不增加。M1-02 的 Token 签发与认证接入、M1-03 的运行身份仍按其依赖和任务边界后续实施。

## 2026-08-25 · M1-04 · IN_PROGRESS

- 负责人：Codex；用户已明确确认实现冻结的 `gateway rotate-key --maintenance` 命令。当前落地 Pinned/Public TLS Identity、TLS 1.3、Control/Work 精确 ALPN、10 秒有界握手、Pending Handshake 上限、Rotation Journal/同盘原子替换、Agent Token-derived Dialer，以及“首个 Admin 前不监听、成功创建后只启动一次”的生命周期接入。
- 续签与热加载：Pinned 证书在启动及常驻 Server 的 24 小时检查中，于剩余 `<=30` 天时复用原私钥签发新证书；新证书先同目录临时写入/fsync/原子替换，再经 `tls.Config.GetCertificate` 发布给后续握手，旧连接不受影响。失败保留旧有效身份并暴露最近续签错误，供后续 M6 日志与 Metric 接入；不新增配置项、依赖或 Protocol。
- 验收：Gateway Identity/ALPN/预算、自动续签/同 SPKI/热加载/失败保留旧身份、Agent Pinned SPKI 与未知 ALPN、首个 Admin 后实际监听、以及 External Lock 冲突不换钥/释放后成功轮换的定向测试均通过；整合后 Windows `go mod verify`、全包 Test、相关 Race、Vet 与双 Diff Check 通过。Linux 专属 Bootstrap E2E 已以 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c` 和 `GOOS=linux GOARCH=amd64 go vet` 编译/静态检查，但 WSL 未安装 Go 且 Docker 不可用，未获得真实 Linux 运行证据。
- 未完成：`gateway rotate-key` 成功后的 Minimal Security Audit Event 契约与 append-only 写入路径尚未落地；其 SQLite Migration 属数据库 Schema 变更，实施前仍需单独确认。`xtunnel_gateway_certificate_expiry_seconds` 与 30/7/1 天告警已归属 M6-02/M6-06，不再阻塞 M1-04。M1-04 因 Audit 写入缺口保持 `IN_PROGRESS`。

## 2026-08-25 · M1-02 · REVIEW

- 负责人：Codex。新增 Connection Token 首次签发与验证应用服务，并扩展事务内 Repository 查询；M1 只允许每个 Agent 签发第 1 代唯一 `ACTIVE` Token，Rotate/Revoke 保留给 M2，未提前实现。
- 安全边界：认证 Secret 由 `crypto/rand` 生成 32 字节，持久化层只接收 SHA-256 摘要；完整 `xta_...` 只随成功结果一次性返回。服务端先使用冻结的 Protocol v1 Parse 校验文本 Token，再精确核对 Agent/Token/Version，并以常量时间比较摘要；Endpoint 与 TLS Trust 不写入 Repository。
- 验收命令：`GOTOOLCHAIN=local go test -count=1 ./...`、`go test -race -count=1 ./internal/application ./internal/repository ./internal/repository/sqlite`、`go vet ./...`、`go mod verify`、`git diff --check`、`git diff --cached --check` 均通过；覆盖 CSPRNG 失败、Agent 不可用、重复 ACTIVE/版本、身份/摘要不符、失效 Token、畸形 Token 和 ULID 边界。
- 状态影响：M1-02 转为 `REVIEW`，无 CI Run、未完成 M1-04，故不进入 M1-05，也不增加任何 `DONE` 计数。

## 2026-08-25 · M1-03 · REVIEW

- 负责人：Codex。新增仅驻留内存的 Agent Instance 身份和 Server Session Registry：`ai_`/`sess_` 均为 CSPRNG 生成的 26 位大写 Crockford ULID；同一进程重连复用 Instance，新进程重新生成，未提供持久化、导入、恢复或目录锁接口。
- 并发边界：认证成功后才原子安装 Session；同一 `(agent_id, instance_id)` 重连递增 generation，旧连接只能在 Agent、Instance、Session 和 generation 全部相等时清理当前项。重复 Session ID 明确拒绝，锁内不执行认证、IO 或连接关闭。
- 验收命令：`GOTOOLCHAIN=local go test -count=1 ./internal/identity ./internal/server/runtime`、`go test -race -count=1 ./internal/identity ./internal/server/runtime`、`go vet ./internal/identity ./internal/server/runtime`，以及本轮整合的全量 Test/Race/Vet、`go mod verify` 和双 Diff Check 均通过；覆盖非法 ID、随机源失败、进程重启、64 路并发替换、旧 cleanup fencing 与 Session ID 冲突。
- 状态影响：M1-03 转为 `REVIEW`。认证 Handler 尚未接入（M1-05），无 CI Run，M1 里程碑仍为 `IN_PROGRESS` 且 `DONE=0`。

## 2026-08-25 · ARCH-RESET · Tunnel / Connector / Service 对齐 Cloudflare

- 用户决策：管理端创建 Tunnel；Tunnel 下可添加多个 Connector；同一 Tunnel 的全部 Connector 使用完全相同的当前 Token，默认共同承载新连接并互为备份；全部代理 Service 直接挂在 Tunnel 下。旧记录中的“逻辑 Agent / Instance / 旧 Tunnel / Binding”语义由本记录明确废止，分别替换为“Tunnel / Connector / Service / 无 Binding”。`xtunnel-agent` 只保留为 Binary 名称。
- 契约变更：Protocol v1 改为 `tunnel_id=tun_...`、`connector_id=con_...`、`service_id=svc_...`；`ConnectorAuth*`、`TunnelSnapshot/ServiceConfig`、`ServiceHealth*` 与 `OpenRequest.service_id` 成为新机器契约。M05-01 至 M05-03、M05-05 至 M05-09 完成产物并保持 `REVIEW`；M05-04 因新契约正式 `generate-check` 未完成而回到 `IN_PROGRESS`，M05-10 同样保持 `IN_PROGRESS`。旧干净 checkout/CI 证据不能证明新语义 Gate。
- M1 产物：未发布 `000002` Migration 重做为 `tunnels/tunnel_tokens`；Connector/Session/实时状态不落库。完整 Token 使用 AES-256-GCM 密文保存，AAD 绑定 Tunnel/Token/Version，认证仍使用 SHA-256 Secret Hash 常量时间比较。创建后的 `Current` 每次返回与首次签发逐字节相同的 ACTIVE Token，不新增行或 Version。独立 32 字节主密钥使用 `credentials/tunnel-token.key`，Linux 权限 `0600`；已有密文但密钥缺失/损坏时启动失败。
- Runtime 与负载：Registry Key 改为 `(tunnel_id, connector_id)`；同 Connector 重连使用 generation fencing，同 Tunnel 多 Connector 可并存。M1 已在真实 TCP Echo 数据面接入 Current Session、DRAINING、Idle/Capacity 过滤与 `Least Active + Round Robin tie-break` 原子租约，并保留旧 generation ActiveWork tombstone；M2-02 改为覆盖 Session churn 与容量快速变化的规模化公平性加固，不重复实现默认负载基线。
- 本地验收：`go1.27.0`、`GOTOOLCHAIN=local`；`go test -count=1 ./...`、`go test -race -count=1 ./...`、`go vet ./...`、`git diff --check`、`git diff --cached --check` 通过。Protocol `lint/breaking`、Golden 与连续生成 Hash 由同版本受管工具通过；新契约的正式 `generate-check` 和独立 Review 仍待干净 checkout/CI，因此不勾选 M05-10 或任何 M1 DONE。
- 工作区边界：本轮未暂存、未提交、未推送。原有 staged 文件仍是旧快照，且旧 `000002_agents.sql` 显示为 staged-add/worktree-delete；提交前必须由用户自行重新暂存全部最新文件，禁止直接提交当前 Index。

## 2026-08-25 · M1-05 至 M1-14 · REVIEW（核心能力）

- 负责人：Codex；用户授权在 M05-10 尚未 `DONE` 时先完成核心能力，并要求每个阶段完成后停下等待 Review。本轮只完成 M1，不进入 M2，也未暂存、提交或推送。
- Control 与身份：同一 Tunnel 的当前 Token 可被多个 ephemeral Connector 同时使用；Control Auth 在 Success flush 前以可回滚 replacement 链原子预安装 Session 并执行 Connector 配额准入，写入失败恢复最近仍健康的旧代，嵌套重连会跳过已失败或已清理节点且不覆盖更新 Current。Success Frame 完整写出后立即终结旧链，后续协议状态或交接失败只清理新 Session，不会复活旧代；同 Connector 重连继续由完整 generation identity fencing。Control Reader、Writer 与 Owner 单一化，有界 Outbox 支持优先级、合并、满载关闭，并按实际 Wire Size 拆分 Health Batch、拒绝单项超限。
- Work 与数据面：WorkHello 使用 Session Secret HMAC、一次性 Lease 与 Replay 原子校验；Server/Agent WorkPool 覆盖 Connecting、Idle、Opening、Active。Server 使用本地单调 Heartbeat Timeout 清理失联 Session，并在 Heartbeat 上对账已耗尽 Demand 与池缺口；有效 Lease 继续合并，已消费 WorkConn 形成缺口时发送更高 generation 的补充 Demand。Tunnel 数据面优先从有 Idle Work 的 Eligible Connector 执行 Least Active + Round Robin；候选 Idle 被并发抢走时立即回到 Tunnel 级选择，只有全部暂时无 Idle 才按 Tunnel 共享一个 Pending Group，选择一个最佳 Connector，发布绝对 Pending 目标并在统一 `work_acquire_timeout` 内等待；Drain/Session 关闭仍在同一总超时内重选。目标下降立即发送无 Grant 的更高 generation，并撤销 Server 侧旧 Budget Lease；OPEN 仍只传 `service_id`，RAW 前仅允许在同一 Connector 内换一个 WorkConn 重试一次，业务字节开始后不重试。
- 流式与退出：双向 RAW Proxy 覆盖同读缓冲保留、TCP Half-Close、Context Cancel、Origin Timeout/Reset。Agent 收到进程退出信号后发送唯一 DrainRequest；Server 立即撤下 WorkAuth 与选路资格，等待 OPENING 收束、关闭非 ACTIVE WorkConn 并返回匹配且幂等的 DrainAck；ACTIVE 自然结束，固定 Deadline 后才强制关闭。
- 资源限制：共享 Limit Manager 覆盖 Connector 全局/单 Tunnel、Work 全状态、PendingOpen 与 Active 的 global/tunnel/service/source IP 预算，所有 Lease 均 exactly-once 归还。AUTH/Control/Work Frame 使用 Schema 上限；Pending TLS/Auth 使用独立非阻塞 Gate。Linux 启动检查 `RLIMIT_NOFILE`，错误逐项列出 Work、Public Active、Pending Open、Control、TLS、Auth、Listener、SQLite、Management、Metrics 与安全余量预算；其他平台明确不执行该 Linux 专属检查。
- E2E 证据：`tests/integration/tcp_echo_test.go` 使用真实 SQLite Tunnel/Token、TLS Gateway、同一 Token 派生的 Agent Control/Work、8 条 WorkConn、静态测试 Service、ephemeral Public Listener 与 Echo Origin，验证二进制载荷、客户端 Half-Close、快速 Drain 退出、goroutine 回基线，并在 Linux 环境可用时检查 `/proc/self/fd` 回基线；没有新增临时生产 Schema 或绕过 M3 的生产配置接口。
- 本地主验证：`go1.27.0`、`GOTOOLCHAIN=local`；核心 13 个包执行 `go test -count=20` 全通过，随后执行 `go test -race -count=2` 全通过；整仓 `go test -count=1 ./...`、`go test -race -count=1 ./...`、`go vet ./...`、`go mod verify`、`git diff --check` 全通过。`internal/server/limits` 与 `internal/server/bootstrap` 使用 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c` 交叉编译通过。
- Review 修复验证：补充并发 Revoke/Success、Health Frame 上限、Heartbeat Timeout、Demand 实际消费后补池、Pending Open FD、Admin 提交后 Gateway 启动失败退出与重启、SQLite 约束失败分支；随后补充 replacement 写失败恢复旧 Current、嵌套回滚跳过失效代、post-flush 不复活旧代、9 条并发 Pending 聚合补池、Timeout/Cancel 配额归还、Drain 重选、Demand 降低立即撤销旧 Lease 等回归。相关 5 包 `go test -count=20`、整仓 Test/Race/Vet 均通过；既有 Linux amd64 交叉 Vet/测试编译与 Proto lint/breaking 证据保持有效，`generate-check` 仍只允许在包含当前生成物的干净 checkout 或 CI 中作为正式证据。
- 二次 Review 修复：Connection Token 先完成 Credential/Secret/ACTIVE 校验再裁决 Tunnel 撤销，Gateway Host 拒绝内部空白与控制字符，Control AUTH 阻塞 IO 响应 Context 取消；Server SIGTERM 分离停止 Accept、排空 ACTIVE、Deadline 强关和 Session 收束，Owner 退出先取消对端 Drain 等待；Agent 普通 Control 断开保留旧代 ACTIVE，WorkPool 在 IDLE 提交点执行 canceled/generation/drain fence；Half-Close 即使 `CloseRead` 失败也继续 `CloseWrite`。新增对应失败分支和并发测试，并修正 Pending Group 测试未在锁内取得稳定快照造成的 Race。相关 9 包 `go test -count=10`、相关 9 包 Race、`internal/tunnel` Race `-count=20`、整仓 Test/Race/Vet、`go mod verify` 与双 Diff Check 均通过；未执行真实 OS SIGTERM 进程 Smoke，未新增或修改 Proto、Schema、依赖和配置。
- 三次 Review 修复：Agent 使用跨 Control generation 的 Binary 级共享 WorkConn Budget，旧代 detached ACTIVE 在 worker 真正退出前持续占用 `max_total=256`，槽位释放后唤醒当前代补池；heartbeat `Enqueue(ErrOwnerClosed)` 按普通 Session 结束处理，retired Pool 由 Runtime 统一登记、取消并等待。Tunnel 在 Idle 选择与提交竞争失败后执行非阻塞重选，不再把总等待时间耗在已空 Pool；RAW Proxy 将 `CloseRead` 清理错误延后到双向复制结束后报告，只有复制或 `CloseWrite` 错误立即中断。新增跨代预算、OwnerClosed 竞争、retired Pool 等待、非阻塞 Idle 提交和真实 TCP 反向流回归测试；Go 1.27.0 / `GOTOOLCHAIN=local` 下定向 5 包 `go test -count=5`、定向 Race `-count=2`、定向 Vet、整仓 Test/Race/Vet 与 `go mod verify` 通过。证据来自脏工作区，未执行 Linux Proto Wrapper、真实 OS SIGTERM 或 CI，不替代 Gate 证据；未新增或修改 Proto、Schema、依赖和配置。
- 四次 Review 修复：Agent retired Pool 不再直接监听已经取消的进程 Context；进程取消时记录单一固定 Deadline，当前 generation 结束两阶段 Drain 后，旧 generation ACTIVE 只等待该 Deadline 的剩余窗口，自然结束则不取消，到期才统一强关并等待 `Pool.Done`。Tunnel 在 Pending Group 选定旧 Session 后遇到 generation replacement 时，先 exactly-once 释放旧 membership，再将陈旧选择转换为可重试结果，在剩余 `work_acquire_timeout` 内回到全 Tunnel 选择。新增 retired Pool 自然结束、Deadline 强关及 `TestAcquireWorkRetriesStalePendingSessionAfterGenerationReplacement` 确定性回归；Go 1.27.0 / `GOTOOLCHAIN=local` 下 Connector `go test -count=20`、Race `-count=5`，Tunnel `go test -count=10`、Race `-count=2`，以及整仓 Test/Race/Vet、`go mod verify` 与双 Diff Check 全部通过。证据来自脏工作区，未执行 Linux Proto Wrapper、真实 OS SIGTERM 或 CI，不替代 Gate 证据；未新增或修改 Proto、Schema、依赖和配置。
- 状态影响：M1-05 至 M1-14 转为 `REVIEW`；M1-01 至 M1-03 继续为 `REVIEW`，M1-04 仅因 Minimal Security Audit Event append-only 写入尚未完成而保持 `IN_PROGRESS`；证书 Metric/告警已经转交 M6，不构成 M1 反向依赖。M05-10 仍缺最新 Tunnel/Connector/Service 契约的 CI 与独立 Review；因此 M1 里程碑保持 `IN_PROGRESS`、`DONE=0`，M1 Gate Checklist 暂不勾选，M2 保持 `NOT_STARTED`。
- Review 边界：本地实现与失败分支已具备 Review 条件，但必须由用户完成本阶段代码/架构 Review；之后若要把任务置为 `DONE`，还需补齐 M1-04、M05-10 与项目规则要求的 CI/独立复审证据。当前 Index 是重构前后的混合旧快照，提交前必须重新暂存完整工作树，禁止直接提交现有暂存区。

## 2026-08-26 · M0-09/M1 Windows CI 回归修复 · REVIEW

- 根因：M1 接入完整 Connection Token 后，Windows Service Smoke 仍使用旧式 `xta_` 加标准 Base64 随机串作为占位 Token。服务安装阶段的轻量校验只确认前缀，因此 SCM 可以接受启动请求；Agent 进入拨号流程后由 Protocol v1 Parser 判定 Token 畸形并永久退出。安装命令随后只能观察到服务未达到 `Running`，最终以 30 秒超时报错，掩盖了真实的进程退出原因。
- 修复：新增共享的 `deploy/smoketoken`，直接复用生产 `connectiontoken.Encode` 生成独立、合法且不会落盘的 Smoke Token，避免在 PowerShell 中复制 Protobuf、HMAC 或 Base64URL Wire 规则。Token 指向 `127.0.0.1:1` 且使用 Public CA 信任模式；该端点按预期连接失败后会进入正常重连路径，使服务保持运行以完成 SCM 生命周期验证。Windows Service 状态等待同时增加提前失败诊断：当目标不是 `Stopped`、服务已经停止且 SCM 提供非零退出码时，立即返回 Win32 Exit Code、Service-specific Exit Code 和 PID，不再把确定性启动失败隐藏为超时。
- 本地验收：Windows `go1.27.0` / `GOTOOLCHAIN=local` 下执行 `go test ./deploy/smoketoken ./internal/agent/service`、`go test ./...`、`go test -race ./...`、`go vet ./...`、`go mod verify`、`git diff --check` 均通过；PowerShell 5.1/7 脚本解析和 ASCII 检查通过；Windows arm64 的 Service 测试二进制与 Smoke Token Helper 交叉编译通过。另以非管理员前台进程验证合法 Smoke Token 在不可达端点下保持重连运行，没有再次永久退出。
- 证据边界：当前宿主不是管理员，未执行真实 SCM 的 install/start/stop/uninstall Smoke；本记录随修复代码一并提交，Commit SHA 以本次 Git 历史为准，但尚无对应 GitHub Actions 重跑结果。本轮不修改公开命令、Protocol、配置、数据库、依赖或部署支持矩阵，因此无需改 README/技术方案；M0-09、M05-10、M1 及全部 Gate 状态保持不变，不标记任何任务 `DONE`，等待用户 Review 和后续 CI 证据。

## 2026-08-26 · M1 Linux Half-Close CI 回归修复 · REVIEW

- Actions 根因：`io.Copy` 已从 TCP 源端读取正常 EOF 后，Linux 的 `TCPConn.CloseRead` 仍可能因该读半边已经完成关闭而返回包装后的 `syscall.ENOTCONN`。代理层此前只把 `net.ErrClosed` 识别为幂等清理终态，因此在业务字节和双向 Half-Close 均成功后仍返回 `close source read half`，并连带导致 `internal/proxy` 与上层 `internal/tunnel` 用例失败。
- 生产修复：`proxyOneWay` 仅在 `io.Copy` 正常 EOF 后，将 `nil`、`net.ErrClosed` 和可由 `errors.Is` 识别的 `syscall.ENOTCONN` 视为已经完成的 `CloseRead`；其他 `CloseRead` 错误仍延迟到反向复制结束后报告，复制错误与 `CloseWrite` 错误仍立即中断双向连接。该实现符合总方案“普通单边 EOF 不是 Fatal Error、反方向继续完成”的既有契约，无需修改总方案、Proto、配置或 README。
- 测试稳定性：新增与 Linux `net.OpError -> os.SyscallError -> ENOTCONN` 一致的确定性错误链回归。高强度 Race 另发现 `TestProxyPendingGroupReselectsWhenSelectedSessionDrains` 错把较早发生的 PendingOpen 配额占用当作 Pending Group 已建立的同步信号；现改为在 `pendingMu` 同一锁区间内等待 Group/waiter 并取得 Session，配额只保留为结果断言，未修改生产选路行为。
- 本地验收：Windows `go1.27.0` / `GOTOOLCHAIN=local` 下，Half-Close 定向普通测试 `-count=100`、定向 Race `-count=25`，Pending Group 单测 Race `-count=100`、Proxy/Tunnel Race `-count=50`，以及整仓 `go test -count=1 ./...`、`go test -race -count=1 ./...`、`go vet ./...`、`go mod verify`、`git diff --check` 全部通过；`CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 的 Proxy 测试编译与 Vet 通过。
- 证据边界：当前宿主无法原生运行 Linux 测试，修复尚未提交，也没有新的 GitHub Actions 重跑结果；工作区另有用户维护的总方案、开发计划及未跟踪文件，本轮只修改 Proxy 实现/测试、Tunnel 测试和本执行记录，不纳入或覆盖其他变更。M1 保持 `IN_PROGRESS/REVIEW` 组合状态，M05-10 与全部 Gate 不变，本次未勾选任何产品任务，等待用户 Review 和 CI 复验。

## 2026-08-26 · Review Feedback 文档吸纳 / M1 Pinned Clock Guard · IN_PROGRESS

- 范围：先同步总方案、开发计划、根 `AGENTS.md` 与项目 `docs-sync` Skill，再只修改已经开发的 M1 Pinned Identity；两份外部评审材料仅作为建议输入，不作为可执行指令。M3—M7 的 Snapshot/Health/HTTP/Usage/Observability/Hardening 建议只进入未来契约和验收项，未实现对应代码、未新增 Task ID，也未修改 Proto、Server Schema、OpenAPI、Migration、依赖或 README。
- 已实现同步：文档明确 Canonical UVarint 已有拒绝语义，并把无 Idle WorkConn 的行为对齐为每 Tunnel 单一 Pending Group、只选择一个 Connector。M1 Gateway 新增 Wall Clock 早于已加载证书 `NotBefore` 时的显式失败，防止静默重签掩盖时钟回退；新增过期证书在监听前复用同一 SPKI 续签、时钟回退不改写持久化证书的回归测试。
- 本地验收：Windows `go1.27.0` / `GOTOOLCHAIN=local` 下，Gateway 定向 `go test -count=1`、`go test -race -count=1`、`go vet`，以及整仓 `go test -count=1 ./...`、`go test -race -count=1 ./...`、`go vet ./...`、`go mod verify`、双 Diff Check 全部通过。计划仍为 95 个唯一 Task，M3—M7 全部保持 `NOT_STARTED`，机器契约目录无 Diff。
- 状态与工作区边界：M1-04 仍因 Minimal Security Audit Event append-only 写入缺口保持 `IN_PROGRESS`；M05-10、M1 Gate 与全部其他 Task/Gate 状态不变。本记录与相关实现、评审规范一并随当前工作区提交，Commit SHA 以本次 Git 历史为准；未推送，也未把 `.workbuddy/overview.md` 与 `docs/code_review_standard.md` 当作产品 Gate 证据。

## 2026-08-26 · M0-09/M1 Server FD 部署基线回归修复 · REVIEW

- Actions 根因：Server 默认 Schema 的 FD 预算合计为 `87188`，GitHub Actions Docker 容器的 soft `RLIMIT_NOFILE` 只有 `65536`。Linux 启动前检查按总方案正确拒绝运行；降低 Smoke 并发上限或修改 Schema 默认值只会掩盖官方部署入口无法承载默认配置的问题，因此未采用。
- 部署修复：用户明确确认后，将官方 Server OCI/Compose/systemd 的 `nofile` soft/hard 基线统一为 `1048576`。OCI Smoke 的正常容器与缺少 Runtime tmpfs 的边界容器均显式传入同一 Ulimit，并通过 Docker Inspect 校验实际 HostConfig；Compose Server 增加同值 `ulimits.nofile`，systemd Server Unit 增加 `LimitNOFILE=1048576`，systemd Smoke 校验运行时属性。应用自身的 FD Budget Manager 和各项配置上限保持不变，提高 OS 上限不会绕过应用容量控制。
- 边界加固：原 Server 边界容器同时缺少 `/run/xtunnel` 和足够 FD，却只断言非零退出，可能因 FD 先失败而把 Runtime Directory 校验误记为通过。修复后边界容器先满足 FD 预算，并要求日志明确包含 `/run/xtunnel` 且不包含 `file descriptor budget`，从而证明测试到达预期失败点。README 同步直接运行 Server OCI 镜像时的 `--ulimit nofile=1048576:1048576` 要求；总方案已经冻结“预算不足必须快速失败”的行为，Server Schema、CI Workflow、Proto、OpenAPI、Migration 和依赖均未修改。
- 本地验收：WSL root Docker `29.1.3` 下，`sh -n`、`dash -n`、`bash --posix -n`、ShellCheck、`docker compose config --quiet` 全部通过；解析 Compose JSON 确认 Server soft/hard 均为 `1048576`，Docker 临时探针容器的 HostConfig 同样为 `1048576:1048576` 且已清理。`systemd-analyze verify` 未报告 Unit 语法或 `LimitNOFILE` 错误，仅报告 WSL 挂载权限、宿主 snapd 新字段和未安装正式 Binary；Windows `go1.27.0` / `GOTOOLCHAIN=local` 下整仓 `go test -count=1 ./...`、`go vet ./...`、`go mod verify`、`git diff --check` 通过。
- 证据边界：本机 Server OCI Smoke 的 BuildKit 冷编译在 360 秒内未完成，按既有阈值中止，未取得真实 Server 容器生命周期、负向 Runtime Directory 或 amd64/arm64 Smoke PASS；中止后确认没有遗留 Smoke Container、Volume 或 Image。真实 systemd Smoke 与新的 GitHub Actions Run 也尚未执行，因此 M0-09、M1-12 和全部 Gate 状态保持不变，本次未勾选任何产品任务。本记录随当前完整工作区提交，Commit SHA 以本次 Git 历史为准；提交不等于 CI/Gate 通过，也不会执行推送。

## 2026-08-26 · M0-09/M1 Agent OCI Token 与存活判定回归修复 · REVIEW

- Actions 根因：Agent OCI Smoke 仍注入旧占位值 `xta_oci_smoke_not_secret`。Agent 先记录 `process_started`，随后由 Protocol v1 Parser 将该值判定为畸形 Token 并永久退出；`wait_for_start` 只查询历史启动日志，没有确认容器当前仍为 `Running`，因此错误进入 `stop_target`，最终由 `docker kill` 报出 `container is not running`。EXIT Trap 的 `docker rm --force ... || true` 仅执行幂等清理，不是本次失败来源。
- 修复：OCI 与 Compose 双栈 Smoke 改为读取由生产编码器生成、受 Golden Test 逐字节锁定的 `tests/golden/protocol-v1/connection-token-v1.txt`，不在 Shell 中复制 Wire 规则，也不为原本只依赖 Docker 的 Smoke 新增宿主 Go 工具链要求。启动等待在发现 `process_started` 后继续确认容器存活并观察一个稳定周期；容器若在 SIGTERM 前退出仍判为失败，`docker kill` 错误会附带容器最终状态与日志，而不是用 `|| true` 掩盖。Windows Smoke Token Helper 同步移动到平台无关的 `deploy/smoketoken`，继续由生产编码器生成独立随机 Token。
- 本地验收：Windows `go1.27.0` / `GOTOOLCHAIN=local` 下 `go mod verify`、`go test -count=1 ./...`、`go vet ./...` 与 Windows arm64 Smoke Token Helper 交叉编译通过；PowerShell 脚本解析通过。WSL root Docker 下，修改后的 Agent OCI Runtime Smoke 使用同一源码交叉编译 Binary 与正式 distroless Runtime Base 连续执行 10 次通过，均完成两轮启动、SIGTERM 与退出码 0；Compose 双栈 `--skip-build` Smoke 通过。`sh -n`、`dash -n`、`bash --posix -n`、ShellCheck 与 `git diff --check` 均通过，验证后已清理临时 Binary、Container 与 Image。
- 证据边界：本机正式多阶段 Dockerfile 的 Agent 冷构建在 360 秒内未完成，按阈值终止；因此临时 Runtime Image 结果不能替代正式 Dockerfile 构建，也没有 Linux arm64 或新的 GitHub Actions Run 证据。M0-09、M0-10、M1 及全部 Gate 状态保持不变，本次未勾选任何产品任务；当前工作区同时存在其他已暂存、未暂存和未跟踪的 M1 改动，本记录不将其纳入本次修复证据，提交前必须重新核对完整 Index。

## 2026-08-26 · M1-04 / M1 阶段收口 · REVIEW

- 授权与范围：用户明确确认 M1-04 所需数据库 Schema 变更，并继续要求“每完成一个大阶段停下 Review”。本轮只收口 M1 与其实际阻塞的 Protocol/部署回归，不进入 M2，不修改 OpenAPI、Server 配置、第三方依赖或生产权限模型。
- 审计契约：新增 `000003_security_audit_events.sql` 与 GORM Repository。事件使用 `evt_<ULID>`/`op_<ULID>`，冻结 M1 的 event/action/actor/resource/result 枚举、UTF-8 字节边界、Nullable 与 32-byte Digest；`operation_id` 唯一，完全相同重放幂等成功，任一 ID 冲突失败。SQLite Trigger 拒绝 UPDATE/DELETE；Application Writer 在固定物理连接上以 `synchronous=FULL` 耐久 COMMIT，恢复普通 `NORMAL` 模式后才派生结构化日志，不提供 lossy fallback。
- 崩溃一致性：离线换钥使用 v2 Rotation Journal 在文件替换前持久化事件/操作 ID、时间、资源与前后 SPKI Digest；部分替换恢复只完成同一组文件，审计耐久落库前不删除 Journal。普通 Server 在 Admin Bootstrap/Gateway Listener 前执行 Reconciliation；数据库写失败会阻止启动或让维护命令以 `AUDIT_WRITE_FAILED_AFTER_COMMIT` 非零退出并保留 Journal，重试只补写旧事件，不再次换钥。Journal 已 unlink 后的目录同步失败使用独立 Warning 并按成功结束，避免误导重试；重启若重新出现 Journal，仍只幂等重放。测试覆盖半替换恢复、写入失败不产成功日志、Journal 保留、启动前补写、FULL/NORMAL 模式切换、cleanup uncertainty、无效元数据不改 SPKI、完全相同并发重放、两个唯一 ID 的独立冲突以及多字节 SQL 边界。
- Protocol 与部署回归：CI Run #10 的 Linux amd64/arm64 Job 已通过最新 Tunnel/Connector/Service 契约的 `lint/breaking/generate-check`；补齐 WorkHello session secret/无 MAC bytes/HMAC input/MAC、Snapshot size/SHA-256、第二枚 Connection Token 与未知 `format_version` 拒绝证据，独立 Protocol Review 通过。Agent OCI/Compose Smoke 改用合法 Golden Token，并在历史 `process_started` 后继续确认容器存活；共享 Smoke Token Helper 移至 `deploy/smoketoken`。CI #10 整体仍在旧 Agent OCI Smoke 失败，当前修复没有新 CI Run，不能宣称 CI 已修复或 Gate PASS。
- 验收结果：Windows `go1.27.0` / `GOTOOLCHAIN=local` 下 `go test -count=1 ./...`、`go test -race -count=1 ./...`、`go vet ./...`、`go mod verify`、双 Diff Check 通过；审计相关包另执行普通测试 `-count=20`、Race `-count=2` 和独立复审。`internal/repository/sqlite` 与 `internal/server/bootstrap` 在 Linux amd64/arm64 交叉测试编译通过，Linux amd64 交叉 Vet 通过。Shell/PowerShell 解析和 ShellCheck 已通过。最终 Linux 原生 Docker 定向复跑因 Docker/WSL 挂载运行失去进展而终止，不记为 PASS；正式 Dockerfile 双架构 Runtime Smoke、真实 systemd/Windows SCM Smoke 与包含当前修复的全绿 CI 尚未取得。
- 状态与 Review 边界：M05-04、M1-04 转为 `REVIEW`，M1-01 至 M1-14 现均处于 `REVIEW`；M05-10、M1 Gate 仍因全绿 CI 和用户阶段 Review 未满足而不标记 `DONE`。M2 保持 `NOT_STARTED`，本轮到此停止。工作树仍混有 staged、unstaged 和 untracked 变更；新增 Golden Fixture 及 Smoke Helper 移动必须在提交前整体重新暂存核对，禁止直接提交当前 Index。本轮未暂存、提交或推送。

## 2026-08-26 · M0.5/M1 Gate 正式收口 / M2 开工 · DONE

- Review：用户明确确认 M1 阶段代码与架构 Review 通过，同意继续下一大阶段。此前独立 Protocol Review 与 M1-04 安全审计复审均已完成，无未处理 P0/P1/P2。
- Commit 与 CI：M1 主体 Commit `87e3027093b0d4801bde1317dbdc8e3be50e6a32` 对应 GitHub Actions CI #11，全绿；当前 HEAD `1dd960e5cf6c0cfd5ed5c54774b4e14e31acbbe9` 对应 CI #12，全绿。CI 链接分别为 `https://github.com/lifei6671/xtunnel/actions/runs/32934944297` 与 `https://github.com/lifei6671/xtunnel/actions/runs/32934990953`。
- Linux amd64/arm64 验收：两个原生 Runner 均确认 `go1.27.0`、`GOTOOLCHAIN=local`、Node `v24.19.0`、npm `11.17.0`；`check-go-version`、Proto `lint/breaking/generate-check`、OpenAPI validate/test、Web ci/check/build、Go Module verify、`go test ./...`、定向 Race、`go vet ./...`、Server/Agent Build、正式 Server/Agent OCI Smoke、生成物与工作树清洁检查全部通过。
- Windows 验收：Windows Runner 确认 `go1.27.0`、`GOTOOLCHAIN=local`；Agent Test/Race/Vet/Build、Windows arm64 Binary 与 Test 交叉编译、真实 Windows Service install/start/stop/uninstall Smoke、工作树清洁检查全部通过。
- 状态：M05-01 至 M05-10、M1-01 至 M1-14 全部由 `REVIEW/IN_PROGRESS` 转为 `DONE`；M0.5 与 M1 退出 Gate 正式通过。M2 转为 `IN_PROGRESS`，先实施 M2-01；M2-03 与 M2-04 因入口依赖满足转为 `READY`。M0-09 和 M0-02 仍保留其独立阶段 Review 边界，不因本次 M1 Review 自动标记 `DONE`。

## 2026-08-26 · M2-01 开工与 M2 契约确认点 · IN_PROGRESS

- M2-01 进展：新增 3/8 Connector 的独立 generation、并发 replacement、并发 Lease/双重 Release、旧 generation 负载保留和最终 Runtime 计数/Session ID 收束矩阵；Control Auth 真实穿过 SQLite、Token Protector、Server/Agent AUTH，证明三枚 ephemeral Connector 逐字节复用同一 ACTIVE Token 且各自 Session/Secret 独立；SQLite Migration 测试明确拒绝出现 Connector/Session/Work 等运行态表。
- M2-02 提前发现的生产缺陷：动态 Eligible 候选集合不含 `lastPicked` 时，旧实现把 RR 游标重置到排序首项；候选交替 `{B,C}`/`{A,C}` 时 C 会永久饥饿。最小修复改为从 `lastPicked` 的有序后继开始并环回，Least Active、锁与 Lease 所有权不变。该修复属于 M2-01 测试发现的直接阻塞，不据此提前把 M2-02 标记为完成。
- 定向验收：Windows `go1.27.0`、`GOTOOLCHAIN=local`；Runtime `go test -count=20`、Race `-count=5`、Vet 通过；Control Auth/SQLite `go test -count=20`、Race `-count=3`、Vet 通过；`git diff --check` 通过。尚未补齐 3+ Connector Session Pool/Proxy churn、整仓 Test/Race/Vet 或 CI，M2-01 保持 `IN_PROGRESS`。
- Ask First 阻塞一：M2-04 的 Rotate 必须写 Security Audit Event，但 `000003_security_audit_events.sql` 与 Repository 仅允许 `GATEWAY_KEY_ROTATE/GATEWAY_IDENTITY`。需要用户确认新增 forward-only Migration，扩展 Token Reveal/Rotate/Revoke 与 Tunnel Revoke 的 action/resource 枚举，并增加精确 Repository CAS/状态迁移接口；未确认前禁止省略审计或绕过 CHECK。
- Ask First 阻塞二：M2-03 需要在 `controlauth`、`runtime`、`sessionruntime` 间增加 Connector Metadata/Lifecycle Snapshot/Event 的内部跨包契约，并接入稳定结构化日志事件与技术方案已冻结的 online/active/idle Metric；这会改变跨模块接口与日志契约，需要明确确认。未确认前 M2-03/M2-04 标记 `BLOCKED`，未修改 Migration、日志/Metric、Proto、OpenAPI、配置、依赖或生产权限。
- 工作区边界：本轮未执行 `git add`、commit 或 push；共享 Index 已存在 `internal/server/runtime/multi_connector_test.go` 的早期暂存版本，而 Worktree 含完整测试，提交前必须由用户重新暂存并核对，不得直接提交当前 Index。

## 2026-08-26 · M2 契约变更授权 · IN_PROGRESS

- 用户确认实施 M2 所需的前向 Migration，扩展 append-only Security Audit Event 的 Token Reveal/Rotate/Revoke 与 Tunnel Revoke 枚举，并增加精确 Repository CAS/状态迁移接口。
- 用户确认增加 `controlauth`、`runtime`、`sessionruntime` 之间的内部 Connector Lifecycle/Revoke 契约，以及稳定且不含 Secret/高基数标签的生命周期日志与 Metric Source。
- 授权边界不包含 Proto、OpenAPI、Server 配置、第三方依赖、锁文件或生产权限模型变更；M2-03/M2-04 由 `BLOCKED` 转为 `IN_PROGRESS`。

## 2026-08-26 · M2-01 至 M2-08 · REVIEW

- Multi-Connector/Selection：同一 ACTIVE Token 的三 Connector 认证与独立 Session/Secret、3/8 Connector generation/replacement、并发 Lease exactly-once、旧 generation 负载保留、动态 Eligible 集合 RR 公平、Session Pool/Proxy churn 与最终计数归零均有自动化证据；SQLite 明确没有 Connector/Session/Work 持久化表。
- Lifecycle/Observability：Runtime 提供 generation-fenced Connected/Heartbeat/Draining/Disconnected、Current/Tombstone 确定性快照和五项无 Label Metric Source；Bootstrap 将项目统一 JSON Logger 注入 Session Manager。`liveSessions + cleanupOnce/done` 覆盖 current/retiring 全 generation，Revoke/Shutdown/replacement 等待 Control Owner、Authenticator、Pool 与 Registry 全部锁外收敛后才返回；旧 cleanup 不会删除新代。
- Credential/Tunnel Revoke：前向 Migration `000004_credential_lifecycle_audit.sql` 在单事务内保留 v3 证据并扩展四类 ADMIN 操作，恢复索引与 append-only Trigger。Reveal/Rotate/Token Revoke 使用 durable transaction；Rotate 复用 Endpoint/TLS Trust，旧 ACTIVE 转 `REVOKED_FOR_NEW_SESSION`，新 Token/Tunnel Version/审计原子提交；Tunnel Revoke 在持久提交后对同一 Manager 建立永久 Fence 并关闭全代 Runtime。post-COMMIT cleanup 返回已提交结果与可识别错误；Runtime 收敛失败追加独立 FAILED 审计事实。
- Failover：`AcceptRaw` 前 Transport 失败同池第二条只做非阻塞获取，两次失败或 `OPEN_DRAINING` 后最多跨 Connector OPEN 一次；备用候选被抢、replacement 或 drain 时继续下一个当前候选，不创建第二 Pending Group。所有尝试复用同一 `connection_id`；Context Cancel、Protocol/普通 OPEN Error、RAW 已提交或已转发业务字节均不重放。测试覆盖单 Work、三 Connector 竞争、Control Crash、取消、RAW 字节不重放与随机 ID 失败零资源泄漏。
- 独立 Review：三轮交叉复审先后发现并关闭动态 RR 饥饿、同池阻塞、备用候选竞争、取消窗口、post-COMMIT 误判、Runtime 审计缺口、retiring Session 遗失、Revoke cleanup 提前返回、重复/缺失生命周期事件、Control Owner 未等待及随机 ID 失败 Work 泄漏。本次阶段 Review 又发现并修复 Security Audit post-COMMIT cleanup 漏记 committed 日志、Tunnel Revoke 后 Reveal/Rotate 错误分类、Owner 启动到 Session 登记之间的 Revoke/Shutdown 提前返回窗口、收敛原因竞态，以及 README M2 状态表述不一致；新增确定性回归测试后，最终工作树整仓 Test/Race/Vet 复验通过，无剩余 P0/P1/P2。此前不可提交的早期 Index 已按最终工作树重新整体暂存，并从 staged snapshot 复验通过。M5 REST Handler 与 M6 `/metrics` 导出仍按后续依赖实施，不在 M2 提前制造入口。
- 本地验收：Windows `go1.27.0`、`GOTOOLCHAIN=local`；相关九包 `go test -count=10` 通过，各域高重复测试最高 `-count=30`、Race 最高 `-count=10` 通过；`go test -count=1 ./...`、`go test -race -count=1 ./...`、`go vet ./...`、`go mod verify`、GoFmt 与 `git diff --check` 通过。Linux amd64 Bootstrap Test 交叉编译通过。Proto、OpenAPI、Server Schema、依赖与锁文件无变更。
- 证据边界：本次提交前已重新整体暂存最终工作树，并从 staged snapshot 通过本地测试与 Diff Check；仍没有覆盖本次 M2 变更的 GitHub Actions CI Run，故 M2-01 至 M2-08 全部保持 `REVIEW`，M2 `DONE` 计数仍为 `0/8`、全局仍为 `33/95`。本次未推送，等待用户阶段 Review 与提交后的 CI 证据。

## 2026-08-26 · M2 阶段 Review 通过 / M3-01 开工确认点 · BLOCKED

- 用户已通过 M2 阶段 Review；M2 本地提交为 `4447602984b3d58d0e35a8ba3a5c07d2226bdb62`，工作树干净且本地 `master` 领先 `origin/master` 一个提交。因尚未推送、没有覆盖该提交的 CI Run，M2 继续保持 `REVIEW`，不得伪标 `DONE`。
- M3-01 的 M0-05/M1-14 依赖已满足。只读核对确认 Protocol v1 已冻结 `TunnelSnapshot`、`ServiceConfig`、`ConfigAck` 与 `ServiceHealthBatch`，Server Schema 也已包含 Service Count 和 Health Target Budget 上限；本任务不需要修改 Proto、OpenAPI、Server Schema、第三方依赖或锁文件。
- M3-01 的首个真实产物必须包含 v5 `services` 前向 Migration、Service Domain/GORM Repository、`RepositoryView/TxStore` 的 Service 访问入口，以及独立于 Tunnel ETag 的 Desired Revision CAS。数据库词汇固定为 `health_type = TCP/HTTP`，Disabled 时其余 Health 列全部为 `NULL`；Service 直接外键归属 Tunnel，删除采用 `ON DELETE RESTRICT`，不创建 `tunnel_bindings`。
- 阻塞原因：新增数据库 Schema 和内部跨包 Repository/事务契约属于 Ask First。解除条件是用户明确确认上述最小授权范围；确认后 M3-01 转为 `IN_PROGRESS`，先完成 Migration/Domain/Repository 与定向 Test/Race/Vet，不提前接入 M3-02/M3-03 生产写入口。
- CodeGraph MCP 本轮连续返回 `Transport closed`；结构核对降级为冻结文档、Proto 与定点源码只读审查。三个独立审查分别覆盖存储事务、Snapshot/Apply 和 Health/Durable Operations，未修改代码、机器契约、暂存区或提交历史。

## 2026-08-26 · M3-01 数据库/内部契约授权 · IN_PROGRESS

- 用户明确确认新增 v5 `services` 前向 Migration，以及 Service GORM Repository、`RepositoryView/TxStore` Service 入口和 Tunnel Desired Revision CAS/事务契约。
- 授权边界不包含 Proto、OpenAPI、Server Schema、第三方依赖、锁文件、生产权限模型或 M3-12 Durable Operations；M3-01 由 `BLOCKED` 转为 `IN_PROGRESS`。

## 2026-08-26 · M3-01 · REVIEW

- 产物：新增 v5 `services` 前向 Migration、`svc_` CSPRNG/ULID 身份、Service/Origin/Health 领域模型、GORM Service Repository、`RepositoryView/TxStore.Services()` 与 Tunnel Desired Revision CAS；Service 直接使用 `ON DELETE RESTRICT` 归属 Tunnel，不存在 `tunnel_bindings`。
- 关键断言：覆盖 v4→v5 数据保留与失败回滚、NULL/非法 ID、Origin/Health/nullable 组合、跨 Tunnel 访问、稳定排序、Create/Update/Delete、Service Version CAS、Tunnel Version/Desired Revision 双 CAS、revoked fence、并发 exactly-once 和最终状态回读。独立复审发现并关闭 `TEXT PRIMARY KEY` 可接受 NULL、ASCII 控制空白绕过 CHECK、revoked/并发 CAS 证据不足；最终无剩余 P0/P1/P2。
- 验收环境：Windows `go1.27.0`，`GOTOOLCHAIN=local`。
- 验收命令：`go test -count=20 ./internal/identity ./internal/repository ./internal/repository/sqlite`；同范围 `go test -race -count=5`；`go vet ./internal/identity ./internal/repository/...`；`go test -count=1 ./...`；`go vet ./...`；`go mod verify`；`git diff --check`。
- 验收结果：全部通过。Proto、OpenAPI、Server Schema、第三方依赖、锁文件、README 和用户可见启动方式均未改变，因此无需同步这些权威源或用户文档。
- 证据边界：本次修改尚未提交、未推送、没有对应 CI Run，M3-01 只能进入 `REVIEW`，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`。当前 Index 仅暂存 5 个新增文件，而必要集成与修复仍在 unstaged 工作树；禁止直接提交当前 Index，提交前必须重新整体暂存并复验 staged snapshot。
- 后续阻塞：M3-02/M3-03 需要新增内部 Application Service 与 Snapshot Builder/Size Gate 跨包契约，等待用户 Ask First 授权；未提前增加 M5 REST Handler、Proto 字段、Server 配置或依赖。

## 2026-08-26 · M3-02/M3-03 内部契约授权 · IN_PROGRESS

- 用户明确确认新增 Service Application Service、单个 `Store.WithTx` 内的 Service Version/Tunnel Desired Revision 编排，以及事务提交前的完整 TunnelSnapshot Service Count、Deterministic Bytes 和最终 ControlEnvelope Size Gate。
- 授权边界不包含 REST Handler、Proto、OpenAPI、Server Schema、第三方依赖、锁文件、生产配置或权限模型；M3-02/M3-03 转为 `IN_PROGRESS`。
- CodeGraph MCP 仍返回 `Transport closed`；结构核对继续降级为冻结技术方案、Protocol v1 与定点源码审查，不据此放宽测试或证据要求。

## 2026-08-26 · M3-02 / M3-03 事务内子产物 · BLOCKED

- M3-02 产物：新增 Service Application Service 与最小 `TunnelSnapshotGate` 使用方接口。Create/Update/Delete 在单个 `Store.WithTx` 中校验所属 Tunnel 当前 Version 与 Service Version；Create/Delete、Origin/Health/Enabled 变化构建完整 Candidate、执行 Gate 并让 Tunnel Desired Revision 精确递增一次；Name-only 只递增 Service Version，完整 no-op 不写入、不调用 Gate。Gate/CAS/Revision 错误保持错误链并整体回滚。
- 默认值：`connect_timeout_ms=5000`、`tls_verify=true`、`enabled=true`；部分 Health 固定补齐 `interval_ms=10000`、`timeout_ms=2000`、HTTP `path=/health`、`expected_status=200..399`、`failure_threshold=3`、`success_threshold=2`。TCP 携带 HTTP 专属字段会在边界失败，显式 `false` 不被默认值覆盖。
- M3-03 已验证子产物：新增稳定 TunnelSnapshot Builder；Service 按 `service_id` 排序且不改写输入，Disabled 显式编码为 `HEALTH_TYPE_DISABLED`；拒绝跨 Tunnel、重复 Service、负 Revision 与未来 Required Revision；Service Count、768 KiB Deterministic Snapshot 绝对上限和最终 ControlEnvelope payload 上限均在事务 COMMIT 前执行。
- 真实集成：SQLite + 真实 Builder 在第二个 Service 超过 Count 上限时返回可识别错误，第二个 Service 行与 Tunnel Revision 同时回滚。Create/Update/Delete Gate 失败、Revision exhaustion、双 Version Fence、Revoked Fence、同 Service CAS 一胜一冲突、不同 Service 并发 Revision 连续均有最终数据库状态断言。
- 独立复审：两轮只读审计先后发现并关闭 Builder 可被错误配置放宽 768 KiB 绝对上限、Update Gate 回滚证据、Revision exhaustion、TCP/HTTP partial defaults/mixing 与真实 Builder 回滚链缺口；最终当前授权范围无剩余 P0/P1/P2。
- 验收环境：Windows `go1.27.0`，`GOTOOLCHAIN=local`。定向 `go test -count=20` 通过；四包 `go test -race -count=5` 与 `go vet` 通过；`go test -count=1 ./...`、`go vet ./...`、`go mod verify`、GoFmt、`git diff --check` 全部通过。
- 证据边界：本次仍是未提交工作树，没有 Commit SHA、push 或对应 CI Run；M3-02 只能进入 `REVIEW`，M3-03 不得把事务内子产物冒充整项完成，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`。Proto、OpenAPI、Server Schema、第三方依赖、锁文件、README 和用户可见启动方式未改变，因此无需同步这些权威源或用户文档。
- 剩余阻塞：冻结技术方案要求 Migration 后、Public Listener 启动前对所有存量 Tunnel 复用同一 Builder；当前 Tunnel Repository 尚无列举入口，Bootstrap 尚无 Startup Validator。该内部跨包接口与生产启动失败路径超出本次明确授权，等待用户确认后继续，M3-03 标记 `BLOCKED`。
- 工作区边界：本轮未执行 `git add`、commit 或 push；Index 仍只含部分 M3-01 早期内容，M3-02/M3-03 文件未跟踪，禁止直接提交当前 Index。未来用户要求提交时必须重新整体暂存最终工作树并从 staged snapshot 复验。

## 2026-08-26 · M3-03 Startup Gate 授权 · IN_PROGRESS

- 用户明确确认新增 Tunnel Repository 稳定只读列举入口与 Bootstrap Startup Validator；校验点固定在 SQLite Migration 完成后、任何 Public Listener 启动前，并复用 M3-03 已验证的 Service Count、Deterministic Snapshot Bytes 与最终 ControlEnvelope Size Gate。
- 非法存量 Tunnel 必须携带可定位但不含 Secret 的 Tunnel/Count/Bytes 错误上下文并阻止 Server 启动；不得自动删除、裁剪、分片、压缩或猜测修复存量配置。
- 授权边界不包含 M3-04 Agent Atomic Apply、REST、Proto、OpenAPI、Server Schema、第三方依赖、锁文件、生产配置、权限模型或日志契约变更；M3-03 由 `BLOCKED` 转为 `IN_PROGRESS`。
- CodeGraph MCP 继续返回 `Transport closed`；结构核对降级为冻结方案、Repository、Bootstrap 生命周期与定点源码审查。

## 2026-08-26 · M3-03 Snapshot Builder/Size Gate · REVIEW

- 产物：`TunnelRepository.List` 通过 GORM 按 Tunnel ID 升序返回完整持久化 Tunnel，并拒绝损坏记录；Startup Validator 在 Stable Data Target External Lock 持有、Migration 与同步恢复完成且进程内写入口尚未开放的静止数据集上列举全部 Tunnel/Service，复用同一 Builder 校验 Revision、Service Count、Deterministic Snapshot Bytes 与最终 ControlEnvelope Size。
- 启动顺序：生产装配在 `openGatewayAndBootstrapWith` 中先执行 Startup Gate，再创建 Gateway 生命周期、检查首个 Admin 或打开本机 Bootstrap Socket；非法存量返回可识别 Gate 错误与 `nil` Closer。外层启动失败路径会关闭 SQLite 并释放 External Lock。
- 安全与回归：错误仅含 Tunnel ID、数量、字节数和上限，不包含 Origin、TLS Server Name 或其他 Service 配置。真实 SQLite 集成同时覆盖已有 Admin + Revoked Tunnel 的 Gateway Listener 分支，以及无 Admin/`SETUP_REQUIRED` 的首次 Bootstrap Socket 分支；两者均证明 Listener/Socket 回调未越过 Gate。
- 独立复审：审查先发现无 Admin 分支断言为空的 P1；补充真实 no-admin 集成用例后复核为无 P0/P1/P2，M3-03 可进入 `REVIEW`。未来 M4 新增的 Management/HTTP/TCP Listener 必须保持在本 Gate 下游。
- 验收环境：Windows `go1.27.0`，`GOTOOLCHAIN=local`。Startup 集成用例 `go test -count=20` 通过；新增 no-admin 用例 `go test -race -count=5`、existing-admin 用例 `go test -race -count=1` 通过；`go test -count=1 ./...`、`go vet ./...`、`go mod verify`、GoFmt、`git diff --check` 与 `git diff --cached --check` 全部通过。
- 证据边界：本次仍是未提交工作树，没有 Commit SHA、push、对应 CI Run 或 staged snapshot 验证，故 M3-03 仅标记 `REVIEW`；M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`。本轮未执行 `git add`、commit 或 push，且当前 Index 只包含部分 M3 变更，禁止直接提交当前 Index。
- 文档同步：Proto、OpenAPI、Server Schema、第三方依赖、锁文件、README、用户可见命令与部署方式均未改变，因此无需同步其他权威源或用户文档。M3-04 涉及 Agent Config Runtime/ConfigAck 内部跨包接口与并发资源生命周期，状态设为 `BLOCKED`，等待用户明确授权。

## 2026-08-26 · M3-04 Agent Atomic Apply 授权 · IN_PROGRESS

- 用户明确确认新增 Agent Config Runtime、ConfigAck 使用方接口以及 Candidate/Current Runtime 并发资源生命周期；实现必须遵循 `Validate → Prepare/Build Candidate → Start gated Candidate Resources → Atomic Publish Runtime + Revision + Digest → ConfigAck → bounded Retire`。
- Candidate 发布前不得上报 Health 或参与选择；任何校验、构建或启动失败都必须释放 Candidate、保持旧 Runtime 与旧 Revision/Digest 不变。Retire 必须在 Ack 成功入队后触发，且不得关闭旧 Revision 已进入 ACTIVE 的 WorkConn。
- M3-04 只建立原子 Runtime/资源生命周期与 Ack 契约；Token-only 重连集成、Snapshot debounce/coalesce、Origin Resolver、中心 Health Scheduler、Health Batch/Eligible Selection 分别留给 M3-05 至 M3-09，不提前实现。
- 授权边界不包含 Proto、OpenAPI、Server Schema、第三方依赖、锁文件、生产配置、权限模型、日志契约或数据库变更；CodeGraph MCP 返回 `Transport closed`，结构核对降级为冻结方案、Proto 权威和定点源码审查。

## 2026-08-26 · M3-04 Agent In-Memory Atomic Apply · REVIEW

- 产物：新增独立 `internal/agent/configruntime`。`Manager` 用单次 atomic Load/Swap 管理 Snapshot、Revision、Deterministic SHA-256 Digest、私有 Runtime Resources 与 Acked Gate；`Session` 单独保存当前 Control Session 的 observed revision/digest 基线。Builder 只收到独立 Clone，输入先递归拒绝 Unknown Field、校验 Tunnel/Revision/Service 数量并按 Service ID 稳定排序，不允许 Builder 保留或修改 Manager 拥有的 Snapshot。
- 发布与失败语义：严格执行 `Validate → Build → Start gated Candidate → Atomic Publish → APPLIED Ack 入队 → Gate Active → bounded Retire`。Build/Start/Apply Context 取消会 Abort 未发布 Candidate 并保持旧元组；Ack 入队失败时新元组保持不可选择、旧资源进入 pending，后续成功 Apply 或 Close 才回收。Candidate 使用 Manager-owned Lifetime Context，Publish 与 Close 通过生命周期提交栅栏互斥，Retire 使用独立 Deadline，Manager Close 等待全部所属 goroutine，并以测试证明关键路径 exactly-once。
- 所有权边界：当前只读 `View` 仅暴露 Snapshot 深拷贝、Revision、Digest 和 Acked，不暴露可执行 `Retire` 的 owning Resources。M3-04 不创建或关闭 WorkPool、Origin Socket、已进入 ACTIVE 的 WorkConn，也不接入生产 Control Session、Bootstrap/Reconnect、WorkDemand、Origin Resolver 或 Health Scheduler；这些职责继续由 M3-05 至 M3-09 所有。
- 独立复审：首轮发现 Builder 输入别名、Apply Context 错误拥有已发布 Candidate、Close/Publish 缺少提交栅栏、Retire exactly-once 证据不足，以及 View 暴露 owning Resources；逐项修复并复核后无剩余 P0/P1/P2，M3-04 内核可进入 `REVIEW`。
- 验收环境：Windows `go1.27.0`，`GOTOOLCHAIN=local`。`go test -count=30 ./internal/agent/configruntime`、`go test -race -count=10 ./internal/agent/configruntime`、`go vet ./internal/agent/configruntime`、`go test -count=1 ./...`、`go vet ./...` 与 `go mod verify` 全部通过；完成 GoFmt。清理同一 M3 工作区已有的 6 处空白行尾后，`git diff --check` 与 `git diff --cached --check` 均通过。
- 跨任务阻塞一：总技术方案将 `max_services_per_tunnel = 1000` 写为 V0.1 固定上限，但 Server Schema 当前只把 `1000` 作为默认值、允许配置至 `2147483647`，Server Builder 使用该配置；Agent 没有本地 Server 配置来源。M3-04 内核只接受装配方注入的内部上限，M3-05 生产接线前必须裁定是否把 Schema maximum 收紧为 1000。
- 跨任务阻塞二：冻结语义规定 `observed_revision` 只代表当前 Session 已成功应用的内存 Revision，因此 Reject 新 Snapshot 时仍是旧 Revision；当前 Protocol State 会把相同 Revision 的 `APPLIED → REJECTED` 判为冲突。M3-05/M3-06 需要让 Server 以该 Session 唯一 outstanding Snapshot 关联 Reject，不能把失败的目标 Revision伪装成已观测 Revision。
- 跨任务阻塞三：总技术方案固定 High Priority 顺序为 `Error → Drain → ConfigAck → 最新 Heartbeat`，当前 Outbox 使用普通 FIFO high slice，Heartbeat 原位覆盖可能让携带新 observed revision 的 Heartbeat 先于 ConfigAck 出队。M3-05/M3-06 接线前必须落实固定优先级或等价地阻止该 Heartbeat 越过 Ack。
- 证据边界：任务表重新统计仍为 M3 `0/13`、全局 `33/95`。本次仍是未提交工作树，没有 Commit SHA、push、对应 CI Run 或 staged snapshot 验证，故只标记 `REVIEW`；未执行 `git add`、commit 或 push，当前 Index 仍不是可直接提交的完整 M3 快照。
- 文档同步：本次只同步开发计划状态、实现边界与可复现证据。Proto、OpenAPI、Server Schema、第三方依赖、锁文件、README、用户命令与部署方式均未改变；M3-05 因跨模块接口、Schema/Protocol Runtime/Outbox 行为边界标记 `BLOCKED`，等待用户明确授权与裁定。

## 2026-08-26 · M3-05 生产 Remote Config 接线授权 · IN_PROGRESS

- 用户明确确认 M3-05 的 Agent/Server 生产接线与三项契约裁定：`max_services_per_tunnel` 的 V0.1 绝对上限固定为 1000；REJECTED Ack 保留旧 `observed_revision`，由当前 Session 唯一 outstanding Snapshot 关联失败目标；High Priority Outbox 固定为 `Error → Drain → ConfigAck → 最新 Heartbeat`。
- 授权范围包含 Server Schema、Protocol State、Control Outbox、Agent/Server Remote Config 生产装配和对应测试；不修改 Proto、OpenAPI、第三方依赖、锁文件、生产权限模型或日志字段契约。
- 每代 Control Session 必须先取得完整 Snapshot 并成功发送 APPLIED Ack，才能开放 Connector 级 config-ready 门、ONLINE、Work Auth、Pool 与 WorkDemand；Server 不可达、Snapshot 被拒绝或 Ack 未完成时禁止本地配置回退。

## 2026-08-26 · M3-05 Token-only Startup/Reconnect + Remote Config · REVIEW

- 上限与协议：Server Schema、Snapshot Builder 和 Agent Config Runtime 同时把单 Tunnel Service 绝对上限收紧为 1000。Protocol State 以每 Session 唯一 outstanding Snapshot 关联 Ack；APPLIED 才推进 observed revision，REJECTED 保留旧 observed，非法、错配或无 outstanding Ack 快速失败。Outbox 按固定高优先级排序，Snapshot outstanding 时只允许 Error、Drain 和 ConfigAck 越过，Heartbeat 与普通消息等待 Ack。
- Server：新增 SQLite 普通只读事务 `ReadConsistent` 和生产 Snapshot Source，在同一 WAL 快照读取完整 Services 与 Tunnel Revision，真实并发测试证明不会拼接新 Revision 与旧 Services。Gateway Bootstrap 注入该 Source；Session install 后即进入 `liveSessions`，首条业务消息为完整 Snapshot，Ack 前 `Resolve/GrantLease/RegisterIdle/Pool/SetPendingOpens/reconcileDemand` 全部关闭。APPLIED 后按 `ObserveConnected → config-ready → 幂等 Demand Reconcile` 发布；REJECTED 保持 syncing 且可被 Revoke/Shutdown 收敛；replacement 新代 Ack 前没有可服务回退代。
- Agent：`Runtime.Run` 创建跨重连复用、由进程显式关闭的 Config Manager；每代认证 Tunnel 都新建 Config Session，observed revision/digest 基线归零。Snapshot Apply 和 ConfigAck 只经同一 Control Owner/Outbox；Ack 前收到 WorkDemand 会 fail-closed；普通与 Drain Heartbeat 都读取本代 observed revision。Config Manager 延续到 Control/WorkPool 排空之后再有界关闭；Agent 仍只有 Connection Token 输入，不创建本地 Snapshot、Revision、数据库或业务配置文件。
- 集成与边界：真实 TLS Gateway、Control/Work AUTH、Agent Runtime、Server Session Manager、SQLite Snapshot Source、WorkPool、Tunnel Proxy 和 TCP Echo 链路通过。该用例注入静态 OriginDialer，只证明 M3-05 Snapshot/Ack 门禁没有破坏既有数据面；生产 Agent 在 M3-07 前仍使用 `UnobservedOriginDialer` 安全拒绝 Origin。持续 Snapshot Reconcile 属于 M3-06，真实 Origin Resolver 属于 M3-07，Health 与最终服务级 Eligible 属于 M3-08/M3-09。
- 独立复审：最终只读审查核对服务上限、Ack 关联、Outbox 屏障、SQLite 一致性、Server 门禁、Agent 重连/Drain 生命周期及敏感信息边界，代码无剩余 P0/P1/P2；发现的文档状态漂移已在本节和任务表修复。CodeGraph MCP 仍返回 `Transport closed`，结构审查使用冻结技术方案、Proto、Schema 与定点源码完成。
- 验收环境：Windows `go1.27.0`，`GOTOOLCHAIN=local`。核心七包 `go test -count=20` 通过；11 个相关包与真实集成 `go test -race -count=3` 通过；`go test -count=1 ./...`、`go test -race -count=1 ./...`、`go vet ./...`、`go mod verify`、GoFmt、`git diff --check` 与 `git diff --cached --check` 全部通过。
- 证据边界：本次仍是未提交的混合 staged/unstaged 工作树，没有 Commit SHA、push、对应 CI Run 或 staged snapshot 复验，故 M3-05 只能标记 `REVIEW`；M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`。未执行 `git add`、commit 或 push，当前 Index 不是可直接提交的完整 M3 快照。
- 文档同步：Server Schema 机器权威已同步 1000 绝对上限，README 同步当前 Remote Config 生产门禁和后续阶段边界；Proto、OpenAPI、第三方依赖、锁文件、用户命令和部署方式未改变。M3-06 保持 `NOT_STARTED`，等待用户完成本阶段 Review 后再继续。

## 2026-08-26 · M3-05 Review 修正：Goroutine Panic 收敛 · REVIEW

- Review 修正：`sessionruntime.Manager.Serve` 从单个超长方法拆为 Session 构造、Owner 启动与安装、首份 Snapshot、入站消费和统一清理阶段；主方法只保留生命周期编排。拆分保持 startup fence、generation replacement、ConfigAck 门禁、Drain、Heartbeat Timeout 和 cleanup 顺序不变，现有顺序回归测试继续通过。
- 并发边界：新增内部 `safego` 启动器，生产 Go 代码的 21 个 goroutine 启动点全部通过该入口启动。Panic 错误使用固定文本且不携带 recovered value；Control Owner、Proxy、Agent Config/Session/WorkPool、Windows Service、Gateway 和 Linux Admin Bootstrap 分别把 Panic 接入既有 fatal、result、cancel、listener stop 与 Wait 收敛路径，不采用仅 recover 后继续运行的静默降级。
- 关键断言：覆盖 Control read/write/owner、双向 Proxy、Server Session 入站、Agent Retire/Close/Session Completion/WorkConn Worker/WorkPool Lifetime、Windows Service Callback、Gateway Handler/Renewal 和 Linux Admin Request 的 Panic；断言错误可用 `errors.Is(..., safego.ErrPanic)` 识别，Secret Panic 原值不进入错误文本，连接、预算、Registry、Pool、WaitGroup 和 Done Channel 均按 owner 规则收敛。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`；定向 Package Test/Race/Vet、`go test -race -timeout 120s ./...`、`go vet ./...` 与 `go mod verify` 全部通过。Linux Admin Bootstrap 完成 Linux amd64 Test Binary 交叉编译与 Linux Vet，但当前宿主未原生运行 Linux-only 测试，不把交叉编译冒充 Runtime Smoke。
- 证据边界：本次仍是未提交的混合 staged/unstaged 工作树，没有 Commit SHA、push、对应 CI Run 或 staged snapshot 复验；M3-05 保持 `REVIEW`，M3 与全局 `DONE` 计数不变。本次未勾选任何产品任务，M3-06 继续保持 `NOT_STARTED`，等待用户 Review。Proto、OpenAPI、Server Schema、数据库、第三方依赖、锁文件、README、用户命令和部署方式均未改变。

## 2026-08-26 · M3-06 Snapshot Reconcile/Observed Revision · REVIEW

- 授权与边界：用户通过 M3-05 阶段 Review 后明确要求继续 M3-06。本轮只实现 Snapshot Reconcile、Observed Revision/Digest、ConfigAck 串行和 Service 提交后 dirty 通知；未提前实现 M3-07 Origin Resolver、M3-08 Health Scheduler 或 M3-09 服务级 Eligible Selection。
- Server Reconciler：`sessionruntime.Manager` 唯一拥有一个 `safego` 包装的 Reconcile goroutine。容量 1 的 wake 只表达“存在 dirty”，per-Tunnel map 保留最新 generation；构建期间同 Tunnel generation 前进时旧 Candidate 不进入 Session，立即重建最新状态。首份 Snapshot 也经同一 Build owner；生产启动保持冻结的 `Gateway → Reconciler` 顺序，关闭则先停止并等待 Reconciler，再排空 Session。
- Session 收敛：每代 Control Session 只允许一个 outstanding Snapshot，期间只保留最高 pending Revision。APPLIED 才推进 observed revision/digest，首份 Ack 后才开放 config-ready；REJECTED 保留旧 observed/config-ready，不重发同或更低的已拒绝 Revision，更高 pending 在 Ack 后串行下发。重放 ConfigAck 不会误消费已建立的新 outstanding。锁序固定为 `Manager.mu → snapshotMu → managed.configMu`，SQLite Build、Outbox Enqueue、Cancel/Close/Wait 和错误上报均在锁外。
- Agent 语义：在递归 Unknown Field 拒绝和 Deterministic Digest 计算后、Candidate Build 前应用 Revision 规则。同 Session 低 Revision 或同 Revision/不同 Digest 返回 `ErrProtocolViolation` 并不 Build/ConfigAck；同 Revision/同 Digest 幂等补 APPLIED Ack。新 Control Session 的 observed 基线仍为空，允许 Restore 后更低 Revision；若与进程 current Revision+Digest 完全相同，可复用 Runtime 并在 Ack 成功后激活 Gate。
- Config Write 接线：`ServiceManagementService` 在 Create/Delete 以及真正改变 Snapshot 的 Update 事务 COMMIT 成功后调用最小 `SnapshotReconcileNotifier.MarkDirty`；Name-only、no-op、Gate/事务失败不通知。通知失败返回真实已提交 Result 并包装 `ErrServiceRuntimeConvergence`，不伪装成回滚。Reconcile Source 失败保留旧 Runtime，记录可查询 `SnapshotError`，在 5 秒周期/新 dirty 后重试并于成功通过 generation fence 后清除。
- 复审：三个无写冲突子任务分别完成 Agent、Application 和 Server 实现；主代理复审额外修正 duplicate Ack 与新 outstanding 错误关联。独立只读复审提出 Source 失败不可查询和 Bootstrap 启动顺序两个 P2；补齐失败状态/恢复测试并修正启动顺序后复核均为 Closed，当前无剩余 P0/P1/P2。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`；`go test -race -count=1 -timeout 180s ./...`、`go vet ./...`、`go mod verify`、GoFmt、`git diff --check` 与 `git diff --cached --check` 全部通过。Source 失败状态与冻结启动顺序修正后，相关四包 `go test -race -count=1 -timeout 120s ./internal/server/sessionruntime ./internal/server/bootstrap ./internal/tunnel ./tests/integration` 再次通过。
- 证据边界：本轮未提交、未推送，没有 Commit SHA 或对应 CI Run；因此 M3-06 只进入 `REVIEW`，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`，本次未勾选任何产品任务或 Gate。当前 `reconcile.go` 与 `reconcile_test.go` 同时存在 staged/unstaged 差异，提交前必须重新暂存最终版本并从 staged snapshot 复验。
- 文档同步：行为符合已冻结的 Revision、ConfigAck、Reconcile 和 Server Startup 契约，本次只同步开发计划任务表、当前队列和执行证据。Proto、OpenAPI、Server Schema、数据库 Migration、第三方依赖、锁文件、README、用户命令和部署方式均无需更新。M3-07 保持 `NOT_STARTED`，本轮到此停止并等待用户阶段 Review。

## 2026-08-26 · M3-07 Origin Resolver · REVIEW

- 授权与边界：用户通过 M3-06 阶段 Review 后明确要求继续 M3-07。本轮只实现 Agent Origin Resolver 及完成它所必需的 Server 提交前语义校验；未启动 M3-08 Health Scheduler、Health goroutine、服务级 Eligible 或额外 Egress Policy。
- 统一 Origin 契约：新增 `internal/originconfig`，由 Repository/Application 的事务提交边界与 Agent Snapshot Candidate 共同复用。HTTP/HTTPS/TCP、规范 ASCII DNS/IPv4/IPv6、Port、Connect Timeout、TLS Server Name 和 HTTP Host Header 组合在 Server 提交前即校验，Agent 保留同义防御校验；不在 Apply 时解析 DNS或固化单一 IP。V0.1 按冻结边界允许受信管理面配置 Loopback、RFC1918 和其他内网 Origin，不提前实现 CIDR、Link-local、Metadata IP 或 DNS Suffix denylist。
- 发布与连接生命周期：进程级 Resolver 同时作为 `configruntime.Builder` 与 OPEN Dialer。Candidate Start 仅按 identity 注册不可变索引，`Gate.Active()` 是唯一可见性条件；Swap 至 APPLIED Ack 返回期间、Ack 失败或持续多 active 内部异常均 fail-closed，不回退旧配置。Abort/Retire 通过 `sync.Once` 只注销条目；Resolver 锁内只复制连接计划，DNS、TCP、TLS、Close 均在锁外，Candidate Context 不拥有业务 Dial 或已建立 Connection。
- Dial/TLS/Error：每条连接使用系统 `net.Dialer.DialContext` 重新解析 DNS，多 A/AAAA、IPv4/IPv6 回退交给标准库；Service `connect_timeout` 同时约束 DNS、TCP 与 `HandshakeContext`。HTTPS 校验名按显式 `tls_server_name`、DNS `origin_host`、IP SAN 顺序确定；`tls_verify=false` 只接受 Snapshot 显式值且仍发送有效显式 SNI。Refused、Timeout、Unreachable、TLS、Service Missing/Disabled/Not Observed 与内部不变量使用冻结 ErrorCode，错误文本不携带 Origin 地址或 TLS 细节。
- 生产接线与真实变更：移除生产 `Config.OriginDialer`、`UnobservedOriginDialer` 和 OPEN 固定 10 秒 Dial 上限；Bootstrap 仍只接收 Connection Token。真实 TLS Gateway/Control/Work/SQLite/Snapshot/Reconciler/Agent/WorkPool/Tunnel Proxy/TCP Echo 集成先连接 Origin A，再经 `ServiceManagementService.Update` 提交 Origin B、推进 Revision 并 MarkDirty；同一 Agent Runtime 未重启即在第二条连接切换到 B，旧 A 已先退出，不能产生误通过。
- 取消语义修正：Builder 语义错误以 `PROTOCOL_ERROR` REJECTED；Apply 或 Manager Owner 在 Build/Start 阶段取消时，Abort 后直接返回 Context 错误且不发送 ConfigAck。测试覆盖 Apply cancel、parent cancel 异步桥接窗口、Abort exactly-once、Ack=0 和 current 不发布。
- 独立复审：三项首轮只读复审发现 Server/Agent 校验漂移、动态 Origin 集成缺口、内部错误码归因和 Owner Cancel 窄竞态；逐项修复后分别复核为 Closed，当前无剩余 P0/P1/P2。动态 Origin 真实集成 `go test -race -count=50` 通过。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。相关六包与真实集成 `go test -race -count=10 -timeout 180s` 通过；Apply 取消用例 `go test -race -count=50`、Config Runtime/Origin/Connector `go test -race -count=5` 通过；`go test -race -count=1 -timeout 180s ./...`、`go vet ./...` 与 `go mod verify` 全部通过，完成 GoFmt。
- 证据边界：本轮未提交、未推送，没有 Commit SHA 或对应 CI Run；因此 M3-07 只进入 `REVIEW`，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`，本次未勾选任何产品任务或 Gate。当前工作树继续混合 M3 多阶段 staged/unstaged 差异，提交前必须重新整体暂存最终版本并从 staged snapshot 复验。
- 文档同步：实现符合已冻结 Origin Resolver、Connection、TLS 与 SSRF 契约，无需修改总技术方案、Proto、OpenAPI、Server Schema、Migration、第三方依赖或锁文件。README 已同步动态 Reconcile、真实 Origin Resolver、DNS/Timeout 和私网访问边界；开发计划已同步任务表、当前队列和证据。M3-08 保持 `NOT_STARTED`，本轮到此停止并等待用户阶段 Review。

## 2026-08-27 · M3-08 中心 Health Scheduler · REVIEW

- 授权与边界：用户通过 M3-07 阶段 Review 后明确要求继续开发。本轮只实现 M3-08 中心 Health Scheduler 及其必要的 Snapshot Candidate/Origin 生命周期接线；未实现 M3-09 Health Batch、Control Outbox Report、Revision Eligible Selection 或后续状态聚合。
- 中心调度与预算：每个 Connector 只创建一个 Health Manager，由单 Owner、Heap 时序和固定 64 个 `safego` Worker 管理全部 Service；固定全局并发 64、每 Origin `host:port` 并发 4、每秒最多 50 次检查，initial jitter 为 `[0, interval]`、后续 jitter 为 `[0.8, 1.2] × interval`，没有 per-service ticker 或 fire-and-forget goroutine。
- 检查与状态：Health Timeout 是外层预算，Origin `connect_timeout` 继续作为内层 DNS/TCP/TLS 预算，并复用 M3-07 Candidate-scoped Resolver/Dial/TLS Policy。TCP 成功连接后立即关闭；HTTP 使用 GET、显式 Host、Connection Close、只读取响应头并按包含边界的状态码范围判断。UNKNOWN 首次成功立即 HEALTHY；连续失败/成功严格按阈值切换，反向结果重置 streak；预算超时进入 `UNKNOWN/HEALTH_BUDGET_EXCEEDED` 且不计 Origin 失败，绝对 Stale Deadline 不晚于上次完成后的 `2 × interval`。
- 原子发布与生命周期：Connector Composite Candidate 按 Origin→Health 构建/启动、Health→Origin Abort/Retire；Health 只使用当前 generation 的不可变 scoped dialer。Gate 空窗立即隐藏状态并暂停检查，未变化 Service 保留状态与 next due，变化 Service 重置 UNKNOWN，epoch/revision fencing 丢弃旧结果；Health 后台 fatal 会取消 Runtime，Shutdown 即使 Deadline 到期也等待全部受控 goroutine 退出。
- 独立复审：首轮只读复审发现最坏 jitter 下 Stale Deadline 可延迟到 `2.2 × interval`，以及首次注销超时会耗尽 once 并留下旧 Plan；修复为绝对 `2 × interval` 上限和失败可重试的显式注销状态后，确定性回归测试与二次复核均确认 Closed，当前没有剩余 P0/P1/P2。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。相关 Health/Origin/Connector 包 `go test -race -count=20 -timeout 180s` 与定向 `go vet` 通过；全仓 `go test -race -count=1 -timeout 240s ./...`、`go vet ./...`、`go mod verify` 和 GoFmt 均通过。修复复核后 Health 包再次执行 `go test -race -count=20 -timeout 180s` 与 `go vet`，结果通过。
- 证据边界：本轮未提交、未推送，没有 Commit SHA 或对应 CI Run；因此 M3-08 只进入 `REVIEW`，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`，本次未勾选任何产品任务或 Gate。当前工作树继续混合多阶段 staged/unstaged 差异，且 M3-08 新文件尚未暂存；提交前必须重新整体暂存最终版本并从 staged snapshot 复验。
- 文档同步：实现符合已冻结 Health Scheduler、Origin 和 Config Runtime 契约；总技术方案、Proto、OpenAPI、Server Schema、数据库 Migration、第三方依赖、锁文件、用户命令与部署方式均保持不变。README 已同步中心 Health 能力，开发计划已同步任务表、当前队列和验收边界。M3-09 保持 `NOT_STARTED`，本轮到此停止并等待用户阶段 Review。

## 2026-08-27 · M3-01 Future Origin Persistence Reservation · REVIEW

- 授权与边界：用户明确要求在 `000005_services.sql` 初始建表时前置支持下一小版本的 UDP、QUIC 与 Unix Socket。本轮只调整持久化形态与约束测试，不实现新协议的 Proto、Application、Snapshot、Resolver、Health 或数据面，也不启动 M3-09。
- 持久化契约：`origin_scheme` 固定接受 `http/https/tcp/udp/quic/unix`。网络 Scheme 必须使用 Host+Port 且 Path 为空；`unix` 独占绝对文件系统 `origin_path`，Host/Port 必须为空；原生 `quic` 必须提供单个 `origin_quic_alpn`，透明 QUIC 流量归入 `udp`。TCP/UDP/Unix 禁止 TLS Server Name 与 HTTP Host，QUIC 禁止 HTTP Host。
- Health 与运行时边界：UDP、QUIC、Unix 预留行当前只能 Disabled Health。`internal/originconfig`、Repository/Application、Protocol Snapshot 与 Agent Resolver 继续只执行 HTTP/HTTPS/TCP；新增字面回归测试确认三个预留 Scheme 在 V0.1 Runtime 仍 fail closed，数据库可表达不等于功能已经发布。
- 迁移兼容边界：本次按用户要求直接修改发布前 Version 5 初始建表，只对新数据库生效。已执行旧 Version 5 的开发数据库必须删除重建；若存在需保留的已部署数据，必须新增向前 Migration，当前 Migrator 不会因 SQL 内容变化自动重放 Version 5。
- 独立复审：首轮发现 QUIC ALPN 的 255 上限按 Unicode 字符而非 UTF-8 字节计算，以及新增 Path/ALPN 列缺少直接边界用例；修复为 `BLOB` 字节长度约束并补齐空值、空白、NUL、ASCII/多字节超限、Unix Root/NUL 用例后，二次复核确认 Closed，当前无剩余 P0/P1/P2。
- 验收：Windows `go1.27.0`、`GOTOOLCHAIN=local`。初轮 `go test -race -count=10 -timeout 180s ./internal/repository/sqlite ./internal/originconfig` 通过；修复后 `go test -race -run '^TestServiceMigrationConstraints$' -count=20 -timeout 180s ./internal/repository/sqlite`、相关包 Race 与 `go vet` 通过；最终 `go test -race -count=1 -timeout 240s ./...`、`go vet ./...`、`go mod verify` 与双 Diff Check 全部通过。DDL 用例覆盖三种合法预留形态、Host/Port/Path 互斥、QUIC ALPN、字段污染、大小写/未知 Scheme、端口与字节边界和预留 Scheme 禁止启用现有 Health。
- 证据边界：本轮未提交、未推送，没有 Commit SHA 或 CI Run；M3-01 继续保持 `REVIEW`，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`，本次未勾选任何产品任务或 Gate。`internal/originconfig/validate_test.go` 处于 staged/unstaged 混合状态，提交前必须重新整体暂存并复验 staged snapshot。
- 文档同步：总技术方案的 Service Schema 与持久化/Runtime 边界、开发计划 M3-01 验收范围和执行证据已同步。Proto、OpenAPI、Server Config Schema、README、依赖、锁文件、用户命令与部署方式不更新，因为本轮没有新增当前版本可用能力。

## 2026-08-27 · M3-09 Health Batch/Revision Fencing + Eligible Selection · REVIEW

- 授权与边界：用户要求继续按开发计划推进。本轮只实现 M3-09 的 Agent Health Pending/Batch、Control Session generation、Server Revision/Freshness Fencing 与服务级 Connector Eligible Selection；未启动 M3-10 Health Target Budget、M3-11 Status 或后续里程碑。
- Agent 与 Outbox：Health Scheduler 通过容量一变更通知和权威快照驱动 Reporter；同一 Service 只保留最新状态，固定每秒或累计 128 项 Flush。完整 Snapshot 使用 Outbox 原子替换待发 Health 集合，保留非 Health 消息；APPLIED ConfigAck 与重连全量 Health 在同一 Outbox 事务中提交，Writer 不可能插入旧 Health。真实 generation 只在当前 Session 出队冻结 Frame 时分配；满 1000 Service 集合替换、空集合、容量失败原子性、REJECTED 与失败重试均有确定性覆盖。
- Server Fencing：Server 整批拒绝空、非法、重复 Service 或超过 128 项的 Batch；重复/倒退 generation、旧 Session 不污染当前状态，未知 Service 和错误 `service_revision` 单项丢弃。`checked_at_ms` 只保存供展示，freshness 使用 Server 本地 monotonic `received_at`，超过 `2 × interval` 即 fail closed。
- Eligible 与数据面：完整 Session identity、Config Ready、Observed/Required Revision、Service Enabled、Health 状态与 TTL 以值型快照在 `TunnelRuntime.mu` 下统一线性化。Health Disabled 通过健康门禁；启用 Health 必须为当前 Revision 的新鲜 HEALTHY。选择过程不再在 Runtime 锁内回调 Session Manager，候选仍按 Idle/Capacity、Least Active + RR；Pending waiter 同时监听 Eligibility generation 与 TTL，失效后取消并 join 旧 Pool Acquire、exactly-once 释放 membership/lease，并在原 Deadline 内改选。
- 独立复审与修复：首轮发现满容量集合替换溢出、Pending 不及时改选、Health 跨 Owner 锁序和 Server 未限制 128 项四个 P1；修复后交叉复审又发现 ConfigAck 与全量 Health 间可插入旧 Batch 的一个 P1。五项均已修复并交叉复核 Closed，最终无剩余 P0/P1/P2；另补 TTL 自动重选、跨 Service Pending 聚合、原子提交失败不变与可重试测试。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。核心七包 `go test -count=20 -timeout 240s` 通过；Control/Agent/Runtime/Session/Tunnel 与真实集成链 `go test -race -count=5 -timeout 300s` 通过；原子失败与 Pending 边界用例额外执行 `-count=20`/Race `-count=10` 通过；最终 `go test -count=1 -timeout 240s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...`、`go mod verify`、GoFmt、`git diff --check` 与 `git diff --cached --check` 全部通过。
- 证据边界：本轮未提交、未推送，没有 Commit SHA 或对应 CI Run；因此 M3-09 只进入 `REVIEW`，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`，本次未勾选任何产品任务或 Gate。当前四个新文件存在 staged 基线与后续 unstaged 修复，另有 Eligibility 新文件未暂存；提交前必须重新整体暂存最终版本并从 staged snapshot 复验。
- 文档同步：README 已同步 Health Batch 与服务级 Eligible 现状，开发计划已同步任务表、当前队列和执行证据。实现符合冻结技术方案；总技术方案、Proto、OpenAPI、Server Schema、数据库 Migration、第三方依赖、锁文件、用户命令与部署方式均保持不变。M3-10 保持 `NOT_STARTED`，本轮到此停止并等待用户阶段 Review。

## 2026-08-27 · M3-10 Health Target Budget Manager · REVIEW

- 授权与边界：用户在 M3-09 本地实现与复审后要求继续。本轮只实现 M3-10 Health Target Budget Manager、配置事务接线、启动基线重建与 Control Auth 容量拒绝；未启动 M3-11 Status、Management REST 或后续里程碑，也未修改机器契约、数据库 Schema、依赖或锁文件。
- Budget Manager：新增单锁、无 IO/回调/Channel 等待的两级 Manager，按 `health-enabled services × unique online connectors` 同时执行单 Tunnel 与 Server 全局硬上限。配置使用 Reserve/Commit/Release；增容立即保守占槽，减容到 Commit 才释放；revision 与单 Reservation fencing 防止旧配置终结新状态，乘法与总量溢出全部 fail closed。
- 配置事务：Service Create/Update/Delete 先在内存构造完整 Candidate，执行 Snapshot Gate 和 Budget Reserve，再首次写入 SQLite；事务提交后先 Commit Budget，再 MarkDirty，Gate/数据库/CAS 失败均 Release。只改 Name 与完整 no-op 不预留。交叉复审发现 SQLite Commit 与 Budget Commit 间存在同 Tunnel 第二次写误报 Reservation Conflict 的 P1；补充 per-Tunnel mutation owner 覆盖 Candidate 读取至 Budget Commit/Release，确定性测试证明同 Tunnel 连续 Revision 串行、不同 Tunnel 不被全局阻塞。
- Runtime 与 Auth：Connector Target 的唯一 Owner Key 是 `(tunnel_id, connector_id)`，generation 仅持独立引用做 fencing。首次 Session 在 `TunnelRuntime.mu` 内按固定 `Runtime.mu → Budget.mu` 顺序取得 Target 后才发布 Current；同 Key replacement 不增加 Target。Finalize、Rollback、乱序旧代 cleanup、Revoke 与 ActiveWork Tombstone 都只释放自身引用，最后一个 Runtime 引用归零才真正归还 Target。容量不足的 Control Auth 返回可重试 `HEALTH_BUDGET_EXCEEDED` 与 `retry_after_ms`，不留下半 Session。
- 启动重建：Migration 和存储打开后、任何 Gateway 或 Admin Bootstrap Listener 前，在单次稳定 Repository Read 中校验全部 Snapshot，并按 Desired Revision 与 `Enabled && Health != nil` 重建 Manager 基线；同一 Manager 实例注入 Runtime Registry。Lifecycle 对 nil Manager 入口快速失败，避免生产装配静默禁用预算。
- 独立复审：Runtime/Auth 与 Bootstrap 复审无阻塞发现；Bootstrap 的 nil Manager P2 已关闭。Manager/Application 首轮复审发现上述 SQLite/Budget 提交窗口 P1，修复后对 owner 引用、锁序、失败释放、Notifier 锁外执行与跨 Tunnel 并发重新复核通过，最终无剩余 P0/P1/P2。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。Health Budget、Application、Runtime、Control Auth 与 Bootstrap 分别完成普通测试 `-count=20`、Race `-count=10` 和定向 Vet；并发 replacement 与边界用例额外高重复通过。最终 `go test -count=1 -timeout 240s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...`、`go mod verify`、GoFmt、`git diff --check` 与 `git diff --cached --check` 全部通过。
- 证据边界：本轮未提交、未推送，没有 Commit SHA、staged snapshot 复验或对应 CI Run；因此 M3-10 只进入 `REVIEW`，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`，本次不勾选任何产品任务或 Gate。当前工作树继续混合 M3 多阶段 staged/unstaged/untracked 差异，提交前必须重新整体暂存最终版本并从 staged snapshot 复验。
- 文档同步：README 已同步两级 Health Target 预算、Auth 超限和启动重建现状；开发计划已同步任务表、当前队列与验收边界。实现符合已冻结总技术方案；Proto、OpenAPI、Server Schema、数据库 Migration、第三方依赖、锁文件、用户命令与部署方式均保持不变。M3-11 保持 `NOT_STARTED`，本轮到此停止并等待用户阶段 Review。

## 2026-08-27 · M3-09/M3-10 未提交变更 Review 修复 · REVIEW

- 授权与边界：用户在未提交变更审查后明确要求修复。本轮只关闭三个 Review finding；未修改 Proto、OpenAPI、Server Schema、数据库 Migration、第三方依赖、锁文件、暂存区或提交历史，也未启动 M3-11。
- Pending Group：恢复每个 Tunnel 同时最多一个 Pending Group。兼容且 Eligible 的不同 Service 共享该组；不兼容或当前不可用的 Service 在锁外等待原组释放，再在原 Deadline 内重新选择，既不产生第二个推测性 Demand，也不把连接导向对该 Service 不健康的 Connector。最后一个 membership 负责 exactly-once 删除组、关闭等待信号并归零对应 Session 的 pending 计数。
- post-COMMIT cleanup：Service Create/Update/Delete 将 `ErrPostCommitCleanup` 识别为数据库已经提交，继续 Commit Health Budget、释放 mutation owner 并触发 Reconcile；返回真实已提交 Result，并用错误链同时保留 cleanup 与 convergence 失败。测试覆盖三种写操作的持久化结果、Budget Revision/Target 与通知调用。
- Drain 顺序：Control Session Owner 新增实际写完成屏障；Flush 只有在没有 in-flight Frame 且 Outbox 已空时成功，调用方 Context 取消和 Owner 关闭都能有界收敛。Agent Drain 在最终 Health Reporter Flush 后等待此前 Health Frame 真正写出，再入队高优先级 DrainRequest，避免优先级队列让 Drain 越过最终 Health Batch。
- 独立复审：并发、锁序、丢失唤醒、重复关闭、错误链与 Wire 顺序均经独立只读复核；三个原 finding 全部关闭，最终无剩余 P0/P1/P2。
- 本地验收：Windows `go1.27.0`、`GOTOOLCHAIN=local`。相关五包 `go test -race -count=5 -timeout 300s` 通过；三项关键回归与 Flush 取消路径 `go test -race -count=20` 通过；`go test -count=1 -timeout 240s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...`、`go mod verify`、GoFmt 与双 Diff Check 全部通过。
- 证据边界：本轮仍是 staged/unstaged 混合的脏工作区，没有 staged snapshot 复验、Commit SHA、push、对应 CI Run 或 OS 原生 Shutdown Smoke；因此 M3-09 与 M3-10 均继续保持 `REVIEW`，本次未勾选任何产品任务或 Gate。提交前必须重新暂存最终版本，并从 staged snapshot 复验。
- 文档同步：修复恢复既有冻结行为，本次只新增开发计划执行记录；总技术方案、README、Proto、OpenAPI、Server Schema、数据库 Migration、依赖、锁文件、用户命令与部署方式均无需更新。

## 2026-08-27 · M3-11 Status Calculator Core · BLOCKED

- 授权与边界：用户要求继续按开发计划和规范推进。本轮严格限制为新增 `internal/server/status` 的纯值型状态计算与表驱动测试；未修改 Proto、OpenAPI、Server Schema、数据库 Migration、依赖、锁文件、REST/Web 或生产装配。
- 状态算法：Tunnel 固定执行 `REVOKED/PENDING/OFFLINE/ONLINE/DEGRADED`；Connector 在 Current Control Session、Heartbeat、Config Ready、Drain 与 Connector-wide Transport 上生成 `ONLINE/DEGRADED/DRAINING`，断开或过期不伪造永久 OFFLINE；Service 严格执行 `DISABLED > APPLY_FAILED > TUNNEL_OFFLINE > CONFIG_SYNCING > ORIGIN_UNHEALTHY > NO_CAPACITY > READY`。
- 多 Connector 与旧状态 fencing：Service 在模块内部按同一 Connector 依次累积 Live、Observed Revision、同 Revision Fresh Health 与 Capacity，禁止把一个 Connector 的 Healthy 与另一个 Connector 的 Capacity 拼成 READY；Tombstone、旧 Revision/过期 Health、未完成首份 Config Ack、Draining 和无 Capacity 均有失败分支测试。Apply Failure 只在仍匹配当前 Required Revision 时覆盖状态，并携带稳定错误码与失败时间输入。
- 独立复审与阻塞：复审发现现有 `repository.Tunnel` 没有跨 Server 重启保留的“曾成功认证”事实，重启后无法区分 PENDING 与 OFFLINE；现有 `ConnectorSnapshot` 也没有在同一线性化点暴露 Config Ready、Observed Revision、Fresh Health 与真实 Capacity，且认证后、Config Ack 前可暂时显示 ONLINE。正确接线需要新增数据库字段/Migration/Repository 写入口，并新增 Session Runtime 联合只读快照跨模块契约；两项均属于 Ask First，M3-11 因此标记 `BLOCKED`。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。`go test -count=20 ./internal/server/status`、`go test -race -count=10 ./internal/server/status`、定向 Vet 通过；最终 `go test -count=1 -timeout 240s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...`、`go mod verify` 与双 Diff Check 全部通过。
- 证据边界：本轮没有 Commit SHA、push、CI Run 或生产接线证据；M3-11 未进入 `REVIEW/DONE`，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`，本次未勾选任何产品任务或 Gate。两个新状态文件存在 staged 基线与后续 unstaged 修正，开发计划为 unstaged；继续实现或提交前必须重新暂存最终版本并从 staged snapshot 复验。
- 文档同步：只同步开发计划任务状态、当前队列与阻塞证据。冻结总技术方案没有变化；README、Proto、OpenAPI、Server Schema、数据库 Migration、依赖、锁文件、用户命令与部署方式不更新，因为尚无已接入的用户可见能力。

## 2026-08-27 · M3-11 Durable Authentication + Runtime Status Wiring · REVIEW

- 授权与边界：用户明确确认继续数据库 Migration 与跨模块 Runtime 快照接线。本轮只完成 M3-11 的生产输入链路、失败分支、复审修复和文档同步；未启动 M3-12，未修改 Proto、OpenAPI、Server Schema、REST/Web、第三方依赖、锁文件、用户命令或部署方式。
- 首次认证事实：新增 v6 Migration `first_authenticated_at`，只允许 `NULL` 或正 UTC Unix 秒。Tunnel Repository 以 `WHERE first_authenticated_at IS NULL` 单调写入，首次写使用 `WithDurableTx(FULL)`，重复认证先走只读快路；该历史事实不推进 Tunnel Aggregate `version/updated_at`，也不持久化 Connector、Session、在线状态或 `last_seen_at`。
- Control Auth 提交：真实 SQLite Store 已注入生产 Handler。只有完整 Connector Auth Success Frame 写出且本地协议状态提交后才记录首次认证；写帧失败不落库。耐久写失败时不发送第二个 AUTH 结果，使用完整 Session identity 清理本 generation、关闭连接并返回不含 Secret 的错误。真实 Token/SQLite 集成测试覆盖成功认证、读取事实、关闭 Store、重开同一数据目录和跨重启值不变。
- Runtime 联合快照：Session Manager 先短暂复制 Current 候选，锁外读取 Heartbeat/WorkPool，最后由 Tunnel Runtime 在同一临界区复制 Lifecycle 与已发布 Eligibility 并执行完整 generation fence；replacement、revoke、Tombstone 和旧 generation 不会进入结果。Status 不读取尚未发布的 managed Config/Health，Services Map 返回独立副本，ConfigReady、Observed Revision、同 Revision Fresh Health 与 Capacity 始终来自同一个 Connector generation。
- 状态接线：`TunnelInputFromRepository` 只用 `FirstAuthenticatedAt` 区分 `PENDING/OFFLINE`，并消费 `RevokedAt`；Connector 在首份 Config Apply/Ack 前保持 `DEGRADED`。Lifecycle 或 Pool 任一进入 Drain 都立即展示 `DRAINING`；Drain 保留只读已发布 Eligibility，使 Service 正确显示 `NO_CAPACITY` 而不是回退为 `CONFIG_SYNCING`，数据面仍由 Lifecycle 非 ONLINE fail closed。Service 保持同 Connector Revision/Health/Capacity 串行门禁，禁止跨 Connector 拼接 READY。
- 独立复审与修复：认证/持久化与 Runtime/Status 两路并行只读复审。复审发现持久事实缺少生产组装与重启集成证据、未发布 Eligibility 泄漏、Lifecycle/Pool Drain 窗口、ConfigReady 技术方案漂移，以及 Drain 删除 Eligibility 导致 Service 错报等问题；全部补实现或确定性测试并经原审查者复核 Closed。最终无剩余 P0/P1/P2。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。Repository/Auth/Bootstrap/Runtime/SessionRuntime/Status 定向普通测试 `-count=20` 通过；关键并发与竞态路径 Race `-count=5` 或 `-count=10` 通过；定向 Vet 通过。最终 `go test -count=1 -timeout 240s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...`、`go mod verify`、GoFmt、`git diff --check` 与 `git diff --cached --check` 全部通过。
- 证据边界：本轮未提交、未推送，没有 Commit SHA、对应 CI Run 或 staged snapshot 复验；因此 M3-11 只进入 `REVIEW`，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`，本次不勾选任何产品任务或 Gate。工作树继续混合 staged/unstaged 差异，提交前必须重新整体暂存最终版本并从 staged snapshot 复验。
- 文档同步：总技术方案第 39/40/57 节已同步跨重启首次认证事实、ConfigReady、已发布 Runtime 快照与 Tunnel SQL；开发计划已同步任务表、当前队列和复现证据。README 不更新，因为尚无 REST/Web 或用户命令可消费该状态；Proto、OpenAPI、Server Schema、依赖和锁文件保持不变。下一项 M3-12 保持 `NOT_STARTED`，本轮到此停止并等待用户阶段 Review。

## 2026-08-27 · M3-12 Durable Operations：Backup/Restore · REVIEW

- 授权与边界：用户明确确认采用推荐契约继续，并确认项目仍在开发中，不做旧 `/var/lib/xtunnel/xtunnel.db` 根布局迁移兼容。本轮获准修改 Linux root 维护命令、本机 Socket 协议、生产 Data Directory 默认布局、systemd/OCI 产物、内部公共接口、持久化归档格式、稳定日志事件和 `modernc.org/sqlite` 直接依赖归类；未修改 Proto、OpenAPI、数据库 Migration 或 `go.sum`，未提交、推送或改变暂存区。
- Stable Target 与部署：生产默认改为 Stable Parent `/var/lib/xtunnel`、可 rename Data Target `/var/lib/xtunnel/data`。systemd 与 OCI 预创建归 Runtime UID/GID 所有的 `0700` leaf，Volume/StateDirectory 挂载父目录；systemd 安装器在任何覆盖写入前检测旧根级 DB、WAL/SHM、credentials、pki 并 fail fast，不自动迁移。
- 在线/离线互斥：Linux root `backup create --output <absolute>` 优先连接权限 `0600` 的 target-bound Unix Socket；Server 与 Client 双向 `SO_PEERCRED`/Target Hash 绑定，连接所有、FIFO、可取消的 Store `writeGate` 同时阻止 SQLite 写事务与 Pinned Identity 续签。唯一 Reader 把 EOF/Shutdown 转为 Create 取消，Archive 完整落盘并取得 release ACK 前不承诺发布；Socket 不存在才获取同一 External Lock 离线执行，连接或认证失败禁止回退。`backup restore --input <absolute>` 始终要求离线 External Lock。
- SQLite 与一致性单元：CLI 用 `openat2 + O_NOFOLLOW + fstat` 固定源 DB inode，Schema 检查与 Online Backup 通过同一 FD 的 `/proc/self/fd` 引用完成；每次打开前后和操作完成后安全重开原路径并 `SameFile` 核对，symlink、path replacement、rename + held WAL 一律 fail closed 并删除候选。Archive 一致性单元包含 SQLite 自包含备份、32 字节 Tunnel Token Master Key，以及 pinned 模式最终 Gateway key/certificate；Pending Gateway Rotation 任意子集都会拒绝 Create。
- Archive 与 Restore：格式固定为 canonical USTAR + Manifest v1、稳定白名单/顺序/Mode/Size/SHA-256、DB 64 GiB/Key/Cert 上限，拒绝 PAX/sparse、路径穿越、链接/特殊文件、未知/重复项及 canonical Tar 末尾后的字节。输出用安全父目录 FD + `O_EXCL 0600` 创建，失败只以该 FD relative `unlinkat` 清理。Restore 在 root-owned `0700` sibling staging 完成文件集、immutable SQLite `quick_check`、精确 Schema、Token Key、全部 Token AEAD/AAD/身份/Secret Hash、Pinned Identity 校验后，才以 rollback + versioned Journal 切换；Schema v1 没有 `tunnel_tokens` 时保持原样恢复，下次正常启动再 forward migration。
- 崩溃恢复与删除安全：Journal 保存 canonical Manifest 与 Hash，`prepared/rollback_ready/installed` 每阶段原子更新并同步父目录；第二次 rename 后只有新 target 完整重验通过才前向提交，否则恢复 rollback。无 Journal 只清理唯一安全的 orphan staging；orphan rollback fail closed。删除 rollback 前先以 FD-relative 两阶段遍历证明全树无 symlink、特殊文件或不同 `statx mount ID`，nested bind mount 测试确认删除前旧文件与外部文件均保持不变。
- 独立复审：契约/安全、并发/部署、Gateway Barrier 三路只读复审先后发现 Token Key 未解密密文、失败清理路径 TOCTOU、Lease 断线仍保留输出、SQLite symlink/rename/WAL、Read 视图写绕过和 Schema v1 恢复回归；全部补生产修复与对抗测试后，截至该轮复审未报告剩余 P0/P1/P2，允许进入 `REVIEW`。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。最终 `go test -count=1 -timeout 300s ./...`、`go test -race -count=1 -timeout 360s ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff`、GoFmt、双 Diff Check 与 Server Schema JSON 解析均通过。Linux amd64 交叉构建 ELF 后在 WSL root 原生运行 SQLite/DurableOps/Bootstrap/Gateway 四包全量测试通过，覆盖 root CLI 在线/离线 Create/Restore、bind mount、Socket shutdown、错误 Key Recovery、Schema v1 Restore、symlink/path replacement/held WAL/rename + held WAL；Linux arm64 Server 与 SQLite/DurableOps/Bootstrap 测试二进制交叉编译通过；三个部署 Shell 通过 `sh -n`。
- 证据边界：本轮没有 Commit SHA、staged snapshot 复验或 CI Run；当前环境没有 Docker，未执行 OCI Runtime Smoke，也未执行 systemd 原生安装/启动 Smoke 与 Linux `-race`。因此 M3-12 只进入 `REVIEW`，M3 Gate 保持未勾选，M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`，本次未勾选任何产品任务或 Gate。工作树继续混合 M3-11 staged 基线、后续 unstaged 修复和 M3-12 untracked 文件；提交前必须重新整体暂存最终版本并从 staged snapshot 复验。
- 文档同步：总技术方案第 26 节、README、开发计划任务表/队列/证据、Server Schema 默认值和 systemd/OCI 布局已同步；仓库没有单独 Candidate Backlog。`go.mod` 将实现已直接导入的 `modernc.org/sqlite` 从 indirect 改为 direct，版本与 `go.sum` 不变。下一项 M3-13 保持 `NOT_STARTED`，其 CI、systemd/OCI Runtime Smoke 与完整 Checklist 不能由本地或交叉编译证据替代。

## 2026-08-27 · M3-12 未提交变更 Review 修复 · REVIEW

- 授权与边界：用户在未提交变更审查后明确要求修复。本轮只关闭两个 Linux Restore 状态机 P1，并补充对应回归测试和开发计划证据；未修改 Proto、OpenAPI、Server Schema、数据库 Migration、第三方依赖、锁文件、README、总技术方案、任务计数、暂存区或提交历史，也未启动 M3-13。
- `installed` 取消收敛：Journal 已持久化为 `installed` 时，新 target 已被承诺为完整有效。重启重验遇到 `context.Canceled` 或 `context.DeadlineExceeded` 现在直接传播错误并保留 target、rollback 与 Journal，禁止把维护取消误判为内容损坏后恢复旧数据；只有确定的状态校验失败才执行 rollback。
- Journal 崩溃屏障：`finishInstalled` 删除旧 rollback 后立即 `fsync` Stable Parent，确认该目录项变化落盘后才删除 Journal，并再次同步父目录。确定性测试在两次同步点分别断言 `no rollback + journal` 与 `no rollback + no journal`，避免断电后形成无法解释的 `target + rollback + no journal`。
- 回归覆盖：Linux 表驱动测试覆盖取消与 Deadline 两种错误均不回滚已提交 target，并验证旧 rollback 和 Journal 保留；另一个测试通过局部、无全局状态的同步注入点验证两道父目录持久化屏障的严格顺序。归档格式、在线 Barrier、权限、部署布局与用户命令均未改变。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。GoFmt、`go test -count=1 -timeout 300s ./...`、`go test -race -count=1 -timeout 360s ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff` 均通过。Linux amd64 DurableOps Test Binary 由固定 Go 1.27 工具链以 `CGO_ENABLED=0` 交叉编译，并在 WSL 原生运行完整包测试通过；WSL 本身未安装 Go，因此未在发行版内重新构建，也未运行 Linux Race。
- 证据边界：本轮仍是 staged/unstaged/untracked 混合的脏工作区，没有 staged snapshot 复验、Commit SHA、push、对应 CI Run、Docker OCI Smoke 或 systemd 原生安装/启动 Smoke。M3-12 继续保持 `REVIEW`，M3-13 仍为 `NOT_STARTED`，本次未勾选任何产品任务或 Gate；提交前必须重新整体暂存最终工作树并从 staged snapshot 复验。
- 文档同步：两项修复恢复总技术方案既有的 Journal phase/fail-closed 与“每次目录项变化后同步父目录”契约，README 的用户可见行为没有变化，因此只追加本执行记录并限定上一轮复审结论的时间边界；总技术方案、README 和机器契约无需更新。

## 2026-08-27 · M3-13 M3 Gate · REVIEW

- 授权与边界：用户确认继续下一步开发。本轮只补 M3 Gate 所需的 DurableOps 崩溃收敛修复和跨 Session 集成证据；未修改 Proto、OpenAPI、Server Schema、数据库 Migration、第三方依赖、锁文件、CI/CD、生产配置、暂存区或提交历史，也未提供旧布局迁移兼容层。
- Application/Session Gate：既有真实 TCP Echo E2E 已证明 Application Service 修改 Origin 后 Agent 无需重启即可生效；本轮进一步关闭 Gateway 后断言旧 Session 不再 Current/Eligible、Runtime Status 为空且数据面不能使用 Agent 进程内旧 Candidate 回退。重连代在真实 Token-only TLS/Auth 后被门闩阻塞于完整 Snapshot 读取，ConfigAck 前无 Pool/Eligible；放行后只接受 Revision 3 完整 Snapshot，并通过第三个 Origin Echo。
- Health Gate：新增第二代 Config Session 自动化证据。即使 Health 状态未变化，新 Session 的 observed 基线也必须归零，并严格按 `APPLIED ConfigAck → 当前 Revision 全量 Health Batch → 后续增量 Batch` 排序，禁止沿用上一代 Reporter 的去重基线。
- Archive 原子发布：Backup 不再直接写最终 OutputPath，而是在固定输出父目录 FD 下创建随机隐藏 `0600` 候选；文件完整 `fsync`/Close 且在线 release ACK 成功后，才以同一 dirfd 的 `renameat2(RENAME_NOREPLACE)` 原子发布并同步父目录。并发目标存在时禁止覆盖；失败只相对固定 dirfd 删除本次候选。Manifest 读取只接受与 `json.Marshal` 逐字节一致的 canonical JSON，Journal Hash 与归档声明绑定同一 canonical bytes。子进程 hard-exit 测试证明 ACK 前最终路径不可见。
- Restore 崩溃收敛：补齐 `prepared target-only`、`rollback_ready rollback-only/target-only`、`installed rollback-only` 可达组合；prepared 回滚按“删除 staging → 父目录 fsync → 删除 Journal → 父目录 fsync”收敛。启动时只清理固定前缀、普通 `0600` 的 Journal 临时文件，并用已打开父目录 FD 相对删除；符号链接或异常对象 fail closed 保留现场。
- Checklist 审计：M3 Gate 八项均已有自动化证据，覆盖 Snapshot Deterministic/Revision/Size/Count、Atomic Apply 生命周期与 Digest 规则、Health 调度/预算/Replacement fencing，以及在线/离线 Backup/Restore 与 Journal 状态表。提交与 OCI CI 证据已经补齐，但 Checklist 暂不勾选，因为仍缺少隔离 systemd Runtime Smoke。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。GoFmt、`go test -count=1 -timeout 300s ./...`、`go test -race -count=1 -timeout 360s ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff`、Server Schema JSON 解析与双 Diff Check 全部通过。Linux amd64 DurableOps、Agent Connector、Integration Test Binary 由固定工具链以 `CGO_ENABLED=0` 交叉构建，并在 WSL root 原生运行完整包测试通过；WSL 内未安装 Go，因此未运行 Linux Race。
- 独立复审与剩余风险：独立只读复审未报告 P0/P1/P2。两个非阻塞 P3 被显式保留：进程在隐藏候选完整写入后 SIGKILL 会留下私有 `.xtunnel-backup-pending-*` 磁盘垃圾但不会暴露最终路径；rename 成功后父目录 `fsync` 失败会返回“不确定持久化”错误并保留完整最终文件。前者进入 M7-04 Filesystem Failpoint 阶段评估显式清理策略，禁止在并发 Create 下按前缀盲删。
- Commit/CI 与部署证据：完整 staged snapshot 复验通过后创建并推送 `a3213e4e52733719a1e08eafcdbb7cae4015a7c1`（`fix(m3): harden gate recovery and reconnect evidence`）。[CI #33065901938](https://github.com/lifei6671/xtunnel/actions/runs/33065901938) 精确绑定该 SHA，Windows Agent Service 与 Linux `verify` amd64/arm64 矩阵全部成功；Workflow 中两个 Linux Job 均执行 Server/Agent 原生 OCI Smoke。此前本地 WSL/Docker 阻塞尝试不计证据。systemd 249 可用且脚本涉及的 XTunnel Unit、Binary、配置/运行/数据目录及两个服务身份经只读预检均不存在，但 `deploy/systemd/smoke.sh` 明确只允许隔离 Linux 主机并会创建/删除这些系统对象；当前 WSL 未被确认可销毁，因此未执行。M3 `DONE` 仍为 `0/13`、全局仍为 `33/95`。
- 文档同步：总技术方案同步 Backup 输出的“隐藏候选 + ACK 后 no-replace 原子发布”契约；README 已准确表述“release ACK 后才发布”，无需重复扩写。开发计划同步任务表、当前队列、复现证据与 Gate 边界，机器契约保持不变。

## 2026-08-27 · M4-01 Immutable Route Snapshot · REVIEW

- 授权与边界：用户明确确认按推荐契约继续，并说明项目仍在开发期，不需要迁移兼容性改造。本轮新增 Migration 7 和内部 Repository/Route 契约，未修改 Proto、OpenAPI、Server Schema、第三方依赖、锁文件、CI/CD、生产配置、日志字段、暂存区或提交历史；未提前实现 M4-02 HTTP Matcher、M4-06 TCP Listener Manager 或 Management Route 写 API。
- SQLite 唯一权威：新增固定单行 `route_config_state`，generation 从 `0` 开始且只允许非负值；新增 `http_routes`、`tcp_routes`，数据库强制 HTTP `hostname + path_prefix`、TCP `public_port` 唯一、布尔值/端口/时间范围和 `service_id ON DELETE RESTRICT`。Route ID 外部格式尚未冻结，因此没有发明前缀或旧格式兼容层。Migration 自动化覆盖 v6→v7、幂等执行、失败原子回滚、约束与缺失 generation 权威失败。
- 完整读取与不可变快照：`RepositoryView.Routes()` 和 `sqlite.Store.LoadRouteDesiredState` 在同一个普通 SQLite/WAL 只读事务中读取 generation、Tunnel、Service、HTTP/TCP Route；运行时全量校验关联后构建私有 map/slice，只以值或副本暴露 HTTP/TCP/Tunnel 查询。`atomic.Pointer` 仅发布完整对象，禁用 Service/Route 与撤销 Tunnel 不进入公网路由，发布后的热路径不再读取 SQLite。
- 单 Owner 与 generation fencing：唯一 goroutine 在 `Start` 时同步完成首次全量构建，容量 1 dirty Channel 合并突发唤醒，独立原子值保留已通知的最大 generation。构建后同时检查 SQLite 最新 generation、内存 dirty generation 和已发布快照单调下界；新代次出现时丢弃旧候选并立即重建，generation 回退或内部关联损坏时保留旧快照。失败状态允许同 generation 显式重新入队，正常构建期间的重复通知仍被合并；Context 取消能解除 Source 调用，Bootstrap 在 Listener/Session 停止后 `Wait` owner，再允许 SQLite 关闭。
- 启动接线：Gateway Identity 与既有 Stored Snapshot/Health Gate 完成后、任何 Gateway Listener 启动前，同步加载首个 Route Snapshot；首次构建失败阻止启动并逆序清理。首个 Admin 尚未创建时 Route Snapshot 已存在，但 Public Ingress/Gateway 仍保持关闭；Linux 生命周期测试断言初始 generation 为 `0`。
- 关键回归：测试覆盖完整关联构建、输入/返回值不可变、禁用/撤销过滤、重复 Route ID/HTTP Key/TCP Port、构建期间 generation 前进、数据库 fence 与 dirty 通知之间的竞态、已发布 generation 回退、首次非零 generation 成为通知下界、突发 dirty 合并、构建失败不发布部分结果、同 generation 修复后重试、发布后查询零 Source IO、取消解阻塞和并发读跨多次原子发布。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。`go test -race -count=3 -timeout 180s ./internal/repository/sqlite ./internal/server/route ./internal/server/bootstrap ./internal/server/snapshot`、`go test -race -count=20 -timeout 180s ./internal/server/route`、`go test -count=1 -timeout 180s ./...`、`go test -race -count=1 -timeout 360s ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff` 与 `git diff --check` 全部通过。`CGO_ENABLED=0` 下 linux/amd64、linux/arm64 的 `go build ./...` 及 Bootstrap/Route Test Binary 交叉编译通过；未在本轮冒充 Linux 原生运行证据。首次未关闭 CGO 的交叉尝试因 Windows 缺少 Linux C 头文件/交叉汇编器失败，只作为环境排查，不计验收通过。
- 独立复审：首次只读复审发现两个 P2：异常 generation 回退可覆盖已发布快照、同 generation 瞬时失败后无法再次唤醒。补充单调下界与失败态同代次重入队后，又将回退检查前移以避免更高 dirty 对永久回退 Source 忙循环，并把首次非零发布代次提升为通知下界；复审指出后一项缺少直接回归测试（P3），补测后最终复跑 `go test -race -count=10 -timeout 120s ./internal/server/route` 和 `go vet ./internal/server/route`，结论 `APPROVED`，无剩余 P0/P1/P2/P3。
- 证据边界：当前仍是包含 unstaged/untracked 文件的脏工作区，没有完整 staged snapshot 复验、Commit SHA、push 或对应 CI Run；M0-10 后这些证据是 `DONE` 强制条件。因此 M4-01 只进入 `REVIEW`，M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`，本次未勾选任何产品任务或 Gate。M4-02 保持 `NOT_STARTED`，等待用户阶段 Review。
- 文档同步：总技术方案第 85 节补充 `route_config_state` 单行持久化权威及同事务提交要求；开发计划同步当前阶段、仪表盘、M4-01 状态、当前队列和本执行证据。README 不更新，因为尚无已接入的用户可见 HTTP/TCP Ingress、Route 管理命令或配置；Proto、OpenAPI 与 Server Schema 无需更新。

## 2026-08-27 · M4-03/M4-07 Service Proxy 前置契约与拨号基础 · 实现准备

- 授权与边界：用户确认 V0.1 继续支持 HTTP Host 和现有 `connect_timeout`，并冻结 disable chunked、disable Happy Eyeballs、HTTP idle timeout、HTTP max idle connections 与 TCP keepalive interval 五项 Service Proxy 契约；同时要求把 V0.1 不支持的协议与应用体验写入 V1.0，并保留类型化扩展点。本轮未实现 M4-03 HTTP Reverse Proxy、M4-07 Raw TCP Ingress、REST/OpenAPI、Web 表单或用户可见管理入口。
- V0.1 默认与边界：唯一 Go 默认入口为 `repository.ServiceProxyOptions.WithDefaults()`，冻结值是 `disable_chunked_encoding=false`、`disable_happy_eyeballs=false`、`http_idle_connection_timeout_ms=90000`、`http_max_idle_connections=100`、`tcp_keepalive_interval_ms=30000`，其中 KeepAlive `0` 表示显式禁用。Application 只按输入 presence 覆盖完整默认值；`connect_timeout_ms` 仍是 DNS/TCP/TLS 单一总预算，HTTP 专属字段只允许 HTTP/HTTPS Service。
- 持久化与 Wire 权威：新增向前 Migration 8，把五项配置作为 `services` 类型化列持久化，并覆盖 v7→v8 默认值升级、约束、失败原子回滚、CRUD 往返与显式 false/0。Protocol v1 在既有 `ServiceConfig` 后追加字段 12/13：所有 Service 必须携带 `OriginConnectionOptions`，HTTP/HTTPS 必须携带 `HTTPProxyOptions`，TCP 必须缺失后者；Agent 不对缺失消息提供兼容默认。生成物、文本契约测试和 Protocol Golden 已同步，Golden 明确冻结 30s TCP KeepAlive、90s HTTP Idle Timeout 与 TCP keepalive=0。
- 已接通运行基础：Server Snapshot 从 Repository 显式构建完整 Wire 子对象；Agent 把 disable Happy Eyeballs 映射为 `net.Dialer.FallbackDelay=-1`，把 keepalive=0 映射为 `net.Dialer.KeepAlive=-1`，每次 Dial 使用当前不可变 Service 参数且不改变既有 DNS/TCP/TLS 总超时与 TLS 错误脱敏。Route Snapshot 已携带 `origin_http_host`、disable chunked、HTTP idle timeout 与 max idle connections，后续 M4-03 必须以现有 `TunnelID + ServiceID + RequiredRevision` 隔离连接池。
- HTTP 待实现语义：Host 唯一优先级为 `origin_http_host > preserve_host > 规范化 origin host[:port]`。禁用 Chunked 时不得通过整包缓存计算长度，长度无法安全确定必须显式拒绝。每个 HTTP 池的 idle/max 设置仍受全局 WorkConn/Idle/FD 硬预算限制。这些行为尚未进入真实 HTTP 数据面，不能视为 M4-03 已开始或通过。
- 验证证据：Windows `go1.27.0`、`GOTOOLCHAIN=local`。`proto.sh lint` 与 `proto.sh breaking` 通过；重复生成的 `control.pb.go` 稳定。`go test -count=1 -timeout 60s ./...`、`go vet ./...`，以及 Repository/Application/Snapshot/Route/Agent Origin/Connector/Protocol 定向 Race 全部通过。正式 `generate-check` 需要干净 checkout，本次脏工作区结果不能冒充该 Gate；当前也没有 Commit SHA 或对应 CI Run。
- 独立复审：首次只读复审发现一个 P2：Route Snapshot 只有 `origin_http_host`，缺少 Host 第三优先级所需的 Origin scheme/host/port，M4-03 热路径无法在不查询 SQLite 的情况下构造回落 Host。修复后 `HTTPRoute` 从同代 Service Desired State 按值携带 `OriginScheme`、`OriginHost`、`OriginPort` 与 `OriginHTTPHost`，并补充源输入变更不污染已发布快照的断言；复审定向执行十轮 Route Race 与 Vet 后结论 `APPROVED`，无剩余 P0/P1/P2/P3。
- V1.0 规划：新建 `docs/xtunnel_standalone_v1.0.md`，只记录 SOCKS `proxy_type`、基于 Hostname 的 SSH/RDP/SMB、UNIX/UNIX+TLS、Bastion、独立 TLS timeout、custom CA/mTLS/HTTP2 Origin 和 Access 策略等未来类型化扩展点。V0.1 Raw TCP 可承载对应协议字节流，但不等于已提供 V1.0 的 Zero Trust 应用体验。Access 折叠截图无可见字段，因此不发明具体字段或通用 JSON 容器。
- 状态与文档边界：M4-03、M4-07 和 M4 Gate 均保持 `NOT_STARTED`，M4 `DONE` 仍为 `0/10`，全局仍为 `33/95`；本次未勾选任何产品任务或 Gate。README、OpenAPI、Server Schema 与 `AGENTS.md` 不更新：前两者尚无对应用户入口，Service Desired State 不属于 Server 主配置，本轮也没有新增协作规则。

## 2026-08-27 · M4-02 HTTP Host/Path Router · REVIEW

- 授权与边界：用户确认把现有间接模块 `golang.org/x/net v0.57.0` 提升为直接依赖，使用 `idna.Lookup` 完成 IDNA 规范化且不升级版本。本轮只实现 HTTP Matcher、Route 写入规范化函数、Snapshot canonical key Gate 和测试；未修改数据库 Migration、Proto、OpenAPI、Server Schema、CI/CD、生产配置、日志契约、暂存区或提交历史，也未提前实现 M4-03 HTTP Ingress/Reverse Proxy。
- Canonical Host：Route 写入值统一小写、移除一个 ASCII/IDNA 等价尾点、剥离并校验可选端口，再按 IDNA Lookup 生成唯一键；畸形 Label、控制字符、重复尾点与非法端口失败。公网 Host 额外按 HTTP authority 校验：IPv6 必须方括号包围，方括号不允许 IPv4；Snapshot 仍以 bare canonical IP 持久化和查找，避免地址/端口出现两种解释。
- Path 与匹配：非根 Route Prefix 解码等价 percent 表达后移除全部尾部 `/`，内部重复斜线不折叠。公网请求只接受 origin-form，逐项核对 `RequestURI`、`URL.Path`、`RawPath`、`RawQuery`、`ForceQuery` 以及 Scheme/Host/User/Opaque/Fragment 元数据；匹配使用已经验证的 decoded Path，执行 Exact Canonical Host + Longest Segment-boundary Prefix，因此 `/foo` 匹配 `/foo`、`/foo/`、`/foo//bar`，但不匹配 `/foobar`，且后续 M4-03 必须转发同一请求的原始路径语义。
- Fail-closed 矩阵：拒绝非法 percent、encoded slash/backslash、明文或编码 dot-segment、反斜杠、Unicode control、非法 UTF-8、request-target 明文 `#`，以及多层解码后形成危险分隔符或 dot-segment 的输入。递归检查按有限 8 层执行；更深仍可解码的输入直接失败。非首层的非法 literal percent 不会中止扫描，其后的合法 `%HH` 仍会被检查，关闭 `/foo/%25ZZ/%252Fbar` 绕过。Snapshot 构建对持久化 canonical Prefix 执行同一危险编码 Gate，数据库异常行不能绕过公网匹配边界。
- 回归覆盖：表驱动测试覆盖大小写/尾点/三种 IDNA 等价句点/端口/Unicode/IPv4/IPv6，root fallback、最长 Prefix、segment boundary、重复斜线与 Trailing Slash，RawPath 为空及非空的真实 `http.ReadRequest` 形态，query/ForceQuery/URL 元数据不一致、nil Request/URL/Snapshot、not-found，以及 Request、CanonicalPathPrefix、Snapshot 三层对抗编码矩阵；断言具体 Route、canonical Host、Path/RawPath、found 与错误类型。
- 独立复审：只读复审先后发现 request-target 明文 `#`、Snapshot encoded key 绕过、非法 literal percent 提前终止递归扫描、IDNA 等价尾点、裸 IPv6 authority、过期注释和缺失边界测试；全部补生产修复与直接回归。最终复审重新执行普通测试、20 轮 Race、Vet、Module Verify/Tidy Diff 和目标 Diff Check，结论 `APPROVED`，无剩余 P0/P1/P2/P3。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。`go test -count=1 -timeout 60s ./internal/server/route`、`go test -race -count=20 -timeout 180s ./internal/server/route`、`go test -count=1 -timeout 180s ./...`、`go test -race -count=1 -timeout 360s ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff` 与 `git diff --check` 全部通过。
- 证据与文档边界：当前仍是包含既有 M4-01、Service Proxy 准备和本次 M4-02 差异的脏工作区，没有 staged snapshot 复验、Commit SHA、push 或对应 CI Run，因此 M4-02 只进入 `REVIEW`，M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`，M4 Gate Checklist 保持未勾选。总技术方案同步 Exact Canonical Host 与完整 `INVALID_PATH` 语义；README 不更新，因为用户可见 HTTP Ingress 和 Route 管理入口尚未实现；Proto、OpenAPI、Server Schema 与 `AGENTS.md` 无需更新。下一项 M4-03 保持 `NOT_STARTED`，等待用户阶段 Review。

## 2026-08-27 · M4-03 Streaming Reverse Proxy · REVIEW

- 范围与契约边界：新增生产 HTTP Ingress Listener、Streaming Reverse Proxy 与 Tunnel-aware Transport，并接入现有 Route Snapshot、Tunnel Proxy、WorkPool、ActiveWork 和 Bootstrap 生命周期。本轮没有新增 Migration、Proto、OpenAPI、Server Schema、第三方依赖、配置项、日志字段或兼容垫片；Forwarded/Trusted Proxy、WebSocket、TCP Listener、Management Route 写 API 与公网 Rate/Body Limit 仍留给 M4-04 至 M4-09。
- 流式请求与路由：每个请求只读取一次不可变 Route Snapshot，严格匹配后保留原始 Path、RawPath、RawQuery 与 ForceQuery，不整体读取 Request/Response Body。Origin Host 唯一优先级为 `origin_http_host > preserve_host > origin host[:port]`；Server 到 Agent 固定发送 HTTP/1.1 明文字节，HTTP/HTTPS Origin TLS、DNS/TCP/TLS 共享 connect timeout、Happy Eyeballs 与 TCP KeepAlive 继续由 Agent 当前 Snapshot 负责。
- 连接池与 generation fencing：`http.Transport` 按 `TunnelID + ServiceID + RequiredRevision` 隔离，每条 KeepAlive 连接对应一条 ACTIVE WorkConn。同键顺序请求复用，跨 Service/Revision 不复用；Route generation 只允许单调前进，高代原子发布新池并在锁外关闭旧 idle，落后代次使用单请求 Transport，Response Body 到达终态后 exactly-once 清理，旧请求不能回退或污染新池。HTTP idle/max idle 直接使用冻结 Service 参数，连接仍受现有 Pending/Active/WorkConn/FD 硬预算。
- Streaming 与失败语义：Reverse Proxy 使用 100ms FlushInterval；入口固定 10s Header Read 与 60s Request Body sliding idle timeout，每次真实 Body Read 续期，不设置整包总超时。禁用 Chunked 时只接受明确无 Body 或正数可信 Content-Length，`Body != nil && ContentLength == 0` 同样显式拒绝，禁止为推导长度整体缓存。公开错误稳定映射为 `TUNNEL_OFFLINE`、`ORIGIN_REFUSED`、`ORIGIN_TIMEOUT`、`WORK_POOL_EXHAUSTED`、`SERVICE_CONFIG_NOT_OBSERVED`、`SERVICE_DISABLED` 与 Route 404；未知内部失败只返回通用 503，不回显 Origin、Token 或底层错误文本。
- OPEN、取消与资源生命周期：Server OPEN 使用 6s 端到端总预算，Write/Read 只进一步收紧而不能累加成两个窗口。Context deadline callback 在 RAW 提交和清除 Deadline 前执行 exactly-once stop-and-wait，避免旧取消回调把过期 Deadline 写回 ACTIVE 连接；RAW 提交后观察到取消按 `ErrRawCommitted` 关闭，禁止重放。HTTP StopAccepting 先关闭 Handler admission fence，再关闭 Listener；Shutdown Deadline 后主动 Close socket，并等待 Serve、已准入 Handler、ActiveWork、WorkConn 与 Lease 全部收敛。ActiveWork 仅在连接关闭和 Lease 释放完成后从 Runtime 摘除，Drain 不会提前观察到零。
- 回归覆盖：真实 HTTP/1.1 测试覆盖 Host 三段优先级、Raw Path/Query、Chunked 开关、Context Cancel、同键 KeepAlive、跨 Service/Revision 隔离、generation 交错、Response streaming、Request backpressure、滑动 Body idle、Header/Shutdown Deadline、稳定错误映射与敏感信息不泄漏。显式 `XTUNNEL_RUN_LARGE_STREAM_TEST=1` 用常量空间 Reader/Writer 完成单次 1GiB upload + 1GiB download；OPEN 受控连接测试锁定总预算与 clear/cancel/callback Deadline 覆盖交错；Runtime 阻塞 Close 测试锁定 FD/Lease 释放前 Drain 不得返回。
- 验收环境与结果：Windows `go1.27.0`、`GOTOOLCHAIN=local`。`./tools/check-go-version.ps1`、`go test -count=1 -timeout 240s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff` 与 `git diff --check` 全部通过；M4-03 核心包额外完成普通 20 轮与 Race 10 轮。PowerShell 中设置 `$env:XTUNNEL_RUN_LARGE_STREAM_TEST='1'` 后执行 `go test -count=1 -timeout 120s ./internal/server/httpingress -run '^TestHandlerTransfersOneGiBInEachDirection$'` 通过。`CGO_ENABLED=0` 下 linux/amd64、linux/arm64 `go build ./...` 通过；未把交叉构建冒充 Linux 原生生命周期或部署 Smoke。
- 独立复审：首次复审发现 Request Body idle 缺失、Shutdown 未拥有 Handler 归零、Transport generation 回退、ActiveWork 提前 detach、OPEN 时限叠加、稳定错误分类缺失及禁用 Chunked 的零长度手工请求边界；逐项补实现和确定性测试后，又发现 Context deadline callback 可在成功清零后写回过期 Deadline，追加 stop-and-wait fence 与受控交错回归。最终复审执行核心包普通 20 轮、Race 10 轮、Vet 与目标 Diff Check 后结论 `APPROVED`，无剩余 P0/P1/P2/P3。
- 状态与证据边界：当前仍是包含 M4-01、M4-02、Service Proxy 前置契约和 M4-03 的 unstaged/untracked 脏工作区，没有完整 staged snapshot 复验、Commit SHA、push 或对应 CI Run；M0-10 后这些是 `DONE` 强制证据。因此 M4-03 只进入 `REVIEW`，M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`，本次未勾选 M4 Gate。README 已同步实际 HTTP Ingress 能力与后续边界；总技术方案既有 10s/60s/6s 时限、错误语义和 Service Proxy 契约无需改写，V1.0 未来协议规划保持不变。

## 2026-08-28 · M4-03 RequiredRevision 与 Upgrade 复审修复 · REVIEW

- 修复范围：后续未提交变更复审发现两个 P1。HTTP Transport 此前只用 `RequiredRevision` 隔离连接池，却没有把该 Revision 传入 Tunnel Connector 资格门禁；旧 Route 请求可能在 Service 已切换到新 Revision 后新建 WorkConn，造成 Server 仍使用旧 Host/Proxy 参数而 Agent 连接新 Origin。现在 RequiredRevision 已贯穿 HTTP Dial、初选、Pending 等待、提交后复核和跨 Connector 重选，精确比较 Service RequiredRevision；`ObservedRevision` 仍保持大于等于语义，允许其他 Service 推高 Tunnel Revision。Wire OPEN 不新增字段，因此本次修复没有消除 Agent 已应用新 Snapshot 到 Server 收到 ConfigAck 之间的极短跨进程窗口；彻底封闭该窗口需要单独授权 Protocol 变更，本次未修改 Proto。
- Upgrade 生命周期：M4-05 尚未实现，M4-03 Handler 现在同时识别 `Connection` 的大小写无关 `upgrade` token 和非空 `Upgrade` Header，在创建 Transport/ReverseProxy 前返回 `501 UPGRADE_NOT_SUPPORTED`。因此当前阶段不会让 `httputil.ReverseProxy` Hijack 一条脱离 HTTP Server Shutdown/Drain 所有权的连接，也没有提前实现 WebSocket。
- 回归与本地证据：补充 Transport rev1/rev2 传递、Runtime Service 精确 Revision、旧 Route 不消耗新版 Connector IDLE Work，以及 Upgrade 零 Dial 的确定性测试。Windows `go1.27.0`、`GOTOOLCHAIN=local` 下，`./tools/check-go-version.ps1`、定向三包普通测试、定向三包 Race 5 轮、`go test -count=1 -timeout 240s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...` 与 `go mod verify` 通过；未重复执行 1GiB Gate、Linux 原生生命周期/部署 Smoke 或 CI。
- 文档与状态边界：总技术方案和 README 已同步 Route Revision 选择不变量与当前 Upgrade 限制；开发计划 M4-03 验收要点补齐这两项。Proto、OpenAPI、Server Schema、Migration、V1.0 与 `AGENTS.md` 均无需更新，因为 Wire/API/配置/持久化、未来 WebSocket 目标和协作规则没有变化。M4-03 继续保持 `REVIEW`，M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`；当前工作树无 Commit SHA 和 CI Run，本次未勾选任何产品任务或 Gate。

## 2026-08-28 · M4-04 Forwarded/Trusted Proxy 边界 · REVIEW

- 范围与配置权威：生产 HTTP Handler 接入既有 `http_ingress.trusted_proxies`，启动时把 CIDR 编译为只读 Prefix 集合；未新增 Server Schema 字段、默认值、热加载路径、Proto、OpenAPI、Migration、第三方依赖或日志字段。未提前实现 M4-05 WebSocket、M4-08 Caddy/Nginx E2E 或 M4-09 公网限流。
- Peer 与代理链：实际 TCP Peer 不受信时完全不消费外部 Forwarded 元数据，统一从 Peer IP、直连 Scheme 和原始 Host 重建。Peer 受信时只接受单个 `X-Forwarded-For` Header 行，使用有界扫描解析最多 32 个纯 IP，从右向左剥离受信代理并在首个不受信地址停止；更左侧伪造值忽略。Peer、CIDR 与链地址统一处理 IPv4-mapped IPv6，传给 Source Limit 与 `OpenRequest.client_addr` 的是规范化裸 IP。
- Header 清洗与失败语义：可信 `X-Forwarded-Proto` 只接受 `http/https` 单值，`X-Forwarded-Host` 使用严格 HTTP authority 规则并拒绝 bare IPv6。重复 Header、多值、空值、非法 IP/Host/Proto 和第 33 跳在 Tunnel Dial 前返回 `400 INVALID_FORWARDED_HEADER`。Reverse Proxy 转发前删除 `Forwarded`、`X-Real-IP` 和所有 `X-Forwarded-*`，只写入一组权威 `For/Proto/Host`。
- 生产限流接线：独立复审发现 normalized bare IP 最初与 Tunnel Proxy 只接受 `IP:port` 的 Source 解析不兼容，会让启用 LimitManager 的真实 HTTP OPEN 全部失败。修复后 Source parser 同时接受规范化 IP 与直接 TCP 的 `IP:port`，OpenRequest 保留原始输入形态；真实 LimitManager、Connector、OPEN 和计数释放回归已锁定该路径。
- 复审修复：三路只读复审还发现两个问题：完整 `strings.Split` 会在 32 跳拒绝前按超长 Header 放大 Slice，持久化 Host parser 会接受非法 bare IPv6 authority。分别改为 `strings.Cut` 有界扫描和严格 authority Gate，并补 IPv4-mapped IPv6、32/33 跳、真实重复 Wire Header、bracketed/bare IPv6、未知 Header 清洗与零 Dial 失败测试。最终复审结论 `APPROVED`，无剩余 P0/P1/P2/P3。
- 本地验证：Windows `go1.27.0`、`GOTOOLCHAIN=local`。`./tools/check-go-version.ps1`、HTTP Ingress/Tunnel/Bootstrap/Config 定向普通测试、HTTP Ingress Race 10 轮、Tunnel/Bootstrap Race 5 轮、`go test -count=1 -timeout 240s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff` 与 `git diff --check` 均通过。
- 证据与状态边界：当前是基于本地提交 `c7b16e6` 的 unstaged/untracked 工作树，没有包含 M4-04 的 Commit SHA、push、对应 CI Run 或真实 Caddy/Nginx E2E；因此 M4-04 只进入 `REVIEW`，M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`，本次未勾选任何产品任务或 M4 Gate。下一项 M4-05 保持 `NOT_STARTED`，等待用户阶段 Review。
- 文档同步：总技术方案澄清单行 XFF 内最多 32 跳、未受信 Peer 不解析、全受信链/缺失 XFF、Proto/Host 单值和 IPv6 authority 规则；README 同步已实现的可信代理边界。Server Schema 既有字段与默认值保持不变；Proto、OpenAPI、Migration、V1.0、CI/CD、部署文档与 `AGENTS.md` 无需更新。

## 2026-08-28 · M4-05 WebSocket Upgrade · REVIEW

- 范围与协议边界：HTTP Ingress 只把无 Request Body/Transfer-Encoding 的 HTTP/1.1 `GET`、`Connection: upgrade` 与单值 `Upgrade: websocket` 识别为 WebSocket；已知长度超过通用 Body 上限的握手返回 `413 REQUEST_BODY_TOO_LARGE`，其余带 Body 的握手返回 `501 UPGRADE_NOT_SUPPORTED`，两者都在 ACTIVE Lease/Tunnel Dial 前拒绝并关闭客户端连接复用，避免标准库请求体 writeLoop 与 101 后双向复制竞争同一 WorkConn。`Sec-WebSocket-*` 透明交给 Origin/Client 裁决，h2c、HTTP/2 与其他 Upgrade 继续在 Tunnel Dial 前返回 501。未新增 Tunnel Protocol、IngressType、Proto、OpenAPI、Server Schema、Migration、第三方依赖或日志字段。
- Transport 与数据面：每次 Upgrade 创建 fresh、禁用 KeepAlive 的 HTTP/1.1 Transport，沿用同一 Route、RequiredRevision、Host 和 M4-04 Forwarded 权威，不进入普通连接池，也不会在握手字节发出后跨 WorkConn 重试。Origin 响应头使用冻结的 10 秒阶段预算；成功 101 后由标准库 ReverseProxy 逐字节双向复制，Text、Ping/Pong 等帧不在应用层解析。
- Timeout、Half-Close 与生命周期：101 后 Client/Tunnel backend 共用固定 1 小时 sliding idle window，任一方向真实字节进展同时推进两端 Deadline；该窗口由 XTunnel 独立拥有，前置代理不得用相同或更短的方向性 read/write timeout 覆盖它。Caddy 不设置方向性 upstream timeout；标准 Nginx 无法表达共享 idle，部署模板使用其支持上限内的 `24d` ceiling，严格单向连续流超过 24 天时需要反向 heartbeat 或改用 Caddy。该窗口不构成统一总生命周期时限。单边 EOF 保留 TCP Half-Close；Client、Origin/Agent 断连、idle 到期、Context Cancel 或 Hard Deadline 主动关闭两端。Hijack Handler 继续由 request tracker 所有，Graceful Shutdown 等待自然结束，到期后取消 BaseContext 并等待 Handler 归零；`Server.Close` 同样有界强关。
- 回归覆盖：真实 HTTP Server 与原始 TCP 覆盖 Upgrade、双向 Text、Ping/Pong、权威 Forwarded、fresh Dial、握手失败不重试、Client/Origin 断连、真实 TCP Half-Close、共享 idle 双向续期及静默到期、自然 Shutdown、Deadline 强关和活跃连接直接 `Server.Close`；额外覆盖已知长度、未知 chunked 与已知超限三类带 Body 握手的零 Dial、零 ACTIVE 和连接关闭。提供 `XTUNNEL_RUN_WEBSOCKET_SOAK=1` 的显式 `>=1h` soak 入口，每分钟以 Ping 验证持续进展；本轮未运行该长时用例，不能把默认 skip 记作通过。
- 本地验证：Windows `go1.27.0`、`GOTOOLCHAIN=local`。最终 HTTP Ingress 普通测试 5 轮与 Race 5 轮通过；本轮带 Body 握手回归普通 50 轮、Race 10 轮及包级普通/Race/Vet 通过。`./tools/check-go-version.ps1`、`go test -count=1 -timeout 240s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff`、`git diff --check` 与 `git diff --cached --check` 通过。`CGO_ENABLED=0` 下 linux/amd64、linux/arm64 `go build ./...` 通过，未把交叉构建冒充 Linux 原生 Runtime Smoke。
- 独立复审：三路只读复审先发现 Hijack Shutdown 所有权、旧 generation 101 Body 能力丢失、握手跨 WorkConn 重试、ACTIVE idle 缺失、代理层过度裁决 `Sec-WebSocket-*`、Deadline 并发回退和单向续期证据不足；逐项改为 request owner 排空、fresh Transport、透明 Sec Header、1 小时共享 sliding idle 与单 applier/version 重放，并补确定性交错和真实连接回归。后续 mixed Index 复审又发现带 Body 的 Upgrade 可让请求 writer 与 WebSocket copier 同时写 WorkConn；本轮已在 Dial 前拒绝，并补已知长度 `Expect: 100-continue`、未知 chunked Body 与已知超限 Body 的错误优先级回归。
- 证据与状态边界：当前是基于本地提交 `c7b16e6` 的混合 staged/unstaged 工作树；`websocket.go` 与 `websocket_test.go` 同时存在 staged/unstaged 差异，提交前必须重新暂存最终版本并从 staged snapshot 复验。本轮没有 Commit SHA、push、对应 CI Run、真实 Caddy/Nginx E2E 或已完成的 1 小时 soak，因此 M4-05 只进入 `REVIEW`，M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`，未勾选任何产品任务或 M4 Gate。M4-06 保持 `NOT_STARTED`，等待用户阶段 Review。

## 2026-08-28 · M4-06 TCP Listener Manager · REVIEW

- 范围与端口模型：`tcp_ingress.min_port..max_port` 冻结为包含两端的逻辑预留池，不在启动时预监听整个范围。TCP Route 创建既可显式选择池内端口，也可省略并由 Server 确定性选择；Normalize 后 SQLite 始终持久化非零具体端口，禁用 Route 继续占用逻辑端口，删除才释放。未新增 Server Schema 字段、Migration、Proto、OpenAPI、第三方依赖、日志字段或本地第二配置源，也未提前实现 M4-07 Raw TCP/SSH 转发。
- 事务与 fencing：可预测的范围、保留、占用和池耗尽错误在事务前拒绝，`BEGIN IMMEDIATE` 内再以最新 Desired State 重验；成功写入在同一事务推进 Service Version/RequiredRevision、所属 Tunnel DesiredRevision 与全局 Route Generation，Gate 失败或并发冲突整笔回滚且不发 dirty。全局 Generation 只用于 Server Reconcile fencing，不作为客户端 ExpectedGeneration；不同 Tunnel 并发自动分配仍取得不同端口和连续代次。
- Listener Reconcile：唯一 owner 只对当前完整 Snapshot 中启用 Route 的具体端口执行 `Listen`。Route Snapshot 原子发布后通过非阻塞 observer 唤醒 TCP owner，并保留 5 秒周期重试；单端口绑定或 Accept 异常只发布稳定 `LISTEN_FAILED`，其他 Listener 继续。A→B 先成功监听并发布 B 再释放 A，B 失败时 A 保留且同步当前非端口 Route 字段；同端口变更复用 Socket 并在 admission fence 内原子更新准入值。连接登记前以当前 Route Snapshot generation 读取作为准入线性化点，删除、禁用或更新后的旧 Listener 即使尚未 reconcile 也不再接纳新连接，旧 generation 候选不会污染新状态。
- 生命周期与资源：Listener、Accept goroutine 与连接 Handler 均有明确 owner、停止条件和 Wait 路径；Stop 先建立准入 fence，再关闭 Listener 解阻塞 Accept，持续 Listener Close 失败时保留 residual ownership 并用 TCP Deadline 收敛阻塞，Shutdown Deadline 后取消 Handler Context、关闭残留连接。普通客户端连接的 Close 错误只由 Manager Close 聚合，不调用进程 `ReportRuntimeError`；Handler panic 仍为 fatal，并与同次 Close 错误合并。M4-07 前 Handler 保持 fail closed。启动顺序冻结为 Route Snapshot → TCP Listener Restore → HTTP Ingress → Gateway → Runtime Reconciler；首次 Admin 前即使 SQLite 已有 TCP Route 也不占用端口，完整关闭 SQLite/Bootstrap 后会从持久化 Route 恢复监听。
- FD 预算：启动 Gate 按逻辑池全部 `50001` 个端口加一个原子换口候选计算 TCP 峰值，固定 Listener 合计为 `50006`，默认 Server FD 总预算由 `87188` 提升到 `137192`；这只是容量上界，不改变按需绑定语义。单元测试直接锁定 `50002 / 50006 / 137192`，README 同步当前能力、启动/停机顺序和 OCI/systemd `nofile` 要求。
- 本地验证：Windows `go1.27.0`、`GOTOOLCHAIN=local`。`tcpport/tcpingress` 普通测试 20 轮，`application/sqlite/route/bootstrap` 普通测试 10 轮，Bootstrap 普通 5 轮与 Race 3 轮，TCP Ingress Race 100 轮通过；此前 Handler panic 两项回归普通 50 轮、Race 10 轮及包级普通/Race/Vet 通过。本次 generation 准入、连接 Close 非 fatal 与 panic 合并回归在 Race 下 20 轮通过；随后执行 `go test -count=1 -timeout 300s ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff`、GoFmt、`git diff --check` 与 `git diff --cached --check` 全部通过。Linux amd64/arm64 `go build ./...`、Bootstrap test compile 与历史全仓 Race 属于此前证据；本次没有重跑 Linux Runtime 或全仓 Race，不能将定向 Race 冒充完整 Gate。
- 复审与状态边界：三路只读复审先后发现 admission fence、候选取消泄漏、Accept 异常假 ACTIVE、持续 Close 阻塞、Route publish dirty 接线、Bootstrap/FD 证据及测试 happens-before 问题；修复并高轮复验后通过。后续 mixed Index 复审又发现 Handler panic 会跳过连接 Close、却先释放 Manager ownership；本轮已把 Close 注册为先于 ownership 删除执行的 defer，并让 panic 与同时发生的 Close 错误合并为一次 Runtime Error。最终未提交变更复审继续发现 Snapshot 发布到 TCP reconcile 之间旧 Listener 仍可准入，以及普通连接 Close 错误误入 Fatal Runtime Channel；现已增加当前 generation 准入线性化点，并把连接级清理错误与 owner/panic fatal 分离，补充直接回归。当前仍是 mixed staged/unstaged 工作树，没有包含 M4-06 最终状态的 Commit SHA、push、对应 CI Run 或隔离 Linux 主机部署/Listener 生命周期证据，因此 M4-06 只进入 `REVIEW`，M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`，未勾选任何产品任务或 M4 Gate。提交前必须重新暂存最终版本并从 staged snapshot 复验。
- 文档同步：总技术方案同步逻辑池、具体端口持久化、事务 fencing、按需监听、`LISTEN_FAILED`、原子换口、准入与关闭不变量；README 同步用户可见行为和 FD 预算；开发计划同步 M4-06 验收、当前队列与本记录。Server Schema、Migration、Proto、OpenAPI、V1.0、CI/CD、部署文件与 `AGENTS.md` 均无需更新。

## 2026-08-28 · M4-07 Raw TCP/SSH Data Plane · REVIEW

- 范围与生产接线：新增 Tunnel owner 值传递 `DialRequest`，HTTP `TunnelDialer.Dial` 与 TCP `tcpTunnelProxy.Serve` 均使用同一路由输入，不复用 Wire `OpenRequest` 也不新建泛化 common package。M4-06 TCP Listener 在准入时捕获不可变 `TCPRoute`，生产 Handler 把 TunnelID、ServiceID、精确 RequiredRevision、`INGRESS_TYPE_TCP` 和真实 Public Peer 交给 Tunnel Proxy；Serve 忽略调用方 ClientAddr，只使用已 Accept Peer 的真实地址。未新增 Proto、OpenAPI、Server Schema、Migration、第三方依赖或本地业务配置源。
- 超时与 Origin 选项：Public TCP Pre-OPEN 的 10 秒唯一 owner 下沉到 Tunnel Proxy，只约束 Work Acquire 与 OPEN；OPEN_OK 后立即取消 Timer，ACTIVE 使用原 Listener Manager Context，因此 RAW 长连接不会在 10 秒后被误杀。HTTP Dial 则继续在 OPEN 后脱离单请求取消，由 Transport 池与 ActiveWork 收敛。Agent 既有 Origin Resolver 继续让 DNS/IPv4/IPv6/TCP/可选 TLS 共用 Service `connect_timeout`，disable Happy Eyeballs 不扩展该预算，TCP KeepAlive 默认 30 秒且 `0` 映射为禁用。
- RAW、Half-Close 与关闭：OPEN_OK 后统一双向代理不解析 SSH、TLS、数据库或其他业务协议，不注入 PROXY Header，只逐字节转发。单边 EOF 只 `CloseWrite` 对端，反方向继续；取消或致命错误对两端设立 Deadline 并 Close，等待两个 copy goroutine 退出。Manager 保持公网连接 owner/Wait，Handler 返回后再保底 Close；StopAccepting 不杀死在途连接，Shutdown Deadline 才强制收敛。
- 错误语义：TCP 失败不写入任何带内错误文本，只关闭公网连接。Server 优先保留 Agent Proto 稳定码，本地容量/Acquire 失败映射 `WORK_POOL_EXHAUSTED`，未观察 Revision 映射 `SERVICE_CONFIG_NOT_OBSERVED`，已观察但无 Connector 映射 `TUNNEL_OFFLINE`，协议错误映射 `PROTOCOL_ERROR`，其余统一 `INTERNAL_ERROR`。Warn 日志只包含 `error_code`、Tunnel/Service 和 Public Port，不记录 Origin、Token 或底层错误文本；正常进程排空取消不记为单连接失败，普通连接错误不进入进程 Fatal Runtime Channel。
- 回归与本地证据：新增真实 `*net.TCPConn` 双连接回归，锁定精确 Revision（含合法 Revision 0）、真实 Peer Address 覆盖伪造 ClientAddr、Pre-OPEN 超时释放、OPEN_OK 后跨过该窗口 RAW 仍存活，以及 Revoke 对 Public Peer/WorkConn/ActiveWork/Lease/配额的有界收敛。Revoke 回归刻意让 WorkConn Close 返回 EOF，并同时断言 Serve 保留 `context.Canceled` 与 EOF，防止正常 Half-Close 掩盖外部取消。SSH identification 加任意二进制字节继续按字节相等，稳定码表和敏感文本不泄露均有断言。Windows `go1.27.0`、`GOTOOLCHAIN=local` 下，新增 Serve 生命周期回归 20 轮、定向 Race、`go test ./...`、`go test -race ./...`、`go vet ./...`、`go mod verify`、`go mod tidy -diff` 和 `git diff --check` 通过；`CGO_ENABLED=0` 的 linux/amd64、linux/arm64 `go build ./...` 通过。
- 复审、状态与证据边界：前轮 P1 已按用户确认收敛。Tunnel Proxy 的 Serve 在 OPEN 成功后把真实 Public Peer 注册到 ActiveWork，RAW 代理只关闭底层 WorkConn，随后快照 lifecycle 取消原因再通过同一 Finish 收敛；因此正常双 EOF 不会误报取消，Revoke/Drain 也不会遗留公网半连接。只读契约复审与高轮/Race 验证后无剩余 P0/P1，M4-07 进入 `REVIEW`；M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`，本轮未勾选任何 M4 Gate。当前仍是 mixed staged/unstaged 工作树，没有包含本轮最终状态的 Commit SHA、CI Run 或隔离 Linux 部署证据，因此不得标记 `DONE`。
- 文档同步：README 已更新 Raw TCP/SSH 生产行为与 `REVIEW` 边界；总技术方案补充 `DialRequest`、双 Context、Public Peer 注册与 exactly-once Finish 契约。Proto、OpenAPI、Server Schema、Migration、V1.0、CI/CD、部署文档与 `AGENTS.md` 均无需更新。

## 2026-08-28 · M4-08 Caddy/Nginx HTTPS/WSS 集成 · REVIEW

- 部署边界：新增 `deploy/reverse-proxy/Caddyfile`、`nginx.conf.template` 与同目录 README。前置代理监听公网 HTTPS/WSS，证书和私钥只由部署环境挂载；代理到同主机 XTunnel HTTP Ingress 的 upstream 固定为 loopback 明文 HTTP/1.1。未修改既有 Compose、Server Schema、Proto、OpenAPI、Migration、数据库、权限模型、日志契约或第三方 Go 依赖，也未提前实现 M4-09 公网限额。
- Header、流式与 WebSocket：Caddy/Nginx 都保留完整 Client Host authority，Origin 不改写，覆盖客户端伪造的 `X-Forwarded-For/Proto/Host` 后交给 XTunnel 可信 loopback 边界再次规范化。公网 Header Read 统一为 `10s`。Nginx 使用 `$http_host` 保留显式端口，显式传递 Upgrade/Connection 并关闭请求/响应 buffering，使用 `large_client_header_buffers 4 1m` 避免默认单 Header 8 KiB 上限先于 Server Schema 拒绝；`client_max_body_size 0` 不在 Server Schema 前独立裁决 Body 大小。Caddy 固定 HTTP/1.1 upstream、原生 Upgrade 与 `100ms` 有限刷新间隔，保留客户端断开向 upstream 的取消传播且不设置方向性读写 timeout。Nginx 标准 HTTP Proxy 无法精确表达共享 idle，模板使用其支持上限内的 `24d` 方向性 ceiling；严格单向连续流超过 24 天时需要反向 heartbeat 或改用 Caddy。
- 自动化与供应链：新增显式 `XTUNNEL_RUN_FRONT_PROXY_E2E=1` Gate，测试从仓库同一部署配置启动真实 host-network Caddy/Nginx；临时 CA 与叶证书只写入测试目录，私钥权限 `0600`，客户端使用 RootCAs 与 ServerName 真校验。HTTPS/WSS 分别断言 Host/path/query、单值 Origin 逐字透明、恶意 Forwarded 覆盖、精确 RequiredRevision、真实 `101` 与双向帧；客户端断开场景断言未结束的 upstream 连接在有界时间内关闭。默认运行的静态契约测试同时锁定 `10s` Header Read、Caddy 非负 `100ms` 刷新且无方向性 timeout、Nginx `4 × 1 MiB` 大 Header、`24d` ceiling 与禁用独立 Body 限制。Caddy `2.11.4-alpine` 和 Nginx `1.30.4-alpine` 固定官方多架构 index digest；测试只允许 `--pull=never`，CI 在原生 amd64/arm64 Runner 显式拉取同一摘要并核对镜像架构。
- 本地验证：Windows `go1.27.0`、`GOTOOLCHAIN=local` 下，工具链检查、HTTP Ingress 普通与 Race、全量普通与 Race、`go vet ./...`、`go mod verify`、`go mod tidy -diff`、`git diff --check` 与 `git diff --cached --check` 曾通过。使用同一 Go 工具链交叉编译 linux/amd64 测试二进制后，在 WSL Ubuntu 22.04 的原生 linux/amd64 Docker daemon 中预拉固定摘要，Caddy/Nginx 真实 HTTPS/WSS E2E 连续 3 轮通过；该结果不冒充 arm64 Runtime 或 GitHub CI 证据。本次 Header/timeout/buffer 策略修复重新通过 HTTP Ingress 包普通/Race、静态策略 Race 20 轮、全仓普通测试、Vet、Module Verify/Tidy Diff 与双 Diff Check；当前环境无 Docker、Caddy 或 Nginx，未执行新配置的真实语法与 E2E，也未重跑全仓 Race，不能沿用历史 Runtime 结果作为本次通过证据。
- 独立复审：首次只读复审发现 Origin 未进入请求/断言的 P1，以及 Docker 清理忽略错误和部分命令缺少有界 Context 的 P2。修复后 HTTPS/WSS 使用不同 Origin 值精确断言单值透明传递；image inspect、stop、force remove 与删除确认均使用独立有界 Context，清理失败显式报告。后续未提交变更复审先发现 Caddy 负刷新会阻断客户端取消传播、Nginx 固定 2 GiB 会覆盖 Server Body 契约，最终又发现双方 `1h` 方向性 timeout 会提前终止仍有单向流量的 WebSocket、公网 Header 默认 60 秒，以及 Nginx 默认 8 KiB 单 Header 缓冲小于 Schema 最大值；现已逐项修复并补静态策略回归，仍待 Docker/CI 复验与最终独立复审。
- 状态与证据边界：当前仍是基于 `c7b16e6` 的 mixed staged/unstaged 工作树，没有包含 M4-08 最终状态的 Commit SHA、push 或对应 CI Run；原生 arm64 E2E 仅已接入 CI，本次新增客户端断开场景也尚未取得 Docker Runtime 证据。因此 M4-08 只保持 `REVIEW`，M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`，本轮不勾选 M4 Gate Checklist。下一项仍为 M4-10 Gate。
- 文档同步：README 已链接可执行部署示例并同步 M4-08 `REVIEW` 边界；总技术方案的 Caddy/Nginx 概念配置补充公网监听、loopback upstream、完整 authority 与单源 Forwarded 规则；开发计划同步任务状态、执行队列、验收证据与剩余 Gate。部署 README 不维护第二套镜像版本权威，固定摘要以测试与 CI 为准。

## 2026-08-28 · M4-09 Public Ingress Limits · REVIEW

- 授权与契约：用户明确确认新增 Server 配置字段并允许修改内部导出签名。`configs/server.schema.json` 新增唯一机器权威 `limits.max_http_body_bytes`，范围 1 byte 至 1 TiB、默认 2 GiB；Go 配置使用 `int64`，Schema/Struct 契约测试同步支持该整数类型。Nginx 前置模板使用 `client_max_body_size 0`，不再以固定 2 GiB 覆盖合法的更大 Server 配置；`large_client_header_buffers 4 1m` 允许单 Header 达到既有 `max_http_header_bytes` Schema 最大值，聚合裁决仍由 Go Server 执行。`httpingress.NewHandler` 改为单一 `HandlerOptions`，TCP Listener Options 增加来源 limiter；未修改 Proto、OpenAPI、Migration、第三方依赖、锁文件、日志字段、暂存区或提交历史。
- 有界来源状态：共享 `LimitManager` 接入既有 open rate、open burst 与 HTTP RPS 配置，实现最多 32 分片的 Token Bucket。每类状态总容量由 `max_active_connections + max_pending_opens` 派生，每个分片在容量压力下淘汰最久未访问来源；OPEN TTL 为完整 Burst 回填所需的向上取整秒数且至少 1 秒，HTTP Burst 等于一秒 RPS、TTL 为 1 秒，Server 重启清空状态。Token 一经入口消费，不因路由竞争、Work 获取或下游 OPEN 失败退还。
- HTTP 真实入口：可信代理归一化成功后逐请求消费 HTTP Token；只有 `http.Transport` 确实新建 Tunnel WorkConn 时才消费 OPEN Token，KeepAlive 复用不重复扣减。独立 `ActiveLease` 按在途 Request 或完整 WebSocket Handler 生命周期计入 Global/Tunnel/Service/Source IP 四级配额，池化 WorkConn 不再继承首个请求的来源归属。已知 Body 超限在 Tunnel Dial 前返回 `413 REQUEST_BODY_TOO_LARGE`，未知长度继续流式读取并在超限后返回同码、关闭客户端复用；速率超限返回 `429 RATE_LIMITED` 与 `Retry-After: 1`。Go HTTP Server 的 `MaxHeaderBytes` 在 Handler 前返回标准 431。
- TCP 与 Lease 生命周期：Raw TCP 在 OS `Accept` 后、准入登记、WaitGroup 和 Handler goroutine 创建前按真实 Peer IP 消费一次 OPEN Token；解析失败或限流拒绝只关闭 Socket，不进入 Tunnel，也不写带内错误。Tunnel Proxy 对 TCP 不重复扣减，继续使用原 `PendingOpen -> ACTIVE -> connection close` Lease；HTTP OPEN_OK 只结束 Pending，ACTIVE 由 Handler 独立持有。四级 ACTIVE 提交与释放复用同一 Manager 锁，独立 ActiveLease 并发重复释放只归还一次。
- 回归与复审：新增 Token 补充、TTL、LRU 淘汰、分片容量、非法 Options、四级 ACTIVE 与并发 exactly-once 测试；HTTP 覆盖可信代理来源 Rate、已知/流式 Body 413、同来源 ACTIVE 拒绝、释放后跨来源复用同一 KeepAlive、OPEN Rate 429 和真实 Socket Header 431；TCP/Tunnel 覆盖非法/拒绝/允许 Accept、TCP 不双扣、HTTP 下游失败仍扣 Token、HTTP WorkConn 不持有公网 ACTIVE。后续复审发现 Nginx 固定 2 GiB Body 和默认 8 KiB 单 Header 都会在 Server Schema 之前拒绝合法输入；本轮关闭独立 Body 裁决并把单 Header 缓冲提升到 1 MiB，用默认运行的静态配置测试锁定边界。
- 本地验证：Windows `go1.27.0`、`GOTOOLCHAIN=local`。定向配置与十个相关包普通测试通过；HTTP Ingress 普通测试 10 轮、Race 3 轮通过；此前全仓普通/Race、Vet、Module Verify 与双 Diff Check 通过。本次 Nginx Header buffer 修复重新通过 HTTP Ingress 包普通/Race、静态策略 Race 20 轮、全仓普通测试、Vet、Module Verify/Tidy Diff、GoFmt 与双 Diff Check；未重跑全仓 Race 或真实 Nginx Runtime。
- 证据边界：当前仍是 M4 多阶段 mixed staged/unstaged 工作树，没有包含 M4-09 最终状态的 Commit SHA、push、对应 CI Run 或高基数来源长时间 LRU/TTL 压测。M4-10 已补完整 Server 启动装配后的公网 HTTP Rate/Body/Header 与 TCP Accept/Open/Active 黑盒证据，且修正后的前置代理配置已通过 Linux amd64 Docker Runtime；M4-09 仍只保持 `REVIEW`，M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`，不勾选任何 M4 Gate Checklist。
- 文档同步：总技术方案第 156 节同步机器默认值镜像、Token Bucket 容量/TTL、HTTP/TCP 扣减点、ACTIVE 所有权与公开 413/429/431 行为；README 同步已实现的用户可见限额和 `REVIEW` 边界；开发计划同步任务表、当前队列与本记录。Proto、OpenAPI、Migration、CI/CD 和 `AGENTS.md` 无需更新；部署配置按既有 Schema Header/Body 上限同步，不产生第二套机器权威。

## 2026-08-28 · M4-10 Product Data Plane Gate · REVIEW

- 完整生产装配：新增 Linux M4 Gate，从真实公网 HTTP/TCP Socket 进入 `openGatewayAndBootstrapWith` 启动的 Route Snapshot、HTTP/TCP Listener、Gateway、Session、Tunnel Proxy，再使用正式 Connection Token 启动真实 Agent Connector 并拨通 HTTP/TCP Origin。冷启动 Desired State 只在 Runtime 启动前通过 Repository 与类型化 GORM fixture 写入；启动后不绕过 Listener、Handler、OPEN、WorkConn 或 Agent Origin Resolver。
- HTTP 与 WebSocket：真实 HTTP 链路断言 Host/Path/Query/Body、Origin Host、Forwarded For/Proto，并用 Origin barrier 稳定验证同来源 `429 RATE_LIMITED`；已知 Body 超限、encoded separator 与真实 Socket Header 431 均在 Origin 前拒绝。WebSocket 使用真实公网 Upgrade、Gateway/Agent/Origin `101` 握手与 RFC 6455 masked text frame 回显，不再只依赖测试 Dialer。
- TCP 与限额：SSH identification、包含零字节的通用 Raw TCP、逐字节相等与公网 Half-Close 均通过真实 Agent。Accept Rate 使用同源连接先完成握手再做集合断言；ACTIVE 使用不同来源并证明 Agent Origin 已拨通、但 ACTIVE 提交失败前没有公网负载进入 Origin；Pending OPEN 使用独立完整 Server/Agent Runtime 和单 Work 硬预算稳定占满唯一 Pending 槽位，不依赖 Token 回填或固定睡眠。所有公网连接具有 10 秒总 Deadline，失败路径拒绝带内字节并排空 Origin 队列。
- HTTPS/WSS 组合 Gate：固定摘要 Caddy/Nginx 测试验证真实公网 TLS、HTTPS/WSS、Host/Origin/Forwarded 与客户端断开收敛；本任务的生产 WebSocket 测试验证同一 HTTP Ingress/TunnelDialer 边界之后的 Gateway→Agent→Origin。两者构成边界重叠的组合证据，不声称已存在单条 WSS→Agent 连续测试链。Nginx `proxy_read_timeout`/`proxy_send_timeout` 从超过实现上限、会导致固定镜像拒绝启动的 `1y` 修正为 `24d`，并同步部署 README、总技术方案、根 README 与静态策略测试；严格单向超过 24 天仍需反向 heartbeat 或使用 Caddy。
- CI 接线：原生 Linux amd64/arm64 `verify` Job 新增 M4 Product Gate 与显式 1 GiB 双向 Streaming Gate；定向 Race 扩展到 Route、HTTP/TCP Ingress、Tunnel 和 Integration。Caddy/Nginx Gate 继续显式拉取固定多架构摘要并核对当前 Runner 架构。没有修改 Proto、OpenAPI、Server Schema、Migration、第三方依赖、锁文件、日志字段或权限模型。
- 本地验证：Windows `go1.27.0`、`GOTOOLCHAIN=local` 下，全仓 `go test -count=1 -timeout 300s ./...`、`go vet ./...` 与扩展定向 Race 通过；显式 1 GiB Upload/Download Gate 通过。Linux amd64 Go 1.27 容器中 Product Gate 连续 5 轮和定向 Race 通过，全仓 Test/Vet 通过。使用交叉编译的 linux/amd64 HTTP Ingress 测试二进制在 WSL 原生 Docker daemon 中运行固定摘要 Caddy/Nginx，修正后的真实 HTTPS/WSS E2E 通过。外部提交发生后又按最终内容复跑 Linux Product Gate、定向 Race、全仓 Test/Vet，Windows 全仓 Test/Vet、扩展 Race、1 GiB Gate，以及固定摘要 Caddy/Nginx Runtime E2E，均通过；`git diff --check` 与 staged diff check 通过。
- 独立复审：只读复审先后发现 TCP Accept/Open/Active 证据不完整、来源 Token 墙钟竞态、拒绝路径未断言零字节、成功连接缺少 Deadline、Origin 失败清理、生产 WebSocket 缺口与 Nginx 文档漂移；现已分别使用独立 Runtime/资源预算、集合断言、真实 masked frame 和 owner 顺序修复。最终复审确认代码无剩余阻塞发现，并限定 HTTPS/WSS 为边界重叠的组合 Gate。
- 提交与 CI：工作期间由外部操作将最终代码、文档与此前用户暂存的 `task-continuity`/`skills-lock.json` 一并提交并推送为 `834a9de`；本 Agent 未执行该 Commit 或 push。[CI #19](https://github.com/lifei6671/xtunnel/actions/runs/33168998225) 于 2026-08-28 通过，耗时 3 分 46 秒，原生 Linux amd64/arm64 `verify` 与 Windows Agent Service Job 均成功，覆盖本任务新增的 Product Gate、1 GiB Gate、扩展 Race、固定摘要 Caddy/Nginx Runtime E2E、全仓 Test/Vet、生成物与工作树清洁检查。
- 状态与证据边界：M4-10 与 M4-01 至 M4-09 继续保持 `REVIEW`。Commit、push、原生 Linux amd64/arm64 和对应 GitHub CI 证据均已齐备，但尚未获得用户阶段 Review 结论，因此 M4 `DONE` 仍为 `0/10`、全局仍为 `33/95`；本次未勾选任何产品任务或 M4 Gate Checklist，也不解锁 M5。当前未提交内容仅为 CI 通过后的 README 与开发计划证据同步，后续提交前需重新暂存并复验这些文档差异。
- 文档同步：总技术方案、根 README 与部署 README 已同步 Nginx `24d` 支持边界和组合 Gate 事实；本次继续同步根 README 的提交/CI 状态，以及开发计划的当前阶段、队列和 Commit/CI 证据。Proto、OpenAPI、Server Schema、Migration、V1.0、依赖/锁文件和 `AGENTS.md` 无需更新，因为本轮没有改变这些权威契约。

## 2026-08-28 · M2 CI 证据闭环 · DONE

- Review 与提交：M2-01 至 M2-08 已在 2026-08-26 完成用户阶段 Review；实现提交为 `4447602984b3d58d0e35a8ba3a5c07d2226bdb62`。本轮通过 `git merge-base --is-ancestor 4447602 HEAD` 取得退出码 0，确认该提交完整包含在当前 `834a9de3838c5743c6b02864148cf6646c24b0a0` 中，不再是未推送的孤立本地证据。
- CI 证据：[CI #19](https://github.com/lifei6671/xtunnel/actions/runs/33168998225) 精确绑定 `834a9de` 并成功完成原生 Linux amd64/arm64 `verify` 与 Windows Agent Service Job；当前 Workflow 执行全仓 Test/Vet、Tunnel/Integration 等扩展 Race、双进程 Build、OCI Smoke 和工作树清洁检查，覆盖 M2 的最终代码状态。
- 状态影响：M2-01 至 M2-08 从 `REVIEW` 转为 `DONE`，M2 仪表盘改为 `8/8 DONE`，全局由 `33/95` 更新为 `41/95`。M2 Gate Checklist 早已在用户 Review 时逐项通过，本轮不改变其验收语义。
- 文档边界：本轮只回写开发计划的任务状态、仪表盘、当前队列和证据记录；M2 行为、Proto、Schema、OpenAPI、数据库、依赖、日志与部署契约均未变化。

## 2026-08-28 · M4 阶段 Review 通过 · REVIEW

- 用户结论：在明确提示“下一步需要确认是否通过 M4 阶段 Review”后，用户回复“继续”，本记录将其作为 M4 阶段 Review 通过。M4 专属的实现、失败分支、独立复审、Checklist、提交 `834a9de` 与 CI #19 证据均已齐备。
- Checklist：M4 Gate 八项均已有真实 Socket、固定摘要 Caddy/Nginx、1 GiB Streaming、HTTP/HTTPS/WebSocket/SSH/Raw TCP、Route/Listener generation fencing 与真实入口限额证据，因此本轮逐项勾选。
- 依赖边界：M4 的里程碑入口依赖明确为 `M2-08 + M3-13`。M2-08 已在本轮转为 `DONE`，但 M3-13 仍缺隔离 Linux amd64/arm64 的 systemd 原生安装/启动 Smoke，继续保持 `REVIEW`。因此 M4-01 至 M4-10 也继续保持 `REVIEW`，不得仅凭 M4 自身证据绕过依赖标记 `DONE`，M5 不解锁。
- 下一步：用户已确认只在 GitHub 临时原生 Linux amd64/arm64 Runner 中运行 systemd Smoke；Workflow 接线已完成。取得精确绑定本次 Workflow Commit 的双架构 CI Run 前，M3-13、M4 和 M5 状态保持不变。
- 文档同步：根 README 同步 M4 Review 已通过及 M3-13 依赖边界；开发计划同步 M2 `DONE`、M4 Checklist、仪表盘、当前队列与执行记录。总技术方案、Proto、OpenAPI、Server Schema、Migration、依赖/锁文件、部署文件和 `AGENTS.md` 无需更新，因为本轮只确认既有证据和任务状态，没有改变任何产品或机器契约。

## 2026-08-28 · M3-13 systemd Packaging Smoke CI 接线 · REVIEW

- 授权与范围：用户明确确认修改 CI，只允许在 GitHub 临时原生 Linux amd64/arm64 Runner 上执行破坏性的 systemd Smoke；本轮不在当前 WSL 或持久主机创建 Unit、服务身份、Binary、Credential、配置、运行目录或数据目录。
- CI 接线：Linux `verify` Matrix 在原生 OCI Smoke 后、最终工作树清洁检查前新增 5 分钟上限的 `Run native systemd packaging smoke` Step。每个 Runner 在 `$RUNNER_TEMP` 原生构建 Server/Agent Binary，再以 `sudo deploy/systemd/smoke.sh` 执行既有 root/systemd/路径/身份预检、安装、Agent 重装换 PID、enable/restart/stop/start、权限与 `LoadCredential` 校验、受管卸载和退出清理；不使用交叉编译 Binary，不把产物写入仓库。
- 本地非破坏性验证：`go1.27.0` 与 `GOTOOLCHAIN=local` 检查通过；PyYAML 解析和 CI Step Contract 断言通过；`sh -n`、`dash -n`、`bash --posix -n`、ShellCheck 通过；Linux amd64/arm64 Server/Agent 的 `CGO_ENABLED=0` 交叉构建通过；三个 systemd 脚本 Git Mode 均为 `100755`；双 Diff Check 通过。交叉构建和语法检查只作为接线反馈，不冒充原生 systemd Runtime。
- 独立复审：两路只读复审均确认 Step 位置、原生 Matrix、显式 Binary 路径、`sudo`、冲突预检、退出清理、5 分钟上限与最终 clean check 正确，没有 P0/P1/P2 阻塞项。Runner 镜像未来若失去 root/systemd 前提，既有预检会显式失败，不会静默跳过。
- 状态边界：当前 Workflow 改动尚未提交、推送，也没有精确绑定其 Commit SHA 的 GitHub Actions Run；因此 M3-01 至 M3-13 继续 `REVIEW`，M3 Checklist 不勾选，M4-01 至 M4-10 继续 `REVIEW`，M5 不解锁。本次 CI 接线未新增 `DONE` 产品任务。
- 文档同步：开发计划同步当前结论、队列和本执行记录。README、总技术方案、Proto、OpenAPI、Server Schema、Migration、依赖/锁文件、systemd/OCI 行为契约和 `AGENTS.md` 无需更新，因为本轮只扩展验证矩阵，没有改变用户命令或运行行为。

## 2026-08-28 · M3/M4 Gate 证据闭环 · DONE

- 用户结论：在已明确说明“提交并推送 CI 接线，双架构 CI 全绿后逐条关闭 M3 Checklist”的执行边界后，用户回复“可以，继续吧”，并明确指出 M3 Gate Checklist 尚未标记完成；本记录据此完成 M3 Gate Checklist 与阶段状态闭环。
- 提交与 CI：CI 接线提交为 `a50709af801afc2520ee34014449fa6f1414c9f7`。[CI #20](https://github.com/lifei6671/xtunnel/actions/runs/33170682836) 精确绑定该 SHA，`verify (amd64)`、`verify (arm64)` 与 Windows Agent Service 均成功；两个 Linux Job 的 `Run native systemd packaging smoke` 和后续 `Verify generated files remain clean` Step 全部为 `success`。
- M3 Checklist：八项产品语义分别由既有 Application/Integration、Snapshot Deterministic/Size、Config Runtime Atomic Apply、Token-only Reconnect、Health Scheduler/Batch/Fencing、Health Budget/Replacement 与 DurableOps Crash/Filesystem 自动化证据证明；systemd Smoke 只补最后的原生部署边界，不替代这些语义测试。M3 实现提交 `07a3d06`、`81e99d7`、`5c3c4a1`、`a31548c` 与 Gate 修复 `a3213e4` 均是 `a50709a` 的祖先，并由当前 CI 覆盖。
- 状态影响：M3-01 至 M3-13 从 `REVIEW` 转为 `DONE`，八项 M3 Gate Checklist 全部勾选。M4 已有提交 `834a9de`、CI #19、八项 Checklist、独立复审和用户阶段 Review，入口依赖 M3-13 闭环后，M4-01 至 M4-10 同步从 `REVIEW` 转为 `DONE`。全局由 `41/95` 更新为 `64/95`，M5-01 依赖满足并进入 `READY`。
- 独立复审：只读 Gate 审计逐项映射 M3-01 至 M3-12 的实现、失败分支、Commit 和自动化证据，确认新 CI 成功后可关闭 M3；同时确认 M4 只需等待 M3 入口依赖，不存在新的 P0/P1/P2 阻塞项。
- 文档同步：根 README 同步 M2/M3/M4 完成状态、DurableOps 与 M4 依赖闭环；开发计划同步当前阶段、仪表盘、M3/M4 任务、M3 Checklist、队列与执行证据。总技术方案、Proto、OpenAPI、Server Schema、Migration、依赖/锁文件、部署行为契约和 `AGENTS.md` 不更新，因为本轮没有改变产品、Wire、REST、配置、持久化或运行行为。

## 2026-08-28 · M5-01 OpenAPI Contract Freeze · DONE

- 授权与范围：用户明确确认首次冻结公共 REST 契约，并同时授权不可变 OpenAPI Baseline、Breaking Wrapper、真实负例和 CI 接线。本轮未新增第三方依赖或修改 Lockfile，也未实现 Handler、Generated Contract 或 Web。
- 机器契约：`api/openapi/openapi.yaml` 从 M0 空骨架扩展为 19 个 Path、25 个 Operation，冻结 Session Cookie/CSRF、Tunnel/Connector/Credential、Service/Nested Exposure、Dashboard/System、安全配置白名单、Security Audit GET-only Query、RFC3339 UTC、opaque Cursor、JSON Merge Patch、Tunnel/Service 强 ETag、Typed Error Details、Secret no-store 和全部状态码。Service ETag 绑定 Service/Tunnel 双版本；Token Reveal/Rotate/Revoke 与 Tunnel Revoke 明确分离。
- Breaking Gate：新增与首版完整契约字节一致的 `api/openapi/openapi.v0.1.baseline.yaml`。`tools/openapi.sh breaking` 固定通过 Vacuum `--original` 与 `--error-on-breaking` 比较独立 Baseline，禁止当前文件自比较；`tools/test-openapi.sh` 使用删除 Operation 的隔离负例证明真实 `breaking-change`，并覆盖 Baseline 缺失失败；CI 在 Validate 后执行 Breaking。
- 本地验证：WSL Linux 下受管 Vacuum `0.30.0` 执行 `./tools/openapi.sh validate`、`./tools/openapi.sh breaking`、`./tools/test-openapi.sh` 全部通过；53 条启用规则、0 Error、Quality 100/100。主契约与 Baseline 字节一致且 SHA-256 均为 `3F44D6C66F4CEE1276C661DF523EF8F5F6BC8B235DBEE4380B788174B4616354`；Shell 三种语法、CI/Spec/Baseline/Ruleset YAML、19 Path/25 Operation、Audit GET-only、双 Diff Check 均通过。
- 实现对齐风险：OpenAPI 以既有 Security Audit `adm_<ULID>` 机器契约为准，M5-03 需修正当前 Admin UUID 生成；M5-05 需把单 Nested Exposure 与 Service Create/PATCH/Delete 纳入同一事务并补齐 DB/Route 不变量；M5-07 需新增 Audit 只读 Query Repository/Application Owner。上述是后续 Handler 达到契约一致性的前置实现项，不以修改 OpenAPI 迁就当前缺口。
- 独立复审：最终只读复审独立复跑 Validate、Breaking 和真实负例，核对主契约与 Baseline 的字节一致性及文档证据，结论为无 P0/P1/P2 阻塞。
- CI 证据：契约实现提交 `ed3e5bf4332bbc275d926580de87c38fbebd4132`；推送与证据同步提交 `8aac03edce94dd076f4b0b256c81412e2b261e91` 精确触发 [CI #22](https://github.com/lifei6671/xtunnel/actions/runs/33178322621)，总耗时 5 分 9 秒，Windows Agent Service 与 Linux amd64/arm64 Verify Matrix 全部通过。GitHub Action 运行时报告 Node 20 弃用 Warning，不是任务失败，不影响本任务结论。
- 状态边界：M5-01 的产物、关键断言、Breaking 失败分支、独立复审、本地验收与精确 CI 证据已齐备，因此从 `REVIEW` 转为 `DONE`；M5 为 `1/11`、全局为 `65/95`，M5-02 解锁为 `READY`。M5 Gate Checklist 全部保持未勾选，不把 OpenAPI 单任务验收冒充完整 M5 Gate。
- 文档同步：总技术方案已同步 Cookie/CSRF Wire、Exposure、Service Composite ETag、Token Revoke、只读 Audit Query、安全 Config 白名单与 Typed Error；开发计划同步当前阶段、M5 状态、任务行、队列和本记录；README 同步 19 Path/25 Operation、Breaking 命令与 `DONE` 证据边界。Proto、Server Schema、Migration、依赖/Lockfile、运行日志契约和 `AGENTS.md` 均未改变。

## 2026-08-29 · M5-02 Generated Client/Server Contract · REVIEW

- 授权与选型：用户明确确认 Generator 选型、精确版本和依赖/Lockfile 变更。Go 侧锁定 `oapi-codegen v2.8.0`、`oapi-codegen/runtime v1.6.0` 与 `nullable v1.1.0`，生成 Models、标准库 HTTP Server 和 Strict Server Contract；Web 侧锁定 `openapi-typescript 7.13.0` 与 `openapi-fetch 0.17.0`。`openapi-typescript` 的工具 Peer Range 不包含 Web TypeScript `6.0.2`，因此没有使用 `--force`/`--legacy-peer-deps`，而是在 `tools/openapi-ts` 以独立 Lockfile 锁定工具侧 TypeScript `5.9.3`。
- 生成产物：唯一机器权威仍是 `api/openapi/openapi.yaml`。Go 生成物提交到 `internal/server/managementapi/contract.gen.go`，覆盖 25 个 Strict Operation、Request/Response Model、状态码、Header 和 Media Type；TypeScript Schema 提交到 `web/src/api/schema.gen.ts`，开启 immutable 与 read/write markers。`web/src/api/client.ts` 只使用 `openapi-fetch` 装配 `/api/v1` 和 `credentials: same-origin`，不维护第二套 DTO。生成物由 `tools/openapi.sh generate|generate-check` 统一管理，禁止手改。
- 契约澄清：七组 OpenAPI 联合类型补齐显式 discriminator mapping，使生成类型使用既有 Wire 值 `http`/`https`/`tcp`、`TCP`/`HTTP` 和现有 Typed Error code；该变更不改变 Request/Response Wire Shape，也不改写不可变初始 Baseline。Vacuum Breaking 报告为纯 Additive Mapping，没有 Breaking Change。
- 工具与 CI：`tools/bootstrap-openapi.sh` 从 `tools/go.mod` 只读构建受管 `oapi-codegen`，不会回落开发机 PATH；同时支持原生 Linux Go 和 WSL 调用锁定的 Windows Go，并通过 `WSLENV` 真实透传 `GOTOOLCHAIN=local`。CI 在 OpenAPI Step 前按两个 Lockfile 执行 `npm ci`，随后执行 Validate、Breaking、`generate-check` 与 Contract Test。`tools/test-openapi.sh` 真实篡改 Go、TypeScript 生成物并删除 Go 生成物，逐项证明漂移/缺失会失败，退出时恢复仓库文件；另断言 25 Operation、Nullable PATCH、Strict Server、Merge Patch、ETag、Cache-Control、Discriminator、writeOnly 和 Client Base/Credential 语义。
- 本地验证：Windows `go1.27.0`、`GOTOOLCHAIN=local`，Node `24.19.0`、npm `11.17.0`。受管 Generator 空缓存隔离重建成功；Shell `sh`/`dash`/`bash --posix` 语法通过；`openapi.sh validate|breaking|generate-check` 与 `test-openapi.sh` 通过，Vacuum Quality `100/100`；两个 `npm ci` 均为 0 Vulnerability，Web `check`/`build` 通过；根/工具 Module Verify 与 Tidy 零漂移、工具 Generator 只读 Build、全仓 `go test -count=1 -timeout 300s ./...` 和 `go vet ./...` 通过；CI YAML 与两个 Package/Lock JSON 解析、双 Diff Check 通过。npm 首次读取本机 Cache 时报告部分 Tarball 损坏，但按完整性校验自动重新获取后安装成功且 0 Vulnerability；这不是 Lockfile 漂移或被忽略的失败。当前没有原生 Linux 双架构或 GitHub CI 证据。
- 独立复审：复审独立重跑 Validate、Breaking、Generate Check、Contract Test、Module Verify、生成 Package Test/Vet、Web Check、npm Audit、三种 Shell 语法和 Diff Check。首次发现 Archive Checksum 负例只断言退出码，未设置 `GOTOOLCHAIN` 时可能因错误原因伪通过；修复后 `test-openapi.sh` 先强制 `GOTOOLCHAIN=local`，并分别精确匹配 Vacuum SHA-256 与 Generator Build 失败文本，反向实测缺失环境变量明确失败。最终结论为无 P0/P1/P2 阻塞，可进入 `REVIEW`。
- 剩余契约风险：当前冻结 OpenAPI 的 `int64` 由 TypeScript 忠实生成成 `number`，而 Schema 尚未统一限制为 `Number.MAX_SAFE_INTEGER`。并发控制使用字符串 ETag，不受该问题影响，且这不是本任务静默修改 Wire 的理由；在 M5-07/M5-08 实际输出或消费相关字段前，必须单独发起公共 Contract Review，选择安全上限或字符串 Wire。Strict Server 也不等于完整运行时 Schema 校验，CSRF、ETag 业务规则和 `additionalProperties` 等仍由 M5-03 至 M5-10 实现。
- 状态与 Gate 边界：当前产物、本地验证与独立复审已完成，但工作区尚无包含本任务最终状态的 Commit SHA，也没有精确绑定该提交的 GitHub CI Run。因此 M5-02 进入 `REVIEW` 而非 `DONE`，M5 仍为 `1/11`、全局仍为 `65/95`，依赖 M5-02 的后续任务不解锁。M5 Gate Checklist 六项全部保持未勾选；第一项是 M5-11 的完整退出门禁，不能仅凭 M5-02 本地 Generated Drift 通过提前勾选。
- 文档同步：总技术方案同步 Generator/Runtime/生成路径、Nullable/Strict/read-write 语义和 TS 工具隔离；根 README 同步唯一命令入口与 `REVIEW` 边界；根 `AGENTS.md` 同步生成物所有权和禁止手改规则；根 `.gitignore` 排除工具侧 `node_modules`；开发计划同步当前阶段、任务状态、审批决定、队列和本记录。Proto、Server Schema、Migration、部署、生产配置、权限模型和日志契约未改变。

## 2026-08-29 · M5-02 CI 证据闭环 · DONE

- 提交与首次 CI：生成器、Go/TypeScript Contract、OpenAPI discriminator mapping、依赖、Wrapper、真实漂移负例、CI 接线和文档实现提交为 `b3fed9902a42563624ee13d3466361f5eb969bbe`。精确绑定该 SHA 的 [CI #24](https://github.com/lifei6671/xtunnel/actions/runs/33220481060) 中 Windows Agent Service 与 Linux amd64 成功，Linux arm64 在 OpenAPI Contract Test 失败；本轮没有把部分成功或旧 CI 冒充完整证据。
- arm64 失败与修复：失败日志显示 Archive Checksum 负例只把 `VACUUM_LINUX_AMD64_ASSET` 指向损坏 Fixture，arm64 仍请求原正式文件名，因而进入 `vacuum download failed` 而非预期 `SHA-256 mismatch`。最小修复同时覆盖 arm64 Asset，提交为 `1fe7f016987ec7fe52226561357a80ee8b9b6b24`；独立复审在隔离目录模拟 `x86_64` 与 `aarch64`，两者均稳定命中预期校验和失败并保留旧 Vacuum Binary。Windows 本地三种 Shell 语法、`test-openapi.sh` 与 `generate-check` 复跑通过。
- 最终 CI：[CI #25](https://github.com/lifei6671/xtunnel/actions/runs/33220824958) 精确绑定 `1fe7f016987ec7fe52226561357a80ee8b9b6b24`，从 2026-08-29 07:33:30 至 07:38:53（Asia/Shanghai）运行约 5 分 23 秒。Windows Agent Service、Linux amd64 与 Linux arm64 全部成功；两个 Linux Job 均通过 OpenAPI Validate/Breaking/双端 Generate Check/真实负例、Web Check/Build、全仓 Test/Vet、定向 Race、M4 Product/1 GiB/Caddy/Nginx、OCI/systemd Smoke 和最终工作树清洁检查。
- 状态影响：M5-02 的真实产物、关键断言、失败分支、独立复审、Commit SHA 与精确 CI 证据全部齐备，因此从 `REVIEW` 转为 `DONE`。M5 从 `1/11` 更新为 `2/11`，全局从 `65/95` 更新为 `66/95`；M5-03、M5-04、M5-05、M5-07 转为 `READY`，M5-08 继续等待 M5-04/M5-05。TypeScript `int64` 安全整数边界仍须在 M5-07/M5-08 前单独进行公共 Contract Review。
- Gate 边界：M5 Gate Checklist 六项继续全部未勾选。CI 已证明第一项所需的工具链路径，但 M5-11 是 M5-01 至 M5-10 完成后的完整退出 Gate；当前尚无真实 Handler 响应、PATCH/分页、Login/CSRF 或 Web 日常流程证据，不能提前勾选单项或将 M5 标记完成。
- 文档同步：根 README 同步 M5-02 `DONE`、提交与 CI 证据；开发计划同步当前阶段、任务状态、仪表盘、队列和本记录。总技术方案、OpenAPI、生成物、Proto、Server Schema、Migration、依赖/Lockfile、CI、部署、权限与日志契约本轮不再更新，因为证据闭环没有改变已提交的产品或机器行为。

## 2026-08-29 · M5-03 Admin Login/Session/CSRF · DONE

- 授权与边界：用户明确确认新增 v9 Migration 和 M5-03 实现。固定采用标准库 HTTP 与既有生成 Strict Server Contract，不引入 Gin、Playwright 或其他第三方依赖，不修改 OpenAPI、Proto、Server Schema、CI/CD、生产配置、权限模型或 Lockfile；真实反代 HTTPS Browser E2E 保留给 M5-10。
- 持久化与身份：v9 在同一事务内把规范的历史 UUID Admin ID 迁移为 `adm_<ULID>`，损坏 ID、DDL 失败和版本写入失败均整体回滚；新增 `ads_<ULID>` Admin Session，Cookie 原始 Token 只保存 SHA-256，独立 32-byte CSRF Token 用于 `/auth/me` 恢复。绝对 TTL 12 小时、空闲 TTL 30 分钟、最长每分钟触碰一次；成功口令校验后创建 Session 前有界清理最多 128 条过期记录。
- Handler 与运行时：Login、Logout、`/auth/me` 由生成 Strict Handler/Router 接管，未知 Auth 路径与 Method 保持类型化 404/405，其他尚未实现 API 由边界返回稳定 500 而不落入 SPA。Cookie 固定 Secure、HttpOnly、SameSite=Lax、Host-only、Path `/api/v1`；Login 严格校验单值 JSON Content-Type、64 KiB Body、Origin、Host 和可信代理 Client IP，非 Loopback 明文 fail closed。失败限流固定为 `(Client IP, Username)` 每分钟 5 次、全局每分钟 100 次、冷却 `1/2/4/8/15` 分钟、4096 项 LRU、30 分钟无活动回收；Argon2 最多 4 个非阻塞并发槽位，饱和时立即 `429`。Management 使用 10/30/30/90 秒 Header/Read/Write/Idle Timeout，启动早于 Admin 检查；`SETUP_REQUIRED` 只保留 Management，Shutdown 关闭准入、排空或强关连接并等待 Handler 归零后才允许关闭 SQLite。
- Web 与验证：Web 提供会话探测、登录、`SETUP_REQUIRED` 初始化引导、认证错误/限流反馈和带 Origin/CSRF 的退出，CSRF 只驻留内存。Go 1.27.0 且 `GOTOOLCHAIN=local` 下，`go test -count=1 -timeout 300s ./...`、`go test -race -count=1 -timeout 300s ./...`、`go vet ./...`、`go build ./cmd/server`、`git diff --check` 均通过；`npm --prefix web run check` 与 `npm --prefix web run build` 通过，Vite 构建 1806 Modules。真实 SQLite + TLS HTTP Server 黑盒覆盖 `SETUP_REQUIRED`、Origin、密码、Cookie、`/auth/me`、CSRF 失败、Logout、生成路由 404/405、重复 Header/Cookie 和 Argon2 并发饱和。
- 独立复审：数据库、集成和安全三路复审发现并推动修复 Touch 到期竞态测试、远端明文准入、Handler Drain、生成 Strict Router 接线、Argon2 并发上限、HTTP Timeout 与重复 Content-Type；最终三路均确认无剩余 P0/P1/P2。残余验证边界只有真实 Caddy/Nginx HTTPS Browser E2E 与长时 FD/慢读压力，分别由 M5-10 与 M7 继续闭环。
- 提交、CI、复审、状态与 Gate：最终 29 个文件的 staged snapshot 在未暂存/未跟踪文件均为 0 时完成复验，M5-03 实现提交为 `3cbdb337c2db302e5eb8c8f18bb7f1c669efa86d`，证据文档提交后远端 Head 为 `354682ee83ced2169fe10353fc878f92820cedc1`。精确绑定该 Head 的 [CI #27](https://github.com/lifei6671/xtunnel/actions/runs/33225874899) 从 2026-08-29 09:14:43 至 09:20:05（Asia/Shanghai）运行约 5 分 22 秒；Windows Agent Service、Linux amd64 与 Linux arm64 全部成功，两路 Linux 均通过 Web Check/Build、全仓 Go Test/Vet/Build、现行定向 Race、既有 M4/OCI/systemd Gate 与最终工作树清洁检查。用户阶段复审通过，因此 M5-03 转为 `DONE`，M5 `DONE` 更新为 `3/11`、全局更新为 `67/95`，并解锁 M5-04 进入 `IN_PROGRESS`。M5 Gate Checklist 六项全部保持未勾选；尤其 Login/Secure Cookie/CSRF/Logout 的 Go TLS 黑盒是 M5-03 任务证据，不能替代 M5-10 的完整 Browser E2E 和 M5-11 Gate。
- 文档同步：总技术方案此前已同步 v9 身份迁移、Admin Session/CSRF 数据血缘、限流常量和 Management 生命周期；本次仅由根 README 与开发计划同步 M5-03 的精确 CI 证据和阶段 Review 边界。OpenAPI、生成物、Proto、Server Schema、依赖/Lockfile、CI/CD、部署配置、日志契约和 `AGENTS.md` 均无需更新，因为本次没有改变产品或机器行为。

## 2026-08-29 · M5-04 Tunnel/Connector/Credential API · DONE

- 授权与范围：用户以“继续”明确确认 M5-04 所需的跨包导出符号、Repository 接口和 Application Service 变更。本轮只实现冻结 OpenAPI 已有的 10 个 Tunnel/Connector/Credential Operation；不新增 REST Path、依赖、Migration、配置字段或 Lockfile，不实现 M5-05 Service API、M5-06 opaque Pagination/完整 PATCH 并发矩阵、M5-08/M5-09 Web 页面。
- 聚合与持久化：新增 `identity.NewTunnelID`，Tunnel Repository 增加 `COUNT(*)`、名称 CAS 与删除 CAS。`TunnelManagementService.Create` 在事务外生成、编码和加密首代 Credential，再在同一个 SQLite 写事务内以 `COUNT(*)` 检查 `max_tunnels`、创建 Tunnel 与 Token v1，Token 插入失败会整体回滚；`Delete` 在同一 `BEGIN IMMEDIATE` 内完成版本校验、Service Count/有界引用列表与删除，不级联 Service/Route。持久提交后使用删除专用 Runtime owner，以 per-Tunnel AUTH admission/startup 临时栅栏等待全部 generation 与 ActiveWork 收敛；成功后摘除 Runtime 与栅栏，失败则保留 fail-closed 栅栏并返回收敛错误，不复用永久 Revoke 墓碑。
- 运行态投影：Tunnel Get/List 分别读取 Current Session 与含 ActiveWork Tombstone 的两类值型快照；每类快照各自经过 generation fence，跨方法只承诺并发 replacement/revoke 下的短暂最终一致，不声称原子联合快照。状态只调用 `server/status`，`connectors_online` 只计 ONLINE，`active_connections` 保留断开后仍活跃的 Tombstone Work，`last_seen_at` 取最新心跳。Connector API 只返回目标 Tunnel 当前且心跳新鲜的真实运行态项，按 Connector ID 排序，不持久化 Connector，也不伪造 OFFLINE。
- HTTP 与安全：生成 Strict Router 驱动 Tunnel CRUD、Tunnel Revoke、Reveal/Rotate/Revoke Token 和 Connector List；统一 Middleware 每请求只认证一次 Admin Session，所有 Mutation 统一执行同源 Origin 与 CSRF，Reveal GET 按冻结契约只要求 Session。Tunnel 强 ETag 只接受单个引号包围的正整数；缺失、非法和 stale 分别返回 428、400 和 412。Create/Reveal/Rotate 返回同一纯函数生成的四类部署命令，且固定 `Cache-Control: no-store`、`Pragma: no-cache`；Token Revoke 只返回 Metadata，不含 Token 或命令。
- 自动化证据：真实 SQLite + TLS HTTP Server 黑盒覆盖 Create/Reveal 同值、Rename、Rotate、Token Revoke 无 Secret、Reveal 不可用、Tunnel Revoke、Delete、Service 引用 409 详情、Current Connector 投影、Session/Origin/CSRF、428/400/412、严格 JSON、Secret 缓存头，以及 Runtime 收敛失败不得掩盖持久提交；Application 负例证明 Token 插入失败时 Tunnel 同事务回滚并覆盖 `max_tunnels`。Runtime/Session 测试覆盖 admission 在持久化 Verify 前建立、认证成功后跨 Handler 返回转交 startup、Delete 不可穿越该交接、失败保留栅栏，以及 256 个不同 Tunnel 连续删除不增长 Runtime/栅栏 Map。Go `go1.27.0` 且 `GOTOOLCHAIN=local` 下，`go test -count=1 -timeout 300s ./...`、`go test -race -count=1 -timeout 300s ./internal/application ./internal/repository/... ./internal/server/runtime ./internal/server/controlauth ./internal/server/sessionruntime ./internal/server/managementapi ./internal/server/bootstrap ./tests/integration`、`go vet ./...`、`go build ./cmd/server` 与 `git diff --check` 均通过。全仓测试首次在受限沙箱中只因 Windows 临时目录父路径拒绝访问而失败，同一命令获准在沙箱外重跑后全绿；未把该环境失败写成产品失败或静默忽略。
- 独立复审：契约与并发所有权两路只读复审先后发现事务配额线性化、提交后 Runtime 错误映射、AUTH 准入建立时点、Handler 到 startup 租约交接，以及 startup 被栅栏拒绝时过早释放 admission 的问题；全部完成生产修复和直接回归。最终两路复审均确认无剩余 P0/P1/P2/P3，交接阻塞 Close 用例另在 Race 下连续 20 轮通过。
- 提交、CI、复审、状态与 Gate：24 个 M5-04 代码、测试和文档文件组成实现提交 `2a357544ccceb25b02eb9c410762b73b46747e81`，14 个无关未跟踪 Skill 目录未暂存、未提交。精确绑定该提交的 [CI #28](https://github.com/lifei6671/xtunnel/actions/runs/33233709963) 从 2026-08-29 12:23:37 至 12:29:03（Asia/Shanghai）运行 5 分 26 秒；Windows Agent Service、Linux amd64 与 Linux arm64 全部成功，两路 Linux 均通过现行全仓 Test/Vet/Build、定向 Race、OpenAPI/Web 和既有 Product/OCI/systemd Gate。用户阶段复审通过，因此 M5-04 转为 `DONE`，M5 更新为 `4/11`、全局更新为 `68/95`。M5-06 仍负责 Tunnel/Service 完整 428/412、PATCH omitted/null/value、opaque token 默认 50/最大 200 与并发原子递增；M5 Gate Checklist 六项全部保持未勾选。
- 文档同步：根 README 与开发计划同步 M5-04 的实现提交、精确 CI、用户复审、`DONE` 状态、仪表盘和后续队列；总技术方案此前已经同步 Delete 专用 AUTH/startup 临时栅栏契约，本次证据闭环不再改变产品行为。OpenAPI、Proto、Server Schema、Migration、生成 Contract、依赖/Lockfile、CI/CD、部署配置、日志契约和 `AGENTS.md` 均无需修改。

## 2026-08-29 · M5-05 Service API · REVIEW

- 授权与范围：用户明确确认 M5-05 所需的内部导出 Application 类型/接口、Route Repository 扩展、Handler 接线与 v10 Migration。本轮只实现冻结 OpenAPI 已有的 7 个 Service Operation；不修改 OpenAPI、Proto、Server Schema、依赖/Lockfile、CI/CD、生产配置或权限模型，也不提前实现 M5-06 的完整 omitted/null/value、428/412 与 opaque Pagination 矩阵。
- 聚合与持久化：新增复合 `ServiceAPIService`，复用既有 per-Tunnel 写 owner，在同一 `BEGIN IMMEDIATE` 中原子提交 Service、唯一 Nested Exposure、Tunnel Desired Revision 与 Route Generation；Health Budget 先 Reserve、提交后 Commit，Runtime dirty 只在释放写锁后通知。v10 以单表唯一索引和跨 HTTP/TCP 双向 INSERT/UPDATE Trigger 保证一个 Service 最多一个 Exposure；同表或跨表历史重复会使 Migration 整体回滚。Repository 新增 HTTP CRUD 与一次一致性 `GetExposureByService`，Service Delete 先删 Exposure，并在 Exposure 已移除时仍推进 Route Generation，避免完整 Route Snapshot 保留旧 Tunnel/Service 状态。
- Runtime 与投影：生产 Bootstrap 将 Session Snapshot、Route Manager、Connection Limits、TCP Apply Failure 和冻结 TCP Port Policy 注入复合 owner。List/Get 从一次 Route Desired State 读取中组合持久化 Service/Exposure，并只消费 generation-fenced Runtime 值型快照；状态统一调用 `server/status`，`active_connections` 来自 Limit owner，M6 Usage 未就绪时固定返回 `UNAVAILABLE` 与三个 null 计数器。Service Snapshot 字段变化与 Exposure 变化都会在同事务推进新 Route Generation；在线 Manager 回归证明 disable 移除、enable 恢复、Origin/RequiredRevision 刷新均收敛，name-only/no-op 不误推进配置代次。
- HTTP 与安全：生成 Strict Router 驱动 List/Create/Get/PATCH/Delete/Enable/Disable；统一 Session/Origin/CSRF Middleware，Create 使用父 Tunnel ETag，后续 Mutation 使用绑定 Service/Tunnel 双版本的强 composite ETag。响应使用冻结 Origin/Health/Exposure 联合类型，不公开内部 Route ID；TCP 端口冲突、HTTP Route 冲突、Snapshot/Health Budget 失败映射到冻结错误码。请求预检对 Create 联合类型与 PATCH 四个嵌套对象执行 `DisallowUnknownFields` 和 EOF 校验，真实 TLS 黑盒覆盖 8 个 unknown-field `400 INVALID_REQUEST`；Application 按有效 Origin scheme 拒绝不适用的 HTTP/TLS 字段。
- 自动化证据：Go `go1.27.0` 且 `GOTOOLCHAIN=local` 下，`go test -count=1 -timeout 360s ./...`、`go test -race -count=1 -timeout 360s ./...`、`go vet ./...`、`go build ./cmd/server ./cmd/agent`、`go mod verify`、`go mod tidy -diff` 与 `git diff --check` 均通过；`npm --prefix web run check`、`npm --prefix web run build` 通过。Repository/Migration 测试覆盖 v9→v10 重复数据 fail-closed、事务回滚、只读保护和 CRUD；Application 覆盖原子版本、端口分配、Exposure 切型/移除、无 Exposure 删除、通知失败持久事实和运行态投影；真实 SQLite + TLS 黑盒覆盖完整 Service 生命周期、认证、同源/CSRF、ETag、响应默认值和 unknown field。
- 提交与 CI：实现提交为 `0841eb4ac9115e1e19cfc5b0934be3a4aec49ac9`，证据文档提交后远端 Head 为 `5d6e6f96fbb8c48448e58d7516a8d70b6ad276dd`。精确绑定该 Head 的 [CI #30](https://github.com/lifei6671/xtunnel/actions/runs/33245404948) 从 2026-08-29 17:24:06 至 17:29:11（Asia/Shanghai）运行 5 分 05 秒；Windows Agent Service、Linux amd64 与 Linux arm64 三个 Job 全部成功。
- 独立复审：存储、并发所有权与冻结契约三路只读复审发现并推动修复在线 Service 字段更新未推进 Route Generation、Create/PATCH 嵌套 unknown field 被静默接受，以及 Origin Patch 不适用字段被清零的问题；最终三路复核均确认原问题关闭，无剩余 P0/P1/P2/P3。M5-06 负责的完整并发 PATCH、omitted/null/value 与 opaque Pagination 仍保持后续边界。
- 状态与 Gate：当前实现、失败分支、本地验证、三路独立复审、实现 Commit 与精确 CI 证据均已齐备；但尚未取得用户阶段复审结论，因此 M5-05 继续保持 `REVIEW`，不得提前标记 `DONE`。M5 保持 `4/11`、全局保持 `68/95`，M5-06/M5-08 仍未解锁，M5 Gate Checklist 六项全部保持未勾选。本次未勾选任何产品任务。
- 文档同步：总技术方案同步 one-Exposure 持久化约束、Service Snapshot 与 Route Generation 的收敛语义；根 README 同步已实现但尚待 CI 的 Service API 能力；开发计划同步 M5-05 `REVIEW`、当前队列和本执行记录。OpenAPI、生成 Contract、Proto、Server Schema、依赖/Lockfile、CI/CD、部署配置、日志契约与 `AGENTS.md` 无需更新，因为实现遵守既有机器契约且没有改变这些边界。

## 2026-08-29 · M5-05 CI 与用户复审闭环 · DONE

- 证据闭环：M5-05 实现提交 `0841eb4ac9115e1e19cfc5b0934be3a4aec49ac9`、三路独立复审和精确 [CI #30](https://github.com/lifei6671/xtunnel/actions/runs/33245404948) 已齐备；CI 精确绑定远端 Head `5d6e6f96fbb8c48448e58d7516a8d70b6ad276dd`，Windows Agent Service、Linux amd64 与 Linux arm64 三个 Job 全部成功。用户随后明确给出阶段复审结论“通过”。
- 状态影响：M5-05 从 `REVIEW` 转为 `DONE`，M5 从 `4/11` 更新为 `5/11`，全局从 `68/95` 更新为 `69/95`；M5-06 与 M5-08 的依赖闭环并转为 `READY`，M5-07 继续保持 `READY`。M5 Gate Checklist 六项全部保持未勾选，不把单个 Service API 任务的验收冒充完整 M5 Gate。
- 文档同步：根 README 同步 M5-05 已通过精确 CI 与用户复审；开发计划同步当前阶段、仪表盘、M5-05/M5-06/M5-08 状态、当前队列和本记录。总技术方案、OpenAPI、生成 Contract、Proto、Server Schema、Migration、依赖/Lockfile、CI/CD、部署配置、日志契约与 `AGENTS.md` 均无需更新，因为本轮只闭环既有实现证据，没有改变产品或机器契约。

## 2026-08-29 · M5-06 PATCH/ETag/Pagination 并发契约 · REVIEW

- 授权与范围：用户明确确认 M5-06 所需的内部跨包导出投影方法。本轮只实现冻结 OpenAPI 已定义的 Pagination、Merge PATCH、ETag 与并发 CAS 语义；不修改 OpenAPI、Proto、Server Schema、Migration、第三方依赖/Lockfile、CI/CD、生产配置、权限或日志契约。
- Pagination 与安全：Tunnel、Connector 和 Service List 统一使用进程级 32-byte 随机密钥签名的 HMAC-SHA256 Cursor，绑定 Resource Type、`id_asc` 排序、最后 ID 与 Filter Hash；Service/Connector 额外绑定父 Tunnel。默认 50、最大 200、终页省略 `next_page_token`；篡改、跨资源、跨过滤器、跨进程或空 Token 均稳定返回 `400 INVALID_PAGE_TOKEN`，随机源失败时 Handler 启动直接失败。
- PATCH、ETag 与提交响应：Tunnel/Service 所有冻结 Mutation 完整覆盖缺失 `If-Match` 的 428 和 stale ETag 的 412，PATCH 只接受 `application/merge-patch+json`。Service 强 opaque ETag 绑定 Service ID、Tunnel ID 与双版本，按 HTTP `etagc` 语法验证后与服务端当前值常量时间比较。Create/PATCH/Enable/Disable 成功响应通过 `ProjectMutation` 只使用本次已提交的持久字段投影 Body/Status/ETag，不会混入后续并发版本。
- 矩阵与原子性证据：真实 SQLite + TLS HTTP Server 表验覆盖全 Mutation 428/412、两条 PATCH 错误媒体类型、Origin 7 字段、Proxy 5 字段、Health 8 字段与 Exposure 5 字段的逐字段 `null → 422`，以及三类 Cursor 终页 Wire 省略；Handler 转换表验逐字段证明 25 个 value 均映射到正确 Application 字段且 sibling 保持 omitted。同 ETag 并发 PATCH 精确证明 Tunnel/Service 各一次成功与一次 412；Repository 并发测试证明只有一个 CAS 赢家，Service Version、Tunnel Desired Revision 与 Route Generation 各只递增一次。
- 验证与复审：Windows `go1.27.0` 且 `GOTOOLCHAIN=local` 下，高重复并发测试 `count=20`、`go test -count=1 -timeout 360s ./...`、`go test -race -count=1 -timeout 360s ./...`、`go vet ./...`、`go build ./cmd/server ./cmd/agent`、`go mod verify`、`go mod tidy -diff`、`npm --prefix web run check`、`npm --prefix web run build` 和 `git diff --check` 全部通过。并发、契约与安全三路独立复审最终均为 `APPROVED`，无剩余 P0/P1/P2/P3。
- 提交、状态与 Gate：11 个 M5-06 代码/测试文件的实现提交为 `e5407f29f7fb906487d2e9a00628be3d58205928`；14 个无关未跟踪 Skill 目录未被暂存或提交。当前没有精确绑定该提交的 GitHub CI Run，也没有用户阶段复审结论，因此 M5-06 从 `READY` 进入 `REVIEW` 而非 `DONE`。M5 保持 `5/11`、全局保持 `69/95`，M5 Gate Checklist 六项全部保持未勾选；完整 API Contract Suite、真实 Browser E2E 与 M5 总 Gate 仍分别属于 M5-10/M5-11。本次未勾选任何产品任务。
- 文档同步：根 README 同步 M5-06 已完成本地实现并进入 `REVIEW`；开发计划同步当前阶段、任务状态、队列与本执行证据。总技术方案、OpenAPI、生成 Contract、Proto、Server Schema、Migration、依赖/Lockfile、CI/CD、部署配置、权限、日志契约与 `AGENTS.md` 无需更新：实现遵守现有唯一机器权威与已冻结产品语义，未新增外部行为或第二套默认值。

## 2026-08-29 · M5-06 精确 CI 证据 · REVIEW

- CI 证据：[CI #33248860032](https://github.com/lifei6671/xtunnel/actions/runs/33248860032) 精确绑定远程 Head `8146d254510bc1161f836d17992462a6f8ed2781`，从 2026-08-29 18:53:47 至 18:59:21（Asia/Shanghai）运行 5 分 34 秒。`verify (amd64)`、`verify (arm64)` 与 `Windows Agent service` 三个 Job 全部成功；两路 Linux 均通过现行 OpenAPI/Web、全仓 Go Test/Vet/Build、定向 Race、M4 Product/OCI/systemd Gate 与最终工作树清洁检查，Windows Job 通过 Agent Service 安装、升级、卸载与 arm64 交叉构建。
- 状态与 Gate：M5-06 已具备实现 Commit、本地验收、失败分支、三路独立复审和精确 CI 证据，但尚未取得用户阶段复审结论，因此继续保持 `REVIEW` 而非 `DONE`。M5 保持 `5/11`、全局保持 `69/95`，M5 Gate Checklist 六项全部保持未勾选；本次未勾选任何产品任务。
- 文档同步：根 README 与开发计划同步 M5-06 精确 CI 事实与当前 Review 边界。总技术方案、OpenAPI、生成 Contract、Proto、Server Schema、Migration、依赖/Lockfile、CI/CD、部署配置、权限、日志契约与 `AGENTS.md` 无需更新：本轮只闭环已实现行为的远程验证证据，没有改变产品、机器契约或运行方式。

## 2026-08-29 · M5-06 CI 与用户复审闭环 · DONE

- 证据闭环：M5-06 实现提交 `e5407f29f7fb906487d2e9a00628be3d58205928`、三路独立复审和精确 [CI #33248860032](https://github.com/lifei6671/xtunnel/actions/runs/33248860032) 已齐备；CI 精确绑定远端 Head `8146d254510bc1161f836d17992462a6f8ed2781`，Windows Agent Service、Linux amd64 与 Linux arm64 三个 Job 全部成功。用户随后明确给出阶段复审结论“通过”。
- 状态影响：M5-06 从 `REVIEW` 转为 `DONE`，M5 从 `5/11` 更新为 `6/11`，全局从 `69/95` 更新为 `70/95`；M5-07 与 M5-08 继续保持 `READY`，M5-09 仍等待二者完成。M5 Gate Checklist 六项全部保持未勾选，不把单个并发契约任务的验收冒充完整 M5 Gate。
- 文档同步：根 README 同步 M5-06 已通过精确 CI 与用户复审；开发计划同步当前阶段、任务状态、仪表盘、队列和本记录。总技术方案、OpenAPI、生成 Contract、Proto、Server Schema、Migration、依赖/Lockfile、CI/CD、部署配置、权限、日志契约与 `AGENTS.md` 均无需更新，因为本轮只闭环既有实现证据，没有改变产品或机器契约。

## 2026-08-29 · M5-07 Settings/Runtime/Audit API 与 M5-08 Dashboard/Status UI · REVIEW

- 授权与范围：用户确认继续 M5-07/M5-08，并单独确认 Build Version 所需构建系统、OCI 与 CI 调整。本轮实现冻结 OpenAPI 已定义的 System Info/Health/Config、Security Audit Query 与 Dashboard，不新增 REST Path、Proto、Server Schema、Migration、第三方依赖或 Lockfile；Audit 继续保持 GET-only。
- M5-07：新增显式 Config 白名单投影、真实 SQLite 健康检查、进程入口 `started_at` 与单调 Uptime；Security Audit 使用 `(occurred_at,event_id) DESC` 稳定 Keyset、绑定全部 Filter 的 HMAC opaque Cursor、`from` inclusive/`to` exclusive 时间边界、Unicode Resource ID、可空 JSON 与小写 Digest Hex。Handler 严格要求 UTC `Z`，不返回 Token、Password、Private Key、Path、Listen、Trusted Proxy 或内部预算。
- M5-08：Dashboard 复用 System Health owner 和既有 Tunnel/Service Application 投影，前端直接渲染 Server 权威状态；Tunnel/Connector/Service/Active Connections 展示真实值，M6 Usage 与 Recent Errors 明确呈现 `UNAVAILABLE`，不伪造零值。桌面 `1440×1000` 与移动 `390×844` 真实浏览器复查通过，浏览器 Console 无 Warning。
- Build Version：新增 `internal/buildinfo` 单一 owner，Server System Info 与 Agent Connector Auth 共用同一不可变版本。未注入构建固定为 `(devel)`；正式 Local/OCI/CI 产品构建通过同一 Linker Target 显式注入。Dockerfile 的 Server/Agent Target 共用 `XTUNNEL_VERSION` 缺失、非法字符与 64 字符上限校验；OCI Smoke 从最终 Binary 校验注入值，Compose、Linux systemd CI 与 Windows amd64/arm64 Agent 构建入口同步传值。
- 验证与复审：Windows `go1.27.0`、`GOTOOLCHAIN=local` 下，`./tools/check-go-version.ps1`、`go test -count=1 -timeout 300s ./...`、受影响包 `go test -race -count=1 -timeout 300s`、`go vet ./...`、`$buildVersion='v0.1.0-local.verify'; $ldflags="-X github.com/lifei6671/xtunnel/internal/buildinfo.version=$buildVersion"; go build -trimpath -ldflags $ldflags ./cmd/server ./cmd/agent`、`npm --prefix web run check`、`npm --prefix web run build`、`sh -n`、`dash -n`、`bash --posix -n` 和 `git diff --check` 均通过。三路独立只读复审最终均为 `APPROVED`，无剩余 P0/P1/P2/P3。本机没有 Docker 与 Actionlint，OCI 正负例和 CI YAML 尚无本轮本地运行证据，等待远程 CI 验证。
- 状态与 Gate：当前为混合 staged/unstaged/untracked 脏工作区，没有最终 staged snapshot 复验、实现 Commit SHA、push、精确 CI Run 或用户阶段复审，因此 M5-07 与 M5-08 只进入 `REVIEW`，不得标记 `DONE`。M5 保持 `6/11`、全局保持 `70/95`，M5-09 不解锁，M5 Gate Checklist 六项全部保持未勾选。本次未勾选任何产品任务。
- 文档同步：根 README 同步本地实现状态、Dashboard 与双 Binary Version 构建入口；总技术方案同步 Build Version、Dashboard/System/Audit 与 OCI/Compose 长期契约；开发计划同步状态、计数、队列与本记录。OpenAPI、生成 Contract、Proto、Server Schema、Migration、依赖/Lockfile、反向代理、systemd/Windows 安装方式、权限与日志契约无需更新，因为机器契约和运行安装边界未变化。提交前必须选择性重新暂存本任务最终文件，并排除无关 `.agents/skills` 与 `output/`。

## 2026-08-29 · M5-07/M5-08 Linux Fixture 修复与精确 CI · REVIEW

- 首轮远程证据：实现与文档推送到远端 Head `7fe01da441e079876964a5eb91b15b22e84301ff` 后，[CI #33253282206](https://github.com/lifei6671/xtunnel/actions/runs/33253282206) 的 Windows Agent Service 成功，但 Linux amd64/arm64 同时在 `Test Go modules and build both processes` 失败。远程日志定位到四个 Linux-only Bootstrap 生命周期测试均因 `gatewayLifecycleTestConfig` 缺少现行公开投影要求的 `logging.level` 与 `limits.max_tunnels`，导致构造 System Read Service 返回 `system read input is invalid`；该失败不记录为产品 Gate 通过。
- 最小修复：提交 `71c706a013861946e6ca45f7104b72a28d04766e` 只在 Linux 生命周期测试 Fixture 补入 `Logging.Level=info` 与 `MaxTunnels=16`，不放宽生产 Config/System Read 校验，不修改外部行为、机器契约、依赖、部署或运行配置。
- 本地验证：Windows `go1.27.0`、`GOTOOLCHAIN=local` 下，Bootstrap 定向测试、`go test -count=1 -timeout 300s ./...`、`go vet ./...`、`go test -race -count=1 -timeout 180s ./internal/server/bootstrap`、Linux amd64/arm64 Bootstrap Test Binary 交叉编译与 `git diff --check` 全部通过。WSL 实例没有 `go`，因此 Linux 原生定向测试未在本地运行，未将交叉编译冒充原生证据。
- 精确 CI：修复 Head `71c706a013861946e6ca45f7104b72a28d04766e` 触发的 [CI #33253581635](https://github.com/lifei6671/xtunnel/actions/runs/33253581635) 从 2026-08-29 20:52:10 至 20:57:50（Asia/Shanghai）运行 5 分 40 秒；`verify (amd64)`、`verify (arm64)` 与 `Windows Agent service` 三个 Job 全部成功。两路 Linux 均穿过原失败 Bootstrap 测试，并完成现行 OpenAPI/Web、全仓 Go Test/Vet/Build、定向 Race、M4 Product、OCI Version 正负例、systemd 与最终工作树清洁检查。
- 状态与 Gate：M5-07/M5-08 已具备实现 Commit、失败修复、独立复审、本地验证与精确 CI，但尚未取得用户阶段复审结论，因此继续保持 `REVIEW`。M5 保持 `6/11`、全局保持 `70/95`，M5-09 不解锁，M5 Gate Checklist 六项全部保持未勾选；本次未勾选任何产品任务。
- 文档同步：只更新开发计划的当前结论、队列和精确 CI 证据。总技术方案、README、OpenAPI/生成物、Proto、Server Schema、Migration、依赖/Lockfile、CI/CD、部署配置、权限与日志契约无需更新，因为本次是测试 Fixture 修复和既有行为的验证闭环，没有改变产品或用户命令。

## 2026-08-29 · M5-07/M5-08 用户阶段复审 · DONE

- 复审与证据：用户确认阶段复审通过。实现、Linux Fixture 修复和证据提交依次为 `440d0183e2668b4213bbf6a74f2f11601de6a911`、`7fe01da441e079876964a5eb91b15b22e84301ff`、`71c706a013861946e6ca45f7104b72a28d04766e`、`a9f211ece1aa8cf68ce57eabdc23d1af9e6c1c2e`；精确绑定最终 Head 的 [CI #33253928022](https://github.com/lifei6671/xtunnel/actions/runs/33253928022) 从 2026-08-29 21:00:53 至 21:06:24（Asia/Shanghai）运行 5 分 31 秒，Windows Agent Service、Linux amd64 与 Linux arm64 全部成功。
- 状态影响：M5-07 与 M5-08 从 `REVIEW` 转为 `DONE`，M5 从 `6/11` 更新为 `8/11`，全局从 `70/95` 更新为 `72/95`；M5-09 的 M5-03 至 M5-08 依赖全部闭环，转为 `READY`。M5-10 继续等待 M5-09，M5-11 继续等待 M5-01 至 M5-10。
- Gate 边界：M5 Gate Checklist 六项全部保持未勾选。M5-07/M5-08 的 API、并发契约、Dashboard 浏览器复查和精确 CI 只是对应任务证据，不能替代 M5-09 日常管理工作流、M5-10 完整 Contract/Browser E2E 或 M5-11 总 Gate。
- 文档同步：根 README 同步 M5-07/M5-08 已完成复审并解锁 M5-09；开发计划同步当前阶段、任务状态、仪表盘、队列和本记录。总技术方案、OpenAPI/生成物、Proto、Server Schema、Migration、依赖/Lockfile、CI/CD、部署配置、权限、日志契约与 `AGENTS.md` 均无需更新，因为本轮只闭环既有实现证据，没有改变产品或机器契约。

## 2026-08-29 · M5-09 Tunnel/Connector/Service 管理 UI · REVIEW

- 实现范围：新增“链路工作台”，按创建 Tunnel、复制同一枚 ACTIVE Token 的 Connector 部署命令、确认只读 Runtime 上线、配置 Service 的日常顺序组织操作；支持 Tunnel 创建/重命名/撤销/删除、Token Reveal/Rotate/Revoke、Connector opaque Pagination、Service 创建/编辑/启用/禁用/删除及 HTTP/HTTPS/TCP Origin、HTTP/TCP Exposure 和可选健康检查。Connector 没有新增持久化 CRUD，Agent 也没有新增本地 Service Config。
- 并发与安全边界：所有 Mutation 携带同源 Origin 与内存 CSRF；Tunnel/Service 写入前只使用详情响应头的原始 ETag，`412` 会关闭旧弹层、刷新并要求重新确认；Workspace、分页、Token Reveal 和 Service 动作前 GET 均具备 Tunnel/generation fencing。Connection Token 只进入一次性 React 弹层状态，关闭后从 DOM 清除，不写 URL、LocalStorage、SessionStorage 或日志；浏览器验证 Fixture 已删除，未保留 Token-shaped 产物。
- 本地验证：Go 工具链检查通过 `go1.27.0 (local)`；`npm --prefix web ci`、`npm --prefix web run check`、`npm --prefix web run build`、`./tools/test-web-proxy.ps1` 通过；`go test -count=1 -timeout 180s ./web ./internal/server/managementapi ./internal/server/bootstrap`、同范围 `go test -race -count=1 -timeout 180s`、`go test -count=1 -timeout 300s ./...`、`go vet ./...` 与 `git diff --check` 通过。首次 `npm ci` 因仍运行的 Vite Preview 锁定 Windows 原生模块而失败，关闭本地预览后从锁文件完整重装并通过，不属于产品失败。
- 浏览器开发反馈：真实 Chromium 内核使用内存 Mock Route 复查 1440×1000 与 390×844 布局、Credential/Service Dialog、Escape 焦点恢复、Secret 关闭后 DOM 清除、浏览器 Storage 为空、移动端无页面横向溢出和 Console 零 Warning/Error。该证据不连接真实 Server，不能表述为 Browser E2E；完整 API Contract、真实浏览器工作流、CSRF/428/412/no-store 自动化与生成漂移组合验收仍属于 M5-10。
- 独立复审：契约、安全、前端、可访问性与测试证据三路复审先后发现并关闭旧 Dialog ETag 重试、Service 确认时机、跨 Tunnel 迟到响应、Mutation 网络异常、Dialog 焦点和错误提示遮挡等问题；最终无剩余 P0/P1，结论为 `APPROVED` / `APPROVED_FOR_REVIEW`。
- 状态与 Gate：当前尚无包含最终源码的实现 Commit SHA、精确 GitHub CI Run 或用户阶段复审，因此 M5-09 只进入 `REVIEW`。M5 保持 `8/11`、全局保持 `72/95`，M5-10/M5-11 继续等待；M5 Gate Checklist 六项全部保持未勾选，本次未将任何新产品任务标记为 `DONE`。
- 文档同步：根 README 与本开发计划同步 M5-09 能力、证据边界和 `REVIEW` 状态。总技术方案、OpenAPI/生成物、Proto、Server Schema、Migration、依赖/Lockfile、CI/CD、部署配置、权限、日志契约与 `AGENTS.md` 无需更新，因为本轮仅消费既有冻结 Contract，没有改变机器权威或外部协议。

## 2026-08-30 · M5-09 精确 CI 证据 · REVIEW

- 提交与 CI：M5-09 最终源码、README 与计划记录共同提交为 `daeb19d68f2009dbb92f8b333243f60e2e5b3649`。精确绑定该 SHA 的 [CI #33255917474](https://github.com/lifei6671/xtunnel/actions/runs/33255917474) 从 2026-08-29 21:47:33 至 21:53:08（Asia/Shanghai）运行 5 分 35 秒；Windows Agent Service、Linux amd64 与 Linux arm64 三个 Job 全部成功，两路 Linux 均通过现行 OpenAPI/Web、全仓 Go Test/Vet/Build、定向 Race、M4 Product、OCI/systemd 与最终工作树清洁检查。
- 状态与 Gate：最终产物、独立复审、提交和精确 CI 已齐备，但“继续下一步任务”按项目既有裁定不能替代用户阶段复审“通过”。M5-09 继续保持 `REVIEW`，M5 保持 `8/11`、全局保持 `72/95`，M5-10/M5-11 不解锁；M5 Gate Checklist 六项全部保持未勾选。本次未勾选任何产品任务。
- 文档同步：根 README 与本开发计划只同步精确 CI 和剩余 Review 边界。总技术方案、OpenAPI/生成物、Proto、Server Schema、Migration、依赖/Lockfile、CI/CD、部署配置、生产配置、权限、日志契约与 `AGENTS.md` 无需更新，因为证据闭环没有改变产品或机器契约。

## 2026-08-30 · M5-09 用户阶段复审与 M5-10 授权 · IN_PROGRESS

- 用户明确确认 M5-09 阶段复审通过。结合最终提交 `daeb19d68f2009dbb92f8b333243f60e2e5b3649`、独立复审和精确 [CI #33255917474](https://github.com/lifei6671/xtunnel/actions/runs/33255917474)，M5-09 从 `REVIEW` 转为 `DONE`；M5 从 `8/11` 更新为 `9/11`，全局从 `72/95` 更新为 `73/95`，M5-10 依赖闭环并进入 `IN_PROGRESS`。
- 授权边界：用户明确确认 M5-10 可以新增并锁定 `@playwright/test 1.62.1`、更新 `web/package-lock.json` 并修改 CI 接入真实 Browser E2E。Node 依赖仍只由 npm 11 Lockfile 和 `npm ci` 解析，CI 通过仓库本地锁定的 Playwright CLI 安装对应 Chromium。实现必须使用真实 Server、临时 SQLite、HTTPS 与 Chromium，不能把 Mock Route 或组件级 TLS Harness 冒充 Browser E2E；Secret 不得写入 Storage、Trace、HAR、Video、Screenshot 或测试报告。
- Gate 边界：M5 Gate Checklist 六项全部保持未勾选。M5-10 只有在 API 实际响应 Contract、错误码/并发 PATCH/CSRF/no-store、生成漂移和真实 Browser E2E 完成验证与复审后才能进入 `REVIEW`；M5-11 继续等待。

## 2026-08-30 · M5-10 Contract/E2E Test Suite · IN_PROGRESS

- Contract Suite：新增运行时 OpenAPI 响应校验器，先固定预期 `200/201/204`，再校验声明 Header、JSON Content-Type、Body Schema 或 `204` 空 Body；由 OpenAPI 反向核对 Auth 3、Tunnel 10、Service 7、Dashboard/System/Audit 5，共 25/25 Operation 的真实成功响应。12 个带 CSRF Security Requirement 的 Mutation 均证明缺少 Token 时返回 `403 CSRF_INVALID` 且资源状态不变。23 个 `APIErrorCode` 中 17 个由真实 HTTP 失败响应覆盖、5 个由实际错误映射 Case 覆盖，`FORBIDDEN` 是 V0.1 单管理员无角色边界下唯一冻结不可达项，不伪造生产请求。
- Browser E2E：Web 直接依赖精确锁定 `@playwright/test 1.62.1`。Linux 启动器构建真实 `xtunnel-server` 与嵌入 Web，创建临时 SQLite、管理员和 Loopback Certificate；同一 Server 依次由 CI 已按摘要锁定的 Caddy、Nginx 使用 host network 在同一 Origin 终止 HTTPS，Chromium 各执行 Login/Secure Cookie/AuthMe/CSRF/Logout、Tunnel UI 创建/重命名/删除、Connector Guide、Token Reveal/Rotate no-store、Service UI 创建/双页面 stale ETag `412`/重新编辑/启停/删除。Token 关闭后必须从 DOM、URL、LocalStorage 和 SessionStorage 消失；Trace、HAR、Video、Screenshot、Storage State 和持久测试报告均不生成。
- CI 与生命周期：Linux amd64 使用本地锁定 CLI 安装 Chromium；Caddy/Nginx 镜像摘要提升为同一 Job 环境权威，既有 Front Proxy E2E 先拉取并核对架构，随后 Browser Gate 复用。Browser step 精确创建当前 Runner 所有、权限 `0700` 的 `/run/xtunnel` 并在退出时 `rmdir`，不污染后续 OCI/systemd Smoke。Server、Docker Proxy、外部锁、Backup Socket、临时数据库、证书、密码与 Playwright Output 均有有界停止和精确清理；密码只注入单次 Playwright 命令，失败日志不回显 Server/Proxy 内容。
- 本地验证：Windows `go1.27.0` 且 `GOTOOLCHAIN=local` 下，Web Lockfile `npm ci`、`npm run check/build`、Playwright Test List、依赖树 `1.62.1`、OpenAPI Validate/Breaking/Generate Check/Wrapper Test、Contract 定向与 `count=10`、Management API Race/整包/Vet、全仓 `go test ./...`、`go vet ./...`、两进程 Build、`go mod verify`、`go mod tidy -diff`、Shell 语法、CI YAML 解析与 `git diff --check` 均通过。首轮三路复审发现并推动修复成功状态假阳性、人工错误码自证、Vite 代替真实反代、Secret 子进程继承、无界清理和 UI Rename 绕行；Contract、Browser、安全生命周期及 CI/Lockfile/文档三路二次复审均为 `APPROVED`，无剩余 P0/P1/P2/P3。
- 状态与 Gate：本机没有 Docker，WSL 只有 Windows Node/npm 互操作且缺少 Linux Go/Node，不能执行生产 Linux Server + Docker + Chromium 的完整 Browser Gate；未将交叉构建、Playwright `--list` 或静态 Shell 校验冒充真实 E2E。当前没有实现 Commit、精确 GitHub CI Run 或用户阶段复审，M5-10 继续保持 `IN_PROGRESS`，M5 保持 `9/11`、全局保持 `73/95`，M5-11 继续等待，M5 Gate Checklist 六项全部保持未勾选。

## 2026-08-30 · M5-10 首轮 CI 环境变量修复 · IN_PROGRESS

- 首轮提交与 CI：M5-10 实现提交并推送为 `f438a8be0ed295de4caba8075ef905189654e201`。精确绑定该 SHA 的 [CI #33288936289](https://github.com/lifei6671/xtunnel/actions/runs/33288936289) 中 Windows Agent Service 成功，但 Linux amd64/arm64 均在 `Test Go modules and build both processes` 失败，Browser Gate 尚未运行；本轮不记录为产品 Gate 通过。
- 根因与修复：两个锁定代理镜像原以 `XTUNNEL_CADDY_IMAGE`/`XTUNNEL_NGINX_IMAGE` 放入整个 Linux Job 环境，Server 的配置安全边界按设计拒绝未知 `XTUNNEL_` 变量，`TestProcessExitsOnSIGTERM` 因子进程先返回 `unknown XTUNNEL environment variable` 而失败。最小修复将 Job 级变量改为非产品前缀 `BROWSER_E2E_*`，只在 Browser 命令前临时映射为启动器要求的 `XTUNNEL_*` 输入，不再污染任何 Go Test、Server 或其他 CI 步骤。
- 本地验证与边界：带 `BROWSER_E2E_*` 环境运行 `go test ./internal/server/config ./internal/server/bootstrap` 通过，CI YAML 解析、Job 环境无 `XTUNNEL_` 前缀断言与 `git diff --check` 通过。Windows 不编译 `process_unix_test.go`，因此 Linux 原生回归仍须由修复提交后的精确 CI 补证。M5-10 继续保持 `IN_PROGRESS`，M5 Gate Checklist 六项全部未勾选，本次未勾选任何产品任务。

## 2026-08-30 · M5-10 Browser CI FD 上限修复 · IN_PROGRESS

- 二轮提交与 CI：环境变量隔离修复提交并推送为 `11338d4b935156573b6eed81a448af62d4f8a81b`。精确绑定该 SHA 的 [CI #33289102867](https://github.com/lifei6671/xtunnel/actions/runs/33289102867) 中 Windows Agent Service 成功，Linux amd64 的 OpenAPI、Web Build、全仓 Go Test/Vet/Build、M4 Product Data Plane、1 GiB Streaming 与原生 Caddy/Nginx HTTPS Ingress E2E 均成功，随后真实 Browser E2E 在启动器提升 `nofile` 时因 Runner 当前 shell 无权越过 hard limit 而失败；Chromium 测试尚未启动，本轮不记录为 Browser Gate 通过。
- 根因与修复：Server 默认 FD 预算要求进程 `nofile` 至少为 `137192`，Browser 启动器继续固定并验证 `1048576`，不能降低门槛规避失败。最小 CI 修复在已经创建隔离 Runtime Directory 后，通过 Runner 的 `sudo prlimit --pid "$$" --nofile=1048576:1048576` 只提升当前 Browser step shell 的 soft/hard limit，后续 Server 和 Chromium 子进程继承该上限；应用配置、FD Budget Manager、OCI/systemd 基线与其他 CI 步骤均不改变。
- 状态与 Gate：真实 Chromium 经 Caddy/Nginx 的两轮工作流仍需修复提交后的精确 CI 补证。M5-10 继续保持 `IN_PROGRESS`，M5 保持 `9/11`、全局保持 `73/95`，M5-11 继续等待，M5 Gate Checklist 六项全部保持未勾选。
