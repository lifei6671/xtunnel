# M7-08 Large Transfer/Privileged Network Chaos 交付证据

> 状态：`IN_PROGRESS`（生产链路 Harness、Builder 与 Runner 已落地；WSL2 netem
> 开发 Smoke 部分通过，TCP Reset 因内核缺少 `SOCK_DESTROY` 明确失败）

## 当前范围

- 起始 Head：`3316268813955a6b8320eec0c1b911fba19f9487`。
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

## 修复记录

- 首轮 Reset Harness 曾把活动流量的 20 秒读 Timeout 当成“已解阻”，并在等待 Origin
  时失败。现已显式拒绝实现 `net.Error` 且 `Timeout()==true` 的结果。
- loopback 的 nft `reject with tcp reset` 虽能命中并计数，但不会销毁同一内核里的现有
  socket。Runner 记录计数后先移除 reject table，避免它截获销毁过程生成的 RST，再以
  `ss -K` 执行精确 socket destroy 并复查连接；内核不支持时立即失败，不再等待测试
  Deadline。
- `smoke` 移除随机 Loss，只保留有界 Delay/Jitter/Rate，避免 1 秒 TCP Dial Deadline
  因 SYN 随机丢失产生开发态抖动；Loss 1%/5% 仍是 `full` 的必跑档，不能跳过。

## 未完成项

- clean checkout 的 Runner `full`：`NOT RUN`。
- 1 GiB 双向生产链路 Runtime：WSL2 dirty-development 定向档已通过；clean Runner
  `full` 仍为 `NOT RUN`。
- Loss 1%/5%、Jitter 50 ms、10 Mbit/s 完整矩阵：`NOT RUN`。
- 支持 `SOCK_DESTROY` 的原生 Linux TCP Reset/恢复：`NOT RUN`。
- Linux amd64/arm64 特权 Runner、精确 CI、Artifact 读回：`NOT RUN`。
- 当前 Target 的独立 Tier 3 复审、正式 Commit、推送和用户阶段复审：`NOT RUN`。

因此 M7-08 只能保持 `IN_PROGRESS`；本文件不能作为 M7-08、M7-10 或 Alpha Release
Gate 的通过证据。

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
- 当前已完成 PyYAML 结构读回、三个 Bash `run` Block 的 `bash -n`、
  `actionlint v1.7.12`、Action Tag/SHA 读回和 `git diff --check`；Workflow 尚未提交，
  精确 GitHub Actions 为 `NOT RUN`，因此任务状态仍为 `IN_PROGRESS`。
