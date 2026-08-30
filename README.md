# XTunnel

XTunnel Standalone V0.1 正在按开发计划逐步实现。核心领域模型已对齐 Cloudflare Tunnel：管理端创建 Tunnel，Tunnel 持有一枚可重复取回的 ACTIVE Token；同一 Token 可启动多个临时 Connector，全部代理 Service 挂在 Tunnel 下。M1 核心数据面、M2 Credential Lifecycle & Failover Hardening、M3 Configuration/Health/Durable Operations、M4 Product Data Plane 与完整 M5 REST API/Web Console 均已通过验收、精确 CI 与用户阶段复审。Tunnel CRUD/Revoke、Token Reveal/Rotate/Revoke、四类部署命令、只读运行态 Connector 列表，以及 7 个 Service Operation、唯一 Nested Exposure 事务与运行态状态投影均已接入生成 Contract；Tunnel、Connector 和 Service List 已接入 HMAC opaque Pagination，Tunnel/Service 已覆盖完整 428/412、PATCH omitted/null/value 和原子 CAS 并发矩阵；System/Config/Security Audit 只读 Handler、只消费 Server 权威状态的 Dashboard API/UI，以及 Tunnel/Connector/Service 日常管理工作台均已完成。M5 总 Gate 的 OpenAPI/生成漂移、实际响应 Contract、并发 PATCH、Pagination、认证会话与真实 Web 日常工作流六项全部通过。M6-01 的全链路 JSON Logging、有限错误码和 Windows Event Log Source/Smoke 已通过本地验收、独立复审、两次精确 CI 与用户阶段复审；M6-02 的 Server 私有 Prometheus Registry、独立 `/metrics` 生命周期、20 项有限基数指标和真实 Linux 黑盒也已通过本地验收、独立复审、Linux amd64/arm64 精确 CI 与用户阶段复审；M6-03 的进程私有 OTLP/HTTP Runtime、五段跨进程 Trace、W3C OPEN 传播和真实 Linux 双架构黑盒已通过本地验收、独立复审、最终 Head 精确 CI 与用户阶段复审。M6-04 Usage Aggregation 已完成 minute/hour/day 前向 Migration、单 Owner 60 秒批量 Flush、幂等 Rollup、7 日 Retention、Service/Dashboard 当日读模型及真实 Linux 黑盒，当前等待精确 CI 与用户阶段复审。Server 已具备 Multi-Connector 公平选择、在线生命周期快照、Token Rotate/Revoke、Tunnel 全代 Revoke、RAW 前受限故障切换、持续 Snapshot/Route Reconcile、按 Service Health/Revision 过滤的 Connector 选择、两级 Health Target 硬预算、生产 HTTP Ingress，以及按持久化 Route 恢复的 Raw TCP/SSH Listener 与转发生命周期；Agent 已从当前 Ack 生效的内存 Snapshot 解析并连接 HTTP/HTTPS/TCP Origin，由进程级中心调度器执行服务健康检查，并经 Control Outbox 批量上报。

## 开发运行

项目固定使用 Go 1.27.0，并要求本地工具链模式：

```powershell
$env:GOTOOLCHAIN='local'
./tools/check-go-version.ps1
```

Proto 工具链运行在 Linux amd64/arm64；Windows 开发机通过安装了 Go 1.27.0 的 WSL 使用。工具只安装到仓库忽略的 `.tools/bin`，不会读取系统 `PATH` 中的 Buf 或 Generator：

```sh
export GOTOOLCHAIN=local
./tools/bootstrap-proto.sh
./tools/proto.sh lint
./tools/proto.sh breaking
./tools/proto.sh generate-check
```

`api/proto` 已使用 Tunnel/Connector/Service 语义生成 Protocol v1 代码与初始 Baseline。当前工作区的 `lint`、`breaking`、Golden Vector 与连续生成 Hash 已通过；正式 `generate-check` 仍需在包含这些变更的干净 checkout 执行，不能把脏工作区的等价检查冒充完整 Gate。

OpenAPI 机器契约固定为 3.1.0，并使用仓库锁定的 vacuum 校验。工具同样只安装到 `.tools/bin`，Windows 开发机通过 WSL 执行：

```sh
export GOTOOLCHAIN=local
npm --prefix tools/openapi-ts ci
./tools/bootstrap-openapi.sh
./tools/openapi.sh validate
./tools/openapi.sh breaking
./tools/openapi.sh generate-check
./tools/test-openapi.sh
```

当前 `api/openapi/openapi.yaml` 已冻结 19 个 Path、25 个 Operation，并由独立不可变初始 Baseline、真实 Breaking 负例和 CI Step 保护。Go Strict Server Contract 生成到 `internal/server/managementapi/contract.gen.go`，TypeScript Schema 生成到 `web/src/api/schema.gen.ts`，`web/src/api/client.ts` 只负责装配 `/api/v1` 与同源 Cookie。需要显式更新两端生成物时运行 `./tools/openapi.sh generate`；普通开发和 CI 使用 `generate-check`，任一端漂移都会失败。生成实现提交为 `b3fed99`，arm64 Checksum Fixture 修复为 `1fe7f01`；精确绑定最终 SHA 的 CI #25 在 Windows、Linux amd64 和 Linux arm64 全绿，M5-02 已转为 `DONE`。M5 总 Gate 仍全部未勾选，不能把生成契约验收冒充完整 REST/Web Gate。

Web 工程固定使用 Node 24.19.0、npm 11.17.0，直接依赖由 `web/package-lock.json` 精确锁定。生产构建必须先于任何会编译 `web` Go Package 的命令：

```powershell
Push-Location web
npm ci
npm run build
Pop-Location

$env:GOTOOLCHAIN='local'
go test ./...
$buildVersion='v0.1.0-local'
$ldflags="-X github.com/lifei6671/xtunnel/internal/buildinfo.version=$buildVersion"
go build -ldflags $ldflags ./cmd/server
go build -ldflags $ldflags ./cmd/agent
```

`v0.1.0-local` 只用于本地开发；正式发布值由发布流程同时注入 Server 与 Agent。未显式注入的普通构建固定报告 `(devel)`，运行时配置和环境变量不能覆盖 Binary Version。

`web/dist` 是被忽略的可重复构建产物，不提交占位文件；缺少生产构建时 Go Embed 会直接编译失败。当前 Web 已接入生成的同源 API Client，并提供登录、`SETUP_REQUIRED` 引导、会话恢复、退出、Dashboard，以及 Tunnel/Connector/Service 链路工作台；CSRF Token 和一次性 Connection Token 只保存在页面内存中。Agent、访问入口与 Settings 导航仍禁用待接入；M5-11 总 Gate 已通过精确 CI 与用户阶段复审，M5 完成度为 `11/11`。

本地开发只允许 HTTPS。将两个环境变量指向仓库外已信任的 Loopback Certificate 和 Private Key，并把 `management.public_url` 配置为浏览器实际使用的 Vite HTTPS Origin：

```powershell
$env:XTUNNEL_DEV_TLS_CERT='C:\path\to\loopback-cert.pem'
$env:XTUNNEL_DEV_TLS_KEY='C:\path\to\loopback-key.pem'
npm --prefix web run dev

./tools/test-web-proxy.ps1
```

开发代理只转发 `/api/v1` 到 `127.0.0.1:8080`，并保留 Host/Origin。缺少证书时不会降级 HTTP。M5-03 已用真实 SQLite 和 TLS HTTP Server 黑盒覆盖 Login、Secure Cookie、`/auth/me`、CSRF 失败与 Logout。M5-10 的 `npm --prefix web run test:e2e` 只作为 Linux Browser Gate 使用：它构建真实 Server，使用临时 SQLite 和管理员，在同一管理 Origin 上依次经锁定的 Caddy、Nginx 终止 HTTPS，并让 Chromium 各执行一次完整管理工作流。调用方必须按 CI 中的精确摘要预拉两个代理镜像、通过 `XTUNNEL_CADDY_IMAGE`/`XTUNNEL_NGINX_IMAGE` 传入，并预创建当前用户可写且权限为 `0700` 的 `/run/xtunnel`；本机开发仍使用上面的 Vite HTTPS 入口。

Server 继续使用 Schema 驱动的配置入口；Agent 没有 YAML 或本地配置文件，只接收一个版本化 Connection Token：

```text
xtunnel-server --config <server.yaml> [--set <schema.path>=<value>]...
xtunnel-agent run --token 'xta_...'
```

Server 的 `--config` 可省略；`--set` 可以重复使用，同一路径以后出现的值为准。Server 配置按 `CLI > XTUNNEL_* Environment > YAML > Schema Default` 合并，字段以 `configs/server.schema.json` 为唯一机器权威。

Connection Token 对用户始终是单个不透明的 `xta_...` 字符串，语义上携带 Server Endpoint、TLS Trust、Tunnel/Token Identity 与认证 Secret。创建 Tunnel 时首次签发；之后“添加 Connector”只取回逐字节相同的当前 Token，不创建 Connector 数据库行，也不新增 Token Version。只有显式 Rotate 才产生新版本；Token Revoke 只阻止新认证，Tunnel Revoke 则在持久提交后关闭全部 generation。上述 Application Workflow 与 M5-04 REST Handler 均已完成本地实现；Create、Reveal、Rotate 的 Secret 响应固定使用 `Cache-Control: no-store` 与 `Pragma: no-cache`。`run` 按 `--token`、`XTUNNEL_TOKEN`、OS Service Credential 的顺序取值；Linux systemd 使用运行时 Credential `xtunnel-agent.token`，Windows SCM 使用 DPAPI Machine-scope 加密 Credential。缺失时启动失败。

`--token` 只用于 `run` 的前台交互运行和 `service install` 的一次性安装输入。持久 Linux Unit 或 Windows SCM 配置都不包含 Token；Linux 自安装将它保存为 root-only `LoadCredential` Source，Windows 自安装将它保存为 `%ProgramData%\XTunnel\credentials\agent.token.dpapi` 的 DPAPI Machine-scope 密文。Tunnel 下的 Service、Origin 和 Health Policy 由 Server 远端下发，Agent 只在内存中应用。每次启动或重连都由 Server 先发送当前完整 Snapshot；APPLIED ConfigAck 前，Connector 不会进入 ONLINE、Work Auth、Pool 或 WorkDemand。后续 Service Origin 变更经持续 Reconcile 原子生效，无需重启 Agent；每条新连接使用系统 Resolver 解析 DNS，并让 DNS、IPv4/IPv6、TCP 与 HTTPS TLS 握手共同受该 Service 的 `connect_timeout` 约束。Agent 使用单个中心 Scheduler、固定并发与 Rate Budget、Heap 时序和 Jitter 执行 TCP/HTTP Health Check；结果按 Service 合并，固定每批最多 128 项、每秒 Flush，并在 Outbox 出队时按当前 Control Session 分配 generation。APPLIED ConfigAck 与重连全量 Health 通过同一 Outbox 事务提交，不会夹入旧状态。Server 只让 Current、非 Draining、已观测目标 Revision 且 Service 启用的 Connector 参与选择；Health Disabled 直接通过健康门禁，启用 Health 的 Service 必须具有当前 Revision、未超过 `2 × interval` 的 HEALTHY 报告，再按 Idle/Capacity、Least Active + RR 选择。Server 同时按 `(tunnel_id, connector_id)` 对 Health-enabled Service 计费，单 Tunnel 与全局上限来自 Server Schema；同 Connector 重连只增加 generation 引用而不重复计费，超限的新 Control Auth 返回可重试的 `HEALTH_BUDGET_EXCEEDED`，配置变更则在 SQLite 写入前 Reserve，并在事务提交后 Commit。V0.1 允许受信管理面配置 Loopback、RFC1918 和其他内网 Origin；可选 Egress Policy 仍属于后续阶段。

当前进程在启动输入校验通过后初始化标准库 `log/slog` JSON Handler，并在 `info` 级别输出 `process_started`、`process_stopped` 以及 Connector connected/replaced/draining/disconnected 生命周期事件。基础字段固定为 `timestamp`、`level`、`component`、`event`；真实请求或 Trace 上下文存在时可追加 `request_id`、`trace_id`。Server 使用私有 Prometheus Registry，通过独立监听面导出 20 项无高基数 ID Label 的运行指标；默认地址为 `127.0.0.1:9090/metrics`。非 loopback 暴露不增加应用层认证，必须由部署网络策略隔离；V0.1 不提供 Agent 本地 Metrics Listener。

OpenTelemetry Trace 默认关闭；Server 与 Agent 各自只有在进程环境中设置 `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` 或 `OTEL_EXPORTER_OTLP_ENDPOINT` 时，才启用进程私有的 OTLP/HTTP Protobuf Exporter。生产 Endpoint 必须使用 HTTPS，明文 HTTP 只允许 loopback；支持标准的 Headers、Timeout、`none`/`gzip` Compression 以及固定 `http/protobuf` Protocol 环境变量。Exporter 使用有界非阻塞队列，Collector 失败只产生一次脱敏 `EXPORT_FAILED` 告警，进程退出时最多用 5 秒 Flush。V0.1 不安装全局 OTel Provider/Propagator，也不接受文件型 CA/mTLS 环境变量；需要自定义 CA 或双向 TLS 时必须在前置 Collector/代理层终止，不能把证书或私钥路径交给 XTunnel。

Server Usage 以成功 `OPEN_OK` 后的 RAW 数据面为唯一来源，按 UTC minute、Tunnel 和 Service 聚合连接数与双向业务字节；内部 Failover 和 Agent Heartbeat 不重复入账。单 Owner 每 60 秒批量写入 SQLite，完成的 minute/hour 立即幂等上卷，day 固定保留 7 个 UTC 日并执行有界 Incremental Vacuum。Service 与 Dashboard 返回已持久化的当日 `AVAILABLE` Usage，最多落后当前流量 60 秒；进程 `kill -9` 仍可能丢失最后一个未 Flush 内存窗口。

Server 当前按以下顺序初始化存储：

```text
Resolve Stable Data Target
→ Acquire Linux External Lock
→ Recover / Roll Back Pending Restore Journal
→ Validate Canonical Data Directory
→ Open SQLite with GORM
→ Run Forward-only Migration
→ Load/Create independent Tunnel Token Master Key
→ Validate stored Snapshots and rebuild Health Target Budget
→ Load immutable Route Snapshot
→ Start Usage Flusher
→ Start Management API
→ No Admin: remain SETUP_REQUIRED with Management only
→ After first Admin: TCP Listener Restore → HTTP Ingress → Agent Gateway → Runtime Reconciler
```

`server.data_dir` 必须是绝对路径，父目录和正式数据目录都需预先存在；Server 不会自动创建数据目录。生产默认 Stable Parent 为 `/var/lib/xtunnel`，正式 Data Target 为 `/var/lib/xtunnel/data`；systemd StateDirectory 与 OCI Volume 必须挂载父目录，不能把可被 Restore rename 的 `data` leaf 直接用作挂载点。项目仍在开发中，不自动迁移旧 `/var/lib/xtunnel/xtunnel.db` 布局，systemd 安装器发现旧布局会在覆盖包装产物前拒绝。Linux 运行环境还需预先创建归 Runtime UID 所有、权限为 `0700` 的 `/run/xtunnel`。数据库固定为 `<server.data_dir>/xtunnel.db`，连接使用 WAL、Foreign Keys、5 秒 Busy Timeout 和 Normal Synchronous。完整 Tunnel Token 只以 AES-256-GCM 密文写入数据库，独立 32 字节主密钥位于 `<server.data_dir>/credentials/tunnel-token.key`；只要数据库已有 Token 密文，密钥缺失、损坏或权限不安全就会阻止启动。

Linux root 可使用以下维护命令；Archive 路径必须是绝对路径，输出不允许 stdout，也不会覆盖已有文件：

```sh
xtunnel-server backup create --output /secure/backup/xtunnel-backup.tar
xtunnel-server backup restore --input /secure/backup/xtunnel-backup.tar
```

运行中的 Server 通过 `/run/xtunnel/backup-<target-hash>.sock` 提供在线 Create Barrier；Socket 不存在时 Create 获取同一 External Lock 后离线执行，Socket 存在但连接或认证失败时不会静默回退。在线 Lease 断线会立即取消归档，只有完整落盘并收到 release ACK 的输出才会发布。Restore 始终要求 Server 已停止并独占 External Lock。CLI 以 `openat2` 固定 SQLite 源 inode，同一 FD 完成 Schema 检查和包含 WAL 可见状态的 Backup；原路径在操作期间被 symlink/rename 替换会 fail closed 并删除候选，不会切换源或遗漏原名 WAL。Archive 为权限 `0600` 的 canonical USTAR，包含 SQLite 自包含备份、32 字节 Tunnel Token Master Key，以及 pinned 模式下最终 Gateway key/certificate；Public TLS 外部证书不进入 Archive。存在未完成 Gateway Rotation Journal/临时文件时 Create 会拒绝，需先完成正常启动或维护 Reconciliation。Restore 会先在 sibling staging 中校验 Manifest/Hash、SQLite 完整性与实际 Schema、Token Key 对全部 Token 密文的解密和身份/Secret Hash 一致性、Pinned Identity，再以 rollback + Journal 原子切换；下次启动会在打开 SQLite 前自动完成或回滚中断的 Restore。该能力已随 M3 Gate 的崩溃/文件系统自动化证据和 CI #20 完成阶段验收；正式 Alpha 发布仍需 M7 Filesystem Failpoint/Release Matrix 证据。

生产 HTTP Ingress 使用严格 Host/Path Route、Streaming Reverse Proxy 和 Tunnel-aware HTTP/1.1 Transport。每条池化连接对应一条 ACTIVE WorkConn，并按 Tunnel、Service 与 Required Revision 隔离；建立新 WorkConn 时只选择 Service Required Revision 与匹配 Route 精确相等的 Connector，同键顺序请求可 KeepAlive 复用，跨键不会复用，旧 Route generation 也不会覆盖新池。Origin Host 按 `origin_http_host > preserve_host > origin host[:port]` 选择；禁用 Chunked 且请求长度无法在不读取 Body 的前提下确定时返回显式错误。入口采用 10 秒 Header Read、60 秒 Request Body 滑动空闲窗，OPEN 使用 6 秒总预算；Tunnel Offline、Origin Refused/Timeout、容量耗尽、配置未观察和 Service Disabled 返回稳定公开错误码，不暴露内部拨号文本。Forwarded 边界只信任 `http_ingress.trusted_proxies` 中的实际 TCP Peer，从右向左验证最多 32 跳代理链，并在删除全部外部伪造元数据后只重建一组 `X-Forwarded-For/Proto/Host`；未受信 Peer 始终使用真实 Peer IP。HTTP/1.1 WebSocket Upgrade 必须无 Request Body；已知长度超过 Server Body 上限的握手先返回 `413 REQUEST_BODY_TOO_LARGE`，其余带 Body 或 Transfer-Encoding 的握手在 Tunnel Dial 前返回 `501 UPGRADE_NOT_SUPPORTED`，两者都关闭客户端连接复用。合法 Upgrade 使用 fresh、不可重试的 WorkConn 透明转发双向帧，保留 Half-Close；Origin 握手响应头受 10 秒预算限制，101 后任一方向字节进展都会续期双方共享的 1 小时 idle window，完整连接不设短总时限。h2c 与其他 Upgrade 同样在 Tunnel Dial 前返回 `501 UPGRADE_NOT_SUPPORTED`。TCP Listener Manager 把 `tcp_ingress.min_port..max_port` 作为逻辑预留池：Route 可显式选端口，也可由服务端在事务内自动选择并持久化具体端口；Server 只监听已启用 Route 实际占用的端口，不会预绑定整个范围。TCP Accept 在登记连接前核验当前 Route generation，删除、禁用或更新后的旧 Listener 不再接纳新连接；通过准入的连接使用当时捕获的 Tunnel、Service 和精确 Required Revision，在 10 秒 Pre-OPEN 上界内建立 WorkConn。进入 RAW 后不识别 SSH 或其他业务协议，逐字节双向转发并保留 Half-Close。Tunnel/Origin/容量失败只关闭公网连接，Server 日志只记录脱敏稳定 `error_code` 和路由标识，不输出 Origin 或底层错误文本。

M4-07 已接入精确 Required Revision、10 秒 Pre-OPEN、逐字节双向转发、Half-Close 和脱敏稳定 `error_code`。TCP Listener 把值传递 `DialRequest` 与真实 Public Peer 交给 Tunnel Proxy；OPEN 和 ACTIVE 使用分离的 Context，Tunnel Revoke、Drain 或 Server Shutdown 都能主动关闭 WorkConn 与公网 Peer。该任务已随提交 `834a9de` 通过 CI #19 的原生 Linux amd64/arm64 Product Gate 和用户阶段验收，并在 M3-13 依赖闭环后转为 `DONE`。

M4-08 提供 [Caddy/Nginx HTTPS/WSS 前置代理示例](deploy/reverse-proxy/README.md)，TLS 只在前置代理终止，代理到 XTunnel 的 upstream 固定为 loopback HTTP/1.1。两者都在公网 TLS 端落实 10 秒 Header Read；Caddy 使用 `100ms` 有限刷新间隔、保留客户端断开的 upstream 取消传播且不设置方向性 upstream timeout。标准 Nginx 无法表达双向共享 idle，模板使用其支持上限内的 `24d` 方向性 ceiling，避免覆盖 XTunnel 的 1 小时共享裁决；严格单向超过 24 天的流需要反向 heartbeat 或改用 Caddy。自动化 E2E 使用固定多架构镜像摘要和临时测试证书，分别验证真实 HTTPS、WSS 双向帧、完整 Host authority、Origin 透明传递、外部伪造 Forwarded 元数据被覆盖，以及客户端中断后的 upstream 收敛；修正后的固定摘要 Caddy/Nginx 已在本地 Linux amd64 Docker Runtime 和提交 `834a9de` 对应的 CI #19 原生 Linux amd64/arm64 Job 通过。用户阶段验收已通过，M3-13 依赖闭环后该任务转为 `DONE`。

M4-09 已把 Public Ingress 限额接入真实入口。Raw TCP 在 `Accept` 后、连接登记与 goroutine 创建前按实际 Peer IP 消费 OPEN Token，HTTP 在可信代理规范化后逐请求限速，并只在新建 Tunnel WorkConn 时消费 OPEN Token；KeepAlive 复用不重复扣减。Global/Tunnel/Service/Source IP ACTIVE 配额覆盖 TCP 连接或 HTTP/WebSocket Handler 生命周期，来源状态使用最多 32 分片的有界 LRU + TTL。HTTP Body 默认流式限制为 2 GiB，唯一上限来自 `configs/server.schema.json` 的 `limits.max_http_body_bytes`，前置 Nginx 不再另设固定阈值；超限返回 `413 REQUEST_BODY_TOO_LARGE`。Nginx 同时允许单个 Header 达到 Schema 支持的最大 1 MiB，不会被默认 8 KiB 缓冲提前拒绝；Header 聚合上限仍由 Go HTTP Server 在 Handler 前以标准 431 裁决。请求或 OPEN 速率超限返回 `429 RATE_LIMITED` 和 `Retry-After: 1`。完整启动装配后的公网黑盒 E2E 与原生 Linux amd64/arm64 CI 已在提交 `834a9de`、CI #19 通过，用户阶段验收也已通过；M3-13 依赖闭环后该任务转为 `DONE`。

收到 `SIGINT` 或 `SIGTERM` 后，Server 先停止 Metrics、Management、TCP/HTTP/Gateway 新入口并关闭 idle HTTP WorkConn，再在同一个固定 Deadline 内并行排空 Metrics/Management 抓取与请求、TCP/HTTP 请求和 Session ACTIVE；到期后主动关闭残留 Socket，随后关闭 Gateway、Route Owner、SQLite 并释放 External Lock。XTunnel V0.1 Server 的生产运行边界仍为 Linux amd64/arm64，不提供 Windows Server External Lock；Agent 支持 Linux amd64/arm64 与 Windows amd64/arm64。Registry 已按 Tunnel 对 Current Connector Session 先执行未排空、Pool Idle 与容量过滤，再用 Least Active + 稳定 Round Robin 取得原子连接租约，并保留旧 generation ActiveWork tombstone。M2 的 Token Rotate/Revoke、跨 Connector 故障切换策略和在线生命周期可观测性已通过阶段 Review 与全绿 CI，M2 当前为 `DONE`；这些实现不重复 M1 已有的默认负载选择。

## OCI 与 Agent Service Self-install

当前提供 Linux `amd64`/`arm64` 的 OCI 构建骨架，以及 Server Shell 包装和 Agent Binary 自管理的 systemd 服务。Builder 与 Runtime Base 均以不可变摘要固定。Agent OCI Image 的默认命令是 `run`，容器不执行 `service install/uninstall`；镜像固定使用非 root `UID:GID 65532:65532`。只有 Server 使用 `/var/lib/xtunnel` 稳定父目录 Volume 和 `/run/xtunnel` 可写 tmpfs；镜像在 Volume 首次 copy-up 前预创建权限 `0700` 的 `/var/lib/xtunnel/data` leaf。Agent 不声明持久 Volume，通过 `XTUNNEL_TOKEN` 环境变量接收 Token。

Server 默认配置的启动 FD 预算为 `137192`。其中 TCP Listener 按默认逻辑端口池 `10000..60000` 的最大占用量和一个原子换口候选计入峰值预算，但启动时只绑定已启用 Route 的具体端口。仓库提供的 Compose 和 systemd Unit 将 `nofile` soft/hard limit 固定为 `1048576`；若绕过这些入口直接运行 Server OCI 镜像，也必须向容器提供同等上限，例如 `--ulimit nofile=1048576:1048576`。应用仍按配置预算限制实际连接数，不会因为提高进程上限而无界占用 FD。

```sh
docker buildx build --load --platform linux/amd64 --target server --build-arg XTUNNEL_VERSION=v0.1.0-local --tag xtunnel-server:local -f deploy/docker/Dockerfile .
docker buildx build --load --platform linux/amd64 --target agent --build-arg XTUNNEL_VERSION=v0.1.0-local --tag xtunnel-agent:local -f deploy/docker/Dockerfile .

./deploy/docker/smoke.sh --target server --platform linux/amd64
./deploy/docker/smoke.sh --target agent --platform linux/amd64
```

项目同时提供 `deploy/docker/compose.dualstack.yaml`。该 Profile 为 Server/Agent 创建同时分配 IPv4、IPv6 地址的 Bridge Network；Management 只发布到宿主机 `127.0.0.1`/`::1`，Agent Gateway 显式发布到 `0.0.0.0`/`::`。启动前必须提供 Management 的真实外部 HTTPS Origin、Agent Gateway 公网主机名和 Tunnel Token；Compose 继续使用既有宿主变量名 `XTUNNEL_AGENT_TOKEN`，并映射为 Agent 容器内的 `XTUNNEL_TOKEN`：

```sh
export XTUNNEL_MANAGEMENT_PUBLIC_URL=https://admin.example.com
export XTUNNEL_AGENT_GATEWAY_HOSTNAME=tunnel.example.com
export XTUNNEL_AGENT_TOKEN='xta_...'
export XTUNNEL_VERSION=v0.1.0-local

docker compose --file deploy/docker/compose.dualstack.yaml up --build --detach
docker compose --file deploy/docker/compose.dualstack.yaml down

sh deploy/docker/dualstack-smoke.sh --platform linux/amd64
```

Compose 内部使用 `:8080`、`:7443` 表示双栈通配监听。Server 的底层监听原语会为这种空 Host 地址分别创建原生 `tcp4`、`tcp6` Socket；显式 IPv4 或 IPv6 地址仍保持单一地址族。Management、HTTP/TCP Ingress 与 Agent Gateway 均已接入生产启动生命周期，但该双栈监听原语尚未接入这些产品 Listener，也尚未取得 Compose 双栈应用连通证据。因此现阶段的 Compose Smoke 只证明双栈网络、宿主端口绑定、OCI 安全边界与进程生命周期，不代表 Management、Ingress 或 Agent Gateway 已通过双栈应用连接验收，也不证明公网 IPv6 路由或防火墙已经就绪。

Agent systemd 自安装只支持 root、Linux 和 systemd 249 及以上；任一条件不满足都会在写文件或创建用户前快速失败。Binary 内嵌首行为 `# Managed by xtunnel-agent service install` 的 Unit，`service install` 创建 `xtunnel-agent` 系统用户/组，把当前 Binary 原子安装到 `/usr/local/bin/xtunnel-agent`，并创建 `/etc/xtunnel/credentials/agent.token`（父目录 `root:root 0700`、文件 `root:root 0600`）。Unit 使用 `LoadCredential` 注入 Token，`ExecStart=/usr/local/bin/xtunnel-agent run`，不含 Secret。已有 Unit 不是普通文件或缺少该 marker 时拒绝覆盖或卸载。

```sh
sudo ./deploy/systemd/install.sh server --binary ./xtunnel-server --config ./server.yaml
sudo xtunnel-agent service install --token 'xta_...'

sudo ./deploy/systemd/uninstall.sh server
sudo xtunnel-agent service uninstall
```

Agent 卸载只删除由 managed marker 确认归属的 Unit 和已安装 Binary，保留 root-only Credential Source 与 `xtunnel-agent` 用户/组，便于安全重装；`deploy/systemd` Shell 包装仅服务于 Server。

Windows Agent 使用同一组 `service install/uninstall` 子命令，不提供 PowerShell、批处理或 MSI 安装脚本。管理员在提升权限的 PowerShell 中执行：

```powershell
.\xtunnel-agent.exe service install --token 'xta_...'
& "$env:ProgramFiles\XTunnel\xtunnel-agent.exe" service uninstall
```

Windows 自安装仅支持 amd64/arm64、提升权限的 Administrator 与可用 SCM。它注册 `XTunnelAgent`（DisplayName `XTunnel Agent`），以 `NT AUTHORITY\LocalService` 运行；Binary 安装到 `%ProgramFiles%\XTunnel\xtunnel-agent.exe`，SCM ImagePath 只包含该 Binary 与 `run`，不含 Token。Credential 使用 `CRYPTPROTECT_LOCAL_MACHINE | CRYPTPROTECT_UI_FORBIDDEN` 加密后写入 `%ProgramData%\XTunnel\credentials\agent.token.dpapi`，该密文不是用户配置文件。Description 必须精确为 `Managed by xtunnel-agent service install`，用于归属判断；非受管的同名 Service 拒绝覆盖或删除。重复安装通过 Windows Replace Existing + Write Through 语义替换 Binary/Credential 并重启服务。卸载立即删除受管 SCM Service 并保留 DPAPI Credential；若命令由已安装 EXE 自身执行，Windows 文件锁会让 Binary 安排到下次系统重启删除。

SCM 的 Stop/Shutdown 最多等待 30 秒；运行回调异常返回非零 Service Exit，并为 non-crash failure 配置恢复重启。安装器同时注册受 managed marker 保护的 `XTunnelAgent` Application Event Log Source，SCM 模式把共享 JSON 日志按级别持久写入该 Source；Source 缺失、被修改、打不开或写入失败时明确失败，不回退 stderr。卸载只清理确认受管的 Source，并继续保留 DPAPI Credential。

`deploy/windows/smoke.ps1` 只是需要提升权限、会临时安装/删除 `XTunnelAgent` 的隔离验收脚本，不是用户安装入口。真实部署始终使用 Agent Binary 自身的 `service install/uninstall`。

`deploy/systemd/smoke.sh` 会创建并清理专用的测试用户、配置和数据目录。它只能在隔离 Linux 主机上以 root 身份执行，且在发现已有 XTunnel Unit、配置或数据目录时会拒绝运行：

```sh
sudo ./deploy/systemd/smoke.sh \
  --server-binary ./xtunnel-server \
  --agent-binary ./xtunnel-agent
```
