# M7-03 Graceful Shutdown Chaos 验证证据

> 状态：`IN_PROGRESS`（开发态 Harness 已建立；尚无 clean `full`、Race、精确 CI、
> commit-bound 最终独立复审或用户阶段复审）

## 证据边界

本任务只增加测试与可复现 Runner，不修改 Server/Agent 生产关闭实现、30 秒默认 Drain
窗口、Proto、OpenAPI、Server Schema、Migration、依赖、CI/CD、权限或日志契约。

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
| 资源终态 | Session Snapshot 为空；每场景 FD 不超过基线 `+10`，goroutine 不超过基线 `+20` |

## 当前开发证据

- Go 工具链检查：`go1.27.0`、`GOTOOLCHAIN=local` 通过。
- Windows 交叉编译：`GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0` 的
  `internal/server/bootstrap` Test Binary 构建成功。
- WSL2 开发运行：完整 `TestM7GracefulShutdownChaos` 五个场景通过；每个场景最终
  `FD=7/7`、`goroutine=3/3`。Hard Deadline 为 `250ms`，观测 Force Close
  约 `254ms`。该结果来自 dirty 工作区开发 Binary，仅是开发反馈，不能替代 clean
  `full`、Linux Race 或 CI。
- M7-03 Runner `smoke` 通过；`sh -n`、`dash -n` 与 `shellcheck -s sh` 均通过。
- 相关 owner 的 Windows 定向 Test、Race 和 Vet 通过：`internal/proxy`、Server
  Runtime/WorkPool/Session/HTTP/TCP、Agent WorkPool/Connector、Server Usage。
- Windows 全量 `go test ./...`、`go vet ./...` 通过；Linux build-tag 的 M7-03 产品级
  场景由上述 WSL2 Test Binary 开发运行覆盖。

## 待完成

- clean M7-03 Linux Runner `full` 与 Linux 定向 Race；当前 WSL2 未安装 Go，不能把
  Windows 相关 owner Race 或 Linux 交叉编译冒充该证据。
- clean Commit 绑定的正式输出、精确 CI 与 Tier 3 最终独立复审。
- 用户明确阶段复审；在此之前不得标记 `DONE` 或勾选 M7 Alpha Gate。
