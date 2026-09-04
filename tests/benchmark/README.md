# M7-01 Benchmark

本目录保存 M7-01 的可复现运行入口和人工复审证据。基准只测量当前真实产品路径，
不定义新的配置项、默认值或性能门槛。

## 测量分区

- `Proxy Buffer`：把当前 `io.Copy` TCP 快路径与明确屏蔽
  `io.WriterTo`/`io.ReaderFrom` 的 Generic Buffered Path 分开；只有后者比较
  16/32/64 KiB，避免传入 Buffer 被 Go 快路径忽略。
- `Connector Selection`：调用真实 `Proxy.selectConnector` 路径，包含 Ready Pool
  快照复制、Registry 选择、Eligibility Predicate 与 Pool Snapshot。
- `HTTP/1.1 WorkConn Capacity`：通过真实 Transport 的 `RoundTrip` 测量活动连接峰值、
  Keep-Alive 复用和清理收敛。`MaxIdleConnections` 只表示空闲连接保留量，不能解释成
  活动 WorkConn 硬上限。

三组基准必须独立运行。Connector Selection 保持串行；锁竞争、Mutex/Block Profile
属于 M7-05，不在 M7-01 中混入。

## 运行方式

快速验证 Benchmark 能被构建和执行：

```sh
./tests/benchmark/run-m7-01.sh -m smoke
```

正式采样必须在固定 Linux amd64 环境、干净提交和空闲主机上运行：

```sh
./tests/benchmark/run-m7-01.sh -m full -o /tmp/xtunnel-m7-01
```

`full` 模式需要项目固定的 Go 1.27、`strace` 和 GNU
`/usr/bin/time`，但脚本不会安装或修改主机工具。它会记录 Commit、工作区状态、
Go/OS/CPU/RAM/FD/GOMAXPROCS，并分别采集：

- 无 `strace` 的 Throughput、CPU/RSS 与 Allocation 主结果；
- 对 pooled 16/32/64 KiB 使用精确 Benchmark 正则分别采集 GNU `time`、Syscall、
  CPU/Heap Profile 和 `GODEBUG=gctrace=1` 原始 GC 指标；
- GC 汇总文件记录每个尺寸的 `gctrace_lines`，原始文件保留暂停、CPU 和 Heap 变化细节。

分析器会显著改变耗时，因此不得把 `strace`、Profile 或 GC 轮次与主吞吐结果混合比较。
每个 smoke 测试进程使用 1 分钟超时；`full` 的主结果、`strace`、Profile 和 GC 测试进程
分别使用 5 分钟超时，超时即失败。该限制按单个测试进程计算，不是整个脚本的总运行时限。

Windows 主机可用项目固定工具链交叉编译 Linux amd64 Benchmark，再交给已有的 Linux
环境运行；这个路径不会在 Linux 中下载或切换 Go：

```powershell
./tests/benchmark/build-m7-01-linux.ps1 -OutputDirectory C:\Temp\xtunnel-m7-01-bin
```

```sh
./tests/benchmark/run-m7-01.sh -m full \
  -b /mnt/c/Temp/xtunnel-m7-01-bin \
  -o /tmp/xtunnel-m7-01
```

交叉编译入口兼容 Windows PowerShell 5.1，并以 UTF-8 无 BOM、LF 换行写出清单。清单记录源码
Commit、工作区状态、Go/GOOS/GOARCH 和三个 Binary SHA-256。Prebuilt `full` 模式会
fail-closed 校验稳定 `go1.27.1+`/`local/linux/amd64/v1/CGO=0`、相同干净 Commit 与三个哈希，
随后把清单与三个 Binary 复制到 Linux-native `/tmp` 临时目录，设置最小权限并再次校验
SHA-256；`full` 的所有测试只执行这些临时副本，避免从 WSL DrvFS 长跑执行。脚本退出时
只按精确文件名清理副本并用非递归 `rmdir` 删除临时目录。已校验清单及其 SHA-256 仍会
固化到结果目录。Smoke 保持直接执行原路径。Prebuilt 模式只保存 CPU/Heap Profile，不使用
运行环境中偶然存在但未经校验的 Go；Profile 应带回受控环境，用清单记录的同版工具链
执行 `go tool pprof -top`。运行环境若为 WSL2，证据中必须明确标注，不能写成原生裸机结果。

## 复审规则

正式证据至少包含：

1. 干净 Commit SHA、完整环境和完整命令；
2. 每组五次原始结果及离散程度；
3. Throughput、CPU、Syscall、RSS、Heap、GC 和 FD 解释；
4. HTTP/1.1 峰值连接与 Keep-Alive 复用/收敛；
5. Connector 数量增长时的 `ns/op`、`B/op`、`allocs/op`；
6. 结论、未验证边界，以及是否足以支持生产实现或 Schema 默认值变更。

GitHub 托管 Runner 的硬件与邻居负载可能变化，普通 CI 只用于正确性回归。未经同一固定
环境复测，不得用托管 Runner 的数值设定性能阈值，也不得据此调整 Server Schema 默认值。
