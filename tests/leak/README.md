# M7-07 Goroutine/FD/Memory Leak Harness

M7-07 Runner 调度 Linux-only 的 `internal/server/bootstrap.TestM7ResourceLeak`。
Harness 复用已经完成的真实 Bootstrap、TLS Gateway、Token-only Agent、Public Listener
和 Origin 测试设施，不新增生产入口、配置、依赖或协议。

## 覆盖范围

- `connection_churn_and_cancel_drain`：每个 epoch 反复建立真实 Public TCP，执行二进制
  Round Trip 与 Half-Close，等待 ACTIVE/Pending/Limit Map 归零，再通过 Agent Context
  Cancel 完成两阶段 Drain；
- `reconnect`：复用 M7-02 的真实 Server Close/Reopen、Agent 重连、完整 Snapshot 和
  generation fencing；
- `drain`：复用 M7-03 的 TCP/HTTP/WebSocket 自然排空、Server Hard Deadline、Agent
  两阶段 Drain 与 30 秒 Agent Hard Deadline；
- 每个分区先执行一个 warmed epoch，后续等量 epoch 必须回到 warmed FD/goroutine
  基线；业务 owner 自有的 Session、WorkPool、Pending、Active 和 Limit 仍精确归零；
- 每个 epoch 完成全部 Cleanup 后执行两轮 GC，记录 `HeapAlloc`、`HeapObjects`、
  `HeapInuse`、`StackInuse` 和 RSS。Full 的三个等量测量 epoch 相对 warmed baseline 的
  累计 `HeapAlloc` 不得超过 1 MiB、`HeapObjects` 不得超过 3,000；RSS 只作为趋势诊断，
  不要求字节级回落。

Harness 不检查 TCP `TIME_WAIT=0`，也不把旧 generation 合法存续的 ACTIVE、Tombstone
或 retired Pool 在自然完成前误报为泄漏。

## 运行

直接在 Go `go1.27.1` 或更新的 `1.27.x` Linux amd64 环境运行：

```sh
sh ./tests/leak/run-m7-07.sh -m smoke
sh ./tests/leak/run-m7-07.sh -m full -o /tmp/xtunnel-m7-07-full
```

- `smoke`：2 个 epoch，每个 epoch 20 条真实 TCP，只运行 churn/cancel/drain 快速路径；
- `full`：4 个 epoch，每个 epoch 100 条真实 TCP，运行 churn、Reconnect、完整 Drain
  矩阵，并对缩小为 2 epoch × 20 TCP 的完整三分区再次运行 Race；
- `full` 要求工作树 clean，结果目录必须位于仓库外且为空；Runner 会冻结运行前后的
  Commit/工作树状态。clean checkout 没有被忽略的 `web/dist`，所以 Full 会先按仓库
  固定顺序执行 `npm --prefix web ci/check/build`，再运行 Go Test。三个 Web 步骤分别受
  10/5/5 分钟 watchdog 约束；Runner 依赖 POSIX `sh` 与 GNU coreutils
  `timeout`/`sha256sum`，所有 watchdog 均在 TERM 后 15 秒 KILL；环境、
  精确命令、前后 Commit/Tree/工作树、Web、普通测试、Race 与 prebuilt Binary（如有）
  都进入相对路径 SHA-256 清单，并在搬移到第二个临时目录后再次读回校验。

Windows 没有 Go 的 Linux 环境时，可生成仅供开发 Smoke 的测试 Binary：

```powershell
./tests/leak/build-m7-07-linux.ps1 \
  -OutputDirectory C:\Temp\xtunnel-m7-07-bin \
  -AllowDirty
```

然后在 Linux/WSL2 运行：

```sh
sh ./tests/leak/run-m7-07.sh -m smoke \
  -b /mnt/c/Temp/xtunnel-m7-07-bin
```

预编译 Binary 必须匹配当前 checkout 的 Commit、Go 版本、目标平台和 SHA-256；它只
支持 `smoke`。Windows 交叉编译与 WSL2 运行都不能替代原生 Linux amd64/arm64 Full、
目标 Commit 的精确 CI 或 commit-bound 最终复审。

## 证据边界

- 正式 FD Gate 必须在 Linux-native 环境执行；Windows 没有 `/proc/self/fd`，不能把
  goroutine/heap 结果描述为 FD 证据。
- Go allocator、Race Runtime、SQLite 与 TLS 可能保留有限缓存，因此先 warm-up，再比较
  GC 后 live heap；不得按本次峰值动态放宽预算。
- `debug.FreeOSMemory` 未参与断言，避免把测试主动 scavenging 描述成产品自然释放。
- Runner 不修改 CI、生产配置、权限、依赖、协议或日志契约；接入 CI 必须单独批准。
