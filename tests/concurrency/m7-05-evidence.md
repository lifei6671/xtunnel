# M7-05 Race/Concurrency Suite 交付证据

> 状态：`REVIEW`（正式实现与 Linux arm64 Race 超时修复 Commit 均已推送；Windows
> 全仓 Race/Vet、隔离 Linux clean `full`、Linux amd64/arm64 全仓 Race 精确 CI 与独立
> 复审已闭环；等待用户阶段复审）

## 证据边界

本任务只补现有并发 owner 的组合回归、全仓 Race 入口与诊断 Profile，不改变生产锁、
Connector Selection 算法、公共 API、Schema、Migration、依赖、配置默认值或日志契约。

- Race 只证明本次实际执行路径未观测到 data race；不证明没有死锁，也不穷举全部
  goroutine 调度交错。
- SQLite 组合测试通过同一启动屏障并发进入 Config Write、Usage Flush 与 Token Rotate，
  但调度器不保证每轮三者同时位于数据库临界区。
- Pinned TLS 测试覆盖运行期正式续签发布、`GetCertificate`、Metric 与真实 TLS 握手；
  Public 身份当前没有运行时发布入口，因此只验证启动时不可变身份的并发读取。
- CPU、Mutex 与 Block Profile 是 Docker Desktop LinuxKit 环境的诊断样本，不设性能
  阈值，也不授权据此修改生产锁或选择算法。Block Profile 的完整摘要包含 fixture 拥有的
  Control Session 等待，必须与 Selection Focus 摘要分开解释。

## 开发矩阵

| 分区 | 当前覆盖 | 核心不变量 |
| --- | --- | --- |
| Session/Config/Usage/Listener | 全仓 Race 与关键 owner 定向 Race | 已有 owner、generation、取消和退出路径在 Race 下无报告 |
| SQLite 组合写 | 真实 Store 上 Config Write + Usage Flush + Token Rotate 同屏障启动 | 无未处理 `SQLITE_BUSY`；配置 revision、Usage totals、Token v2 与旧 Token 失效精确回读 |
| Gateway TLS | Pinned 正式续签发布跨 32 reader；Public 24 reader 并发读取 | 证书链、Leaf、PrivateKey 始终完整配对；各正式入口分别观察完整旧、新代；旧连接不变 |
| Connector Selection | `b.RunParallel`，1/8/32/100 Connector，CPU 1/8/32 | 复用真实 `Proxy.selectConnector`，每轮归还 Lease，异常路径清理 Membership |
| Profile Runner | clean `full`：Web、全仓 Race、主 Benchmark、独立 CPU/Mutex/Block Profile | 结果目录 fail closed；三个 `.test` 与 `.pprof` 留在仓库外；仓库根无忽略二进制 |

## 当前验证

- 起始基线：`10a6df1001c7614e6af273bc114f04f455639d16`，任务开始时工作区干净。
- Windows 工具链：`go1.27.0`、`GOTOOLCHAIN=local` 检查通过。
- Windows 全仓 Race：

  ```text
  go test -race -count=1 -timeout=600s ./...
  ```

  当前实现树已通过；该结果不替代 Linux Race 或 CI。提交前重验首次运行曾在
  `TestServerPinnedRenewalPublishesCompleteIdentityToConcurrentHandshakes` 超时失败；
  定向 Race `count=50` 复现 4 次，goroutine dump 证明续签失败后阻塞在错误状态发布。
  根因是测试每毫秒重读证书，与 Windows 原子替换竞争。测试改为用 `TryRLock` 等待
  正式续签离开无锁磁盘阶段并排队状态写锁；释放 reader 后通过 `LastRenewalError` 与
  磁盘回读确认续签成功。修复后目标 Race `count=50`、Gateway package Race `count=5`
  和本条全仓 Race 均通过。
- Windows `go vet ./...` 通过；Gateway TLS 目标 Race `count=5`、SQLite 组合 Race
  `count=20`、并发 Connector Selection Race Benchmark 均通过。
- Runner 的 `sh -n`、`dash -n` 与 `shellcheck -s sh` 通过；不存在/既有空结果目录的
  `smoke` 通过，非空结果目录在写入前拒绝且保留原内容。
- 用户明确确认 CI/CD 修改后，Linux amd64/arm64 `verify` 的 Race 已从九包白名单替换为
  `go test -race -count=1 -timeout 600s ./...`。PyYAML 6.0.3 解析通过，精确文本断言确认
  新命令恰好一处、旧 `300s` Race 命令为零，Workflow diff-check 通过。
- 正式实现 Commit `af3c92d755fb9a27f3e95c85428d26e0852dd95f` 已推送；精确
  [CI #33477763140](https://github.com/lifei6671/xtunnel/actions/runs/33477763140)
  中 Linux amd64 与两个 Windows Job 通过，Linux arm64 在新增的全仓 Race 阶段失败：
  `TestBackupBarrierBlocksCreateFirstAdmin` 的通用 1 秒结果等待不足以覆盖 arm64 Race 下
  事务内密码哈希，普通 `go test ./...` 已先通过。该 Run 整体为失败，不能记录为 CI PASS。
- arm64 修复只为该测试的首管创建增加 10 秒 Context，并以 Context Deadline 等待
  结果，不改变产品代码或其他通用同步断言。修复后 Windows 目标 Race `count=20`、
  SQLite package Race `count=3`、SQLite Vet、全仓 Race 与全仓 Vet 均通过。修复 Commit
  `dcd0f551f7c37a70d303ebe06cf69734bfc718cb` 已推送，Tree
  `0325cfc87190ddac117a64ba761ca7c4d7f4edc0`；Head SHA 精确匹配的
  [CI #33479628065](https://github.com/lifei6671/xtunnel/actions/runs/33479628065)
  四 Job 全部成功，Linux amd64/arm64 的全仓 Race 均通过。
- 当时六个实现/状态文档路径复制到隔离 clone，以一次性本地快照 Commit
  `b99bdca7ba599f6bde939421f7a86eccc6de3cfd`、Tree
  `3f96e44aef2bc70eaff9666c3fde619211e25218` 冻结。该 Commit 不在项目 refs 中，
  不替代正式项目 Commit 或 CI；Runner mode 在快照树中为 `100755`。
- 快照环境：Docker Desktop LinuxKit amd64、`CGO_ENABLED=1`、Go `1.27.0/local`、
  Node `24.19.0`、npm `11.17.0`、32 logical CPU。基础镜像使用仓库固定 Digest：
  - Go：`sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466`
  - Node：`sha256:3638d9a6fe4030bd716be989438248074489337ba3275657f93595428be4fc03`
- 同一 clean 快照运行：

  ```text
  ./tests/concurrency/run-m7-05.sh -m full -o /results2
  ```

  Web `npm ci/check/build`、Linux amd64/CGO=1 全仓 Race、主 Benchmark 与三个独立
  Profile 均通过；运行后工作树仍干净，仓库根 `tunnel.test=absent`。结果目录包含三份
  独立 `.test`、三份 `.pprof`、完整命令输出与 `pprof -top`/Focus 摘要。
- 100 Connector 主 Benchmark 的五次样本：CPU=1 为约 `35.5-36.2 us/op`，CPU=8
  为约 `30.2-31.0 us/op`，CPU=32 为约 `31.0-32.8 us/op`；均为 `10 allocs/op`，
  约 `19.3 KB/op`。这些数值只描述本次虚拟化环境，不是门槛或跨环境比较结论。
- Mutex Focus 覆盖 `selectConnector`、`acquireConnectorWhere`、`releaseConnector` 与
  `Pools`；Block 完整摘要主要包含 fixture 的 Control Session 等待，Selection Focus
  仅占完整 Block 累计延迟的一部分。当前只记录归因入口，不据此修改实现。
- 关键结果 SHA-256：
  - `environment.txt`：`b2366fb6a9cbaa2856fd8d1f3324b36a7c68b192bfa230e47b3e7ef560d34d35`
  - `full-race.txt`：`48c9aa37f1aeaa7f07eccdf5695c03e6c104097d2ce8f4435401d4c1ff1f0ebe`
  - `connector-selection-benchmark.txt`：`f6f3d58a0526e82204fd96713e770dcc9b6c017ec60422347f649b16a6568e2a`
  - CPU Profile：`217767e5b21177047fd0271dc69a9d5eb6bd991a5edf6d0d97af9eb7a2218484`
  - Mutex Profile：`3b748195808ff268e91d0db5d01eb54f99bbbf5fa9384127e165b47aaf49e0f2`
  - Block Profile：`608fc448cc9b860cfd5e5d74ee9e229ce0d1f859f480f177f02a51c41aa12029`

## Checkpoint 复审

- Gateway TLS：`CHILD_AGENT / Tier 3 / FULL_SCOPE` 初审发现 1 项 P1——直接持锁的
  旧观察不能证明正式读取路径跨越发布。Repair round 1 改为每类正式入口在续签前、
  发布后分别观察完整旧、新代；当时的 fresh 复审 `PASSED`，P0/P1/P2=`0/0/0`。
  提交前重验随后把磁盘轮询改为 `TryRLock` 发布屏障，该旧结论对 Gateway 文件已 stale；
  修复后验证通过，当前内容仍须纳入正式 Commit 的 commit-bound 最终独立复审。
- SQLite 组合写：`CHILD_AGENT / Tier 3 / FULL_SCOPE`，Coverage=`COMPLETE`、
  Freshness=`FRESH`、Gate=`PASSED`，P0/P1/P2=`0/0/0`。
- Benchmark/Runner：初审发现 1 项 P1（最终文件模式）与 2 项 P2（结果覆盖、忽略的
  `tunnel.test` 遗留）。Repair round 1 已关闭两项 P2，内容 Gate=`PASSED`，
  P0/P1/P2(content)=`0/0/0`；隔离 clean 快照已验证 mode=`100755` 且仓库根无遗留
  二进制。正式项目提交树仍必须再次核对 mode=`100755`。
- CI：`CHILD_AGENT / Tier 3 / Build-CI surface`，按 Git blob
  `7fafd2cc191613ff8e02fc9ff63299645ab8f3f7` 完成 fresh 分区复审；
  Coverage=`COMPLETE`、Freshness=`FRESH`、Gate=`PASSED`，P0/P1/P2=`0/0/0`。
  本次没有触发 GitHub Actions，该内容 Gate 不能替代正式 Commit 的精确 CI Run。
- 文档同步：`CHILD_AGENT / docs-sync` 按 CI、开发计划和本证据文件的当前候选内容完成
  fresh 审计；Coverage=`COMPLETE`、Freshness=`FRESH`、Gate=`PASSED`，
  P0/P1/P2=`0/0/0`，确认 M7-05、M7 与 Alpha Gate 均未越权晋级。
- CI 接线前的 7 路径未提交候选曾通过集成复审；因 CI 与文档随后纳入 Target，该结论
  不再覆盖完整 8 路径候选，只保留为未变分区的历史证据。
- 正式实现 Commit `af3c92d755fb9a27f3e95c85428d26e0852dd95f` 的 commit-bound
  Tier 3 独立复审已通过，Coverage=`COMPLETE`、Freshness=`FRESH`、
  P0/P1/P2=`0/0/0`，提交树中 Runner mode=`100755`。
- arm64 修复候选独立复审为 `PASSED`、P0/P1/P2=`0/0/0`。修复 Commit `dcd0f55...`
  的 commit-bound 读回未发现代码问题，但因提交树仍写“修复候选尚待提交”记录 1 项
  文档 P2；当前 docs-only 收口已修正该时态并记录实际推送与精确 CI，不修改实现。

## 阶段边界

- Runner 已在正式实现 Commit 中固定为 `100755`；修复 Commit 已推送，精确 CI 四 Job
  全部成功，两个原生 Linux Runner 的全仓 Race 均通过。
- M7-05 当前为 `REVIEW`，仅等待用户阶段复审；用户明确批准前不得标记 `DONE`，不得
  启动 M7-06，也不得勾选 Alpha Release Gate。
