#!/bin/sh
set -eu

# 所有子命令只使用 .tools/bin 中经过版本校验的工具，禁止回落到 PATH。
script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd)
versions_file="$script_dir/versions.env"
bin_dir="$repo_root/.tools/bin"
buf_bin="$bin_dir/buf"
plugin_bin="$bin_dir/protoc-gen-go"
proto_dir="$repo_root/api/proto"
generated_dir="$repo_root/internal/protocol/gen"
baseline_file="$repo_root/api/proto/baseline/v1.binpb"

fail() {
    printf 'proto: %s\n' "$1" >&2
    exit 1
}

usage() {
    printf 'Usage: %s {lint|breaking|generate-check}\n' "$0" >&2
    exit 2
}

[ -r "$versions_file" ] || fail "missing $versions_file"
. "$versions_file"

select_buf_sha256() {
    platform=$(uname -s)
    architecture=$(uname -m)

    case "$platform:$architecture" in
        Linux:x86_64)
            expected_buf_sha256=$BUF_LINUX_AMD64_SHA256
            ;;
        Linux:aarch64|Linux:arm64)
            expected_buf_sha256=$BUF_LINUX_ARM64_SHA256
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

validate_tools() {
    command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
    [ -x "$buf_bin" ] || fail "missing managed Buf; run $script_dir/bootstrap-proto.sh"
    [ -x "$plugin_bin" ] || fail "missing managed protoc-gen-go; run $script_dir/bootstrap-proto.sh"

    actual_buf_sha256=$(file_sha256 "$buf_bin")
    [ "$actual_buf_sha256" = "$expected_buf_sha256" ] || \
        fail "managed Buf SHA-256 mismatch: got $actual_buf_sha256"
    [ "$("$buf_bin" --version)" = "$BUF_VERSION" ] || fail 'managed Buf version mismatch'
    [ "$("$plugin_bin" --version)" = "$PROTOC_GEN_GO_EXPECTED_VERSION" ] || \
        fail 'managed protoc-gen-go version mismatch'
}

has_proto_input() {
    first_proto=$(find "$proto_dir" -type f -name '*.proto' -print | sed -n '1p')
    [ -n "$first_proto" ]
}

ensure_no_generated_proto() {
    [ -d "$generated_dir" ] || return 0
    first_generated=$(find "$generated_dir" -type f -name '*.pb.go' -print | sed -n '1p')
    [ -z "$first_generated" ] || \
        fail "found generated code without Proto input: $first_generated"
}

[ "$#" -eq 1 ] || usage
select_buf_sha256
validate_tools

cd "$repo_root"

case "$1" in
    lint)
        if ! has_proto_input; then
            printf 'SKIP: empty Protocol scaffold; no Proto files to lint.\n'
            exit 0
        fi
        "$buf_bin" lint
        ;;
    breaking)
        if ! has_proto_input; then
            printf 'SKIP: initial Protocol contract is not frozen; no Proto files exist.\n'
            exit 0
        fi
        # 首个不可变 Baseline 在 M05-04 由已冻结的三份 Proto 一次性构建并提交。
        # 之后始终比较该已提交二进制，禁止与当前 Schema 自比较伪造通过。
        [ -f "$baseline_file" ] || \
            fail "Protocol files exist but the initial Breaking baseline is missing: $baseline_file"
        "$buf_bin" breaking --against "$baseline_file"
        ;;
    generate-check)
        if ! has_proto_input; then
            ensure_no_generated_proto
            printf 'SKIP: empty Protocol scaffold; no code generation input.\n'
            exit 0
        fi

        command -v git >/dev/null 2>&1 || fail 'git is required for generate-check'
        "$buf_bin" generate

        # git diff 不覆盖 staged/untracked 文件，因此两种检查必须同时执行。
        git diff --exit-code -- api/proto internal/protocol/gen buf.lock
        generated_status=$(git status --porcelain --untracked-files=all -- \
            api/proto internal/protocol/gen buf.lock)
        [ -z "$generated_status" ] || {
            printf '%s\n' "$generated_status" >&2
            fail 'generated Protocol files differ from the repository state'
        }
        ;;
    *)
        usage
        ;;
esac
