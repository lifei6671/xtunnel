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
repo_dir=$(CDPATH='' cd -- "$script_dir/../.." && pwd)
temp_dir=$(mktemp -d)

cleanup() {
	sh "$script_dir/uninstall.sh" server >/dev/null 2>&1 || true
	if [ -x /usr/local/bin/xtunnel-agent ]; then
		/usr/local/bin/xtunnel-agent service uninstall >/dev/null 2>&1 || true
	else
		"$agent_binary" service uninstall >/dev/null 2>&1 || true
	fi
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

first_token_path="$repo_dir/tests/golden/protocol-v1/connection-token-v1.txt"
second_token_path="$repo_dir/tests/golden/protocol-v1/connection-token-v1-secondary.txt"
if [ ! -r "$first_token_path" ] || [ ! -r "$second_token_path" ]; then
	printf '%s\n' "systemd smoke Connection Token fixtures are not readable" >&2
	exit 1
fi
IFS= read -r smoke_agent_token <"$first_token_path" || true
IFS= read -r reinstall_agent_token <"$second_token_path" || true
if [ -z "$smoke_agent_token" ] || [ -z "$reinstall_agent_token" ] || [ "$smoke_agent_token" = "$reinstall_agent_token" ]; then
	printf '%s\n' "systemd smoke Connection Token fixtures must be non-empty and distinct" >&2
	exit 1
fi

# 无迁移策略必须表现为安全拒绝，不能留下新 Binary、Config、Unit 或服务身份。
mkdir -p /var/lib/xtunnel
printf '%s\n' legacy > /var/lib/xtunnel/xtunnel.db
if sh "$script_dir/install.sh" server --binary "$server_binary" --config "$temp_dir/server.yaml" >/dev/null 2>&1; then
	printf '%s\n' "server install unexpectedly accepted the legacy data layout" >&2
	exit 1
fi
test "$(cat /var/lib/xtunnel/xtunnel.db)" = legacy
test ! -e /var/lib/xtunnel/data
test ! -e /usr/local/bin/xtunnel-server
test ! -e /etc/xtunnel/server.yaml
test ! -e /etc/systemd/system/xtunnel-server.service
if id xtunnel-server >/dev/null 2>&1 || getent group xtunnel-server >/dev/null 2>&1; then
	printf '%s\n' "rejected legacy install created the Server service identity" >&2
	exit 1
fi
rm -rf -- /var/lib/xtunnel

sh "$script_dir/install.sh" server --binary "$server_binary" --config "$temp_dir/server.yaml"
if "$agent_binary" service install >/dev/null 2>&1; then
	printf '%s\n' "agent install unexpectedly accepted a missing --token" >&2
	exit 1
fi
if "$agent_binary" service install --token invalid-smoke-token >/dev/null 2>&1; then
	printf '%s\n' "agent install unexpectedly accepted an invalid --token" >&2
	exit 1
fi
"$agent_binary" service install --token "$smoke_agent_token"
first_agent_pid=$(systemctl show --property=MainPID --value xtunnel-agent.service)
smoke_agent_token=$reinstall_agent_token
"$agent_binary" service install --token "$smoke_agent_token"
second_agent_pid=$(systemctl show --property=MainPID --value xtunnel-agent.service)
case "$first_agent_pid:$second_agent_pid" in
	*[!0-9:]*|0:*|*:0|:*|*:)
		printf 'Agent reinstall returned invalid MainPIDs: before=%s after=%s\n' "$first_agent_pid" "$second_agent_pid" >&2
		exit 1
		;;
esac
if [ "$first_agent_pid" -eq "$second_agent_pid" ]; then
	printf 'Agent reinstall did not restart the service: MainPID=%s\n' "$first_agent_pid" >&2
	exit 1
fi

for unit in xtunnel-server.service xtunnel-agent.service; do
	systemctl is-enabled --quiet "$unit"
	systemctl is-active --quiet "$unit"
	systemctl restart "$unit"
	systemctl is-active --quiet "$unit"
done

test "$(stat -c '%a:%U:%G' /etc/xtunnel/server.yaml)" = '640:root:xtunnel-server'
test ! -e /etc/xtunnel/agent.yaml
test ! -e /etc/xtunnel/agent.token
test "$(stat -c '%a:%U:%G' /etc/xtunnel/credentials)" = '700:root:root'
test "$(stat -c '%a:%U:%G' /etc/xtunnel/credentials/agent.token)" = '600:root:root'
test "$(cat /etc/xtunnel/credentials/agent.token)" = "$smoke_agent_token"
test "$(stat -c '%a:%U:%G' /run/xtunnel)" = '700:xtunnel-server:xtunnel-server'
test "$(stat -c '%a:%U:%G' /run/xtunnel-agent)" = '700:xtunnel-agent:xtunnel-agent'
test "$(stat -c '%a:%U:%G' /var/lib/xtunnel)" = '700:xtunnel-server:xtunnel-server'
test "$(stat -c '%a:%U:%G' /var/lib/xtunnel/data)" = '700:xtunnel-server:xtunnel-server'
test ! -e /var/lib/xtunnel-agent
test "$(stat -c '%a:%U:%G' /usr/local/bin/xtunnel-server)" = '755:root:root'
test "$(stat -c '%a:%U:%G' /usr/local/bin/xtunnel-agent)" = '755:root:root'
cmp -s "$agent_binary" /usr/local/bin/xtunnel-agent
test "$(stat -c '%a:%U:%G' /etc/systemd/system/xtunnel-server.service)" = '644:root:root'
test "$(stat -c '%a:%U:%G' /etc/systemd/system/xtunnel-agent.service)" = '644:root:root'
test "$(systemctl show --property=LimitNOFILE --value xtunnel-server.service)" = 1048576
test -f /var/lib/xtunnel/data/xtunnel.db
test "$(sed -n '1p' /etc/systemd/system/xtunnel-agent.service)" = '# Managed by xtunnel-agent service install'
grep -Fx 'LoadCredential=xtunnel-agent.token:/etc/xtunnel/credentials/agent.token' /etc/systemd/system/xtunnel-agent.service >/dev/null
grep -Fx 'ExecStart=/usr/local/bin/xtunnel-agent run' /etc/systemd/system/xtunnel-agent.service >/dev/null
if grep '^ExecStart=.*--token' /etc/systemd/system/xtunnel-agent.service >/dev/null \
	|| grep '^ExecStart=.*xta_' /etc/systemd/system/xtunnel-agent.service >/dev/null; then
	printf '%s\n' "managed Agent unit leaked a Token into ExecStart" >&2
	exit 1
fi

agent_pid=$(systemctl show --property=MainPID --value xtunnel-agent.service)
case "$agent_pid" in
	''|*[!0-9]*|0)
		printf 'Agent MainPID is invalid: %s\n' "$agent_pid" >&2
		exit 1
		;;
esac
credentials_directory=/run/credentials/xtunnel-agent.service
# Unit 没有 Token 参数或环境变量；Agent 成功常驻且运行时 Credential 内容正确，
# 共同证明它已通过 CREDENTIALS_DIRECTORY 读取 LoadCredential 产物。
runtime_credential="/proc/$agent_pid/root$credentials_directory/xtunnel-agent.token"
test -r "$runtime_credential"
test "$(cat "$runtime_credential")" = "$smoke_agent_token"

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
/usr/local/bin/xtunnel-agent service uninstall

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
test ! -e /etc/xtunnel/agent.yaml
test ! -e /etc/xtunnel/agent.token
test -f /etc/xtunnel/credentials/agent.token
test -f /var/lib/xtunnel/data/xtunnel.db
test ! -e /var/lib/xtunnel-agent
id xtunnel-server >/dev/null 2>&1
id xtunnel-agent >/dev/null 2>&1
test "$(getent passwd xtunnel-agent | cut -d: -f6)" = /nonexistent
test "$(cat /etc/xtunnel/credentials/agent.token)" = "$smoke_agent_token"

printf '%s\n' "systemd packaging smoke passed"
