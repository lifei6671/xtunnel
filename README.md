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

Server 只支持 Linux `amd64` / `arm64`。构建环境需要 Go `1.27.1` 或更新的 `1.27.x` 补丁版、
Node 24.19.0 与 npm 11.17.0：

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

### 2. 准备并安装 Server

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

### 3. 创建 Tunnel 并连接 Agent

在 Web 控制台创建 Tunnel，复制当前的 `xta_...` Connection Token，然后安装 Agent：

```sh
# Linux systemd；安装器会把 Token 保存为 root-only Credential。
sudo ./bin/xtunnel-agent service install --token 'xta_...'
```

```powershell
# Windows amd64 / arm64；请在提升权限的 PowerShell 中执行。
# 安装器会使用 DPAPI Machine-scope 加密 Token。
.\xtunnel-agent.exe service install --token 'xta_...'
```

回到 Web 控制台创建 Service，填写内网 Origin，再选择 HTTP 或 TCP 公网入口。

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
  Server 的 M8-04 开发验收与操作步骤见 [Windows Server SCM](deploy/windows-server/README.md)，
  Windows Server 支持矩阵待 M8-05 Preview Gate 完成后更新。
- **OCI / Compose**：参见 [`deploy/docker`](deploy/docker) 与
  [双栈 Compose 模板](deploy/docker/compose.dualstack.yaml)。
- **公网 HTTPS / WSS**：参见 [Caddy / Nginx 前置代理示例](deploy/reverse-proxy/README.md)。

完整的证书、诊断、日志、指标、审计导出、备份与恢复流程见
[运维诊断 Runbook](docs/operations_runbook.md)。

## 当前边界

XTunnel Standalone V0.1 聚焦单 Server、自托管和反向代理。当前不提供 UDP、QUIC、
HTTP/3、Server 集群、多租户、RBAC/OIDC、WAF/CDN、L3 VPN 或 Agent 自动升级。
完整边界以 [V0.1 技术方案](docs/xtunnel_standalone_v0.1.md)为准。

| 组件 | 当前支持平台 |
| --- | --- |
| Server | Linux amd64 / arm64 |
| Agent | Linux amd64 / arm64；Windows amd64 / arm64 |
| OCI | Linux amd64 / arm64 |

## 开发与贡献

实现、契约与测试采用证据驱动的里程碑流程。开始开发前请先阅读：

- [V0.1 完整技术方案](docs/xtunnel_standalone_v0.1.md)
- [开发计划、任务状态与验收证据](docs/xtunnel_standalone_v0.1_development_plan.md)
- [代码评审标准](docs/code_review_standard.md)

## License

XTunnel 使用 [GNU General Public License v3.0](LICENSE)。
