#!/bin/sh
set -eu

usage() {
	cat <<'EOF'
Usage: smoke.sh --server-binary PATH --agent-binary PATH

Runs install, start, restart, stop, start, and uninstall checks for both
systemd services. This destructive test is only for an isolated Linux host. It
refuses to run when any XTunnel path, service user, or service group exists.
EOF
}

server_binary=
agent_binary=
while [ "$#" -gt 0 ]; do
	case "$1" in
		--server-binary)
			[ "$#" -ge 2 ] || { usage >&2; exit 2; }
			server_binary=${2-}
			shift 2
			;;
		--agent-binary)
			[ "$#" -ge 2 ] || { usage >&2; exit 2; }
			agent_binary=${2-}
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

if [ "$(id -u)" -ne 0 ]; then
	printf '%s\n' "systemd smoke test must run as root" >&2
	exit 1
fi

if [ -z "$server_binary" ] || [ -z "$agent_binary" ] || [ ! -x "$server_binary" ] || [ ! -x "$agent_binary" ]; then
	printf '%s\n' "both --server-binary and --agent-binary must name executable files" >&2
	exit 2
fi

if ! command -v systemctl >/dev/null 2>&1 || ! systemctl show --property=Version --value >/dev/null; then
	printf '%s\n' "a running systemd system manager is required" >&2
	exit 1
fi

for path in \
	/etc/systemd/system/xtunnel-server.service \
	/etc/systemd/system/xtunnel-agent.service \
	/usr/local/bin/xtunnel-server \
	/usr/local/bin/xtunnel-agent \
	/etc/xtunnel \
	/run/xtunnel \
	/run/xtunnel-agent \
	/var/lib/xtunnel \
	/var/lib/xtunnel-agent; do
	if [ -e "$path" ] || [ -L "$path" ]; then
		printf 'refusing to overwrite existing path: %s\n' "$path" >&2
		exit 1
	fi
done

for service_user in xtunnel-server xtunnel-agent; do
	if id "$service_user" >/dev/null 2>&1 || getent group "$service_user" >/dev/null 2>&1; then
		printf 'refusing to reuse existing service identity: %s\n' "$service_user" >&2
		exit 1
	fi
done

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
temp_dir=$(mktemp -d)

cleanup() {
	sh "$script_dir/uninstall.sh" server >/dev/null 2>&1 || true
	sh "$script_dir/uninstall.sh" agent >/dev/null 2>&1 || true
	for unit in xtunnel-server.service xtunnel-agent.service; do
		if systemctl is-active --quiet "$unit"; then
			printf 'cleanup stopped before deleting files because %s is still active\n' "$unit" >&2
			return
		fi
	done
	rm -rf -- \
		/etc/xtunnel \
		/run/xtunnel \
		/run/xtunnel-agent \
		/var/lib/xtunnel \
		/var/lib/xtunnel-agent \
		"$temp_dir"
	for service_user in xtunnel-server xtunnel-agent; do
		if id "$service_user" >/dev/null 2>&1; then
			userdel "$service_user" >/dev/null 2>&1 || true
		fi
		if getent group "$service_user" >/dev/null 2>&1; then
			groupdel "$service_user" >/dev/null 2>&1 || true
		fi
	done
}
trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

umask 077
cat >"$temp_dir/server.yaml" <<'EOF'
management:
  public_url: https://smoke.invalid
agent_gateway:
  public_hostname: smoke.invalid
EOF

# Unit 会把 Agent 的通用 Schema 默认值覆盖到独立的 systemd 状态目录。
cat >"$temp_dir/agent.yaml" <<'EOF'
server:
  endpoint: 127.0.0.1:7443
  tls:
    mode: public
EOF

sh "$script_dir/install.sh" server --binary "$server_binary" --config "$temp_dir/server.yaml"
sh "$script_dir/install.sh" agent --binary "$agent_binary" --config "$temp_dir/agent.yaml"

for unit in xtunnel-server.service xtunnel-agent.service; do
	systemctl is-enabled --quiet "$unit"
	systemctl is-active --quiet "$unit"
	systemctl restart "$unit"
	systemctl is-active --quiet "$unit"
done

test "$(stat -c '%a:%U:%G' /etc/xtunnel/server.yaml)" = '640:root:xtunnel-server'
test "$(stat -c '%a:%U:%G' /etc/xtunnel/agent.yaml)" = '640:root:xtunnel-agent'
test "$(stat -c '%a:%U:%G' /run/xtunnel)" = '700:xtunnel-server:xtunnel-server'
test "$(stat -c '%a:%U:%G' /run/xtunnel-agent)" = '700:xtunnel-agent:xtunnel-agent'
test "$(stat -c '%a:%U:%G' /var/lib/xtunnel)" = '700:xtunnel-server:xtunnel-server'
test "$(stat -c '%a:%U:%G' /var/lib/xtunnel-agent)" = '700:xtunnel-agent:xtunnel-agent'
test "$(stat -c '%a:%U:%G' /usr/local/bin/xtunnel-server)" = '755:root:root'
test "$(stat -c '%a:%U:%G' /usr/local/bin/xtunnel-agent)" = '755:root:root'
test "$(stat -c '%a:%U:%G' /etc/systemd/system/xtunnel-server.service)" = '644:root:root'
test "$(stat -c '%a:%U:%G' /etc/systemd/system/xtunnel-agent.service)" = '644:root:root'
test -f /var/lib/xtunnel/xtunnel.db

# 凭据占位文件只用于证明卸载保留边界，不包含真实 Token。
install -o xtunnel-agent -g xtunnel-agent -m 0600 /dev/null /var/lib/xtunnel-agent/token

for unit in xtunnel-server.service xtunnel-agent.service; do
	systemctl stop "$unit"
	if systemctl is-active --quiet "$unit"; then
		printf '%s is still active after stop\n' "$unit" >&2
		exit 1
	fi
	systemctl start "$unit"
	systemctl is-active --quiet "$unit"
done

sh "$script_dir/uninstall.sh" server
sh "$script_dir/uninstall.sh" agent

for path in \
	/etc/systemd/system/xtunnel-server.service \
	/etc/systemd/system/xtunnel-agent.service \
	/usr/local/bin/xtunnel-server \
	/usr/local/bin/xtunnel-agent; do
	if [ -e "$path" ] || [ -L "$path" ]; then
		printf 'uninstall left packaging artifact behind: %s\n' "$path" >&2
		exit 1
	fi
done

test -f /etc/xtunnel/server.yaml
test -f /etc/xtunnel/agent.yaml
test -f /var/lib/xtunnel/xtunnel.db
test -f /var/lib/xtunnel-agent/token
id xtunnel-server >/dev/null 2>&1
id xtunnel-agent >/dev/null 2>&1

printf '%s\n' "systemd packaging smoke passed"
