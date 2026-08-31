#!/bin/sh

# 运行 M7-02 重连风暴测试。所有中间输出只进入 Linux-native /tmp，退出时清理。
# smoke 只执行 100 Connector；full 依次执行 100/500/1000/5000 Connector。

set -eu

usage() {
    printf '%s\n' 'Usage: run-m7-02.sh [-m smoke|full] [-b prebuilt-directory]'
    printf '%s\n' '  -m  Run mode. Default: smoke.'
    printf '%s\n' '  -b  Directory containing bootstrap.test and manifest.txt.'
    printf '%s\n' '  -h  Show this help.'
}

fail() {
    printf 'm7-02 chaos: %s\n' "$1" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

read_soft_fd_limit() {
    # Runner 已在入口限制为 Linux；Linux 的 dash/ash/bash 均提供任务要求的 -n。
    # shellcheck disable=SC3045
    ulimit -n
}

manifest_value() {
    manifest_key=$1
    sed -n "s/^${manifest_key}=//p" "$prebuilt_dir/manifest.txt"
}

verify_manifest_value() {
    manifest_key=$1
    expected_value=$2
    actual_value=$(manifest_value "$manifest_key")
    [ "$actual_value" = "$expected_value" ] || \
        fail "prebuilt manifest $manifest_key must equal $expected_value"
}

verify_file_hash() {
    verify_path=$1
    expected_hash=$2
    actual_hash=$(sha256sum "$verify_path")
    actual_hash=${actual_hash%% *}
    [ "$actual_hash" = "$expected_hash" ] || \
        fail "SHA-256 mismatch: $verify_path"
}

cleanup() {
    cleanup_status=$?
    trap - 0
    trap '' HUP INT TERM

    if [ -n "$run_dir" ]; then
        if ! rm -f "$run_dir/100.txt" "$run_dir/500.txt" \
            "$run_dir/1000.txt" "$run_dir/5000.txt" \
            "$run_dir/manifest.txt" "$run_dir/bootstrap.test"; then
            printf '%s\n' 'm7-02 chaos: failed to remove temporary files' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        if ! rmdir "$run_dir"; then
            printf '%s\n' 'm7-02 chaos: failed to remove temporary directory' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        run_dir=
    fi

    exit "$cleanup_status"
}

check_full_fd_limit() {
    required_fd_limit=16384
    soft_fd_limit=$(read_soft_fd_limit) || fail 'cannot read the soft open-file limit'

    case $soft_fd_limit in
        unlimited) ;;
        ''|*[!0-9]*) fail "unexpected soft open-file limit: $soft_fd_limit" ;;
        *)
            [ "$soft_fd_limit" -ge "$required_fd_limit" ] || \
                fail "5000 Connector tier requires ulimit -n >= $required_fd_limit; got $soft_fd_limit"
            ;;
    esac

    printf 'M7-02 5000 Connector FD preflight: soft=%s required=%s\n' \
        "$soft_fd_limit" "$required_fd_limit"
}

run_case() {
    connector_count=$1
    case_timeout=$2
    case_output="$run_dir/$connector_count.txt"

    if [ "$connector_count" -eq 5000 ]; then
        check_full_fd_limit
    fi

    printf 'M7-02 tier start: connectors=%s timeout=%s\n' \
        "$connector_count" "$case_timeout"
    if [ -n "$test_binary" ]; then
        if ! XTUNNEL_M7_02_CONNECTORS=$connector_count \
            "$test_binary" -test.run '^TestM7ReconnectStorm$' \
            -test.count=1 -test.timeout="$case_timeout" -test.v \
            >"$case_output" 2>&1; then
            cat "$case_output" >&2
            fail "Connector tier failed: $connector_count"
        fi
    else
        if ! XTUNNEL_M7_02_CONNECTORS=$connector_count \
            go test ./internal/server/bootstrap -run '^TestM7ReconnectStorm$' \
            -count=1 -timeout="$case_timeout" -v >"$case_output" 2>&1; then
            cat "$case_output" >&2
            fail "Connector tier failed: $connector_count"
        fi
    fi
    cat "$case_output"
    printf 'M7-02 tier passed: connectors=%s timeout=%s\n' \
        "$connector_count" "$case_timeout"
}

mode=smoke
prebuilt_dir=
run_dir=
test_binary=

while [ "$#" -gt 0 ]; do
    case $1 in
        -m)
            [ "$#" -ge 2 ] || fail '-m requires a value'
            mode=$2
            shift 2
            ;;
        -b)
            [ "$#" -ge 2 ] || fail '-b requires a value'
            prebuilt_dir=$2
            shift 2
            ;;
        -h)
            usage
            exit 0
            ;;
        --)
            shift
            break
            ;;
        *)
            usage >&2
            fail "unknown argument: $1"
            ;;
    esac
done

[ "$#" -eq 0 ] || fail 'positional arguments are not supported'
case $mode in
    smoke|full) ;;
    *) fail "unsupported mode: $mode" ;;
esac

require_command uname
[ "$(uname -s)" = Linux ] || fail 'M7-02 chaos runner requires Linux'
require_command cat
require_command git
require_command mktemp
require_command rm
require_command rmdir
require_command sed
require_command sha256sum

script_dir=$(CDPATH='' cd -P "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH='' cd -P "$script_dir/../.." && pwd -P)
[ -f "$repo_root/go.mod" ] || fail 'repository root does not contain go.mod'
[ -x "$repo_root/tools/check-go-version.sh" ] || \
    fail 'repository root does not contain an executable Go version check'
cd "$repo_root"

git_root=$(git rev-parse --show-toplevel) || fail 'cannot resolve Git repository root'
git_root=$(CDPATH='' cd -P "$git_root" && pwd -P)
[ "$git_root" = "$repo_root" ] || fail 'script is not running from the XTunnel repository root'

if [ "$mode" = full ] && [ -n "$(git status --porcelain --untracked-files=all)" ]; then
    fail 'full mode requires a clean worktree'
fi

if [ ! -d /tmp ] || [ ! -w /tmp ]; then
    fail 'a writable Linux-native /tmp is required'
fi
run_dir=$(mktemp -d /tmp/xtunnel-m7-02.XXXXXX) || \
    fail 'cannot create temporary run directory'
trap 'cleanup' 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if [ -n "$prebuilt_dir" ]; then
    [ -d "$prebuilt_dir" ] || fail "prebuilt directory not found: $prebuilt_dir"
    prebuilt_dir=$(CDPATH='' cd -P "$prebuilt_dir" && pwd -P)
    [ -r "$prebuilt_dir/manifest.txt" ] || fail 'prebuilt manifest not found'
    [ -f "$prebuilt_dir/bootstrap.test" ] || fail 'prebuilt bootstrap.test not found'

    verify_manifest_value go_version go1.27.0
    verify_manifest_value toolchain local
    verify_manifest_value goos linux
    verify_manifest_value goarch amd64
    verify_manifest_value goamd64 v1
    verify_manifest_value cgo_enabled 0
    manifest_clean=$(manifest_value worktree_clean)
    case $manifest_clean in
        true|false) ;;
        *) fail 'prebuilt manifest worktree_clean must equal true or false' ;;
    esac
    manifest_commit=$(manifest_value commit)
    current_commit=$(git rev-parse HEAD)
    [ "$manifest_commit" = "$current_commit" ] || \
        fail 'prebuilt manifest Commit does not match the current checkout'
    expected_binary_hash=$(manifest_value bootstrap_sha256)
    [ -n "$expected_binary_hash" ] || \
        fail 'prebuilt manifest is missing bootstrap_sha256'
    verify_file_hash "$prebuilt_dir/bootstrap.test" "$expected_binary_hash"

    if [ "$mode" = full ]; then
        verify_manifest_value worktree_clean true
        require_command chmod
        require_command cp
        require_command stat
        run_filesystem=$(stat -f -c %T "$run_dir") || \
            fail 'cannot identify temporary run filesystem'
        case $run_filesystem in
            9p|drvfs|v9fs) fail 'full mode temporary directory must not use WSL DrvFS' ;;
        esac

        manifest_hash=$(sha256sum "$prebuilt_dir/manifest.txt")
        manifest_hash=${manifest_hash%% *}
        cp "$prebuilt_dir/manifest.txt" "$prebuilt_dir/bootstrap.test" "$run_dir/"
        chmod 400 "$run_dir/manifest.txt"
        chmod 500 "$run_dir/bootstrap.test"
        verify_file_hash "$run_dir/manifest.txt" "$manifest_hash"
        verify_file_hash "$run_dir/bootstrap.test" "$expected_binary_hash"
        test_binary="$run_dir/bootstrap.test"
    else
        [ -x "$prebuilt_dir/bootstrap.test" ] || \
            fail 'prebuilt bootstrap.test is not executable'
        test_binary="$prebuilt_dir/bootstrap.test"
    fi
else
    require_command go
    export GOTOOLCHAIN=local
    "$repo_root/tools/check-go-version.sh"
fi

printf 'M7-02 mode: %s\n' "$mode"
printf 'M7-02 commit: %s\n' "$(git rev-parse HEAD)"
printf 'M7-02 platform: %s\n' "$(uname -a)"
printf 'M7-02 soft open-file limit: %s\n' "$(read_soft_fd_limit)"

if [ "$mode" = smoke ]; then
    run_case 100 2m
else
    run_case 100 2m
    run_case 500 5m
    run_case 1000 10m
    run_case 5000 30m
fi

printf '%s\n' 'M7-02 chaos run completed.'
