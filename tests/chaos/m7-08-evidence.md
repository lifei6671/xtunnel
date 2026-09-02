# M7-08 Large Transfer/Privileged Network Chaos 交付证据

> 状态：`DONE`（正式实现、原生 Linux amd64/arm64 特权 `full`、Artifact 回读、
> 精确 CI、commit-bound Tier 3 独立复审、证据 CI 与用户阶段复审均已完成）

## 当前范围

- 正式实现 Commit：`0f629f926ed3bdbbf9c698dab82130a1282e4731`。
- 测试路径：Public TCP Listener→Server/Gateway→Token-only Agent→Origin。
- clean 档：每方向 1 GiB，发送端和接收端分别计算 SHA-256，字节数必须精确一致。
- 受损档：Loss 1%/5%、100 ms Delay+50 ms Jitter、10 Mbit/s；每方向 8 MiB。
- 生命周期：每档执行双向 TCP Half-Close，结束后检查 FD、goroutine、Active/Idle 和
  Server Runtime 资源收敛。
- Reset：Runner 用 nftables counter 证明活动公网流量命中故障规则，移除会截获 RST 的
  reject table 后再用 `ss -K` 销毁精确目标 socket；测试拒绝 Timeout 伪证据，并要求
  故障撤销后新连接恢复。

本任务没有改变生产实现、Wire/REST/Config/Persistence 契约、依赖、权限或日志字段。

## 已执行验证

- Windows 工具链：`tools/check-go-version.ps1` 通过，版本为 `go1.27.0 (local)`。
- `gofmt`、PowerShell Parser、WSL2 `sh -n`、`dash -n`、ShellCheck 和任务文件
  `git diff --check` 通过。
- Windows 成功生成 `linux/amd64`、`GOAMD64=v1`、`CGO_ENABLED=0` 的
  `bootstrap.test` 与 dirty-development Manifest；交叉构建不作为 Linux Runtime PASS。
- Windows 的 Linux arm64、`CGO_ENABLED=0` Bootstrap Test Binary 交叉编译通过，最终
  Binary 大小为 `40373234` bytes；交叉编译不作为 arm64 原生 Runtime PASS。
- 同一 Test Binary 在 WSL2 独立 namespace 的 clean 1 GiB 双向开发档通过：

  ```text
  bytes_per_direction=1073741824
  upload_sha256=29fddf94839f22d967c01da8fdfcb2219c2bc7d206388970162b58114e077e42
  download_sha256=5462afe8e48f15efe43acdcf26224b2462feaeea74210fa13da5d923701f42f4
  lost=0 duplicate=0 half_close=true
  baseline_fd=8 final_fd=8 baseline_goroutines=3 final_goroutines=3
  result=PASS
  ```

  该命令是 dirty-development Binary 的定向运行，不是 clean Runner `full`。
- WSL2 Ubuntu 22.04、kernel `6.18.33.2-microsoft-standard-WSL2`、root、seed
  `20260902` 的隔离 namespace Smoke：

  ```text
  profile=delay 20ms 5ms rate 100mbit
  bytes_per_direction=8388608
  upload_sha256=5351604d9baa8549dc27dc086c58a3010aff4e6c018e9d22ed6213a8f5aa42ef
  download_sha256=98de66313c2ca653a73ea4b33dba0c43df9b35eb0e9c52df504e286b7c81d4a0
  lost=0 duplicate=0 half_close=true
  baseline_fd=7 final_fd=7 baseline_goroutines=3 final_goroutines=3
  result=PASS
  ```

- 同一 Smoke 的 Reset 档中，nft dport/sport 规则均累计 `7 packets`，证明目标流量
  已命中；`ss -K` 返回 `RTNETLINK answers: Invalid argument`，执行后目标 ESTABLISHED
  socket 仍存在。最终 Runner 返回：

  ```text
  m7-08 chaos: kernel did not destroy the active socket; SOCK_DESTROY support is required
  ```

  Runner 随后清理 namespace、qdisc、nft table 和 Linux-native 临时目录。该档结论为
  `BLOCKED_BY_WSL2_KERNEL`，不是产品失败，也不是 Reset PASS。

## 精确 CI 与 Artifact 回读

- [CI #33583345819 Attempt 1](https://github.com/lifei6671/xtunnel/actions/runs/33583345819/attempts/1)
  精确绑定正式实现 Commit。M7-08 原生特权 amd64 Job `100102157629`、arm64 Job
  `100102157615` 均成功；Linux verify arm64 Job `100102157501` 在前置全量 Go 步骤的
  既有 `TestProcessExitsOnSIGTERM` 中以 `process exit error = signal: terminated` 失败，
  因而 Attempt 1 整体保持 `failure`。
- 用户明确确认仅重跑失败的 arm64 verify Job。[Attempt 2](https://github.com/lifei6671/xtunnel/actions/runs/33583345819/attempts/2)
  只重新执行 Job `100105330507`，此前失败未复现，后续全部 Gate 均成功，Run 最终为
  `completed/success`。
- amd64 Artifact `9829213126` 与 arm64 Artifact `9829209726` 均已下载并逐项回读：
  `mode=full`、seed=`20260902`、clean worktree、`go1.27.0/local`、精确 Commit；每端
  8 项 Manifest 均通过 SHA-256 校验，clean 1 GiB、Loss 1%/5%、Jitter 50 ms、
  10 Mbit/s、Reset 与恢复结果齐备。
- Reset nft dport/sport counter 分别为 amd64 `7/7 packets`、arm64 `5/5 packets`；
  两端均记录活动连接被非 Timeout 主动解阻，并在撤销故障后恢复新连接。
- 证据 Commit `584f699c04e247f44b8ac80a4aad373200f82ea9` 的
  [CI #33586979302 Attempt 1](https://github.com/lifei6671/xtunnel/actions/runs/33586979302/attempts/1)
  中，双架构 M7-08 特权 Job、Windows Jobs 与 arm64 verify 均成功；amd64 verify Job
  `100113067865` 在既有 `TestProcessExitsOnSIGTERM` 中偶发 `signal: terminated`，因此
  Attempt 1 保持 `failure`。
- 用户明确确认只重跑失败的 amd64 verify Job。[Attempt 2](https://github.com/lifei6671/xtunnel/actions/runs/33586979302/attempts/2)
  仅执行 Job `100115793630`；原失败未复现，全部后续 Gate 均成功，Run 最终为
  `completed/success`。

## 独立复审

- 正式 Commit 的 7 路径经 `CHILD_AGENT`、`FULL_SCOPE / Tier 3` commit-bound 复审。
- Coverage=`COMPLETE`、Freshness=`FRESH`、Gate=`PASSED`、P0/P1/P2=`0/0/1`。
- 唯一 P2 是本文件与开发计划仍保留旧的 `IN_PROGRESS/NOT RUN` 状态；本次证据同步
  已由证据 Commit `584f699c04e247f44b8ac80a4aad373200f82ea9` 修复并通过精确 CI；
  实现本身无 P0/P1/P2 问题。

## 修复记录

- 首轮 Reset Harness 曾把活动流量的 20 秒读 Timeout 当成“已解阻”，并在等待 Origin
  时失败。现已显式拒绝实现 `net.Error` 且 `Timeout()==true` 的结果。
- loopback 的 nft `reject with tcp reset` 虽能命中并计数，但不会销毁同一内核里的现有
  socket。Runner 记录计数后先移除 reject table，避免它截获销毁过程生成的 RST，再以
  `ss -K` 执行精确 socket destroy 并复查连接；内核不支持时立即失败，不再等待测试
  Deadline。
- `smoke` 移除随机 Loss，只保留有界 Delay/Jitter/Rate，避免 1 秒 TCP Dial Deadline
  因 SYN 随机丢失产生开发态抖动；Loss 1%/5% 仍是 `full` 的必跑档，不能跳过。

## 阶段结论

- 用户已明确回复“`M7-08 阶段复审通过`”，M7-08 转为 `DONE`。
- M7-08 已无未完成验收项；两条 CI Attempt 1 的偶发失败事实继续保留，不因 failed-only
  重跑成功而改写。
- 本文件只证明 M7-08，不代表 M7-09、M7-10 或 Alpha Release Gate 已通过。

## CI 接线

- 用户已明确授权修改 M7-08 CI。
- `.github/workflows/ci.yml` 新增原生 Linux amd64/arm64
  `privileged-network-chaos` Job，与现有 verify Job 并行且不依赖其工作目录。
- 触发范围固定为非 PR Push、手动 `workflow_dispatch` 和每日 `18:30 UTC` Nightly；
  Pull Request 不执行特权网络修改。
- Job 固定 Go `1.27.0`、Node `24.19.0`，按仓库规则先执行 Web `npm ci/check/build`，
  再安装 `iproute2` 与 `nftables`。Runner 通过 root、最小 PATH 和仅当前 Checkout 的
  transient `safe.directory` 执行，不修改全局 Git 配置。
- 运行输出分为 amd64/arm64 两个 Artifact。Runner 失败时仍上传预检、控制台日志和
  已生成结果；成功时必须先校验 Runner 内部 `artifact-sha256.txt`。上传使用固定 Commit
  `actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02`（v4.6.2），保留
  14 天。
- PyYAML 结构读回、三个 Bash `run` Block 的 `bash -n`、`actionlint v1.7.12`、
  Action Tag/SHA 读回和 `git diff --check` 均已通过；正式 Workflow 已随 Commit
  `0f629f926ed3bdbbf9c698dab82130a1282e4731` 提交，并由 CI `#33583345819` 完成原生
  Linux amd64/arm64 特权执行。
