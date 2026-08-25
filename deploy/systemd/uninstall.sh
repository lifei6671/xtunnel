#!/bin/sh
set -eu

usage() {
	cat <<'EOF'
Usage: uninstall.sh server

Stops and disables XTunnel Server, then removes only its service unit and
installed binary. Configuration and persistent data remain in place. Agent
uninstallation is managed by the xtunnel-agent binary itself.
EOF
}

component=${1-}
case "$component" in
	server) ;;
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
	printf '%s\n' "uninstall must run as root" >&2
	exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
	printf '%s\n' "systemctl is required" >&2
	exit 1
fi

unit=xtunnel-server.service
if [ -e "/etc/systemd/system/$unit" ] || [ -L "/etc/systemd/system/$unit" ]; then
	systemctl disable --now "$unit"
fi
rm -f -- "/etc/systemd/system/$unit" /usr/local/bin/xtunnel-server
systemctl daemon-reload

printf 'uninstalled %s; configuration, credentials, data, and service users were preserved\n' "$unit"
