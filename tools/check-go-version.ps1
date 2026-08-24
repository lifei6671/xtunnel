$ErrorActionPreference = 'Stop'

$expectedVersion = 'go1.27.0'

# 必须在调用 Go 前检查环境；否则 toolchain 指令可能先下载或切换工具链，导致
# 不合规的工具链进入构建过程后才被发现。
if ($env:GOTOOLCHAIN -ne 'local') {
    Write-Error 'GOTOOLCHAIN must be set to local before running Go commands.'
    exit 1
}

$actualMode = (& go env GOTOOLCHAIN).Trim()
$actualVersion = (& go env GOVERSION).Trim()

if ($actualMode -ne 'local') {
    Write-Error "expected GOTOOLCHAIN=local, got $actualMode"
    exit 1
}

if ($actualVersion -ne $expectedVersion) {
    Write-Error "expected $expectedVersion, got $actualVersion"
    exit 1
}

Write-Output "Go toolchain check passed: $actualVersion ($actualMode)"
