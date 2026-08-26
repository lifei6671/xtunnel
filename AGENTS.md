# XTunnel 项目协作规则

本文件适用于仓库根目录及全部子目录。更具体目录中的 `AGENTS.md` 或
`AGENTS.override.md` 可以补充局部规则，但不得降低这里冻结的工具链、契约、安全和
验证基线。

## 仓库地图与权威来源

- `cmd/server`、`cmd/agent`：进程入口与生命周期装配；业务规则不得堆入 `main` package。
- `internal/`：Server、Agent、Protocol、Repository 和 Tunnel 等内部实现；先确认现有 package 的职责与所有权，再新增目录或抽象。
- `api/proto/*.proto`：Protocol v1 的唯一 Wire Authority； `internal/protocol/gen/*.pb.go` 是提交仓库的生成物，不得手工修改。
- `configs/server.schema.json`：Server 配置字段、类型、默认值、范围、Secret 和热加载属性的唯一机器权威；Go Struct、示例和文档不得独立发明第二套默认值。
- Agent 不使用本地业务配置 Schema 或 YAML；Bootstrap 本地输入只接受版本化 Connection Token。Service、Origin 和 Health Policy 只经已认证的 Control Session 以 Snapshot 下发并在内存应用，不得新增第二套本地业务配置来源。
- `api/openapi/openapi.yaml`：REST Request/Response、状态码和错误结构的唯一机器权威； Handler 和 Web 不得用手写 DTO 反向定义 OpenAPI。
- `migrations/` 与 `internal/repository/`：持久化 Schema、迁移和 Repository 实现；运行时 Session、Connector 和连接状态不得因实现方便而写入 SQLite。
- `tools/`：版本检查、Proto/OpenAPI 工具安装与校验入口；不得维护绕过 Wrapper 的第二套生成命令，Proto/OpenAPI Wrapper 也不得回落到开发机 `PATH` 中的同名工具。
- `web/`：React/Vite 源码；`web/dist` 是被忽略的可重复构建产物，不提交占位文件。
- `tests/golden/`：人工复审的稳定字节与行为基线；普通测试不得自动重写 Fixture。
- `deploy/`：OCI、systemd 和平台部署产物；支持矩阵与运行权限以技术方案和已验证部署证据为准。

产品边界和长期行为以 `docs/xtunnel_standalone_v0.1.md` 为准；任务、依赖、状态和证据以`docs/xtunnel_standalone_v0.1_development_plan.md` 为准。文档中的规划路径和命令不代表文件已经存在，执行前必须从当前工作区核实。

## Go 1.27 强制基线

- 项目必须使用 Go 1.27。根 `go.mod` 必须声明 `go 1.27`。
- 根 `go.mod` 的 `toolchain` 指令记录稳定的精确 `go1.27.x` 补丁版本；`tools/go.mod`、 CI、OCI Builder 和版本检查入口必须使用同一版本，禁止写入 `latest`、`stable` 或占位值。
- 本地开发、测试、代码生成、CI 和发布构建必须设置 `GOTOOLCHAIN=local`。执行 Go 命令前先检查 `go env GOVERSION` 和 `go env GOTOOLCHAIN`；版本或模式不匹配时快速失败，不得自动下载或切换工具链，也不得把结果作为验收证据。
- 项目允许并应在适合的实现中优先采用稳定 Go 1.27 能力，不兼容 Go 1.26 及更早工具链，但不得为了展示新语法增加无关复杂度。
- 不得新增旧 Go 版本兼容垫片。`GOEXPERIMENT`、tip-only API、开发分支能力和未进入稳定 Go 1.27 的特性默认禁止；确需使用时必须先获得明确授权并同步技术方案和验收规则。
- 调整 Go minor/patch 版本、放宽兼容范围或改变工具链固定策略属于开发基线变更，必须先获得明确确认，并同步技术方案、开发计划、根/工具 Module、CI、OCI Builder 和验证证据。

## 真实命令入口

执行任何 Go 命令或需要构建 Go Generator 的 Proto 工具入口前，先设置`GOTOOLCHAIN=local`，并使用当前平台对应的版本检查入口：

```powershell
$env:GOTOOLCHAIN = 'local'
./tools/check-go-version.ps1
```

```sh
export GOTOOLCHAIN=local
./tools/check-go-version.sh
```

- 修改 Go 代码后，至少对改动文件运行 `gofmt`，并执行相关 package 测试和静态检查； 全量基础验证为 `go test ./...`、`go vet ./...`。
- 修改 Web 或在缺少 `web/dist` 时执行会编译 Embed Package 的 Go 命令，先运行 `npm --prefix web ci`、`npm --prefix web run check`、`npm --prefix web run build`。
- 修改 Proto 时，在 Linux amd64/arm64 环境使用 `./tools/bootstrap-proto.sh`、 `./tools/proto.sh lint` 和 `./tools/proto.sh breaking`。`generate-check` 只在包含本次生成物的干净 checkout 或 CI 中作为正式通过证据；脏工作区中的生成和等价检查只能作为开发反馈。
- 修改 OpenAPI 时，使用 `./tools/bootstrap-openapi.sh`、`./tools/openapi.sh validate` 和 `./tools/test-openapi.sh`。
- 完整验证顺序和跨平台矩阵以 `.github/workflows/ci.yml` 为准；不要把定向测试、脏工作区等价检查或单项 Schema/Fixture `VALID` 冒充完整 CI/Gate。
- 长时间运行的测试、Race、Fuzz、Smoke 和部署检查必须设置与对应任务一致的超时。

## Go 实现原则

- 开始实现前，先阅读技术方案和开发计划中对应任务、依赖、契约、失败分支与 Gate。
- 只使用稳定 Go 1.27 中真实存在的能力；不确定 API 或语义时，查当前工具链文档或源码，  不得凭印象编写。
- package 名使用简短、具体的业务或技术概念；避免新增 `util`、`common`、`helper` 等无法表达所有权的泛化 package。
- 接口定义在使用方附近，并保持最小；只有出现当前、明确的替换或复用需求时才引入接口。
- 已有领域职责、状态机或安全决策存在指定 owner/入口时，调用该入口，不得在其他 package 复制实现。规则禁止某条捷径时，必须同时指出受支持的正式入口。
- 不为一次性逻辑创建 helper、factory、wrapper 或兼容壳；确认无调用方的旧代码直接删除。
- 错误必须显式处理并携带操作上下文；需要保留错误链时使用 `%w`。不得用日志后继续、空fallback 或无限重试掩盖非法内部状态。
- 不得忽略有业务意义的 `Close`、`Flush`、`Commit` 和持久化错误。只有确认错误不影响正确性且调用点已表达原因时，才可显式丢弃清理错误。

## 并发、取消与资源生命周期

- 长生命周期的网络、Session 和后台循环入口在存在上游 Context 时必须接收并传递`context.Context`，尊重取消和 Deadline，不得用 `context.Background()` 截断已有调用链。遵循标准库接口、无法接收 Context 的阻塞 IO 必须另有 Deadline 或 Close 取消路径。
- 每个 goroutine 必须有明确 owner、停止条件和等待退出路径；禁止 fire-and-forget 和 orphan goroutine。
- `TunnelRuntime`、Session、WorkPool、Listener、Config Write 和 Usage Flush 等共享状态必须遵循技术方案规定的唯一所有权、固定锁顺序和 exactly-once 计数语义。
- 技术方案规定的 Runtime/Owner 锁内禁止网络/磁盘 IO、`Close`、等待 Channel 或其他不可控阻塞；跨 owner 操作先在锁内生成不可变输入，释放锁后再执行外部动作。
- Control Session 保持 Single Reader、Single Writer、Single Owner 和有界队列；其他 goroutine 只通过 Owner/Outbox 交互，队列满或无法保证顺序时关闭 Session，不得无限等待或静默丢弃；旧 generation 的 cleanup 不得破坏新 generation 状态。
- 仅取消 Context 不足以解除阻塞中的 `net.Conn.Read`/`Write`。取消、Fatal Error 或 Shutdown 必须按协议通过 `Close`、`CloseWrite` 或 Deadline 主动解除 IO，并等待相关 goroutine、FD 和计数归零。
- 普通单边 EOF 不是 Fatal Error，应按 Half-Close 规则允许反方向继续完成；不得为简化退出逻辑直接关闭双向连接。
- Graceful Shutdown 必须停止新入口、排空既有工作，并在 Deadline 后主动关闭残留 Socket；不得无限等待自然取消。Server 必须在连接与 Session 关闭后再 Flush/Close SQLite。

## 日志与敏感信息

- 统一使用项目的 `log/slog` JSON Handler 和稳定字段；调用方不得使用 `fmt.Print*` 写运行日志或替代结构化日志。
- 只在真实上下文存在时记录 `request_id`、`trace_id` 和业务 ID；不得在日志层生成替代 ID或输出空字段。
- Tunnel Token、Admin Password、Session Cookie、Session Secret、TLS Private Key 和 Authorization Header 绝不能进入日志、错误文本、测试输出或提交内容。
- 共享 Handler 的脱敏不能替代调用方约束：不得把 Secret 拼入 `event` 或其他非敏感字段，也不得直接记录完整 Config、HTTP Header、Cookie、请求体或认证对象。
- 改变日志字段名、语义或级别会影响检索、告警和审计，执行前必须获得明确确认。

## 测试与生成物

- 新增功能必须补充覆盖关键行为和失败分支的单元测试或最小可验证用例；编译通过、文件存在、浅断言或单一 Happy Path 不代表任务完成。
- 多场景逻辑优先使用 table-driven tests，测试名描述行为；断言具体业务字段、状态变化和副作用，不只断言 `NoError`/`NotNil`。
- 仅在确认没有共享环境变量、全局状态、固定端口、目录、SQLite、证书、Fixture 或时序依赖后使用 `t.Parallel()`；不能确认时保持串行。
- 并发、Session、WorkPool、Tunnel、Listener、Config Write 和 Usage Flush 等路径按任务要求运行 `go test -race ./...` 或更小的定向 Race Suite。
- 修改 `.proto` 后只通过仓库 Wrapper 重新生成并提交 `internal/protocol/gen`；不得手改`*.pb.go`，不得维护孤立生成物。
- Golden 测试逐字节比较已有 Fixture。更新 Fixture 必须作为显式 Protocol Review 变更，普通测试运行不得自动接受或覆盖新输出。
- 新增平台实现时同步考虑对应的 `*_linux.go`、`*_windows.go` 或 `*_unsupported.go` 失败路径，并执行任务要求的原生运行或交叉编译验证；交叉编译不能冒充原生 Runtime Smoke。

## 契约、进度与证据

- 实现与冻结的 Proto、JSON Schema 或 OpenAPI 冲突时，默认修正实现；不得静默修改机器契约或总方案迁就代码。
- 修改契约时，先改唯一机器权威，再同步生成物、语义镜像、Golden 和相关测试；不得在多个文件中独立维护同一份默认值、Wire 字段或 REST DTO。
- 产物完成且任务规定的验收命令已执行后，才可标记 `REVIEW`。只有真实产物、关键断言、失败分支、复审和可复现证据全部齐备，才可标记 `DONE` 或勾选 Gate。
- M0-10 完成后的新 `DONE` 证据必须同时记录 Commit SHA、验收命令与结果，以及 CI Run 链接或编号；干净 checkout 的本地结果可以补充证据，但不能替代 CI，脏工作区结果只能作为开发反馈。
- 验证无法执行时必须说明原因、影响和剩余风险，不得把未运行、跳过或等价检查写成通过。

## 变更边界

- 新增、升级或移除第三方依赖，修改 Lockfile、公共 API/Protocol、数据库 Schema、CI/CD、构建系统、生产配置、权限模型、日志契约或大规模跨包结构前，必须先取得明确确认。
- 保留用户已有修改，不清理无关脏工作区，不擅自改变暂存区，不手工编辑生成的 Lockfile 或校验文件，不提交密钥、Token、密码或私有配置。
- 只做当前任务需要的最小改动。新增目录级 `AGENTS.md` 只放该子树稳定、专属的知识与不变量， 不复制根规则，也不得降低根规则。
