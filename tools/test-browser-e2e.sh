#!/bin/sh
set -eu

repo_root=$(CDPATH= cd "$(dirname "$0")/.." && pwd -P)
runtime_dir=/run/xtunnel
server_pid=
proxy_cli_pid=
proxy_container=
temp_dir=
lock_path=
backup_socket_path=
bootstrap_socket_path=

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

# stop_process 先给进程完整的优雅退出窗口；超过上限后主动 KILL 并回收，避免
# Browser Gate 因 Server 的 30 秒 Drain 或异常子进程永久挂起。
stop_process() {
  stop_pid=$1
  stop_limit=$2
  if [ -z "$stop_pid" ]; then
    return
  fi
  if kill -0 "$stop_pid" 2>/dev/null; then
    kill "$stop_pid" 2>/dev/null || true
    stop_elapsed=0
    while kill -0 "$stop_pid" 2>/dev/null && [ "$stop_elapsed" -lt "$stop_limit" ]; do
      sleep 1
      stop_elapsed=$((stop_elapsed + 1))
    done
    if kill -0 "$stop_pid" 2>/dev/null; then
      kill -KILL "$stop_pid" 2>/dev/null || true
    fi
  fi
  wait "$stop_pid" 2>/dev/null || true
}

stop_proxy() {
  if [ -n "$proxy_container" ] && docker container inspect "$proxy_container" >/dev/null 2>&1; then
    if ! timeout 40s docker stop --time 35 "$proxy_container" >/dev/null 2>&1; then
      timeout 10s docker kill "$proxy_container" >/dev/null 2>&1 || true
    fi
    timeout 10s docker rm --force "$proxy_container" >/dev/null 2>&1 || true
  fi
  stop_process "$proxy_cli_pid" 5
  proxy_cli_pid=
  proxy_container=
}

cleanup() {
  stop_proxy
  stop_process "$server_pid" 35
  if [ -n "$backup_socket_path" ] && [ -S "$backup_socket_path" ]; then
    rm -- "$backup_socket_path"
  fi
  if [ -n "$lock_path" ] && [ -f "$lock_path" ]; then
    rm -- "$lock_path"
  fi
  if [ -n "$temp_dir" ] && [ -d "$temp_dir" ]; then
    rm -rf -- "$temp_dir"
  fi
}

validate_image() {
  image_value=$1
  image_label=$2
  printf '%s\n' "$image_value" | grep -Eq '@sha256:[0-9a-f]{64}$' ||
    fail "$image_label must be pinned by an exact sha256 digest."
  docker image inspect "$image_value" >/dev/null 2>&1 ||
    fail "$image_label must already exist locally; the Browser E2E does not pull images."
  image_arch=$(docker image inspect --format '{{.Architecture}}' "$image_value")
  [ "$image_arch" = "$host_arch" ] || fail "$image_label architecture does not match the current host."
}

write_proxy_configs() {
  printf '%s\n' \
    '{' \
    '  admin off' \
    '  auto_https off' \
    '  servers {' \
    '    timeouts {' \
    '      read_header 10s' \
    '    }' \
    '  }' \
    '}' \
    'https://127.0.0.1:5173 {' \
    '  tls /etc/xtunnel/tls/loopback.crt /etc/xtunnel/tls/loopback.key' \
    '  reverse_proxy 127.0.0.1:8080 {' \
    '    header_up Host {hostport}' \
    '    header_up X-Forwarded-For {remote_host}' \
    '    header_up X-Forwarded-Proto {scheme}' \
    '    header_up X-Forwarded-Host {hostport}' \
    '    transport http {' \
    '      versions 1.1' \
    '    }' \
    '  }' \
    '}' \
    >"$caddy_config"

  printf '%s\n' \
    'worker_processes 1;' \
    'events {}' \
    'http {' \
    '  server {' \
    '    listen 127.0.0.1:5173 ssl;' \
    '    server_name 127.0.0.1;' \
    '    ssl_certificate /etc/xtunnel/tls/loopback.crt;' \
    '    ssl_certificate_key /etc/xtunnel/tls/loopback.key;' \
    '    client_header_timeout 10s;' \
    '    client_max_body_size 0;' \
    '    location / {' \
    '      proxy_http_version 1.1;' \
    '      proxy_set_header Host $http_host;' \
    '      proxy_set_header X-Forwarded-For $remote_addr;' \
    '      proxy_set_header X-Forwarded-Proto $scheme;' \
    '      proxy_set_header X-Forwarded-Host $http_host;' \
    '      proxy_request_buffering off;' \
    '      proxy_buffering off;' \
    '      proxy_pass http://127.0.0.1:8080;' \
    '    }' \
    '  }' \
    '}' \
    >"$nginx_config"
}

start_proxy() {
  proxy_kind=$1
  proxy_container="xtunnel-browser-e2e-$proxy_kind-$$"
  proxy_log="$temp_dir/$proxy_kind.log"

  case "$proxy_kind" in
    caddy)
      docker run --rm --name "$proxy_container" --network host \
        --volume "$caddy_config:/etc/caddy/Caddyfile:ro" \
        --volume "$cert_file:/etc/xtunnel/tls/loopback.crt:ro" \
        --volume "$key_file:/etc/xtunnel/tls/loopback.key:ro" \
        "$caddy_image" caddy run --config /etc/caddy/Caddyfile --adapter caddyfile \
        >"$proxy_log" 2>&1 &
      ;;
    nginx)
      docker run --rm --name "$proxy_container" --network host \
        --volume "$nginx_config:/etc/nginx/nginx.conf:ro" \
        --volume "$cert_file:/etc/xtunnel/tls/loopback.crt:ro" \
        --volume "$key_file:/etc/xtunnel/tls/loopback.key:ro" \
        "$nginx_image" nginx -c /etc/nginx/nginx.conf -g 'daemon off;' \
        >"$proxy_log" 2>&1 &
      ;;
    *)
      fail "Unsupported Browser E2E proxy kind."
      ;;
  esac
  proxy_cli_pid=$!

  proxy_ready=false
  proxy_attempt=0
  while [ "$proxy_attempt" -lt 60 ]; do
    if ! kill -0 "$proxy_cli_pid" 2>/dev/null; then
      printf '%s\n' "$proxy_kind HTTPS proxy exited before readiness; logs were withheld to protect secrets." >&2
      return 1
    fi
    if curl --insecure --silent --show-error --output /dev/null \
      --write-out '%{http_code}' https://127.0.0.1:5173/ | grep -q '^200$'; then
      proxy_ready=true
      break
    fi
    proxy_attempt=$((proxy_attempt + 1))
    sleep 1
  done
  if [ "$proxy_ready" != true ]; then
    printf '%s\n' "$proxy_kind HTTPS proxy did not become ready; logs were withheld to protect secrets." >&2
    return 1
  fi
}

run_browser_suite() {
  suite_kind=$1
  start_proxy "$suite_kind" || return 1
  suite_status=0
  XTUNNEL_E2E_PASSWORD="$e2e_admin_password" \
    XTUNNEL_E2E_PROXY_KIND="$suite_kind" \
    XTUNNEL_E2E_OUTPUT_DIR="$temp_dir/playwright-$suite_kind" \
    "$repo_root/web/node_modules/.bin/playwright" test \
      --config "$repo_root/web/playwright.config.ts" || suite_status=$?
  if [ "$suite_status" -ne 0 ]; then
    printf '%s\n' "$suite_kind Browser E2E failed; Server and proxy logs were withheld to protect secrets." >&2
  fi
  stop_proxy
  return "$suite_status"
}

trap cleanup EXIT
trap 'exit 130' HUP INT TERM

# 即使调用环境意外带入同名变量，也先移除 export 属性；临时密码只作为下方
# 单次 Playwright 命令的环境前缀传递，不进入 Server 或 Proxy 子进程环境。
unset XTUNNEL_E2E_PASSWORD
unset e2e_admin_password

[ "$(uname -s)" = Linux ] || fail "Browser E2E requires Linux because XTunnel V0.1 Server is Linux-only."
for command_name in awk curl docker go grep mktemp npm openssl sha256sum stat timeout tr; do
  command -v "$command_name" >/dev/null 2>&1 || fail "Browser E2E requires $command_name."
done
[ -d "$runtime_dir" ] || fail "$runtime_dir must be created by the caller with mode 0700."
[ -w "$runtime_dir" ] || fail "$runtime_dir must be writable by the current user."
[ "$(stat -c '%a' "$runtime_dir")" = 700 ] || fail "$runtime_dir must have mode 0700."
docker info >/dev/null 2>&1 || fail "Browser E2E requires an available Docker Engine."

case $(uname -m) in
  x86_64) host_arch=amd64 ;;
  aarch64 | arm64) host_arch=arm64 ;;
  *) fail "Browser E2E does not support the current host architecture." ;;
esac

XTUNNEL_CADDY_IMAGE=${XTUNNEL_CADDY_IMAGE:-}
XTUNNEL_NGINX_IMAGE=${XTUNNEL_NGINX_IMAGE:-}
[ -n "$XTUNNEL_CADDY_IMAGE" ] || fail "XTUNNEL_CADDY_IMAGE is required."
[ -n "$XTUNNEL_NGINX_IMAGE" ] || fail "XTUNNEL_NGINX_IMAGE is required."
validate_image "$XTUNNEL_CADDY_IMAGE" XTUNNEL_CADDY_IMAGE
validate_image "$XTUNNEL_NGINX_IMAGE" XTUNNEL_NGINX_IMAGE
# 镜像变量属于测试编排输入；校验后改存为非导出变量，避免 Server 将它们
# 误判为未知产品配置。代理仍由当前 shell 使用同一精确摘要启动。
caddy_image=$XTUNNEL_CADDY_IMAGE
nginx_image=$XTUNNEL_NGINX_IMAGE
unset XTUNNEL_CADDY_IMAGE XTUNNEL_NGINX_IMAGE

umask 077
temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/xtunnel-browser-e2e.XXXXXX")
temp_dir=$(CDPATH= cd "$temp_dir" && pwd -P)
state_parent="$temp_dir/state"
data_dir="$state_parent/data"
password_file="$temp_dir/admin-password"
config_file="$temp_dir/server.yaml"
cert_file="$temp_dir/loopback.crt"
key_file="$temp_dir/loopback.key"
caddy_config="$temp_dir/Caddyfile"
nginx_config="$temp_dir/nginx.conf"
server_binary="$temp_dir/xtunnel-server"
server_log="$temp_dir/server.log"
mkdir -m 700 "$state_parent" "$data_dir"

export GOTOOLCHAIN=local
"$repo_root/tools/check-go-version.sh"
ulimit -n 1048576 || fail "Browser E2E requires a nofile limit of at least 1048576."

npm --prefix "$repo_root/web" run build
(cd "$repo_root" && go build -o "$server_binary" ./cmd/server)

e2e_admin_password=$(openssl rand -base64 36 | tr -d '\n')
[ -n "$e2e_admin_password" ] || fail "Could not generate the temporary administrator password."
printf '%s' "$e2e_admin_password" >"$password_file"

printf '%s\n' \
  'server:' \
  "  data_dir: \"$data_dir\"" \
  'management:' \
  '  listen: "127.0.0.1:8080"' \
  '  public_url: "https://127.0.0.1:5173"' \
  '  allowed_hosts:' \
  '    - "127.0.0.1:5173"' \
  'http_ingress:' \
  '  listen: "127.0.0.1:18081"' \
  'agent_gateway:' \
  '  listen: "127.0.0.1:17443"' \
  '  public_hostname: "gateway.example.test"' \
  >"$config_file"

target_hash=$(printf '%s' "$data_dir" | sha256sum | awk '{print $1}')
[ -n "$target_hash" ] || fail "Could not calculate the Server data target hash."
lock_path="$runtime_dir/server-lock-$target_hash.lock"
backup_socket_path="$runtime_dir/backup-$target_hash.sock"
bootstrap_socket_path="$runtime_dir/admin-bootstrap.sock"

openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 1 \
  -subj /CN=127.0.0.1 -addext subjectAltName=IP:127.0.0.1 \
  -keyout "$key_file" -out "$cert_file" >/dev/null 2>&1
write_proxy_configs

"$server_binary" --config "$config_file" >"$server_log" 2>&1 &
server_pid=$!

# 空数据目录必须先由 Server 创建 pinned Gateway 身份，再通过 root-only Bootstrap
# Socket 在线提交首个管理员；离线先建库会按安全边界禁止首次身份生成。
bootstrap_ready=false
bootstrap_attempt=0
while [ "$bootstrap_attempt" -lt 60 ]; do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    fail "xtunnel-server exited before admin bootstrap; logs were withheld to protect secrets."
  fi
  if [ -S "$bootstrap_socket_path" ]; then
    bootstrap_ready=true
    break
  fi
  bootstrap_attempt=$((bootstrap_attempt + 1))
  sleep 1
done
[ "$bootstrap_ready" = true ] || fail "xtunnel-server admin bootstrap did not become ready; logs were withheld to protect secrets."

if [ "$(id -u)" -eq 0 ]; then
  "$server_binary" admin create --username e2e-admin --password-file "$password_file" --config "$config_file"
else
  command -v sudo >/dev/null 2>&1 || fail "Browser E2E requires sudo for the root-only admin bootstrap."
  sudo -n "$server_binary" admin create --username e2e-admin --password-file "$password_file" --config "$config_file"
fi
rm -- "$password_file"

server_ready=false
server_attempt=0
while [ "$server_attempt" -lt 60 ]; do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    fail "xtunnel-server exited before readiness; logs were withheld to protect secrets."
  fi
  if curl --silent --show-error --output /dev/null \
    --header 'Host: 127.0.0.1:5173' \
    --write-out '%{http_code}' \
    http://127.0.0.1:8080/api/v1/auth/me | grep -q '^401$'; then
    server_ready=true
    break
  fi
  server_attempt=$((server_attempt + 1))
  sleep 1
done
[ "$server_ready" = true ] || fail "xtunnel-server did not become ready; logs were withheld to protect secrets."

run_browser_suite caddy
run_browser_suite nginx
e2e_admin_password=
