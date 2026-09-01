# M7-04 Server Persistence/Filesystem Failpoints 开发证据

> 状态：`IN_PROGRESS`（SQLite、Gateway Rotation、Backup/Restore 的确定性
> Failpoint 与目录切换真实子进程 `SIGKILL` 已实现，隔离 clean `full` 与 Tier 3
> 集成复审已通过；最终项目 Commit/CI 与 Linux Race 尚未闭环）

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
  当前 WSL2 未安装 Go，Linux Race 为 `UNAVAILABLE`，仍需最终 CI 补齐。
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
- `git diff --check` 通过；当前只出现仓库既有的 Windows LF/CRLF 工作树提示。

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

## 隐藏候选清理策略评估

SIGKILL 可能留下本次进程创建的私有 `0600` 隐藏候选。当前文件名不绑定最终输出名、
创建时间、owner lease 或操作 ID，因此自动按前缀扫描删除无法区分历史残留与并发 Create。
M7-04 的安全结论是继续保留 fail-closed 行为：生产代码不自动扫描、不盲删，也不新增
维护命令。任何自动清理协议必须先冻结 owner 身份、并发排除、目标绑定与审计语义，
并单独取得维护命令/长期行为变更授权。

## 剩余边界

- 仍缺真实存储层的 `EIO/ENOSPC`、SQLite WAL/COMMIT fsync 中断、断电恢复与任意时刻
  崩溃证据；当前环境无提权、挂载和隔离块设备权限，不能安全补齐这些证据。
- 隔离开发态 clean `full` 与终态集成复审已通过；仍缺最终项目 Commit、精确 CI 与
  Linux Race。
- M7-04 保持 `IN_PROGRESS`；M7 Alpha Gate Checklist 不勾选，M7-09 继续等待
  M7-04 `DONE`。
