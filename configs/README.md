# XTunnel 配置示例

本目录提供 Server 的可加载 YAML 示例，以及 Agent 的 Token Bootstrap 输入模板。
它们用于帮助部署，不是新的配置权威；字段、类型、范围、默认值、Secret 标记和
热加载属性始终以 `server.schema.json` 为准。

## Server

Linux 与 Windows 示例都完整覆盖当前 pinned TLS 模式可同时生效的全部 Schema 字段：

```sh
cp configs/server.example.yaml server.yaml
```

```powershell
Copy-Item .\configs\server.windows.example.yaml .\server.windows.yaml
```

- `server.example.yaml`：Linux systemd、OCI 或前台运行示例。
- `server.windows.example.yaml`：Windows 原生路径与安全边界示例。

Windows 示例不会改变 `server.schema.json` 中面向现有 Linux 部署的默认值，也不会被
Server 自动发现。Windows 的配置加载及后续原生运行必须显式传入配置文件：

```powershell
.\xtunnel-server.exe --config .\server.windows.yaml
```

受管 Windows 部署中，`XTunnelServer` Service SID 对配置文件只有读取权限；SYSTEM 与
Administrators 保留完全控制。Data/Runtime 目录对 Service SID 授予 Modify，但不允许其
更改 DACL 或 owner。不要用 LocalService 组权限或 Config 可写权限替代这组精确边界。

在 M8-06 的 Windows 原生运行与发布验证完成前，该文件只代表构建和配置契约基线，
不代表 Windows Server 已进入正式支持矩阵。

至少需要替换：

- `management.public_url`：浏览器实际访问的 HTTPS Origin。
- `agent_gateway.public_hostname`：Agent 可从公网连接的 Gateway 主机名。
- Windows 部署还应确认 `server.data_dir` 使用带盘符的绝对路径，并为运行 Server 的
  身份配置受限 ACL。

若手工前台运行，还需要预先创建 `server.data_dir`。Linux systemd 自安装会创建
默认的 `/var/lib/xtunnel/data`：

```sh
sudo xtunnel-server service install --config "$PWD/server.yaml"
```

Server 使用 Strict YAML。未知字段、重复 Key、多个 YAML Document、错误类型和
跨字段约束都会让启动失败。覆盖优先级固定为：

```text
CLI --set > XTUNNEL_* 环境变量 > YAML > Schema 默认值
```

环境变量由点分路径转换为大写双下划线，例如：

```text
management.public_url
→ XTUNNEL_MANAGEMENT__PUBLIC_URL
```

数组覆盖值必须使用 JSON Array。CLI `--set` 可以重复；同一路径以后出现的值为准：

```sh
xtunnel-server \
  --config ./server.yaml \
  --set logging.level=debug \
  --set 'management.allowed_hosts=["admin-alt.example.com"]'
```

V0.1 的 Server 主配置全部为 `x-reloadable: false`，修改后必须显式重启。

### Gateway TLS 模式

完整示例默认使用 `pinned`：

```yaml
agent_gateway:
  tls:
    mode: pinned
```

Server 在安全数据目录中创建和维护 Pinned Identity。Connection Token 会包含对应的
SPKI Pin；更换 Private Key 后必须重新签发 Token。

使用公共 CA 或系统信任链证书时，切换为 `public` 并启用证书路径：

```yaml
agent_gateway:
  tls:
    mode: public
    cert_file: /etc/xtunnel/tls/tunnel.crt
    key_file: /etc/xtunnel/tls/tunnel.key
```

Windows 路径应使用单引号保留反斜杠；下面仍然只是路径，不包含证书或私钥内容：

```yaml
agent_gateway:
  tls:
    mode: public
    cert_file: 'C:\ProgramData\XTunnel\Server\tls\tunnel.crt'
    key_file: 'C:\ProgramData\XTunnel\Server\tls\tunnel.key'
```

`pinned` 模式禁止出现 `cert_file` 和 `key_file`。路径本身不是 Secret，但私钥内容是
Secret；不要把证书私钥写进 YAML、Git、日志或备份说明。Windows 外部私钥必须位于
本机固定卷的普通文件中，拒绝 Reparse Point，并允许 `NT SERVICE\XTunnelServer` 读取；
证书可以向普通用户开放读取，但文件和父目录不得向普通用户或其他非授权服务开放写入、
删除或更改 DACL/owner，私钥还不得向这些主体开放读取。Server 只验证 operator-owned 文件的 owner、
DACL 与文件身份，不会改写证书管理器维护的 ACL。Linux 会精确要求 `key_file` 的权限为
`0600`，其他权限（包括 `0400`、`0640`）都会让 Server 启动失败：

```sh
chmod 600 /etc/xtunnel/tls/tunnel.key
```

### 可信代理

`management.trusted_proxies` 与 `http_ingress.trusted_proxies` 只应包含直接连接到
对应 Listener 的真实反向代理 CIDR。不要为了“省事”填写 `0.0.0.0/0`、`::/0` 或
整个内网，否则外部客户端可能伪造 Forwarded 元数据。

## Agent

Agent 刻意没有 YAML、本地业务 Schema 或 Desired State 文件。它唯一的 Bootstrap
输入是一枚完整 Connection Token；Token 已经包含 Endpoint、TLS Trust、Tunnel/Token
Identity 与认证 Secret。

[`agent-bootstrap.env.example`](agent-bootstrap.env.example) 只说明环境变量名称，
不会被 Agent 自动加载。直接前台运行时可把 Token 注入当前进程环境：

```sh
XTUNNEL_TOKEN='xta_...' xtunnel-agent run
```

无副作用连接诊断使用相同来源：

```sh
XTUNNEL_TOKEN='xta_...' xtunnel-agent diagnose
```

原生生产服务应让安装器保存平台 Credential：

```sh
# Linux systemd：root-only Credential + LoadCredential
sudo xtunnel-agent service install --token 'xta_...'
```

```powershell
# Windows SCM：DPAPI Machine-scope + 受限 ACL
.\xtunnel-agent.exe service install --token 'xta_...'
```

Docker Compose 接收宿主变量 `XTUNNEL_AGENT_TOKEN`，再映射为容器内
`XTUNNEL_TOKEN`。不要把真实 Token 直接写进 Compose YAML。

## Secret 安全

- 不要提交真实 `xta_...` Token、私钥、管理员密码或 Session Cookie。
- 不要把 Token 粘贴到日志、工单、聊天记录或公开终端输出。
- `--token` 适合一次性安装输入；共享环境中应注意 Shell History 和进程参数可见性。
- Linux Credential 不能包含注释、引号、空白或末尾换行。
- Windows DPAPI Blob 不能跨机器复制，也不是可手工编辑的配置文件。
- Token 泄露或人员权限变化后，在管理端 Rotate 或 Revoke，并重新安装 Agent Credential。

更多证书、诊断、备份和故障处置见
[运维诊断 Runbook](../docs/operations_runbook.md)。
