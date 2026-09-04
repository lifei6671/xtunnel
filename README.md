<p align="center">
  <img src="docs/static/logo.png#gh-light-mode-only" alt="XTunnel" width="180">
</p>

<h1 align="center">XTunnel</h1>

<p align="center"><strong>把内网服务，安全、可控地发布到公网。</strong></p>

<p align="center">
  自托管反向隧道 · Web 控制台 · HTTP / WebSocket / TCP · Linux / Windows Agent
</p>

<p align="center">
  <a href="#快速开始">快速开始</a> ·
  <a href="configs/README.md">配置示例</a> ·
  <a href="docs/operations_runbook.md">运维手册</a> ·
  <a href="docs/xtunnel_standalone_v0.1_development_plan.md">开发进度</a>
</p>
<p align="center">
  <a href="https://github.com/lifei6671/xtunnel/actions/workflows/ci.yml"><img src="https://github.com/lifei6671/xtunnel/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-GPL--3.0-blue.svg" alt="License: GPL-3.0"></a>
</p>

> [!WARNING]
> **开发预览**：核心产品能力已经完成，发布、升级与跨平台安装矩阵仍在收尾。
> Alpha Release Gate 尚未通过，仓库当前没有正式 Release，不应按稳定生产版本使用。

## 让私有服务拥有稳定的公网入口

XTunnel 是一款可自托管的反向隧道。你只需在公网部署一个 XTunnel Server，
在内网运行 XTunnel Agent，就能通过 Web 控制台发布 HTTP、WebSocket、SSH、
数据库和其他 TCP 服务，无需为每个内网服务单独暴露公网端口。

```text
公网用户
   │
   ▼
Caddy / Nginx ── XTunnel Server
                        │ TLS/TCP
                 ┌──────┴──────┐
                 ▼             ▼
             Agent A       Agent B
                 │             │
              内网服务      内网服务
```

## 为什么选择 XTunnel

- **统一发布**：HTTP、WebSocket、SSH 与通用 TCP 使用同一套 Tunnel、Service 和入口模型。
- **集中管理**：在 Web 控制台管理 Tunnel、Connector、Service、路由、状态与用量。
- **轻量 Agent**：Agent 只需要一枚 Connection Token，不维护本地业务配置。
- **多 Connector**：同一 Tunnel 可以运行多个 Connector，按健康状态和负载选择可用节点。
- **数据自持**：Server 单节点运行，业务状态存储在自己的 SQLite 数据目录中。
- **可观测可恢复**：提供 JSON 日志、Prometheus、OpenTelemetry、诊断、审计导出和备份恢复。

## 适合这些场景

- 从公网访问家庭实验室、NAS 或办公室里的 Web 服务。
- 为 SSH、数据库、消息服务和其他 TCP 协议提供固定入口。
- 给开发、测试和内部工具提供无需公网 IP 的临时或长期访问路径。
- 在多个内网节点运行 Connector，降低单个 Agent 离线带来的影响。

## 快速开始

当前没有正式 Release。下面的流程用于从源码体验开发预览版；生产化之前请先阅读
[V0.1 技术方案](docs/xtunnel_standalone_v0.1.md)与
[当前开发计划](docs/xtunnel_standalone_v0.1_development_plan.md)。

### 1. 构建 Server 与 Agent

Server 支持 Linux `amd64` / `arm64`，并提供 Windows `amd64` Preview（NTFS）。
构建环境需要 Go `1.27.1` 或更新的 `1.27.x` 补丁版、Node 24.19.0 与 npm 11.17.0。
以下命令均在仓库根目录执行。

**Linux：**

```sh
export GOTOOLCHAIN=local
./tools/check-go-version.sh
npm --prefix web ci
npm --prefix web run check
npm --prefix web run build
mkdir -p bin
go build -trimpath -o bin/xtunnel-server ./cmd/server
go build -trimpath -o bin/xtunnel-agent ./cmd/agent
```

**Windows PowerShell：**

```powershell
$env:GOTOOLCHAIN = 'local'
.\tools\check-go-version.ps1
npm --prefix web ci
npm --prefix web run check
npm --prefix web run build
go build -trimpath -o server.exe ./cmd/server
go build -trimpath -o agent.exe ./cmd/agent
```

每条命令成功后再执行下一条；Web 构建产物会嵌入 Server。

### 2. 准备并安装 Server

#### Linux systemd

复制完整配置示例，至少替换 `admin.example.com` 与 `tunnel.example.com`：

```sh
cp configs/server.example.yaml server.yaml
sudo ./bin/xtunnel-server service install --config "$PWD/server.yaml"
sudo /usr/local/bin/xtunnel-server admin create \
  --username admin \
  --config /etc/xtunnel/server.yaml
```

Management 与 HTTP Ingress 默认只监听 Loopback。使用仓库提供的
[Caddy / Nginx HTTPS/WSS 示例](deploy/reverse-proxy/README.md)终止公网 TLS，
然后通过 `management.public_url` 打开 Web 控制台。

#### Windows 前台运行

首次部署时复制完整示例；已有配置可直接编辑：

```powershell
Copy-Item .\configs\server.windows.example.yaml .\configs\server.yaml
```

在该文件中修改下面几个字段，其余配置保留。这个示例适用于 Server 与 Agent 在同一台电脑：

```yaml
management:
  listen: "127.0.0.1:8080"
  public_url: ""

agent_gateway:
  listen: "0.0.0.0:7443"
  public_hostname: "127.0.0.1:7443"
```

- `management.public_url` 可以留空：浏览器直接访问 `http://127.0.0.1:8080`，
  `management.listen` 必须为 Loopback IP。通过远程域名访问时，填写实际 HTTPS Origin，
  并按 [反向代理示例](deploy/reverse-proxy/README.md)配置 HTTPS。
- `agent_gateway.public_hostname` 必须填写 Agent 可达的域名或 IP，可以附端口。
  Agent 在其他电脑时，将 `127.0.0.1` 换成 Server 的局域网或公网地址，
  并确保防火墙及端口映射允许连接。IPv6 带端口写成 `[2001:db8::10]:7443`；
  省略端口时沿用 Gateway 监听端口。
- `listen` 是绑定地址，`0.0.0.0` 表示监听所有 IPv4 网卡；
  `public_hostname` 是写入 Connection Token 的连接地址。

**首次运行按以下顺序操作：**

1. 初始化当前用户的数据目录和运行目录：

   ```powershell
   .\server.exe init --config .\configs\server.yaml
   ```

2. 首次启动，建立数据库和 Gateway 身份：

   ```powershell
   .\server.exe --config .\configs\server.yaml
   ```

   等待 `process_started` 日志。此时管理页面提示初始化管理员（`SETUP_REQUIRED`），
   仅管理端和 Metrics 监听。按 `Ctrl+C` 停止，等待进程退出后执行下一步。

3. 创建首个管理员。**账号名由你指定，密码在终端提示后输入，不会回显：**

   ```powershell
   .\server.exe admin create --config .\configs\server.yaml --username admin
   ```

   请在 PowerShell 或 Windows Terminal 的交互终端中执行。重定向输入或自动化运行时，
   使用 `--password-file` 指定由你保护的密码文件。

   XTunnel 的初始管理员由这个命令创建。务必先完成上一步首次启动；
   创建管理员时 Windows 前台 Server 必须已经停止。

4. 再次启动：

   ```powershell
   .\server.exe --config .\configs\server.yaml
   ```

   打开 [本机 Web 控制台](http://127.0.0.1:8080)，使用 `admin` 和刚设置的密码登录。
   之后日常运行只需执行本步骤；配置变更后重启生效。

初始化和运行命令均使用同一个 Windows 用户及配置文件。前台数据位于当前用户
`%LOCALAPPDATA%\XTunnel\Server`，服务模式使用独立数据目录。
作为 Windows 服务运行时，按 [Windows Server SCM](deploy/windows-server/README.md)
中的安装、停服创建管理员及启动步骤操作。

Server 在各入口成功绑定后输出 `info` 级别的 `listener_started` JSON 日志，
`listener` 标识入口，`address` 给出实际监听地址与端口。管理入口还记录
`public_url`，用于打开 Web 控制台。首次管理员创建前仅记录 Management 和 Metrics；
HTTP Ingress、Agent Gateway 及已配置的 TCP 转发端口在管理员初始化后启动时记录。

### 3. 创建 Tunnel 并连接 Agent

进入 Web 控制台的“服务与隧道”，点击“创建 Tunnel”。第一步填写名称并点击
“创建并继续”；第二步选择平台并复制安装命令，在源站设备运行 Agent。页面底部每
3 秒自动刷新已连接的连接器，接入后可点击“下一步：添加服务”，也可点击“完成”稍后配置。
添加服务页面依次填写名称、公网入口与源站服务；连接超时、TLS、Host Header 和健康检查
位于“高级设置”。HTTP 路径使用前缀匹配，TCP 公网端口可以留空自动分配。
详情页的“概览”展示基本信息和连接器表格，“服务”页签管理服务；点击“安装连接器”
可再次打开安装抽屉。
Server 会将完整 Connection Token 加密持久保存；只要它仍有效，就可以重新打开
“安装连接器”获取同一枚 Token，为其他机器安装 Agent。Rotate 后获取的是新一代 Token，
Revoke 后不能再获取或使用已撤销的 Token 建立新认证。

以下命令中的 `xta_...` 仅表示占位符，`--token` 必须传入从控制台复制的完整值：

```sh
# Linux systemd；安装器会把 Token 保存为 root-only Credential。
sudo ./bin/xtunnel-agent service install --token 'xta_...'
```

```powershell
# Windows amd64 / arm64；请在提升权限的 PowerShell 中执行。
# 安装器会使用 DPAPI Machine-scope 加密 Token。
.\agent.exe service install --token 'xta_...'
```

回到该 Tunnel 的“服务”页签创建 Service，填写内网 Origin，再选择 HTTP 或 TCP 公网入口。

Connection Token 内含签发时的 Gateway IP 或 DNS 名称、端口及 TLS 信任信息，Agent
据此连接 Server。地址来自 `agent_gateway.public_hostname`：可以填写 `IP:端口`、
`域名:端口` 或 `[IPv6]:端口`；省略端口时使用 `agent_gateway.listen` 的端口。
管理页面使用的 `management.listen`（示例为 `127.0.0.1:8080`）不用于 Agent 连接。
修改 Gateway 对外地址后，先重启 Server 使配置生效，再对受影响 Tunnel 执行 Rotate，
重新获取 Token 并更新 Agent；重复获取原 Token 不会改写其中的旧地址。

> [!IMPORTANT]
> Connection Token 同时包含连接地址、TLS 信任、Tunnel 身份与认证 Secret。
> 不要把真实 Token 提交到 Git、粘贴到工单，或长期保留在共享 Shell History 中。

## 配置：Server 完整，Agent 极简

- [Server 完整注释示例](configs/server.example.yaml)：覆盖当前 Schema 中全部可同时生效的字段。
- [Agent Bootstrap 环境模板](configs/agent-bootstrap.env.example)：只展示 Token 注入方式，
  不会被 Agent 自动读取，也不是本地业务配置。
- [配置说明](configs/README.md)：字段权威、覆盖优先级、TLS 模式、Secret 与平台 Credential 边界。

Server 使用 Strict YAML，覆盖优先级固定为：

```text
CLI --set > XTUNNEL_* 环境变量 > YAML > Schema 默认值
```

字段、类型、范围与默认值的唯一机器权威是
[`configs/server.schema.json`](configs/server.schema.json)。V0.1 的 Server 主配置不支持热加载。

Agent 刻意不提供 YAML、`--config` 或本地业务 Schema。Service、Origin、Health Policy
都由 Server 通过已认证的 Control Session 下发，并只在 Agent 内存中应用。

## 部署选择

- **Linux Binary + systemd**：Server 与 Agent 都由 Binary 自身执行 `service install/uninstall`。
- **Windows SCM**：Agent 以 `LocalService` 运行，Token 使用 DPAPI Machine-scope 加密。
  Server 提供 amd64 + NTFS Preview，以 `LocalService + Service SID` 运行；
  安装维护与候选验收步骤见 [Windows Server SCM](deploy/windows-server/README.md)。
- **OCI / Compose**：参见 [`deploy/docker`](deploy/docker) 与
  [双栈 Compose 模板](deploy/docker/compose.dualstack.yaml)。
- **公网 HTTPS / WSS**：参见 [Caddy / Nginx 前置代理示例](deploy/reverse-proxy/README.md)。

Windows Server Preview 的 `backup create`、`backup restore` 在停服后也不可用；
当前使用维护窗口中的主机或基础设施级备份，具体边界见
[Windows Server SCM](deploy/windows-server/README.md#备份与恢复边界)。

证书、诊断、日志、指标、审计导出及平台维护边界见
[运维诊断 Runbook](docs/operations_runbook.md)。

## 当前边界

XTunnel Standalone V0.1 聚焦单 Server、自托管和反向代理。当前不提供 UDP、QUIC、
HTTP/3、Server 集群、多租户、RBAC/OIDC、WAF/CDN、L3 VPN 或 Agent 自动升级。
完整边界以 [V0.1 技术方案](docs/xtunnel_standalone_v0.1.md)为准。

| 组件 | 当前支持平台 |
| --- | --- |
| Server | Linux amd64 / arm64；Windows amd64 Preview（NTFS） |
| Agent | Linux amd64 / arm64；Windows amd64 / arm64 |
| OCI | Linux amd64 / arm64 |

Windows Server arm64 仅保持构建兼容性。Windows Server Preview 的维护范围和验收证据见
[Windows Server SCM](deploy/windows-server/README.md)。

## 开发与贡献

实现、契约与测试采用证据驱动的里程碑流程。开始开发前请先阅读：

- [V0.1 完整技术方案](docs/xtunnel_standalone_v0.1.md)
- [开发计划、任务状态与验收证据](docs/xtunnel_standalone_v0.1_development_plan.md)
- [代码评审标准](docs/code_review_standard.md)

## License

XTunnel 使用 [GNU General Public License v3.0](LICENSE)。
