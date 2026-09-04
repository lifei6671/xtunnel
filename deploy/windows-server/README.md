# Windows Server SCM

本入口用于 M8-04 的 Windows amd64 + NTFS 服务实现与开发验收。Windows Server
发布支持以 M8-05 Preview Gate 为准，任务证据见[开发计划](../../docs/xtunnel_standalone_v0.1_development_plan.md)。

## 安装与首个管理员

在提升权限的 PowerShell 中，从外部部署目录运行 `xtunnel-server.exe`。先复制并编辑
[完整配置示例](../../configs/server.windows.example.yaml)，设置公网域名、管理 Origin 和监听地址。
`server.data_dir: auto` 在服务中解析到固定 ProgramData Profile。

```powershell
.\xtunnel-server.exe service install --config .\server.windows.yaml
Stop-Service XTunnelServer
.\xtunnel-server.exe admin create --config "$env:ProgramData\XTunnel\server.yaml" --username admin
Start-Service XTunnelServer
```

`admin create` 在交互终端安全读取密码，也可使用 `--password-file` 指定管理员自行保护的
密码文件。该命令核验服务已停止，再取得与服务相同的外部锁并提交 SQLite 事务。尚无管理员时，
服务仅启动 Management/Metrics；创建后重启才开放 HTTP、TCP 与 Agent Gateway。

安装前先完成平台、权限、配置、NTFS、对象类型和受管身份预检。固定布局为：

| 对象 | 路径 | Service SID 权限 |
| --- | --- | --- |
| Binary | `%ProgramFiles%\XTunnel\xtunnel-server.exe` | Read & Execute |
| Config | `%ProgramData%\XTunnel\server.yaml` | Read |
| Data | `%ProgramData%\XTunnel\Server\data` | Modify |
| Runtime | `%ProgramData%\XTunnel\Server\runtime` | Modify |

服务以 LocalService 运行，权限授予精确的 `NT SERVICE\XTunnelServer` SID。
SYSTEM/Administrators 保留完全控制；Data/Runtime 的 OWNER RIGHTS 限权防止通过 Owner
隐含权限更改 ACL。配置、密钥、数据库和锁与前台 LocalAppData Profile 分离。
同名服务、事件源或文件属性不符合受管契约时，命令返回错误并保留现场。

## 运行与维护

```powershell
Get-Service XTunnelServer
Stop-Service XTunnelServer
Start-Service XTunnelServer
Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='XTunnelServer'} -MaxEvents 20
```

SCM 就绪对应配置、存储和该启动状态所需 Listener 已完成初始化。Stop/Shutdown 停止新入口，
在 30 秒排空期限到达后主动关闭残留 Socket，再等待资源 owner、数据库和锁收敛。
异常退出通过非零 Service Exit 报告，SCM 最多执行两次间隔 5 秒的恢复重启；计数重置周期为一天。
Application Event Log 中的记录使用共享 JSON 日志格式。

升级在维护窗口中停止服务，由外部部署工具替换 Binary/Config 并保持其受管 Owner/DACL，
再启动验证。`service install` 拒绝直接覆盖已安装服务。Public TLS 的证书和私钥由管理员或
证书管理器维护，安装与启动只校验，不修改外部文件权限。

卸载同样使用外部部署目录中的程序：

```powershell
.\xtunnel-server.exe service uninstall
```

卸载停止服务，删除受管 Binary、SCM 对象和 Event Source，保留 Config/Data/Runtime 及权限。
同名重装使用相同 Service SID；安装源需与保留配置一致。所有安装对象不完整或发生身份漂移时，
先由管理员核查现场，再按维护流程处理。

## 隔离 Runner 验证

`smoke.ps1` 只用于无既有 XTunnelServer 对象的一次性提升权限 Windows amd64 Runner。
它会安装服务、创建随机测试管理员、验证失败恢复、读取日志并卸载重装，保留测试数据。

```powershell
.\deploy\windows-server\smoke.ps1 -ServerPath "$env:RUNNER_TEMP\xtunnel-server.exe" -ConfigPath .\configs\server.windows.example.yaml
```

CI 随后显式运行 `TestServiceTokenIsolation`，用真实 SCM LocalService 令牌验证同服务与
其他服务的权限边界。普通单测不执行这项主机操作；原生 Smoke 与令牌证据不能由交叉编译替代。

CI 首次安装启动失败时，可通过 `-DiagnosticPath` 指定 `internal/server/bootstrap`
构建的测试程序。脚本在停止服务后临时替换 Binary，读取服务回调返回前写入的诊断事件，
再还原候选；原安装失败仍返回失败。此入口仅用于固定测试配置、没有真实凭据的一次性 Runner。
