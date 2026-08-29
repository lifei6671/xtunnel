#!/bin/sh
set -eu

usage() {
	cat <<'EOF'
Usage: smoke.sh [--target server|agent] [--platform linux/amd64|linux/arm64] [--image NAME] [--skip-build]

Builds one OCI target and verifies its architecture, dedicated entrypoint,
non-root identity, read-only root filesystem, lifecycle logs, and clean SIGTERM
shutdown. Server uses its persistent data volume and runtime tmpfs; Agent uses
only an environment-provided non-secret test Token.
EOF
}

target=server
platform=linux/amd64
image=
build_image=1
build_version=${XTUNNEL_VERSION:-v0.1.0-smoke}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--target)
			[ "$#" -ge 2 ] || { usage >&2; exit 2; }
			target=${2-}
			shift 2
			;;
		--platform)
			[ "$#" -ge 2 ] || { usage >&2; exit 2; }
			platform=${2-}
			shift 2
			;;
		--image)
			[ "$#" -ge 2 ] || { usage >&2; exit 2; }
			image=${2-}
			shift 2
			;;
		--skip-build)
			build_image=0
			shift
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

case "$target" in
	server|agent) ;;
	*)
		printf '%s\n' "target must be server or agent" >&2
		exit 2
		;;
esac

case "$platform" in
	linux/amd64|linux/arm64) ;;
	*)
		printf '%s\n' "platform must be linux/amd64 or linux/arm64" >&2
		exit 2
		;;
esac

if ! command -v docker >/dev/null 2>&1; then
	printf '%s\n' "docker is required for the OCI smoke test" >&2
	exit 1
fi

if [ -z "$image" ]; then
	image="xtunnel-$target-smoke:local"
fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH='' cd -- "$script_dir/../.." && pwd)

agent_token=
if [ "$target" = agent ]; then
	agent_token_path="$repo_dir/tests/golden/protocol-v1/connection-token-v1.txt"
	if [ ! -r "$agent_token_path" ]; then
		printf 'Agent smoke Connection Token fixture is not readable: %s\n' "$agent_token_path" >&2
		exit 1
	fi
	# Protocol Golden 由生产编码器生成并逐字节锁定。复用该公开测试向量可让
	# OCI Smoke 保持“只依赖 Docker”，同时避免 Shell 复制 Wire 编码规则。
	IFS= read -r agent_token <"$agent_token_path" || true
	if [ -z "$agent_token" ]; then
		printf '%s\n' "Agent smoke Connection Token fixture is empty" >&2
		exit 1
	fi
fi

if [ "$build_image" -eq 1 ]; then
	if ! docker buildx version >/dev/null 2>&1; then
		printf '%s\n' "docker buildx is required to build the OCI smoke image" >&2
		exit 1
	fi
	docker buildx build \
		--load \
		--platform "$platform" \
		--target "$target" \
		--build-arg "XTUNNEL_VERSION=$build_version" \
		--tag "$image" \
		--file "$script_dir/Dockerfile" \
		"$repo_dir"
fi

expected_arch=${platform#linux/}
expected_entrypoint="/usr/local/bin/xtunnel-$target"
test "$(docker image inspect --format '{{.Architecture}}' "$image")" = "$expected_arch"
test "$(docker image inspect --format '{{.Config.User}}' "$image")" = '65532:65532'
test "$(docker image inspect --format '{{join .Config.Entrypoint " "}}' "$image")" = "$expected_entrypoint"
image_volumes=$(docker image inspect --format '{{json .Config.Volumes}}' "$image")
if [ "$target" = server ]; then
	printf '%s' "$image_volumes" | grep -F '"/var/lib/xtunnel"' >/dev/null
else
	test "$image_volumes" = null
fi

volume=
container=
boundary_container=
layout_dir=$(mktemp -d)
# Server 默认容量的 FD 预算为 87188。OCI Runtime 必须显式提供更高的
# soft/hard limit；镜像内的非 root 进程无法自行提升宿主施加的硬上限。
server_nofile_limit=1048576

cleanup() {
	if [ -n "$boundary_container" ]; then
		docker rm --force "$boundary_container" >/dev/null 2>&1 || true
	fi
	if [ -n "$container" ]; then
		docker rm --force "$container" >/dev/null 2>&1 || true
	fi
	if [ -n "$volume" ]; then
		docker volume rm --force "$volume" >/dev/null 2>&1 || true
	fi
	rm -rf -- "$layout_dir"
}
trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if [ "$target" = server ]; then
	volume="xtunnel-$target-smoke-$$"
	docker volume create "$volume" >/dev/null
fi

# 最终镜像只应包含自己的入口二进制；--help 可避免误存在时启动常驻进程。
if [ "$target" = server ]; then
	sibling=agent
else
	sibling=server
fi
if docker run --rm --platform "$platform" --entrypoint "/usr/local/bin/xtunnel-$sibling" "$image" --help >/dev/null 2>&1; then
	printf 'image unexpectedly contains sibling binary: xtunnel-%s\n' "$sibling" >&2
	exit 1
fi

run_target() {
	if [ "$target" = server ]; then
		docker run --detach \
			--platform "$platform" \
			--read-only \
			--ulimit "nofile=$server_nofile_limit:$server_nofile_limit" \
			--tmpfs /run/xtunnel:rw,nosuid,nodev,noexec,size=1m,mode=0700,uid=65532,gid=65532 \
			--mount "type=volume,source=$volume,target=/var/lib/xtunnel" \
			"$image" \
			--set management.public_url=https://smoke.invalid \
			--set agent_gateway.public_hostname=smoke.invalid
	else
		docker run --detach \
			--platform "$platform" \
			--read-only \
			--env "XTUNNEL_TOKEN=$agent_token" \
			"$image"
	fi
}

wait_for_start() {
	attempt=0
	while [ "$attempt" -lt 30 ]; do
		if docker logs "$container" 2>&1 | grep -F '"event":"process_started"' >/dev/null; then
			# process_started 是历史日志，不等于进程此刻仍然存活。额外观察一个
			# 调度周期，避免把“启动后立即因永久错误退出”误判为健康启动。
			if [ "$(docker inspect --format '{{.State.Running}}' "$container")" = true ]; then
				sleep 1
				if [ "$(docker inspect --format '{{.State.Running}}' "$container")" = true ]; then
					return 0
				fi
			fi
			docker logs "$container" >&2 || true
			printf '%s\n' "container exited after process_started" >&2
			return 1
		fi
		if [ "$(docker inspect --format '{{.State.Running}}' "$container")" != true ]; then
			docker logs "$container" >&2 || true
			printf '%s\n' "container exited before process_started" >&2
			return 1
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	docker logs "$container" >&2 || true
	printf '%s\n' "container did not report process_started" >&2
	return 1
}

verify_runtime_mounts() {
	test "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$container")" = true
	if [ "$target" = server ]; then
		test "$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/xtunnel"}}{{.RW}}{{end}}{{end}}' "$container")" = true
		test -n "$(docker inspect --format '{{index .HostConfig.Tmpfs "/run/xtunnel"}}' "$container")"
		test "$(docker inspect --format '{{range .HostConfig.Ulimits}}{{if eq .Name "nofile"}}{{.Soft}}:{{.Hard}}{{end}}{{end}}' "$container")" = "$server_nofile_limit:$server_nofile_limit"
	else
		test "$(docker inspect --format '{{len .Mounts}}' "$container")" -eq 0
		test "$(docker inspect --format '{{join .Config.Cmd " "}}' "$container")" = run
		docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container" | grep -Fx "XTUNNEL_TOKEN=$agent_token" >/dev/null
	fi
}

verify_embedded_version() {
	[ "$build_image" -eq 1 ] || return 0
	binary_path="$layout_dir/xtunnel-$target"
	docker cp "$container:$expected_entrypoint" "$binary_path"
	# Linker 符号在最终 Distroless Image 中没有可执行的诊断 Shell；直接从刚构建
	# 的 Binary 查找精确测试版本，并与 buildinfo 的 Linker 子进程测试共同锁定接线。
	LC_ALL=C grep -aF -- "$build_version" "$binary_path" >/dev/null
}

verify_server_data_layout() {
	[ "$target" = server ] || return 0
	rm -f -- "$layout_dir/xtunnel.db" "$layout_dir/legacy-xtunnel.db"
	docker cp "$container:/var/lib/xtunnel/data/xtunnel.db" "$layout_dir/xtunnel.db"
	test -s "$layout_dir/xtunnel.db"
	if docker cp "$container:/var/lib/xtunnel/xtunnel.db" "$layout_dir/legacy-xtunnel.db" >/dev/null 2>&1; then
		printf '%s\n' "Server wrote SQLite at the legacy Volume root" >&2
		return 1
	fi
}

stop_target() {
	# 容器在收到本次 SIGTERM 前退出属于生命周期失败，不能用 `|| true`
	# 掩盖；捕获 Docker 错误并附带最终状态，便于 CI 直接定位根因。
	if ! kill_error=$(docker kill --signal TERM "$container" 2>&1); then
		container_state=$(docker inspect --format 'status={{.State.Status}} exit={{.State.ExitCode}}' "$container" 2>/dev/null || printf '%s' unavailable)
		docker logs "$container" >&2 || true
		printf 'container was not running when SIGTERM was sent: %s (%s)\n' "$container_state" "$kill_error" >&2
		return 1
	fi
	status=$(docker wait "$container")
	if [ "$status" -ne 0 ]; then
		docker logs "$container" >&2 || true
		printf '%s\n' "container did not exit cleanly after SIGTERM" >&2
		return 1
	fi
	docker logs "$container" 2>&1 | grep -F '"event":"process_stopped"' >/dev/null
	docker rm "$container" >/dev/null
	container=
}

verify_server_runtime_boundary() {
	[ "$target" = server ] || return 0
	boundary_container=$(docker run --detach \
		--platform "$platform" \
		--read-only \
		--ulimit "nofile=$server_nofile_limit:$server_nofile_limit" \
		--mount "type=volume,source=$volume,target=/var/lib/xtunnel" \
		"$image" \
		--set management.public_url=https://smoke.invalid \
		--set agent_gateway.public_hostname=smoke.invalid)

	attempt=0
	while [ "$attempt" -lt 10 ] && [ "$(docker inspect --format '{{.State.Running}}' "$boundary_container")" = true ]; do
		attempt=$((attempt + 1))
		sleep 1
	done
	if [ "$(docker inspect --format '{{.State.Running}}' "$boundary_container")" = true ]; then
		printf '%s\n' "server unexpectedly started without a writable /run/xtunnel" >&2
		return 1
	fi
	status=$(docker inspect --format '{{.State.ExitCode}}' "$boundary_container")
	if [ "$status" -eq 0 ]; then
		printf '%s\n' "server exited successfully without its required runtime tmpfs" >&2
		return 1
	fi
	boundary_logs=$(docker logs "$boundary_container" 2>&1 || true)
	if ! printf '%s' "$boundary_logs" | grep -F '/run/xtunnel' >/dev/null; then
		printf '%s\n' "server boundary failure did not identify the missing /run/xtunnel runtime directory" >&2
		printf '%s\n' "$boundary_logs" >&2
		return 1
	fi
	if printf '%s' "$boundary_logs" | grep -F 'file descriptor budget' >/dev/null; then
		printf '%s\n' "server boundary check failed at the FD budget before validating /run/xtunnel" >&2
		printf '%s\n' "$boundary_logs" >&2
		return 1
	fi
	docker rm "$boundary_container" >/dev/null
	boundary_container=
}

verify_server_runtime_boundary

container=$(run_target)
wait_for_start
verify_runtime_mounts
verify_embedded_version
verify_server_data_layout
stop_target

# Server 的第二次启动会重新打开同一卷中的 SQLite；Agent 则重复验证无状态前台生命周期。
container=$(run_target)
wait_for_start
verify_runtime_mounts
verify_server_data_layout
stop_target

printf 'OCI smoke passed: target=%s platform=%s image=%s\n' "$target" "$platform" "$image"
