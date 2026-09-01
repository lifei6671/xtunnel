# M7-04 Server Persistence/Filesystem Failpoints 交付证据

> 状态：`DONE`（最终实现 Commit、commit-bound clean `full`、原生 Linux Race、
> 精确 CI、Tier 3 commit-bound 最终独立复审与用户阶段复审均已通过）

## 证据边界

本任务只验证现有 Server durable operation 的异常传播与恢复收敛，不首次实现维护命令。
当前生产改动仅增加包内、未导出、按值传递的文件操作集合，让测试能在精确的
write/fsync/rename 边界返回 `EIO` 或 `ENOSPC`；默认入口仍调用原有真实系统操作，
不存在环境变量开关、全局可变 Hook 或公开测试 API。

当前测试结果分为三类，不能相互冒充：

- SQLite `SQLITE_FULL` 由数据库自身的 `PRAGMA max_page_count` 原生触发；
- Gateway、Backup 与 Restore 的 `EIO/ENOSPC` 是包内确定性 syscall-boundary 注入；
- Backup ACK 前 hard-exit、Gateway Key/Certificate rename 后与 Restore 两次目录切换后
  使用真实 Linux 子进程 `SIGKILL`，Restore 另以完整磁盘状态矩阵验证恢复。

上述证据不等于真实块设备故障、内核返回 EIO、宿主磁盘耗尽、断电或任意未设屏障
时刻的崩溃耐久性证明。

## 开发矩阵

| 分区 | 当前覆盖 | 核心不变量 |
| --- | --- | --- |
| SQLite Migration | 同一事务先成功建表，再由大 BLOB 写入原生触发 `SQLITE_FULL`；解除页数限制后重跑 | 失败后表整体回滚、版本不增加；重跑成功且 `integrity_check=ok` |
| Gateway Rotation | 临时文件/Journal 写入、Journal 与身份目录同步、Key/Cert rename 的 `EIO/ENOSPC`；真实 Key/Cert rename 后子进程 `SIGKILL` | 无 Journal 时保持旧完整身份；有效 Journal 可恢复到 after-state；Key/Cert 中间态 fail closed，不加载错配身份 |
| Backup Create | 归档 write、候选 fsync、发布 rename、发布后父目录 fsync；ACK 前真实 hard-exit | ACK 前最终路径不可见；活进程失败只清理本次候选；rename 后目录 fsync 失败保留已发布完整文件 |
| Restore Journal | 临时 write、文件 fsync、Journal rename、父目录 fsync；真实 target→rollback 与 staging→target rename 后子进程 `SIGKILL` | 失败后只能读取完整旧 phase 或完整新 phase；第一次切换后恢复旧 target，第二次切换后完成新 target；临时文件收敛；既有三阶段状态矩阵继续恢复 target/staging/rollback |
| 历史 Backup 候选 | 并发创建旁存在历史私有隐藏候选 | 不按 `.xtunnel-backup-pending-*` 前缀盲删，不误删未知 owner 的候选 |

## 当前验证

- 起始基线：`HEAD 17b94968b117de9002c25e3f427c0dc956ee9faf`，任务开始时工作区干净。
- 最终实现 Commit：`fdb7b3d02b72094564c417205b682b5fc9f71cf6`，Tree
  `1d96dc2986e57b861711f8f90eff77cdc9d9bf17`，Parent 为起始基线；已推送且
  远端 `master` 精确一致。Commit 恰好包含 14 个任务路径，Runner mode=`100755`。
- 工具链：Windows `go1.27.0`、`GOTOOLCHAIN=local` 检查通过。
- Windows 三包定向验证通过：

  ```text
  go test -count=1 -timeout=180s ./internal/repository/sqlite ./internal/server/gateway ./internal/server/durableops
  go test -race -count=1 -timeout=300s ./internal/repository/sqlite ./internal/server/gateway ./internal/server/durableops
  go vet ./internal/repository/sqlite ./internal/server/gateway ./internal/server/durableops
  ```

- SQLite 目标测试 `count=20`、目标 Race `count=10` 与包级 Test/Vet 通过。
- Gateway 分区的 Windows Test/Race/Vet 通过；Linux amd64、`CGO_ENABLED=0` Test
  Binary 在 WSL2 Linux-native `/tmp` 的目标测试与包级测试通过，Linux Vet 通过。
- Durableops `linux/amd64`、`GOAMD64=v1`、`CGO_ENABLED=0` Test Binary 在 WSL2
  运行五个定向场景并返回 `PASS`，包括 Backup hard-exit、发布 Failpoint、历史候选保留、
  Restore Journal Failpoint 与 interrupted-state matrix。
- M7-04 Builder 已生成 SQLite/Gateway/Durableops 三个 Linux Test Binary 与 Manifest；
  WSL2 Linux-native `/tmp` prebuilt `smoke` 三分区全部通过，PowerShell Parser/ASCII、
  `sh -n`、`dash -n` 与 `shellcheck -s sh` 通过。
- 当前 14 个交付文件复制到独立本地 clone，并以一次性快照 Commit
  `b64a8b5994e36a7af62439c54689461788f8530e`（Tree
  `8964e2f32dfdf2efa6091e5221b784f9ce71cd6d`）冻结。源文件 SHA-256、快照 Blob 与
  复制前后源文件哈希一致；Runner mode=`100755`，Manifest=`worktree_clean=true`。
  同一快照的 WSL2 Linux-native `/tmp` prebuilt `full` 三分区全部通过，包含四个真实
  rename 后 `SIGKILL` 场景；`full.log` SHA-256 为
  `15121d2491d83c3471a69fe580c10f1562e4fbcea9cc5112b218971e76b6dd40`。
  该一次性本地 Commit 不在项目 refs 中，不替代最终项目 Commit 或 CI。
- 正式 Commit 另以固定 Go `1.27.0`、`GOTOOLCHAIN=local` 构建 `linux/amd64`、
  `GOAMD64=v1`、`CGO_ENABLED=0` 三个 Test Binary；Manifest
  `85226DD2269515798DEB33596738737E997EE308376216F08578113F9C003565`
  记录 `worktree_clean=true`。同一产物在 WSL2 Linux-native `/tmp` 的 `full`
  三分区全部通过，包含四个真实 rename 后 `SIGKILL` 场景。
- 官方 `golang:1.27.0-bookworm` 镜像 Digest
  `sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452`，
  Linux amd64、CGO=1、`GOTOOLCHAIN=local`。对正式 Commit 的 `git archive`
  快照执行以下 Race，三包全部通过：

  ```text
  go test -race -count=1 -timeout 300s ./internal/repository/sqlite ./internal/server/gateway ./internal/server/durableops
  ```

- [GitHub Actions #33468280052](https://github.com/lifei6671/xtunnel/actions/runs/33468280052)
  的 Head SHA 精确匹配正式 Commit，结论 `completed/success`；Linux amd64、Linux
  arm64、Windows Agent service 与 Windows arm64 Agent runtime 四个 Job 全部成功。
  CI 自身的 Linux Race 白名单不包含本次三个包，因此本任务 Race 结论来自上一条
  独立原生 Linux 运行，不混同为 CI Race。
- 正式 Commit 范围 `git diff --check` 通过；工作区在提交、推送与复审时干净。

## Checkpoint 复审

- SQLite：`CHILD_AGENT / Tier 3 / FULL_SCOPE` 初审发现 1 项 P1——必须证明
  `SQLITE_FULL` 发生在事务内 DDL 成功之后。修复加入明确屏障并扩大 payload 后，
  Repair round 1 复审 `PASSED`，Coverage=`COMPLETE`、Freshness=`FRESH`，
  P0/P1/P2=`0/0/0`。
- Backup/Restore：`CHILD_AGENT / Tier 3 / FULL_SCOPE`，Gate=`PASSED`，
  Coverage=`COMPLETE`、Freshness=`FRESH`，P0/P1/P2=`0/0/0`。
- Gateway Rotation：初审发现 1 项 P1——部分 Journal 写入失败后只保持旧身份可读，
  却未收敛会永久阻塞启动的残留。修复在任何身份 rename 前精确回滚当前操作拥有的
  Journal/Key/Cert 临时路径并同步目录；Repair round 1 复审 `PASSED`，
  Coverage=`COMPLETE`、Freshness=`FRESH`，P0/P1/P2=`0/0/0`。
- Linux Harness：`CHILD_AGENT / Tier 3 / FULL_SCOPE`，Gate=`PASSED`，
  Coverage=`COMPLETE`、Freshness=`FRESH`，P0/P1/P2=`0/0/0`；开发态 Builder 与
  WSL2 prebuilt `smoke` 已通过，clean `full` 未运行。
- 前一确定性 Failpoint checkpoint 的完整 Target 集成：`CHILD_AGENT / Standard Mode /
  Tier 3 / PARTITIONED_PLUS_INTEGRATION`，Gate=`PASSED`，Coverage=`COMPLETE`、
  P0/P1/P2=`0/0/0`。新增真实 `SIGKILL` 与 Runner `full` 接线改变了 Target，
  因此该结论不再作为当前终态 Freshness。
- 当前完整 Target 集成：`MIXED / Standard Mode / Tier 3 /
  PARTITIONED_PLUS_INTEGRATION`，Integration mode=`CHILD_AGENT`，Gate=`PASSED`，
  Coverage=`COMPLETE`、Freshness=`FRESH`，P0/P1/P2=`0/0/0`。首轮发现一项证据
  P1：误把不可用的 Linux Race 写为已通过；修复后 Repair round 1 复审关闭。
- 正式 Commit-bound 最终复审：`CHILD_AGENT / Standard Mode / Tier 3 /
  PARTITIONED_PLUS_INTEGRATION`，Gate=`PASSED`，Coverage=`COMPLETE`、
  Freshness=`FRESH`，P0/P1/P2=`0/0/0`，无 Finding。复审确认正式 Commit tree、
  14 个任务路径、14/14 Git Blob、Runner mode、远端 SHA、精确 CI 与独立 Linux
  Race 一致；该结论只支持进入 `REVIEW`，不替代用户阶段批准。

## 用户阶段复审

- 用户已明确回复“`M7-04 阶段复审通过`”。最终实现 Commit
  `fdb7b3d02b72094564c417205b682b5fc9f71cf6`、WSL2 clean Runner `full`、Docker
  Linux amd64/CGO=1 三包 Race、
  [CI #33468280052](https://github.com/lifei6671/xtunnel/actions/runs/33468280052)、
  commit-bound Tier 3 最终独立复审，以及证据 Head
  `806bfa0d719259642dc152a0b96f80894b0cd637` 的
  [CI #33469332157](https://github.com/lifei6671/xtunnel/actions/runs/33469332157)
  共同构成 M7-04 `DONE` 证据。
- 状态影响：M7-04 从 `REVIEW` 转为 `DONE`，M7 为 `4/10 IN_PROGRESS`，全局
  `DONE` 更新为 `89/95`；M7-09 的任务级依赖已满足并转为 `READY`。本次未启动
  M7-05 至 M7-09，也未勾选 Alpha Release Gate Checklist。

## 隐藏候选清理策略评估

SIGKILL 可能留下本次进程创建的私有 `0600` 隐藏候选。当前文件名不绑定最终输出名、
创建时间、owner lease 或操作 ID，因此自动按前缀扫描删除无法区分历史残留与并发 Create。
M7-04 的安全结论是继续保留 fail-closed 行为：生产代码不自动扫描、不盲删，也不新增
维护命令。任何自动清理协议必须先冻结 owner 身份、并发排除、目标绑定与审计语义，
并单独取得维护命令/长期行为变更授权。

## 剩余边界

- 仍缺真实存储层的 `EIO/ENOSPC`、SQLite WAL/COMMIT fsync 中断、断电恢复与任意时刻
  崩溃证据；本轮未在专用可销毁块设备或断电仿真环境运行，不记录为通过。
- 最终项目 Commit、commit-bound clean `full`、原生 Linux Race、精确 CI、最终独立
  复审与用户阶段复审均已通过。
- M7-04 已 `DONE`；全局 `DONE` 为 `89/95`，M7 Alpha Gate Checklist 不勾选，
  M7-09 已解锁为 `READY`。
