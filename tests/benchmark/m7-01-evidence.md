# M7-01 调优证据

> 状态：`FORMAL_SAMPLE_COMPLETE_AWAITING_PRODUCTION_AUTH`

## 证据边界

- Benchmark 源码 Commit：`a1fef7ade670a529860b23fdb5485c7d42b61c2b`，工作树为空。
- 正式采样：WSL2 Linux amd64，三组主结果均使用 `2s × 5`；Syscall、GNU time、
  CPU/Heap Profile 与 GC 使用独立进程采集，避免分析器污染主吞吐结果。
- 结果目录：`/tmp/xtunnel-m7-01-checkout.iVasUp/results`。该目录是本地主机临时原始证据，
  本文只固化可复核摘要；Runner 可在同类受控环境重复生成全部原始文件。
- 本次没有修改生产复制路径、Connector 选择算法、Server Schema/Repository 默认值、
  公共 API/Protocol、依赖、Lockfile 或 CI/CD；正式采样完成不等于冻结实现契约已闭环。

## 环境与命令

- Kernel：`6.18.33.2-microsoft-standard-WSL2`，`x86_64`；不是裸机 Linux 证据。
- CPU：13th Gen Intel Core i9-13900KF，16 Core / 32 Logical CPU；`GOMAXPROCS` 未设置。
- RAM：33,552,023,552 Bytes；Swap 8 GiB。
- FD：Soft 10,240，Hard 1,048,576；Kernel `file-max=9223372036854775807`。
- Go：`go1.27.0`、`GOTOOLCHAIN=local`、`GOOS=linux`、`GOARCH=amd64`、
  `GOAMD64=v1`、`CGO_ENABLED=0`。
- 网络与负载：全部使用同一 WSL2 Guest 内的 TCP Loopback，因此外部 RTT/Bandwidth 不适用；
  Proxy 每次操作单向复制 8 MiB（64 KiB 写块），HTTP 每个请求返回 1 KiB 并分别保持
  1/16/64/100/128 条并发连接，Connector 选择为无网络 IO 的串行 1/8/32/100 Pool 热路径。
- Prebuilt Manifest SHA-256：
  `56790f5ce3c20a292333264e9759da40c9d28894ad59bb7fe377bc018c0ceebd`；三个测试
  Binary 的哈希均在 Windows 构建目录和 Linux-native `/tmp` 暂存目录重复校验。

```powershell
./tests/benchmark/build-m7-01-linux.ps1 `
  -OutputDirectory C:\Users\lifei\AppData\Local\Temp\xtunnel-m7-01-a1fef7a
```

源码通过完整 Git Bundle 复制到 WSL-native `/tmp`，从 Bundle 创建干净 checkout 并固定到
上述 Commit；正式命令为：

```sh
./tests/benchmark/run-m7-01.sh -m full \
  -b /mnt/c/Users/lifei/AppData/Local/Temp/xtunnel-m7-01-a1fef7a \
  -o /tmp/xtunnel-m7-01-checkout.iVasUp/results
```

## Proxy Buffer

主结果的 Throughput 原始五次值和中位数如下，单位为 MB/s：

| 路径 | 五次原始值 | 中位数 |
| --- | --- | ---: |
| `io.Copy` TCP Fast Path | 6945.56 / 6808.37 / 6950.52 / 7231.43 / 7009.26 | 6950.52 |
| Generic 32 KiB Baseline | 5989.11 / 6430.90 / 6551.66 / 6199.62 / 6646.60 | 6430.90 |
| Pooled 16 KiB | 3226.93 / 3205.76 / 3158.90 / 4027.72 / 3209.80 | 3209.80 |
| Pooled 32 KiB | 6554.71 / 4488.49 / 6691.84 / 6924.61 / 4646.87 | 6554.71 |
| Pooled 64 KiB | 7244.17 / 7429.41 / 7386.11 / 5820.18 / 6832.47 | 7244.17 |

为保证 time、Syscall、Profile 和 GC 都能精确归属到单个尺寸，Runner 又分别执行了五次：

| Pool | 五次原始 MB/s | 中位数 | RSS KiB | Syscall 总数 | Voluntary CS | GC Trace 行 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| 16 KiB | 3231.65 / 3241.78 / 3301.97 / 4637.27 / 3253.55 | 3253.55 | 5440 | 3548 | 454652 | 5 |
| 32 KiB | 6641.76 / 6293.63 / 6427.39 / 4644.34 / 4505.61 | 6293.63 | 5684 | 2899 | 234743 | 5 |
| 64 KiB | 7482.94 / 7320.71 / 6392.09 / 7545.25 / 5950.81 | 7320.71 | 6208 | 2677 | 129488 | 5 |

- 64 KiB 的独立中位吞吐比 32 KiB 高约 16.3%，Syscall 少约 7.7%，代价是最大 RSS
  增加 524 KiB；三种尺寸均为约 3 allocs/op、约 78–98 B/op，且都只记录到 5 行
  启动/强制 GC Trace，没有持续 GC 压力信号。
- 16/32/64 KiB CPU Profile 的首要 Flat Hotspot 均为 `linux.Syscall6`，分别约
  89.31% / 90.23% / 92.65%；尺寸增大主要减少 Read/Write 与 Context Switch，而不是
  消除内核调用成本。
- 32 KiB 与 64 KiB 均有明显离散；这是 WSL2 Loopback、固定 8 MiB Payload 的单机结果。
  当前生产 TCP `io.Copy` Fast Path 中位数已达 6950.52 MB/s，且 Generic Benchmark
  刻意屏蔽 `WriterTo`/`ReaderFrom`。因此该数据只能把 64 KiB 标记为后续复验候选，
  不能单独支持把冻结的 32 KiB 技术基线改为 64 KiB。

## Connector Selection

| Connector 数 | 五次 ns/op | 中位 ns/op | B/op | allocs/op |
| ---: | --- | ---: | ---: | ---: |
| 1 | 753.8 / 743.4 / 740.6 / 743.5 / 751.1 | 743.5 | 772 | 5 |
| 8 | 2191 / 2195 / 2187 / 2203 / 2221 | 2195 | 1156 | 5 |
| 32 | 9598 / 9598 / 9441 / 9434 / 9350 | 9441 | 8916 | 10 |
| 100 | 27858 / 28218 / 27882 / 27829 / 27790 | 27858 | 19284 | 10 |

- 全组 GNU time：User 71.45s、System 10.50s、CPU 154%、Elapsed 53.04s、
  Max RSS 32,668 KiB；一次独立 Strace 共 2479 个 Syscall。
- CPU Profile 中 `sessionEligibleLocked` 约占 2.89% Flat / 16.86% Cumulative；
  Heap `alloc_space` 主要来自 `sessionruntime.Manager.Pools` 约 61.98% 与
  `runtime.Registry.acquireConnectorWhere` 约 37.75%。
- 100 Connector 的选择中位数约 27.9 us，当前没有冻结的性能失败阈值。Profile 清楚标出
  后续优化候选，但不足以授权修改 Registry/Session Manager 的生产所有权与选择算法。

## HTTP/1.1 WorkConn Capacity

Benchmark 通过真实 `transportPool -> http.Transport.RoundTrip -> Proxy.Dial ->
Registry/Session Manager/WorkPool -> OPEN -> HTTP/1.1` 路径运行：

| 并发 | 五次 requests/s | 中位 requests/s | Peak/Established WorkConn | KeepAlive | 结束 Active/Total |
| ---: | --- | ---: | --- | ---: | --- |
| 1 | 73656 / 74277 / 74183 / 74692 / 73276 | 74183 | 1 / 1 | 100% | 0 / 0 |
| 16 | 128838 / 129365 / 131411 / 130473 / 131696 | 130473 | 16 / 16 | 100% | 0 / 0 |
| 64 | 176715 / 182360 / 181185 / 176853 / 136250 | 176853 | 64 / 64 | 100% | 0 / 0 |
| 100 | 176205 / 197584 / 199872 / 200213 / 201083 | 199872 | 100 / 100 | 100% | 0 / 0 |
| 128 | 204466 / 204540 / 206114 / 205692 / 202380 | 204540 | 128 / 128 | 100% | 0 / 0 |

- 计时前完成建连，故 `workconn_dials/request=0`；每个并发度都建立并达到同数量的真实
  WorkConn 峰值，随后全部 KeepAlive 复用并在结束时收敛到零。
- 全组 GNU time：User 214.97s、System 37.60s、CPU 372%、Elapsed 67.79s、
  Max RSS 42,428 KiB；一次独立 Strace 共 3458 个 Syscall。CPU/Heap 主要由 Go Runtime
  调度、Futex/Select 与 `net/http` 请求/Transport 分配构成。
- 并发 64 有一次 136,250 requests/s 离群值，但资源生命周期断言全部通过。当前固定环境
  已验证 128 条活动 WorkConn；既有 `http_max_idle_connections=100` 是每隔离池空闲连接
  保留量，不是活动 WorkConn 硬上限，因此没有证据支持调整该 Schema 默认值。

## 失败注入与采样修复记录

- 首轮长采样暴露 Benchmark Fixture 在未运行 Agent Heartbeat Writer 时触发 30 秒
  Heartbeat Timeout，以及长测试进程缺少自身 Timeout。修复后 Fixture 使用仅限 Benchmark
  的 10 分钟/30 分钟窗口，并为 Smoke/Full 分别增加 1 分钟/5 分钟进程上限；提交
  `f7ae01a575db3e877f093ffed72bf84e2d1734e6`。
- 第二轮在 DrvFS 上出现测试 Binary 已输出 `PASS` 但进程退出挂起。Runner 改为先复制到
  Linux-native `/tmp`、再次校验哈希并从原生文件系统执行；提交
  `a1fef7ade670a529860b23fdb5485c7d42b61c2b`。
- 从 Windows 工作树的 `.git` 直接在 WSL Clone 曾出现 Pack Inflate 错误；正式源码改用
  `git bundle verify` 通过的完整 Bundle 复制到 WSL-native `/tmp` 后创建干净 checkout。
  该问题属于跨文件系统证据装配，不是产品或 Benchmark 失败。

## 调优决策

1. **保留 32 KiB 技术基线，不采用 64 KiB。** 64 KiB 是当前 WSL2 Generic Fallback
   的最佳候选，但离散度、虚拟化环境和传输类型覆盖不足以支持修改冻结基线。
2. **生产 Buffer 契约仍需实现。** 总技术方案已经冻结数据代理使用 32 KiB `sync.Pool`；
   当前生产路径仍是裸 `io.Copy`。Benchmark 证明 Generic 32 KiB 从约 32,834 B/op、
   4 allocs/op 降至约 78–88 B/op、3 allocs/op。推荐下一范围只实现“显式保留
   `WriterTo`/`ReaderFrom` 快路径 + 32 KiB pooled Generic Fallback”及关键测试，不改变
   公开 API、协议、Schema、默认值、依赖或其他生产算法；该生产变更需用户另行授权。
3. **不调整 Connector 生产算法。** 将 `Sessions.Pools` 快照复制和 Registry 获取分配记录为
   后续 M7-05 Profile/并发证据的候选热点，不提前跨任务优化。
4. **不调整 HTTP/WorkConn 默认值。** 128 并发已验证，且 100 的现有语义不是活动硬上限。
5. 若未来要引入 64 KiB Pool、改变 Connector 选择实现或调整 Server Schema/Repository
   默认值，必须先在原生 Linux、TLS/包装连接和代表性 Payload/网络条件下复测，并分别取得
   生产实现或配置契约授权。

结论：M7-01 的三组真实路径 Benchmark、正式环境、五次结果、CPU/RSS/Syscall/Heap/GC/FD
解释和“保持默认值”调优决策已经齐备；但 32 KiB `sync.Pool` 冻结实现尚未落地，M7-01
保持 `IN_PROGRESS`，等待推荐生产范围授权，不能提前进入阶段复审。
