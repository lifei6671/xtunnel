# M7-10 Release Gate

`run-m7-10.sh` 在同一次运行中构建 Agent 与 Server 的 Linux amd64/arm64
OCI Layout。校验器逐个验证 OCI Descriptor 的 SHA-256 与大小、精确平台集合、
非 root 用户、进程入口、Agent 默认 `run`、Server 数据卷，以及 Image Config 和
Layer 中不得出现的 Connection Token、Bearer Authorization 或 PEM Private Key 形状。

正式入口只在 Go `1.27.1` 或更新的 `1.27.x` 与可用的 Docker Buildx 环境运行：

```sh
GOTOOLCHAIN=local ./tests/release/run-m7-10.sh -o /tmp/xtunnel-m7-10
```

输出目录必须预先不存在且位于仓库外。成功后先按 `artifact-sha256.txt` 复核，再把
整个目录作为候选证据上传；它只有在 `alpha-release-gate` 汇总成功后才可作为发布
签核输入，本身不是正式发布物。任何构建日志或 Layout 校验失败都会阻止总 Gate。
Backup Archive 含技术方案白名单内的 Token Master Key，属于受控 Secret 载体，
不进入本候选证据。

## M8-05 Windows amd64 候选 Artifact

Windows CI 使用与产品 Gate、SCM smoke 相同的候选 EXE。产品 Gate 通过已认证的
System Info API 读取实际运行时版本，并生成包含完整 Commit SHA、Server SHA-256、
Windows/amd64 与前台/SCM 两种模式的成功报告。候选验证器将该报告与最终 EXE 字节绑定。

在干净的 Windows amd64 checkout、Go 1.27.1+ / GOTOOLCHAIN=local 环境中执行：

```powershell
$env:GOTOOLCHAIN = 'local'
.\tools\check-go-version.ps1
$serverPath = Join-Path $env:RUNNER_TEMP 'xtunnel-server.exe'
$productReport = Join-Path $env:RUNNER_TEMP 'm8-05-product.json'
$outputPath = Join-Path $env:RUNNER_TEMP 'm8-05-windows-candidate'
$commit = (git rev-parse HEAD).Trim()
go run ./tests/release/windowsverify -server $serverPath -commit $commit -product-report $productReport -output $outputPath
if ($LASTEXITCODE -ne 0) { throw 'Windows candidate verification failed' }
```

输出目录必须位于仓库外且预先不存在。输入 Binary 必须从该提交构建并注入
`v0.1.0-ci.<完整 Commit SHA>`。验证器检查 PE amd64、Go 构建元数据、VCS 身份、
产品报告中的实际版本与摘要绑定和 Secret 形状，生成标准命名的 `xtunnel-server-windows-amd64.exe`、
`manifest.json`、`artifact-sha256.txt` 及产品报告。

CI 上传 `m8-05-windows-amd64-<Commit>-attempt-<Run Attempt>` 候选证据，保留 14 天。
它不是正式 Release；M8-05 聚合 Gate 与用户阶段验收完成后才能更新 Windows Server
Preview 支持声明。此入口不改变既有 Linux OCI 的精确双平台集合。
