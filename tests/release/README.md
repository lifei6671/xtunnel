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
