#!/bin/sh

# 运行 M7-03 Graceful Shutdown Chaos。测试 Binary 和输出只进入 Linux-native
# /tmp，退出时精确清理；Runner 不修改网络 namespace 或生产配置。

set -eu

usage() {
    printf '%s\n' 'Usage: run-m7-03.sh [-m smoke|full] [-b prebuilt-directory]'
    printf '%s\n' '  -m  smoke runs the TCP natural-drain case; full runs the complete matrix.'
    printf '%s\n' '  -b  Directory containing bootstrap.test and manifest.txt.'
    printf '%s\n' '  -h  Show this help.'
}

fail() {
    printf 'm7-03 chaos: %s\n' "$1" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
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

verify_go_127_patch() {
    actual_version=$(manifest_value go_version)
    case "$actual_version" in
        go1.27.*)
            patch=${actual_version#go1.27.}
            case "$patch" in ''|0*|*[!0-9]*) fail "prebuilt manifest go_version must be Go 1.27.x, got $actual_version" ;; esac
            [ "$patch" -ge 1 ] || fail "prebuilt manifest go_version must be Go 1.27.x, got $actual_version"
            ;;
        *) fail "prebuilt manifest go_version must be Go 1.27.x, got $actual_version" ;;
    esac
}

verify_file_hash() {
    verify_path=$1
    expected_hash=$2
    actual_hash=$(sha256sum "$verify_path")
    actual_hash=${actual_hash%% *}
    [ "$actual_hash" = "$expected_hash" ] || fail "SHA-256 mismatch: $verify_path"
}

cleanup() {
    cleanup_status=$?
    trap - 0
    trap '' HUP INT TERM
    if [ -n "$run_dir" ]; then
        if ! rm -f "$run_dir/output.txt" "$run_dir/manifest.txt" "$run_dir/bootstrap.test"; then
            printf '%s\n' 'm7-03 chaos: failed to remove temporary files' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        if ! rmdir "$run_dir"; then
            printf '%s\n' 'm7-03 chaos: failed to remove temporary directory' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        run_dir=
    fi
    exit "$cleanup_status"
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
[ "$(uname -s)" = Linux ] || fail 'M7-03 chaos runner requires Linux'
require_command git
require_command mktemp
require_command rm
require_command rmdir
require_command sed
require_command sha256sum
require_command stat

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
run_dir=$(mktemp -d /tmp/xtunnel-m7-03.XXXXXX) || fail 'cannot create temporary run directory'
trap 'cleanup' 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if [ -n "$prebuilt_dir" ]; then
    [ -d "$prebuilt_dir" ] || fail "prebuilt directory not found: $prebuilt_dir"
    prebuilt_dir=$(CDPATH='' cd -P "$prebuilt_dir" && pwd -P)
    [ -r "$prebuilt_dir/manifest.txt" ] || fail 'prebuilt manifest not found'
    [ -f "$prebuilt_dir/bootstrap.test" ] || fail 'prebuilt bootstrap.test not found'

    verify_go_127_patch
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
    [ "$(manifest_value commit)" = "$(git rev-parse HEAD)" ] || \
        fail 'prebuilt manifest Commit does not match the current checkout'
    expected_binary_hash=$(manifest_value bootstrap_sha256)
    [ -n "$expected_binary_hash" ] || fail 'prebuilt manifest is missing bootstrap_sha256'
    verify_file_hash "$prebuilt_dir/bootstrap.test" "$expected_binary_hash"
    if [ "$mode" = full ]; then
        verify_manifest_value worktree_clean true
    fi

    run_filesystem=$(stat -f -c %T "$run_dir") || fail 'cannot identify temporary run filesystem'
    case $run_filesystem in
        9p|drvfs|v9fs) fail 'temporary directory must not use WSL DrvFS' ;;
    esac
    require_command chmod
    require_command cp
    cp "$prebuilt_dir/manifest.txt" "$prebuilt_dir/bootstrap.test" "$run_dir/"
    chmod 400 "$run_dir/manifest.txt"
    chmod 500 "$run_dir/bootstrap.test"
    verify_file_hash "$run_dir/bootstrap.test" "$expected_binary_hash"
    test_binary="$run_dir/bootstrap.test"
else
    require_command go
    export GOTOOLCHAIN=local
    "$repo_root/tools/check-go-version.sh"
fi

case $mode in
    smoke)
        test_pattern='^TestM7GracefulShutdownChaos$/tcp_half-close_survives_Server_graceful_drain$'
        test_timeout=2m
        ;;
    full)
        test_pattern='^TestM7GracefulShutdownChaos$'
        test_timeout=5m
        ;;
esac

printf 'M7-03 mode: %s\n' "$mode"
printf 'M7-03 commit: %s\n' "$(git rev-parse HEAD)"
printf 'M7-03 platform: %s\n' "$(uname -a)"
printf 'M7-03 test pattern: %s\n' "$test_pattern"

if [ -n "$test_binary" ]; then
    if ! "$test_binary" -test.run "$test_pattern" -test.count=1 \
        -test.timeout="$test_timeout" -test.v >"$run_dir/output.txt" 2>&1; then
        cat "$run_dir/output.txt" >&2
        fail 'Graceful Shutdown Chaos failed'
    fi
else
    if ! go test ./internal/server/bootstrap -run "$test_pattern" -count=1 \
        -timeout="$test_timeout" -v >"$run_dir/output.txt" 2>&1; then
        cat "$run_dir/output.txt" >&2
        fail 'Graceful Shutdown Chaos failed'
    fi
fi
cat "$run_dir/output.txt"
printf '%s\n' 'M7-03 chaos run completed.'
