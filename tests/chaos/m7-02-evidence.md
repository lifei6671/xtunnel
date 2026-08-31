# M7-02 重连风暴开发证据

> 状态：`IN_PROGRESS`（生产分类已修复，WSL2 全档开发验证通过；等待 clean Commit/CI 正式证据）

## 证据边界

- 当前结果来自 Windows `go1.27.0`/`GOTOOLCHAIN=local` 交叉编译的
  `linux/amd64`、`GOAMD64=v1`、`CGO_ENABLED=0` 测试 Binary，并在 WSL2
  `6.18.33.2-microsoft-standard-WSL2` Loopback 上执行。
- 工作区含本任务未提交改动，Manifest 明确记录 `worktree_clean=false`；以下只能作为
  开发反馈，不能替代 clean checkout、精确 Commit、CI 或原生 Linux 验收。
- 100/500/1000 使用完整生产 Connector Runtime，每个 Connector 恢复 8 条 Idle
  WorkConn；每档从 Startup/Recovery 阶段开始即用单一受控 Probe 并发尝试真实 TCP
  Origin 往返，记录首个成功，不等待全部 WorkPool 就绪后才启动。5000 使用生产
  TLS/Auth/Control Session 与 reconnect，仅不创建会额外放大到约 40000 条的 WorkConn。
- 未修改 Proto、OpenAPI、Server Schema/Repository 默认值、依赖、CI/CD、权限模型或
  日志契约，也未据 WSL2 样本调整 Gateway Rate/Burst。

## 已通过的开发验证

固定输入：Connector 首次启动在 2 秒窗口内 Stagger；Server 使用同一 Gateway 地址、
关闭 Runtime 和 SQLite 后重新打开持久化资源；恢复断言 Connector 集合不变、Session ID
全部更换、Current Session 唯一且旧 Session 无污染。

| Connector | 模式 | Startup first success | Recovery control p99/max | Recovery work p99/max | Recovery first success |
| ---: | --- | ---: | ---: | ---: | ---: |
| 100 | full-runtime | 3002 ms | 1221/1221 ms | 1221/1221 ms | 3025 ms |
| 500 | full-runtime | 3004 ms | 1253/1273 ms | 1273/1273 ms | 3075 ms |
| 1000 | full-runtime | 3003 ms | 32300/32419 ms | 32300/32439 ms | 3141 ms |

| Connector | Peak FD | Peak RSS | Peak goroutines | Recovery CPU time | Data-plane attempts |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 100 | 1826 | 196728 KiB | 9317 | 15599 ms | 2 |
| 500 | 9026 | 762840 KiB | 46517 | 23630 ms | 2 |
| 1000 | 18026 | 1397344 KiB | 93017 | 235755 ms | 2 |

三档首条业务成功均来自并发 Probe 的第二次尝试。1000 档在约 `3.1 s` 已出现首个业务
成功，而最终 WorkPool 尾部约为 `32.5 s`，两个指标已独立测量；约 1.29 GiB RSS 是当前
WSL2 同进程 Server+Agent 测量值，只记录容量边界，不预设 SLO。阶段时延在同一阶段再次
换代时会按 Connector 重置，表中分布归属最终 Current Session，不沿用已被替换 Session
的首次就绪时间。

同一 Binary 的前序连续 WSL 调用曾出现宿主命令不返回且没有终态输出；并行检查时 Linux
内已无 `bootstrap.test` 进程，人工中止的是失去子进程的 WSL interop 会话，该次不计为
通过。随后各档都从 Linux-native 临时目录隔离复跑，1000 档于 `35.72 s` 通过并形成上表
证据。当前没有足够信息把宿主会话异常归因于产品，但仍保留为正式 clean `full` 必须再次
验证的 Runner/WSL 稳定性风险，不把隔离通过提升为正式 Gate 证据。

重连算法的确定性单元测试另覆盖 100/500/1000/5000：`±20%` Jitter 分散、永久认证
错误只尝试一次、`retry_after` 不被负向 Jitter 降低，且不使用真实等待。

## 5000 Connector 修复与复验

修复前，5000 control-only 档连续两次在 Server Restart 后失败。代表错误链为：

```text
authenticate connector control session
  -> connector control auth protocol violation
  -> auth result frame: frame: truncated frame
  -> read: connection reset by peer
```

根因是 `controlauth.classifyReadError` 把所有 `frame.ErrTruncatedFrame` 包装为
`controlauth.ErrProtocol`；`reconnect.classify` 又把全部 `controlauth.ErrProtocol` 判为
永久错误。现已从永久协议错误集合中移除 `ErrTruncatedFrame`，让读途中断保留底层
EOF/Reset identity 并进入既有有界 Jitter Backoff；非法 UVarint、超限帧和完整但畸形
Protobuf 仍保持永久 `ErrProtocol`。

最终 Binary 下的 5000 control-only 连续两轮均通过：

| Run | Startup config max | Recovery control p99/max | Recovery config p99/max | Peak FD | Peak RSS |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 2085 ms | 3418/3598 ms | 3418/3598 ms | 10025 | 841128 KiB |
| 2 | 2025 ms | 3353/3614 ms | 3374/3633 ms | 10025 | 825396 KiB |

| Run | Peak goroutines | Recovery CPU time | Peak Pending TLS/Auth upper bound |
| ---: | ---: | ---: | ---: |
| 1 | 50016 | 30555 ms | 294 |
| 2 | 50016 | 33490 ms | 485 |

两轮均恢复 5000/5000 Connector、完整 ConfigAck 与新 Session fencing，
`generation_resets=5000`，未再出现 `Agent owner exited before phase readiness`。

`peak_pending_tls_auth_upper_bound` 仅是 `/proc/net/tcp` 中 IPv4 Gateway 端
`SYN_RECV+ESTABLISHED` 减去已发布 Current Control 的观测上界；它还包含 AUTH 已完成但
尚未发布 Current 的连接，不是内部 TLS/Auth Semaphore 的精确占用，也不能解释为突破
配置的 512/512 上限。

M7-02 仍保持 `IN_PROGRESS`：当前工作区未提交，正式 `full` Runner 的 clean checkout
门禁不能满足，也尚无精确 Commit/CI 与最终 commit-bound 复审；本轮不能写成 `DONE` 或
上升为 Alpha Gate 通过。

## 已执行命令

```text
./tools/check-go-version.ps1
go test ./internal/agent/controlauth ./internal/agent/reconnect -count=1 -timeout=60s
go test -race ./internal/agent/controlauth ./internal/agent/reconnect -count=1 -timeout=60s
go vet ./internal/agent/controlauth ./internal/agent/reconnect
./tests/chaos/build-m7-02-linux.ps1 -OutputDirectory <temp> -AllowDirty
./tests/chaos/run-m7-02.sh -m smoke -b <wsl-prebuilt-path>
XTUNNEL_M7_02_CONNECTORS=500 bootstrap.test ...
XTUNNEL_M7_02_CONNECTORS=1000 bootstrap.test ...
XTUNNEL_M7_02_CONNECTORS=5000 bootstrap.test ...  # 最终 Binary 连续两次通过
RLIMIT_NOFILE hard=4096 + XTUNNEL_M7_02_CONNECTORS=5000 bootstrap.test ...  # 预期 FAIL，未 Skip
go test ./... -count=1 -timeout=120s
go vet ./...
```

POSIX Runner 已通过 `sh -n`、`dash -n` 和 `shellcheck -s sh`。正式 `full` 模式要求
当前 checkout 与预编译 Manifest 均为 clean，因此本轮没有绕过该门禁执行正式 full。
