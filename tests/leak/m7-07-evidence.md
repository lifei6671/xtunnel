# M7-07 Goroutine/FD/Memory Leak Evidence

## 当前状态

- 任务：`M7-07`
- 状态：`DONE`
- Delivery 基线：`2bf793bf5a9a51326bbcef1a13dd417a4fa381e0`
- 正式实现 Commit：`c527265fa165fd08b6c7f14644bd8138d83eea30`
- 首轮证据 Commit：`d91d43c8b2557b170d0a0edc32ce961fe851a7b4`
- CI 回归修复 Commit：`bf3554e3d775acbeeca2c770b5b9f3004906bc54`、
  `2ca093b6c6a892f3a970a565b4077658191ba492`
- 最新精确 CI：[#33510562933](https://github.com/lifei6671/xtunnel/actions/runs/33510562933)，
  `completed/success`
- `DONE` 计数：`92/95`
- M7：`7/10 IN_PROGRESS`

## 实现范围

- 新增 Linux-only `TestM7ResourceLeak`，按分区执行 connection churn、Cancel/Drain、
  Reconnect 与完整 Drain Matrix。
- 每个分区使用一个 warmed epoch 和后续等量 epoch；业务 owner 的 Session、WorkPool、
  Pending、Active、Limit Map 继续由既有产品 fixture 精确归零。
- 进程终态读取 `/proc/self/fd`、`VmRSS`、goroutine 和 Go `MemStats`。FD/goroutine 必须
  连续三次不高于连续三次稳定的 warmed baseline；Repair round 2 后，Full 的三个等量
  测量 epoch 相对 warmed baseline 的累计 `HeapAlloc` 固定 allowance 为 1 MiB、
  `HeapObjects` 为 3,000，不再使用会放过稳定小泄漏的逐步阈值。
- 新增 POSIX Runner、Windows-to-Linux development builder 与使用说明。Runner 的
  `smoke`/`full` 规模、超时、clean-tree Gate 和 Artifact SHA-256 均为固定值。
- 原始 Leak Harness/CI 接线未修改生产代码；精确 CI 暴露 WebSocket Hard Deadline 的
  生产关停缺口后，只对 `httpingress` WebSocket 生命周期做最小修复。Proto/OpenAPI/
  生成物、Server Schema、Migration、依赖/Lockfile、配置、权限与日志契约均未修改。

## 开发验证

工具链：Windows `go1.27.0`，`GOTOOLCHAIN=local`。

- `tools/check-go-version.ps1`：PASS。
- `GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0 go test -c
  ./internal/server/bootstrap`：PASS。
- PowerShell Builder AST parse：PASS。
- `sh -n tests/leak/run-m7-07.sh`：PASS。
- WSL2 Linux 6.18 development runs，Binary 均先复制到 Linux-native `/tmp`：
  - churn/cancel/drain，2 epoch × 10 连接：PASS；两轮终态均为 FD `7`、goroutine `3`，
    `HeapAlloc` 从 `2,341,616` 到 `2,427,008`，`HeapObjects` 从 `9,140` 到 `9,381`；
  - reconnect，2 epoch × 1 Connector，包含两次 Server Close/Reopen：PASS；两轮终态均
    为 FD `7`、goroutine `3`，`HeapAlloc` 从 `2,358,872` 到 `2,453,920`；
  - 完整 Drain Matrix，2 epoch：PASS；每个 TCP/HTTP/WebSocket、Server Hard Deadline、
    Agent 自然 Drain 和 30 秒 Hard Deadline 场景的 Session/Limit/FD/goroutine 归零断言
    均通过；分区终态 FD `7`、goroutine `3`，RSS 未持续上升；
  - Repair round 2 前 Runner `smoke`，2 epoch × 20 连接：PASS；Artifact 清单 SHA-256
    `2fa8cc108c5d9efb01246a8702b037fe2665630d6c7a35ee545d18606139e8c3`，只作为旧版
    Artifact 结构的历史开发反馈；
  - Repair round 2 后 Runner `smoke`，2 epoch × 20 连接：PASS；连续稳定资源采样终态
    FD `7`、goroutine `3`，累计 `HeapAlloc +73,496`、`HeapObjects +282`；环境、命令、
    前后 identity、测试日志和 prebuilt Binary 6/6 首次与搬移读回均为 `OK`，结果目录
    `/tmp/xtunnel-m7-07.n81NqZ`，新清单 SHA-256
    `272a7086d303cc32b980cf3fd6f34707b4030220d254df952522fc15cd614790`。
  - Repair round 3 后 Runner `smoke`，2 epoch × 20 连接：PASS；终态 FD `7`、goroutine
    `3`，累计 `HeapAlloc +114,736`、`HeapObjects +294`；Artifact 原目录与搬移目录均
    6/6 `OK`，结果目录 `/tmp/xtunnel-m7-07.hTy3Ag`，清单 SHA-256
    `e632c8a52e1e426370846b0c2f399ec3c07c84c4d9de275ac37a0263f4fe02ef`。

以上是 WSL2/脏工作区开发反馈，不是原生 Linux Full 或 CI 证据。

## 审计与剩余 Gate

- 实现前生命周期审计未发现生产 P0/P1/P2；两项 M7-07 验收缺口是缺少 GC 后 live heap
  Gate，以及既有 integration helper 在 `t.Cleanup` 前采样，不能单独承担正式 Leak Gate。
  当前实现使用同 package 产品 fixture，并在每个 epoch 子测试 Cleanup 完成后采样。
- Windows 全仓 `go test -count=1 -timeout=300s ./...` 与 `go vet ./...`：PASS。
- 隔离 Docker Linux amd64 clean `full`：从 Delivery 基线与当前 6 个 M7-07 文件冻结仓库外
  临时候选 Commit `23b0f282df66aa12e1b8ac642794ea17be3d6884`、Tree
  `b5ec2be7e4bd67359776e2c18a8092dfaeb58377`，Runner mode=`100755`。Docker LinuxKit
  amd64、Go `1.27.0/local`、CGO=1、Node `24.19.0`、npm `11.17.0`，镜像 ID
  `sha256:1e1cf7cdbaba0d85f78d8bbf61cd84fd5ff45a1bbce41720b450499109fe0aa5`。
  Web `ci/check/build`、4 epoch × 100 连接的全部三分区普通测试与 2 epoch × 20 连接的
  完整三分区 Race 均 PASS；普通/Race 分别耗时 `155.374s`/`84.235s`。
- 普通 Full 第四测量 epoch 相对 warmed baseline 的 retained heap：churn
  `HeapAlloc +284,120`/`HeapObjects +1,031`，Reconnect `+59,088`/`+185`，Drain
  `+94,944`/`+398`；Race 第二测量 epoch分别为 `+119,624`/`+392`、`+34,112`/`+129`、
  `+86,592`/`+370`。所有分区终态 FD `7`、goroutine `3`。
- Full 运行前后 identity 均精确为上述 Commit/Tree，worktree status 为空；环境、命令、
  前后 identity、Web 三份日志、普通日志和 Race 日志 9/9 在原目录与搬移目录均校验为
  `OK`。Artifact 清单 SHA-256 为
  `be05b98b892331f281eb21308d30e68385afc2fab614760a71df75ca22d1dc66`。该临时候选 Commit
  不在项目历史中；Docker LinuxKit 证据不替代正式 Commit、独立原生主机/arm64 或 CI。
- 隔离 Docker full 首次在测试启动前快速失败：Linux bind mount 将 Windows checkout
  的 CRLF 识别为工作区变化，clean-tree Gate 正确拒绝；改为容器内 Linux-native clone
  后，第二次在 Go 编译前发现 clean clone 缺少被忽略的 `web/dist`。当前 Repair round 1
  已按仓库规则在 full 模式补充 `npm ci/check/build` 与 Artifact 日志；两次均未启动
  M7-07 产品测试，不记为产品失败或 PASS。
- Repair round 2 独立复审发现并正在关闭：heap oracle 可放过稳定小泄漏、单点 warmed
  baseline、Builder 输出可进入仓库且 Commit/Tree 缺少前后 freshness、watchdog 无 KILL、
  Artifact 使用绝对路径且覆盖不全、Race 未覆盖 Reconnect/Drain，以及退出码和 GNU 工具
  证据不足。当前已改为累计 heap 预算与合成回归、连续稳定资源样本、仓库外空目录与双向
  identity freeze、`timeout -k 15s`、相对完整 Artifact 搬移读回、完整三分区 Race，并保留
  原始失败码。
- Repair round 3 独立复审补充发现 Builder 的个别 Git 命令未立即检查原始退出码、Web
  `npm ci/check/build` 未受进程级 watchdog 约束，以及 Artifact 搬移校验临时目录只在成功
  路径清理。当前分别补为每条 Git 命令立即 fail-closed、Web 10/5/5 分钟且 TERM 后 15 秒
  KILL、EXIT/HUP/INT/TERM trap 精确清理并保留原始退出码；PowerShell AST、`sh -n`、
  `dash -n` 与 WSL ShellCheck 均 PASS。被复审判定过期的 R1/R2 Docker 候选已停止，不计
  产品失败或 PASS；上述 R3 clean `full` 是修复后的新鲜结果。
- 修复后工作树 Tier 3 三分区独立复审均为 `COMPLETE/FRESH/PASSED`：Harness、
  Runner/Builder、状态/证据的 P0/P1/P2 均为 `0/0/0`。最终 WebSocket 生产修复与测试
  Harness 冻结内容另经两路独立复审，Review mode=`WORKTREE/CHILD_AGENT`、
  Coverage=`COMPLETE`、Freshness=`FRESH`、Gate=`PASSED`、P0/P1/P2=`0/0/0`。

## CI 接线

- 用户明确回复“确认修改 M7-07 CI”。`.github/workflows/ci.yml` 的现有 Linux
  `verify` amd64/arm64 矩阵新增 `Run native M7 resource leak full`，固定 15 分钟 Step
  上限，执行 `sh ./tests/leak/run-m7-07.sh -m full`，结果只写入架构隔离的
  `$RUNNER_TEMP/m7-07-leak-<arch>`。
- Step 要求 Artifact 清单存在，并在结果目录执行 `sha256sum -c artifact-sha256.txt` 与
  清单自身 SHA-256；相对路径不会错误地从仓库根解析。现有 Linux Job 45 分钟总上限、
  工具链、权限、Runner、其他步骤和 Windows Job 均未修改。
- 本地验证：PyYAML `6.0.3` 解析及精确矩阵/命令/超时断言 PASS；Workflow Diff Check
  PASS；CI Artifact 读回块对 R3 clean `full` 的 9/9 条目再次返回 `OK`，清单 SHA-256
  仍为 `be05b98b892331f281eb21308d30e68385afc2fab614760a71df75ca22d1dc66`。
  本机未安装 `actionlint`，因此该项为 `UNAVAILABLE`，未使用临时下载替代仓库工具链。
- 用户明确回复“确认暂存、提交并推送 M7-07”。7 个 delivery-owned 路径以正式实现
  Commit `c527265fa165fd08b6c7f14644bd8138d83eea30` 推送至 `origin/master`，Parent
  `2bf793bf5a9a51326bbcef1a13dd417a4fa381e0`，Tree
  `0e6ee53aaebb09c49a73ec2e1ddfc0adc285756e`，Runner mode=`100755`；提交信息为
  `test(leak): add M7-07 resource leak gate`，提交后工作区干净且远端 SHA 精确一致。
- 精确 GitHub Actions [#33502663587](https://github.com/lifei6671/xtunnel/actions/runs/33502663587)
  的 Head SHA 精确匹配正式实现 Commit，结论为 `completed/success`。Linux amd64 Job
  `99839352999`（15m20s）、Linux arm64 Job `99839353376`（12m51s）、Windows Agent
  service Job `99839353136`（5m03s）与 Windows arm64 Agent runtime Job
  `99839353139`（3m54s）均成功。
- 两个原生 Linux Job 的 `Run native M7 resource leak full` 均 PASS：amd64 普通/Race
  分别为 `155.71s`/`87.09s`，Artifact 清单 SHA-256 为
  `fd062ac72251d22214488678d22a1523e39700e2cdd53134cd041127224522a8`；arm64
  普通/Race 分别为 `155.59s`/`90.98s`，Artifact 清单 SHA-256 为
  `76acfa9f82e30feb06145b735ccd075933d5bf96a0cc30978aed54ae76f6bd71`。两个 Job 的
  9/9 Artifact 校验、其余既有 Gate 与最终 clean-tree 检查同样成功。
- 首轮证据 Commit `d91d43c8b2557b170d0a0edc32ce961fe851a7b4` 的精确
  [CI #33504366959](https://github.com/lifei6671/xtunnel/actions/runs/33504366959) 为
  `completed/failure`：arm64 Job `99844797106` 暴露既有 TCP Ingress 测试 oracle 缺口，
  amd64 Job `99844797616` 暴露 WebSocket Hard Deadline 卡住；两个 Windows Job 成功。
  这两项均发生在 `Test Go modules and build both processes`，未冒充 Leak Step PASS。
- Commit `bf3554e3d775acbeeca2c770b5b9f3004906bc54` 最小修正 TCP Ingress oracle；精确
  [CI #33505969409](https://github.com/lifei6671/xtunnel/actions/runs/33505969409) 中 arm64
  Job `99849966932` 与两个 Windows Job 成功，amd64 Job `99849967300` 仍由同一 WebSocket
  Hard Deadline 问题失败，因此该轮仍为 `completed/failure`。
- 根因是 Origin 已 EOF 后 ReverseProxy 只半关闭 Hijacked Client，Shutdown Context 取消
  未主动解除剩余 Client→Backend 阻塞。Commit
  `2ca093b6c6a892f3a970a565b4077658191ba492`（Tree
  `e6acb622694835fd92e2c8c589e8d371be0d506b`）让 WebSocket owner 在取消时幂等关闭
  Client 与 Backend，并等待 `context.AfterFunc` 回调退出；同时增加 Origin 先 EOF、Client
  仍半开的确定性回归，以及 Harness 失败清理和 ACTIVE 发布屏障。
- 修复提交前验证：Windows Go `go1.27.0/local` 下 WebSocket 回归普通 `count=100` 与
  Race `count=20`、`httpingress` package 普通 `count=20`、全仓 Test/Vet 均 PASS；Docker
  Linux amd64 的 WebSocket Hard Deadline 普通 `count=50`/Race `count=20`、TCP Half-Close
  普通 `count=50`/Race `count=20`、六场景普通 `count=2`/Race `count=1`、全仓 Test/Vet
  均 PASS。一次六场景 `count=10` 因 300 秒 package 超时预算不足而超时，不记为产品失败；
  随后的高计数运行暴露并关闭了 ACTIVE 发布测试竞态。
- 修复提交的精确 [CI #33510562933](https://github.com/lifei6671/xtunnel/actions/runs/33510562933)
  Head SHA 精确匹配 `2ca093b6c6a892f3a970a565b4077658191ba492`，结论为
  `completed/success`。Linux amd64 Job `99864922479`（15m02s）、Linux arm64 Job
  `99864922559`（12m48s）、Windows arm64 Agent runtime Job `99864922226`（4m04s）与
  Windows Agent service Job `99864922780`（5m05s）全部成功，两个 Linux Job 的既有 Gate
  与最终 clean-tree 检查也全部成功。
- 最新 Leak CI 中，amd64 普通/Race 分别为 `155.73s`/`87.71s`，Artifact 清单 SHA-256
  为 `8d82fe0e9170e5fcd0031ed72ac30503d2aa4e3f7a4ccfea71011b31a691ce6d`；arm64
  普通/Race 分别为 `155.61s`/`91.09s`，清单 SHA-256 为
  `bdd94a03c81edb12e51a685911e364d9780a8f5c9c23ab06a5281571aaa23c54`。两边均完成
  9/9 Artifact 原目录、搬移目录与 CI 读回校验。
- CI 接线后的工作树 Tier 3 复审由 CI/Runner 静态分区、状态/证据分区和跨分区集成复审
  组成，三路均为 `COMPLETE/FRESH/PASSED`、P0/P1/P2=`0/0/0`。该结果只说明当前工作树
  接线无已知阻断缺陷，不替代正式 Commit 上的 commit-bound 复审或精确 GitHub Actions。
- M7-07 从 `IN_PROGRESS` 转为 `REVIEW`，等待用户明确阶段复审批准；在批准前不得转为
  `DONE`。全局 `DONE` 保持 `91/95`、M7 保持 `6/10 IN_PROGRESS`；M7-08 不启动，
  Alpha Release Gate Checklist 不变。

## 用户阶段复审批准

- 2026-09-01，用户明确回复“`M7-07 阶段复审通过`”。
- M7-07 从 `REVIEW` 转为 `DONE`；全局 `DONE` 从 `91/95` 更新为 `92/95`，M7 从
  `6/10` 更新为 `7/10 IN_PROGRESS`。
- M7-08、M7-09 保持 `READY`，M7-10 继续等待二者 `DONE`；Alpha Release Gate
  Checklist 保持未勾选。历史失败 CI 及其结论保持原样。
