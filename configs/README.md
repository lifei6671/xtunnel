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
- `server.windows.example.yaml`：Windows 前台与受管服务的路径、安全边界示例。

Windows 示例不会改变 `server.schema.json` 中面向现有 Linux 部署的默认值，也不会被
Server 自动发现。Windows 的配置加载及后续原生运行必须显式传入配置文件：

```powershell
.\xtunnel-server.exe --config .\server.windows.yaml
```

Windows 前台配置默认使用 `server.data_dir: auto`。它只经 Windows Known Folder API 解析为
当前登录用户的 `%LOCALAPPDATA%\XTunnel\Server\data`，同级 Runtime 目录用于该用户的锁和
本机运行资源。前台与服务不共用 SQLite、密钥、Journal 或锁。受管 Windows
服务及固定配置的离线维护入口将
`auto` 解析为 `%ProgramData%\XTunnel\Server\data`；`XTunnelServer` Service SID
对配置文件只有读取权限，SYSTEM 与 Administrators 保留完全控制，Data/Runtime 则仅授予
Service SID Modify（不含更改 DACL 或 owner）。不要用 LocalService 组权限、用户目录 ACL 或
Config 可写权限替代这组精确边界。
Data/Runtime 的 OWNER RIGHTS 限权 ACE 抑制文件 Owner 隐含的改 ACL 权限。
安装根保持 Protected DACL；服务运行时子目录、锁和密钥继承直接父目录的精确权限，
在使用前校验 Owner、对象身份与完整继承 ACL。
服务安装、首个管理员创建和维护步骤见 [Windows Server SCM](../deploy/windows-server/README.md)。

Windows Server amd64 Preview 已通过 M8-05 Gate 和阶段验收，本配置适用于本机 NTFS
上的前台与 SCM 部署；Windows Server arm64 仅保持构建兼容性。

至少需要替换：

- `management.public_url`：浏览器实际访问的 HTTPS Origin；本机访问可留空，此时监听地址必须是 Loopback IP，通过 `http://127.0.0.1:8080` 等对应地址访问。
- `agent_gateway.public_hostname`：Agent 可连接的域名或 IP，可附端口，如 `192.0.2.10:7443` 或 `[2001:db8::10]:7443`；省略端口时沿用 Gateway 监听端口。填写 Agent 实际能到达的地址。
- Windows 前台部署应保留 `server.data_dir: auto`，并为当前用户 Profile 配置受限 ACL；不要将
  `%LOCALAPPDATA%` 作为普通环境变量字符串手工展开或把用户目录传给服务安装器。

Windows 手工前台运行前，先使用已校验的配置显式准备 Data 和 Runtime 目录：

```powershell
.\xtunnel-server.exe init --config .\server.windows.yaml
```

`init` 准备或复核受管目录及 External Lock，不启动 Server、SQLite 或 Listener；
日常启动仍要求目录已存在。每次取得锁前都通过同一 no-follow Handle 验证 Runtime
目录和锁文件的 Owner/Protected DACL，新锁在创建时设置受管权限。旧版本或手工创建的
锁文件若不符合该权限边界，启动及 `init` 都会拒绝并保留现场，不会自动修改 ACL/owner。
Linux systemd 自安装会创建默认的 `/var/lib/xtunnel/data`：

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
删除或更改 DACL/owner，私钥还不得向这些主体开放读取。Server 验证可信 owner、有效安全性质与
文件身份，不要求精确 ACE 集合，也不会改写证书管理器维护的 ACL。Linux 会精确要求 `key_file` 的权限为
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
