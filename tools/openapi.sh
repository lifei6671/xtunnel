#!/bin/sh
set -eu

# 开发机和 CI 共用此入口，只校验唯一的 OpenAPI 机器契约。
script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd)
versions_file="$script_dir/versions.env"
vacuum_bin="$repo_root/.tools/bin/vacuum"
openapi_dir="$repo_root/api/openapi"
openapi_file="$openapi_dir/openapi.yaml"
ruleset_file="$openapi_dir/ruleset.yaml"

fail() {
    printf 'openapi: %s\n' "$1" >&2
    exit 1
}

usage() {
    printf 'Usage: %s validate\n' "$0" >&2
    exit 2
}

[ "$#" -eq 1 ] || usage
[ "$1" = validate ] || usage

[ -r "$versions_file" ] || fail "missing $versions_file"
. "$versions_file"

select_binary_sha256() {
    platform=$(uname -s)
    architecture=$(uname -m)

    case "$platform:$architecture" in
        Linux:x86_64)
            expected_vacuum_sha256=$VACUUM_LINUX_AMD64_BINARY_SHA256
            ;;
        Linux:aarch64|Linux:arm64)
            expected_vacuum_sha256=$VACUUM_LINUX_ARM64_BINARY_SHA256
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

validate_tool() {
    command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
    [ -x "$vacuum_bin" ] || fail "missing managed vacuum; run $script_dir/bootstrap-openapi.sh"

    actual_vacuum_sha256=$(file_sha256 "$vacuum_bin")
    [ "$actual_vacuum_sha256" = "$expected_vacuum_sha256" ] || \
        fail "managed vacuum SHA-256 mismatch: got $actual_vacuum_sha256"
    [ "$("$vacuum_bin" version)" = "$VACUUM_VERSION" ] || \
        fail 'managed vacuum version mismatch'
}

select_binary_sha256
validate_tool

[ -r "$openapi_file" ] || fail "missing $openapi_file"
[ -r "$ruleset_file" ] || fail "missing $ruleset_file"

cd "$repo_root"
# Vacuum 会区分解析错误和规则失败；Wrapper 对外统一为“契约校验失败=1”，
# 从而与自身的命令用法错误（退出码 2）保持清晰边界。
if ! VACUUM_NO_UPDATE_CHECK=true "$vacuum_bin" lint \
    --no-update-check \
    --no-style \
    --fail-severity error \
    --remote=false \
    --base "$openapi_dir" \
    --ruleset "$ruleset_file" \
    "$openapi_file"; then
    fail 'OpenAPI validation failed'
fi
