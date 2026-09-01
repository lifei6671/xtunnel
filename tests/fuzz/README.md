# M7-06 Protocol/Parser Fuzz

本目录集中保存 M7-06 的跨 package Runner、Protocol/Frame Fuzz 目标和证据。Runner
使用 POSIX `sh` 语法，并依赖项目 Linux Runner 提供的 GNU coreutils `timeout` 与
`sha256sum`。Route 与
Forwarded 目标必须访问各自 package 的未导出正式解析入口，因此分别位于
`internal/server/route/matcher_fuzz_test.go` 与
`internal/server/httpingress/forwarded_fuzz_test.go`；不得为测试导出第二套 Parser。

## 覆盖范围

- `FuzzUVarintDecoder`：最短/非最短 UVarint、截断和溢出。
- `FuzzFrameDecoder`：有界 Frame、精确消费和 canonical 重编码。
- `FuzzControlEnvelope`、`FuzzWorkHello`：正式 Frame + Protobuf + 递归未知字段入口。
- `FuzzSnapshotMatchHTTPFromWire`：真实 `http.ReadRequest` 生成的 Host、RawPath 和
  RequestURI 组合。
- `FuzzSnapshotMatchHTTPRejectsDangerousPath`：encoded separator、dot-segment 和最多
  九层编码。
- `FuzzParseForwardedFor`、`FuzzNormalizeForwardedHeaders`：纯 IP 链、32 跳上限、
  trusted proxy 选择和权威 Header 重写。

标准库 HTTP Parser 在 Handler 前拒绝的非法 Request Line/percent escape 只记为
Parser-stage 400；只有成功进入 `Snapshot.MatchHTTP` 的输入才能证明 Matcher 的
`ErrInvalidPath` / `ErrInvalidHost` 收敛。

## 运行

先固定本地 Go 工具链：

```sh
export GOTOOLCHAIN=local
./tools/check-go-version.sh
```

开发反馈使用 `smoke`，每个目标独立运行 5 秒：

```sh
sh ./tests/fuzz/run-m7-06.sh -m smoke
```

正式候选使用干净工作区的 `full`，每个目标独立运行 60 秒，并把结果写到仓库外的新
目录：

```sh
sh ./tests/fuzz/run-m7-06.sh -m full -o /tmp/xtunnel-m7-06
```

Runner 使用私有 `GOCACHE`，固定 `-parallel=1`、最小化预算、Go 测试超时和进程级
watchdog，记录并在结束时复核 Commit/Tree，同时保存平台、命令、完整日志、工作区快照
及统一 SHA-256 清单。任一 panic、OOM、timeout、失败语料、HEAD/Tree 或工作区漂移都会
使整体失败；失败语料必须保留复现，不得自动删除或改写 Golden。

Short Fuzz 只说明本次实际执行输入未发现失败，不能证明全部输入空间安全。普通
`go test ./...` 只回放 seed corpus，不能冒充 mutation Fuzz 或 CI Short Fuzz。
