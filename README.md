# XTunnel

XTunnel Standalone V0.1 正在按开发计划逐步实现。当前 Server/Agent 已具备配置加载、共享 JSON 结构化日志和前台进程生命周期骨架；Server 已接入 Stable Data Target、Linux External Lock、GORM SQLite 与显式 Migration，尚未启动 Management、Ingress 或 Agent Gateway。

## 开发运行

项目固定使用 Go 1.27.0，并要求本地工具链模式：

```powershell
$env:GOTOOLCHAIN='local'
./tools/check-go-version.ps1
```

Proto 工具链运行在 Linux amd64/arm64；Windows 开发机通过安装了 Go 1.27.0 的 WSL 使用。工具只安装到仓库忽略的 `.tools/bin`，不会读取系统 `PATH` 中的 Buf 或 Generator：

```sh
export GOTOOLCHAIN=local
./tools/bootstrap-proto.sh
./tools/proto.sh lint
./tools/proto.sh breaking
./tools/proto.sh generate-check
```

当前尚未进入 Protocol v1 冻结阶段，因此 `api/proto` 为空，三个检查会明确输出 `SKIP`。这只证明 M0 工具链骨架可执行，不代表 Protocol Lint 或 Breaking Gate 已通过。

OpenAPI 机器契约固定为 3.1.0，并使用仓库锁定的 vacuum 校验。工具同样只安装到 `.tools/bin`，Windows 开发机通过 WSL 执行：

```sh
./tools/bootstrap-openapi.sh
./tools/openapi.sh validate
./tools/test-openapi.sh
```

当前 `api/openapi/openapi.yaml` 只有可校验骨架，Server 固定为同源基路径 `/api/v1`，尚不包含业务路径或 DTO。Validate 通过只代表 M0 骨架和基路径约束有效，不代表 M5 REST Contract Gate 已通过。

两个进程使用相同的配置入口：

```text
xtunnel-server --config <server.yaml> [--set <schema.path>=<value>]...
xtunnel-agent  --config <agent.yaml>  [--set <schema.path>=<value>]...
```

`--config` 可省略；`--set` 可以重复使用，同一路径以后出现的值为准。配置仍按 `CLI > XTUNNEL_* Environment > YAML > Schema Default` 合并，未知 Flag、位置参数或 Schema 路径会直接失败。配置字段以 `configs/server.schema.json` 和 `configs/agent.schema.json` 为准。

当前进程在配置校验通过后初始化标准库 `log/slog` JSON Handler，并在 `info` 级别输出 `process_started`、`process_stopped` 生命周期事件。基础字段固定为 `timestamp`、`level`、`component`、`event`；真实请求或 Trace 上下文存在时可追加 `request_id`、`trace_id`。

Server 当前按以下顺序初始化存储：

```text
Resolve Stable Data Target
→ Acquire Linux External Lock
→ Check Pending Restore Journal
→ Validate Canonical Data Directory
→ Open SQLite with GORM
→ Run Forward-only Migration
```

`server.data_dir` 必须是绝对路径，父目录和正式数据目录都需预先存在；Server 不会自动创建数据目录。Linux 运行环境还需预先创建归 Runtime UID 所有、权限为 `0700` 的 `/run/xtunnel`。数据库固定为 `<server.data_dir>/xtunnel.db`，连接使用 WAL、Foreign Keys、5 秒 Busy Timeout 和 Normal Synchronous。发现待处理 Restore Journal 时，当前版本会在打开数据库前拒绝启动；正式恢复状态机由后续 M3-12 实现。

收到 `SIGINT` 或 `SIGTERM` 后，Server 先关闭 SQLite 再释放 External Lock，Agent 也会正常退出。XTunnel V0.1 的生产运行边界为 Linux amd64/arm64；Windows 当前用于构建和单元测试，不提供生产 External Lock。完整 Listener、Session 和 Drain 流程将在后续任务中接入。
