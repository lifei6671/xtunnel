#!/bin/sh

# 运行 M7-01 的三组基准，并把环境、原始结果和分析器输出保存到独立目录。
# smoke 模式只验证基准可执行；full 模式要求干净工作区和 Linux 分析工具。

set -eu

usage() {
    printf '%s\n' 'Usage: run-m7-01.sh [-m smoke|full] [-o output-directory] [-b binary-directory]'
    printf '%s\n' '  -m  Run mode. Default: smoke.'
    printf '%s\n' '  -o  Result directory. Default: a new temporary directory.'
    printf '%s\n' '  -b  Directory containing prebuilt Linux benchmark binaries.'
    printf '%s\n' '  -h  Show this help.'
}

fail() {
    printf 'm7-01 benchmark: %s\n' "$1" >&2
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

verify_prebuilt_binary() {
    verify_name=$1
    verify_key=$2
    expected_hash=$(manifest_value "$verify_key")
    [ -n "$expected_hash" ] || fail "prebuilt manifest is missing $verify_key"
    actual_hash=$(sha256sum "$prebuilt_dir/$verify_name")
    actual_hash=${actual_hash%% *}
    [ "$actual_hash" = "$expected_hash" ] || fail "prebuilt binary hash mismatch: $verify_name"
}

cleanup_prebuilt_stage() {
    cleanup_status=$?
    trap - 0
    trap '' HUP INT TERM

    if [ -n "$prebuilt_run_dir" ]; then
        if ! rm -f "$prebuilt_run_dir/manifest.txt" \
            "$prebuilt_run_dir/proxy.test" \
            "$prebuilt_run_dir/tunnel.test" \
            "$prebuilt_run_dir/httpingress.test"; then
            printf '%s\n' 'm7-01 benchmark: failed to remove staged prebuilt files' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        if ! rmdir "$prebuilt_run_dir"; then
            printf '%s\n' 'm7-01 benchmark: failed to remove staged prebuilt directory' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        prebuilt_run_dir=
    fi

    exit "$cleanup_status"
}

verify_staged_binary() {
    staged_name=$1
    staged_key=$2
    staged_expected_hash=$(sed -n "s/^${staged_key}=//p" "$prebuilt_run_dir/manifest.txt")
    [ -n "$staged_expected_hash" ] || fail "staged prebuilt manifest is missing $staged_key"
    staged_actual_hash=$(sha256sum "$prebuilt_run_dir/$staged_name")
    staged_actual_hash=${staged_actual_hash%% *}
    [ "$staged_actual_hash" = "$staged_expected_hash" ] || \
        fail "staged prebuilt binary hash mismatch: $staged_name"
}

stage_prebuilt_full() {
    expected_manifest_hash=$1
    prebuilt_run_dir=$(mktemp -d /tmp/xtunnel-m7-01-prebuilt.XXXXXX) || \
        fail 'cannot create Linux-native prebuilt staging directory'
    trap 'cleanup_prebuilt_stage' 0
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM

    staged_filesystem=$(stat -f -c %T "$prebuilt_run_dir") || \
        fail 'cannot identify prebuilt staging filesystem'
    case $staged_filesystem in
        9p|drvfs|v9fs) fail 'prebuilt staging directory must not use WSL DrvFS' ;;
    esac

    cp "$prebuilt_dir/manifest.txt" \
        "$prebuilt_dir/proxy.test" \
        "$prebuilt_dir/tunnel.test" \
        "$prebuilt_dir/httpingress.test" \
        "$prebuilt_run_dir/"
    chmod 400 "$prebuilt_run_dir/manifest.txt"
    chmod 500 "$prebuilt_run_dir/proxy.test" \
        "$prebuilt_run_dir/tunnel.test" \
        "$prebuilt_run_dir/httpingress.test"

    staged_manifest_hash=$(sha256sum "$prebuilt_run_dir/manifest.txt")
    staged_manifest_hash=${staged_manifest_hash%% *}
    [ "$staged_manifest_hash" = "$expected_manifest_hash" ] || \
        fail 'staged prebuilt manifest hash mismatch'
    verify_staged_binary proxy.test proxy_sha256
    verify_staged_binary tunnel.test tunnel_sha256
    verify_staged_binary httpingress.test httpingress_sha256
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
        printf 'gomaxprocs=%s\n' "${GOMAXPROCS-unset}"
        if [ -n "$prebuilt_dir" ]; then
            printf 'prebuilt_manifest_begin\n'
            sed -n '1,120p' "$prebuilt_dir/manifest.txt"
            printf 'prebuilt_manifest_end\n'
        else
            go env GOVERSION GOTOOLCHAIN GOOS GOARCH GOAMD64
        fi
        uname -a
        if [ -r "/proc/$$/limits" ]; then
            sed -n '/^Max open files[[:space:]]/p' "/proc/$$/limits"
        else
            printf 'fd_limit=unavailable\n'
        fi
        if command -v lscpu >/dev/null 2>&1; then
            lscpu
        fi
        if command -v free >/dev/null 2>&1; then
            free -b
        elif [ -r /proc/meminfo ]; then
            sed -n '1,20p' /proc/meminfo
        fi
        if [ -r /proc/sys/fs/file-max ]; then
            printf 'kernel_file_max='
            cat /proc/sys/fs/file-max
        fi
    } >"$environment_file"
}

run_smoke_case() {
    smoke_name=$1
    smoke_package=$2
    smoke_pattern=$3
    smoke_binary=$4
    smoke_file="$result_dir/$smoke_name-smoke.txt"

    if [ -n "$prebuilt_dir" ]; then
        run_and_show "$smoke_file" "$prebuilt_dir/$smoke_binary" \
            -test.run '^$' -test.bench "$smoke_pattern" -test.benchmem \
            -test.count=1 -test.benchtime=1x -test.timeout=1m
    else
        run_and_show "$smoke_file" go test "$smoke_package" -run '^$' \
            -bench "$smoke_pattern" -benchmem -count=1 -benchtime=1x -timeout=1m
    fi
}

run_full_case() {
    full_name=$1
    full_package=$2
    full_pattern=$3
    full_binary=$4
    full_analysis=${5-yes}
    full_result="$result_dir/$full_name-benchmark.txt"
    full_time="$result_dir/$full_name-time.txt"
    full_syscalls="$result_dir/$full_name-syscalls.txt"
    full_profile="$result_dir/$full_name-profile.txt"
    full_cpu_profile="$result_dir/$full_name-cpu.pprof"
    full_heap_profile="$result_dir/$full_name-heap.pprof"

    if [ -n "$prebuilt_dir" ]; then
        run_and_show "$full_result" /usr/bin/time -v -o "$full_time" \
            "$prebuilt_run_dir/$full_binary" -test.run '^$' -test.bench "$full_pattern" \
            -test.benchmem -test.count=5 -test.benchtime=2s -test.timeout=5m
    else
        run_and_show "$full_result" /usr/bin/time -v -o "$full_time" \
            go test "$full_package" -run '^$' -bench "$full_pattern" \
            -benchmem -count=5 -benchtime=2s -timeout=5m
    fi

    [ "$full_analysis" = yes ] || return 0

    if [ -n "$prebuilt_dir" ]; then
        strace -f -c -o "$full_syscalls" \
            "$prebuilt_run_dir/$full_binary" -test.run '^$' -test.bench "$full_pattern" \
            -test.count=1 -test.benchtime=1x -test.timeout=5m \
            >"$result_dir/$full_name-strace-run.txt" 2>&1

        "$prebuilt_run_dir/$full_binary" -test.run '^$' -test.bench "$full_pattern" \
            -test.count=1 -test.benchtime=5s -test.cpuprofile "$full_cpu_profile" \
            -test.memprofile "$full_heap_profile" -test.timeout=5m \
            >"$result_dir/$full_name-profile-run.txt" 2>&1
    else
        strace -f -c -o "$full_syscalls" \
            go test "$full_package" -run '^$' -bench "$full_pattern" \
            -count=1 -benchtime=1x -timeout=5m \
            >"$result_dir/$full_name-strace-run.txt" 2>&1

        go test "$full_package" -run '^$' -bench "$full_pattern" \
            -count=1 -benchtime=5s -cpuprofile "$full_cpu_profile" \
            -memprofile "$full_heap_profile" -timeout=5m \
            >"$result_dir/$full_name-profile-run.txt" 2>&1
    fi

    if [ -z "$prebuilt_dir" ]; then
        {
            printf '%s\n' 'CPU profile top:'
            go tool pprof -top "$full_cpu_profile"
            printf '%s\n' 'Heap profile top:'
            go tool pprof -top -alloc_space "$full_heap_profile"
        } >"$full_profile"
    else
        printf '%s\n' 'Profiles were captured; analyze them with the same Go 1.27 toolchain.' \
            >"$full_profile"
    fi
}

run_proxy_analysis_case() {
    analysis_name=$1
    analysis_pattern=$2
    analysis_prefix="$result_dir/proxy-buffer-$analysis_name"
    analysis_result="$analysis_prefix-benchmark.txt"
    analysis_time="$analysis_prefix-time.txt"
    analysis_syscalls="$analysis_prefix-syscalls.txt"
    analysis_cpu_profile="$analysis_prefix-cpu.pprof"
    analysis_heap_profile="$analysis_prefix-heap.pprof"
    analysis_profile="$analysis_prefix-profile.txt"
    analysis_gc="$analysis_prefix-gc.txt"
    analysis_gc_summary="$analysis_prefix-gc-summary.txt"

    if [ -n "$prebuilt_dir" ]; then
        run_and_show "$analysis_result" /usr/bin/time -v -o "$analysis_time" \
            "$prebuilt_run_dir/proxy.test" -test.run '^$' -test.bench "$analysis_pattern" \
            -test.benchmem -test.count=5 -test.benchtime=2s -test.timeout=5m

        strace -f -c -o "$analysis_syscalls" \
            "$prebuilt_run_dir/proxy.test" -test.run '^$' -test.bench "$analysis_pattern" \
            -test.count=1 -test.benchtime=1x -test.timeout=5m \
            >"$analysis_prefix-strace-run.txt" 2>&1

        "$prebuilt_run_dir/proxy.test" -test.run '^$' -test.bench "$analysis_pattern" \
            -test.count=1 -test.benchtime=5s -test.cpuprofile "$analysis_cpu_profile" \
            -test.memprofile "$analysis_heap_profile" -test.timeout=5m \
            >"$analysis_prefix-profile-run.txt" 2>&1

        run_and_show "$analysis_gc" env GODEBUG=gctrace=1 \
            "$prebuilt_run_dir/proxy.test" -test.run '^$' -test.bench "$analysis_pattern" \
            -test.count=1 -test.benchtime=5s -test.timeout=5m
    else
        run_and_show "$analysis_result" /usr/bin/time -v -o "$analysis_time" \
            go test ./internal/proxy -run '^$' -bench "$analysis_pattern" \
            -benchmem -count=5 -benchtime=2s -timeout=5m

        strace -f -c -o "$analysis_syscalls" \
            go test ./internal/proxy -run '^$' -bench "$analysis_pattern" \
            -count=1 -benchtime=1x -timeout=5m \
            >"$analysis_prefix-strace-run.txt" 2>&1

        go test ./internal/proxy -run '^$' -bench "$analysis_pattern" \
            -count=1 -benchtime=5s -cpuprofile "$analysis_cpu_profile" \
            -memprofile "$analysis_heap_profile" -timeout=5m \
            >"$analysis_prefix-profile-run.txt" 2>&1

        run_and_show "$analysis_gc" go test ./internal/proxy -run '^$' \
            -bench "$analysis_pattern" -count=1 -benchtime=5s \
            -timeout=5m -exec 'env GODEBUG=gctrace=1'
    fi

    gc_trace_lines=$(grep -c 'gc [0-9][0-9]* @' "$analysis_gc" || true)
    printf 'gctrace_lines=%s\n' "$gc_trace_lines" >"$analysis_gc_summary"

    if [ -z "$prebuilt_dir" ]; then
        {
            printf '%s\n' 'CPU profile top:'
            go tool pprof -top "$analysis_cpu_profile"
            printf '%s\n' 'Heap profile top:'
            go tool pprof -top -alloc_space "$analysis_heap_profile"
        } >"$analysis_profile"
    else
        printf '%s\n' 'Profiles were captured; analyze them with the exact Go version recorded in the prebuilt manifest.' \
            >"$analysis_profile"
    fi
}

mode=smoke
result_dir=
prebuilt_dir=
prebuilt_run_dir=

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

require_command git
require_command mktemp
require_command sed

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../.." && pwd)
cd "$repo_root"

if [ -n "$prebuilt_dir" ]; then
    [ -d "$prebuilt_dir" ] || fail "prebuilt binary directory not found: $prebuilt_dir"
    prebuilt_dir=$(CDPATH='' cd -- "$prebuilt_dir" && pwd)
    [ -r "$prebuilt_dir/manifest.txt" ] || fail 'prebuilt manifest not found'
    for required_binary in proxy.test tunnel.test httpingress.test; do
        [ -x "$prebuilt_dir/$required_binary" ] || \
            fail "prebuilt benchmark binary not executable: $required_binary"
    done
else
    require_command go
    export GOTOOLCHAIN=local
    "$repo_root/tools/check-go-version.sh"
fi

if [ -z "$result_dir" ]; then
    result_dir=$(mktemp -d "${TMPDIR:-/tmp}/xtunnel-m7-01.XXXXXX")
else
    mkdir -p "$result_dir"
    result_dir=$(CDPATH='' cd -- "$result_dir" && pwd)
fi

capture_environment "$result_dir/environment.txt"

if [ "$mode" = full ]; then
    [ "$(uname -s)" = Linux ] || fail 'full mode requires Linux'
    require_command env
    require_command grep
    require_command strace
    require_command sha256sum
    [ -x /usr/bin/time ] || fail 'full mode requires /usr/bin/time'
    if [ -n "$(git status --porcelain --untracked-files=all)" ]; then
        fail 'full mode requires a clean worktree'
    fi
    if [ -n "$prebuilt_dir" ]; then
        require_command chmod
        require_command cp
        require_command rm
        require_command rmdir
        require_command stat
        if [ ! -d /tmp ] || [ ! -w /tmp ]; then
            fail 'prebuilt full mode requires a writable Linux-native /tmp'
        fi
        validated_manifest_hash=$(sha256sum "$prebuilt_dir/manifest.txt")
        validated_manifest_hash=${validated_manifest_hash%% *}
        manifest_commit=$(manifest_value commit)
        current_commit=$(git rev-parse HEAD)
        [ "$manifest_commit" = "$current_commit" ] || \
            fail 'prebuilt manifest Commit does not match the current checkout'
        verify_manifest_value worktree_clean true
        verify_manifest_value go_version go1.27.0
        verify_manifest_value toolchain local
        verify_manifest_value goos linux
        verify_manifest_value goarch amd64
        verify_manifest_value goamd64 v1
        verify_manifest_value cgo_enabled 0
        verify_prebuilt_binary proxy.test proxy_sha256
        verify_prebuilt_binary tunnel.test tunnel_sha256
        verify_prebuilt_binary httpingress.test httpingress_sha256

        stage_prebuilt_full "$validated_manifest_hash"

        cp "$prebuilt_run_dir/manifest.txt" "$result_dir/prebuilt-manifest.txt"
        manifest_hash=$(sha256sum "$result_dir/prebuilt-manifest.txt")
        manifest_hash=${manifest_hash%% *}
        [ "$manifest_hash" = "$validated_manifest_hash" ] || \
            fail 'recorded prebuilt manifest hash mismatch'
        printf '%s  %s\n' "$manifest_hash" 'prebuilt-manifest.txt' \
            >"$result_dir/prebuilt-manifest.sha256"
        printf 'prebuilt_manifest_sha256=%s\n' "$manifest_hash" \
            >>"$result_dir/environment.txt"
    fi
fi

printf 'M7-01 mode: %s\n' "$mode"
printf 'Results: %s\n' "$result_dir"

if [ "$mode" = smoke ]; then
    run_smoke_case proxy-buffer ./internal/proxy '^BenchmarkProxyBuffer/' proxy.test
    run_smoke_case connector-selection ./internal/tunnel '^BenchmarkConnectorSelection/' tunnel.test
    run_smoke_case http1-workconn ./internal/server/httpingress '^BenchmarkHTTP1WorkConnCapacity/' httpingress.test
else
    run_full_case proxy-buffer ./internal/proxy '^BenchmarkProxyBuffer/' proxy.test no
    run_proxy_analysis_case pooled-16k '^BenchmarkProxyBuffer/pooled_16k$'
    run_proxy_analysis_case pooled-32k '^BenchmarkProxyBuffer/pooled_32k$'
    run_proxy_analysis_case pooled-64k '^BenchmarkProxyBuffer/pooled_64k$'
    run_full_case connector-selection ./internal/tunnel '^BenchmarkConnectorSelection/' tunnel.test
    run_full_case http1-workconn ./internal/server/httpingress '^BenchmarkHTTP1WorkConnCapacity/' httpingress.test
fi

printf 'M7-01 benchmark completed. Results: %s\n' "$result_dir"
