#!/bin/sh
set -eu


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

case "$actual_version" in
    go1.27.*)
        patch=${actual_version#go1.27.}
        case "$patch" in
            ''|0*|*[!0-9]*)
                echo "expected Go 1.27.x, got $actual_version" >&2
                exit 1
                ;;
        esac
        [ "$patch" -ge 1 ] || {
            echo "expected Go 1.27.x, got $actual_version" >&2
            exit 1
        }
        ;;
    *)
        echo "expected Go 1.27.x, got $actual_version" >&2
        exit 1
        ;;
esac

echo "Go toolchain check passed: $actual_version ($actual_mode)"
