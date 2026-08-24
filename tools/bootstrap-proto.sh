#!/bin/sh
set -eu

# 本脚本只安装仓库锁定的工具，任何下载或构建失败都不会覆盖已有工具。
script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd)
versions_file="$script_dir/versions.env"
bin_dir="$repo_root/.tools/bin"
buf_bin="$bin_dir/buf"
plugin_bin="$bin_dir/protoc-gen-go"
buf_temp=''
plugin_temp=''
plugin_temp_dir=''

fail() {
    printf 'proto bootstrap: %s\n' "$1" >&2
    exit 1
}

cleanup() {
    if [ -n "$buf_temp" ]; then
        rm -f -- "$buf_temp"
    fi
    if [ -n "$plugin_temp" ]; then
        rm -f -- "$plugin_temp"
    fi
    if [ -n "$plugin_temp_dir" ]; then
        rm -rf -- "$plugin_temp_dir"
    fi
}

trap cleanup 0
trap 'cleanup; exit 1' HUP INT TERM

[ -r "$versions_file" ] || fail "missing $versions_file"
# versions.env 只包含本仓库维护的常量，作为两个 Wrapper 的单一版本来源。
. "$versions_file"

select_buf_distribution() {
    platform=$(uname -s)
    architecture=$(uname -m)

    case "$platform:$architecture" in
        Linux:x86_64)
            buf_asset=$BUF_LINUX_AMD64_ASSET
            buf_sha256=$BUF_LINUX_AMD64_SHA256
            ;;
        Linux:aarch64|Linux:arm64)
            buf_asset=$BUF_LINUX_ARM64_ASSET
            buf_sha256=$BUF_LINUX_ARM64_SHA256
            ;;
        *)
            fail "unsupported platform $platform/$architecture; use Linux x86_64 or arm64"
            ;;
    esac
}

file_sha256() {
    checksum=$(sha256sum "$1")
    printf '%s\n' "${checksum%% *}"
}

buf_is_current() {
    [ -x "$buf_bin" ] || return 1
    [ "$(file_sha256 "$buf_bin")" = "$buf_sha256" ] || return 1
    [ "$("$buf_bin" --version 2>/dev/null)" = "$BUF_VERSION" ] || return 1
}

plugin_is_current() {
    [ -x "$plugin_bin" ] || return 1
    [ "$("$plugin_bin" --version 2>/dev/null)" = "$PROTOC_GEN_GO_EXPECTED_VERSION" ] || return 1
}

select_buf_distribution

command -v curl >/dev/null 2>&1 || fail 'curl is required'
command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
command -v mktemp >/dev/null 2>&1 || fail 'mktemp is required'
command -v go >/dev/null 2>&1 || fail 'Go is required'

# 必须先通过统一版本入口，避免 toolchain 指令自动下载或切换 Go。
[ "${GOTOOLCHAIN-}" = 'local' ] || fail 'GOTOOLCHAIN must be set to local'
sh "$script_dir/check-go-version.sh"

umask 077
mkdir -p "$bin_dir"

if buf_is_current; then
    printf 'Buf %s is already installed.\n' "$BUF_VERSION"
else
    buf_temp=$(mktemp "$bin_dir/.buf.XXXXXX") || fail 'cannot create temporary Buf file'
    buf_url="$BUF_DOWNLOAD_BASE_URL/$buf_asset"
    printf 'Downloading Buf %s for %s...\n' "$BUF_VERSION" "$architecture"
    curl --fail --location --silent --show-error --output "$buf_temp" "$buf_url" || \
        fail 'Buf download failed'

    actual_sha256=$(file_sha256 "$buf_temp")
    [ "$actual_sha256" = "$buf_sha256" ] || fail "Buf SHA-256 mismatch: got $actual_sha256"
    chmod 0755 "$buf_temp"
    [ "$("$buf_temp" --version)" = "$BUF_VERSION" ] || fail 'Buf version check failed'
    mv -f -- "$buf_temp" "$buf_bin"
    buf_temp=''
    printf 'Installed Buf %s.\n' "$BUF_VERSION"
fi

if plugin_is_current; then
    printf 'protoc-gen-go %s is already installed.\n' "$PROTOC_GEN_GO_VERSION"
else
    # 版本输出包含可执行文件名，因此临时产物也必须命名为 protoc-gen-go。
    plugin_temp_dir=$(mktemp -d "$bin_dir/.protoc-gen-go.XXXXXX") || \
        fail 'cannot create temporary protoc-gen-go directory'
    plugin_temp="$plugin_temp_dir/protoc-gen-go"
    printf 'Building protoc-gen-go %s from tools/go.mod...\n' "$PROTOC_GEN_GO_VERSION"
    (
        cd "$script_dir"
        GOTOOLCHAIN=local go build -mod=readonly -o "$plugin_temp" "$PROTOC_GEN_GO_MODULE"
    ) || fail 'protoc-gen-go build failed'

    chmod 0755 "$plugin_temp"
    [ "$("$plugin_temp" --version)" = "$PROTOC_GEN_GO_EXPECTED_VERSION" ] || \
        fail 'protoc-gen-go version check failed'
    mv -f -- "$plugin_temp" "$plugin_bin"
    plugin_temp=''
    rmdir "$plugin_temp_dir"
    plugin_temp_dir=''
    printf 'Installed protoc-gen-go %s.\n' "$PROTOC_GEN_GO_VERSION"
fi

printf 'Proto toolchain bootstrap completed in %s.\n' "$bin_dir"
