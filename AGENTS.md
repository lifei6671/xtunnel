# XTunnel 项目协作规则

本文件适用于仓库根目录及全部子目录。更具体目录中的 `AGENTS.md` 或 `AGENTS.override.md` 可以补充局部规则，但不得降低这里冻结的 Go 工具链基线。

## Go 1.27 强制基线

- 项目必须使用 Go 1.27。根 `go.mod` 必须声明 `go 1.27`。
- 初始化 Go Module 时，选择一个稳定的 `go1.27.x` 补丁版本，由根 `go.mod` 的 `toolchain` 指令记录，并让 `tools/go.mod`、CI、OCI Builder 和版本检查入口使用同一个精确版本；禁止写入 `latest`、`stable` 或占位值。
- 本地开发、测试、代码生成、CI 和发布构建必须设置 `GOTOOLCHAIN=local` 并使用上述精确工具链。执行 Go 命令前先检查 `go env GOVERSION` 和 `go env GOTOOLCHAIN`；版本或模式不匹配时应快速失败并报告，不得自动下载/切换工具链，也不得把结果作为验收证据。
- 项目允许并应在适合的实现中优先采用 Go 1.27 已稳定发布的语言、标准库和运行时特性，不需要兼容 Go 1.26 及更早工具链。
- 不得为旧 Go 版本新增兼容垫片。若 Go 1.27 原生能力能够更直接地解决当前问题，优先使用原生能力，但不得为了展示新语法而增加无关复杂度。
- `GOEXPERIMENT`、tip-only API、开发分支能力和尚未进入稳定 Go 1.27 的特性默认禁止；确需使用时必须先获得明确授权，并同步技术方案和验收规则。
- 调整 Go minor/patch 版本、放宽兼容范围或改变工具链固定策略属于开发基线变更，必须先获得明确确认，并同步技术方案、开发计划、根/工具 Module、CI、OCI Builder 和验证证据。

## Go 实现与验证

- 开始实现前，先阅读 `docs/xtunnel_standalone_v0.1.md` 和 `docs/xtunnel_standalone_v0.1_development_plan.md` 中对应任务、依赖、契约和 Gate。
- 只使用稳定 Go 1.27 中真实存在的能力；不确定 API 或语义时，先查当前工具链文档或源码，不得凭印象编写。
- 新增或修改 Go 代码后，至少执行与改动相关的格式化、单元测试和静态检查。仓库形成统一命令入口后，优先使用项目命令；在此之前使用 `gofmt`、`go test ./...` 和 `go vet ./...`。
- 并发、Session、WorkPool、Tunnel、Listener、Config Write 和 Usage Flush 等路径按对应任务要求运行 `go test -race ./...` 或更小的定向 Race Suite。
- 使用 Go 1.27 新特性的代码必须有覆盖其关键行为和失败分支的测试；编译通过不等于任务完成。

## 契约与进度

- 产品边界和长期行为以 `docs/xtunnel_standalone_v0.1.md` 为准；任务、依赖、状态和证据以 `docs/xtunnel_standalone_v0.1_development_plan.md` 为准。
- Protocol、Config 和 REST 分别以已落盘的 Proto、JSON Schema 和 OpenAPI 为机器权威。实现与冻结契约冲突时，默认修正实现，不得静默改文档迁就代码。
- 没有真实产物、关键测试、失败分支、复审和可复现命令证据，不得把任务标记为 `REVIEW`、`DONE` 或勾选 Gate。
- 文档中的规划路径和命令不代表文件已经存在；执行前必须从当前工作区核实。

## 变更边界

- 新增或升级第三方依赖、修改公共 API/Protocol、数据库 Schema、生产配置或权限模型前，必须先取得明确确认。
- 保留用户已有修改，不清理无关脏工作区，不擅自改变暂存区，不提交密钥、Token、密码或私有配置。
- 只做当前任务需要的最小改动；验证无法执行时必须说明原因、影响和剩余风险，不得把未运行写成通过。
