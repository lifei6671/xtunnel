#!/bin/sh
set -eu

usage() {
	cat <<'EOF'
Usage: dualstack-smoke.sh [--platform linux/amd64|linux/arm64] [--skip-build]

Starts the Compose deployment and verifies that its network and both containers
receive IPv4 and IPv6 addresses without weakening the OCI runtime boundaries.
EOF
}

platform=linux/amd64
build=1
while [ "$#" -gt 0 ]; do
	case "$1" in
		--platform)
			[ "$#" -ge 2 ] || { usage >&2; exit 2; }
			platform=${2-}
			shift 2
			;;
		--skip-build)
			build=0
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

case "$platform" in
	linux/amd64|linux/arm64) ;;
	*)
		printf '%s\n' "platform must be linux/amd64 or linux/arm64" >&2
		exit 2
		;;
esac

if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
	printf '%s\n' "Docker Engine with the Compose v2 plugin is required" >&2
	exit 1
fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
compose_file="$script_dir/compose.dualstack.yaml"
project="xtunnel-dualstack-smoke-$(date +%s)-$$"
export COMPOSE_PROJECT_NAME="$project"
export COMPOSE_PARALLEL_LIMIT=1
export XTUNNEL_PLATFORM="$platform"
export XTUNNEL_MANAGEMENT_PUBLIC_URL=https://localhost:8080
export XTUNNEL_AGENT_GATEWAY_HOSTNAME=localhost
# 端口 0 让 Docker 为四个宿主监听分别选择空闲端口，避免 Smoke 与现有服务冲突。
export XTUNNEL_MANAGEMENT_PORT=0
export XTUNNEL_AGENT_GATEWAY_PORT=0
export XTUNNEL_AGENT_TOKEN=xta_compose_smoke_not_secret
if [ "$build" -eq 1 ]; then
	export XTUNNEL_SERVER_IMAGE="xtunnel-server-$project:local"
	export XTUNNEL_AGENT_IMAGE="xtunnel-agent-$project:local"
else
	export XTUNNEL_SERVER_IMAGE="${XTUNNEL_SERVER_IMAGE:-xtunnel-server:local}"
	export XTUNNEL_AGENT_IMAGE="${XTUNNEL_AGENT_IMAGE:-xtunnel-agent:local}"
fi

compose() {
	docker compose --file "$compose_file" "$@"
}

cleanup() {
	compose down --volumes --remove-orphans >/dev/null 2>&1 || true
	if [ "$build" -eq 1 ]; then
		docker image rm --force "$XTUNNEL_SERVER_IMAGE" "$XTUNNEL_AGENT_IMAGE" >/dev/null 2>&1 || true
	fi
}
trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

compose config --quiet
if [ "$build" -eq 1 ]; then
	# 两个纯 Go Binary 顺序冷编译，避免资源受限的 WSL Builder 同时展开两棵编译图。
	compose build server
	compose build agent
fi
compose up --detach --no-build

wait_for_start() {
	service=$1
	attempt=0
	while [ "$attempt" -lt 30 ]; do
		if compose logs "$service" 2>&1 | grep -F '"event":"process_started"' >/dev/null; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	compose logs "$service" >&2 || true
	printf '%s\n' "$service did not report process_started" >&2
	return 1
}

wait_for_start server
wait_for_start agent

network_id=$(docker network ls \
	--filter "label=com.docker.compose.project=$project" \
	--filter "label=com.docker.compose.network=dualstack" \
	--format '{{.ID}}')
test -n "$network_id"
test "$(docker network inspect --format '{{.EnableIPv6}}' "$network_id")" = true

verify_container() {
	service=$1
	expected_entrypoint="/usr/local/bin/xtunnel-$service"
	expected_arch=${platform#linux/}
	container_id=$(compose ps --quiet "$service")
	test -n "$container_id"
	image_id=$(docker inspect --format '{{.Image}}' "$container_id")
	test "$(docker inspect --format '{{.Config.User}}' "$container_id")" = '65532:65532'
	test "$(docker image inspect --format '{{.Architecture}}' "$image_id")" = "$expected_arch"
	test "$(docker inspect --format '{{join .Config.Entrypoint " "}}' "$container_id")" = "$expected_entrypoint"
	test "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$container_id")" = true
	test "$(docker inspect --format '{{join .HostConfig.CapDrop " "}}' "$container_id")" = ALL
	docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$container_id" | grep -F 'no-new-privileges:true' >/dev/null
	if [ "$service" = server ]; then
		test "$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/xtunnel"}}{{.RW}}{{end}}{{end}}' "$container_id")" = true
	else
		test "$(docker inspect --format '{{len .Mounts}}' "$container_id")" -eq 0
		test "$(docker inspect --format '{{join .Config.Cmd " "}}' "$container_id")" = run
		docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container_id" | grep -Fx "XTUNNEL_TOKEN=$XTUNNEL_AGENT_TOKEN" >/dev/null
	fi

	ipv4_address=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$container_id")
	ipv6_address=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.GlobalIPv6Address}}{{end}}' "$container_id")
	test -n "$ipv4_address"
	test -n "$ipv6_address"
}

verify_container server
verify_container agent

server_id=$(compose ps --quiet server)
agent_id=$(compose ps --quiet agent)
test -n "$(docker inspect --format '{{index .HostConfig.Tmpfs "/run/xtunnel"}}' "$server_id")"
agent_tmpfs=$(docker inspect --format '{{json .HostConfig.Tmpfs}}' "$agent_id")
test "$agent_tmpfs" = null || test "$agent_tmpfs" = '{}'
server_volume=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/xtunnel"}}{{.Name}}{{end}}{{end}}' "$server_id")
test -n "$server_volume"
test -z "$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/xtunnel"}}{{.Name}}{{end}}{{end}}' "$agent_id")"

published_ports=$(docker inspect --format '{{json .NetworkSettings.Ports}}' "$server_id")
printf '%s' "$published_ports" | grep -F '"HostIp":"127.0.0.1"' >/dev/null
printf '%s' "$published_ports" | grep -F '"HostIp":"::1"' >/dev/null
printf '%s' "$published_ports" | grep -F '"HostIp":"0.0.0.0"' >/dev/null
printf '%s' "$published_ports" | grep -F '"HostIp":"::"' >/dev/null
if printf '%s' "$published_ports" | grep -F '"HostPort":"0"' >/dev/null; then
	printf '%s\n' "Docker did not allocate the requested ephemeral host ports" >&2
	exit 1
fi

compose stop
for service in server agent; do
	compose logs "$service" 2>&1 | grep -F '"event":"process_stopped"' >/dev/null
	container_id=$(compose ps --all --quiet "$service")
	test "$(docker inspect --format '{{.State.ExitCode}}' "$container_id")" -eq 0
done

printf 'Compose dual-stack smoke passed: platform=%s project=%s\n' "$platform" "$project"
