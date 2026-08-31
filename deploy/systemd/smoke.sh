#!/bin/sh
set -eu

usage() {
	cat <<'EOF'
Usage: smoke.sh --server-binary PATH --agent-binary PATH

Runs install, start, restart, stop, start, and uninstall checks for both
systemd services. It also injects a bounded Server startup failure, recovery
restart, and runtime-only stop timeout. This destructive test is only for an
isolated Linux host. It refuses to run when any XTunnel path, service user,
service group, or runtime drop-in exists.
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
	/run/systemd/system/xtunnel-server.service.d \
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
umask 077
temp_dir=$(mktemp -d)
unmanaged_agent_unit_owned=0
unmanaged_agent_unit_fingerprint=

remove_owned_unmanaged_agent_unit() {
	[ "$unmanaged_agent_unit_owned" -eq 1 ] || return 0
	if [ ! -f /etc/systemd/system/xtunnel-agent.service ] \
		|| ! cmp -s "$temp_dir/unmanaged-xtunnel-agent.service" /etc/systemd/system/xtunnel-agent.service \
		|| [ "$(stat -c '%a:%u:%g:%s:%i:%y:%z' /etc/systemd/system/xtunnel-agent.service)" != "$unmanaged_agent_unit_fingerprint" ]; then
		printf '%s\n' "cleanup preserved an unmanaged Agent unit whose identity changed during the smoke" >&2
		return 1
	fi
	rm -f -- /etc/systemd/system/xtunnel-agent.service
	unmanaged_agent_unit_owned=0
}

cleanup() {
	if ! remove_owned_unmanaged_agent_unit; then
		printf '%s\n' "cleanup skipped Agent uninstall to avoid deleting a changed unmanaged unit" >&2
	else
		if [ -x /usr/local/bin/xtunnel-agent ]; then
			/usr/local/bin/xtunnel-agent service uninstall >/dev/null 2>&1 || true
		else
			"$agent_binary" service uninstall >/dev/null 2>&1 || true
		fi
	fi
	sh "$script_dir/uninstall.sh" server >/dev/null 2>&1 || true
	for unit in xtunnel-server.service xtunnel-agent.service; do
		if systemctl is-active --quiet "$unit"; then
			printf 'cleanup stopped before deleting files because %s is still active\n' "$unit" >&2
			return
		fi
	done
	rm -rf -- \
		/etc/xtunnel \
		/run/systemd/system/xtunnel-server.service.d \
		/run/xtunnel \
		/run/xtunnel-agent \
		/var/lib/xtunnel \
		/var/lib/xtunnel-agent \
		"$temp_dir"
	systemctl daemon-reload >/dev/null 2>&1 || true
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

wait_for_unit_state() {
	unit=$1
	want=$2
	deadline=$(( $(date +%s) + 30 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if [ "$(systemctl show --property=ActiveState --value "$unit")" = "$want" ]; then
			return 0
		fi
		sleep 1
	done
	printf '%s did not reach ActiveState=%s\n' "$unit" "$want" >&2
	return 1
}

wait_for_restarted_pid() {
	unit=$1
	old_pid=$2
	deadline=$(( $(date +%s) + 30 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		state=$(systemctl show --property=ActiveState --value "$unit")
		new_pid=$(systemctl show --property=MainPID --value "$unit")
		case "$new_pid" in
			''|*[!0-9]*) new_pid=0 ;;
		esac
		if [ "$state" = active ] && [ "$new_pid" -ne 0 ] && [ "$new_pid" -ne "$old_pid" ]; then
			return 0
		fi
		sleep 1
	done
	printf '%s did not recover with a new MainPID after failure\n' "$unit" >&2
	return 1
}

wait_for_journal_pattern() {
	unit=$1
	since=$2
	pattern=$3
	deadline=$(( $(date +%s) + 10 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if journalctl -u "$unit" --since "@$since" --no-pager | grep -E "$pattern" >/dev/null; then
			return 0
		fi
		sleep 1
	done
	printf '%s journal did not match expected pattern: %s\n' "$unit" "$pattern" >&2
	systemctl show --property=ActiveState,SubState,Result,ExecMainStatus,NRestarts "$unit" >&2 || true
	journalctl -u "$unit" --since "@$since" --no-pager -n 50 >&2 || true
	return 1
}

wait_for_unit_property() {
	unit=$1
	property=$2
	want=$3
	deadline=$(( $(date +%s) + 10 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		actual=$(systemctl show --property="$property" --value "$unit")
		if [ "$actual" = "$want" ]; then
			return 0
		fi
		sleep 1
	done
	printf '%s property %s=%s, want %s\n' "$unit" "$property" "$actual" "$want" >&2
	systemctl show --property=ActiveState,SubState,Result,ExecMainStatus,NRestarts,TimeoutStopUSec "$unit" >&2 || true
	return 1
}

assert_agent_side_effects_absent() {
	for path in \
		/usr/local/bin/xtunnel-agent \
		/etc/xtunnel \
		/run/xtunnel-agent \
		/var/lib/xtunnel-agent; do
		if [ -e "$path" ] || [ -L "$path" ]; then
			printf 'rejected Agent service operation modified protected path: %s\n' "$path" >&2
			exit 1
		fi
	done
	if id xtunnel-agent >/dev/null 2>&1 || getent group xtunnel-agent >/dev/null 2>&1; then
		printf '%s\n' "rejected Agent service operation created the service identity" >&2
		exit 1
	fi
}

assert_agent_targets_absent() {
	assert_agent_side_effects_absent
	if [ -e /etc/systemd/system/xtunnel-agent.service ] \
		|| [ -L /etc/systemd/system/xtunnel-agent.service ]; then
		printf '%s\n' "rejected Agent service operation created the systemd unit" >&2
		exit 1
	fi
}

expect_agent_failure() {
	failure_name=$1
	expected_text=$2
	shift 2
	failure_output="$temp_dir/$failure_name.output"
	set +e
	"$@" >"$failure_output" 2>&1
	failure_status=$?
	set -e
	if [ "$failure_status" -eq 0 ]; then
		printf '%s unexpectedly succeeded\n' "$failure_name" >&2
		exit 1
	fi
	if grep -F "$smoke_agent_token" "$failure_output" >/dev/null; then
		printf '%s leaked the Connection Token in failure output\n' "$failure_name" >&2
		exit 1
	fi
	if ! grep -F "$expected_text" "$failure_output" >/dev/null; then
		failure_bytes=$(wc -c <"$failure_output" | tr -d ' ')
		printf '%s did not report the expected failure; output_bytes=%s\n' "$failure_name" "$failure_bytes" >&2
		exit 1
	fi
}

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

# 产品入口必须在任何账户、Binary、Credential 或 Unit 写入前拒绝不支持的权限和
# systemd 版本。复制到可遍历的隔离目录，避免 Runner 临时目录权限掩盖 non-root 断言。
preflight_agent_binary="$temp_dir/xtunnel-agent-preflight"
cp "$agent_binary" "$preflight_agent_binary"
chmod 755 "$preflight_agent_binary"
chmod 711 "$temp_dir"
if ! command -v runuser >/dev/null 2>&1; then
	printf '%s\n' "runuser is required for the Agent non-root preflight smoke" >&2
	exit 1
fi
nonroot_user=nobody
nonroot_uid=$(id -u "$nonroot_user" 2>/dev/null || true)
case "$nonroot_uid" in
	''|*[!0-9]*|0)
		printf '%s\n' "a real non-root nobody identity is required for the Agent preflight smoke" >&2
		exit 1
		;;
esac
expect_agent_failure \
	agent-nonroot-install \
	'service install must run as root' \
	runuser -u "$nonroot_user" -- "$preflight_agent_binary" service install --token "$smoke_agent_token"
assert_agent_targets_absent
expect_agent_failure \
	agent-nonroot-uninstall \
	'service uninstall must run as root' \
	runuser -u "$nonroot_user" -- "$preflight_agent_binary" service uninstall
assert_agent_targets_absent

old_systemd_bin="$temp_dir/old-systemd-bin"
mkdir "$old_systemd_bin"
cat >"$old_systemd_bin/systemctl" <<'EOF'
#!/bin/sh
if [ "$#" -eq 3 ] && [ "$1" = show ] && [ "$2" = '--property=Version' ] && [ "$3" = --value ]; then
	printf '%s\n' 248
	exit 0
fi
exit 97
EOF
chmod 755 "$old_systemd_bin/systemctl"
expect_agent_failure \
	agent-old-systemd-install \
	'systemd 249 or newer is required; found 248' \
	env PATH="$old_systemd_bin:$PATH" "$agent_binary" service install --token "$smoke_agent_token"
assert_agent_targets_absent
expect_agent_failure \
	agent-old-systemd-uninstall \
	'systemd 249 or newer is required; found 248' \
	env PATH="$old_systemd_bin:$PATH" "$agent_binary" service uninstall
assert_agent_targets_absent

# 同名 Unit 只有首行 managed marker 才属于 XTunnel。用字节、权限、owner、inode 和
# 纳秒时间戳冻结外来对象，install/uninstall 两条产品路径都必须拒绝且原对象不变。
cat >"$temp_dir/unmanaged-xtunnel-agent.service" <<'EOF'
[Unit]
Description=Foreign xtunnel-agent service owned by the systemd smoke fixture

[Service]
Type=oneshot
ExecStart=/bin/true
EOF
install -o root -g root -m 0640 \
	"$temp_dir/unmanaged-xtunnel-agent.service" \
	/etc/systemd/system/xtunnel-agent.service
unmanaged_agent_unit_fingerprint=$(stat -c '%a:%u:%g:%s:%i:%y:%z' /etc/systemd/system/xtunnel-agent.service)
unmanaged_agent_unit_owned=1
expect_agent_failure \
	agent-unmanaged-unit-install \
	'refusing to overwrite an unmanaged xtunnel-agent.service' \
	"$agent_binary" service install --token "$smoke_agent_token"
assert_agent_side_effects_absent
cmp -s "$temp_dir/unmanaged-xtunnel-agent.service" /etc/systemd/system/xtunnel-agent.service
test "$(stat -c '%a:%u:%g:%s:%i:%y:%z' /etc/systemd/system/xtunnel-agent.service)" = "$unmanaged_agent_unit_fingerprint"
expect_agent_failure \
	agent-unmanaged-unit-uninstall \
	'refusing to remove an unmanaged xtunnel-agent.service' \
	"$agent_binary" service uninstall
assert_agent_side_effects_absent
cmp -s "$temp_dir/unmanaged-xtunnel-agent.service" /etc/systemd/system/xtunnel-agent.service
test "$(stat -c '%a:%u:%g:%s:%i:%y:%z' /etc/systemd/system/xtunnel-agent.service)" = "$unmanaged_agent_unit_fingerprint"
remove_owned_unmanaged_agent_unit
assert_agent_targets_absent
printf '%s\n' "Agent service preflight rejection smoke passed"

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
printf '%s\n' "systemd packaging baseline passed"

# 启动失败使用 runtime-only Restart=no，避免无效配置形成无限重启；恢复后立即删除
# drop-in。Journal、Result 和退出码共同证明故障可定位，生产 Unit 不被改写。
server_unit=xtunnel-server.service
server_dropin=/run/systemd/system/xtunnel-server.service.d
cp /etc/xtunnel/server.yaml "$temp_dir/server.valid.yaml"
mkdir -p "$server_dropin"
cat >"$server_dropin/m6-06.conf" <<'EOF'
[Service]
Restart=no
EOF
systemctl daemon-reload
systemctl stop "$server_unit"
startup_failure_since=$(date +%s)
printf '%s\n' 'invalid: [unterminated' > /etc/xtunnel/server.yaml
set +e
systemctl start "$server_unit"
startup_command_status=$?
set -e
case "$startup_command_status" in
	''|*[!0-9]*)
		printf 'systemctl start returned invalid status=%s\n' "$startup_command_status" >&2
		exit 1
		;;
esac
wait_for_unit_state "$server_unit" failed
wait_for_unit_property "$server_unit" Result exit-code
server_exit_status=$(systemctl show --property=ExecMainStatus --value "$server_unit")
case "$server_exit_status" in
	''|*[!0-9]*|0)
		printf 'Server startup failure returned invalid ExecMainStatus=%s\n' "$server_exit_status" >&2
		exit 1
		;;
esac
wait_for_journal_pattern "$server_unit" "$startup_failure_since" 'load server config'
printf '%s\n' "systemd startup failure diagnostics passed"
cp "$temp_dir/server.valid.yaml" /etc/xtunnel/server.yaml
rm -f -- "$server_dropin/m6-06.conf"
rmdir "$server_dropin"
systemctl daemon-reload
systemctl reset-failed "$server_unit"
systemctl start "$server_unit"
systemctl is-active --quiet "$server_unit"

# 生产 Restart=on-failure 必须在非预期退出后产生新 PID；该注入不修改磁盘数据。
restart_count_before=$(systemctl show --property=NRestarts --value "$server_unit")
server_pid_before=$(systemctl show --property=MainPID --value "$server_unit")
case "$restart_count_before:$server_pid_before" in
	*[!0-9:]*|:*|*:|*:0)
		printf 'Server recovery precondition is invalid: NRestarts=%s MainPID=%s\n' "$restart_count_before" "$server_pid_before" >&2
		exit 1
		;;
esac
recovery_since=$(date +%s)
kill -KILL "$server_pid_before"
wait_for_restarted_pid "$server_unit" "$server_pid_before"
restart_count_after=$(systemctl show --property=NRestarts --value "$server_unit")
case "$restart_count_after" in
	''|*[!0-9]*)
		printf 'Server recovery returned invalid NRestarts=%s\n' "$restart_count_after" >&2
		exit 1
		;;
esac
if [ "$restart_count_after" -le "$restart_count_before" ]; then
	printf 'Server NRestarts did not increase: before=%s after=%s\n' "$restart_count_before" "$restart_count_after" >&2
	exit 1
fi
wait_for_journal_pattern "$server_unit" "$recovery_since" 'process_started'
printf '%s\n' "systemd restart recovery diagnostics passed"

# 先记录生产 Unit 的真实 Stop 上限；这里只用 runtime drop-in 把隔离测试压缩到 2 秒，
# 并把停止信号改为不终止进程的 SIGCONT。systemd 会在超时后发送 SIGKILL，测试结束后
# 恢复原 Unit，不预改生产 TimeoutStopSec 或 KillSignal。
production_stop_timeout=$(systemctl show --property=TimeoutStopUSec --value "$server_unit")
case "$production_stop_timeout" in
	''|0|infinity)
		printf 'Server production TimeoutStopUSec is not a finite positive value: %s\n' "$production_stop_timeout" >&2
		exit 1
		;;
esac
systemctl stop "$server_unit"
mkdir -p "$server_dropin"
cat >"$server_dropin/m6-06.conf" <<'EOF'
[Service]
Restart=no
TimeoutStopSec=2s
KillSignal=SIGCONT
EOF
systemctl daemon-reload
systemctl start "$server_unit"
server_timeout_pid=$(systemctl show --property=MainPID --value "$server_unit")
case "$server_timeout_pid" in
	''|*[!0-9]*|0)
		printf 'Server timeout injection returned invalid MainPID=%s\n' "$server_timeout_pid" >&2
		exit 1
		;;
esac
timeout_since=$(date +%s)
set +e
systemctl stop "$server_unit"
stop_command_status=$?
set -e
case "$stop_command_status" in
	''|*[!0-9]*)
		printf 'systemctl stop returned invalid status=%s\n' "$stop_command_status" >&2
		exit 1
		;;
esac
wait_for_unit_property "$server_unit" Result timeout
wait_for_journal_pattern "$server_unit" "$timeout_since" 'stop-sigterm.*timed out'
printf '%s\n' "systemd stop timeout diagnostics passed"
rm -f -- "$server_dropin/m6-06.conf"
rmdir "$server_dropin"
systemctl daemon-reload
systemctl reset-failed "$server_unit"
systemctl start "$server_unit"
systemctl is-active --quiet "$server_unit"

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
