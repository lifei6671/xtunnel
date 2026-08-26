# Overview: XTunnel 代码审查标准与流程

## 完成内容

为 XTunnel 项目制定了一份完整的代码审查标准与流程文档，位于 `docs/code_review_standard.md`。

## 文档结构

1. **审查维度与严重性分级** — 定义五大审查维度（正确性/安全性/可维护性/性能/测试）和三级严重性（🔴 Blocker / 🟡 Suggestion / 💭 Nit），每级附带具体检查项表
2. **项目特定审查检查点** — 针对 XTunnel 的并发模型、Secret 管理、契约一致性、测试质量、变更边界，给出逐条 Yes/No 检查清单
3. **审查流程** — PR 提交前自检清单、审查触发条件与角色分工、时间盒、意见反馈格式模板、冲突升级路径、审查完成判定（REVIEW/DONE/Gate）
4. **自动化 Gate 与 CI 集成** — 列出已有 CI Gate（工具链/Proto/OpenAPI/Web/测试/Race/生成物清洁/Windows），定义审查者对 CI 结果的判定规则和脏工作区结果的处理原则
5. **审查沟通原则** — 审查者和作者各自的行为准则
6. **快速参考卡** — PR 提交前 30 秒自检、审查者 60 秒快速扫描、严重性快速判定表

## 关键设计决策

- **不重复 AGENTS.md**：文档与 AGENTS.md 配合使用，不复制已有规则，而是将规则转化为可执行的审查检查点
- **项目特定化**：所有检查点针对 XTunnel 的实际架构（Tunnel Token AES-256-GCM、Control Session Single Owner、Race Suite 包列表、Proto/OpenAPI 契约层级），而非通用 Go 项目
- **CI 对齐**：Gate 定义直接引用 `.github/workflows/ci.yml` 的实际 Step，包括 Linux amd64/arm64 和 Windows Agent 矩阵
- **脏工作区原则**：明确区分开发反馈与正式 Gate，防止脏工作区等价检查冒充 CI 通过
