# XTunnel

XTunnel Standalone V0.1 正在按开发计划逐步实现。核心领域模型已对齐 Cloudflare Tunnel：管理端创建 Tunnel，Tunnel 持有一枚可重复取回的 ACTIVE Token；同一 Token 可启动多个临时 Connector，全部代理 Service 挂在 Tunnel 下。M1 核心数据面已通过阶段 Review 与全绿 CI；M2 Credential Lifecycle & Failover Hardening 已完成本地实现、整仓 Test/Race/Vet 与独立交叉复审，现等待阶段 Review。Server 已具备 Multi-Connector 公平选择、在线生命周期快照、Token Rotate/Revoke、Tunnel 全代 Revoke 与 RAW 前受限故障切换；Management REST、生产 Public Listener、远端 Service 配置和 `/metrics` 导出仍由后续里程碑实现。

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
./tools/bootstrap-openapi.sh
./tools/openapi.sh validate
./tools/test-openapi.sh
```

当前 `api/openapi/openapi.yaml` 只有可校验骨架，Server 固定为同源基路径 `/api/v1`，尚不包含业务路径或 DTO。Validate 通过只代表 M0 骨架和基路径约束有效，不代表 M5 REST Contract Gate 已通过。

Web 工程固定使用 Node 24.19.0、npm 11.17.0，直接依赖由 `web/package-lock.json` 精确锁定。生产构建必须先于任何会编译 `web` Go Package 的命令：

```powershell
Push-Location web
npm ci
npm run build
Pop-Location

$env:GOTOOLCHAIN='local'
go test ./...
go build ./cmd/server
```

`web/dist` 是被忽略的可重复构建产物，不提交占位文件；缺少生产构建时 Go Embed 会直接编译失败。当前页面只是 React/Vite 工程骨架，不包含业务 DTO、API Client 或真实管理页面。

本地开发只允许 HTTPS。将两个环境变量指向仓库外已信任的 Loopback Certificate 和 Private Key，并把 `management.public_url` 配置为浏览器实际使用的 Vite HTTPS Origin：

```powershell
$env:XTUNNEL_DEV_TLS_CERT='C:\path\to\loopback-cert.pem'
$env:XTUNNEL_DEV_TLS_KEY='C:\path\to\loopback-key.pem'
npm --prefix web run dev

./tools/test-web-proxy.ps1
```

开发代理只转发 `/api/v1` 到 `127.0.0.1:8080`，并保留 Host/Origin。缺少证书时不会降级 HTTP；脚本 Smoke 只验证 HTTPS 和代理机械链路，真实 Login、Secure Cookie、CSRF 与 Logout E2E 留到 M5。

Server 继续使用 Schema 驱动的配置入口；Agent 没有 YAML 或本地配置文件，只接收一个版本化 Connection Token：

```text
xtunnel-server --config <server.yaml> [--set <schema.path>=<value>]...
xtunnel-agent run --token 'xta_...'
```

Server 的 `--config` 可省略；`--set` 可以重复使用，同一路径以后出现的值为准。Server 配置按 `CLI > XTUNNEL_* Environment > YAML > Schema Default` 合并，字段以 `configs/server.schema.json` 为唯一机器权威。

Connection Token 对用户始终是单个不透明的 `xta_...` 字符串，语义上携带 Server Endpoint、TLS Trust、Tunnel/Token Identity 与认证 Secret。创建 Tunnel 时首次签发；之后“添加 Connector”只取回逐字节相同的当前 Token，不创建 Connector 数据库行，也不新增 Token Version。只有显式 Rotate 才产生新版本；Token Revoke 只阻止新认证，Tunnel Revoke 则在持久提交后关闭全部 generation。上述 Application Workflow 已完成，REST Handler 仍归 M5。`run` 按 `--token`、`XTUNNEL_TOKEN`、OS Service Credential 的顺序取值；Linux systemd 使用运行时 Credential `xtunnel-agent.token`，Windows SCM 使用 DPAPI Machine-scope 加密 Credential。缺失时启动失败。

`--token` 只用于 `run` 的前台交互运行和 `service install` 的一次性安装输入。持久 Linux Unit 或 Windows SCM 配置都不包含 Token；Linux 自安装将它保存为 root-only `LoadCredential` Source，Windows 自安装将它保存为 `%ProgramData%\XTunnel\credentials\agent.token.dpapi` 的 DPAPI Machine-scope 密文。Tunnel 下的 Service、Origin 和 Health Policy 由 Server 远端下发，Agent 只在内存中应用。

当前进程在启动输入校验通过后初始化标准库 `log/slog` JSON Handler，并在 `info` 级别输出 `process_started`、`process_stopped` 以及 Connector connected/replaced/draining/disconnected 生命周期事件。基础字段固定为 `timestamp`、`level`、`component`、`event`；真实请求或 Trace 上下文存在时可追加 `request_id`、`trace_id`。Session Manager 同时提供五项无 Label Runtime Metric Source，HTTP `/metrics` 导出归 M6。

Server 当前按以下顺序初始化存储：

```text
Resolve Stable Data Target
→ Acquire Linux External Lock
→ Check Pending Restore Journal
→ Validate Canonical Data Directory
→ Open SQLite with GORM
→ Run Forward-only Migration
→ Load/Create independent Tunnel Token Master Key
```

`server.data_dir` 必须是绝对路径，父目录和正式数据目录都需预先存在；Server 不会自动创建数据目录。Linux 运行环境还需预先创建归 Runtime UID 所有、权限为 `0700` 的 `/run/xtunnel`。数据库固定为 `<server.data_dir>/xtunnel.db`，连接使用 WAL、Foreign Keys、5 秒 Busy Timeout 和 Normal Synchronous。完整 Tunnel Token 只以 AES-256-GCM 密文写入数据库，独立 32 字节主密钥位于 `<server.data_dir>/credentials/tunnel-token.key`；只要数据库已有 Token 密文，密钥缺失、损坏或权限不安全就会阻止启动。发现待处理 Restore Journal 时，当前版本会在打开数据库前拒绝启动；正式恢复状态机由后续 M3-12 实现。

收到 `SIGINT` 或 `SIGTERM` 后，Server 先让 Session 退出选路并执行 Drain，Agent 停止补充 WorkConn、等待 ACTIVE 连接自然结束，超过固定 Deadline 才强制关闭；随后 Server 关闭 SQLite 并释放 External Lock。XTunnel V0.1 Server 的生产运行边界仍为 Linux amd64/arm64，不提供 Windows Server External Lock；Agent 支持 Linux amd64/arm64 与 Windows amd64/arm64。Registry 已按 Tunnel 对 Current Connector Session 先执行未排空、Pool Idle 与容量过滤，再用 Least Active + 稳定 Round Robin 取得原子连接租约，并保留旧 generation ActiveWork tombstone。M2 的 Token Rotate/Revoke、跨 Connector 故障切换策略和在线生命周期可观测性已完成本地实现，当前仍待阶段 Review 与覆盖本次变更的全绿 CI，不能标记为 `DONE`；这些实现不重复 M1 已有的默认负载选择。

## OCI 与 Agent Service Self-install

当前提供 Linux `amd64`/`arm64` 的 OCI 构建骨架，以及 Server Shell 包装和 Agent Binary 自管理的 systemd 服务。Builder 与 Runtime Base 均以不可变摘要固定。Agent OCI Image 的默认命令是 `run`，容器不执行 `service install/uninstall`；镜像固定使用非 root `UID:GID 65532:65532`。只有 Server 使用 `/var/lib/xtunnel` 持久 Volume 和 `/run/xtunnel` 可写 tmpfs；Agent 不声明持久 Volume，通过 `XTUNNEL_TOKEN` 环境变量接收 Token。

Server 默认配置的启动 FD 预算为 `87188`。仓库提供的 Compose 和 systemd Unit 将 `nofile` soft/hard limit 固定为 `1048576`；若绕过这些入口直接运行 Server OCI 镜像，也必须向容器提供同等上限，例如 `--ulimit nofile=1048576:1048576`。应用仍按配置预算限制实际连接数，不会因为提高进程上限而无界占用 FD。

```sh
docker buildx build --load --platform linux/amd64 --target server --tag xtunnel-server:local -f deploy/docker/Dockerfile .
docker buildx build --load --platform linux/amd64 --target agent --tag xtunnel-agent:local -f deploy/docker/Dockerfile .

./deploy/docker/smoke.sh --target server --platform linux/amd64
./deploy/docker/smoke.sh --target agent --platform linux/amd64
```

项目同时提供 `deploy/docker/compose.dualstack.yaml`。该 Profile 为 Server/Agent 创建同时分配 IPv4、IPv6 地址的 Bridge Network；Management 只发布到宿主机 `127.0.0.1`/`::1`，Agent Gateway 显式发布到 `0.0.0.0`/`::`。启动前必须提供 Management 的真实外部 HTTPS Origin、Agent Gateway 公网主机名和 Tunnel Token；Compose 继续使用既有宿主变量名 `XTUNNEL_AGENT_TOKEN`，并映射为 Agent 容器内的 `XTUNNEL_TOKEN`：

```sh
export XTUNNEL_MANAGEMENT_PUBLIC_URL=https://admin.example.com
export XTUNNEL_AGENT_GATEWAY_HOSTNAME=tunnel.example.com
export XTUNNEL_AGENT_TOKEN='xta_...'

docker compose --file deploy/docker/compose.dualstack.yaml up --build --detach
docker compose --file deploy/docker/compose.dualstack.yaml down

sh deploy/docker/dualstack-smoke.sh --platform linux/amd64
```

Compose 内部使用 `:8080`、`:7443` 表示双栈通配监听。Server 的底层监听原语会为这种空 Host 地址分别创建原生 `tcp4`、`tcp6` Socket；显式 IPv4 或 IPv6 地址仍保持单一地址族。当前该双栈原语尚未接入 Server 启动路径；Management 和 Ingress 仍未实现，Agent Gateway 已由独立 Listener 接入首个 Admin 之后的生产生命周期，但尚未取得 Compose 双栈应用连通证据。因此现阶段的 Compose Smoke 只证明双栈网络、宿主端口绑定、OCI 安全边界与进程生命周期，不代表双栈 Agent Gateway 已可建立应用连接，也不证明公网 IPv6 路由或防火墙已经就绪。

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

SCM 的 Stop/Shutdown 最多等待 30 秒；运行回调异常返回非零 Service Exit，并为 non-crash failure 配置恢复重启。当前尚未注册 Windows Event Log Source，SCM 模式下的 JSON stderr 不保证持久可见；这一可观测性缺口属于 M6，不能把当前 stderr 当作生产日志 Gate 已通过。

`deploy/windows/smoke.ps1` 只是需要提升权限、会临时安装/删除 `XTunnelAgent` 的隔离验收脚本，不是用户安装入口。真实部署始终使用 Agent Binary 自身的 `service install/uninstall`。

`deploy/systemd/smoke.sh` 会创建并清理专用的测试用户、配置和数据目录。它只能在隔离 Linux 主机上以 root 身份执行，且在发现已有 XTunnel Unit、配置或数据目录时会拒绝运行：

```sh
sudo ./deploy/systemd/smoke.sh \
  --server-binary ./xtunnel-server \
  --agent-binary ./xtunnel-agent
```
