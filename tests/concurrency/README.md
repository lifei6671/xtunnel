# M7-05 Race/Concurrency

本目录保存 M7-05 的 Linux Race 与 Profile 可复现入口。Runner 不修改生产配置、
不设置性能失败阈值，也不把 Profile 采样耗时混入主 Benchmark 结果。

## 覆盖范围

- `smoke`：对 Session Replacement、Config Apply、Usage Flush、Listener Reconcile、
  Gateway TLS 热加载和 Tunnel 选择相关包运行定向 Race，并以最小迭代验证串行及并发
  Connector Selection Benchmark 可以构建和执行。
- `full`：在干净 Commit 上执行 `go test -race ./...`，随后独立采集 Connector
  Selection 主结果、CPU Profile、Mutex Profile 和 Block Profile。三个 Profile 使用
  独立测试进程，不能作为主结果的吞吐或延迟样本。

并发 Benchmark 复用 M7-01 的真实 `Proxy.selectConnector` fixture，包含
`Session Manager.Pools` 副本、`TunnelRuntime` eligibility/least-active/RR 选择与
`WorkPool.Snapshot`。每次迭代都归还 Connector Lease；它不模拟网络吞吐，也不改变
选择算法或锁所有权。

## 环境要求

- Linux amd64；
- Go `1.27.1` 或更新的 `1.27.x` 补丁版，`GOTOOLCHAIN=local`；
- `CGO_ENABLED=1`，用于原生 Race Detector；
- Git、`go tool pprof`，以及可用的 C 编译器；
- `full` 需要仓库 CI 使用的 Node/npm；Runner 会先执行 Web 的 `npm ci`、静态检查和
  构建，因为干净 checkout 不包含被忽略的 `web/dist`；
- `full` 必须从干净工作区运行，结果目录必须位于仓库外。显式 `-o` 只接受
  不存在或已经为空的目录；Runner 在写入首个证据文件前检查并启用禁止覆盖，避免
  混入旧结果或覆盖并发写入者的同名文件。

Runner 只使用 POSIX `sh` 语法。`lscpu`、`free` 和 `nproc` 若存在会补充环境信息，
不存在时不会伪造数据。

## 运行方式

快速检查：

```sh
./tests/concurrency/run-m7-05.sh -m smoke
```

正式采样：

```sh
./tests/concurrency/run-m7-05.sh -m full -o /tmp/xtunnel-m7-05
```

`full` 主 Benchmark 对 1/8/32/100 Connector 使用 `-cpu=1,8,32`、
`2s x 5`。CPU、Mutex 和 Block Profile 固定使用 100 Connector、
`GOMAXPROCS=32` 和 10 秒采样；Mutex Fraction 与 Block Rate 均设为 `1`，以保留
每次争用/阻塞事件。三个 Profile 进程各自通过 `go test -o` 保留对应的 `.test`
二进制；原始 `.pprof`、测试输出、完整 `go tool pprof -top` 摘要，以及只保留
Selection/Pools/Lease 调用栈的 Focus 摘要都写入结果目录。Block
Profile 的完整摘要会包含 fixture 所拥有的 Control Session 等待；判断选择热路径时必须
同时查看 Focus 摘要，不能把这些有意存在的后台等待归因给 `TunnelRuntime`。

## 证据边界

- Race 只证明本次实际执行路径没有观测到 data race；它不证明没有死锁，也不穷举所有
  goroutine 调度交错。
- Mutex/Block/CPU Profile 是诊断证据。单次 WSL2、虚拟机或共享 CI Runner 的数值
  不得用于设定性能阈值，也不能单独授权修改生产锁、选择算法、公共 API 或默认值。
- Profile 使用 `CGO_ENABLED=1`，但仍需如实记录宿主是 WSL2、虚拟机还是原生 Linux；
  环境标签不能由 Runner 猜测。
- M7-01 串行 CPU/Allocation 数据可作为前值，但不能替代本任务的并发 Mutex/Block
  Profile。正式结论必须绑定干净 Commit，并单独记录精确 CI Race 结果。
