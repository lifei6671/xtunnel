#!/bin/sh

# 运行 M7-05 的 Linux Race Suite 和 Connector Selection 并发 Profile。
# 主 Benchmark 与 CPU/Mutex/Block Profile 始终使用独立进程，避免分析器污染主结果。

set -eu

usage() {
    printf '%s\n' 'Usage: run-m7-05.sh [-m smoke|full] [-o output-directory]'
    printf '%s\n' '  -m  Run mode. Default: smoke.'
    printf '%s\n' '  -o  New or empty result directory outside the repository. Default: a new /tmp directory.'
    printf '%s\n' '  -h  Show this help.'
}

fail() {
    printf 'm7-05 concurrency: %s\n' "$1" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_empty_directory() {
    inspected_directory=$1
    directory_entries=$(find "$inspected_directory/." ! -name . -prune -print) || \
        fail "cannot inspect result directory: $inspected_directory"
    [ -z "$directory_entries" ] || \
        fail "result directory must be empty: $inspected_directory"
}

run_and_show() {
    run_output_file=$1
    shift

    if ! "$@" >"$run_output_file" 2>&1; then
        sed -n '1,240p' "$run_output_file" >&2
        return 1
    fi
    sed -n '1,240p' "$run_output_file"
}

capture_environment() {
    environment_file=$1
    {
        printf 'captured_at_utc=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
        printf 'commit=%s\n' "$(git rev-parse HEAD)"
        printf 'worktree_status_begin\n'
        git status --porcelain --untracked-files=all
        printf 'worktree_status_end\n'
        printf 'gomaxprocs_environment=%s\n' "${GOMAXPROCS-unset}"
        go env GOVERSION GOTOOLCHAIN GOOS GOARCH GOAMD64 CGO_ENABLED CC
        if command -v node >/dev/null 2>&1; then
            printf 'node_version=%s\n' "$(node --version)"
        else
            printf 'node_version=unavailable\n'
        fi
        if command -v npm >/dev/null 2>&1; then
            printf 'npm_version=%s\n' "$(npm --version)"
        else
            printf 'npm_version=unavailable\n'
        fi
        uname -a
        if [ -r "/proc/$$/limits" ]; then
            sed -n '/^Max open files[[:space:]]/p' "/proc/$$/limits"
        else
            printf 'fd_limit=unavailable\n'
        fi
        if command -v nproc >/dev/null 2>&1; then
            printf 'logical_cpu_count=%s\n' "$(nproc)"
        else
            printf 'logical_cpu_count=unavailable\n'
        fi
        if command -v lscpu >/dev/null 2>&1; then
            lscpu
        elif [ -r /proc/cpuinfo ]; then
            sed -n '1,80p' /proc/cpuinfo
        fi
        if command -v free >/dev/null 2>&1; then
            free -b
        elif [ -r /proc/meminfo ]; then
            sed -n '1,20p' /proc/meminfo
        fi
    } >"$environment_file"
}

write_commands() {
    commands_file=$1
    if [ "$mode" = smoke ]; then
        cat >"$commands_file" <<'EOF'
go test -race -count=1 -timeout=5m ./internal/server/runtime ./internal/server/sessionruntime ./internal/agent/configruntime ./internal/server/usage ./internal/server/tcpingress ./internal/server/gateway ./internal/tunnel ./tests/integration
go test ./internal/tunnel -run '^$' -bench '^BenchmarkConnectorSelection(Concurrent)?/' -benchmem -count=1 -benchtime=1x -cpu=1,2 -timeout=2m
EOF
        return
    fi

    cat >"$commands_file" <<'EOF'
npm --prefix web ci
npm --prefix web run check
npm --prefix web run build
go test -race -count=1 -timeout=10m ./...
go test ./internal/tunnel -run '^$' -bench '^BenchmarkConnectorSelectionConcurrent/' -benchmem -count=5 -benchtime=2s -cpu=1,8,32 -timeout=10m
go test ./internal/tunnel -run '^$' -bench '^BenchmarkConnectorSelectionConcurrent/connectors_100$' -count=1 -benchtime=10s -cpu=32 -o <result>/connector-selection-cpu.test -cpuprofile <result>/connector-selection-cpu.pprof -timeout=5m
go tool pprof -top <result>/connector-selection-cpu.pprof
go test ./internal/tunnel -run '^$' -bench '^BenchmarkConnectorSelectionConcurrent/connectors_100$' -count=1 -benchtime=10s -cpu=32 -o <result>/connector-selection-mutex.test -mutexprofile <result>/connector-selection-mutex.pprof -mutexprofilefraction=1 -timeout=5m
go tool pprof -top <result>/connector-selection-mutex.pprof
go tool pprof -top -focus 'selectConnector|acquireConnectorWhere|releaseConnector|Pools' <result>/connector-selection-mutex.pprof
go test ./internal/tunnel -run '^$' -bench '^BenchmarkConnectorSelectionConcurrent/connectors_100$' -count=1 -benchtime=10s -cpu=32 -o <result>/connector-selection-block.test -blockprofile <result>/connector-selection-block.pprof -blockprofilerate=1 -timeout=5m
go tool pprof -top <result>/connector-selection-block.pprof
go tool pprof -top -focus 'selectConnector|acquireConnectorWhere|releaseConnector|Pools' <result>/connector-selection-block.pprof
EOF
}

mode=smoke
result_dir=

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

require_command date
require_command find
require_command git
require_command go
require_command mktemp
require_command mkdir
require_command sed

script_dir=$(CDPATH='' cd -P "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH='' cd -P "$script_dir/../.." && pwd -P)
[ -f "$repo_root/go.mod" ] || fail 'repository root does not contain go.mod'
[ -x "$repo_root/tools/check-go-version.sh" ] || \
    fail 'repository root does not contain an executable Go version check'
cd "$repo_root"

git_root=$(git rev-parse --show-toplevel) || fail 'cannot resolve Git repository root'
[ "$git_root" = "$repo_root" ] || fail 'script path does not resolve to the Git repository root'

export GOTOOLCHAIN=local
export GOOS=linux
export GOARCH=amd64
export GOAMD64=v1
export CGO_ENABLED=1
"$repo_root/tools/check-go-version.sh"

[ "$(uname -s)" = Linux ] || fail 'runner requires Linux'
case $(uname -m) in
    x86_64|amd64) ;;
    *) fail 'runner requires native Linux amd64' ;;
esac
[ "$(go env GOTOOLCHAIN)" = local ] || fail 'GOTOOLCHAIN must equal local'
[ "$(go env GOOS)" = linux ] || fail 'GOOS must equal linux'
[ "$(go env GOARCH)" = amd64 ] || fail 'GOARCH must equal amd64'
[ "$(go env GOAMD64)" = v1 ] || fail 'GOAMD64 must equal v1'
[ "$(go env CGO_ENABLED)" = 1 ] || fail 'CGO_ENABLED must equal 1'

if [ "$mode" = full ] && [ -n "$(git status --porcelain --untracked-files=all)" ]; then
    fail 'full mode requires a clean worktree'
fi

umask 077
if [ -z "$result_dir" ]; then
    result_dir=$(mktemp -d "${TMPDIR:-/tmp}/xtunnel-m7-05.XXXXXX") || \
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

# 目录检查后启用 noclobber，阻止并发写入者在检查与产物创建之间抢占同名文件。
set -C
capture_environment "$result_dir/environment.txt"
write_commands "$result_dir/commands.txt"

printf 'M7-05 mode: %s\n' "$mode"
printf 'Results: %s\n' "$result_dir"

if [ "$mode" = smoke ]; then
    run_and_show "$result_dir/targeted-race.txt" \
        go test -race -count=1 -timeout=5m \
        ./internal/server/runtime \
        ./internal/server/sessionruntime \
        ./internal/agent/configruntime \
        ./internal/server/usage \
        ./internal/server/tcpingress \
        ./internal/server/gateway \
        ./internal/tunnel \
        ./tests/integration
    run_and_show "$result_dir/connector-selection-smoke.txt" \
        go test ./internal/tunnel -run '^$' \
        -bench '^BenchmarkConnectorSelection(Concurrent)?/' \
        -benchmem -count=1 -benchtime=1x -cpu=1,2 -timeout=2m
else
    require_command node
    require_command npm
    run_and_show "$result_dir/web-npm-ci.txt" npm --prefix web ci
    run_and_show "$result_dir/web-check.txt" npm --prefix web run check
    run_and_show "$result_dir/web-build.txt" npm --prefix web run build

    run_and_show "$result_dir/full-race.txt" \
        go test -race -count=1 -timeout=10m ./...
    run_and_show "$result_dir/connector-selection-benchmark.txt" \
        go test ./internal/tunnel -run '^$' \
        -bench '^BenchmarkConnectorSelectionConcurrent/' \
        -benchmem -count=5 -benchtime=2s -cpu=1,8,32 -timeout=10m

    cpu_profile="$result_dir/connector-selection-cpu.pprof"
    cpu_binary="$result_dir/connector-selection-cpu.test"
    mutex_profile="$result_dir/connector-selection-mutex.pprof"
    mutex_binary="$result_dir/connector-selection-mutex.test"
    block_profile="$result_dir/connector-selection-block.pprof"
    block_binary="$result_dir/connector-selection-block.test"

    run_and_show "$result_dir/connector-selection-cpu-run.txt" \
        go test ./internal/tunnel -run '^$' \
        -bench '^BenchmarkConnectorSelectionConcurrent/connectors_100$' \
        -count=1 -benchtime=10s -cpu=32 -o "$cpu_binary" \
        -cpuprofile "$cpu_profile" -timeout=5m
    run_and_show "$result_dir/connector-selection-cpu-top.txt" \
        go tool pprof -top "$cpu_profile"

    run_and_show "$result_dir/connector-selection-mutex-run.txt" \
        go test ./internal/tunnel -run '^$' \
        -bench '^BenchmarkConnectorSelectionConcurrent/connectors_100$' \
        -count=1 -benchtime=10s -cpu=32 \
        -o "$mutex_binary" \
        -mutexprofile "$mutex_profile" -mutexprofilefraction=1 -timeout=5m
    run_and_show "$result_dir/connector-selection-mutex-top.txt" \
        go tool pprof -top "$mutex_profile"
    run_and_show "$result_dir/connector-selection-mutex-focus-top.txt" \
        go tool pprof -top \
        -focus 'selectConnector|acquireConnectorWhere|releaseConnector|Pools' \
        "$mutex_profile"

    run_and_show "$result_dir/connector-selection-block-run.txt" \
        go test ./internal/tunnel -run '^$' \
        -bench '^BenchmarkConnectorSelectionConcurrent/connectors_100$' \
        -count=1 -benchtime=10s -cpu=32 \
        -o "$block_binary" \
        -blockprofile "$block_profile" -blockprofilerate=1 -timeout=5m
    run_and_show "$result_dir/connector-selection-block-top.txt" \
        go tool pprof -top "$block_profile"
    run_and_show "$result_dir/connector-selection-block-focus-top.txt" \
        go tool pprof -top \
        -focus 'selectConnector|acquireConnectorWhere|releaseConnector|Pools' \
        "$block_profile"

    if [ -n "$(git status --porcelain --untracked-files=all)" ]; then
        fail 'worktree changed during full run'
    fi
fi

printf 'M7-05 concurrency run completed. Results: %s\n' "$result_dir"
