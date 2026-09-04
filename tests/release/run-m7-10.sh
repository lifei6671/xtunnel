#!/bin/sh

# 构建双架构 OCI Layout，并在上传前校验平台、配置、内容摘要和秘密形状。
set -eu

usage() {
	printf '%s\n' 'Usage: run-m7-10.sh -o output-directory'
}

fail() {
	printf 'M7-10 release gate: %s\n' "$1" >&2
	exit 1
}

output_dir=
while [ "$#" -gt 0 ]; do
	case "$1" in
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

[ -n "$output_dir" ] || { usage >&2; exit 2; }
command -v docker >/dev/null 2>&1 || fail 'docker is required'
command -v go >/dev/null 2>&1 || fail 'Go is required'
command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
[ "$(go env GOTOOLCHAIN)" = local ] || fail 'GOTOOLCHAIN must be local'
docker buildx version >/dev/null 2>&1 || fail 'docker buildx is required'

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH='' cd -- "$script_dir/../.." && pwd)
[ -x "$repo_dir/tools/check-go-version.sh" ] || fail 'repository root does not contain an executable Go version check'
"$repo_dir/tools/check-go-version.sh"
initial_commit=$(git -C "$repo_dir" rev-parse HEAD)
initial_tree=$(git -C "$repo_dir" rev-parse 'HEAD^{tree}')
initial_status=$(git -C "$repo_dir" status --porcelain --untracked-files=all)
[ -z "$initial_status" ] || \
	fail 'formal release gate requires a clean checkout'
mkdir "$output_dir" || fail 'output directory must not already exist'
output_dir=$(CDPATH='' cd -- "$output_dir" && pwd)
case "$output_dir/" in "$repo_dir/"*) fail 'output directory must be outside the repository' ;; esac
version="v0.1.0-alpha.${GITHUB_SHA:-$(git -C "$repo_dir" rev-parse HEAD)}"
binary_dir="$output_dir/binaries"
mkdir "$binary_dir"

run_scanned() {
	log_file=$1
	shift
	command_status=0
	"$@" >"$log_file" 2>&1 || command_status=$?
	if ! go run "$script_dir/secretscan" -path "$log_file" >/dev/null; then
		printf 'secret scan rejected %s; raw log suppressed\n' "$log_file" >&2
		return 1
	fi
	if [ "$command_status" -ne 0 ]; then
		sed -n '1,240p' "$log_file" >&2
		return "$command_status"
	fi
}

# Hosted Runner 的默认 docker driver 不提供 OCI exporter。每次发布验证使用独立的
# docker-container builder，并通过显式 --builder 隔离，不改变调用方的默认构建器。
builder_name="xtunnel-m7-10-$$"
builder_owned=false
cleanup_builder() {
	if [ "$builder_owned" != true ]; then
		return 0
	fi
	if ! docker buildx rm "$builder_name" >/dev/null 2>&1; then
		printf '%s\n' 'M7-10 cleanup failed for the isolated Buildx builder' >&2
		return 1
	fi
}
trap 'exit 129' 1
trap 'exit 130' 2
trap 'exit 143' 15
trap 'status=$?; trap - 0; cleanup_builder || { [ "$status" -ne 0 ] || status=1; }; exit "$status"' 0
builder_create_log="$output_dir/buildx-create.txt"
builder_create_status=0
docker buildx create --driver docker-container --name "$builder_name" >"$builder_create_log" 2>&1 || builder_create_status=$?
if [ "$builder_create_status" -eq 0 ]; then
	builder_owned=true
fi
if ! go run "$script_dir/secretscan" -path "$builder_create_log" >/dev/null; then
	printf 'secret scan rejected %s; raw log suppressed\n' "$builder_create_log" >&2
	exit 1
fi
if [ "$builder_create_status" -ne 0 ]; then
	sed -n '1,240p' "$builder_create_log" >&2
	exit "$builder_create_status"
fi
run_scanned "$output_dir/buildx-bootstrap.txt" docker buildx inspect \
	--builder "$builder_name" --bootstrap

{
	printf 'commit=%s\n' "$(git -C "$repo_dir" rev-parse HEAD)"
	printf 'worktree_clean=%s\n' "$(test -z "$(git -C "$repo_dir" status --porcelain --untracked-files=all)" && printf true || printf false)"
	printf 'go_version=%s\n' "$(go env GOVERSION)"
	printf 'go_toolchain=%s\n' "$(go env GOTOOLCHAIN)"
	printf 'docker_version=%s\n' "$(docker version --format '{{.Server.Version}}')"
	printf 'buildx_version=%s\n' "$(docker buildx version)"
	printf 'buildx_driver=docker-container\n'
	printf 'platforms=linux/amd64,linux/arm64\n'
} >"$output_dir/environment.txt"

# Standalone 发布候选与部署表面使用同一规则扫描。四个平台 Binary 均从当前干净
# Commit 构建；配置示例只允许哈希固定、不可用的占位 Token。
for platform in linux-amd64 linux-arm64 windows-amd64 windows-arm64; do
	goos=${platform%-*}
	goarch=${platform#*-}
	extension=
	[ "$goos" != windows ] || extension=.exe
	for target in agent server; do
		# V0.1 的 Windows Server 不在支持矩阵，不能把交叉编译结果包装成发布物。
		[ "$goos" != windows ] || [ "$target" = agent ] || continue
		(
			cd "$repo_dir"
			CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
				-ldflags="-s -w -X github.com/lifei6671/xtunnel/internal/buildinfo.version=$version" \
				-o "$binary_dir/xtunnel-$target-$platform$extension" "./cmd/$target"
		)
	done
done
go run "$script_dir/secretscan" -path "$binary_dir" >"$output_dir/binary-secret-scan.txt" 2>&1
go run "$script_dir/secretscan" -path "$repo_dir/configs" \
	-allowlist "$script_dir/config-secret-allowlist.txt" >"$output_dir/config-secret-scan.txt" 2>&1
go run "$script_dir/secretscan" -path "$repo_dir/deploy" >"$output_dir/deploy-secret-scan.txt" 2>&1

for target in agent server; do
	archive="$output_dir/xtunnel-$target.oci.tar"
	log="$output_dir/$target-build.txt"
	run_scanned "$log" docker buildx build \
		--builder "$builder_name" \
		--pull \
		--progress plain \
		--platform linux/amd64,linux/arm64 \
		--provenance=false \
		--sbom=false \
		--target "$target" \
		--build-arg "XTUNNEL_VERSION=$version" \
		--output "type=oci,dest=$archive" \
		--file "$repo_dir/deploy/docker/Dockerfile" \
		"$repo_dir"
	run_scanned "$output_dir/$target-verify.txt" go run "$script_dir/ociverify" \
		-archive "$archive" -target "$target"
done

# 先扫描全部可上传文本；OCI Layer 已由 ociverify 解包扫描。公开 Golden 和含
# Master Key 的 Backup 都不进入发布 Artifact，因此不需要路径级宽松匹配。
go run "$script_dir/secretscan" -path "$output_dir" >"$output_dir/artifact-secret-scan.txt" 2>&1

(
	cd "$output_dir"
	find . -type f ! -name artifact-sha256.txt -print | LC_ALL=C sort | while IFS= read -r artifact; do
		sha256sum "$artifact"
	done >artifact-sha256.txt
	sha256sum -c artifact-sha256.txt
)
[ "$(git -C "$repo_dir" rev-parse HEAD)" = "$initial_commit" ] || fail 'repository commit changed during release gate'
[ "$(git -C "$repo_dir" rev-parse 'HEAD^{tree}')" = "$initial_tree" ] || fail 'repository tree changed during release gate'
[ "$(git -C "$repo_dir" status --porcelain --untracked-files=all)" = "$initial_status" ] || fail 'repository status changed during release gate'
printf 'M7-10 release candidate evidence directory: %s\n' "$output_dir"
