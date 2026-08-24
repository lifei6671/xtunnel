# XTunnel

XTunnel Standalone V0.1 正在按开发计划逐步实现。当前 Server/Agent 已具备配置加载、共享 JSON 结构化日志和前台进程生命周期骨架，尚未启动 Management、Ingress、Agent Gateway 或数据库。

## 开发运行

项目固定使用 Go 1.27.0，并要求本地工具链模式：

```powershell
$env:GOTOOLCHAIN='local'
./tools/check-go-version.ps1
```

两个进程使用相同的配置入口：

```text
xtunnel-server --config <server.yaml> [--set <schema.path>=<value>]...
xtunnel-agent  --config <agent.yaml>  [--set <schema.path>=<value>]...
```

`--config` 可省略；`--set` 可以重复使用，同一路径以后出现的值为准。配置仍按 `CLI > XTUNNEL_* Environment > YAML > Schema Default` 合并，未知 Flag、位置参数或 Schema 路径会直接失败。配置字段以 `configs/server.schema.json` 和 `configs/agent.schema.json` 为准。

当前进程在配置校验通过后初始化标准库 `log/slog` JSON Handler，并在 `info` 级别输出 `process_started`、`process_stopped` 生命周期事件。基础字段固定为 `timestamp`、`level`、`component`、`event`；真实请求或 Trace 上下文存在时可追加 `request_id`、`trace_id`。收到 `SIGINT` 或 `SIGTERM` 后进程正常退出。完整启动链、监听器、数据库和 Drain 流程将在后续任务中接入。
