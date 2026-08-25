#!/bin/sh
set -eu

usage() {
	cat <<'EOF'
Usage: install.sh server|agent --binary PATH --config PATH

Installs a packaged XTunnel binary, its configuration, and the matching
systemd service. The script must run as root. It creates the dedicated
role-specific system user when it does not already exist.
EOF
}

component=${1-}
case "$component" in
	server|agent)
		shift
		;;
	-h|--help|'')
		usage
		exit 0
		;;
*)
		usage >&2
		exit 2
		;;
esac

if [ "$(id -u)" -ne 0 ]; then
	printf '%s\n' "install must run as root" >&2
	exit 1
fi

binary=
config=
while [ "$#" -gt 0 ]; do
	case "$1" in
		--binary)
			binary=${2-}
			shift 2
			;;
		--config)
			config=${2-}
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

if [ -z "$binary" ] || [ -z "$config" ] || [ ! -f "$binary" ] || [ ! -f "$config" ]; then
	printf '%s\n' "--binary and --config must name existing regular files" >&2
	exit 2
fi

if ! command -v systemctl >/dev/null 2>&1 || ! command -v useradd >/dev/null 2>&1; then
	printf '%s\n' "systemctl and useradd are required" >&2
	exit 1
fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
unit="xtunnel-$component.service"
unit_source="$script_dir/$unit"
binary_target="/usr/local/bin/xtunnel-$component"
config_target="/etc/xtunnel/$component.yaml"
unit_target="/etc/systemd/system/$unit"
service_user="xtunnel-$component"

if [ ! -f "$unit_source" ]; then
	printf 'unit source does not exist: %s\n' "$unit_source" >&2
	exit 1
fi

if [ "$component" = server ]; then
	state_dir=/var/lib/xtunnel
else
	state_dir=/var/lib/xtunnel-agent
fi

if ! id "$service_user" >/dev/null 2>&1; then
	useradd --system --user-group --home-dir "$state_dir" --shell /usr/sbin/nologin "$service_user"
fi

install -d -m 0755 /usr/local/bin /etc/xtunnel /etc/systemd/system
install -m 0755 "$binary" "$binary_target"
# root 负责更新配置，服务只能通过自己的组读取，避免两个角色互读凭据。
install -o root -g "$service_user" -m 0640 "$config" "$config_target"
install -m 0644 "$unit_source" "$unit_target"

systemctl daemon-reload
systemctl enable --now "$unit"

printf 'installed and started %s\n' "$unit"
