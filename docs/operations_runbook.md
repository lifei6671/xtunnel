# XTunnel V0.1 运维诊断 Runbook

本文档面向 XTunnel Standalone V0.1 的值班与故障处理。命令以当前公开 CLI、
Management API、systemd Unit 和 Windows SCM 契约为准。不要把 Connection Token、
Admin Session Cookie、密码、私钥或完整配置粘贴进工单、日志或共享终端历史。

## 1. 先判断 Agent 是否具备连接条件

`diagnose` 是无副作用 Precheck：它依次验证 Connection Token、Endpoint、DNS、TCP、
TLS、Public CA 或 Pinned SPKI，以及 ALPN，并输出每步的 `PASS/WARNING/FAIL` 和最终
`READY/READY_DEGRADED/NOT_READY`。它不会执行 Auth，不会登记或替换 Connector
Session，也不会接收或应用 Snapshot；因此 `READY` 不等于业务 Session 已上线。

Linux/macOS 的 Bash 中可交互读取 Token，避免明文进入 shell history：

```bash
IFS= read -r -s XTUNNEL_TOKEN
export XTUNNEL_TOKEN
xtunnel-agent diagnose
unset XTUNNEL_TOKEN
```

PowerShell：

```powershell
$secureToken = Read-Host 'Connection Token' -AsSecureString
$tokenPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureToken)
try {
    $env:XTUNNEL_TOKEN = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($tokenPointer)
    xtunnel-agent diagnose
} finally {
    Remove-Item Env:XTUNNEL_TOKEN -ErrorAction SilentlyContinue
    [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($tokenPointer)
}
```

判断顺序：

1. Token 解析失败：从 Management UI/API 重新取得该 Tunnel 当前 Token，禁止手工改写。
2. DNS/TCP 失败：核对 Token 内 Endpoint、DNS 和防火墙；不要增加旁路 Endpoint。
3. TLS/Pin 失败：核对 Server 当前证书和 Token 内 Trust Descriptor；禁止 TOFU、
   `--insecure` 或自动接受新 Key。
4. ALPN 失败：确认链路没有由反向代理终止或改写 Agent Gateway TLS。
5. Precheck 为 `READY` 但 Connector Offline：继续查看 Agent/Server 生命周期日志和
   Dashboard；这是 Auth、Session 或 Snapshot 阶段问题，不属于 `diagnose` 的证明范围。

## 2. 证书到期与续签

Prometheus 指标 `xtunnel_gateway_certificate_expiry_seconds` 是当前热加载叶证书的 Unix
到期时间。规则文件 `deploy/monitoring/prometheus/xtunnel-alerts-v1.yaml` 只消费该指标，
使用 `time()` 计算非重叠的 30 天、7 天、1 天和已过期窗口。把该文件加入 Prometheus
的 `rule_files` 后，通过 Prometheus 自身的配置重载流程发布；不要复制表达式维护第二份。

```bash
curl --fail --silent http://127.0.0.1:9090/metrics \
  | grep '^xtunnel_gateway_certificate_expiry_seconds '
```

- `pinned` 模式：Server 在进入 30 天窗口后复用原私钥自动续签，SPKI Pin 不变。
  成功事件为 `gateway_certificate_renewed`，包含旧、新到期 Unix 时间；失败事件为
  `gateway_certificate_renewal_failed`，`error_code=CERTIFICATE_RENEWAL_FAILED`，旧有效
  身份继续服务。失败时先检查磁盘权限、空间、只读挂载和 Backup Barrier。
- `public` 模式：指标和告警仍有效，但证书续签归外部证书管理系统所有。替换外部证书后
  按既有发布流程重启 Server 并确认指标更新。
- 必须更换 Pinned Private Key/SPKI 时，只能在离线维护窗口执行
  `xtunnel-server gateway rotate-key --maintenance`，然后为每个 Tunnel Rotate Token、
  重新部署 Token 并重启 Agent。V0.1 不支持在线双 Pin 轮换。

## 3. 查询和导出 Security Audit

页面查询和 API 都使用管理员 Session Cookie。GET 导出不需要 CSRF Header；响应为
`application/x-ndjson`、`Cache-Control: no-store`。Server 在响应开始前固定导出上界，
因此并发新增事件不会混入本次文件。连接中断或 `curl` 非零退出时必须删除候选文件，
不能把部分 NDJSON 当成完整导出。

以下示例交互读取 Cookie，历史中只留下变量名。导出支持 `action`、`result`、
`resource_type`、`resource_id`、`occurred_from`（包含）和 `occurred_to`（不包含）筛选：

```bash
IFS= read -r -s XTUNNEL_ADMIN_SESSION
export XTUNNEL_ADMIN_SESSION
management_url=https://admin.example.com
output=xtunnel-security-audit.ndjson
candidate=$(mktemp "${output}.tmp.XXXXXX")
if curl --fail --silent --show-error \
  --cookie "xtunnel_admin_session=${XTUNNEL_ADMIN_SESSION}" \
  --get "${management_url}/api/v1/security-audit-events/export" \
  --data-urlencode 'result=FAILED' \
  --data-urlencode 'occurred_from=2026-08-30T00:00:00Z' \
  --output "$candidate"; then
  chmod 0600 "$candidate"
  mv -- "$candidate" "$output"
else
  rm -f -- "$candidate"
fi
unset XTUNNEL_ADMIN_SESSION
```

不要使用 `-k` 跳过 Management TLS 校验。若部署的 Management URL 不是示例地址，使用
配置中的 `management.public_url`；Cookie 是 Host-only，不能跨主机复用。

## 4. Linux systemd

服务名为 `xtunnel-server.service` 和 `xtunnel-agent.service`：

```sh
systemctl status xtunnel-server.service xtunnel-agent.service
systemctl show xtunnel-server.service \
  --property=ActiveState,SubState,Result,ExecMainStatus,NRestarts,TimeoutStopUSec
journalctl -u xtunnel-server.service -u xtunnel-agent.service \
  --since '30 minutes ago' --output=short-iso
```

- 启动失败：`Result=exit-code` 和非零 `ExecMainStatus` 是服务管理器级事实；在 Journal 中从
  `process_started` 前后的首个错误定位配置、权限、端口或数据目录问题。
- 恢复重启：Unit 使用 `Restart=on-failure`、`RestartSec=5s`。核对 `NRestarts` 增长、
  MainPID 变化，以及新的 `process_started`。不要靠无限重启掩盖稳定启动错误。
- Stop/Shutdown：先看 `TimeoutStopUSec` 的实际值。生产 Unit 当前没有改写
  `TimeoutStopSec`，沿用 systemd Manager 默认值；应用仍应在自身有界 Graceful
  Shutdown 内收敛。出现 `Result=timeout`、`stop-sigterm timed out` 或残留进程时，保留
  Journal、MainPID 和时间线，再检查连接 owner 与关闭路径。不要为了让告警消失而直接
  放大生产超时。

仓库的 systemd Smoke 只允许在没有既有 XTunnel 路径、Unit 或服务身份的隔离 Runner
运行。它用无效配置验证启动失败，用 `SIGKILL` 验证恢复重启，并以临时 runtime drop-in
把 `KillSignal` 改为不终止进程的 `SIGCONT`，验证 systemd 超时诊断；退出时删除该
drop-in 并恢复正常服务。生产 Unit 文件不因此改变 `TimeoutStopSec` 或 `KillSignal`。

## 5. Windows SCM

Agent 服务名和 Event Log Provider 都是 `XTunnelAgent`：

```powershell
Get-CimInstance Win32_Service -Filter "Name='XTunnelAgent'" |
    Select-Object Name, State, ProcessId, ExitCode, StartMode, StartName, PathName
sc.exe qfailure XTunnelAgent
Get-WinEvent -FilterHashtable @{
    LogName='Application'
    ProviderName='XTunnelAgent'
    StartTime=(Get-Date).AddMinutes(-30)
} | Select-Object TimeCreated, Id, LevelDisplayName, Message
```

- 启动失败：`windows_service_failed` 的 `error_code=CREDENTIAL_LOAD_FAILED` 表示 DPAPI
  Credential 无法读取/解密；不要把明文 Token 写进 ImagePath 或日志。
- 恢复重启：安装器配置 5 秒后重启，并启用 non-crash failure recovery。核对失败事件、
  新 `windows_service_running` 事件与变化后的 ProcessId。
- Stop/Shutdown：`windows_service_stop_requested` 后应出现 `windows_service_stopped`。
  若 30 秒内 Runtime 未退出，服务写入 `windows_service_failed` 和
  `error_code=STOP_TIMEOUT`，以非零码退出并交由 SCM recovery。收集 Event Log、SCM
  状态和时间线后再分析阻塞 owner。

Windows CI Smoke 会在隔离 Runner 中损坏后恢复 DPAPI 候选副本，验证真实启动失败和
SCM 自动恢复。M6 Gate 另构建独立的 Windows-only SCM Helper，临时把隔离 Runner 上
同一受管 Service 的 ImagePath 切换到 Helper：首次 `runtime-failure` 让回调返回错误，首次
`stop-timeout` 忽略取消并真实等待生产 Handler 的 30 秒上界；Marker 让 SCM recovery
的第二进程稳定等待取消。Smoke 必须从 Application Event Log 定位 `RUNTIME_FAILED`、
`STOP_TIMEOUT`，验证非零 Service Exit、29—35 秒 Stop 窗口、恢复新 PID 和 Token 不泄漏，
并在每轮 `finally` 恢复生产 ImagePath、停止 Helper、重新启动原 Agent。Helper 只属于
CI Gate，不进入生产 Binary、用户 CLI 或持久 SCM 配置；普通 Stop 或 Handler 单测仍不能
冒充该真实 Gate。

## 6. 证据边界

告警 YAML、Runbook、单元测试、交叉编译或普通 Stop 只能证明各自范围。M6-07/M6 Gate
仍需绑定当前提交的 Linux amd64/arm64 原生联合黑盒、Race、提升权限 Windows SCM
持久 Event Log Smoke 和精确 CI；既有 M6-06 平台证据不能替代本 Gate。
