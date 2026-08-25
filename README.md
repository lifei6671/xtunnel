# XTunnel

XTunnel Standalone V0.1 正在按开发计划逐步实现。当前 Server/Agent 已具备配置加载、共享 JSON 结构化日志和前台进程生命周期骨架；Server 已接入 Stable Data Target、Linux External Lock、GORM SQLite 与显式 Migration，尚未启动 Management、Ingress 或 Agent Gateway。

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

当前尚未进入 Protocol v1 冻结阶段，因此 `api/proto` 为空，三个检查会明确输出 `SKIP`。这只证明 M0 工具链骨架可执行，不代表 Protocol Lint 或 Breaking Gate 已通过。

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

两个进程使用相同的配置入口：

```text
xtunnel-server --config <server.yaml> [--set <schema.path>=<value>]...
xtunnel-agent  --config <agent.yaml>  [--set <schema.path>=<value>]...
```

`--config` 可省略；`--set` 可以重复使用，同一路径以后出现的值为准。配置仍按 `CLI > XTUNNEL_* Environment > YAML > Schema Default` 合并，未知 Flag、位置参数或 Schema 路径会直接失败。配置字段以 `configs/server.schema.json` 和 `configs/agent.schema.json` 为准。

当前进程在配置校验通过后初始化标准库 `log/slog` JSON Handler，并在 `info` 级别输出 `process_started`、`process_stopped` 生命周期事件。基础字段固定为 `timestamp`、`level`、`component`、`event`；真实请求或 Trace 上下文存在时可追加 `request_id`、`trace_id`。

Server 当前按以下顺序初始化存储：

```text
Resolve Stable Data Target
→ Acquire Linux External Lock
→ Check Pending Restore Journal
→ Validate Canonical Data Directory
→ Open SQLite with GORM
→ Run Forward-only Migration
```

`server.data_dir` 必须是绝对路径，父目录和正式数据目录都需预先存在；Server 不会自动创建数据目录。Linux 运行环境还需预先创建归 Runtime UID 所有、权限为 `0700` 的 `/run/xtunnel`。数据库固定为 `<server.data_dir>/xtunnel.db`，连接使用 WAL、Foreign Keys、5 秒 Busy Timeout 和 Normal Synchronous。发现待处理 Restore Journal 时，当前版本会在打开数据库前拒绝启动；正式恢复状态机由后续 M3-12 实现。

收到 `SIGINT` 或 `SIGTERM` 后，Server 先关闭 SQLite 再释放 External Lock，Agent 也会正常退出。XTunnel V0.1 的生产运行边界为 Linux amd64/arm64；Windows 当前用于构建和单元测试，不提供生产 External Lock。完整 Listener、Session 和 Drain 流程将在后续任务中接入。

## OCI 与 systemd 包装

当前提供 Linux `amd64`/`arm64` 的 OCI 构建骨架，以及 Server/Agent 的 systemd 包装。Builder 与 Runtime Base 均以不可变摘要固定。OCI 容器只运行前台进程，不执行安装子命令；镜像固定使用非 root `UID:GID 65532:65532`，持久化目录为 `/var/lib/xtunnel`。生产运行时需要为该目录提供该 UID/GID 可写的 Volume；Server 在只读根文件系统下还必须挂载该 UID/GID 可写、权限 `0700` 的 `/run/xtunnel` tmpfs。

```sh
docker buildx build --load --platform linux/amd64 --target server --tag xtunnel-server:local -f deploy/docker/Dockerfile .
docker buildx build --load --platform linux/amd64 --target agent --tag xtunnel-agent:local -f deploy/docker/Dockerfile .

./deploy/docker/smoke.sh --target server --platform linux/amd64
./deploy/docker/smoke.sh --target agent --platform linux/amd64
```

项目同时提供 `deploy/docker/compose.dualstack.yaml`。该 Profile 为 Server/Agent 创建同时分配 IPv4、IPv6 地址的 Bridge Network；Management 只发布到宿主机 `127.0.0.1`/`::1`，Agent Gateway 显式发布到 `0.0.0.0`/`::`。启动前必须提供 Management 的真实外部 HTTPS Origin 和 Agent Gateway 公网主机名：

```sh
export XTUNNEL_MANAGEMENT_PUBLIC_URL=https://admin.example.com
export XTUNNEL_AGENT_GATEWAY_HOSTNAME=tunnel.example.com

docker compose --file deploy/docker/compose.dualstack.yaml up --build --detach
docker compose --file deploy/docker/compose.dualstack.yaml down

sh deploy/docker/dualstack-smoke.sh --platform linux/amd64
```

Compose 内部使用 `:8080`、`:7443` 表示双栈通配监听。Server 的底层监听原语会为这种空 Host 地址分别创建原生 `tcp4`、`tcp6` Socket；显式 IPv4 或 IPv6 地址仍保持单一地址族。当前原语尚未接入 Server 启动路径，Management、Agent Gateway 和 Ingress 仍未实现，因此现阶段的 Compose Smoke 只证明双栈网络、宿主端口绑定、OCI 安全边界与进程生命周期，不代表这些端口已可建立应用连接，也不证明公网 IPv6 路由或防火墙已经就绪。

systemd 包装只支持 Linux systemd。安装脚本分别创建 `xtunnel-server` 与 `xtunnel-agent` 系统用户/组；Server 状态目录为 `/var/lib/xtunnel`，Agent 状态目录为 `/var/lib/xtunnel-agent`。Agent Unit 会把 `data_dir` 和 `auth.token_file` 覆盖到 `/var/lib/xtunnel-agent` 及其 `token` 文件。二进制、配置和 Unit 分别安装到 `/usr/local/bin`、`/etc/xtunnel`、`/etc/systemd/system`；配置权限为 `root:<角色组> 0640`。卸载仅移除对应 Unit 和二进制，保留配置、凭据、持久化数据及服务用户/组。

```sh
sudo ./deploy/systemd/install.sh server --binary ./xtunnel-server --config ./server.yaml
sudo ./deploy/systemd/install.sh agent --binary ./xtunnel-agent --config ./agent.yaml

sudo ./deploy/systemd/uninstall.sh server
sudo ./deploy/systemd/uninstall.sh agent
```

`deploy/systemd/smoke.sh` 会创建并清理专用的测试用户、配置和数据目录。它只能在隔离 Linux 主机上以 root 身份执行，且在发现已有 XTunnel Unit、配置或数据目录时会拒绝运行：

```sh
sudo ./deploy/systemd/smoke.sh \
  --server-binary ./xtunnel-server \
  --agent-binary ./xtunnel-agent
```
