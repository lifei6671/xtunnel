# M7-03 Graceful Shutdown Chaos 验证证据

> 状态：`REVIEW`（六场景 clean `full`、Bootstrap Linux Race、精确 CI 与 commit-bound
> 最终独立复审均已通过；等待用户阶段复审）

## 证据边界

本任务增加测试与可复现 Runner，并修复 Agent 收到匹配 DrainAck 后同步等待 ACTIVE
导致 Control Owner 停止发送 Heartbeat 的生产缺陷。修复不改变 30 秒默认 Drain 窗口、
Proto、OpenAPI、Server Schema、Migration、依赖、CI/CD、权限或日志契约。

产品级测试使用真实 Linux Socket、Server Bootstrap、SQLite、Gateway Control/Work Session、
Token-only Agent 与 TCP/HTTP Origin。组件级 DrainRequest/Ack、旧 Ack、OPENING、Usage
exactly-once 和真实 SIGTERM 接线继续由对应 owner 测试证明，不由本 Harness 重复实现。

## 场景矩阵

| 场景 | 核心断言 |
| --- | --- |
| TCP Half-Close 自然排空 | Shutdown 停新 Accept；Public `CloseWrite` 后 Origin 尾部字节完整返回；Active 完成前 Shutdown 不返回 |
| HTTP Streaming 自然排空 | 首块 Flush 后启动 Shutdown；Slow Origin 释放后两段响应逐字节一致 |
| WebSocket 自然排空 | Shutdown 期间仍可完成真实帧双向传输，Hijacked Handler 自然退出 |
| Hard Deadline | TCP、HTTP Slow Origin、WebSocket 同时阻塞；250ms 测试 Deadline 后主动解除两端 IO，错误链包含 `context.DeadlineExceeded` |
| Agent 两阶段 Drain | Server 观察 WorkPool Draining；既有 ACTIVE 继续传输；新 OPEN 不到达 Origin；ACTIVE 完成后 Agent 有界退出 |
| Agent Hard Deadline | 真实 30 秒默认窗口内持续 ACTIVE 流量；Control Owner 保持 DRAINING Heartbeat；Deadline 后 Agent 主动关闭 Public/Origin 两端 IO，错误链同时包含 `context.Canceled` 与 `context.DeadlineExceeded` |
| 资源终态 | Session Snapshot 为空；Connector/Work/Pending Open/Active 全部配额计数与分组 Map 为零；FD、goroutine 精确回到场景基线 |

## 当前开发证据

- Go 工具链检查：`go1.27.0`、`GOTOOLCHAIN=local` 通过。
- Windows 交叉编译：`GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0` 的
  `internal/server/bootstrap` Test Binary 构建成功。
- WSL2 开发运行：更新后的完整 `TestM7GracefulShutdownChaos` 六个场景通过；每个场景
  Session/Quota 归零且 `FD=7/7`、`goroutine=3/3`。Server Hard Deadline 为 `250ms`，
  观测 Force Close 为 `251.728152ms`；Agent 使用真实 30 秒默认窗口，观测 Force Close
  为 `30.001029581s`。该结果来自 dirty 工作区开发 Binary，仅是开发反馈，不能替代
  新实现的 clean `full`、Bootstrap Linux Race 或 CI。
- M7-03 Runner `smoke` 通过；`sh -n`、`dash -n` 与 `shellcheck -s sh` 均通过。
- 相关 owner 的 Windows 定向 Test、Race 和 Vet 通过；新增单测证明匹配 DrainAck 后
  `CompleteDrain` 等待 ACTIVE 期间 Control Owner 继续发送 Heartbeat，并在完成后等待
  owned goroutine 退出。
- Windows 全量 `go test ./...`、`go vet ./...` 通过；Linux build-tag 的 M7-03 产品级
  场景由上述 WSL2 Test Binary 开发运行覆盖。
- clean Commit `886c727271e11c8e87272fe1a19ef8ec14f465fa` 通过 M7-03 Linux Runner
  `full`：五个场景全部 PASS，最终 `FD=6/6`、`goroutine=3/3`，Hard Deadline
  `250ms` 下观测 Force Close 为 `253.320834ms`。Builder Manifest、Runner 与 clean
  WSL2 `/tmp` checkout 均绑定该 Commit。
- WSL2 已安装 Go `1.27.0`；`GOTOOLCHAIN=local CGO_ENABLED=0 go test
  ./internal/server/bootstrap -run '^TestM7GracefulShutdownChaos$' -count=1` 原生源码测试
  通过。TCP Half-Close 契约路径另连续运行 `20/20`，完整五场景连续运行 `5/5` 通过。
- 修复前的 clean `full` 暴露 TCP Listener 探针会被生产 Accept 路径接收并制造第二条
  无人接管 Origin；Commit `886c727271e11c8e87272fe1a19ef8ec14f465fa` 改为同步
  `StopAccepting` 后单次失败断言，同时保留 Shutdown pending 后 Public `CloseWrite`、
  Origin EOF 屏障与反向尾部传输。修复后的 worktree Tier 3 独立复审为 `PASSED`，
  P0/P1/P2=`0/0/0`；该结果不替代最终 commit-bound 复审。
- 本轮初始 Tier 3 checkpoint 复审识别三项 P1：TCP 场景绕过生产 Shutdown 直接调用
  `StopAccepting`、缺少 Agent 30 秒 Hard Deadline，以及资源断言允许宽松 FD/goroutine
  增量且未检查 Quota。当前工作区已分别改为观察生产 Admission Fence、增加真实默认窗口
  场景、并要求 Session/Quota/FD/goroutine 精确归零。
- 新 Agent Hard Deadline 场景暴露生产缺陷：匹配 DrainAck 后同步 `CompleteDrain` 会阻塞
  Control Owner，Server 在 Agent 30 秒窗口之前按 Heartbeat Timeout 关闭 Session。当前修复
  让 owned completion goroutine 等待 ACTIVE，Control Owner 同期继续发送 DRAINING
  Heartbeat；所有退出分支取消 Drain Context 并等待 goroutine 收敛。
- 修复后独立复审又发现 SessionDone 与进程取消竞态：WorkPool 可能先把 ACTIVE 转入
  `detachedActive` 并让 `Wait` 返回，而 Agent 当前 Session owner 尚未等待 `Done`。当前
  `finishSessionPool` 在进程取消或业务错误分支先取消 Pool、等待全部 worker/FD 对应的
  `Done`，再读取 `Wait` 结果；普通 Control 断开仍保留 ACTIVE 并登记 retired Pool，不阻塞重连。
- Repair round 1 `CHILD_AGENT / Tier 3 / Go / FULL_SCOPE` checkpoint 覆盖全部交付路径与
  必要相邻 owner，Coverage=`COMPLETE`、Freshness=`FRESH`、Gate=`PASSED`，
  P0/P1/P2=`0/0/0`。这是 dirty worktree checkpoint，不替代 commit-bound 最终复审。

## 正式收口证据

- 实现与 CI 修复的最终 Commit 为 `45c6be28bd2fef6f802fbc2719bf5e6952f7728d`
  （Tree `9ad761688e2455f495e25eaeb087d930710be02a`）。Windows Builder 在 clean
  Worktree 生成 `linux/amd64`、`GOAMD64=v1`、`CGO_ENABLED=0` Test Binary；Manifest
  记录 `worktree_clean=true`、Go `go1.27.0`、`GOTOOLCHAIN=local`，Binary SHA-256 为
  `1a5f04d40b333cd7289428ea323a38a9bdb633871e419596118ff2b5fa16d29d`，Manifest
  SHA-256 为 `039FB5E014EE4525C99C50A00015B94B295CE4058B779855B48B42CE9F856221`。
- WSL2 Linux-native `/tmp` 精确克隆在该 Commit 上执行 Runner `full`，六个场景全部
  PASS，Runner 终态为 `M7-03 chaos run completed.`、Exit Status `0`。Server 250ms
  Hard Deadline 观测为 `253.592029ms`，Agent 30 秒默认窗口观测为 `30.00122165s`；
  每场景 Session/Quota 清零，FD 与 goroutine 精确回到 `6/6`、`3/3`。该结果是 WSL2
  Linux 内核运行证据，不描述为独立原生 Linux 主机或代表性网络条件。
- 首次精确 CI [#33455091131](https://github.com/lifei6671/xtunnel/actions/runs/33455091131)
  绑定 Commit `80142e593e2e32b773d8f21dfad7dd4fd412d676`，Linux amd64/arm64 在 Product、
  Trace、Diagnostics E2E teardown 暴露测试自有 HTTP keep-alive 与 Pending Open 取消
  竞态，Windows Job 通过。修复提交 `45c6be28bd2fef6f802fbc2719bf5e6952f7728d`
  在释放场景资源后等待 Server 当前 generation 的权威 Runtime Snapshot 归零；未改变生产
  Shutdown、Hard Deadline 或 Half-Close 契约。
- 最终精确 CI [#33457845746](https://github.com/lifei6671/xtunnel/actions/runs/33457845746)
  的 Head SHA 精确为 `45c6be28bd2fef6f802fbc2719bf5e6952f7728d`，结论
  `completed/success`；Linux amd64、Linux arm64、Windows Agent service 与 Windows
  arm64 Agent runtime 四个 Job 全部成功。两个 Linux Job 均通过 `go test ./...`、
  `go test -race -count=1 -timeout 300s ./internal/server/bootstrap ...`、`go vet ./...`
  及 Product、Trace、Diagnostics 原生 E2E，因此 Bootstrap Linux Race 已有正式 CI 证据。
- 最终独立复审为 `CHILD_AGENT / Standard Mode / Tier 3 / Go / FULL_SCOPE`，覆盖
  Baseline `1babca3290447b33b02ae126c9d03c532c97ff8a` 至 Target Commit
  `45c6be28bd2fef6f802fbc2719bf5e6952f7728d`，Coverage=`COMPLETE`、
  Freshness=`FRESH`、Gate=`PASSED`、P0/P1/P2=`0/0/0`。累计 Repair rounds=`3`，
  Target 冻结后 Repair rounds=`0`。

## 剩余审批

- 用户明确阶段复审；在此之前不得标记 `DONE` 或勾选 M7 Alpha Gate。
