#!/bin/sh

# 执行 M7-10 公网数据面成功率、Open P95 和资源收敛 Gate。
set -eu

usage() {
	printf '%s\n' 'Usage: run-scale.sh -m smoke|full -o output-directory'
}

fail() {
	printf 'M7-10 scale gate: %s\n' "$1" >&2
	exit 1
}

mode=smoke
output_dir=
while [ "$#" -gt 0 ]; do
	case "$1" in
	-m) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; mode=$2; shift 2 ;;
	-o) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; output_dir=$2; shift 2 ;;
	-h|--help) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done
case "$mode" in smoke|full) ;; *) usage >&2; exit 2 ;; esac
[ -n "$output_dir" ] || { usage >&2; exit 2; }
command -v go >/dev/null 2>&1 || fail 'Go is required'
command -v timeout >/dev/null 2>&1 || fail 'timeout is required'
command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
[ "$(go env GOTOOLCHAIN)" = local ] || fail 'GOTOOLCHAIN must be local'

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH='' cd -- "$script_dir/../.." && pwd)
[ -x "$repo_dir/tools/check-go-version.sh" ] || fail 'repository root does not contain an executable Go version check'
"$repo_dir/tools/check-go-version.sh"
initial_commit=$(git -C "$repo_dir" rev-parse HEAD)
initial_tree=$(git -C "$repo_dir" rev-parse 'HEAD^{tree}')
initial_status=$(git -C "$repo_dir" status --porcelain --untracked-files=all)
[ "$mode" != full ] || [ -z "$initial_status" ] || \
	fail 'formal scale gate requires a clean checkout'
mkdir "$output_dir" || fail 'output directory must not already exist'
output_dir=$(CDPATH='' cd -- "$output_dir" && pwd)
case "$output_dir/" in "$repo_dir/"*) fail 'output directory must be outside the repository' ;; esac

{
	printf 'mode=%s\n' "$mode"
	printf 'commit=%s\n' "$(git -C "$repo_dir" rev-parse HEAD)"
	printf 'tree=%s\n' "$(git -C "$repo_dir" rev-parse 'HEAD^{tree}')"
	printf 'go_version=%s\n' "$(go env GOVERSION)"
	printf 'go_toolchain=%s\n' "$(go env GOTOOLCHAIN)"
	printf 'uname=%s\n' "$(uname -a)"
	sed -n 's/^Max open files[[:space:]]*/fd_limit=/p' /proc/$$/limits
	command -v lscpu >/dev/null 2>&1 && lscpu
	[ ! -r /proc/meminfo ] || sed -n '1,20p' /proc/meminfo
} >"$output_dir/environment.txt"

run_tier() {
	count=$1
	time_limit=$2
	log="$output_dir/$count.txt"
	tier_status=0
	(
		cd "$repo_dir"
		XTUNNEL_M7_10_CONNECTIONS="$count" timeout -k 30s "$time_limit" \
			go test -v -count=1 -timeout "$time_limit" ./internal/server/bootstrap \
			-run '^TestM7AlphaPublicConnectionGate$'
	) >"$log" 2>&1 || tier_status=$?
	if ! go run "$script_dir/secretscan" -path "$log" >/dev/null; then
		printf 'secret scan rejected %s; raw log suppressed\n' "$log" >&2
		return 1
	fi
	if [ "$tier_status" -ne 0 ]; then
		sed -n '1,240p' "$log" >&2
		return "$tier_status"
	fi
	sed -n '1,120p' "$log"
}

if [ "$mode" = smoke ]; then
	run_tier 10 5m
else
	run_tier 1000 15m
	run_tier 5000 40m
fi

go run "$script_dir/secretscan" -path "$output_dir" >"$output_dir/secret-scan.txt" 2>&1
(
	cd "$output_dir"
	find . -type f ! -name artifact-sha256.txt -print | LC_ALL=C sort | while IFS= read -r artifact; do
		sha256sum "$artifact"
	done >artifact-sha256.txt
	sha256sum -c artifact-sha256.txt
)
[ "$(git -C "$repo_dir" rev-parse HEAD)" = "$initial_commit" ] || fail 'repository commit changed during scale gate'
[ "$(git -C "$repo_dir" rev-parse 'HEAD^{tree}')" = "$initial_tree" ] || fail 'repository tree changed during scale gate'
[ "$(git -C "$repo_dir" status --porcelain --untracked-files=all)" = "$initial_status" ] || fail 'repository status changed during scale gate'
printf 'M7-10 scale artifact directory: %s\n' "$output_dir"
