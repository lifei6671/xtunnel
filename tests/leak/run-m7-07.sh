#!/bin/sh

# 运行 M7-07 产品级资源泄漏 Harness。正式 full 在原生 Linux/Go 环境执行；
# 预编译 Binary 只用于无 Go 的 Linux/WSL2 开发 smoke，不构成正式验收。

set -eu

usage() {
    printf '%s\n' 'Usage: run-m7-07.sh [-m smoke|full] [-o output-directory] [-b prebuilt-directory]'
    printf '%s\n' '  -m  smoke runs short TCP churn; full runs churn, reconnect, drain and targeted Race.'
    printf '%s\n' '  -o  New or empty result directory outside the repository.'
    printf '%s\n' '  -b  Directory containing bootstrap.test and manifest.txt; smoke only.'
    printf '%s\n' '  -h  Show this help.'
}

fail() {
    printf 'm7-07 leak: %s\n' "$1" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_empty_directory() {
    inspected_directory=$1
    entries=$(find "$inspected_directory/." ! -name . -prune -print) || \
        fail "cannot inspect result directory: $inspected_directory"
    [ -z "$entries" ] || fail "result directory must be empty: $inspected_directory"
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

run_and_show() {
    output_file=$1
    shift
    set +e
    "$@" >"$output_file" 2>&1
    run_status=$?
    set -e
    if [ "$run_status" -ne 0 ]; then
        sed -n '1,320p' "$output_file" >&2
        return "$run_status"
    fi
    sed -n '1,320p' "$output_file"
}

write_commands() {
    commands_file=$1
    {
        if [ "$mode" = full ]; then
            printf '%s\n' 'timeout -k 15s 10m npm --prefix web ci'
            printf '%s\n' 'timeout -k 15s 5m npm --prefix web run check'
            printf '%s\n' 'timeout -k 15s 5m npm --prefix web run build'
        fi
        if [ -n "$test_binary" ]; then
            printf 'XTUNNEL_M7_07_EPOCHS=%s XTUNNEL_M7_07_CONNECTIONS=%s timeout -k 15s %s bootstrap.test -test.run %s -test.count=1 -test.timeout=%s -test.v\n' \
                "$epochs" "$connections" "$process_timeout" "$test_pattern" "$test_timeout"
        else
            printf 'XTUNNEL_M7_07_EPOCHS=%s XTUNNEL_M7_07_CONNECTIONS=%s timeout -k 15s %s go test ./internal/server/bootstrap -run %s -count=1 -timeout=%s -v\n' \
                "$epochs" "$connections" "$process_timeout" "$test_pattern" "$test_timeout"
        fi
        if [ "$mode" = full ]; then
            printf '%s\n' "XTUNNEL_M7_07_EPOCHS=2 XTUNNEL_M7_07_CONNECTIONS=20 timeout -k 15s 9m go test -race ./internal/server/bootstrap -run '^TestM7ResourceLeak$' -count=1 -timeout=8m -v"
        fi
    } >"$commands_file"
}

capture_identity() {
    identity_file=$1
    {
        printf 'commit=%s\n' "$(git rev-parse HEAD)"
        printf 'tree=%s\n' "$(git rev-parse 'HEAD^{tree}')"
        printf '%s\n' 'worktree_status_begin'
        git status --porcelain --untracked-files=all
        printf '%s\n' 'worktree_status_end'
    } >"$identity_file"
}

mode=smoke
result_dir=
prebuilt_dir=

while [ "$#" -gt 0 ]; do
    case $1 in
        -m)
            [ "$#" -ge 2 ] || fail '-m requires a value'
            mode=$2
            shift 2
            ;;
        -o)
            [ "$#" -ge 2 ] || fail '-o requires a value'
            result_dir=$2
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
[ "$mode" = smoke ] || [ -z "$prebuilt_dir" ] || fail 'prebuilt binaries are smoke-only'

require_command date
require_command find
require_command git
require_command mktemp
require_command mkdir
require_command cp
require_command chmod
require_command rm
require_command rmdir
require_command sed
require_command sha256sum
require_command timeout
require_command uname
[ "$(uname -s)" = Linux ] || fail 'runner requires Linux'

script_dir=$(CDPATH='' cd -P "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH='' cd -P "$script_dir/../.." && pwd -P)
[ -f "$repo_root/go.mod" ] || fail 'repository root does not contain go.mod'
cd "$repo_root"
git_root=$(git rev-parse --show-toplevel) || fail 'cannot resolve Git repository root'
git_root=$(CDPATH='' cd -P "$git_root" && pwd -P)
[ "$git_root" = "$repo_root" ] || fail 'script path does not resolve to the Git repository root'

if [ "$mode" = full ] && [ -n "$(git status --porcelain --untracked-files=all)" ]; then
    fail 'full mode requires a clean worktree'
fi

umask 077
if [ -z "$result_dir" ]; then
    result_dir=$(mktemp -d "${TMPDIR:-/tmp}/xtunnel-m7-07.XXXXXX") || \
        fail 'cannot create result directory'
else
    if [ -d "$result_dir" ]; then
        require_empty_directory "$result_dir"
    else
        mkdir "$result_dir" || fail "cannot create result directory: $result_dir"
    fi
    result_dir=$(CDPATH='' cd -P "$result_dir" && pwd -P)
fi
case "$result_dir/" in
    "$repo_root/"*) fail 'result directory must be outside the repository' ;;
esac

test_binary=
if [ -n "$prebuilt_dir" ]; then
    prebuilt_dir=$(CDPATH='' cd -P "$prebuilt_dir" && pwd -P)
    [ -f "$prebuilt_dir/bootstrap.test" ] || fail 'prebuilt bootstrap.test not found'
    [ -r "$prebuilt_dir/manifest.txt" ] || fail 'prebuilt manifest.txt not found'
    verify_manifest_value go_version go1.27.0
    verify_manifest_value toolchain local
    verify_manifest_value goos linux
    verify_manifest_value goarch amd64
    verify_manifest_value goamd64 v1
    verify_manifest_value cgo_enabled 0
    [ "$(manifest_value commit)" = "$(git rev-parse HEAD)" ] || \
        fail 'prebuilt manifest Commit does not match the current checkout'
    [ "$(manifest_value tree)" = "$(git rev-parse 'HEAD^{tree}')" ] || \
        fail 'prebuilt manifest Tree does not match the current checkout'
    expected_hash=$(manifest_value bootstrap_sha256)
    actual_hash=$(sha256sum "$prebuilt_dir/bootstrap.test")
    actual_hash=${actual_hash%% *}
    [ "$actual_hash" = "$expected_hash" ] || fail 'prebuilt bootstrap.test SHA-256 mismatch'
    cp "$prebuilt_dir/bootstrap.test" "$result_dir/bootstrap.test"
    chmod 500 "$result_dir/bootstrap.test"
    test_binary="$result_dir/bootstrap.test"
else
    require_command go
    export GOTOOLCHAIN=local
    "$repo_root/tools/check-go-version.sh"
    if [ "$mode" = full ]; then
        require_command node
        require_command npm
    fi
fi

case $mode in
    smoke)
        epochs=2
        connections=20
        test_pattern='^TestM7ResourceLeak$/^connection_churn_and_cancel_drain$'
        test_timeout=3m
        process_timeout=4m
        ;;
    full)
        epochs=4
        connections=100
        test_pattern='^TestM7ResourceLeak$'
        test_timeout=12m
        process_timeout=13m
        ;;
esac

before_status=$(git status --porcelain --untracked-files=all)
before_commit=$(git rev-parse HEAD)
before_tree=$(git rev-parse 'HEAD^{tree}')
capture_identity "$result_dir/identity-before.txt"
write_commands "$result_dir/commands.txt"
{
    printf 'captured_at_utc=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    printf 'mode=%s\ncommit=%s\n' "$mode" "$before_commit"
    printf 'epochs=%s\nconnections_per_epoch=%s\n' "$epochs" "$connections"
    printf 'test_pattern=%s\n' "$test_pattern"
    printf 'platform=%s\n' "$(uname -a)"
    printf 'shell=%s\n' "$(command -v sh)"
    timeout --version | sed -n '1p'
    sha256sum --version | sed -n '1p'
    if [ -r "/proc/$$/limits" ]; then
        sed -n '/^Max open files[[:space:]]/p' "/proc/$$/limits"
    fi
    if [ -n "$test_binary" ]; then
        printf 'execution=prebuilt-smoke\n'
        sed -n '1,80p' "$prebuilt_dir/manifest.txt"
    else
        printf 'execution=local-go\n'
        go env GOVERSION GOTOOLCHAIN GOOS GOARCH GOAMD64 CGO_ENABLED
        if [ "$mode" = full ]; then
            printf 'node_version=%s\n' "$(node --version)"
            printf 'npm_version=%s\n' "$(npm --version)"
        fi
    fi
} >"$result_dir/environment.txt"

export XTUNNEL_M7_07_EPOCHS=$epochs
export XTUNNEL_M7_07_CONNECTIONS=$connections
printf 'M7-07 mode: %s\nResults: %s\n' "$mode" "$result_dir"

if [ "$mode" = full ]; then
    # clean checkout 不包含被忽略的 web/dist；Bootstrap package 使用 go:embed，
    # 因此按仓库固定顺序先恢复依赖、检查并构建 Web，再执行任何 Go Test。
    run_and_show "$result_dir/web-npm-ci.txt" timeout -k 15s 10m npm --prefix web ci
    run_and_show "$result_dir/web-check.txt" timeout -k 15s 5m npm --prefix web run check
    run_and_show "$result_dir/web-build.txt" timeout -k 15s 5m npm --prefix web run build
fi

if [ -n "$test_binary" ]; then
    run_and_show "$result_dir/leak-test.txt" timeout -k 15s "$process_timeout" \
        "$test_binary" -test.run "$test_pattern" -test.count=1 -test.timeout="$test_timeout" -test.v
else
    run_and_show "$result_dir/leak-test.txt" timeout -k 15s "$process_timeout" \
        go test ./internal/server/bootstrap -run "$test_pattern" -count=1 -timeout="$test_timeout" -v
fi

if [ "$mode" = full ]; then
    XTUNNEL_M7_07_EPOCHS=2 XTUNNEL_M7_07_CONNECTIONS=20 \
        run_and_show "$result_dir/leak-race.txt" timeout -k 15s 9m \
        go test -race ./internal/server/bootstrap \
        -run '^TestM7ResourceLeak$' -count=1 -timeout=8m -v
fi

after_commit=$(git rev-parse HEAD)
after_tree=$(git rev-parse 'HEAD^{tree}')
after_status=$(git status --porcelain --untracked-files=all)
[ "$after_commit" = "$before_commit" ] || fail 'HEAD changed while the runner was active'
[ "$after_tree" = "$before_tree" ] || fail 'Tree changed while the runner was active'
[ "$after_status" = "$before_status" ] || fail 'worktree changed while the runner was active'
capture_identity "$result_dir/identity-after.txt"

(
    cd "$result_dir"
    sha256sum environment.txt commands.txt identity-before.txt identity-after.txt leak-test.txt \
        >artifact-sha256.txt
    if [ -f web-npm-ci.txt ]; then
        sha256sum web-npm-ci.txt web-check.txt web-build.txt >>artifact-sha256.txt
    fi
    if [ -f leak-race.txt ]; then
        sha256sum leak-race.txt >>artifact-sha256.txt
    fi
    if [ -f bootstrap.test ]; then
        sha256sum bootstrap.test >>artifact-sha256.txt
    fi
    # 清单只写 Runner 固定的相对路径，允许 Artifact 搬移后读回。
    sha256sum -c artifact-sha256.txt
)
verify_dir=$(mktemp -d "${TMPDIR:-/tmp}/xtunnel-m7-07-verify.XXXXXX") || \
    fail 'cannot create artifact verification directory'
# 从这里开始，任一复制、校验或信号失败都必须清理精确的临时目录，
# 同时保留原始退出码，避免清理结果掩盖真正的验收失败。
cleanup_verify_dir() {
    cleanup_status=$?
    trap - 0 HUP INT TERM
    if [ -n "$verify_dir" ]; then
        if ! rm -f "$verify_dir"/* && [ "$cleanup_status" -eq 0 ]; then
            cleanup_status=1
        fi
        if ! rmdir "$verify_dir" && [ "$cleanup_status" -eq 0 ]; then
            cleanup_status=1
        fi
        verify_dir=
    fi
    exit "$cleanup_status"
}
trap 'cleanup_verify_dir' 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
cp "$result_dir"/* "$verify_dir/"
(
    cd "$verify_dir"
    sha256sum -c artifact-sha256.txt
)
rm -f "$verify_dir"/*
rmdir "$verify_dir"
verify_dir=
trap - 0 HUP INT TERM
sha256sum "$result_dir/artifact-sha256.txt"
printf '%s\n' 'M7-07 leak run completed.'
