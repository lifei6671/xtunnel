#!/bin/sh

# 运行 M7-04 Server Persistence/Filesystem Failpoints。Runner 只调度确定性的
# syscall-boundary 注入、SQLite 原生 SQLITE_FULL、Backup hard-exit 与目录切换后
# 真实子进程 SIGKILL；它不把这些用例描述成真实 Kernel EIO、物理断电或存储设备耐久性证明。

set -eu

usage() {
    printf '%s\n' 'Usage: run-m7-04.sh [-m smoke|full] [-b prebuilt-directory]'
    printf '%s\n' '  -m  smoke runs the three failpoint partitions; full adds crash/recovery cases.'
    printf '%s\n' '  -b  Directory containing sqlite.test, gateway.test, durableops.test and manifest.txt.'
    printf '%s\n' '  -h  Show this help.'
}

fail() {
    printf 'm7-04 chaos: %s\n' "$1" >&2
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
        if ! rm -f \
            "$run_dir/sqlite-output.txt" \
            "$run_dir/gateway-output.txt" \
            "$run_dir/durableops-output.txt" \
            "$run_dir/manifest.txt" \
            "$run_dir/sqlite.test" \
            "$run_dir/gateway.test" \
            "$run_dir/durableops.test"; then
            printf '%s\n' 'm7-04 chaos: failed to remove temporary files' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        if ! rmdir "$run_dir"; then
            printf '%s\n' 'm7-04 chaos: failed to remove temporary directory' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        run_dir=
    fi
    exit "$cleanup_status"
}

run_partition() {
    partition_name=$1
    package_path=$2
    binary_name=$3
    test_pattern=$4
    output_path=$5

    printf 'M7-04 partition: %s\n' "$partition_name"
    printf 'M7-04 test pattern: %s\n' "$test_pattern"
    if [ -n "$prebuilt_dir" ]; then
        if ! "$run_dir/$binary_name" -test.run "$test_pattern" -test.count=1 \
            -test.timeout="$test_timeout" -test.v >"$output_path" 2>&1; then
            cat "$output_path" >&2
            fail "$partition_name partition failed"
        fi
    else
        if ! go test "$package_path" -run "$test_pattern" -count=1 \
            -timeout="$test_timeout" -v >"$output_path" 2>&1; then
            cat "$output_path" >&2
            fail "$partition_name partition failed"
        fi
    fi
    cat "$output_path"
}

mode=smoke
prebuilt_dir=
run_dir=
test_timeout=

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
[ "$(uname -s)" = Linux ] || fail 'M7-04 chaos runner requires Linux'
require_command cat
require_command dirname
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
run_dir=$(mktemp -d /tmp/xtunnel-m7-04.XXXXXX) || fail 'cannot create temporary run directory'
trap 'cleanup' 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

run_filesystem=$(stat -f -c %T "$run_dir") || fail 'cannot identify temporary run filesystem'
case $run_filesystem in
    9p|drvfs|v9fs) fail 'temporary directory must not use WSL DrvFS' ;;
esac

if [ -n "$prebuilt_dir" ]; then
    [ -d "$prebuilt_dir" ] || fail "prebuilt directory not found: $prebuilt_dir"
    prebuilt_dir=$(CDPATH='' cd -P "$prebuilt_dir" && pwd -P)
    [ -r "$prebuilt_dir/manifest.txt" ] || fail 'prebuilt manifest not found'
    for binary_name in sqlite.test gateway.test durableops.test; do
        [ -f "$prebuilt_dir/$binary_name" ] || fail "prebuilt $binary_name not found"
    done

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
    [ "$(manifest_value commit)" = "$(git rev-parse HEAD)" ] || \
        fail 'prebuilt manifest Commit does not match the current checkout'
    if [ "$mode" = full ]; then
        verify_manifest_value worktree_clean true
    fi

    require_command chmod
    require_command cp
    cp "$prebuilt_dir/manifest.txt" \
        "$prebuilt_dir/sqlite.test" \
        "$prebuilt_dir/gateway.test" \
        "$prebuilt_dir/durableops.test" \
        "$run_dir/"
    chmod 400 "$run_dir/manifest.txt"
    chmod 500 "$run_dir/sqlite.test" "$run_dir/gateway.test" "$run_dir/durableops.test"
    for binary_name in sqlite gateway durableops; do
        expected_hash=$(manifest_value "${binary_name}_sha256")
        [ -n "$expected_hash" ] || fail "prebuilt manifest is missing ${binary_name}_sha256"
        verify_file_hash "$prebuilt_dir/$binary_name.test" "$expected_hash"
        verify_file_hash "$run_dir/$binary_name.test" "$expected_hash"
    done
else
    require_command go
    export GOTOOLCHAIN=local
    "$repo_root/tools/check-go-version.sh"
fi

case $mode in
    smoke)
        test_timeout=2m
        gateway_pattern='^TestRotatePinnedIdentityFilesystemFailuresConvergeSafely$'
        durableops_pattern='^(TestCreateFilesystemFailpointsPreservePublicationBoundary|TestWriteJournalFilesystemFailpointsPreserveRecoverablePhase)$'
        ;;
    full)
        test_timeout=5m
        gateway_pattern='^(TestRotatePinnedIdentityFilesystemFailuresConvergeSafely|TestRotatePinnedIdentityRecoversAfterSIGKILL)$'
        durableops_pattern='^(TestCreateFilesystemFailpointsPreservePublicationBoundary|TestWriteJournalFilesystemFailpointsPreserveRecoverablePhase|TestCreateHardExitBeforePublishDoesNotExposeFinalPath|TestRecoverPendingRestoreConvergesInterruptedRenameStates|TestRestoreDirectorySwitchSIGKILLRecovers)$'
        ;;
esac

printf 'M7-04 mode: %s\n' "$mode"
printf 'M7-04 commit: %s\n' "$(git rev-parse HEAD)"
printf 'M7-04 platform: %s\n' "$(uname -a)"
printf '%s\n' 'M7-04 evidence boundary: deterministic syscall-boundary/SQLite native FULL plus real child-process SIGKILL; not physical disk failure or power loss.'

run_partition \
    sqlite \
    ./internal/repository/sqlite \
    sqlite.test \
    '^TestRunMigrationsRollsBackSQLiteFullAndCanRetry$' \
    "$run_dir/sqlite-output.txt"
run_partition \
    gateway \
    ./internal/server/gateway \
    gateway.test \
    "$gateway_pattern" \
    "$run_dir/gateway-output.txt"
run_partition \
    durableops \
    ./internal/server/durableops \
    durableops.test \
    "$durableops_pattern" \
    "$run_dir/durableops-output.txt"

printf '%s\n' 'M7-04 chaos run completed.'
