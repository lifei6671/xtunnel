# M7-02 Reconnect Storm Chaos Test

本目录提供 M7-02 的 Linux POSIX Shell 验收入口。Runner 只调度
`internal/server/bootstrap.TestM7ReconnectStorm`，不修改 Server 配置、Rate/Burst、
协议、生产默认值或 CI。

## 前置条件

- 运行环境必须是 Linux，且仓库中存在可执行的 `tools/check-go-version.sh`。
- 直接运行源码时，Linux 必须安装项目固定的 Go `go1.27.0`；Runner 会先设置
  `GOTOOLCHAIN=local` 并执行项目版本检查，不会下载或切换工具链。
- Linux 没有 Go 时，可在 Windows 使用项目固定工具链生成 `linux/amd64`、
  `GOAMD64=v1`、`CGO_ENABLED=0` 的 `bootstrap.test` 与 `manifest.txt`：

```powershell
./tests/chaos/build-m7-02-linux.ps1 -OutputDirectory C:\Temp\xtunnel-m7-02-bin
```

- 预编译目录的 Manifest 必须记录 `commit`、`worktree_clean`、`go_version`、
  `toolchain`、`goos`、`goarch`、`goamd64`、`cgo_enabled` 和
  `bootstrap_sha256`。Runner 会校验平台、工具链、当前 Commit 与 Binary SHA-256。
- `full` 模式要求当前工作区和预编译 Manifest 都是干净状态。预编译 Binary 会先复制到
  Linux-native `/tmp`，设置最小权限并再次校验 Manifest/Binary SHA-256，避免从 WSL
  DrvFS 长跑执行。
- 5000 Connector 档要求当前 Shell 的 Soft `ulimit -n` 至少为 `16384`；不足时
  Runner 失败，不会跳过该档或降低 Connector 数。测试 Binary 还会按实际 Server 与
  Client FD 预算核验 Hard Limit；当前 5000 control-only 档要求至少 `51216`，满足时才把
  Soft Limit 提升到该预算，Hard Limit 不足或提升失败都直接失败。

Runner 使用 `/tmp/xtunnel-m7-02.XXXXXX` 保存单档临时输出，并通过 Trap 精确清理；
它不会在仓库内生成原始结果。需要保留正式证据时，由调用方把标准输出和标准错误重定向到
仓库外的受控证据目录。

## 运行方式

源码路径的快速验证只运行 100 Connector，单进程超时为 2 分钟：

```sh
./tests/chaos/run-m7-02.sh -m smoke
```

使用 Windows 交叉编译 Binary 的快速验证：

```sh
./tests/chaos/run-m7-02.sh -m smoke -b /mnt/c/Temp/xtunnel-m7-02-bin
```

正式模式依次运行全部容量档：

```sh
./tests/chaos/run-m7-02.sh -m full
```

```sh
./tests/chaos/run-m7-02.sh -m full \
  -b /mnt/c/Temp/xtunnel-m7-02-bin
```

每档都会设置 `XTUNNEL_M7_02_CONNECTORS`，然后执行精确测试：

```text
100 Connector   -> 2m
500 Connector   -> 5m
1000 Connector  -> 10m
5000 Connector  -> 30m
```

源码模式执行：

```sh
go test ./internal/server/bootstrap \
  -run '^TestM7ReconnectStorm$' -count=1 -timeout='<tier-timeout>' -v
```

预编译模式执行同一测试 Binary 的等价 `-test.*` 参数。任一档超时或失败都会立即终止
后续档位并返回失败。

## 测试输出与证据边界

Go 测试负责读取 `XTUNNEL_M7_02_CONNECTORS`，并输出该档的 Reconnect Storm、
Pending TLS/Auth 观测、FD/CPU/RAM、Server Restart 四段恢复时间和 generation fencing
结果。完整 Runtime 档的数据面探测从 Startup/Recovery 阶段开始即与全量就绪等待并发，
因此 `T_first_success` 不等待 `T_workpool_ready` 尾部后才开始计时。Stagger/Jitter、永久错误 Backoff 与 `retry_after` 由
`internal/agent/reconnect` 的确定性规模单元测试独立证明；Chaos JSON 只记录该证据边界，
不重复输出这些单元测试指标。Runner 只负责固定输入、顺序、超时、工具链与 Binary 身份，
不重新计算或解释测试指标。

5000 control-only 档的 `peak_pending_tls_auth_upper_bound` 是 `/proc/net/tcp` 中 IPv4 Gateway
端 `SYN_RECV+ESTABLISHED` 减去已发布 Current Control 的观测上界；它还包含 AUTH 已完成但
尚未发布 Current 的连接，不能反向解释为内部 TLS/Auth Semaphore 的精确占用或突破配置上限。
完整 Runtime 档的同一 Socket 同时承载 WorkConn，因此只输出 mixed 语义，不把它作为
Pending TLS/Auth 数值。

- `smoke` 只证明 100 Connector 档在当前环境可执行，不覆盖 500/1000/5000，也不构成
  M7-02 正式验收。
- `full` 的四档成功只构成当前 Commit、当前 Linux 环境的 Chaos Test 证据；WSL2 结果必须
  明确标注为 WSL2，不能描述为原生裸机结果。
- Manifest 和 SHA-256 校验只证明实际执行的 Binary 与记录一致；交叉编译不等于 Linux
  原生 Runtime 验收，最终结论仍需结合完整输出、环境记录和独立复审。
- 本 Runner 不据测试数值自动调整 Gateway Rate/Burst、TLS Session Resumption、Server
  Schema/Repository 默认值或其他生产实现；这些变化需要单独审批。

## 退出码

- `0`：所选模式的所有档位通过，且临时文件已清理。
- `1`：参数、平台、工具链、工作区、Manifest/SHA-256、FD 前置检查、测试或清理失败。
- `129`、`130`、`143`：分别收到 `HUP`、`INT`、`TERM`，退出前仍会执行清理。

---

# M7-03 Graceful Shutdown Chaos Test

M7-03 Runner 调度 `internal/server/bootstrap.TestM7GracefulShutdownChaos`。测试从真实
公网 TCP/HTTP Listener 进入生产 Bootstrap、Gateway、Token-only Agent 与 Origin，覆盖：

- Server Drain 期间真实 TCP Half-Close 的反向尾部字节；
- HTTP Streaming 与 WebSocket 在 Graceful Period 内自然完成；
- TCP、HTTP Slow Origin、WebSocket 同时阻塞时的 Hard Deadline Force Close；
- Agent 发起两阶段 Drain 后保留既有 ACTIVE、拒绝新 OPEN；
- Agent 在真实 30 秒默认 Drain 窗口内保持 DRAINING Heartbeat，并在超时后强制关闭 ACTIVE；
- Session Snapshot 与全部运行时配额清零，以及 FD/goroutine 精确回到场景基线。

阶段栅栏由 Channel 和 Socket 事件驱动，主断言不依赖随机延迟。组件级的 DrainRequest/Ack
ID、旧 Ack、OPENING、Usage exactly-once、单边 EOF 和 SIGTERM 接线仍由各 owner 的定向
测试负责；本 Runner 不把单一产品级用例冒充这些组件证据。

## 构建与运行

直接在装有项目固定 Go `go1.27.0` 的 Linux 环境运行：

```sh
./tests/chaos/run-m7-03.sh -m smoke
./tests/chaos/run-m7-03.sh -m full
```

Linux 没有 Go 时，可在 Windows 使用项目固定工具链生成 `linux/amd64`、
`GOAMD64=v1`、`CGO_ENABLED=0` 的测试 Binary 与 Manifest：

```powershell
./tests/chaos/build-m7-03-linux.ps1 -OutputDirectory C:\Temp\xtunnel-m7-03-bin
```

然后在 Linux/WSL2 中运行：

```sh
./tests/chaos/run-m7-03.sh -m smoke -b /mnt/c/Temp/xtunnel-m7-03-bin
./tests/chaos/run-m7-03.sh -m full -b /mnt/c/Temp/xtunnel-m7-03-bin
```

`smoke` 只运行 TCP Half-Close 自然排空场景；`full` 执行完整场景矩阵。两种模式都把
Binary 和输出复制到 Linux-native `/tmp/xtunnel-m7-03.XXXXXX`，并在退出时精确清理。
`full` 还要求当前工作区与预编译 Manifest 均为 clean；Builder 的 `-AllowDirty` 只允许
生成开发 Smoke 产物。

## 证据边界

- Windows 交叉编译只证明 Linux Test Binary 可生成，不构成 Linux Runtime 证据。
- WSL2 是 Linux 内核运行证据，但不等于独立原生 Linux 主机或代表性网络条件。
- `smoke` 不覆盖完整矩阵，不能作为 M7-03 正式通过证据。
- `full` 只绑定 Manifest 中的 Commit、工具链、平台与 Binary SHA-256；仍需结合定向
  Test/Race/Vet、精确 CI 和独立交付复审。
- Runner 不调整 30 秒生产 Drain 默认值，不修改 Proto、Schema、CI、网络 namespace、
  `tc netem` 或 `nftables`；特权网络故障注入属于 M7-08。

---

# M7-04 Server Persistence/Filesystem Failpoints

M7-04 Runner 按 SQLite Migration、Gateway Rotation Journal、Backup/Restore 三个分区调度
现有测试。`smoke` 覆盖 SQLite 原生 `SQLITE_FULL` 回滚重试，以及 Gateway、Backup、
Restore 在 write/fsync/rename 边界的确定性故障注入；`full` 额外运行 Backup ACK 前
hard-exit、Gateway Key/Certificate rename 后真实子进程 `SIGKILL`、Restore 两次目录
切换后真实子进程 `SIGKILL`，以及 Restore interrupted-state 恢复矩阵。

直接在装有项目固定 Go `go1.27.0` 的 Linux 环境运行：

```sh
./tests/chaos/run-m7-04.sh -m smoke
./tests/chaos/run-m7-04.sh -m full
```

Linux 没有 Go 时，可在 Windows 生成三个 `linux/amd64`、`GOAMD64=v1`、
`CGO_ENABLED=0` Test Binary 与 Manifest：

```powershell
./tests/chaos/build-m7-04-linux.ps1 -OutputDirectory C:\Temp\xtunnel-m7-04-bin
```

然后在 Linux/WSL2 中运行：

```sh
./tests/chaos/run-m7-04.sh -m smoke -b /mnt/c/Temp/xtunnel-m7-04-bin
./tests/chaos/run-m7-04.sh -m full -b /mnt/c/Temp/xtunnel-m7-04-bin
```

Runner 校验 Manifest 中的 Commit、clean 状态、固定工具链、目标平台与三个 Binary 的
SHA-256，并把 Binary 和输出复制到 Linux-native `/tmp/xtunnel-m7-04.XXXXXX`；Trap 在
退出时精确清理。`full` 要求当前工作区和预编译 Manifest 都为 clean，Builder 的
`-AllowDirty` 只用于开发态 Smoke。

## M7-04 证据边界

- 确定性 Hook 证明应用在 syscall 边界收到 `EIO`/`ENOSPC` 后的状态收敛；它不等于
  Linux Kernel 或真实存储设备产生的 EIO，也不证明物理介质耐久性。
- SQLite 分区使用 SQLite 原生 `SQLITE_FULL`；Backup hard-exit 证明 ACK 前最终路径
  不可见；Gateway/Restore 子进程测试证明真实 `SIGKILL` 后可按 Journal 收敛，但进程
  死亡不等于真实断电、缓存丢失或断电后的文件系统恢复。
- Restore interrupted-state matrix 从可达 Journal/rename 状态验证启动恢复；它不模拟
  存储控制器写乱序。Windows 交叉编译也不构成 Linux Runtime 证据。
- `smoke` 只用于开发反馈；`full` 仍需结合 Linux Race、全仓 Test/Vet、精确 CI 与独立
  交付复审，不能单独作为 M7-04 或 Alpha Gate 通过证据。
