#!/bin/sh

# 聚合既有 M7 正式 Runner，并在上传前统一扫描日志与生成 SHA-256 清单。
set -eu

usage() {
	printf '%s\n' 'Usage: run-hardening.sh -s chaos|analysis -o output-directory'
}

fail() {
	printf 'M7-10 hardening gate: %s\n' "$1" >&2
	exit 1
}

scope=
output_dir=
while [ "$#" -gt 0 ]; do
	case "$1" in
	-s)
		[ "$#" -ge 2 ] || { usage >&2; exit 2; }
		scope=$2
		shift 2
		;;
	-o)
		[ "$#" -ge 2 ] || { usage >&2; exit 2; }
		output_dir=$2
		shift 2
		;;
	-h|--help)
		usage
		exit 0
		;;
	*)
		usage >&2
		exit 2
		;;
	esac
done

case "$scope" in chaos|analysis) ;; *) usage >&2; exit 2 ;; esac
[ -n "$output_dir" ] || { usage >&2; exit 2; }
command -v go >/dev/null 2>&1 || fail 'Go is required'
command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
[ "$(go env GOVERSION)" = go1.27.0 ] || fail 'Go version must be go1.27.0'
[ "$(go env GOTOOLCHAIN)" = local ] || fail 'GOTOOLCHAIN must be local'

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH='' cd -- "$script_dir/../.." && pwd)
initial_commit=$(git -C "$repo_dir" rev-parse HEAD)
initial_tree=$(git -C "$repo_dir" rev-parse 'HEAD^{tree}')
initial_status=$(git -C "$repo_dir" status --porcelain --untracked-files=all)
[ -z "$initial_status" ] || \
	fail 'formal hardening gate requires a clean checkout'
mkdir "$output_dir" || fail 'output directory must not already exist'
output_dir=$(CDPATH='' cd -- "$output_dir" && pwd)
case "$output_dir/" in "$repo_dir/"*) fail 'output directory must be outside the repository' ;; esac

{
	printf 'scope=%s\n' "$scope"
	printf 'commit=%s\n' "$(git -C "$repo_dir" rev-parse HEAD)"
	printf 'tree=%s\n' "$(git -C "$repo_dir" rev-parse 'HEAD^{tree}')"
	printf 'go_version=%s\n' "$(go env GOVERSION)"
	printf 'go_toolchain=%s\n' "$(go env GOTOOLCHAIN)"
	printf 'uname=%s\n' "$(uname -a)"
	sed -n 's/^Max open files[[:space:]]*/fd_limit=/p' /proc/$$/limits
} >"$output_dir/environment.txt"

run_logged() {
	log_file=$1
	shift
	logged_status=0
	"$@" >"$log_file" 2>&1 || logged_status=$?
	if ! go run "$script_dir/secretscan" -path "$log_file" >/dev/null; then
		printf 'secret scan rejected %s; raw log suppressed\n' "$log_file" >&2
		return 1
	fi
	if [ "$logged_status" -ne 0 ]; then
		sed -n '1,240p' "$log_file" >&2
		return "$logged_status"
	fi
	sed -n '1,80p' "$log_file"
}

if [ "$scope" = chaos ]; then
	(
		cd "$repo_dir"
		run_logged "$output_dir/process-recovery.txt" go test -v -count=1 -timeout 90s \
			./internal/server/bootstrap -run '^TestM7ProcessRecoveryAfterSIGKILL$'
		run_logged "$output_dir/m7-02-full.txt" sh ./tests/chaos/run-m7-02.sh -m full
		run_logged "$output_dir/m7-04-full.txt" sh ./tests/chaos/run-m7-04.sh -m full
	)
else
	(
		cd "$repo_dir"
		run_logged "$output_dir/m7-05-runner.txt" sh ./tests/concurrency/run-m7-05.sh \
			-m full -o "$output_dir/m7-05"
		run_logged "$output_dir/m7-06-runner.txt" sh ./tests/fuzz/run-m7-06.sh \
			-m full -o "$output_dir/m7-06"
		run_logged "$output_dir/m7-07-runner.txt" sh ./tests/leak/run-m7-07.sh \
			-m full -o "$output_dir/m7-07"
	)
fi

go run "$script_dir/secretscan" -path "$output_dir" >"$output_dir/secret-scan.txt" 2>&1
(
	cd "$output_dir"
	find . -type f ! -name artifact-sha256.txt -print | LC_ALL=C sort | while IFS= read -r artifact; do
		sha256sum "$artifact"
	done >artifact-sha256.txt
	sha256sum -c artifact-sha256.txt
)
[ "$(git -C "$repo_dir" rev-parse HEAD)" = "$initial_commit" ] || fail 'repository commit changed during hardening gate'
[ "$(git -C "$repo_dir" rev-parse 'HEAD^{tree}')" = "$initial_tree" ] || fail 'repository tree changed during hardening gate'
[ "$(git -C "$repo_dir" status --porcelain --untracked-files=all)" = "$initial_status" ] || fail 'repository status changed during hardening gate'
printf 'M7-10 %s artifact directory: %s\n' "$scope" "$output_dir"
