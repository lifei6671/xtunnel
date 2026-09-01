# M7-06 Protocol/Parser Fuzz Evidence

## 当前状态

- 状态：`DONE`
- 基线 Commit：`207dcd96aa04ac41255e9c53956059cfbabaf73d`
- 起始工作区：clean
- 用户授权：“继续下一步”与随后“继续”授权启动和实现 M7-06；用户随后明确回复
  “确认修改CI”授权最小接入 CI Short Fuzz，并明确回复“确认暂存、提交并推送 M7-06”
  授权固定 10 路径正式提交与推送；用户最终明确回复“`M7-06 阶段复审通过`”。

## 范围与修正

- 新增 8 个最小 Go Fuzz 目标，覆盖 UVarint、Frame、ControlEnvelope、WorkHello、
  Host、RawPath/RequestURI、危险路径编码和 Forwarded Header。
- `parseForwardedFor` 原先会接受 `fe80::1%zone` 后静默剥离 zone，与冻结的 XFF
  “纯 IP”契约冲突；当前最小修正为显式拒绝 zone，不改变公共 API、配置或依赖。
- HTTP Wire 先经过 `http.ReadRequest`；Parser-stage 拒绝与 Matcher-stage
  `INVALID_PATH` 证据保持分离。
- 未修改 Proto/OpenAPI/生成物、Schema、Migration、依赖/Lockfile、部署、权限或日志
  契约；CI 仅在既有 Linux `verify` 矩阵增加 M7-06 Short Fuzz 步骤。

## Windows 开发反馈

环境：Windows、Go `go1.27.0`、`GOTOOLCHAIN=local`。

已通过：

```text
go test -count=1 -timeout=60s ./tests/fuzz ./internal/server/route ./internal/server/httpingress

8 个目标分别执行：
go test <package> -run '^$' -fuzz '^<exact-target>$' \
  -fuzztime=5s -fuzzminimizetime=2s -parallel=1 -timeout=60s
```

短时 mutation 结果：8/8 PASS；单目标执行数约 28,747 至 143,764。本结果是脏工作区
开发反馈，不是 clean `full`、Linux Runtime 或 CI 证据。

## 验证结果与剩余证据

- 统一 Runner 的仓库外私有 Cache `smoke` 读回：`PASS`。隔离 Docker Linux amd64、
  Go `go1.27.0/local`、基线 Commit `207dcd96...`，8/8 目标完成；该次旧候选的结果目录
  未保留可独立定位信息，且当时 Runner 尚未增加进程级 watchdog 与结束 Commit/Tree
  复核，因此只保留为开发反馈，不能升级为正式验收证据。
- 审计修复后的隔离 Linux amd64 clean 候选 `full`：`PASS`。候选从基线 Commit
  `207dcd96aa04ac41255e9c53956059cfbabaf73d` 与当前 delivery-owned 文件组装，仓库外
  临时 Commit 为 `c5c09009422ec23de615278bd4b6ea0cb35702f9`，Tree 为
  `b8a87a4994b4bb9aa7b21ff77eaf5733f1279b7b`；它不是正式项目 Commit。
- `full` 环境：Docker Linux amd64、Go `go1.27.0/local`、Git `2.39.5`、GNU
  coreutils `timeout`/`sha256sum` `9.1`。8 个目标各运行 60 秒，执行数约 `468,350`
  至 `1,662,600`，8/8 PASS；未观测到 panic、OOM、timeout 或失败语料。
- `full` 结果：仓库外目录
  `E:\wx_lifeilin\github.com\lifei6671\.codex-tmp\xtunnel-m706-full-20260901-163844\full-results`；
  运行前后工作区快照均为空，Artifact 清单 SHA-256 为
  `BFC24CFC7E7B9B2BA993B515A534CBE7E607FBBB3FD42CDFAC96E99C93A45528`。
- CI Short Fuzz Workflow：`WIRED`。既有 Linux amd64/arm64 Runner、Go `1.27.0/local`、
  权限和 45 分钟 Job 超时均未改变；新增步骤使用 12 分钟 Step 超时，执行
  `sh ./tests/fuzz/run-m7-06.sh -m smoke`，校验 Artifact 清单内全部产物后再输出清单
  自身 SHA-256。
- CI 命令形状的隔离 Docker Linux amd64 clean 候选：`PASS`。仓库外临时 Commit
  `8d09780f4bf79a0455ab0be345ab919dadc2c0ad`、Tree
  `8c84e1b941eab4e1474367b9534cfdf37237d307`，8/8 Short Fuzz PASS；结果目录为
  `E:\wx_lifeilin\github.com\lifei6671\.codex-tmp\xtunnel-m706-ci-step-20260901-1715`，
  Artifact 清单 SHA-256 为
  `57B76AF8F3786B5EEA164791D7D62E5081BA1157AD966643459784617B30F576`。该结果不是
  GitHub Actions，也不覆盖原生 Linux arm64。
- WSL 本地尝试在执行 Fuzz 前分别因 PowerShell/Bash 变量互操作和 WSL 内缺少 `go`
  失败；两次均不是产品或 Fuzz 失败证据。
- Windows `go test -count=1 -timeout=300s ./...`：`PASS`
- Windows `go vet ./...`：`PASS`
- Windows 三个改动 package 的 `go test -race -count=1 -timeout=120s`：`PASS`
- CI 接线前的 Tier 3 独立实现复审：Gate=`PASSED`、Coverage=`COMPLETE`、
  Freshness=`FRESH`、P0/P1/P2=`0/0/0`。
- CI/Runner 首轮独立审计：发现 1 项 P1，原 Workflow 只输出 Artifact 清单自身
  SHA-256，未执行清单条目校验；Repair round 1 已补充 `sha256sum -c`。
- Repair round 1 的隔离 Docker Linux amd64 clean 候选：`PASS`。仓库外临时 Commit
  `b686be08edafdbe295538a765bddea89a9c79444`、Tree
  `ebf3b2c8a491373a7d73ee5f21229275976e487a`，Runner mode=`100755`；8/8 Short
  Fuzz PASS，`sha256sum -c` 对 12/12 清单条目返回 `OK`。结果目录为
  `E:\wx_lifeilin\github.com\lifei6671\.codex-tmp\xtunnel-m706-ci-repair-20260901-1751`，
  Artifact 清单 SHA-256 为
  `F35DEAEB33DEBC0496B5B72D275071E47E43BA2865F3EC40DD1A18E23EBB18B1`。正式复验
  前两次容器 Harness 分别因 login shell 丢失 Go PATH、未预建模拟 `$RUNNER_TEMP` 失败，
  均发生在 Fuzz 启动前；修正 Harness 后通过，不记为产品失败或 CI PASS。
- Repair round 1 最终工作树独立复审：原 Runner P1=`CLOSED`；实现、Runner、状态契约
  三路均为 Gate=`PASSED`、Coverage=`COMPLETE`、Freshness=`FRESH`、
  P0/P1/P2=`0/0/0`。该结论绑定当前工作树 Blob，不替代正式 Commit 上的 commit-bound
  最终复审。
- 正式 Commit：`5b88b46f29be882525038ea3f2c749fd24a53646`；Parent
  `207dcd96aa04ac41255e9c53956059cfbabaf73d`；Tree
  `dcc9908b6769477610a28cd58f69aea46ace3347`；Runner mode=`100755`；已推送至
  `origin/master`，提交后工作区 clean。
- 精确 GitHub CI：[Run #33492076511](https://github.com/lifei6671/xtunnel/actions/runs/33492076511)
  Head SHA 精确匹配，四 Job 全部 `success`。原生 Linux arm64/amd64 均执行 8/8
  Short Fuzz 并通过，Artifact 清单 12/12 条目为 `OK`；清单 SHA-256 分别为
  `416ff4111685323897683d2fe0261528b2c566cd58e771e0ce9372be2d9e60b7` 与
  `2f39550390f0e899294ee5eee25158d50e3c7ff891de40d1291c8e0f65469cf3`。
- commit-bound Tier 3：实现/集成和 CI/Runner 分区均通过；状态契约分区发现的提交后
  文档时态 P2 已由证据 Commit `6f2a8f688dfba4954edc1d6ea9e8b8d5bc14a29a` 修正。
  该 Commit 的精确 [CI #33493628266](https://github.com/lifei6671/xtunnel/actions/runs/33493628266)
  四 Job 全部成功，两个 Linux Job 均通过 8/8 Short Fuzz、Artifact 12/12 校验与最终
  clean-tree；最终独立复审 Gate=`PASSED`、Coverage=`COMPLETE`、Freshness=`FRESH`、
  P0/P1/P2=`0/0/0`。

## 用户阶段复审

用户明确回复“`M7-06 阶段复审通过`”。M7-06 已从 `REVIEW` 转为 `DONE`；全局
`DONE` 从 `90/95` 更新为 `91/95`，M7 从 `5/10` 更新为 `6/10 IN_PROGRESS`。
本次不启动 M7-07，也不勾选 Alpha Release Gate Checklist。
