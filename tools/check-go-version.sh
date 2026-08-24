#!/bin/sh
set -eu

expected_version='go1.27.0'

# 必须在调用 Go 前检查环境；否则 toolchain 指令可能先下载或切换工具链，导致
# 不合规的工具链进入构建过程后才被发现。
if [ "${GOTOOLCHAIN-}" != 'local' ]; then
    echo 'GOTOOLCHAIN must be set to local before running Go commands.' >&2
    exit 1
fi

actual_mode=$(go env GOTOOLCHAIN)
actual_version=$(go env GOVERSION)

if [ "$actual_mode" != 'local' ]; then
    echo "expected GOTOOLCHAIN=local, got $actual_mode" >&2
    exit 1
fi

if [ "$actual_version" != "$expected_version" ]; then
    echo "expected $expected_version, got $actual_version" >&2
    exit 1
fi

echo "Go toolchain check passed: $actual_version ($actual_mode)"
