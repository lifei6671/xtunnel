# XTunnel 代码审查标准与流程

> 本文件是 XTunnel 的代码审查操作规范，定义审查对象、风险分级、流程、反馈格式与证据要求。
> 它不创建新的产品、协议、配置、API、任务状态或发布 Gate 权威；发生冲突时，按下述权威层级处理。

---

## 0. 定位、适用范围与权威层级

本规范适用于工作区 Diff、暂存区 Diff、单个 Commit、Commit Range 和 Pull Request。审查默认只读；除非用户明确要求修复，审查者不得修改代码、文档、暂存区、任务状态或 Gate。

| 领域 | 权威来源 |
|------|----------|
| 项目协作、工具链、安全与变更边界 | 当前路径适用的 `AGENTS.md` / `AGENTS.override.md` |
| V0.1 产品边界、架构与行为语义 | `docs/xtunnel_standalone_v0.1.md` |
| Protocol v1 Wire Contract | `api/proto/*.proto` |
| Server 配置字段、默认值和范围 | `configs/server.schema.json` |
| REST API | `api/openapi/openapi.yaml` |
| Task、状态、依赖、Gate 和执行证据 | `docs/xtunnel_standalone_v0.1_development_plan.md` |
| 实际 CI Job、命令与平台矩阵 | `.github/workflows/ci.yml` |

代码与冻结契约冲突时，默认判定为实现漂移；不得为了让当前实现通过审查而静默弱化权威契约。确需改变公共 API、Protocol、数据库 Schema、依赖、配置、CI/CD、权限或日志契约时，必须先取得项目规则要求的确认。

---

## 1. 审查维度与严重性分级

### 1.1 五大审查维度

| 维度 | 核心问题 | 适用范围 |
|------|----------|----------|
| **正确性** | 代码是否做了它应该做的事？ | 所有逻辑变更、协议交互、状态转换 |
| **安全性** | 是否存在注入、越权、信息泄露？ | 所有处理外部输入、Secret、认证、网络 IO 的路径 |
| **可维护性** | 6 个月后有人能看懂吗？ | 所有变更 |
| **性能** | 是否有明显的瓶颈或不必要的分配？ | 热路径（数据面代理、Session 调度、连接池） |
| **测试** | 关键路径和失败分支是否被覆盖？ | 所有逻辑变更 |

### 1.2 风险分级

分级必须依据可观察影响、可触发性、影响范围和证据，不得仅凭“输入校验”“并发”“性能”等问题类型机械决定。

| 等级 | 含义 | 处理要求 |
|------|------|----------|
| **P0 / Blocker** | 可导致 Secret 泄露、认证绕过、数据损坏、不可恢复状态、已冻结契约破坏、稳定死锁/竞态或生产不可用 | 必须修复；不得延期、批准或合并 |
| **P1 / Must Fix** | 确定性正确性错误、关键失败分支缺失、资源泄漏、错误状态转换、严重测试缺口，或会使本任务验收结论失真 | 批准前必须修复；若问题实际要求改变权威契约，先停止并请求决策 |
| **P2 / Suggestion** | 不影响当前正确性的可维护性、可观测性、局部性能或测试质量改进 | 可修复，也可说明理由后延期；只有形成明确后续工作时才创建 Issue |
| **P3 / Nit** | 命名、注释、格式或非必要替代方案 | 不阻断；不得借 Nit 要求超出任务范围的重构 |

典型 P0/P1 包括：Secret 进入日志或错误文本、手改 `*.pb.go`、忽略有业务意义的 Commit/Flush/Close 错误、锁内执行不可控阻塞、改变公开 Wire/API 但未先修改权威契约、使用不符合项目固定策略的 Go 工具链。

以下情况必须结合上下文判断，不能套模板：

- 外部输入校验缺失可能是 P0/P1，也可能对不可达内部路径不构成问题。
- 只有调用方需要保留错误链或使用 `errors.Is/As` 时，包装错误才必须使用 `%w`；不得把所有 `%v` 一律判错。
- 少量重复代码可以接受；只有形成稳定业务概念或存在当前复用需求时才建议抽取，禁止推动过早抽象。

---

## 2. 项目特定审查检查点

以下检查点针对 XTunnel 的架构特征，是常规 Go 审查之外的补充。

### 2.1 并发与资源生命周期

```
✅ 每个新 goroutine 是否有明确 owner、停止条件和退出等待路径？
✅ 共享状态（Registry、Session Pool、WorkPool、Listener）是否遵循技术方案规定的唯一所有权和固定锁顺序？
✅ Runtime/Owner 锁内是否有网络/磁盘 IO、Close、Channel 等待？
   → 如果有：先在锁内生成不可变输入，释放锁后执行外部动作
✅ Control Session 是否保持 Single Reader / Single Writer / Single Owner？
✅ 队列满时是否关闭 Session 而非无限等待或静默丢弃？
✅ 旧 generation cleanup 是否破坏了新 generation 的状态？
✅ net.Conn.Read/Write 阻塞解除路径：取消 Context 后是否有 Close/CloseWrite/Deadline 主动解除？
✅ 单边 EOF 是否按 Half-Close 规则处理，而非直接关闭双向连接？
✅ Graceful Shutdown 是否遵循：停止新入口 → 排空既有工作 → Deadline 后强制关闭残留？
✅ 跨连接共享的 *tls.Config 发布后是否保持不可变？连接级修改是否先 Clone 或逐连接构造？
✅ 仅非权威遥测可以有界丢弃；Audit、Usage exactly-once Delta 和 Runtime Mutation 是否避开 lossy Observer？
```

### 2.2 Secret 与敏感信息

```
✅ Tunnel Token、Admin Password、Session Cookie、Session Secret、TLS Private Key
   是否出现在日志、错误文本、测试输出或提交内容中？
✅ 共享 Handler 脱敏后，调用方是否仍把 Secret 拼入 event 或其他非敏感字段？
✅ 是否直接记录了完整 Config、HTTP Header、Cookie、请求体或认证对象？
✅ Server 持久化的完整 Tunnel Token 是否使用 AES-256-GCM 密文？主密钥是否独立 32 字节且权限安全？
   → 该要求不意味着 Agent 需要新增本地 Token 数据库或业务配置文件
```

### 2.3 契约一致性

```
✅ Go Struct 的默认值、字段名、类型是否与 configs/server.schema.json 一致？
   → 不得在 Go 代码中独立发明第二套默认值
✅ REST Handler 的 Request/Response、状态码、错误结构是否与 api/openapi/openapi.yaml 一致？
   → 不得用手写 DTO 反向定义 OpenAPI
✅ Proto 变更是否先改 api/proto/*.proto，再通过 Wrapper 重新生成 internal/protocol/gen？
   → 不得手改 *.pb.go
✅ 修改契约时是否同步了生成物、语义镜像、Golden 和相关测试？
```

### 2.4 测试质量

```
✅ 新增功能是否补充了覆盖关键行为和失败分支的测试？
   → 编译通过、文件存在、浅断言或单一 Happy Path 不算完成
✅ 多场景逻辑是否使用 table-driven tests？测试名是否描述行为？
✅ 断言是否针对具体业务字段、状态变化和副作用，而非仅 NoError/NotNil？
✅ 并发路径是否执行“受影响包定向 Race + 当前 CI 基础 Race Suite”？
   → CI 的固定包列表必须从 .github/workflows/ci.yml 读取，不在本文维护第二份清单
   → 修改 tunnel/controlsession/workpool/sessionruntime 等包时，不能用无关基础 Suite 代替定向 Race
✅ t.Parallel() 是否确认了无共享环境变量、全局状态、固定端口、SQLite、证书、Fixture？
✅ Golden 测试变更是否作为显式 Protocol Review，而非普通测试自动接受？
✅ 新增平台实现是否同步考虑了 *_linux.go / *_windows.go / *_unsupported.go？
   → 交叉编译不能冒充原生 Runtime Smoke
```

### 2.5 变更边界

```
✅ 是否新增/升级/移除了第三方依赖？→ 需明确确认
✅ 是否修改了 Lockfile？→ 需明确确认，不手工编辑生成物
✅ 是否修改了公共 API/Protocol/数据库 Schema/CI/CD？→ 需明确确认
✅ 是否修改了权限模型、日志契约或大规模跨包结构？→ 需明确确认
✅ 是否保留了用户已有修改，未清理无关脏工作区？
✅ 是否只做了当前任务需要的最小改动？
```

---

## 3. 审查流程

### 3.0 冻结审查对象

开始审查前必须先声明审查对象，避免结论对应到不断变化的工作区：

| 对象 | 必须记录 |
|------|----------|
| 工作区 Diff | 仓库根目录、staged/unstaged/untracked 清单、明确不纳入的用户改动 |
| 暂存区 Diff | `git diff --cached` 对应的当前 Index；若目标文件还有 unstaged 修改，必须分别审查并提醒重新暂存 |
| 单个 Commit | Commit SHA 与 Parent SHA |
| Commit Range | Base SHA、Head SHA 和采用的 two-dot/three-dot 语义 |
| Pull Request | PR Head SHA、Base Branch/SHA、最新 CI Run |

最低只读检查：

```powershell
git status --short
git diff --stat
git diff
git diff --cached
```

审查者不得清理无关脏工作区、改变 staging、自动接受 Golden、修改生成物或顺手修复问题。若用户随后要求修复，修复属于新的写入范围，并在完成后重新审查最新 Diff。

### 3.1 变更提交审查前自检清单

以下清单同样适用于工作区、Commit 和 PR；“变更作者”不要求必须存在远端 PR。

#### 3.1.1 通用验证（所有变更）

- [ ] 已读取当前路径适用的 `AGENTS.md` / override 和本次变更对应的权威契约
- [ ] 已区分 staged、unstaged、untracked 与明确不在范围内的用户修改
- [ ] `git diff --check` 与必要的生成物/路径一致性检查通过
- [ ] 未提交或输出 Secret、Token、密码、Cookie、Private Key、真实 Pin 或私有配置
- [ ] 未手改生成的 `internal/protocol/gen/*.pb.go`、Lockfile 或校验文件

#### 3.1.2 Go 工具链前置检查

执行任何 Go 命令或需要构建 Go Generator 的入口前，必须先设置本地工具链模式并运行仓库检查入口：

```powershell
$env:GOTOOLCHAIN = 'local'
./tools/check-go-version.ps1
```

```sh
export GOTOOLCHAIN=local
./tools/check-go-version.sh
```

不得只检查 `go 1.27` 或模糊的 `go1.27.x`；根 Module、Tools Module、CI、OCI Builder 与检查脚本必须使用项目当前固定的同一精确补丁版本。

#### 3.1.3 条件验证（按变更类型触发）

| 变更类型 | 必须执行的验证 |
|----------|----------------|
| 纯文档/Skill | 不运行无关 Go 构建；执行 `git diff --check`、引用路径检查，以及受影响的 Task ID/仪表盘/Skill Validator 检查 |
| Go 代码 | 改动文件 `gofmt`、改动包定向测试，再执行 `go test ./...`、`go vet ./...`、`go mod verify` |
| 并发路径 | 受影响包定向 `go test -race`；再确认当前 CI 基础 Race Suite，不得只跑无关包 |
| Proto | `./tools/bootstrap-proto.sh` → `./tools/proto.sh lint` → `./tools/proto.sh breaking` → `./tools/proto.sh generate-check`（干净 checkout） |
| OpenAPI | `./tools/bootstrap-openapi.sh` → `./tools/openapi.sh validate` → `./tools/test-openapi.sh` |
| Web 前端 | `npm --prefix web ci` → `npm --prefix web run check` → `npm --prefix web run build` |
| 会编译 Embed Package 的 Go 命令 | 先确保 `web/dist` 存在或已执行 Web 构建 |
| 平台特定实现 | 对应平台原生测试/Smoke；交叉编译只作为补充，不冒充原生证据 |

#### 3.1.4 工作区与证据边界

- [ ] `git diff --check` 通过（无尾随空格、冲突标记）
- [ ] `git status --porcelain --untracked-files=all` 无非预期文件；预期的用户修改已明确排除或纳入
- [ ] 未提交密钥、Token、密码或私有配置
- [ ] 脏工作区命令只记录为开发反馈，没有冒充干净 checkout 或 CI Gate

### 3.2 审查触发与角色

#### 审查触发条件

以下情况必须发起正式代码审查：

- 所有合并到 `main` 的 PR
- 用户要求审查的工作区 Diff、Commit 或 Commit Range
- 修改 Proto/OpenAPI/JSON Schema 契约的变更
- 修改并发原语（锁、Channel、goroutine 生命周期）的变更
- 新增或修改安全相关路径（认证、授权、Token、加密）的变更
- 修改 CI/CD 配置、部署产物、权限模型的变更

#### 审查角色

| 角色 | 职责 | 适用场景 |
|------|------|----------|
| **审查者** | 冻结审查对象，按风险逐项检查，给出有证据的分级反馈 | 所有正式审查 |
| **契约审查者** | 验证 Proto/OpenAPI/Schema 变更符合机器权威 | 契约变更 |
| **平台审查者** | 验证 `*_linux.go`/`*_windows.go` 失败路径和原生 Smoke | 平台实现变更 |
| **作者** | 回复审查意见，修复 P0/P1，解释或延期 P2，按需处理 P3 | 所有正式审查 |

单一审查者可兼任多个角色，但开发计划或 Gate 明确要求“独立复审”时，同一实现者、同一 Agent 上下文或仅重复运行生成器不构成独立证据。Protocol/Golden、关键安全和重大架构变更按对应 Gate 要求安排独立审查；没有独立证据时必须如实保留未完成状态。

### 3.3 可选协作时间盒

以下只是多人协作建议，不属于正确性条件、Task 验收项或 Release Gate：

| 阶段 | 建议时间 | 说明 |
|------|----------|------|
| 首次响应 | PR 提交后 4 小时内 | 审查者确认已收到并开始审查 |
| 完成首轮审查 | 首次响应后 1 个工作日 | 集中返回首轮已发现问题；复审仍可报告修复引入的新回归 |
| 修复响应 | 收到审查意见后 1 个工作日 | 作者修复或回复每一条意见 |
| 合并确认 | 所有 P0/P1 修复后 | 审查者确认并通过 |

### 3.4 审查意见反馈格式

每条审查意见必须包含以下结构：

```markdown
**[P0/P1/P2/P3] [维度]: [一句话标题]**
`文件路径:行号` — [具体问题描述]

**证据与影响：** [可以触发的输入/状态、违反的契约和潜在影响]

**建议：** [具体的修复方向或代码示例]
```

找不到可定位、可触发或可验证的问题时，不得为了填满模板制造 Finding。问题、假设和可选替代方案应与 P0—P3 Finding 分开呈现。

**示例：**

```markdown
**[P0] 安全性: Secret 泄露风险**
`internal/server/controlauth/handler.go:87` — Token 值被拼入 error message 返回给客户端。

**证据与影响：** 客户端日志或中间代理可能记录完整 error message，导致 Tunnel Token 泄露。
AGENTS.md 明确禁止 Token 进入错误文本。

**建议：** 返回稳定的通用认证错误；服务端仅记录允许公开的 Tunnel/Token 业务 ID，不记录完整 Token 或认证 Secret。
```

### 3.5 冲突升级

| 冲突类型 | 升级路径 |
|----------|----------|
| 审查者与作者对 P0/P1 判断有分歧 | 作者说明可触发条件和证据，请求第二位审查者或用户裁决；裁决前不批准 |
| Wire 字段、方向、状态或 enum 分歧 | 以 `api/proto/*.proto` 为精确权威，总方案只作行为语义解释 |
| Server 配置字段、默认值或范围分歧 | 以 `configs/server.schema.json` 为精确权威 |
| REST Request/Response、状态码或错误结构分歧 | 以 `api/openapi/openapi.yaml` 为精确权威 |
| 产品边界或长期行为分歧 | 以 `docs/xtunnel_standalone_v0.1.md` 为权威；有歧义时请求设计裁决 |
| Task、REVIEW/DONE 或 Gate 证据分歧 | 以开发计划和当前 `AGENTS.md` 的证据规则为准 |
| 工具链版本或 CI 命令分歧 | `AGENTS.md` 定义策略，`.github/workflows/ci.yml` 定义当前实际 Job/命令；二者漂移时报告而非自行放宽 |

### 3.6 审查结论与 Task 状态分离

代码审查只输出以下结论，不把它们当作开发计划状态：

| 审查结论 | 条件 |
|----------|------|
| `CHANGES_REQUESTED` | 存在未修复 P0/P1 |
| `APPROVED_WITH_FOLLOWUPS` | 无 P0/P1；只有已解释的 P2/P3 或明确不属于当前范围的后续项 |
| `APPROVED` | 无 P0/P1，且没有需要跟踪的 P2；P3 可按作者判断处理 |
| `UNABLE_TO_VERIFY` | 缺少源码、契约或稳定审查基线，无法形成可靠代码结论 |

验证证据必须另行标注为“当前审查对象已验证”“仅脏工作区开发反馈”“未运行”或“失败”，
并列出真实命令、平台和结果。代码审查 `APPROVED` 不等于 CI/Gate 通过；验证失败本身通常
形成 P0/P1 Finding，未运行项则作为剩余风险和 Task/Gate 证据缺口记录。

开发计划状态仍由开发计划定义：

- `REVIEW` 表示产物已完成且任务规定的验收命令已执行，等待复审；它不是 PR Label，也不表示已经批准。
- 复审发现 P0/P1 后，应先修复并重新验证最新 Diff；是否回写 `IN_PROGRESS` 取决于任务实际状态，由文档同步流程处理，不能由审查模板自动改写。
- `DONE` 或 Gate 只有在真实产物、关键断言、失败分支、要求的独立复审和可复现证据全部齐备后才能标记。
- M0-10 完成后的新 `DONE` 必须记录 Commit SHA、验收命令与结果，以及 CI Run 链接或编号；脏工作区结果不能替代 CI。

---

## 4. 自动化 Gate 与 CI 集成

### 4.1 已有 CI Gate（不得绕过）

下表只是便于阅读的当前能力摘要，不是第二份 CI 权威。审查时必须打开当前
`.github/workflows/ci.yml` 核对真实 Job、包列表、平台和命令；发生漂移时以 Workflow
为当前执行事实，并把它与 `AGENTS.md`/开发计划的策略冲突作为 Finding 报告。

| Gate | 内容 | 平台 |
|------|------|------|
| 工具链验证 | Go `1.27.1+`（仅 `1.27.x`）+ `GOTOOLCHAIN=local` + Node `v24.19.0` + npm `11.17.0` | Linux amd64/arm64 |
| Proto 契约 | `lint` + `breaking` + `generate-check` | Linux amd64/arm64 |
| OpenAPI 契约 | `validate` + `test-openapi.sh` | Linux amd64/arm64 |
| Web 构建 | `ci` + `check` + `build` | Linux amd64/arm64 |
| Go 测试 | `go test ./...` + 定向 Race Suite | Linux amd64/arm64 |
| Go 静态检查 | `go vet ./...` | Linux amd64/arm64 |
| 构建 | `go build ./cmd/server ./cmd/agent` | Linux amd64/arm64 |
| OCI Smoke | Server + Agent 容器冒烟 | Linux amd64/arm64 |
| 生成物清洁 | `git diff --check` + `git status --porcelain` 为空 | 全平台 |
| Windows Agent | 测试 + Race + vet + arm64 交叉编译 + Service Smoke | Windows |

### 4.2 审查者对 CI 结果的判定规则

| CI 状态 | 审查者行动 |
|---------|-----------|
| 全绿 | 继续人工审查 |
| 任一 required Job 失败 | 阻断批准、合并和正式 CI/Gate 证据，要求作者修复或取得有权裁决者对 Workflow/Branch Protection 的明确变更 |
| 非 required Job 失败 | 记录失败、影响范围和剩余风险；不得宣称“完整 CI 全绿” |
| Race Suite 失败 | 阻断合并，要求作者修复并发缺陷 |
| 生成物清洁检查失败 | 阻断合并，生成物未提交或工作区有残留 |

当前 Windows Job 无条件运行；审查者不能仅凭“看起来与 Windows 无关”自行忽略失败。
如果未来通过路径过滤或 Branch Protection 明确把某 Job 设为非 required，再按实际配置
记录，而不是在本规范中预设例外。

### 4.3 脏工作区结果的处理

> **核心原则：脏工作区结果只能作为开发反馈，不能冒充完整 Gate。**

| 验证类型 | 脏工作区 | 干净 checkout / CI |
|----------|---------|-------------------|
| `go test` 定向测试 | ✅ 可作为开发反馈 | ✅ 正式通过证据 |
| `proto.sh generate-check` | ⚠️ 仅等价参考 | ✅ 正式 Gate |
| 单项 Schema/Fixture `VALID` | ⚠️ 不能冒充完整 CI | ✅ 需在 CI 中通过 |
| 交叉编译通过 | ⚠️ 不能冒充原生 Smoke | ✅ 需原生运行 |

---

## 5. 审查沟通原则

### 5.1 审查者行为准则

1. **对代码不对人** — 评论针对代码质量，不评价作者能力
2. **解释为什么** — 不只说"改这个"，说清楚原因和影响
3. **结论与语气分离** — P0/P1 要明确表达必须修复；P2/P3 可以提供选择，不用命令式口吻制造虚假强制项
4. **首轮尽量完整** — 首轮集中返回已发现问题；修复引入的新回归仍可在复审中追加
5. **表扬好的代码** — 发现巧妙的解决方案或清晰的模式时明确表扬
6. **意图不清时提问** — 不假设代码是错的，先问 "这里的意图是 X 吗？"
7. **尊重最小改动原则** — 不要求超出当前任务范围的重构

### 5.2 作者行为准则

1. **变更说明自包含** — 写清楚审查对象、改了什么、为什么改、怎么验证；不要求必须存在远端 PR
2. **回复每一条意见** — 修复、解释或礼貌拒绝，不留未回复意见
3. **风险优先** — 先修 P0/P1，再处理 P2/P3
4. **不掩饰未验证** — 未运行的验证如实说明，不写成通过
5. **小步提交** — 大型变更拆分为多个可独立审查的 PR

---

## 6. 快速参考卡

### 6.1 变更提交审查前 30 秒自检

```
□ 审查对象和 base/head 已冻结？staged/unstaged/untracked 已分清？
□ 已读取当前路径 AGENTS 和相关机器/行为契约？
□ 若涉及 Go：GOTOOLCHAIN=local + tools/check-go-version 已通过？
□ 若涉及 Go：gofmt、改动包测试、全包 test/vet、go mod verify 已通过？
□ 没有 Secret 在日志/错误/测试中？
□ 没有手改 *.pb.go？
□ git diff --check 通过，工作区只有预期文件？
□ 改了 Proto/OpenAPI？→ Wrapper 重新生成并提交
□ 改了 Web？→ npm ci + check + build
□ 改了并发路径？→ 受影响包定向 Race + 当前 CI 基础 Suite
□ 新增功能有测试？
```

### 6.2 审查者 60 秒快速扫描

```
□ 审查对象、Base/Head 和排除范围是否清楚？
□ 当前 Workflow 的 required CI 是否全绿？
□ 是否有 Secret 泄露风险（Token/密码/Cookie/Key）？
□ 是否有 fire-and-forget goroutine 或锁内阻塞 IO？
□ 是否有 context.Background() 截断上游调用链？
□ 是否有未处理的有业务意义的 Close/Flush/Commit？
□ 是否与冻结契约（Proto/Schema/OpenAPI）冲突？
□ 测试是否覆盖了关键行为和失败分支（非仅 Happy Path）？
□ 变更是否超出当前任务范围（最小改动）？
□ 平台特定代码是否有 *_unsupported.go 失败路径？
```

### 6.3 严重性快速判定

| 如果发现... | 分级 |
|-------------|------|
| 完整 Token/Secret 出现在日志、错误或测试输出中 | P0 |
| 生产 goroutine 无 owner、停止条件或等待路径 | 通常 P0/P1，按可触发性裁决 |
| 手改 `*.pb.go` 或实现违反冻结 Wire Contract | P0 |
| 工具链使用 `latest`/`stable` 或绕过 `GOTOOLCHAIN=local` | P0/P1，按证据有效性裁决 |
| 新关键逻辑只有 Happy Path 或浅断言 | 通常 P1 |
| 需要保留错误链却使用 `%v` | P1/P2，按调用方行为裁决 |
| 变量名不清晰但不影响当前正确性 | P2/P3 |
| 导出函数缺少必要注释 | P2/P3 |

快速判定不能代替证据。最终 Finding 必须说明具体触发条件、违反的权威规则和影响范围。
