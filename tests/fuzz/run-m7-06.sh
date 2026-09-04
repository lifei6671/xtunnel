#!/bin/sh

# 逐目标运行 M7-06 Go Fuzz。每个目标使用独立进程和单 worker，避免一个目标的
# 资源消耗、panic 或 timeout 被其他目标掩盖。

set -eu

usage() {
    printf '%s\n' 'Usage: run-m7-06.sh [-m smoke|full] [-o output-directory]'
    printf '%s\n' '  -m  smoke runs 5s/target; full runs 60s/target and requires a clean worktree.'
    printf '%s\n' '  -o  New or empty result directory outside the repository.'
    printf '%s\n' '  -h  Show this help.'
}

fail() {
    printf 'm7-06 fuzz: %s\n' "$1" >&2
    exit 1
}

fail_with_status() {
    failure_status=$1
    shift
    printf 'm7-06 fuzz: %s (exit=%s)\n' "$1" "$failure_status" >&2
    exit "$failure_status"
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_empty_directory() {
    inspected_directory=$1
    directory_entries=$(find "$inspected_directory/." ! -name . -prune -print) || \
        fail "cannot inspect result directory: $inspected_directory"
    [ -z "$directory_entries" ] || fail "result directory must be empty: $inspected_directory"
}

run_and_show() {
    run_output_file=$1
    shift
    if "$@" >"$run_output_file" 2>&1; then
        sed -n '1,240p' "$run_output_file"
        return 0
    else
        run_status=$?
        sed -n '1,240p' "$run_output_file" >&2
        return "$run_status"
    fi
}

targets='uvarint|./tests/fuzz|FuzzUVarintDecoder
frame|./tests/fuzz|FuzzFrameDecoder
control-envelope|./tests/fuzz|FuzzControlEnvelope
work-hello|./tests/fuzz|FuzzWorkHello
route-wire|./internal/server/route|FuzzSnapshotMatchHTTPFromWire
route-dangerous-path|./internal/server/route|FuzzSnapshotMatchHTTPRejectsDangerousPath
forwarded-for|./internal/server/httpingress|FuzzParseForwardedFor
forwarded-headers|./internal/server/httpingress|FuzzNormalizeForwardedHeaders'

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
    smoke)
        fuzz_time=5s
        minimize_time=2s
        command_timeout=60s
        process_timeout=90s
        ;;
    full)
        fuzz_time=60s
        minimize_time=10s
        command_timeout=3m
        process_timeout=4m
        ;;
    *) fail "unsupported mode: $mode" ;;
esac

require_command cmp
require_command date
require_command find
require_command git
require_command go
require_command mkdir
require_command mktemp
require_command sed
require_command sha256sum
require_command timeout

script_dir=$(CDPATH='' cd -P "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH='' cd -P "$script_dir/../.." && pwd -P)
[ -f "$repo_root/go.mod" ] || fail 'repository root does not contain go.mod'
[ -x "$repo_root/tools/check-go-version.sh" ] || \
    fail 'repository root does not contain an executable Go version check'
cd "$repo_root"

git_root=$(git rev-parse --show-toplevel) || fail 'cannot resolve Git repository root'
[ "$git_root" = "$repo_root" ] || fail 'script path does not resolve to the Git repository root'

export GOTOOLCHAIN=local
"$repo_root/tools/check-go-version.sh"
[ "$(go env GOTOOLCHAIN)" = local ] || fail 'GOTOOLCHAIN must equal local'

if [ "$mode" = full ] && [ -n "$(git status --porcelain --untracked-files=all)" ]; then
    fail 'full mode requires a clean worktree'
fi

umask 077
if [ -z "$result_dir" ]; then
    result_dir=$(mktemp -d "${TMPDIR:-/tmp}/xtunnel-m7-06.XXXXXX") || \
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

set -C
start_commit=$(git rev-parse HEAD) || fail 'cannot resolve initial commit'
start_tree=$(git rev-parse 'HEAD^{tree}') || fail 'cannot resolve initial tree'
git status --porcelain --untracked-files=all >"$result_dir/worktree-before.txt"
{
    printf 'captured_at_utc=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    printf 'commit=%s\n' "$start_commit"
    printf 'tree=%s\n' "$start_tree"
    printf 'mode=%s\n' "$mode"
    printf 'fuzz_time=%s\n' "$fuzz_time"
    printf 'minimize_time=%s\n' "$minimize_time"
    go env GOVERSION GOTOOLCHAIN GOOS GOARCH GOAMD64 CGO_ENABLED
    go version
    git --version
    timeout --version | sed -n '1p'
    sha256sum --version | sed -n '1p'
} >"$result_dir/environment.txt"

export GOCACHE="$result_dir/go-cache"
mkdir "$GOCACHE" || fail 'cannot create private Go cache'

: >"$result_dir/commands.txt"
printf '%s\n' "$targets" | while IFS='|' read -r label package target; do
    printf "timeout -k 15s %s go test %s -run '^$' -fuzz '^%s$' -fuzztime=%s -fuzzminimizetime=%s -parallel=1 -timeout=%s\n" \
        "$process_timeout" "$package" "$target" "$fuzz_time" "$minimize_time" "$command_timeout" \
        >>"$result_dir/commands.txt"
    if run_and_show "$result_dir/$label.log" \
        timeout -k 15s "$process_timeout" \
        go test "$package" -run '^$' -fuzz "^$target$" \
        -fuzztime="$fuzz_time" -fuzzminimizetime="$minimize_time" \
        -parallel=1 -timeout="$command_timeout"
    then
        :
    else
        run_status=$?
        fail_with_status "$run_status" "target failed: $target"
    fi
done

git status --porcelain --untracked-files=all >"$result_dir/worktree-after.txt"
cmp "$result_dir/worktree-before.txt" "$result_dir/worktree-after.txt" >/dev/null || \
    fail 'worktree changed during fuzz run; preserve any generated reproducer'
end_commit=$(git rev-parse HEAD) || fail 'cannot resolve final commit'
end_tree=$(git rev-parse 'HEAD^{tree}') || fail 'cannot resolve final tree'
[ "$end_commit" = "$start_commit" ] || fail 'HEAD changed during fuzz run'
[ "$end_tree" = "$start_tree" ] || fail 'HEAD tree changed during fuzz run'

sha256sum "$result_dir/commands.txt" "$result_dir/environment.txt" "$result_dir"/*.log \
    "$result_dir/worktree-before.txt" "$result_dir/worktree-after.txt" \
    >"$result_dir/artifact-sha256.txt"
printf 'M7-06 fuzz %s completed. Results: %s\n' "$mode" "$result_dir"
