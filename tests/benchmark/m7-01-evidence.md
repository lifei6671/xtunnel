# M7-01 调优证据

> 状态：`PENDING_FIXED_LINUX_RUN`

## 当前实现证据

- 基线 Commit：待 Benchmark 实现提交后填写。
- Proxy Buffer：待运行。
- HTTP/1.1 WorkConn Capacity：待运行。
- Connector Selection CPU/Allocation：待运行。

## 正式环境

- OS/Kernel：待填写。
- CPU：待填写。
- RAM：待填写。
- FD Limit：待填写。
- Go/GOTOOLCHAIN：待填写。
- GOMAXPROCS：待填写。
- RTT/Bandwidth/连接数/负载：待填写。

## 结果与决策

正式结果必须由 `run-m7-01.sh -m full` 在固定 Linux amd64 环境、干净提交上产生。
当前 Windows 主机具备项目固定 Go 1.27，但 WSL 缺少 Go；本阶段本地结果只能作为
Benchmark 正确性与 Windows 开发反馈，不能作为 Linux Syscall 或 Server 默认值调优证据。

在正式结果完成前：

- 不调整 32 KiB Proxy Buffer 基线；
- 不调整 HTTP/1.1、WorkConn 或 Connector 的 Schema/Repository 默认值；
- 不修改 Registry、Session Manager 或 Transport 的生产选择算法；
- M7-01 保持实现中，不能进入 `REVIEW`。
